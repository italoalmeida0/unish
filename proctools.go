package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

func init() {
	extraCommands = append(extraCommands,
		extraCmd{"pgrep", cmdPgrep},
		extraCmd{"pkill", cmdPkill},
	)
}

func cmdPgrep(ctx context.Context, hc interp.HandlerContext, args []string) error {
	_ = ctx
	args = splitAttached("pgrep", args)
	fs := newFlagSet("pgrep", hc.Stderr)
	full := fs.Bool("f", false, "")
	ignoreCase := fs.Bool("i", false, "")
	listName := fs.Bool("l", false, "")
	exact := fs.Bool("x", false, "")
	count := fs.Bool("c", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(hc.Stderr, "pgrep: no matching criteria specified")
		return flag.ErrHelp
	}
	if len(rest) > 1 {
		fmt.Fprintln(hc.Stderr, "pgrep: only one pattern can be provided")
		return exitError{2}
	}
	pattern := rest[0]
	matched, err := matchProcs(pattern, *full, *ignoreCase, *exact)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "pgrep: %v\n", err)
		return exitError{2}
	}
	if *count {
		fmt.Fprintln(hc.Stdout, len(matched))
		if len(matched) == 0 {
			return exitError{1}
		}
		return nil
	}
	for _, p := range matched {
		if *listName {
			fmt.Fprintf(hc.Stdout, "%d %s\n", p.pid, procCmdName(p))
		} else {
			fmt.Fprintln(hc.Stdout, p.pid)
		}
	}
	if len(matched) == 0 {
		return exitError{1}
	}
	return nil
}

const pkillBoolFlags = "filxena"

// recoverPkillSplitArgs undoes unintended splitting by splitAttached("pkill", ...)
// caused by flagSpecs["pkill"].values = "signal" treating 'i','n','a','l','s' as
// short value-taking flags (e.g. "-if pat" -> "-i", "f", "pat"; "-sigterm pat" -> "-s", "igterm", "pat").
func recoverPkillSplitArgs(args []string) []string {
	if len(args) <= 2 {
		return args
	}
	hasTrailingOperand := func(from int) bool {
		for j := from; j < len(args); j++ {
			aj := args[j]
			if aj == "--" {
				return j+1 < len(args)
			}
			if aj == "-s" || aj == "--signal" || aj == "-g" {
				j++
				continue
			}
			if strings.HasPrefix(aj, "-") && len(aj) > 1 {
				continue
			}
			return true
		}
		return false
	}

	out := make([]string, 0, len(args))
	out = append(out, args[0])
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			next := args[i+1]
			if a == "-s" && strings.HasPrefix(strings.ToLower(next), "ig") &&
				isSignalName(strings.ToUpper("s"+next)) && hasTrailingOperand(i+2) {
				out = append(out, "-s"+next)
				i++
				continue
			}
			if (a == "-i" || a == "-n" || a == "-a" || a == "-l") && hasTrailingOperand(i+2) {
				out = append(out, a+next)
				i++
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

func cmdPkill(ctx context.Context, hc interp.HandlerContext, args []string) error {
	_ = ctx
	args = recoverPkillSplitArgs(args)
	sig := "TERM"
	var cleanArgs []string
	cleanArgs = append(cleanArgs, args[0])
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			cleanArgs = append(cleanArgs, args[i:]...)
			break
		}
		if a == "-s" || a == "--signal" || a == "-g" {
			cleanArgs = append(cleanArgs, a)
			if i+1 < len(args) {
				i++
				cleanArgs = append(cleanArgs, args[i])
			}
			continue
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && len(a) > 1 {
			body := a[1:]
			if allASCIIUint(body) {
				sig = body
				continue
			}
			up := strings.ToUpper(body)
			if isSignalName(up) {
				sig = strings.TrimPrefix(up, "SIG")
				continue
			}
			if len(body) > 1 && allCharsIn(body, pkillBoolFlags) {
				for j := 0; j < len(body); j++ {
					cleanArgs = append(cleanArgs, "-"+string(body[j]))
				}
				continue
			}
		}
		cleanArgs = append(cleanArgs, a)
	}
	fs := newFlagSet("pkill", hc.Stderr)
	full := fs.Bool("f", false, "")
	ignoreCase := fs.Bool("i", false, "")
	exact := fs.Bool("x", false, "")
	echo := fs.Bool("e", false, "")
	sigFlag := fs.String("signal", "", "")
	sigShort := fs.String("s", "", "")
	listSignals := fs.Bool("l", false, "")
	newest := fs.Bool("n", false, "")
	ancestors := fs.Bool("a", false, "")
	pgroup := fs.String("g", "", "")
	if err := fs.Parse(cleanArgs[1:]); err != nil {
		return err
	}
	if *listSignals {
		fmt.Fprintln(hc.Stdout, "HUP INT QUIT ILL TRAP ABRT BUS FPE KILL USR1 SEGV USR2 PIPE ALRM TERM")
		return nil
	}
	if *sigShort != "" {
		sig = strings.ToUpper(strings.TrimPrefix(strings.TrimPrefix(*sigShort, "-"), "SIG"))
	}
	if *sigFlag != "" {
		sig = strings.ToUpper(strings.TrimPrefix(strings.TrimPrefix(*sigFlag, "-"), "SIG"))
	}
	if !isValidKillSignal(sig) {
		fmt.Fprintf(hc.Stderr, "pkill: invalid signal: %s\n", sig)
		return exitError{1}
	}
	_ = ancestors
	rest := fs.Args()
	if *pgroup != "" {
		j := globalJobs.resolveJobSpec("%" + *pgroup)
		if j == nil || j.proc == nil {
			return exitError{1}
		}
		if sig == "0" {
			return nil
		}
		if err := killProc(j.proc, sig); err != nil {
			return exitError{1}
		}
		return nil
	}
	if len(rest) == 0 {
		fmt.Fprintln(hc.Stderr, "pkill: no matching criteria specified")
		return flag.ErrHelp
	}
	if len(rest) > 1 {
		fmt.Fprintln(hc.Stderr, "pkill: only one pattern can be provided")
		return exitError{2}
	}
	pattern := rest[0]
	matched, err := matchProcs(pattern, *full, *ignoreCase, *exact)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "pkill: %v\n", err)
		return exitError{2}
	}
	if *newest && len(matched) > 1 {
		newestProc := matched[0]
		for _, p := range matched[1:] {
			if p.pid > newestProc.pid {
				newestProc = p
			}
		}
		matched = []procInfo{newestProc}
	}
	if len(matched) == 0 {
		return exitError{1}
	}
	killed := 0
	for _, p := range matched {
		if sig == "0" {
			if runtime.GOOS == "windows" {
				killed++
				continue
			}
		}
		proc, err := os.FindProcess(p.pid)
		if err != nil {
			continue
		}
		if err := killProc(proc, sig); err == nil {
			killed++
			if *echo {
				fmt.Fprintf(hc.Stdout, "%s killed (pid %d)\n", procCmdName(p), p.pid)
			}
		}
	}
	if killed == 0 {
		return exitError{1}
	}
	return nil
}

