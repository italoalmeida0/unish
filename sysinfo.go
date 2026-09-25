package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"

	"golang.org/x/term"
)

func init() {
	extraCommands = append(extraCommands,
		extraCmd{"df", cmdDf},
		extraCmd{"arch", cmdArch},
		extraCmd{"tty", cmdTty},
		extraCmd{"logname", cmdLogname},
		extraCmd{"sync", cmdSync},
		extraCmd{"nohup", cmdNohup},
		extraCmd{"nice", cmdNice},
		extraCmd{"ps", cmdPs},
		extraCmd{"free", cmdFree},
		extraCmd{"uptime", cmdUptime},
		extraCmd{"env", cmdEnv},
		extraCmd{"stat", cmdStat},
		extraCmd{"ss", cmdSs},
		extraCmd{"id", cmdId},
	)
}

func cmdDf(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("df", hc.Stderr)
	human := fs.Bool("h", false, "")
	fs.BoolVar(human, "human-readable", false, "")
	block1k := fs.Bool("k", false, "")
	posix := fs.Bool("P", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	_ = block1k
	_ = posix
	paths := fs.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}
	type row struct {
		fs, total, used, avail int64
		mount                  string
	}
	var rows []row
	for _, p := range paths {
		full := resolve(hc.Dir, p)
		total, free, avail, fstype, mount := diskUsage(full)
		used := total - free
		if used < 0 {
			used = 0
		}
		_ = fstype
		rows = append(rows, row{total: total, used: used, avail: avail, mount: mount})
	}
	if *human {
		fmt.Fprintln(hc.Stdout, "Filesystem      Size  Used Avail Use% Mounted on")
	} else {
		fmt.Fprintln(hc.Stdout, "Filesystem     1K-blocks     Used Available Use% Mounted on")
	}
	for _, r := range rows {
		pct := 0
		if r.total > 0 {
			pct = int(r.used * 100 / r.total)
		}
		if *human {
			fmt.Fprintf(hc.Stdout, "%-15s %4s  %4s %4s %3d%% %s\n",
				"fs", dfHuman(r.total), dfHuman(r.used), dfHuman(r.avail), pct, filepath.ToSlash(r.mount))
		} else {
			fmt.Fprintf(hc.Stdout, "%-15s %10d %8d %9d %3d%% %s\n",
				"fs", r.total/1024, r.used/1024, r.avail/1024, pct, filepath.ToSlash(r.mount))
		}
	}
	return nil
}

func dfHuman(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d", n)
	}
	units := []byte("KMGTPE")
	f := float64(n)
	e := -1
	for f >= 1024 && e < len(units)-1 {
		f /= 1024
		e++
	}
	if f >= 100 {
		return fmt.Sprintf("%.0f%c", f, units[e])
	}
	return fmt.Sprintf("%.1f%c", f, units[e])
}

type procInfo struct {
	pid, ppid int
	user      string
	cpu       float64
	memPct    float64
	vszKB     int64
	rssKB     int64
	tty       string
	stat      string
	start     string
	time      string
	cmd       string
}

func cmdPs(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("ps", hc.Stderr)
	bsd := false
	full := false
	for _, a := range args[1:] {
		if a == "aux" {
			bsd = true
			continue
		}
		t := strings.TrimPrefix(a, "-")
		if a != t {
			if strings.Contains(t, "f") || strings.Contains(t, "u") || strings.Contains(t, "l") {
				full = true
			}
		}
	}
	_ = fs
	if bsd && !full {
		full = false
	}
	procs, err := listProcs()
	if err != nil {
		fmt.Fprintln(hc.Stderr, "ps:", err)
		return exitError{1}
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].pid < procs[j].pid })
	if !full {
		// GNU `ps` / `ps -e` default format lists "PID TTY TIME CMD".
		// With no args GNU shows only the session; with -e/-a/-A it
		// shows every process. unish is single-process for builtins,
		// so plain `ps` reports self; -e lists all known processes.
		if len(args) == 1 {
			fmt.Fprintln(hc.Stdout, "    PID TTY          TIME CMD")
			me := os.Getpid()
			fmt.Fprintf(hc.Stdout, "%7d %-12s %8s %s\n", me, "?", "00:00:00", "unish")
			return nil
		}
		fmt.Fprintln(hc.Stdout, "    PID TTY          TIME CMD")
		for _, p := range procs {
			fmt.Fprintf(hc.Stdout, "%7d %-12s %8s %s\n", p.pid, p.tty, p.time, p.cmd)
		}
		return nil
	}
	if full {
		fmt.Fprintln(hc.Stdout, "UID          PID    PPID  C STIME TTY          TIME CMD")
		for _, p := range procs {
			fmt.Fprintf(hc.Stdout, "%-12s %5d %5d  - %s %-12s %s %s\n",
				p.user, p.pid, p.ppid, p.start, p.tty, p.time, p.cmd)
		}
		return nil
	}
	fmt.Fprintln(hc.Stdout, "USER         PID %CPU %MEM    VSZ   RSS TTY      STAT START   TIME COMMAND")
	for _, p := range procs {
		fmt.Fprintf(hc.Stdout, "%-12s %5d %4s %4s %6s %5s %-8s %-4s %-7s %8s %s\n",
			p.user, p.pid, pctStr(p.cpu), pctStr(p.memPct),
			kbStr(p.vszKB), kbStr(p.rssKB), p.tty, p.stat, p.start, p.time, p.cmd)
	}
	return nil
}

