package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

type extraCmd struct {
	name string
	main func(ctx context.Context, hc interp.HandlerContext, args []string) error
}

var extraCommands []extraCmd

func init() {
	extraCommands = []extraCmd{
		{"cat", cmdCat},
		{"ls", cmdLs},
		{"grep", cmdGrep},
		{"head", cmdHead},
		{"tail", cmdTail},
		{"sort", cmdSort},
		{"uniq", cmdUniq},
		{"wc", cmdWc},
		{"tee", cmdTee},
		{"tr", cmdTr},
		{"seq", cmdSeq},
		{"cut", cmdCut},
		{"paste", cmdPaste},
		{"comm", cmdComm},
		{"split", cmdSplit},
		{"diff", cmdDiff},
		{"cmp", cmdCmp},
		{"hexdump", cmdHexdump},
		{"strings", cmdStrings},
		{"dirname", cmdDirname},
		{"basename", cmdBasename},
		{"realpath", cmdRealpath},
		{"readlink", cmdReadlink},
		{"ln", cmdLn},
		{"du", cmdDu},
		{"sleep", cmdSleep},
		{"kill", cmdKill},
		{"pwd", cmdPwd},
		{"true", cmdTrue},
		{"false", cmdFalse},
		{"yes", cmdYes},
		{"which", cmdWhich},
		{"printenv", cmdPrintenv},
		{"whoami", cmdWhoami},
		{"nproc", cmdNproc},
		{"clear", cmdClear},
		{"echo", cmdEcho},
		{"date", cmdDate},
		{"uname", cmdUname},
		{"hostname", cmdHostname},
		{"md5sum", cmdMd5sum},
		{"sha1sum", cmdSha1sum},
		{"sha256sum", cmdSha256sum},
		{"shasum", cmdShasum},
		{"mktemp", cmdMktemp},
		{"find", cmdFind},
	}
}

func lookupExtra(name string) *extraCmd {
	for i := range extraCommands {
		if extraCommands[i].name == name {
			return &extraCommands[i]
		}
	}
	return nil
}

func extraHandler(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if len(args) == 0 {
			return next(ctx, args)
		}
		if args[0] == "ls" {
			args = filterLsArgs(args)
		}
		// Job-control builtins with real-pid semantics live here so
		// they bypass mvdan/sh's fake-id-only implementations.
		// (trackExec also routes them; this covers direct chains.)
		cmd := lookupExtra(args[0])
		if cmd == nil {
			// Not one of ours: run as an external with job tracking
			// so background jobs get real PIDs ($! killable/waitable).
			// Fall back to the next handler when tracking can't start
			// it (e.g. shell builtins like `command`, `exec`).
			if err, handled := runExternalTracked(ctx, args); handled {
				return err
			}
			return next(ctx, args)
		}
		for _, a := range args[1:] {
			if a == "--version" {
				hc := interp.HandlerCtx(ctx)
				fmt.Fprintf(hc.Stdout, "%s (unish coreutils) 1.0\n", args[0])
				return nil
			}
		}
		hc := interp.HandlerCtx(ctx)
		err := cmd.main(ctx, hc, splitAttached(args[0], args))
		if isPipeClosed(err) || isPipeClosed(ctx.Err()) {
			// SIGPIPE semantics: downstream closed the pipe early
			// (e.g. `seq 1 5 | head -n 0`). GNU tools exit 0 silently;
			// never surface "write |1: The pipe is being closed".
			return nil
		}
		if err != nil {
			switch {
			case ctx.Err() == context.DeadlineExceeded:
				return interp.NewExitStatus(124)
			case ctx.Err() == context.Canceled:
				return interp.NewExitStatus(130)
			}
			if ee, ok := err.(exitError); ok {
				return interp.NewExitStatus(uint8(ee.code))
			}
			if err == flag.ErrHelp {
				return interp.NewExitStatus(2)
			}
			// Go flag parse failures already printed "flag provided..." +
			// "Try 'X --help'..." to stderr: exit 2 like GNU, without an
			// extra "bash: ..." duplicate line (agents key off bash text).
			if msg := err.Error(); strings.HasPrefix(msg, "flag provided") ||
				strings.HasPrefix(msg, "flag needs an argument") ||
				strings.HasPrefix(msg, "invalid boolean") ||
				strings.HasPrefix(msg, "invalid value") {
				return interp.NewExitStatus(2)
			}
			return err
		}
		return nil
	}
}

// runExternalTracked starts args[0] as a real OS process with job-table
// tracking. Returns (err, true) when it handled the command; (nil, false)
// when the command is a shell builtin/function that must go through the
// normal handler chain.
func runExternalTracked(ctx context.Context, args []string) (error, bool) {
	hc := interp.HandlerCtx(ctx)
	// Never shadow shell builtins, functions, or our own commands:
	// those must keep their in-process semantics (env changes, etc.).
	if interp.IsBuiltin(args[0]) || lookupExtra(args[0]) != nil {
		return nil, false
	}
	path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
	if err != nil {
		return nil, false
	}
	cmd := exec.CommandContext(ctx, path, args[1:]...)
	cmd.Dir = hc.Dir
	cmd.Stdin = hc.Stdin
	cmd.Stdout = hc.Stdout
	cmd.Stderr = hc.Stderr
	// Exported shell variables, like the default handler.
	cmd.Env = shellExecEnv(hc)
	if err := cmd.Start(); err != nil {
		return nil, false
	}
	j := globalJobs.track(cmd, args)
	hashRecord(args[0], path)
	// Wait synchronously (foreground). Background statements run this
	// whole handler in a goroutine already, so no extra work needed.
	<-j.done
	if j.err != nil {
		if ee, ok := j.err.(*exec.ExitError); ok {
			return interp.NewExitStatus(uint8(ee.ExitCode())), true
		}
		return j.err, true
	}
	return nil, true
}

// shellExecEnv builds the environment for external processes from the
// shell's exported variables (mirrors interp's unexported execEnv).
// It starts from the OS environment so children (python, node, git...)
// never lose PATH/SystemRoot/etc when the shell has few exports, then
// overlays shell exports. On Windows it also forces UTF-8 stdio for
// children (PYTHONIOENCODING/PYTHONUTF8) unless the user already set
// them, so `print(emoji)` doesn't die with charmap/UnicodeEncodeError
// under cp1252/cp850 consoles — the classic Windows agent papercut.
// Shell exports always win over the injected defaults.
func shellExecEnv(hc interp.HandlerContext) []string {
	base := os.Environ()
	merged := make(map[string]string, len(base)+16)
	order := make([]string, 0, len(base)+16)
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k := kv[:i]
			if _, ok := merged[k]; !ok {
				order = append(order, k)
			}
			merged[k] = kv[i+1:]
		}
	}
	hc.Env.Each(func(name string, vr expand.Variable) bool {
		if vr.Exported && vr.IsSet() && vr.Kind == expand.String {
			if _, ok := merged[name]; !ok {
				order = append(order, name)
			}
			merged[name] = vr.Str
		}
		return true
	})
	if runtime.GOOS == "windows" {
		if _, ok := merged["PYTHONIOENCODING"]; !ok {
			order = append(order, "PYTHONIOENCODING")
			merged["PYTHONIOENCODING"] = "utf-8"
		}
		if _, ok := merged["PYTHONUTF8"]; !ok {
			order = append(order, "PYTHONUTF8")
			merged["PYTHONUTF8"] = "1"
		}
	}
	list := make([]string, 0, len(order))
	for _, k := range order {
		list = append(list, k+"="+merged[k])
	}
	return list
}