func allASCIIUint(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func allCharsIn(s, set string) bool {
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(set, s[i]) < 0 {
			return false
		}
	}
	return true
}

func isSignalName(s string) bool {
	clean := strings.TrimPrefix(s, "SIG")
	switch clean {
	case "HUP", "INT", "QUIT", "ILL", "TRAP", "ABRT", "BUS", "FPE",
		"KILL", "USR1", "SEGV", "USR2", "PIPE", "ALRM", "TERM",
		"STOP", "CONT", "CHLD", "TSTP", "TTIN", "TTOU", "URG",
		"XCPU", "XFSZ", "VTALRM", "PROF", "WINCH", "IO", "SYS":
		return true
	}
	return false
}

func isValidKillSignal(sig string) bool {
	if isSignalName(sig) {
		return true
	}
	if allASCIIUint(sig) {
		n, err := strconv.Atoi(sig)
		return err == nil && n >= 0 && n <= 64
	}
	return false
}

func baseNameAnySlash(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func trimWinExe(name string) string {
	if len(name) > 4 && strings.EqualFold(name[len(name)-4:], ".exe") {
		return name[:len(name)-4]
	}
	return name
}

func procCmdName(p procInfo) string {
	if p.comm != "" {
		return p.comm
	}
	c := strings.TrimSpace(p.cmd)
	if strings.HasPrefix(c, "[") && strings.HasSuffix(c, "]") && len(c) > 2 {
		return c[1 : len(c)-1]
	}
	if i := strings.IndexAny(c, " \t"); i >= 0 {
		c = c[:i]
	}
	return baseNameAnySlash(c)
}

func matchProcs(pattern string, full, ignoreCase, exact bool) ([]procInfo, error) {
	procs, err := listProcs()
	if err != nil {
		return nil, err
	}
	return matchProcsFromList(procs, os.Getpid(), pattern, full, ignoreCase, exact)
}

func matchProcsFromList(procs []procInfo, myPid int, pattern string, full, ignoreCase, exact bool) ([]procInfo, error) {
	rePattern := pattern
	if exact {
		rePattern = "^(?:" + pattern + ")$"
	}
	if ignoreCase {
		rePattern = "(?i)" + rePattern
	}
	re, err := regexp.Compile(rePattern)
	if err != nil {
		return nil, err
	}
	var matched []procInfo
	for _, p := range procs {
		if p.pid == myPid {
			continue
		}
		if full {
			target := p.cmd
			if re.MatchString(target) {
				matched = append(matched, p)
				continue
			}
			if runtime.GOOS == "windows" && !strings.ContainsAny(target, " \t") {
				if trimmed := trimWinExe(target); trimmed != target && re.MatchString(trimmed) {
					matched = append(matched, p)
				}
			}
			continue
		}
		name := procCmdName(p)
		if re.MatchString(name) {
			matched = append(matched, p)
			continue
		}
		if runtime.GOOS == "windows" {
			if trimmed := trimWinExe(name); trimmed != name && re.MatchString(trimmed) {
				matched = append(matched, p)
			}
		}
	}
	return matched, nil
}

// parseProcStat parses a Linux /proc/[pid]/stat record safely, supporting
// process names containing spaces and parentheses and arbitrary page sizes.
func parseProcStat(s string, uptime float64, pageSize int) (comm, stat string, ppid int, cpu float64, vszKB, rssKB int64, cpuTime string, ok bool) {
	li := strings.IndexByte(s, '(')
	ri := strings.LastIndexByte(s, ')')
	if li < 0 || ri <= li {
		return "", "", 0, -1, -1, -1, "00:00:00", false
	}
	comm = s[li+1 : ri]
	rest := strings.Fields(s[ri+1:])
	if len(rest) < 2 {
		return comm, "?", 0, -1, -1, -1, "00:00:00", false
	}
	stat = rest[0]
	ppid, _ = strconv.Atoi(rest[1])
	cpu = -1
	vszKB = -1
	rssKB = -1
	cpuTime = "00:00:00"
	// rest[0] is field 3 (state); rest[21] is field 24 (rss), requiring len(rest) >= 22.
	if len(rest) > 21 {
		const clkTck = int64(100)
		utime, _ := strconv.ParseInt(rest[11], 10, 64)
		stime, _ := strconv.ParseInt(rest[12], 10, 64)
		startTicks, _ := strconv.ParseInt(rest[19], 10, 64)
		vsz, _ := strconv.ParseInt(rest[20], 10, 64)
		rss, _ := strconv.ParseInt(rest[21], 10, 64)
		pageKB := int64(pageSize) / 1024
		if pageKB <= 0 {
			pageKB = 4
		}
		vszKB = vsz / 1024
		rssKB = rss * pageKB
		totalTicks := utime + stime
		elapsed := uptime - float64(startTicks)/float64(clkTck)
		if elapsed > 0 {
			cpu = float64(totalTicks) / float64(clkTck) / elapsed * 100
		}
		mins := totalTicks / clkTck / 60
		secs := totalTicks / clkTck % 60
		cpuTime = fmt.Sprintf("%02d:%02d:%02d", mins/60, mins%60, secs)
	}
	return comm, stat, ppid, cpu, vszKB, rssKB, cpuTime, true
}

// parseProcBtime extracts the btime epoch seconds from Linux /proc/stat safely.
func parseProcBtime(data string) (int64, bool) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "btime" {
			if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

// parseDarwinProcArgs2 decodes a macOS sysctl kern.procargs2 buffer into
// the executable basename and space-joined argv command line.
func parseDarwinProcArgs2(raw []byte) (execBase, cmdLine string) {
	if len(raw) < 5 {
		return "", ""
	}
	argc := int(int32(binary.LittleEndian.Uint32(raw[:4])))
	if argc <= 0 || argc > 4096 {
		return "", ""
	}
	pos := 4
	endExec := pos
	for endExec < len(raw) && raw[endExec] != 0 {
		endExec++
	}
	if endExec >= len(raw) {
		return "", ""
	}
	execPath := string(raw[pos:endExec])
	execBase = baseNameAnySlash(execPath)
	pos = endExec
	for pos < len(raw) && raw[pos] == 0 {
		pos++
	}
	args := make([]string, 0, min(argc, 64))
	for len(args) < argc && pos < len(raw) {
		end := pos
		for end < len(raw) && raw[end] != 0 {
			end++
		}
		arg := string(raw[pos:end])
		args = append(args, arg)
		pos = end + 1
	}
	cmdLine = strings.TrimSpace(strings.Join(args, " "))
	return execBase, cmdLine
}