func pctStr(f float64) string {
	if f < 0 {
		return "0.0"
	}
	return fmt.Sprintf("%.1f", f)
}

func kbStr(n int64) string {
	if n < 0 {
		return "0"
	}
	return strconv.FormatInt(n, 10)
}

func cmdFree(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("free", hc.Stderr)
	mega := fs.Bool("m", false, "")
	giga := fs.Bool("g", false, "")
	human := fs.Bool("h", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	mi := memInfo()
	// GNU free prints KiB by default.
	div := int64(1024)
	if *mega {
		div = 1024 * 1024
	} else if *giga {
		div = 1024 * 1024 * 1024
	}
	fmt.Fprintln(hc.Stdout, "               total        used        free      shared  buff/cache   available")
	if *human {
		fmt.Fprintf(hc.Stdout, "Mem: %11s %11s %11s %11s %11s %11s\n",
			freeHuman(mi.total), freeHuman(mi.total-mi.free-mi.cached-mi.buffers),
			freeHuman(mi.free), freeHuman(mi.shared),
			freeHuman(mi.cached+mi.buffers), freeHuman(mi.avail))
		fmt.Fprintf(hc.Stdout, "Swap:%11s %11s %11s\n",
			freeHuman(mi.swapTotal), freeHuman(mi.swapTotal-mi.swapFree), freeHuman(mi.swapFree))
		return nil
	}
	fmt.Fprintf(hc.Stdout, "Mem: %11d %11d %11d %11d %11d %11d\n",
		mi.total/div, (mi.total-mi.free-mi.cached-mi.buffers)/div, mi.free/div,
		mi.shared/div, (mi.cached+mi.buffers)/div, mi.avail/div)
	fmt.Fprintf(hc.Stdout, "Swap:%11d %11d %11d\n",
		mi.swapTotal/div, (mi.swapTotal-mi.swapFree)/div, mi.swapFree/div)
	return nil
}

func freeHuman(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"Ki", "Mi", "Gi", "Ti"}
	f := float64(n)
	e := 0
	for f >= 1024 && e < len(units)-1 {
		f /= 1024
		e++
	}
	return fmt.Sprintf("%.1f%s", f, units[e])
}

