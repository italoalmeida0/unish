package unish

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	exitStatus := 0
	for _, p := range paths {
		full := resolve(hc.Dir, p)
		if _, err := os.Stat(full); err != nil {
			fmt.Fprintf(hc.Stderr, "df: %s: No such file or directory\n", p)
			exitStatus = 1
			continue
		}
		total, free, avail, fstype, mount := diskUsage(full)
		used := total - free
		if used < 0 {
			used = 0
		}
		_ = fstype
		rows = append(rows, row{total: total, used: used, avail: avail, mount: mount})
	}
	if len(rows) > 0 {
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
	}
	if exitStatus != 0 {
		return exitError{exitStatus}
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
	pid, ppid, pgid int
	user            string
	comm            string
	cpu             float64
	memPct          float64
	vszKB           int64
	rssKB           int64
	tty             string
	stat            string
	start           string
	time            string
	cmd             string
}

func cmdPs(_ context.Context, hc interp.HandlerContext, args []string) error {
	bsd := false
	full := false
	var oformat string
	var pids []int
	hasPidFilter := false
	for i := 0; i < len(args[1:]); i++ {
		a := args[1:][i]
		if a == "aux" || a == "ax" {
			bsd = true
			continue
		}
		t := strings.TrimPrefix(a, "-")
		if a != t {
			if strings.HasPrefix(t, "-format=") {
				oformat = strings.TrimPrefix(t, "-format=")
				continue
			}
			// Walk cluster chars; -o/-p take a value from the rest of
			// the cluster or the next argument (GNU getopt behavior).
			for j := 0; j < len(t); j++ {
				c := t[j]
				switch c {
				case 'o':
					if j == len(t)-1 {
						if i+1 < len(args[1:]) {
							i++
							oformat = args[1:][i]
						}
					} else {
						oformat = t[j+1:]
					}
					j = len(t)
				case 'p':
					hasPidFilter = true
					if j == len(t)-1 {
						if i+1 < len(args[1:]) {
							i++
							pids = append(pids, psParsePids(args[1:][i])...)
						}
					} else {
						pids = append(pids, psParsePids(t[j+1:])...)
					}
					j = len(t)
				default:
					if c == 'f' || c == 'l' {
						full = true
					} else if c == 'u' {
						bsd = true
					}
				}
			}
		}
	}

	procs, err := listProcs()
	if err != nil {
		fmt.Fprintln(hc.Stderr, "ps:", err)
		return exitError{1}
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].pid < procs[j].pid })
	if hasPidFilter {
		want := make(map[int]bool, len(pids))
		for _, p := range pids {
			want[p] = true
		}
		kept := procs[:0]
		for _, p := range procs {
			if want[p.pid] {
				kept = append(kept, p)
			}
		}
		procs = kept
	}

	if oformat != "" {
		// GNU: -o overrides every other output mode.
		if err := psPrintFormat(hc, procs, oformat); err != nil {
			return err
		}
		if hasPidFilter && len(procs) == 0 {
			return exitError{1}
		}
		return nil
	}

	if bsd {
		fmt.Fprintln(hc.Stdout, "USER         PID %CPU %MEM    VSZ   RSS TTY      STAT START   TIME COMMAND")
		for _, p := range procs {
			fmt.Fprintf(hc.Stdout, "%-12s %5d %4s %4s %6s %5s %-8s %-4s %-7s %8s %s\n",
				p.user, p.pid, pctStr(p.cpu), pctStr(p.memPct),
				kbStr(p.vszKB), kbStr(p.rssKB), p.tty, p.stat, p.start, p.time, p.cmd)
		}
		if hasPidFilter && len(procs) == 0 {
			return exitError{1}
		}
		return nil
	}

	if full {
		fmt.Fprintln(hc.Stdout, "UID          PID    PPID  C STIME TTY          TIME CMD")
		for _, p := range procs {
			fmt.Fprintf(hc.Stdout, "%-12s %5d %5d  - %s %-12s %s %s\n",
				p.user, p.pid, p.ppid, p.start, p.tty, p.time, p.cmd)
		}
		if hasPidFilter && len(procs) == 0 {
			return exitError{1}
		}
		return nil
	}

	// GNU `ps` / `ps -e` default format lists "PID TTY TIME CMD".
	if len(args) == 1 && !hasPidFilter {
		fmt.Fprintln(hc.Stdout, "    PID TTY          TIME CMD")
		me := os.Getpid()
		fmt.Fprintf(hc.Stdout, "%7d %-12s %8s %s\n", me, "?", "00:00:00", "unish")
		return nil
	}
	fmt.Fprintln(hc.Stdout, "    PID TTY          TIME CMD")
	for _, p := range procs {
		fmt.Fprintf(hc.Stdout, "%7d %-12s %8s %s\n", p.pid, p.tty, p.time, p.cmd)
	}
	if hasPidFilter && len(procs) == 0 {
		return exitError{1}
	}
	return nil
}