var lsColorMode = "never" // --color[=WHEN]: never/always/auto

func filterLsArgs(args []string) []string {
	out := args[:1:1]
	for _, a := range args[1:] {
		if a == "--color" {
			lsColorMode = "always" // GNU: WHEN omitted means always
			continue
		}
		if strings.HasPrefix(a, "--color=") {
			lsColorMode = strings.TrimPrefix(a, "--color=")
			continue
		}
		if a == "--hyperlink" || strings.HasPrefix(a, "--hyperlink=") ||
			a == "--group-directories-first" || a == "--indicator-style=classify" ||
			a == "--exclude" || strings.HasPrefix(a, "--exclude=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func callOverride(ctx context.Context, args []string) ([]string, error) {
	if len(args) == 0 {
		return args, nil
	}
	switch args[0] {
	case "kill":
		hc := interp.HandlerCtx(ctx)
		if err := cmdKill(ctx, hc, splitAttached("kill", args)); err != nil {
			return []string{"false"}, nil
		}
		return []string{"true"}, nil
	case "umask":
		hc := interp.HandlerCtx(ctx)
		if err := cmdUmask(ctx, hc, args); err != nil {
			return []string{"false"}, nil
		}
		return []string{"true"}, nil
	case "ulimit":
		hc := interp.HandlerCtx(ctx)
		if err := cmdUlimit(ctx, hc, args); err != nil {
			return []string{"false"}, nil
		}
		return []string{"true"}, nil
	case "printf":
		// Bypass mvdan/sh's printf (no precision specs like %.0s).
		hc := interp.HandlerCtx(ctx)
		if err := cmdPrintf(ctx, hc, args); err != nil {
			if ee, ok := err.(exitError); ok {
				return []string{"true"}, interp.BuiltinExit(uint8(ee.code))
			}
			return []string{"true"}, interp.BuiltinExit(1)
		}
		return []string{"true"}, nil
	case "hash":
		// Bypass mvdan/sh's hash (empty-table notice differs) with a
		// real implementation backed by the hash table below.
		hc := interp.HandlerCtx(ctx)
		if err := cmdHashTable(ctx, hc, args); err != nil {
			return []string{"false"}, nil
		}
		return []string{"true"}, nil
	case "type":
		hc := interp.HandlerCtx(ctx)
		if err := cmdTypeDesc(ctx, hc, args); err != nil {
			return []string{"false"}, nil
		}
		return []string{"true"}, nil
	case "jobs":
		// Bypass mvdan/sh's stub ("unsupported builtin") with a real
		// implementation backed by the job table.
		hc := interp.HandlerCtx(ctx)
		if err := cmdJobs(hc, args); err != nil {
			return []string{"false"}, nil
		}
		return []string{"true"}, nil
	case "history":
		// mvdan/sh declares history but never implements it
		// (falls into "unsupported builtin"); serve the live
		// interactive history instead.
		hc := interp.HandlerCtx(ctx)
		if err := cmdHistory(ctx, hc, args); err != nil {
			return []string{"false"}, nil
		}
		return []string{"true"}, nil
	case "test", "[":
		// Own GNU test implementation: the /dev/null family always
		// exists (Windows has no such nodes) and diagnostics match GNU.
		hc := interp.HandlerCtx(ctx)
		switch err := runTest(hc, args); {
		case err == nil:
			return []string{"true"}, nil
		case errors.Is(err, errTestFalse):
			return []string{"false"}, nil
		default:
			return []string{"true"}, interp.BuiltinExit(2)
		}
	case "wait":
		// Tracked externals wait here; everything else flows through
		// to mvdan/sh's builtin (in-process jobs, real pids, errors).
		// NOTE: ["true"]/["false"] stand in for exit 0/1 because a
		// CallHandler cannot return arbitrary codes; exact waited
		// statuses surface via `wait $!; echo $?` == 0/1 only.
		// Full-fidelity codes need `wait` as an ExecHandler — see
		// cmdWaitTracked note.
		if code, handled := cmdWaitTracked(ctx, args); handled {
			if code != 0 {
				return []string{"false"}, nil
			}
			return []string{"true"}, nil
		}
		return args, nil
	}
	return args, nil
}

func resolve(dir, p string) string {
	if p == "" || p == "-" || p == "/dev/stdin" || dir == "" {
		return p
	}
	if runtime.GOOS == "windows" {
		// MSYS-style paths (/c/x, /cygdrive/c/x, /tmp...) LOOK absolute
		// to filepath.IsAbs, but Windows can't open them: translate
		// before anything else.
		if len(p) > 0 && (p[0] == '/' || p[0] == '\\') {
			if w, ok := msysPath(p); ok {
				return w
			}
		}
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

var msysMountsOnce sync.Once
var msysMounts map[string]string // lowercase unix prefix -> windows root

// msysPath converts MSYS/Cygwin-style paths to Windows paths in a
// self-contained way, with no external dependency:
//
//   - /c/Users/x    -> C:\Users\x   (single-letter drive)
//   - /cygdrive/c/x -> C:\x          (cygdrive prefix)
//   - /tmp, /home.. -> resolved against /etc/fstab mounts
//     (longest-prefix match), falling back to the MSYS root
//   - /dev/null, /dev/stdin... -> NUL / handled by resolve() callers
//
// Unknown paths report "don't know" (false) and the caller falls back
// to a plain filepath.Join. Virtual paths (/proc, /sys, /dev/*) are
// intentionally not translated: they have no Windows equivalent.
func msysPath(p string) (string, bool) {
	return msysPathInline(p)
}

// msysPathInline converts purely in Go, without spawning a process.
func msysPathInline(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	// Normalize \ to / for analysis.
	s := strings.ReplaceAll(p, "\\", "/")
	if !strings.HasPrefix(s, "/") {
		return "", false
	}
	// /cygdrive/c/rest -> C:\rest
	if strings.HasPrefix(strings.ToLower(s), "/cygdrive/") {
		rest := s[len("/cygdrive/"):]
		if len(rest) >= 1 && isASCIILetter(rest[0]) {
			drive := strings.ToUpper(string(rest[0]))
			tail := rest[1:]
			tail = strings.TrimPrefix(tail, "/")
			if tail == "" {
				return drive + `:\`, true
			}
			return drive + `:\` + strings.ReplaceAll(tail, "/", `\`), true
		}
		return "", false
	}
	// /c/rest or /C -> C:\rest (single-letter drive).
	if len(s) >= 2 && s[0] == '/' && isASCIILetter(s[1]) && (len(s) == 2 || s[2] == '/') {
		drive := strings.ToUpper(string(s[1]))
		tail := ""
		if len(s) > 2 {
			tail = s[3:]
		}
		if tail == "" {
			return drive + `:\`, true
		}
		return drive + `:\` + strings.ReplaceAll(tail, "/", `\`), true
	}
	// Other absolute paths (/tmp, /home, custom mounts...): resolve
	// against /etc/fstab with longest-prefix match, falling back to
	// the MSYS root. Virtual paths (/proc, /sys, /dev/...) have no
	// Windows equivalent and are left untranslated.
	if w, ok := msysMountLookup(s); ok {
		return w, true
	}
	return "", false
}

// msysMountLookup resolves an absolute unix-style path against the
// MSYS mount table (parsed from /etc/fstab) with longest-prefix match.
// Returns false for virtual paths (/proc, /sys, /dev/...) and when no
// mount covers the path, so the caller never gets an invented path.
func msysMountLookup(s string) (string, bool) {
	msysMountsOnce.Do(func() {
		msysMounts = loadMsysMounts()
	})
	if isMsysVirtual(s) {
		return "", false
	}
	best := ""
	for mp := range msysMounts {
		if mp == "/" {
			continue
		}
		if s == mp || strings.HasPrefix(s, mp+"/") {
			if len(mp) > len(best) {
				best = mp
			}
		}
	}
	if best != "" {
		root := msysMounts[best]
		tail := strings.TrimPrefix(s[len(best):], "/")
		if tail == "" {
			return root, true
		}
		return root + `\` + strings.ReplaceAll(tail, "/", `\`), true
	}
	root, ok := msysMounts["/"]
	if !ok || root == "" {
		return "", false
	}
	tail := strings.TrimPrefix(s, "/")
	if tail == "" {
		return root, true
	}
	return root + `\` + strings.ReplaceAll(tail, "/", `\`), true
}

// isMsysVirtual reports MSYS virtual paths with no Windows equivalent.
func isMsysVirtual(s string) bool {
	if s == "/proc" || strings.HasPrefix(s, "/proc/") ||
		s == "/sys" || strings.HasPrefix(s, "/sys/") ||
		s == "/dev" || strings.HasPrefix(s, "/dev/") {
		return true
	}
	return false
}

// loadMsysMounts parses the MSYS /etc/fstab mount table into
// unix-prefix -> windows-root entries. Every real mount is kept so
// custom mounts resolve by longest-prefix match; the "none / cygdrive"
// control line is skipped. With no MSYS at all, the AX scratch paths
// (/tmp, /var/tmp, /temp, /scratch, /var/log) still resolve to the OS
// temp dir (agent experience: scratch must work on a bare machine)
// while every other MSYS-only path reports "don't know".
// No external process is spawned.
func loadMsysMounts() map[string]string {
	m := map[string]string{}
	// No PATH lookup, no subprocess: probe the well-known install
	// locations directly. Extra roots can be added via UNISH_MSYS_ROOT.
	knownRoots := []string{`C:\tools\msys64`, `C:\msys64`}
	cands := []string{}
	if extra := os.Getenv("UNISH_MSYS_ROOT"); extra != "" {
		cands = append(cands, filepath.Join(extra, "etc", "fstab"))
	}
	for _, r := range knownRoots {
		cands = append(cands, filepath.Join(r, "etc", "fstab"))
	}
	for _, f := range cands {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, ln := range strings.Split(string(data), "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" || strings.HasPrefix(ln, "#") {
				continue
			}
			f := strings.Fields(ln)
			if len(f) < 2 {
				continue
			}
			win, mp := f[0], strings.ToLower(f[1])
			if win == "none" || win == "cygdrive" || !strings.HasPrefix(mp, "/") {
				continue // control lines (cygdrive prefix), not mounts
			}
			root := strings.ReplaceAll(win, "/", `\`)
			if _, dup := m[mp]; !dup {
				m[mp] = root
			}
		}
	}
	if len(m) > 0 {
		// Custom fstab without scratch entries: still pin the AX
		// scratch paths (see pinAX) so they always work.
		pinAX(m)
		return m
	}
	// No fstab (or no usable entries): the MSYS install dir itself is
	// the "/" root. UNISH_MSYS_ROOT wins over the known locations.
	// Either way, the AX scratch paths are pinned so agents always
	// have working scratch dirs, even on machines without MSYS.
	if extra := os.Getenv("UNISH_MSYS_ROOT"); extra != "" {
		if fi, err := os.Stat(extra); err == nil && fi.IsDir() {
			m["/"] = extra
			pinAX(m)
			return m
		}
	}
	for _, r := range knownRoots {
		if fi, err := os.Stat(r); err == nil && fi.IsDir() {
			m["/"] = r
			pinAX(m)
			return m
		}
	}
	// Bare machine, no MSYS anywhere: only the AX scratch paths
	// resolve; everything else stays untranslated.
	pinAX(m)
	return m
}

// pinAX pins the agent-experience scratch paths to the OS temp dir so
// they always work, even on machines without MSYS:
//
//   - /tmp, /var/tmp, /temp, /scratch -> %TEMP% (flat scratch)
//   - /var/log -> %TEMP%\unish-log (writable log sink; the real
//     C:\Windows\Logs needs elevation, useless for agents)
//
// Everything else stays strict: unknown paths report "don't know"
// instead of landing somewhere unexpected.
func pinAX(m map[string]string) {
	if tmp := os.TempDir(); tmp != "" {
		for _, mp := range []string{"/tmp", "/var/tmp", "/temp", "/scratch"} {
			if _, dup := m[mp]; !dup {
				m[mp] = tmp
			}
		}
		if _, dup := m["/var/log"]; !dup {
			ld := filepath.Join(tmp, "unish-log")
			// Create once so `cmd >> /var/log/x` never fails on a
			// fresh machine; ignore errors (fallback: open fails
			// loudly like bash would).
			_ = os.MkdirAll(ld, 0o755)
			m["/var/log"] = ld
		}
	}
}

func shellGetenv(hc interp.HandlerContext, key string) string {
	if v := hc.Env.Get(key); v.IsSet() {
		return v.Str
	}
	return os.Getenv(key)
}

// shellOpenHandler is mvdan/sh's DefaultOpenHandler with self-contained
// MSYS translation: redirections (>, >>, <) and internals that open files
// go through here, so `/tmp/x`, `/c/...` work in redirections.
// Keeps the /dev/null -> NUL workaround from the default handler.
func shellOpenHandler() interp.OpenHandlerFunc {
	return func(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
		mc := interp.HandlerCtx(ctx)
		if runtime.GOOS == "windows" && path == "/dev/null" {
			path = "NUL"
			flag &^= os.O_TRUNC
		} else if path != "" {
			path = resolve(mc.Dir, path)
		}
		return os.OpenFile(path, flag, perm)
	}
}

type flagSpec struct {
	bools  string
	values string
	long   map[string]string
}

var flagSpecs = map[string]flagSpec{
	"grep": {
		bools:  "clFivnhqrRaEoxwsH",
		values: "emABCf",
		long: map[string]string{
			"count": "c", "files-with-matches": "l", "fixed-strings": "F",
			"ignore-case": "i", "invert-match": "v", "line-number": "n",
			"no-filename": "h", "quiet": "q", "silent": "q", "recursive": "r",
			"regexp": "e", "max-count": "m", "after-context": "A",
			"before-context": "B", "context": "C", "only-matching": "o",
			"extended-regexp": "E", "file": "f", "exclude": "exclude",
			"include": "include", "exclude-dir": "exclude-dir", "color": "",
			"line-regexp": "x", "word-regexp": "w", "with-filename": "H",
			"no-messages": "s",
			"label":       "label",
		},
	},
	"head": {bools: "qv", values: "nc", long: map[string]string{
		"lines": "n", "bytes": "c", "quiet": "q", "silent": "q", "verbose": "v",
	}},
	"tail": {bools: "qvf", values: "nc", long: map[string]string{
		"lines": "n", "bytes": "c", "quiet": "q", "silent": "q",
		"verbose": "v", "follow": "f",
	}},
	"sort": {bools: "rufnczsVbdighM", values: "tko", long: map[string]string{
		"reverse": "r", "unique": "u", "ignore-case": "f", "numeric-sort": "n",
		"check": "c", "key": "k", "zero-terminated": "z",
		"stable": "s", "version-sort": "V", "output": "o",
		"ignore-leading-blanks": "b", "field-separator": "t",
		"dictionary-order": "d", "ignore-nonprinting": "i",
		"general-numeric-sort": "g", "human-numeric-sort": "h",
		"month-sort": "M",
	}},
	"uniq": {bools: "cduig", values: "fsw", long: map[string]string{
		"count": "c", "repeated": "d", "unique": "u", "ignore-case": "i",
		"skip-fields": "f", "skip-chars": "s", "check-chars": "w",
		"group": "g",
	}},
	"wc": {bools: "lwcmL", values: "", long: map[string]string{
		"lines": "l", "words": "w", "bytes": "c", "chars": "m",
		"max-line-length": "L",
	}},
	"tee": {bools: "a", values: "", long: map[string]string{"append": "a"}},
	"tr": {bools: "dsct", values: "", long: map[string]string{
		"delete": "d", "squeeze-repeats": "s", "complement": "c",
		"truncate-set1": "t",
	}},
	"cut": {bools: "sn", values: "dfcb", long: map[string]string{
		"delimiter": "d", "fields": "f", "characters": "c", "bytes": "b",
		"only-delimited": "s", "complement": "complement",
		"output-delimiter": "output-delimiter",
	}},
	"paste": {bools: "s", values: "d", long: map[string]string{
		"delimiters": "d", "serial": "s",
	}},
	"cat": {bools: "nbEsTvA", values: "", long: map[string]string{
		"number": "n", "number-nonblank": "b", "squeeze-blank": "s",
		"show-ends": "E", "show-tabs": "T", "show-all": "A",
	}},
	"ls": {bools: "alhdRFQSrt1mxCi", values: "", long: map[string]string{
		"all": "a", "long": "l", "human-readable": "h", "directory": "d",
		"recursive": "R", "classify": "F", "quoted": "Q",
		"size": "S", "reverse": "r", "time": "t",
		"full-time": "full-time", "time-style": "time-style",
	}},
	"comm": {bools: "123", values: ""},
	"split": {bools: "dv", values: "labn", long: map[string]string{
		"lines": "l", "suffix-length": "a", "bytes": "b", "number": "n",
		"verbose": "v",
	}},
	"diff": {bools: "qu", values: "L", long: map[string]string{
		"brief": "q", "report-identical-files": "s", "unified": "u",
		"label": "L",
	}},
	"cmp": {bools: "sl", values: "n", long: map[string]string{
		"silent": "s", "quiet": "s",
	}},
	"id": {bools: "ugnra", values: "", long: map[string]string{
		"name": "n", "real": "r",
	}},
	"du": {bools: "shb", values: "", long: map[string]string{
		"summarize": "s", "human-readable": "h", "bytes": "b",
	}},
	"df": {bools: "hkP", values: "", long: map[string]string{
		"human-readable": "h",
	}},
	"ps":   {bools: "aefu", values: "", long: map[string]string{}},
	"free": {bools: "mhg", values: "", long: map[string]string{}},
	"stat": {bools: "", values: "c", long: map[string]string{}},
	"ss": {bools: "tulnpa", values: "", long: map[string]string{
		"tcp": "t", "udp": "u", "listening": "l", "numeric": "n", "processes": "p", "all": "a",
	}},
	"pgrep": {bools: "filxc", values: "", long: map[string]string{
		"full": "f", "ignore-case": "i", "list-name": "l", "exact": "x", "count": "c",
	}},
	"pkill": {bools: "filxe", values: "signal", long: map[string]string{
		"full": "f", "ignore-case": "i", "exact": "x", "echo": "e", "signal": "signal",
	}},
	"nc": {bools: "zvul", values: "wp", long: map[string]string{
		"zero": "z", "verbose": "v", "udp": "u", "listen": "l",
	}},
	"netcat": {bools: "zvul", values: "wp", long: map[string]string{
		"zero": "z", "verbose": "v", "udp": "u", "listen": "l",
	}},
	"env": {bools: "i", values: "", long: map[string]string{}},
	"nice": {bools: "", values: "n", long: map[string]string{
		"adjustment": "n",
	}},
	"cp": {bools: "rfvnPd", values: "", long: map[string]string{
		"recursive": "r", "force": "f", "verbose": "v", "no-clobber": "n",
		"no-dereference": "P",
	}},
	"mv": {bools: "funv", values: "", long: map[string]string{
		"force": "f", "update": "u", "no-clobber": "n", "verbose": "v",
	}},
	"rm": {bools: "rfv", values: "", long: map[string]string{
		"recursive": "r", "force": "f", "verbose": "v",
	}},
	"mkdir": {bools: "pv", values: "m", long: map[string]string{
		"parents": "p", "verbose": "v",
	}},
	"touch": {bools: "cm", values: "dr", long: map[string]string{
		"no-create": "c", "date": "d", "reference": "r",
	}},
	"chmod": {bools: "Rv", values: "", long: map[string]string{
		"recursive": "R", "verbose": "v",
	}},
	"xargs": {bools: "t0r", values: "nIPd", long: map[string]string{
		"no-run-if-empty": "r", "null": "0", "max-procs": "P", "delimiter": "d",
	}},
	"sed": {bools: "nEr", values: "ef", long: map[string]string{
		"quiet": "n", "silent": "n", "expression": "e", "file": "f",
		"regexp-extended": "E",
	}},
	"timeout": {bools: "p", values: "sk", long: map[string]string{
		"preserve-status": "p", "signal": "s", "kill-after": "k",
	}},
	"shasum": {bools: "bt", values: "a", long: map[string]string{
		"algorithm": "a", "binary": "b", "text": "t",
	}},
	"base64": {bools: "di", values: "w", long: map[string]string{
		"decode": "d", "wrap": "w", "ignore-garbage": "i",
	}},
	"tar": {bools: "cxtzvkP", values: "fC", long: map[string]string{
		"keep-old-files": "k", "absolute-names": "P", "exclude": "exclude",
		"directory": "C", "file": "f", "gzip": "z", "verbose": "v",
	}},
	"gzip": {bools: "kcf", values: "", long: map[string]string{
		"keep": "k", "stdout": "c", "force": "f",
	}},
	"gunzip": {bools: "kcf", values: "", long: map[string]string{
		"keep": "k", "stdout": "c", "force": "f",
	}},
	"mktemp": {bools: "dut", values: "p", long: map[string]string{
		"directory": "d", "tmpdir": "p", "dry-run": "u",
	}},
	"ln": {bools: "sf", values: "", long: map[string]string{
		"symbolic": "s", "force": "f",
	}},
	"date": {bools: "u", values: "dr", long: map[string]string{"universal": "u", "utc": "u", "date": "d", "reference": "r"}},
	"uname": {bools: "amnrsvpi", values: "", long: map[string]string{
		"all": "a", "machine": "m", "nodename": "n",
		"release": "r", "sysname": "s",
		"version": "v", "processor": "p", "hardware-platform": "i",
	}},
	"hexdump": {bools: "Cv", values: "nse", long: map[string]string{
		"canonical": "C",
	}},
	"strings": {bools: "ao", values: "nt", long: map[string]string{
		"radix": "t", "bytes": "n",
	}},
	"md5sum":    {bools: "btcqsz", values: "", long: map[string]string{"check": "c", "quiet": "q", "status": "s", "strict": "z", "tag": "tag", "warn": "w"}},
	"sha1sum":   {bools: "btcqsz", values: "", long: map[string]string{"check": "c", "quiet": "q", "status": "s", "strict": "z", "tag": "tag", "warn": "w"}},
	"sha256sum": {bools: "btcqsz", values: "", long: map[string]string{"check": "c", "quiet": "q", "status": "s", "strict": "z", "tag": "tag", "warn": "w"}},
	"readlink":  {bools: "fm", values: ""},
	"realpath":  {bools: "em", values: ""},
	"basename":  {bools: "a", values: "s"},
	"od": {bools: "Anvxcd", values: "tNfwjA", long: map[string]string{
		"format": "t", "address-radix": "A", "output-duplicates": "",
		"read-bytes": "N", "skip-bytes": "j", "width": "w",
		"strings": "S",
	}},
	"nl": {bools: "", values: "biwsvn", long: map[string]string{
		"body-numbering": "b", "line-increment": "i", "number-width": "w",
		"number-separator": "s", "starting-line-number": "v",
		"number-format": "n",
	}},
	"tac": {bools: "br", values: "s", long: map[string]string{
		"before": "b", "regex": "r", "separator": "s",
	}},
	"rev": {bools: "", values: ""},
	"fold": {bools: "sb", values: "w", long: map[string]string{
		"width": "w", "spaces": "s", "bytes": "b",
	}},
	"expand": {bools: "", values: "t", long: map[string]string{"tabs": "t"}},
	"unexpand": {bools: "a", values: "t", long: map[string]string{
		"tabs": "t", "all": "a", "first-only": "first-only",
	}},
	"join": {bools: "i", values: "12jateov", long: map[string]string{
		"ignore-case": "i",
	}},
}

func splitAttached(cmd string, args []string) []string {
	spec, ok := flagSpecs[cmd]
	if !ok {
		return args
	}
	var movable, rest []string
	movable = append(movable, args[0])
	argv := args[1:]
	endFlags := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if endFlags {
			rest = append(rest, a)
			continue
		}
		if a == "--" {
			endFlags = true
			rest = append(rest, a)
			continue
		}
		if strings.HasPrefix(a, "--") && len(a) > 2 {
			name := a[2:]
			val := ""
			hasVal := false
			if i := strings.IndexByte(name, '='); i >= 0 {
				val, hasVal = name[i+1:], true
				name = name[:i]
			}
			short, known := spec.long[name]
			if !known {
				rest = append(rest, a)
				continue
			}
			if short == "" {
				continue
			}
			if len(short) > 1 {
				if hasVal {
					movable = append(movable, "--"+short, val)
				} else if i+1 < len(argv) {
					movable = append(movable, "--"+short, argv[i+1])
					i++
				} else {
					rest = append(rest, "--"+short)
				}
				continue
			}
			if strings.ContainsRune(spec.values, rune(short[0])) {
				if hasVal {
					movable = append(movable, "-"+short, val)
				} else if i+1 < len(argv) {
					movable = append(movable, "-"+short, argv[i+1])
					i++
				} else {
					rest = append(rest, "-"+short)
				}
			} else {
				movable = append(movable, "-"+short)
				if hasVal {
					rest = append(rest, val)
				}
			}
			continue
		}
		if len(a) > 2 && a[0] == '-' && a[1] != '-' &&
			strings.ContainsRune(spec.values, rune(a[1])) {
			movable = append(movable, "-"+string(a[1]), a[2:])
			continue
		}
		// Value flags with negative-number values: "-n -1" (head/tail).
		// Go's flag package would treat "-1" as a flag; glue it as the
		// value so `head -n -1` / `tail -n -1` parse like GNU.
		if len(a) == 2 && a[0] == '-' && strings.ContainsRune(spec.values, rune(a[1])) &&
			i+1 < len(argv) && len(argv[i+1]) > 1 && argv[i+1][0] == '-' && argv[i+1][1] >= '0' && argv[i+1][1] <= '9' {
			movable = append(movable, a, argv[i+1])
			i++
			continue
		}
		if len(a) > 2 && a[0] == '-' && a[1] != '-' && isFlagChar(cmd, a[1]) {
			if expanded, ok := cascadeBools(spec, a[1:]); ok {
				movable = append(movable, expanded...)
				continue
			}
		}
		rest = append(rest, a)
	}
	return append(movable, rest...)
}

func isFlagChar(cmd string, c byte) bool {
	if isASCIILetter(c) {
		return true
	}
	// Combined shorts may include digits (ls -1a, comm -12, head -2).
	if c >= '0' && c <= '9' {
		return true
	}
	return false
}

func cascadeBools(spec flagSpec, s string) ([]string, bool) {
	var out []string
	for i := 0; i < len(s); i++ {
		c := s[i]
		// Digit-combined shorts (ls -1a, head -2): the digit is a
		// standalone bool occurrence; keep it as "-1" etc.
		if c >= '0' && c <= '9' {
			if strings.ContainsRune(spec.bools, rune(c)) {
				out = append(out, "-"+string(c))
				continue
			}
			// Unknown digit inside a cluster: split off the rest as
			// positional (bash parity: `ls -1a` works even when the
			// cluster mixes digit and letter flags).
			if i > 0 {
				return append(out, "-"+s[i:]), true
			}
			return nil, false
		}
		if strings.ContainsRune(spec.bools, rune(c)) {
			out = append(out, "-"+string(c))
			continue
		}
		if strings.ContainsRune(spec.values, rune(c)) {
			out = append(out, "-"+string(c))
			if i+1 < len(s) {
				out = append(out, s[i+1:])
			}
			return out, true
		}
		return nil, false
	}
	return out, true
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func parseN(args []string, def uint64) (uint64, []string) {
	n := def
	rest := args[:0:0]
	skipNext := false
	for _, a := range args {
		// Don't treat the VALUE of -n/-c (e.g. "-1" in "-n -1") as an
		// attached count; it belongs to the flag.
		if skipNext {
			skipNext = false
			rest = append(rest, a)
			continue
		}
		if a == "-n" || a == "-c" {
			skipNext = true
			rest = append(rest, a)
			continue
		}
		if len(a) > 1 && a[0] == '-' && a[1] >= '0' && a[1] <= '9' {
			if v, err := parseUint(a[1:]); err == nil {
				n = v
				continue
			}
		}
		rest = append(rest, a)
	}
	return n, rest
}

func parseUint(s string) (uint64, error) {
	var n uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + uint64(c-'0')
	}
	return n, nil
}

func openInputs(dir string, args []string, stdin io.Reader) (readers []io.Reader, names []string, closeAll func(), err error) {
	if len(args) == 0 {
		return []io.Reader{stdin}, []string{""}, func() {}, nil
	}
	var files []io.Closer
	closeAll = func() {
		for _, f := range files {
			f.Close()
		}
	}
	for _, a := range args {
		if a == "-" || a == "/dev/stdin" {
			readers = append(readers, stdin)
			names = append(names, "standard input")
			continue
		}
		p := resolve(dir, a)
		f, e := openShellFile(p)
		if e != nil {
			closeAll()
			return nil, nil, func() {}, e
		}
		files = append(files, f)
		readers = append(readers, f)
		names = append(names, a)
	}
	return readers, names, closeAll, nil
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	// bash parity: never dump "Usage of X:" (GNU prints a short hint).
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Try '%s --help' for more information.\n", name)
	}
	return fs
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdCat(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("cat", hc.Stderr)
	numAll := fs.Bool("n", false, "")
	numNonBlank := fs.Bool("b", false, "")
	squeeze := fs.Bool("s", false, "")
	showEnds := fs.Bool("E", false, "")
	showTabs := fs.Bool("T", false, "")
	showNonprint := fs.Bool("v", false, "")
	showAll := fs.Bool("A", false, "")
	fs.BoolVar(numAll, "number", false, "")
	fs.BoolVar(numNonBlank, "number-nonblank", false, "")
	fs.BoolVar(squeeze, "squeeze-blank", false, "")
	fs.BoolVar(showEnds, "show-ends", false, "")
	fs.BoolVar(showTabs, "show-tabs", false, "")
	fs.BoolVar(showNonprint, "show-nonprinting", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *showAll {
		*showEnds, *showTabs, *showNonprint = true, true, true
	}
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "cat:", err)
		return exitError{1}
	}
	defer closeAll()
	if !*numAll && !*numNonBlank && !*squeeze && !*showEnds && !*showTabs && !*showNonprint {
		for _, r := range readers {
			if _, err := io.Copy(hc.Stdout, r); err != nil {
				fmt.Fprintln(hc.Stderr, "cat:", err)
				return exitError{1}
			}
		}
		return nil
	}
	ln := 0
	prevBlank := false
	for _, r := range readers {
		sc := newLineReader(r)
		for sc.Scan() {
			line := sc.Text()
			blank := line == ""
			if *squeeze && blank && prevBlank {
				continue
			}
			prevBlank = blank
			out := line
			if *showNonprint {
				out = catVisible(out)
			}
			if *showTabs {
				out = strings.ReplaceAll(out, "\t", "^I")
			}
			if *showEnds && sc.EndedWithNewline() {
				out += "$"
			}
			nl := "\n"
			if !sc.EndedWithNewline() {
				nl = ""
			}
			number := *numAll || (*numNonBlank && !blank)
			if number {
				ln++
				fmt.Fprintf(hc.Stdout, "%6d\t%s%s", ln, out, nl)
			} else {
				fmt.Fprint(hc.Stdout, out+nl)
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "cat:", err)
			return exitError{1}
		}
	}
	return nil
}

// catVisible renders control and 8-bit bytes like GNU cat -v: ^X for
// controls, ^? for DEL, M-x / M-^X for high-bit bytes.
func catVisible(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == 127:
			b.WriteString("^?")
		case c < 32:
			b.WriteByte('^')
			b.WriteByte(c + 64)
		case c >= 128:
			b.WriteString("M-")
			x := c - 128
			switch {
			case x == 127:
				b.WriteString("^?")
			case x < 32:
				b.WriteByte('^')
				b.WriteByte(x + 64)
			default:
				b.WriteByte(x)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func cmdPwd(_ context.Context, hc interp.HandlerContext, args []string) error {
	_ = args
	fmt.Fprintln(hc.Stdout, hc.Dir)
	return nil
}

// ---- test / [ builtin ----

var errTestFalse = errors.New("test false")
var errTestUsage = errors.New("test usage error")

// runTest implements the test/[ builtin with GNU semantics, including
// the /dev/null device family (which does not exist on Windows).
// A nil return means the expression is true.
func runTest(hc interp.HandlerContext, args []string) error {
	a := args[1:]
	if args[0] == "[" {
		if len(a) == 0 || a[len(a)-1] != "]" {
			fmt.Fprintln(hc.Stderr, "[: missing `]'")
			return errTestUsage
		}
		a = a[:len(a)-1]
	}
	if len(a) == 0 {
		return errTestFalse
	}
	t := &testParser{hc: hc, toks: a}
	ok, err := t.orExpr()
	if err != nil {
		fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], err)
		return errTestUsage
	}
	if t.pos != len(t.toks) {
		fmt.Fprintf(hc.Stderr, "%s: too many arguments\n", args[0])
		return errTestUsage
	}
	if !ok {
		return errTestFalse
	}
	return nil
}

type testParser struct {
	hc   interp.HandlerContext
	toks []string
	pos  int
}

func (t *testParser) peek() string {
	if t.pos < len(t.toks) {
		return t.toks[t.pos]
	}
	return ""
}

func (t *testParser) orExpr() (bool, error) {
	left, err := t.andExpr()
	if err != nil {
		return false, err
	}
	for t.peek() == "-o" {
		t.pos++
		right, err := t.andExpr()
		if err != nil {
			return false, err
		}
		left = left || right
	}
	return left, nil
}

func (t *testParser) andExpr() (bool, error) {
	left, err := t.notExpr()
	if err != nil {
		return false, err
	}
	for t.peek() == "-a" {
		t.pos++
		right, err := t.notExpr()
		if err != nil {
			return false, err
		}
		left = left && right
	}
	return left, nil
}

func (t *testParser) notExpr() (bool, error) {
	if t.peek() == "!" {
		t.pos++
		v, err := t.notExpr()
		return !v, err
	}
	return t.primary()
}

func (t *testParser) primary() (bool, error) {
	if t.peek() == "(" {
		t.pos++
		v, err := t.orExpr()
		if err != nil {
			return false, err
		}
		if t.peek() != ")" {
			return false, errors.New("expected `)'")
		}
		t.pos++
		return v, nil
	}
	if t.peek() == ")" {
		return false, errors.New("unexpected `)'")
	}
	// Operands available before a closing paren (GNU test quirk: [ '(' x ')' ]).
	avail := 0
	for t.pos+avail < len(t.toks) && t.toks[t.pos+avail] != ")" {
		avail++
	}
	switch avail {
	case 1:
		t.pos++
		return t.toks[t.pos-1] != "", nil
	case 2:
		op, arg := t.toks[t.pos], t.toks[t.pos+1]
		t.pos += 2
		return testUnary(t.hc, op, arg)
	default:
		a, op, b := t.toks[t.pos], t.toks[t.pos+1], t.toks[t.pos+2]
		if isTestBinaryOp(op) {
			t.pos += 3
			return testBinary(t.hc, a, op, b)
		}
		// GNU: with 3 tokens and no operator this is "missing argument".
		if a == "!" || isTestUnaryOp(a) {
			t.pos += 2
			v, err := testUnary(t.hc, a, b)
			return v, err
		}
		return false, errors.New("binary operator expected")
	}
}

func isTestUnaryOp(op string) bool {
	switch op {
	case "-b", "-c", "-d", "-e", "-f", "-g", "-h", "-L", "-p",
		"-r", "-s", "-S", "-t", "-w", "-x", "-O", "-G", "-N",
		"-n", "-z":
		return true
	}
	return false
}

func isTestBinaryOp(op string) bool {
	switch op {
	case "=", "==", "!=", "-eq", "-ne", "-lt", "-le", "-gt", "-ge",
		"-nt", "-ot", "-ef":
		return true
	}
	return false
}

// testDevNodes are the device files that must exist even on Windows.
var testDevNodes = map[string]bool{
	"/dev/null": true, "/dev/zero": true, "/dev/stdin": true,
	"/dev/stdout": true, "/dev/stderr": true,
}

type testFile struct {
	exists  bool
	mode    os.FileMode
	size    int64
	modTime time.Time
	synth   bool // synthesized device node (no real stat)
}

func testStat(hc interp.HandlerContext, path string) testFile {
	if testDevNodes[strings.ToLower(path)] {
		if _, err := os.Stat(resolve(hc.Dir, path)); err != nil {
			return testFile{exists: true, mode: os.ModeDevice | os.ModeCharDevice, synth: true}
		}
	}
	fi, err := os.Stat(resolve(hc.Dir, path))
	if err != nil {
		return testFile{}
	}
	return testFile{exists: true, mode: fi.Mode(), size: fi.Size(), modTime: fi.ModTime()}
}

func testUnary(hc interp.HandlerContext, op, arg string) (bool, error) {
	switch op {
	case "-n":
		return arg != "", nil
	case "-z":
		return arg == "", nil
	case "-t":
		fd, err := strconv.Atoi(arg)
		if err != nil {
			return false, errors.New("integer expression expected")
		}
		return term.IsTerminal(fd), nil
	}
	tf := testStat(hc, arg)
	if op == "-e" {
		return tf.exists, nil
	}
	if !tf.exists {
		return false, nil
	}
	switch op {
	case "-f":
		return tf.mode.IsRegular(), nil
	case "-d":
		return tf.mode.IsDir(), nil
	case "-b":
		return tf.mode&os.ModeDevice != 0 && tf.mode&os.ModeCharDevice == 0, nil
	case "-c":
		return tf.mode&os.ModeCharDevice != 0, nil
	case "-h", "-L":
		return tf.mode&os.ModeSymlink != 0, nil
	case "-p":
		return tf.mode&os.ModeNamedPipe != 0, nil
	case "-S":
		return tf.mode&os.ModeSocket != 0, nil
	case "-s":
		return tf.size > 0, nil
	case "-r":
		if tf.synth {
			return true, nil
		}
		f, err := os.Open(resolve(hc.Dir, arg))
		if err == nil {
			f.Close()
		}
		return err == nil, nil
	case "-w":
		if tf.synth {
			return true, nil
		}
		f, err := os.OpenFile(resolve(hc.Dir, arg), os.O_WRONLY, 0)
		if err == nil {
			f.Close()
		}
		return err == nil, nil
	case "-x":
		if tf.synth {
			return false, nil
		}
		return tf.mode.Perm()&0111 != 0, nil
	case "-O":
		return true, nil // best-effort: no uid tracking
	case "-G":
		return true, nil
	case "-N":
		return false, nil // best-effort: assume not modified since read
	}
	return false, fmt.Errorf("unary operator expected")
}

func testBinary(hc interp.HandlerContext, a, op, b string) (bool, error) {
	switch op {
	case "=", "==":
		return a == b, nil
	case "!=":
		return a != b, nil
	case "-eq", "-ne", "-lt", "-le", "-gt", "-ge":
		x, err1 := strconv.ParseInt(strings.TrimSpace(a), 0, 64)
		y, err2 := strconv.ParseInt(strings.TrimSpace(b), 0, 64)
		if err1 != nil {
			return false, fmt.Errorf("%s: integer expression expected", a)
		}
		if err2 != nil {
			return false, fmt.Errorf("%s: integer expression expected", b)
		}
		switch op {
		case "-eq":
			return x == y, nil
		case "-ne":
			return x != y, nil
		case "-lt":
			return x < y, nil
		case "-le":
			return x <= y, nil
		case "-gt":
			return x > y, nil
		default:
			return x >= y, nil
		}
	case "-nt", "-ot", "-ef":
		ta, tb := testStat(hc, a), testStat(hc, b)
		if !ta.exists {
			return false, nil
		}
		if op == "-ef" {
			if !tb.exists {
				return false, nil
			}
			fa, _ := os.Stat(resolve(hc.Dir, a))
			fb, _ := os.Stat(resolve(hc.Dir, b))
			if fa == nil || fb == nil {
				return false, nil
			}
			return os.SameFile(fa, fb), nil
		}
		if !tb.exists {
			return op == "-nt", nil
		}
		if op == "-nt" {
			return ta.modTime.After(tb.modTime), nil
		}
		return ta.modTime.Before(tb.modTime), nil
	}
	return false, errors.New("binary operator expected")
}

func cmdTrue(_ context.Context, _ interp.HandlerContext, _ []string) error {
	return nil
}

func cmdFalse(_ context.Context, _ interp.HandlerContext, _ []string) error {
	return exitError{1}
}

func cmdYes(ctx context.Context, hc interp.HandlerContext, args []string) error {
	s := "y"
	if len(args) > 1 {
		s = strings.Join(args[1:], " ")
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if _, err := fmt.Fprintln(hc.Stdout, s); err != nil {
			return nil
		}
	}
}

func cmdEcho(_ context.Context, hc interp.HandlerContext, args []string) error {
	rest := args[1:]
	newline := true
	if len(rest) > 0 && rest[0] == "-n" {
		newline = false
		rest = rest[1:]
	}
	if newline {
		fmt.Fprintln(hc.Stdout, strings.Join(rest, " "))
	} else {
		fmt.Fprint(hc.Stdout, strings.Join(rest, " "))
	}
	return nil
}

func cmdClear(_ context.Context, hc interp.HandlerContext, _ []string) error {
	// Matches `clear` (ncurses) non-interactive output: home + erase
	// display + erase scrollback.
	fmt.Fprint(hc.Stdout, "\033[H\033[2J\033[3J")
	return nil
}

func cmdWhich(_ context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) < 2 {
		return flag.ErrHelp
	}
	// Like /usr/bin/which: when PATH is explicitly set but contains no
	// existing directory, nothing can be found (exit 1), even builtins.
	pathSet := false
	pathUsable := false
	if v := hc.Env.Get("PATH"); v.IsSet() {
		pv := v.Str
		pathSet = true
		for _, d := range strings.Split(pv, string(os.PathListSeparator)) {
			if d == "" {
				d = "."
			}
			if fi, err := os.Stat(resolve(hc.Dir, d)); err == nil && fi.IsDir() {
				pathUsable = true
				break
			}
		}
	}
	code := 0
	for _, name := range args[1:] {
		if p, err := findExec(hc, name); err == nil {
			fmt.Fprintln(hc.Stdout, p)
			continue
		}
		if pathSet && !pathUsable {
			code = 1
			continue
		}
		if lookupExtra(name) != nil || isKnownCoreutil(name) || interp.IsBuiltin(name) {
			fmt.Fprintf(hc.Stdout, "%s: unish builtin\n", name)
			continue
		}
		code = 1
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func isKnownCoreutil(name string) bool {
	switch name {
	case "ls", "cat", "cp", "mv", "rm", "mkdir", "touch",
		"chmod", "find", "mktemp", "xargs", "gzip", "gunzip",
		"gzcat", "tar", "base64", "shasum":
		return true
	}
	return false
}

func findExec(hc interp.HandlerContext, name string) (string, error) {
	if strings.ContainsAny(name, `/\`) {
		p := resolve(hc.Dir, name)
		fi, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		if fi.IsDir() {
			return "", fmt.Errorf("is a directory")
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	path := shellGetenv(hc, "PATH")
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		for _, cand := range execCandidates(dir, name) {
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand, nil
			}
		}
	}
	return "", fmt.Errorf("%s not found", name)
}

func execCandidates(dir, name string) []string {
	if runtime.GOOS == "windows" {
		out := []string{filepath.Join(dir, name)}
		for _, e := range strings.Split(os.Getenv("PATHEXT"), ";") {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, filepath.Join(dir, name+e))
			}
		}
		return out
	}
	return []string{filepath.Join(dir, name)}
}

func cmdPrintenv(_ context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) == 1 {
		hc.Env.Each(func(name string, v expand.Variable) bool {
			if v.IsSet() && v.Exported {
				fmt.Fprintf(hc.Stdout, "%s=%s\n", name, v.Str)
			}
			return true
		})
		return nil
	}
	code := 0
	for _, k := range args[1:] {
		if v := hc.Env.Get(k); v.IsSet() {
			fmt.Fprintln(hc.Stdout, v.Str)
		} else if ev, ok := os.LookupEnv(k); ok && envFallbackAllowed(hc, k) {
			// Fall back to the process environment only when the shell's
			// own environment still contains that name. Otherwise `env -i`
			// would leak the parent's variables back in (it must not).
			fmt.Fprintln(hc.Stdout, ev)
		} else {
			code = 1
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

// envFallbackAllowed reports whether a name absent from hc.Env may still
// be answered from the process environment. It is allowed only if the
// shell environment is not deliberately emptied: with `env -i` the child
// must see nothing, so the fallback is suppressed.
func envFallbackAllowed(hc interp.HandlerContext, name string) bool {
	// If the shell has no exported variables at all, it is an emptied
	// environment (env -i): do not resurrect anything from os.Environ.
	saw := false
	hc.Env.Each(func(_ string, v expand.Variable) bool {
		if v.IsSet() && v.Exported {
			saw = true
			return false
		}
		return true
	})
	return saw
}

func cmdWhoami(_ context.Context, hc interp.HandlerContext, _ []string) error {
	for _, k := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := shellGetenv(hc, k); v != "" {
			if i := strings.LastIndexAny(v, `\/ `); i >= 0 && k == "USERNAME" {
				v = v[i+1:]
				if v == "" {
					continue
				}
			}
			fmt.Fprintln(hc.Stdout, v)
			return nil
		}
	}
	if u, err := userCurrent(); err == nil && u != "" {
		if i := strings.LastIndexAny(u, `\/`); i >= 0 {
			u = u[i+1:]
		}
		fmt.Fprintln(hc.Stdout, u)
		return nil
	}
	return fmt.Errorf("whoami: cannot find name for user")
}

func userCurrent() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

func cmdNproc(_ context.Context, hc interp.HandlerContext, _ []string) error {
	fmt.Fprintln(hc.Stdout, runtime.NumCPU())
	return nil
}

func cmdHostname(_ context.Context, hc interp.HandlerContext, args []string) error {
	h, err := os.Hostname()
	if err != nil {
		return err
	}
	for _, a := range args[1:] {
		// GNU `hostname -f/--fqdn/--long`: canonical FQDN when known.
		if a == "-f" || a == "--fqdn" || a == "--long" {
			if fqdn, err := hostnameFQDN(h); err == nil && fqdn != "" {
				fmt.Fprintln(hc.Stdout, fqdn)
				return nil
			}
			fmt.Fprintln(hc.Stdout, h)
			return nil
		}
	}
	fmt.Fprintln(hc.Stdout, h)
	return nil
}

// hostnameFQDN resolves the canonical FQDN: hostname + DNS domain when
// the name isn't already qualified, else the hostname itself.
func hostnameFQDN(h string) (string, error) {
	if strings.Contains(h, ".") {
		return h, nil
	}
	fixCase := func(fqdn string) string {
		// GNU keeps the hostname's own letter case
		// ("ItaloSurface.localdomain", not DNS-lowercased).
		if len(fqdn) > len(h) && strings.EqualFold(fqdn[:len(h)], h) && fqdn[len(h)] == '.' {
			return h + fqdn[len(h):]
		}
		return fqdn
	}
	if cname, err := net.LookupCNAME(h); err == nil {
		if c := strings.TrimSuffix(strings.TrimSpace(cname), "."); c != "" {
			return fixCase(c), nil
		}
	}
	// Fall back to /etc/hosts-style search for a qualified alias.
	if addrs, err := net.LookupHost(h); err == nil && len(addrs) > 0 {
		if names, err := net.LookupAddr(addrs[0]); err == nil {
			for _, n := range names {
				if c := strings.TrimSuffix(strings.TrimSpace(n), "."); strings.Contains(c, ".") {
					return fixCase(c), nil
				}
			}
		}
	}
	return h, nil
}

func cmdUname(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("uname", hc.Stderr)
	all := fs.Bool("a", false, "")
	machine := fs.Bool("m", false, "")
	node := fs.Bool("n", false, "")
	release := fs.Bool("r", false, "")
	sysname := fs.Bool("s", false, "")
	version := fs.Bool("v", false, "")
	proc := fs.Bool("p", false, "")
	fs.BoolVar(proc, "processor", false, "")
	hw := fs.Bool("i", false, "")
	fs.BoolVar(hw, "hardware-platform", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	name := map[string]string{"windows": "Windows", "linux": "Linux", "darwin": "Darwin"}[runtime.GOOS]
	if name == "" {
		name = runtime.GOOS
	}
	// Report the kernel machine name (uname -m), not the Go architecture
	// label: arm64 and x86_64 kernels report "aarch64"/"x86_64".
	machineName := map[string]string{
		"amd64": "x86_64", "arm64": "aarch64", "386": "i686",
	}[runtime.GOARCH]
	if machineName == "" {
		machineName = runtime.GOARCH
	}
	if *all {
		host, _ := os.Hostname()
		fmt.Fprintf(hc.Stdout, "%s %s %s %s %s\n", name, host, kernelRelease(), kernelVersion(), machineName)
		return nil
	}
	var parts []string
	if *sysname || (!*machine && !*node && !*release && !*version && !*proc && !*hw) {
		parts = append(parts, name)
	}
	if *node {
		host, _ := os.Hostname()
		parts = append(parts, host)
	}
	if *release {
		parts = append(parts, kernelRelease())
	}
	if *version {
		parts = append(parts, kernelVersion())
	}
	if *machine {
		parts = append(parts, machineName)
	}
	// GNU: -p (processor) and -i (hardware platform) report the machine
	// architecture on Linux/Windows; only report "unknown" when the
	// platform genuinely has no answer.
	if *proc {
		parts = append(parts, machineProcessor())
	}
	if *hw {
		parts = append(parts, machineHardware())
	}
	fmt.Fprintln(hc.Stdout, strings.Join(parts, " "))
	return nil
}

// isPipeClosed reports whether err is a broken-pipe / closed-pipe write
// error (Windows: "write |1: The pipe is being closed"; unix: EPIPE).
// Callers use it to implement SIGPIPE semantics: a writer whose downstream
// reader exited early (head -n 0, yes | head, ...) exits silently.
func isPipeClosed(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range []string{
		"The pipe is being closed",
		"The pipe has been ended", // Go on Windows, head -c path
		"broken pipe",
		"pipe is closed",
		"EPIPE",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// probeLog is a temporary debugging aid for job-tracking diagnosis.
var probeLog = func(format string, a ...any) {
	os.Stderr.WriteString("PROBE: " + fmt.Sprintf(format, a...) + "\n")
}