func cmdUptime(_ context.Context, hc interp.HandlerContext, args []string) error {
	now := time.Now()
	boot := bootTime()
	up := now.Sub(boot)
	if up < 0 {
		up = 0
	}
	load1, load5, load15 := loadAvg()
	users := 1
	fmt.Fprintf(hc.Stdout, " %s up %s,  %d user%s,  load average: %.2f, %.2f, %.2f\n",
		now.Format("15:04:05"), fmtDuration(up), users, plural(users),
		load1, load5, load15)
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func fmtDuration(d time.Duration) string {
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	if days > 1 {
		return fmt.Sprintf("%d days, %d:%02d", days, h, m)
	}
	if days == 1 {
		return fmt.Sprintf("1 day, %d:%02d", h, m)
	}
	if h > 0 {
		return fmt.Sprintf("%d:%02d", h, m)
	}
	return fmt.Sprintf("%d min", m)
}

func cmdEnv(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("env", hc.Stderr)
	ignore := fs.Bool("i", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	var envPairs []string
	var cmd []string
	for i, a := range rest {
		if strings.Contains(a, "=") && !strings.HasPrefix(a, "-") && len(cmd) == 0 {
			k := a[:strings.IndexByte(a, '=')]
			if isEnvName(k) {
				envPairs = append(envPairs, a)
				continue
			}
		}
		cmd = rest[i:]
		break
	}
	_ = ignore
	if len(cmd) == 0 {
		seen := map[string]bool{}
		hc.Env.Each(func(name string, v expand.Variable) bool {
			if v.IsSet() && v.Exported {
				fmt.Fprintf(hc.Stdout, "%s=%s\n", name, v.Str)
				seen[name] = true
			}
			return true
		})
		for _, e := range os.Environ() {
			k := e[:strings.IndexByte(e, '=')]
			if !seen[k] {
				fmt.Fprintln(hc.Stdout, e)
			}
		}
		return nil
	}
	if *ignore {
		os.Clearenv()
	}
	for _, kv := range envPairs {
		k := kv[:strings.IndexByte(kv, '=')]
		v := kv[strings.IndexByte(kv, '=')+1:]
		os.Setenv(k, v)
	}
	if extra := lookupExtra(cmd[0]); extra != nil {
		return extra.main(ctx, hc, splitAttached(cmd[0], cmd))
	}
	return interp.DefaultExecHandler(2)(ctx, cmd)
}

func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func cmdStat(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("stat", hc.Stderr)
	format := fs.String("c", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) == 0 {
		fmt.Fprintln(hc.Stderr, "stat: missing operand")
		return flag.ErrHelp
	}
	code := 0
	for _, p := range fs.Args() {
		full := resolve(hc.Dir, p)
		fi, err := os.Lstat(full)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "stat: cannot statx '%s': No such file or directory\n", p)
			code = 1
			continue
		}
		if *format != "" {
			fmt.Fprintln(hc.Stdout, statFormat(*format, p, full, fi))
			continue
		}
		printStatLong(hc, p, full, fi)
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func fileTypeName(fi os.FileInfo) string {
	switch {
	case fi.IsDir():
		return "directory"
	case fi.Mode()&os.ModeSymlink != 0:
		return "symbolic link"
	case fi.Mode()&os.ModeNamedPipe != 0:
		return "fifo"
	case fi.Mode()&os.ModeSocket != 0:
		return "socket"
	case fi.Mode().IsRegular():
		return "regular file"
	default:
		return "special file"
	}
}

func printStatLong(hc interp.HandlerContext, display, full string, fi os.FileInfo) {
	fmt.Fprintf(hc.Stdout, "  File: %s\n", display)
	fmt.Fprintf(hc.Stdout, "  Size: %-10d\tBlocks: %-10d IO Block: %-6d %s\n",
		fi.Size(), blocksOf(fi), 4096, fileTypeName(fi))
	fmt.Fprintf(hc.Stdout, "Device: %s\tInode: %-10d Links: %d\n",
		deviceOf(full), inodeOf(full), linksOf(full, fi))
	fmt.Fprintf(hc.Stdout, "Access: (%04o/%s)  Uid: (%5d/%8s)   Gid: (%5d/%8s)\n",
		fi.Mode().Perm(), lsPerm(fi), uidOf(full), userOf(full), gidOf(full), groupOf(full))
	fmt.Fprintf(hc.Stdout, "Access: %s\n", fi.ModTime().Format("2006-01-02 15:04:05.000000000 -0700"))
	fmt.Fprintf(hc.Stdout, "Modify: %s\n", fi.ModTime().Format("2006-01-02 15:04:05.000000000 -0700"))
	fmt.Fprintf(hc.Stdout, "Change: %s\n", fi.ModTime().Format("2006-01-02 15:04:05.000000000 -0700"))
	fmt.Fprintf(hc.Stdout, " Birth: %s\n", birthOf(full, fi))
}

func statFormat(f, display, full string, fi os.FileInfo) string {
	var sb strings.Builder
	for i := 0; i < len(f); i++ {
		if f[i] != '%' || i+1 >= len(f) {
			sb.WriteByte(f[i])
			continue
		}
		i++
		switch f[i] {
		case 'n':
			sb.WriteString(display)
		case 'N':
			if fi.Mode()&os.ModeSymlink != 0 {
				if dst, err := os.Readlink(full); err == nil {
					fmt.Fprintf(&sb, "'%s' -> '%s'", display, dst)
					break
				}
			}
			fmt.Fprintf(&sb, "'%s'", display)
		case 's':
			fmt.Fprintf(&sb, "%d", fi.Size())
		case 'F':
			sb.WriteString(fileTypeName(fi))
		case 'a':
			fmt.Fprintf(&sb, "%o", fi.Mode().Perm())
		case 'A':
			sb.WriteString(lsPerm(fi))
		case 'u':
			fmt.Fprintf(&sb, "%d", uidOf(full))
		case 'U':
			sb.WriteString(userOf(full))
		case 'g':
			fmt.Fprintf(&sb, "%d", gidOf(full))
		case 'G':
			sb.WriteString(groupOf(full))
		case 'i':
			fmt.Fprintf(&sb, "%d", inodeOf(full))
		case 'h':
			fmt.Fprintf(&sb, "%d", linksOf(full, fi))
		case 'd':
			sb.WriteString(deviceOf(full))
		case 'y', 'z':
			sb.WriteString(fi.ModTime().Format("2006-01-02 15:04:05.000000000 -0700"))
		case 'Y':
			fmt.Fprintf(&sb, "%d", fi.ModTime().Unix())
		case '%':
			sb.WriteByte('%')
		default:
			sb.WriteByte('%')
			sb.WriteByte(f[i])
		}
	}
	return sb.String()
}

type sockInfo struct {
	proto, state, local, peer string
	recvQ, sendQ              int64
	pid                       int
	procName                  string
}

func cmdSs(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("ss", hc.Stderr)
	tcp := fs.Bool("t", false, "")
	udp := fs.Bool("u", false, "")
	listen := fs.Bool("l", false, "")
	numeric := fs.Bool("n", false, "")
	proc := fs.Bool("p", false, "")
	all := fs.Bool("a", false, "")
	fs.BoolVar(tcp, "tcp", false, "")
	fs.BoolVar(udp, "udp", false, "")
	fs.BoolVar(listen, "listening", false, "")
	fs.BoolVar(numeric, "numeric", false, "")
	fs.BoolVar(proc, "processes", false, "")
	fs.BoolVar(all, "all", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	_ = numeric
	wantState := ""
	rest := fs.Args()
	for i, a := range rest {
		if a == "state" && i+1 < len(rest) {
			wantState = strings.ToUpper(rest[i+1])
		}
	}
	if *listen {
		wantState = "LISTEN"
	}
	protos := []string{}
	if *tcp || (!*tcp && !*udp) {
		protos = append(protos, "tcp")
	}
	if *udp || (!*tcp && !*udp) {
		protos = append(protos, "udp")
	}
	socks, err := listSockets(protos)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "ss:", err)
		return exitError{1}
	}
	if wantState != "" {
		var f []sockInfo
		for _, s := range socks {
			st := strings.ToUpper(s.state)
			if st == wantState ||
				(wantState == "LISTENING" && st == "LISTEN") ||
				(wantState == "LISTEN" && st == "LISTENING") ||
				(wantState == "LISTEN" && s.proto == "udp") {
				f = append(f, s)
			}
		}
		socks = f
	} else if !*all {
		var f []sockInfo
		for _, s := range socks {
			st := strings.ToUpper(s.state)
			if st != "LISTEN" && st != "LISTENING" {
				f = append(f, s)
			}
		}
		socks = f
	}
	hdrProcess := ""
	if *proc {
		hdrProcess = " Process"
	}
	showNetid := len(protos) > 1
	if showNetid {
		fmt.Fprintf(hc.Stdout, "Netid  State   Recv-Q Send-Q  Local Address:Port  Peer Address:Port%s\n", hdrProcess)
	} else {
		fmt.Fprintf(hc.Stdout, "State   Recv-Q Send-Q  Local Address:Port  Peer Address:Port%s\n", hdrProcess)
	}
	for _, s := range socks {
		procCol := ""
		if *proc && s.pid > 0 {
			name := s.procName
			if name == "" {
				name = "unknown"
			}
			procCol = fmt.Sprintf(" users:((\"%s\",pid=%d))", name, s.pid)
		}
		if showNetid {
			fmt.Fprintf(hc.Stdout, "%-6s %-7s %-6d %-6d  %-18s %-18s%s\n",
				s.proto, s.state, s.recvQ, s.sendQ, s.local, s.peer, procCol)
		} else {
			fmt.Fprintf(hc.Stdout, "%-7s %-6d %-6d  %-18s %-18s%s\n",
				s.state, s.recvQ, s.sendQ, s.local, s.peer, procCol)
		}
	}
	return nil
}

func cmdId(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("id", hc.Stderr)
	uidOnly := fs.Bool("u", false, "")
	gidOnly := fs.Bool("g", false, "")
	name := fs.Bool("n", false, "")
	fs.BoolVar(name, "name", false, "")
	real := fs.Bool("r", false, "")
	fs.BoolVar(real, "real", false, "")
	fs.Bool("a", false, "")
	if err := fs.Parse(splitAttached("id", args)[1:]); err != nil {
		return err
	}
	uid, user, gid, group := identity()
	_ = real
	if *uidOnly {
		if *name {
			fmt.Fprintln(hc.Stdout, user)
		} else {
			fmt.Fprintln(hc.Stdout, uid)
		}
		return nil
	}
	if *gidOnly {
		if *name {
			fmt.Fprintln(hc.Stdout, group)
		} else {
			fmt.Fprintln(hc.Stdout, gid)
		}
		return nil
	}
	fmt.Fprintf(hc.Stdout, "uid=%d(%s) gid=%d(%s) groups=%d(%s)\n", uid, user, gid, group, gid, group)
	return nil
}

func cmdArch(_ context.Context, hc interp.HandlerContext, args []string) error {
	_ = args
	arch := map[string]string{
		"amd64": "x86_64", "arm64": "aarch64", "386": "i686",
	}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	fmt.Fprintln(hc.Stdout, arch)
	return nil
}

func cmdTty(_ context.Context, hc interp.HandlerContext, args []string) error {
	_ = args
	if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
		if name := ttyName(); name != "" {
			fmt.Fprintln(hc.Stdout, name)
			return nil
		}
	}
	fmt.Fprintln(hc.Stdout, "not a tty")
	return exitError{1}
}