// psParsePids splits a GNU PID list ("1,2 3") into integers.
func psParsePids(s string) []int {
	var out []int
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	}) {
		if n, err := strconv.Atoi(part); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// psPrintFormat prints the GNU ps -o column selection to hc.Stdout / hc.Stderr.
func psPrintFormat(hc interp.HandlerContext, procs []procInfo, oformat string) error {
	type col struct {
		head  string
		get   func(procInfo) string
		right bool
		wide  int
	}
	fields := map[string]col{
		"pid":     {"PID", func(p procInfo) string { return strconv.Itoa(p.pid) }, true, 7},
		"ppid":    {"PPID", func(p procInfo) string { return strconv.Itoa(p.ppid) }, true, 7},
		"user":    {"USER", func(p procInfo) string { return p.user }, false, 8},
		"uid":     {"USER", func(p procInfo) string { return p.user }, false, 8},
		"comm":    {"COMMAND", procCmdName, false, 0},
		"args":    {"COMMAND", func(p procInfo) string { return p.cmd }, false, 0},
		"cmd":     {"COMMAND", func(p procInfo) string { return p.cmd }, false, 0},
		"command": {"COMMAND", func(p procInfo) string { return p.cmd }, false, 0},
		"time":    {"TIME", func(p procInfo) string { return p.time }, true, 8},
		"cputime": {"TIME", func(p procInfo) string { return p.time }, true, 8},
		"stat":    {"STAT", func(p procInfo) string { return p.stat }, false, 4},
		"state": {"S", func(p procInfo) string {
			if p.stat != "" {
				return p.stat[:1]
			}
			return "?"
		}, false, 1},
		"tty":   {"TT", func(p procInfo) string { return p.tty }, false, 8},
		"%cpu":  {"%CPU", func(p procInfo) string { return pctStr(p.cpu) }, true, 5},
		"pcpu":  {"%CPU", func(p procInfo) string { return pctStr(p.cpu) }, true, 5},
		"%mem":  {"%MEM", func(p procInfo) string { return pctStr(p.memPct) }, true, 5},
		"pmem":  {"%MEM", func(p procInfo) string { return pctStr(p.memPct) }, true, 5},
		"vsz":   {"VSZ", func(p procInfo) string { return kbStr(p.vszKB) }, true, 7},
		"rss":   {"RSS", func(p procInfo) string { return kbStr(p.rssKB) }, true, 7},
		"start": {"STARTED", func(p procInfo) string { return p.start }, false, 7},
	}
	var sel []col
	for _, name := range strings.FieldsFunc(oformat, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	}) {
		c, ok := fields[strings.ToLower(name)]
		if !ok {
			fmt.Fprintf(hc.Stderr, "ps: unknown field name: %s\n", name)
			return exitError{1}
		}
		sel = append(sel, c)
	}
	if len(sel) == 0 {
		return nil
	}
	var sb strings.Builder
	for i, c := range sel {
		if i > 0 {
			sb.WriteByte(' ')
		}
		if c.right {
			fmt.Fprintf(&sb, "%*s", c.wide, c.head)
		} else if c.wide > 0 {
			sb.WriteString(fmt.Sprintf("%-*s", c.wide, c.head))
		} else {
			sb.WriteString(c.head)
		}
	}
	fmt.Fprintln(hc.Stdout, sb.String())
	for _, p := range procs {
		sb.Reset()
		for i, c := range sel {
			if i > 0 {
				sb.WriteByte(' ')
			}
			v := c.get(p)
			if c.right {
				fmt.Fprintf(&sb, "%*s", c.wide, v)
			} else if c.wide > 0 {
				sb.WriteString(fmt.Sprintf("%-*s", c.wide, v))
			} else {
				sb.WriteString(v)
			}
		}
		fmt.Fprintln(hc.Stdout, sb.String())
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
	used := mi.total - mi.free - mi.cached - mi.buffers
	if used < 0 {
		used = 0
	}
	swapUsed := mi.swapTotal - mi.swapFree
	if swapUsed < 0 {
		swapUsed = 0
	}
	fmt.Fprintln(hc.Stdout, "               total        used        free      shared  buff/cache   available")
	if *human {
		fmt.Fprintf(hc.Stdout, "Mem: %11s %11s %11s %11s %11s %11s\n",
			freeHuman(mi.total), freeHuman(used),
			freeHuman(mi.free), freeHuman(mi.shared),
			freeHuman(mi.cached+mi.buffers), freeHuman(mi.avail))
		fmt.Fprintf(hc.Stdout, "Swap:%11s %11s %11s\n",
			freeHuman(mi.swapTotal), freeHuman(swapUsed), freeHuman(mi.swapFree))
		return nil
	}
	fmt.Fprintf(hc.Stdout, "Mem: %11d %11d %11d %11d %11d %11d\n",
		mi.total/div, used/div, mi.free/div,
		mi.shared/div, (mi.cached+mi.buffers)/div, mi.avail/div)
	fmt.Fprintf(hc.Stdout, "Swap:%11d %11d %11d\n",
		mi.swapTotal/div, swapUsed/div, mi.swapFree/div)
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
	_ = args
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

// explicitEnviron carries an exact child environment list set by `env`,
// preventing nested `env` / `nice` / `nohup` invocations from resurrecting
// os.Environ() when `env -i` or `env -u` stripped parent variables.
type explicitEnviron struct {
	expand.Environ
	pairs   []string
	cleared bool
}

func currentEnvPairs(hc interp.HandlerContext) (pairs []string, cleared bool) {
	if ee, ok := hc.Env.(explicitEnviron); ok {
		return slices.Clone(ee.pairs), ee.cleared
	}
	return shellExecEnv(hc), false
}

func cmdEnv(ctx context.Context, hc interp.HandlerContext, args []string) error {
	ignore := false
	nullTerm := false
	var unsets []string
	idx := 1
	for idx < len(args) {
		a := args[idx]
		if a == "--" {
			idx++
			break
		}
		if a == "-" {
			ignore = true
			idx++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
		if strings.HasPrefix(a, "--") {
			switch {
			case a == "--ignore-environment":
				ignore = true
			case a == "--null":
				nullTerm = true
			case a == "--unset":
				if idx+1 >= len(args) {
					fmt.Fprintln(hc.Stderr, "env: option '--unset' requires an argument")
					return exitError{125}
				}
				idx++
				unsets = append(unsets, args[idx])
			case strings.HasPrefix(a, "--unset="):
				unsets = append(unsets, strings.TrimPrefix(a, "--unset="))
			default:
				fmt.Fprintf(hc.Stderr, "env: unrecognized option '%s'\n", a)
				return exitError{125}
			}
			idx++
			continue
		}
		// Short option cluster (-i, -0, -u NAME / -uNAME).
		cluster := a[1:]
		for j := 0; j < len(cluster); j++ {
			switch cluster[j] {
			case 'i':
				ignore = true
			case '0':
				nullTerm = true
			case 'u':
				if j+1 < len(cluster) {
					unsets = append(unsets, cluster[j+1:])
				} else {
					if idx+1 >= len(args) {
						fmt.Fprintln(hc.Stderr, "env: option requires an argument -- 'u'")
						return exitError{125}
					}
					idx++
					unsets = append(unsets, args[idx])
				}
				j = len(cluster)
			default:
				fmt.Fprintf(hc.Stderr, "env: invalid option -- '%c'\n", cluster[j])
				return exitError{125}
			}
		}
		idx++
	}

	rest := args[idx:]
	var envPairs []string
	var cmd []string
	for i, a := range rest {
		if eq := strings.IndexByte(a, '='); eq >= 0 && !strings.HasPrefix(a, "-") {
			k := a[:eq]
			if isEnvName(k) {
				envPairs = append(envPairs, a)
				continue
			}
		}
		cmd = rest[i:]
		break
	}

	childEnv := []string{}
	cleared := ignore
	if !ignore {
		var prevCleared bool
		childEnv, prevCleared = currentEnvPairs(hc)
		cleared = prevCleared
	}
	for _, u := range unsets {
		childEnv = unsetEnvVar(childEnv, u)
	}
	for _, kv := range envPairs {
		eq := strings.IndexByte(kv, '=')
		k, v := kv[:eq], kv[eq+1:]
		childEnv = setEnvVar(childEnv, k, v)
	}

	if len(cmd) == 0 {
		sep := "\n"
		if nullTerm {
			sep = "\x00"
		}
		for _, e := range childEnv {
			fmt.Fprintf(hc.Stdout, "%s%s", e, sep)
		}
		return nil
	}

	hc.Env = explicitEnviron{
		Environ: expand.ListEnviron(childEnv...),
		pairs:   childEnv,
		cleared: cleared,
	}
	return runSysSubcommand(ctx, hc, childEnv, cleared, 0, false, cmd)
}

func envKeyMatchCase(entry, name string, foldCase bool) bool {
	eq := strings.IndexByte(entry, '=')
	if eq < 0 {
		return false
	}
	k := entry[:eq]
	if foldCase {
		return strings.EqualFold(k, name)
	}
	return k == name
}

func envKeyMatch(entry, name string) bool {
	return envKeyMatchCase(entry, name, runtime.GOOS == "windows")
}

func setEnvVar(env []string, name, value string) []string {
	return setEnvVarCase(env, name, value, runtime.GOOS == "windows")
}

func setEnvVarCase(env []string, name, value string, foldCase bool) []string {
	pair := name + "=" + value
	replaced := false
	out := env[:0]
	for _, e := range env {
		if envKeyMatchCase(e, name, foldCase) {
			if !replaced {
				out = append(out, pair)
				replaced = true
			}
			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, pair)
	}
	return out
}

func unsetEnvVar(env []string, name string) []string {
	return unsetEnvVarCase(env, name, runtime.GOOS == "windows")
}

func unsetEnvVarCase(env []string, name string, foldCase bool) []string {
	out := env[:0]
	for _, e := range env {
		if !envKeyMatchCase(e, name, foldCase) {
			out = append(out, e)
		}
	}
	return out
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

func nativeIsLittleEndian() bool {
	var b [2]byte
	binary.NativeEndian.PutUint16(b[:], 1)
	return b[0] == 1
}

// parseProcNetAddr decodes a hex "IP:PORT" endpoint from Linux /proc/net/{tcp,udp}{,6}.
func parseProcNetAddr(hex string) string {
	return parseProcNetAddrEndian(hex, nativeIsLittleEndian())
}

func parseProcNetAddrEndian(hex string, littleEndian bool) string {
	parts := strings.Split(hex, ":")
	if len(parts) != 2 {
		return hex
	}
	port, err := strconv.ParseUint(parts[1], 16, 16)
	if err != nil {
		return hex
	}
	ipHex := parts[0]
	if len(ipHex) == 8 {
		var b [4]byte
		for i := 0; i < 4; i++ {
			v, err := strconv.ParseUint(ipHex[i*2:i*2+2], 16, 8)
			if err != nil {
				return hex
			}
			if littleEndian {
				b[3-i] = byte(v)
			} else {
				b[i] = byte(v)
			}
		}
		return fmt.Sprintf("%d.%d.%d.%d:%d", b[0], b[1], b[2], b[3], port)
	}
	if ip, err := parseProcIPv6HexEndian(ipHex, littleEndian); err == nil {
		return fmt.Sprintf("[%s]:%d", ip, port)
	}
	return hex
}

// parseProcIPv6HexEndian decodes a 32-hex-char IPv6 address from Linux
// /proc/net/{tcp6,udp6}, where the kernel formats in6_addr as four %08X
// host-byte-order 32-bit words.
func parseProcIPv6HexEndian(s string, littleEndian bool) (string, error) {
	if len(s) != 32 {
		return "", fmt.Errorf("bad ipv6 hex length")
	}
	var b [16]byte
	for w := 0; w < 4; w++ {
		for j := 0; j < 4; j++ {
			idx := w*4 + j
			v, err := strconv.ParseUint(s[idx*2:idx*2+2], 16, 8)
			if err != nil {
				return "", err
			}
			if littleEndian {
				b[w*4+(3-j)] = byte(v)
			} else {
				b[idx] = byte(v)
			}
		}
	}
	isV4Mapped := true
	for i := 0; i < 10; i++ {
		if b[i] != 0 {
			isV4Mapped = false
			break
		}
	}
	if isV4Mapped && b[10] == 0xff && b[11] == 0xff {
		return fmt.Sprintf("::ffff:%d.%d.%d.%d", b[12], b[13], b[14], b[15]), nil
	}
	return net.IP(b[:]).String(), nil
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
	silent := false
	for _, a := range args[1:] {
		if a == "-s" || a == "--silent" || a == "--quiet" {
			silent = true
		}
	}
	if f, ok := hc.Stdin.(*os.File); ok {
		fd := int(f.Fd())
		if term.IsTerminal(fd) {
			name := ttyNameFd(fd)
			if name == "" {
				name = f.Name()
			}
			if name != "" {
				if !silent {
					fmt.Fprintln(hc.Stdout, name)
				}
				return nil
			}
		}
	}
	if !silent {
		fmt.Fprintln(hc.Stdout, "not a tty")
	}
	return exitError{1}
}

func cmdLogname(_ context.Context, hc interp.HandlerContext, args []string) error {
	_ = args
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
	childEnv, cleared := currentEnvPairs(hc)
	return runSysSubcommand(ctx, hc, childEnv, cleared, 0, false, rest)
}

func cmdNice(ctx context.Context, hc interp.HandlerContext, args []string) error {
	rest := args[1:]
	adjust := 10
	var cmd []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "--" {
			cmd = rest[i+1:]
			break
		}
		if a == "-n" && i+1 < len(rest) {
			if v, err := strconv.Atoi(rest[i+1]); err == nil {
				adjust = v
			}
			i++
			continue
		}
		if len(a) > 1 && a[0] == '-' && (a[1] >= '0' && a[1] <= '9' || (a[1] == '-' && len(a) > 2)) {
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
	childEnv, cleared := currentEnvPairs(hc)
	return runSysSubcommand(ctx, hc, childEnv, cleared, adjust, true, cmd)
}

func findSubcommandExec(hc interp.HandlerContext, childEnv []string, cleared bool, name string) (string, error) {
	_ = cleared
	if strings.ContainsAny(name, `/\`) || (runtime.GOOS == "windows" && len(name) > 1 && name[1] == ':') {
		return findExec(hc, name)
	}
	var pathVal string
	hasPath := false
	for _, kv := range childEnv {
		if envKeyMatch(kv, "PATH") {
			eq := strings.IndexByte(kv, '=')
			pathVal = kv[eq+1:]
			hasPath = true
			break
		}
	}
	if !hasPath && runtime.GOOS != "windows" {
		pathVal = "/usr/bin:/bin"
	}
	for _, dir := range strings.Split(pathVal, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		searchDir := dir
		if !filepath.IsAbs(searchDir) {
			searchDir = resolve(hc.Dir, searchDir)
		}
		for _, cand := range execCandidates(searchDir, name) {
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand, nil
			}
		}
	}
	return "", fmt.Errorf("%s: No such file or directory", name)
}

func runSysSubcommand(ctx context.Context, hc interp.HandlerContext, childEnv []string, cleared bool, niceAdjust int, hasNice bool, cmdArgs []string) error {
	if extra := lookupExtra(cmdArgs[0]); extra != nil {
		return invokeExtra(ctx, hc, extra, cmdArgs)
	}
	if cmdArgs[0] == "printf" {
		return cmdPrintf(ctx, hc, cmdArgs)
	}
	path, err := findSubcommandExec(hc, childEnv, cleared, cmdArgs[0])
	if err != nil {
		fmt.Fprintf(hc.Stderr, "%v\n", err)
		return exitError{127}
	}
	if childEnv == nil {
		childEnv = []string{}
	}
	c := exec.CommandContext(ctx, path, cmdArgs[1:]...)
	c.Dir = hc.Dir
	c.Stdin = hc.Stdin
	c.Stdout = hc.Stdout
	c.Stderr = hc.Stderr
	c.Env = childEnv
	if err := c.Start(); err != nil {
		fmt.Fprintf(hc.Stderr, "%v\n", err)
		if os.IsNotExist(err) {
			return exitError{127}
		}
		return exitError{126}
	}
	if hasNice {
		setProcNice(c.Process.Pid, currentNice()+niceAdjust)
	}
	hc.ReportBgStart(c.Process.Pid)
	j := globalJobs.track(c, cmdArgs)
	<-j.done
	if j.err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if ee, ok := j.err.(*exec.ExitError); ok {
			if code := signalExitCode(ee); code >= 0 {
				return exitError{code}
			}
			return exitError{ee.ExitCode()}
		}
		return j.err
	}
	return nil
}

var _ = io.Discard