func cmdLogname(_ context.Context, hc interp.HandlerContext, args []string) error {
	_ = args
	// GNU logname prints the utmp login name and fails when there is no
	// login session (e.g. non-interactive containers), even if $USER or
	// $LOGNAME is set. unish has no utmp access, so it succeeds only when
	// LOGNAME is explicitly present AND stdin is a real terminal (closest
	// observable proxy for an interactive login session; /dev/null is a
	// character device but never a terminal).
	loginTTY := term.IsTerminal(int(os.Stdin.Fd()))
	if v := shellGetenv(hc, "LOGNAME"); v != "" && loginTTY {
		if i := strings.LastIndexAny(v, `\/`); i >= 0 {
			v = v[i+1:]
		}
		if v != "" {
			fmt.Fprintln(hc.Stdout, v)
			return nil
		}
	}
	fmt.Fprintln(hc.Stderr, "logname: no login name")
	return exitError{1}
}

func cmdSync(_ context.Context, hc interp.HandlerContext, args []string) error {
	_ = args
	_ = hc
	syncDisks()
	return nil
}

func cmdNohup(ctx context.Context, hc interp.HandlerContext, args []string) error {
	rest := args[1:]
	if len(rest) > 0 && (rest[0] == "--" || strings.HasPrefix(rest[0], "--")) {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		fmt.Fprintln(hc.Stderr, "nohup: missing operand")
		return exitError{125}
	}
	return runNohupNice(ctx, hc, rest)
}

func cmdNice(ctx context.Context, hc interp.HandlerContext, args []string) error {
	rest := args[1:]
	adjust := 10
	var cmd []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "-n" && i+1 < len(rest) {
			if v, err := strconv.Atoi(rest[i+1]); err == nil {
				adjust = v
			}
			i++
			continue
		}
		if len(a) > 1 && a[0] == '-' && a[1] >= '0' && a[1] <= '9' {
			if v, err := strconv.Atoi(a[1:]); err == nil {
				adjust = v
				continue
			}
		}
		if strings.HasPrefix(a, "--adjustment=") {
			if v, err := strconv.Atoi(strings.TrimPrefix(a, "--adjustment=")); err == nil {
				adjust = v
			}
			continue
		}
		cmd = rest[i:]
		break
	}
	if len(cmd) == 0 {
		fmt.Fprintln(hc.Stdout, currentNice())
		return nil
	}
	setNice(adjust)
	return runNohupNice(ctx, hc, cmd)
}

func runNohupNice(ctx context.Context, hc interp.HandlerContext, cmdArgs []string) error {
	if extra := lookupExtra(cmdArgs[0]); extra != nil {
		return extra.main(ctx, hc, splitAttached(cmdArgs[0], cmdArgs))
	}
	return interp.DefaultExecHandler(2)(ctx, cmdArgs)
}

var _ = runtime.GOOS
var _ = filepath.Separator
var _ = io.Discard
