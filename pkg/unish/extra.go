package unish

import (
	"bytes"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

type extraCmd struct {
	name string
	main func(ctx context.Context, hc interp.HandlerContext, args []string) error
}

var extraCommands []extraCmd

type extraRegistry struct {
	n      int
	data   *extraCmd
	byName map[string]*extraCmd
}

var (
	extraRegistryMu  sync.Mutex
	extraRegistryPtr atomic.Pointer[extraRegistry]
)

func rebuildExtraRegistryLocked() {
	n := len(extraCommands)
	var data *extraCmd
	if n > 0 {
		data = &extraCommands[0]
	}
	m := make(map[string]*extraCmd, n)
	for i := range extraCommands {
		m[extraCommands[i].name] = &extraCommands[i]
	}
	extraRegistryPtr.Store(&extraRegistry{
		n:      n,
		data:   data,
		byName: m,
	})
}

func registerExtra(name string, fn func(ctx context.Context, hc interp.HandlerContext, args []string) error) {
	extraRegistryMu.Lock()
	defer extraRegistryMu.Unlock()
	for i := range extraCommands {
		if extraCommands[i].name == name {
			extraCommands[i].main = fn
			rebuildExtraRegistryLocked()
			return
		}
	}
	extraCommands = append(extraCommands, extraCmd{name: name, main: fn})
	rebuildExtraRegistryLocked()
}

func init() {
	base := []extraCmd{
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
		{"awk", cmdAwk},
		{"gawk", cmdAwk},
		{"jq", cmdJq},
		{"rg", cmdRg},
		{"patch", cmdPatch},
		{"bc", cmdBc},
		{"sponge", cmdSponge},
		{"flock", cmdFlock},
		{"column", cmdColumn},
		{"xxd", cmdXxd},
	}
	extraRegistryMu.Lock()
	if len(extraCommands) == 0 {
		extraCommands = make([]extraCmd, len(base), len(base)+48)
		copy(extraCommands, base)
	} else {
		seen := make(map[string]struct{}, len(base)+len(extraCommands))
		merged := make([]extraCmd, 0, len(base)+len(extraCommands))
		for _, c := range base {
			seen[c.name] = struct{}{}
			merged = append(merged, c)
		}
		for _, c := range extraCommands {
			if _, ok := seen[c.name]; !ok {
				seen[c.name] = struct{}{}
				merged = append(merged, c)
			}
		}
		extraCommands = merged
	}
	rebuildExtraRegistryLocked()
	extraRegistryMu.Unlock()

	interp.RegisterFastCallHandler(callOverride, isOverriddenCall, fastCallOverride)
	interp.RegisterFastOpenDevNull(shellOpenHandlerFn, sharedDevNull)
	interp.SetMemPipeDenyFilter(isChildSpawningExtra)
}

func isChildSpawningExtra(name string) bool {
	switch name {
	case "env", "nice", "nohup", "timeout", "find", "xargs", "watch", "flock":
		return true
	}
	return false
}

func lookupExtra(name string) *extraCmd {
	n := len(extraCommands)
	if n == 0 {
		return nil
	}
	data := &extraCommands[0]
	if reg := extraRegistryPtr.Load(); reg != nil && reg.n == n && reg.data == data {
		return reg.byName[name]
	}
	extraRegistryMu.Lock()
	n = len(extraCommands)
	if n == 0 {
		extraRegistryMu.Unlock()
		return nil
	}
	data = &extraCommands[0]
	reg := extraRegistryPtr.Load()
	if reg == nil || reg.n != n || reg.data != data {
		rebuildExtraRegistryLocked()
		reg = extraRegistryPtr.Load()
	}
	extraRegistryMu.Unlock()
	return reg.byName[name]
}

func invokeExtra(ctx context.Context, hc interp.HandlerContext, cmd *extraCmd, args []string) error {
	if hasVersionFlag(args) {
		fmt.Fprintf(hc.Stdout, "%s (unish coreutils) 1.0\n", args[0])
		return nil
	}
	if hc.Stdin == nil {
		hc.Stdin = strings.NewReader("")
	} else if isChildSpawningExtra(args[0]) {
		if osIn, cleanup := interp.BridgeStdinPipeToOS(hc.Stdin); osIn != nil {
			hc.Stdin = osIn
			defer cleanup()
		}
	}
	if args[0] == "ls" {
		lsColorMu.Lock()
		lsColorMode = "never"
		filtered := filterLsArgs(args)
		err := cmd.main(ctx, hc, splitAttached(args[0], filtered))
		lsColorMode = "never"
		lsColorMu.Unlock()
		return err
	}
	return cmd.main(ctx, hc, splitAttached(args[0], args))
}

func extraHandler(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if len(args) == 0 {
			return next(ctx, args)
		}
		// Job-control builtins with real-pid semantics live here so
		// they bypass mvdan/sh's fake-id-only implementations.
		// (trackExec also routes them; this covers direct chains.)
		hc0 := interp.HandlerCtx(ctx)
		// In a background statement bash forks a real process for every one
		// of these (they are external programs there), so $! is a real pid
		// and kill/wait/jobs work. Running them in-process would leave the
		// job table empty and $! a fake id.
		if hc0.InBackground() {
			if err, handled := runSelfTracked(ctx, args); handled {
				return err
			}
		}
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
		hc := interp.HandlerCtx(ctx)
		runCtx := ctx
		var bgJob *trackedJob
		finishBg := func(int, error) {}
		if hc.InBackground() {
			var bgCancel context.CancelFunc
			runCtx, bgCancel = context.WithCancel(ctx)
			var finish func(int, error)
			bgJob, finish = jobsFromContext(ctx).trackInProcess(args, bgCancel)
			finishBg = func(code int, e error) {
				bgCancel()
				finish(code, e)
			}
			hc.ReportBgStart(0)
		}
		err := invokeExtra(runCtx, hc, cmd, args)
		if isPipeClosed(err) || isPipeClosed(runCtx.Err()) {
			// SIGPIPE semantics: downstream closed the pipe early
			// (e.g. `seq 1 5 | head -n 0`). GNU tools exit 0 silently;
			// never surface "write |1: The pipe is being closed".
			finishBg(0, nil)
			return nil
		}
		if err != nil {
			switch {
			case runCtx.Err() == context.DeadlineExceeded:
				st := interp.NewExitStatus(124)
				finishBg(124, st)
				return st
			case runCtx.Err() == context.Canceled:
				code := 130
				if ctx.Err() == nil {
					code = 143
					if bgJob != nil {
						if kx := bgJob.killExit.Load(); kx > 0 {
							code = int(kx)
						}
					}
				}
				st := interp.NewExitStatus(uint8(code))
				finishBg(code, st)
				return st
			}
			if ee, ok := err.(exitError); ok {
				st := interp.NewExitStatus(uint8(ee.code))
				finishBg(ee.code, st)
				return st
			}
			if err == flag.ErrHelp {
				st := interp.NewExitStatus(2)
				finishBg(2, st)
				return st
			}
			// Go flag parse failures already printed "flag provided..." +
			// "Try 'X --help'..." to stderr: exit 2 like GNU, without an
			// extra "bash: ..." duplicate line (agents key off bash text).
			if msg := err.Error(); strings.HasPrefix(msg, "flag provided") ||
				strings.HasPrefix(msg, "flag needs an argument") ||
				strings.HasPrefix(msg, "invalid boolean") ||
				strings.HasPrefix(msg, "invalid value") {
				st := interp.NewExitStatus(2)
				finishBg(2, st)
				return st
			}
			finishBg(1, err)
			return err
		}
		finishBg(0, nil)
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

	jailed := CapabilityHookFromContext(ctx) != nil
	var path string
	var checkedBin string
	if jailed {
		isExplicit := strings.ContainsAny(args[0], `/\`) || (runtime.GOOS == "windows" && isRawAbsOrMSYS(args[0]))
		if isExplicit {
			var err error
			checkedBin, err = checkExec(ctx, args[0], args[0], args, hc.Dir)
			if err != nil {
				hc.ReportBgStart(0)
				fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], err)
				return interp.NewExitStatus(126), true
			}
			fi, serr := os.Stat(checkedBin)
			if serr != nil && runtime.GOOS == "windows" && os.IsNotExist(serr) {
				for _, cand := range sessionWindowsExecCandidates(hc, checkedBin) {
					if cfi, cerr2 := os.Stat(cand); cerr2 == nil && !cfi.IsDir() {
						checkedBin = cand
						fi = cfi
						serr = nil
						break
					}
				}
			}
			if serr != nil {
				hc.ReportBgStart(0)
				fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], serr)
				if os.IsNotExist(serr) {
					return interp.NewExitStatus(127), true
				}
				return interp.NewExitStatus(126), true
			}
			if fi.IsDir() {
				hc.ReportBgStart(0)
				fmt.Fprintf(hc.Stderr, "%s: is a directory\n", args[0])
				return interp.NewExitStatus(126), true
			}
			if runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
				hc.ReportBgStart(0)
				fmt.Fprintf(hc.Stderr, "%s: permission denied\n", args[0])
				return interp.NewExitStatus(126), true
			}
			path = checkedBin
		} else {
			cand, ferr := findExecCtx(ctx, hc, args[0])
			if ferr != nil {
				_, cerr := checkExec(ctx, args[0], args[0], args, hc.Dir)
				hc.ReportBgStart(0)
				if cerr != nil && errors.Is(cerr, os.ErrPermission) {
					fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], cerr)
					return interp.NewExitStatus(126), true
				}
				fmt.Fprintf(hc.Stderr, "%s: command not found\n", args[0])
				return interp.NewExitStatus(127), true
			}
			var err error
			checkedBin, err = checkExec(ctx, args[0], cand, args, hc.Dir)
			if err != nil {
				hc.ReportBgStart(0)
				fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], err)
				return interp.NewExitStatus(126), true
			}
			path = checkedBin
		}
	} else {
		var err error
		path, err = interp.LookPathDir(hc.Dir, hc.Env, args[0])
		if err != nil {
			return nil, false
		}
		checkedBin, err = checkExec(ctx, args[0], path, args, hc.Dir)
		if err != nil {
			hc.ReportBgStart(0)
			fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], err)
			return interp.NewExitStatus(126), true
		}
	}

	newCmd := func() (*exec.Cmd, func(), func()) {
		c := exec.CommandContext(ctx, checkedBin, args[1:]...)
		c.Dir = hc.Dir
		c.Stdin = hc.Stdin
		c.Stdout = hc.Stdout
		c.Stderr = hc.Stderr
		// Exported shell variables, like the default handler.
		c.Env = shellExecEnv(hc)
		as, aw := interp.PrepareExecStdio(c)
		return c, as, aw
	}
	cmd, afterStart, afterWait := newCmd()
	err := cmd.Start()
	for delay := time.Millisecond; runtime.GOOS != "windows" && errors.Is(err, syscall.ETXTBSY) && delay < 300*time.Millisecond; delay *= 2 {
		afterWait()
		time.Sleep(delay)
		cmd, afterStart, afterWait = newCmd()
		err = cmd.Start()
	}
	if err != nil {
		afterWait()
		if jailed {
			hc.ReportBgStart(0)
			if runtime.GOOS != "windows" && errors.Is(err, syscall.ENOEXEC) {
				return runExtraJailedENOEXEC(ctx, hc, checkedBin, args), true
			}
			fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], err)
			if os.IsNotExist(err) {
				return interp.NewExitStatus(127), true
			}
			return interp.NewExitStatus(126), true
		}
		return nil, false
	}
	afterStart()
	defer afterWait()
	// Tell the parent shell the real pid so $! expands to it (and kill/wait
	// on $! work). Must happen before any blocking work, per the mvdan
	// contract for reportBgStart.
	hc.ReportBgStart(cmd.Process.Pid)
	j := jobsFromContext(ctx).track(cmd, args)
	if !jailed {
		hashRecord(args[0], path)
	}
	// Wait synchronously (foreground). Background statements run this
	// whole handler in a goroutine already, so no extra work needed.
	<-j.done
	if j.err != nil {
		if isPipeClosed(j.err) {
			return nil, true
		}
		if ee, ok := j.err.(*exec.ExitError); ok {
			if code := signalExitCode(ee); code >= 0 {
				return interp.NewExitStatus(uint8(code)), true
			}
			return interp.NewExitStatus(uint8(ee.ExitCode())), true
		}
		return j.err, true
	}
	return nil, true
}

func wrapExplicitEnvMiddleware(cleared bool) func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			hcChild := interp.HandlerCtx(ctx)
			if hcChild.Env != nil {
				if _, already := hcChild.Env.(explicitEnviron); !already {
					var pairs []string
					hcChild.Env.Each(func(name string, vr expand.Variable) bool {
						if vr.Exported && vr.IsSet() && vr.Kind == expand.String {
							pairs = append(pairs, name+"="+vr.Str)
						}
						return true
					})
					hcChild.Env = explicitEnviron{
						Environ: hcChild.Env,
						pairs:   pairs,
						cleared: cleared,
					}
					ctx = interp.WithHandlerContext(ctx, hcChild)
				}
			}
			return next(ctx, args)
		}
	}
}

func runExtraJailedENOEXEC(ctx context.Context, hc interp.HandlerContext, checkedBin string, args []string) error {
	b, err := fsReadFile(ctx, hc.Dir, args[0], checkedBin)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], err)
		return interp.NewExitStatus(126)
	}
	line, _, _ := bytes.Cut(b, []byte("\n"))
	if bytes.IndexByte(line, 0) >= 0 {
		fmt.Fprintf(hc.Stderr, "%s: cannot execute binary file\n", args[0])
		return interp.NewExitStatus(126)
	}
	file, err := syntax.NewParser().Parse(bytes.NewReader(b), args[0])
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return interp.NewExitStatus(2)
	}
	execHandlers := []func(interp.ExecHandlerFunc) interp.ExecHandlerFunc{trackExec, extraHandler}
	if hc.Env != nil {
		if ee, ok := hc.Env.(explicitEnviron); ok {
			execHandlers = []func(interp.ExecHandlerFunc) interp.ExecHandlerFunc{wrapExplicitEnvMiddleware(ee.cleared), trackExec, extraHandler}
		}
	}
	r, err := interp.New(
		interp.Dir(hc.Dir),
		interp.Env(expand.ListEnviron(shellExecEnv(hc)...)),
		interp.StdIO(hc.Stdin, hc.Stdout, hc.Stderr),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ReadDirHandler2(shellReadDirHandler()),
		interp.StatHandler(shellStatHandler()),
		interp.AccessHandler(shellAccessHandler()),
		interp.ExecHandlers(execHandlers...),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		return err
	}
	r.Params = slices.Clone(args[1:])
	return r.Run(ctx, file)
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
	var base []string
	explicitCleared := false
	if hc.Env != nil {
		if ee, ok := hc.Env.(explicitEnviron); ok {
			base = ee.pairs
			explicitCleared = ee.cleared
		} else {
			base = os.Environ()
		}
	} else {
		base = os.Environ()
	}
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
	if hc.Env != nil {
		hc.Env.Each(func(name string, vr expand.Variable) bool {
			if vr.Exported && vr.IsSet() && vr.Kind == expand.String {
				if _, ok := merged[name]; !ok {
					order = append(order, name)
				}
				merged[name] = vr.Str
			}
			return true
		})
	}
	if runtime.GOOS == "windows" && !explicitCleared {
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

var (
	lsColorMu   sync.Mutex
	lsColorMode = "never" // --color[=WHEN]: never/always/auto
)

func toolOptForCmd(cmd string) *toolOptParser {
	switch cmd {
	case "rg":
		return &rgOptTable
	case "patch":
		return &patchOpts
	case "sponge":
		return &spongeOpts
	case "flock":
		return &flockOpts
	case "column":
		return &columnOpts
	case "xxd":
		return &xxdOpts
	}
	return nil
}

// hasVersionFlag reports whether args passes --version as an actual option to
// args[0], stopping at "--", skipping values of value-taking flags, ignoring
// builtins like echo/true/false, and stopping before subcommands of wrappers.
func hasVersionFlag(args []string) bool {
	if len(args) < 2 {
		return false
	}
	hasCandidate := false
	for i := 1; i < len(args); i++ {
		if args[i] == "--" {
			return false
		}
		if args[i] == "--version" {
			hasCandidate = true
			break
		}
	}
	if !hasCandidate {
		return false
	}
	cmd := args[0]
	switch cmd {
	case "echo", "true", "false", "jq":
		return false
	}
	spec, hasSpec := flagSpecs[cmd]
	tp := toolOptForCmd(cmd)
	isAwk := cmd == "awk" || cmd == "gawk"
	isWrapper := cmd == "env" || cmd == "timeout" || cmd == "nice" || cmd == "nohup" || cmd == "watch" || cmd == "xargs" || cmd == "flock"
	positionals := 0
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return false
		}
		if a == "--version" {
			return true
		}
		if cmd == "env" && !strings.HasPrefix(a, "-") && strings.IndexByte(a, '=') >= 0 {
			continue
		}
		if strings.HasPrefix(a, "--") && len(a) > 2 {
			if strings.IndexByte(a, '=') < 0 {
				if hasSpec {
					if short, known := spec.long[a[2:]]; known {
						if (len(short) == 1 && strings.IndexByte(spec.values, short[0]) >= 0) ||
							(len(short) > 1 && !spec.longBools[short]) {
							i++
						}
					}
				} else if tp != nil {
					if idx := tp.find(a[2:], 0); idx >= 0 && tp.opts[idx].arg {
						i++
					}
				} else if isAwk {
					switch a[2:] {
					case "field-separator", "assign", "file", "source":
						i++
					}
				}
			}
			continue
		}
		if len(a) >= 2 && a[0] == '-' && a[1] != '-' {
			if hasSpec {
				if len(a) == 2 && strings.IndexByte(spec.values, a[1]) >= 0 {
					i++
				} else if len(a) > 2 {
					for j := 1; j < len(a); j++ {
						c := a[j]
						if strings.IndexByte(spec.bools, c) >= 0 {
							continue
						}
						if strings.IndexByte(spec.values, c) >= 0 && j == len(a)-1 {
							i++
						}
						break
					}
				}
			} else if tp != nil {
				if tp.dashLong {
					if name, _, hasVal := strings.Cut(a[1:], "="); !hasVal {
						if idx := tp.find(name, 0); idx >= 0 {
							if tp.opts[idx].arg {
								i++
							}
							continue
						}
					}
				}
				for j := 1; j < len(a); j++ {
					idx := tp.find("", a[j])
					if idx < 0 {
						break
					}
					if tp.opts[idx].arg {
						if j == len(a)-1 {
							i++
						}
						break
					}
				}
			} else if isAwk {
				for j := 1; j < len(a); j++ {
					switch a[j] {
					case 'b', 'c', 'P', 'S':
						continue
					case 'F', 'v', 'f', 'e', 'W':
						if j == len(a)-1 {
							i++
						}
					}
					break
				}
			}
			continue
		}
		positionals++
		if isWrapper || isAwk {
			if cmd == "timeout" && positionals == 1 {
				continue // duration operand before subcommand
			}
			return false
		}
	}
	return false
}

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

func isOverriddenCall(name string) bool {
	if len(name) == 0 {
		return false
	}
	switch name[0] {
	case '[':
		return name == "["
	case 't':
		return name == "test" || name == "type"
	case 'p':
		if name == "printf" {
			return true
		}
		return hasActiveCapabilityHooks.Load() && (name == "pushd" || name == "popd" || name == "pwd")
	case 'u':
		return name == "umask" || name == "ulimit"
	case 'k':
		return name == "kill"
	case 'h':
		return name == "hash" || name == "history"
	case 'j':
		return name == "jobs"
	case 'f':
		return name == "fg"
	case 'w':
		return name == "wait"
	case 'c':
		return name == "command" || (hasActiveCapabilityHooks.Load() && (name == "cd" || name == "compgen"))
	case 'b':
		return name == "bg" || name == "builtin"
	case 's':
		return hasActiveCapabilityHooks.Load() && name == "source"
	case '.':
		return hasActiveCapabilityHooks.Load() && name == "."
	}
	return false
}

func checkJailedDirBuiltin(ctx context.Context, hc interp.HandlerContext, args []string) ([]string, error) {
	switch args[0] {
	case "cd":
		physical := false
		hadFlags := false
		i := 1
		for i < len(args) {
			a := args[i]
			if a == "--" {
				hadFlags = true
				i++
				break
			}
			if len(a) < 2 || a[0] != '-' {
				break
			}
			validFlags := true
			pFlag := physical
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'L':
					pFlag = false
				case 'P':
					pFlag = true
				case 'e', '@':
				default:
					validFlags = false
				}
			}
			if !validFlags {
				break
			}
			physical = pFlag
			hadFlags = true
			i++
		}
		rest := args[i:]
		if len(rest) > 1 {
			return args, nil
		}
		var target string
		if len(rest) == 0 {
			target = hc.Env.Get("HOME").String()
		} else if rest[0] == "-" {
			target = hc.Env.Get("OLDPWD").String()
		} else {
			target = rest[0]
		}
		if target == "" {
			if hadFlags {
				return append([]string{"cd"}, rest...), nil
			}
			return args, nil
		}
		checkedDir, err := checkChdir(ctx, hc.Dir, "cd", target)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "cd: %v\n", err)
			return nil, interp.CallHandled(1)
		}
		if physical && (len(rest) == 0 || rest[0] != "-") {
			return []string{"cd", checkedDir}, nil
		}
		if len(rest) == 1 && rest[0] != "-" && runtime.GOOS == "windows" && isRawAbsOrMSYS(rest[0]) {
			return []string{"cd", resolveHostPath(hc.Dir, rest[0])}, nil
		}
		if hadFlags {
			return append([]string{"cd"}, rest...), nil
		}
		return args, nil

	case "pushd":
		noChange := false
		i := 1
		if i < len(args) && args[i] == "-n" {
			noChange = true
			i++
		}
		if i < len(args) && args[i] == "--" {
			i++
		}
		rest := args[i:]
		if len(rest) == 1 && rest[0] != "" {
			checkedDir, err := checkChdir(ctx, hc.Dir, "pushd", rest[0])
			if err != nil {
				fmt.Fprintf(hc.Stderr, "pushd: %v\n", err)
				return nil, interp.CallHandled(1)
			}
			// Always rewrite to canonical absolute path so r.dirStack never stores
			// relative entries like "../.." that popd would later evaluate elsewhere.
			if noChange {
				return []string{"pushd", "-n", checkedDir}, nil
			}
			return []string{"pushd", checkedDir}, nil
		}
		return args, nil
	}
	return args, nil
}

func fastCallOverride(ctx context.Context, hc interp.HandlerContext, args []string) ([]string, error) {
	if len(args) == 0 {
		return args, nil
	}
	switch args[0] {
	case "test", "[":
		// Own GNU test implementation: the /dev/null family always
		// exists (Windows has no such nodes) and diagnostics match GNU.
		switch err := runTestCtx(ctx, hc, args); {
		case err == nil:
			return nil, interp.CallHandled(0)
		case err == errTestFalse || errors.Is(err, errTestFalse):
			return nil, interp.CallHandled(1)
		default:
			return nil, interp.CallHandled(2)
		}
	case "cd", "pushd", "popd":
		if CapabilityHookFromContext(ctx) == nil {
			return args, nil
		}
		return checkJailedDirBuiltin(ctx, hc, args)
	case "pwd":
		if CapabilityHookFromContext(ctx) == nil {
			return args, nil
		}
		return nil, cmdPwdJailed(ctx, hc, args)
	case "compgen":
		if CapabilityHookFromContext(ctx) == nil {
			return args, nil
		}
		return handleCompgenJailed(ctx, hc, args)
	case "source", ".":
		if CapabilityHookFromContext(ctx) == nil {
			return args, nil
		}
		return resolveJailedSourceArgs(ctx, hc, args)
	case "builtin":
		if len(args) < 2 || !interp.IsBuiltin(args[1]) {
			return args, nil
		}
		if CapabilityHookFromContext(ctx) == nil {
			switch args[1] {
			case "kill", "jobs", "fg", "bg", "wait":
				return fastCallOverride(ctx, hc, args[1:])
			default:
				return args, nil
			}
		}
		sub := args[1:]
		if isOverriddenCall(sub[0]) {
			replaced, err := fastCallOverride(ctx, hc, sub)
			if err != nil {
				return nil, err
			}
			if len(replaced) != len(sub) || (len(replaced) > 0 && &replaced[0] != &sub[0]) {
				return append([]string{"builtin"}, replaced...), nil
			}
		}
		return args, nil
	case "command":
		if len(args) < 2 {
			return args, nil
		}
		if CapabilityHookFromContext(ctx) == nil {
			if len(args[1]) > 0 && args[1][0] != '-' {
				switch args[1] {
				case "kill", "jobs", "fg", "bg", "wait":
					return fastCallOverride(ctx, hc, args[1:])
				}
			}
			return args, nil
		}
		showMode := byte(0) // 'v' or 'V'
		i := 1
		for i < len(args) {
			a := args[i]
			if a == "--" {
				i++
				break
			}
			if len(a) < 2 || a[0] != '-' {
				break
			}
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'v', 'V':
					showMode = a[j]
				case 'p':
				default:
					fmt.Fprintf(hc.Stderr, "command: invalid option %q\n", a)
					return nil, interp.CallHandled(2)
				}
			}
			i++
		}
		if i >= len(args) {
			return args, nil
		}
		if showMode != 0 {
			return nil, cmdCommandShowJailed(ctx, hc, showMode == 'V', args[i:])
		}
		sub := args[i:]
		if interp.IsBuiltin(sub[0]) && isOverriddenCall(sub[0]) {
			replaced, err := fastCallOverride(ctx, hc, sub)
			if err != nil {
				return nil, err
			}
			if len(replaced) != len(sub) || (len(replaced) > 0 && &replaced[0] != &sub[0]) {
				out := make([]string, 0, i+len(replaced))
				out = append(out, args[:i]...)
				out = append(out, replaced...)
				return out, nil
			}
		}
		return args, nil
	case "printf":
		// Bypass mvdan/sh's printf (no precision specs like %.0s).
		if err := cmdPrintf(ctx, hc, args); err != nil {
			if ee, ok := err.(exitError); ok {
				return nil, interp.CallHandled(uint8(ee.code))
			}
			return nil, interp.CallHandled(1)
		}
		return nil, interp.CallHandled(0)
	case "kill":
		err := cmdKillNetproc(interp.WithHandlerContext(ctx, hc), hc, splitAttached("kill", args))
		if err == nil {
			return nil, interp.CallHandled(0)
		}
		if ee, ok := err.(exitError); ok {
			return nil, interp.CallHandled(uint8(ee.code))
		}
		var exitSt *interp.ExitError
		if errors.As(err, &exitSt) {
			return nil, err
		}
		return nil, interp.CallHandled(1)
	case "umask":
		if CapabilityHookFromContext(ctx) != nil {
			if err := cmdUmaskJailed(ctx, hc, args); err != nil {
				if err == flag.ErrHelp {
					return nil, interp.CallHandled(2)
				}
				return nil, interp.CallHandled(1)
			}
			return nil, interp.CallHandled(0)
		}
		if err := cmdUmask(ctx, hc, args); err != nil {
			return nil, interp.CallHandled(1)
		}
		return nil, interp.CallHandled(0)
	case "ulimit":
		if CapabilityHookFromContext(ctx) != nil {
			if err := cmdUlimitJailed(ctx, hc, args); err != nil {
				if err == flag.ErrHelp {
					return nil, interp.CallHandled(2)
				}
				return nil, interp.CallHandled(1)
			}
			return nil, interp.CallHandled(0)
		}
		if err := cmdUlimit(ctx, hc, args); err != nil {
			return nil, interp.CallHandled(1)
		}
		return nil, interp.CallHandled(0)
	case "hash":
		if CapabilityHookFromContext(ctx) != nil {
			if err := cmdHashTableJailed(ctx, hc, args); err != nil {
				if err == flag.ErrHelp {
					return nil, interp.CallHandled(2)
				}
				return nil, interp.CallHandled(1)
			}
			return nil, interp.CallHandled(0)
		}
		if err := cmdHashTable(ctx, hc, args); err != nil {
			return nil, interp.CallHandled(1)
		}
		return nil, interp.CallHandled(0)
	case "type":
		if CapabilityHookFromContext(ctx) != nil {
			if err := cmdTypeDescJailed(ctx, hc, args); err != nil {
				if err == flag.ErrHelp {
					return nil, interp.CallHandled(2)
				}
				return nil, interp.CallHandled(1)
			}
			return nil, interp.CallHandled(0)
		}
		if err := cmdTypeDesc(ctx, hc, args); err != nil {
			return nil, interp.CallHandled(1)
		}
		return nil, interp.CallHandled(0)
	case "jobs":
		if err := cmdJobsCtx(ctx, hc, args); err != nil {
			if ee, ok := err.(exitError); ok {
				return nil, interp.CallHandled(uint8(ee.code))
			}
			return nil, interp.CallHandled(1)
		}
		return nil, interp.CallHandled(0)
	case "fg":
		if err := cmdFgCtx(ctx, hc, args); err != nil {
			if ee, ok := err.(exitError); ok {
				return nil, interp.CallHandled(uint8(ee.code))
			}
			return nil, interp.CallHandled(1)
		}
		return nil, interp.CallHandled(0)
	case "bg":
		if err := cmdBgCtx(ctx, hc, args); err != nil {
			if ee, ok := err.(exitError); ok {
				return nil, interp.CallHandled(uint8(ee.code))
			}
			return nil, interp.CallHandled(1)
		}
		return nil, interp.CallHandled(0)
	case "history":
		if CapabilityHookFromContext(ctx) != nil {
			if err := cmdHistoryJailed(ctx, hc, args); err != nil {
				return nil, interp.CallHandled(1)
			}
			return nil, interp.CallHandled(0)
		}
		if err := cmdHistory(ctx, hc, args); err != nil {
			return nil, interp.CallHandled(1)
		}
		return nil, interp.CallHandled(0)
	case "wait":
		wctx := interp.WithHandlerContext(ctx, hc)
		if len(args) == 1 {
			cmdWaitTracked(wctx, args)
			return args, nil
		}
		if code, handled := cmdWaitTracked(wctx, args); handled {
			if code != 0 {
				return nil, interp.CallHandled(uint8(code))
			}
			return nil, interp.CallHandled(0)
		}
		return args, nil
	}
	return args, nil
}

func callOverride(ctx context.Context, args []string) ([]string, error) {
	if len(args) == 0 || !isOverriddenCall(args[0]) {
		return args, nil
	}
	return fastCallOverride(ctx, interp.HandlerCtx(ctx), args)
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
	if hc.Env != nil {
		if v := hc.Env.Get(key); v.IsSet() {
			return v.Str
		}
		if _, ok := hc.Env.(explicitEnviron); ok {
			return ""
		}
	}
	return os.Getenv(key)
}

// shellOpenHandler is mvdan/sh's OpenHandler with self-contained MSYS
// translation, hc.Dir resolution, and capability enforcement: redirections
// (>, >>, <, <>, exec 3>file) and sourced scripts go through here.
// Keeps the zero-syscall sharedDevNull fast-path for write-only /dev/null.
func shellOpenHandlerFn(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	if isWriteOnlyDevNull(path, flag) {
		return sharedDevNull, nil
	}
	mc := interp.HandlerCtx(ctx)
	if rwc, ok, err := tryOpenActiveProcSubst(ctx, resolveHostPath(mc.Dir, path), flag); ok {
		if err != nil {
			if _, ok2 := err.(*os.PathError); !ok2 {
				return nil, &os.PathError{Op: "open", Path: path, Err: err}
			}
			return nil, err
		}
		return rwc, nil
	}
	f, err := fsOpenFile(ctx, mc.Dir, "redirect", path, flag, perm)
	if err != nil {
		if _, ok := err.(*os.PathError); !ok {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		return nil, err
	}
	return f, nil
}

func shellOpenHandler() interp.OpenHandlerFunc {
	return shellOpenHandlerFn
}

func shellReadDirHandler() interp.ReadDirHandlerFunc2 {
	return func(ctx context.Context, path string) ([]os.DirEntry, error) {
		mc := interp.HandlerCtx(ctx)
		if CapabilityHookFromContext(ctx) == nil {
			return os.ReadDir(resolveHostPath(mc.Dir, path))
		}
		return fsReadDir(ctx, mc.Dir, "glob", path)
	}
}

func shellStatHandler() interp.StatHandlerFunc {
	return func(ctx context.Context, path string, followSymlinks bool) (os.FileInfo, error) {
		mc := interp.HandlerCtx(ctx)
		if CapabilityHookFromContext(ctx) == nil {
			resolved := resolveHostPath(mc.Dir, path)
			if !followSymlinks {
				return os.Lstat(resolved)
			}
			return os.Stat(resolved)
		}
		if !followSymlinks {
			return fsLstat(ctx, mc.Dir, "stat", path)
		}
		return fsStat(ctx, mc.Dir, "stat", path)
	}
}

func shellAccessHandler() interp.AccessHandlerFunc {
	def := interp.DefaultAccessHandler()
	return func(ctx context.Context, path string, mode interp.AccessMode) error {
		mc := interp.HandlerCtx(ctx)
		if CapabilityHookFromContext(ctx) == nil {
			return def(ctx, resolveHostPath(mc.Dir, path), mode)
		}
		if mode&interp.AccessRead != 0 {
			if _, err := checkFS(ctx, mc.Dir, "access", path, OpRead); err != nil {
				return err
			}
		}
		if mode&interp.AccessWrite != 0 {
			if _, err := checkFS(ctx, mc.Dir, "access", path, OpWrite); err != nil {
				return err
			}
		}
		if mode&interp.AccessExec != 0 || mode == 0 {
			if _, err := checkFS(ctx, mc.Dir, "access", path, OpStat); err != nil {
				return err
			}
		}
		return def(ctx, resolveHostPath(mc.Dir, path), mode)
	}
}

type flagSpec struct {
	bools     string
	values    string
	long      map[string]string
	longBools map[string]bool
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
	}, longBools: map[string]bool{"complement": true}},
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
	}, longBools: map[string]bool{"full-time": true}},
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
	"md5sum":    {bools: "btcqsz", values: "", long: map[string]string{"check": "c", "quiet": "q", "status": "s", "strict": "z", "tag": "tag", "warn": "w"}, longBools: map[string]bool{"tag": true}},
	"sha1sum":   {bools: "btcqsz", values: "", long: map[string]string{"check": "c", "quiet": "q", "status": "s", "strict": "z", "tag": "tag", "warn": "w"}, longBools: map[string]bool{"tag": true}},
	"sha256sum": {bools: "btcqsz", values: "", long: map[string]string{"check": "c", "quiet": "q", "status": "s", "strict": "z", "tag": "tag", "warn": "w"}, longBools: map[string]bool{"tag": true}},
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
	}, longBools: map[string]bool{"first-only": true}},
	"join": {bools: "i", values: "12jateov", long: map[string]string{
		"ignore-case": "i",
	}},
}

func splitAttachedNeedsWork(cmd string, spec flagSpec, argv []string) bool {
	sawRest := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			return false
		}
		if len(a) > 2 && a[0] == '-' {
			if a[1] == '-' {
				name := a[2:]
				if eq := strings.IndexByte(name, '='); eq >= 0 {
					name = name[:eq]
				}
				if _, known := spec.long[name]; known {
					return true
				}
				sawRest = true
				continue
			}
			if strings.IndexByte(spec.values, a[1]) >= 0 {
				return true
			}
			if isFlagChar(cmd, a[1]) && canCascadeBools(spec, a[1:]) {
				return true
			}
			sawRest = true
			continue
		}
		if len(a) == 2 && a[0] == '-' && strings.IndexByte(spec.values, a[1]) >= 0 &&
			i+1 < len(argv) && len(argv[i+1]) > 1 && argv[i+1][0] == '-' && argv[i+1][1] >= '0' && argv[i+1][1] <= '9' {
			if sawRest {
				return true
			}
			i++
			continue
		}
		sawRest = true
	}
	return false
}

func canCascadeBools(spec flagSpec, s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			if strings.IndexByte(spec.bools, c) >= 0 {
				continue
			}
			return i > 0
		}
		if strings.IndexByte(spec.bools, c) >= 0 {
			continue
		}
		if strings.IndexByte(spec.values, c) >= 0 {
			return true
		}
		return false
	}
	return true
}

func splitAttached(cmd string, args []string) []string {
	if len(args) <= 1 {
		return args
	}
	spec, ok := flagSpecs[cmd]
	if !ok {
		return args
	}
	argv := args[1:]
	if !splitAttachedNeedsWork(cmd, spec, argv) {
		return args
	}
	var movable, rest []string
	movable = append(movable, args[0])
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
				if spec.longBools[short] {
					if hasVal {
						movable = append(movable, "--"+short+"="+val)
					} else {
						movable = append(movable, "--"+short)
					}
					continue
				}
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
			if strings.IndexByte(spec.values, short[0]) >= 0 {
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
			strings.IndexByte(spec.values, a[1]) >= 0 {
			movable = append(movable, "-"+string(a[1]), a[2:])
			continue
		}
		// Value flags with negative-number values: "-n -1" (head/tail).
		// Go's flag package would treat "-1" as a flag; glue it as the
		// value so `head -n -1` / `tail -n -1` parse like GNU.
		if len(a) == 2 && a[0] == '-' && strings.IndexByte(spec.values, a[1]) >= 0 &&
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
			if strings.IndexByte(spec.bools, c) >= 0 {
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
		if strings.IndexByte(spec.bools, c) >= 0 {
			out = append(out, "-"+string(c))
			continue
		}
		if strings.IndexByte(spec.values, c) >= 0 {
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
	for i := 0; i < len(args); i++ {
		a := args[i]
		// Don't treat the VALUE of -n/-c (e.g. "-1" in "-n -1") as an
		// attached count; it belongs to the flag.
		if skipNext {
			skipNext = false
			rest = append(rest, a)
			continue
		}
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
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

// fsOpenInput opens a builtin input file resolved against dir with OpRead
// capability enforcement, while preserving `<(cmd)` process substitution
// synchronization and per-CapabilityHook isolation across Windows and POSIX.
func fsOpenInput(ctx context.Context, dir, cmdName, rawPath string) (io.ReadCloser, error) {
	hostPath := resolveHostPath(dir, rawPath)
	if rwc, ok, err := tryOpenActiveProcSubst(ctx, hostPath, os.O_RDONLY); ok {
		return rwc, err
	}
	if CapabilityHookFromContext(ctx) == nil {
		return openShellFile(hostPath)
	}
	return fsOpen(ctx, dir, cmdName, rawPath)
}

func fsReadInputFile(ctx context.Context, dir, cmdName, rawPath string) ([]byte, error) {
	f, err := fsOpenInput(ctx, dir, cmdName, rawPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func fsOpenOutput(ctx context.Context, dir, cmdName, rawPath string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	hostPath := resolveHostPath(dir, rawPath)
	if st := winProcSubstLookupCtx(ctx, hostPath); st != nil && st.op == syntax.CmdOut {
		w, err := openFileRetry(hostPath, flag, perm)
		if err != nil {
			st.finishConsumer()
			return nil, err
		}
		st.consumerOpen()
		return &countedFile{File: w, onClose: st.consumerWriterClose}, nil
	}
	if w, ok, err := openProcSubstOut(ctx, hostPath); ok {
		return w, err
	}
	return fsOpenFile(ctx, dir, cmdName, rawPath, flag, perm)
}

func openInputs(ctx context.Context, cmdName, dir string, args []string, stdin io.Reader) (readers []io.Reader, names []string, closeAll func(), err error) {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
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
		f, e := fsOpenInput(ctx, dir, cmdName, a)
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

func cmdCat(ctx context.Context, hc interp.HandlerContext, args []string) error {
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
	readers, _, closeAll, err := openInputs(ctx, "cat", hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "cat:", err)
		return exitError{1}
	}
	defer closeAll()
	if !*numAll && !*numNonBlank && !*squeeze && !*showEnds && !*showTabs && !*showNonprint {
		buf := make([]byte, 32*1024)
		for _, r := range readers {
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				n, rerr := r.Read(buf)
				if n > 0 {
					if _, werr := hc.Stdout.Write(buf[:n]); werr != nil {
						if isPipeClosed(werr) {
							return werr
						}
						fmt.Fprintln(hc.Stderr, "cat:", werr)
						return exitError{1}
					}
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					if isPipeClosed(rerr) {
						return rerr
					}
					fmt.Fprintln(hc.Stderr, "cat:", rerr)
					return exitError{1}
				}
			}
		}
		return nil
	}
	ln := 0
	prevBlank := false
	for _, r := range readers {
		sc := newLineReader(r)
		for sc.Scan() {
			if err := ctx.Err(); err != nil {
				return err
			}
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
			var werr error
			if number {
				ln++
				_, werr = fmt.Fprintf(hc.Stdout, "%6d\t%s%s", ln, out, nl)
			} else {
				_, werr = fmt.Fprint(hc.Stdout, out+nl)
			}
			if werr != nil {
				if isPipeClosed(werr) {
					return werr
				}
				fmt.Fprintln(hc.Stderr, "cat:", werr)
				return exitError{1}
			}
		}
		if err := sc.Err(); err != nil {
			if isPipeClosed(err) {
				return err
			}
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

func parseTestInt64(s string) (int64, bool) {
	start := 0
	for start < len(s) {
		c := s[start]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != '\v' && c != '\f' {
			break
		}
		start++
	}
	end := len(s)
	for end > start {
		c := s[end-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != '\v' && c != '\f' {
			break
		}
		end--
	}
	if start >= end {
		return 0, false
	}
	neg := false
	if s[start] == '+' || s[start] == '-' {
		neg = s[start] == '-'
		start++
		if start >= end {
			return 0, false
		}
	}
	rem := s[start:end]
	if rem == "0" {
		return 0, true
	}
	// Delegate leading-0 (octal/hex/binary in ParseInt base 0) and 19+ digit integers.
	if rem[0] < '1' || rem[0] > '9' || len(rem) > 18 {
		return 0, false
	}
	var n int64
	for i := 0; i < len(rem); i++ {
		c := rem[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}

func evalFastTestBinary(a, op, b string) (result bool, handled bool) {
	switch op {
	case "=", "==":
		return a == b, true
	case "!=":
		return a != b, true
	case "-eq", "-ne", "-lt", "-le", "-gt", "-ge":
		x, ok1 := parseTestInt64(a)
		if !ok1 {
			return false, false
		}
		y, ok2 := parseTestInt64(b)
		if !ok2 {
			return false, false
		}
		switch op {
		case "-eq":
			return x == y, true
		case "-ne":
			return x != y, true
		case "-lt":
			return x < y, true
		case "-le":
			return x <= y, true
		case "-gt":
			return x > y, true
		default:
			return x >= y, true
		}
	}
	return false, false
}

// runTest implements the test/[ builtin with GNU semantics, including
// the /dev/null device family (which does not exist on Windows).
// A nil return means the expression is true.
func runTest(hc interp.HandlerContext, args []string) error {
	return runTestCtx(context.Background(), hc, args)
}

func runTestCtx(ctx context.Context, hc interp.HandlerContext, args []string) error {
	a := args[1:]
	if args[0] == "[" {
		if len(a) == 0 || a[len(a)-1] != "]" {
			fmt.Fprintln(hc.Stderr, "[: missing `]'")
			return errTestUsage
		}
		a = a[:len(a)-1]
	}
	switch len(a) {
	case 0:
		return errTestFalse
	case 1:
		if a[0] != ")" {
			if a[0] != "" {
				return nil
			}
			return errTestFalse
		}
	case 2:
		op, arg := a[0], a[1]
		if op != "(" && op != ")" && arg != "-a" && arg != "-o" && arg != ")" {
			switch op {
			case "-n":
				if arg != "" {
					return nil
				}
				return errTestFalse
			case "-z", "!":
				if arg == "" {
					return nil
				}
				return errTestFalse
			}
		}
	case 3:
		if a[0] != "!" && a[0] != "(" && a[0] != ")" {
			if res, ok := evalFastTestBinary(a[0], a[1], a[2]); ok {
				if res {
					return nil
				}
				return errTestFalse
			}
		}
	case 4:
		if a[0] == "!" && a[1] != "!" && a[1] != "(" && a[1] != ")" && !isTestBinaryOp(a[1]) {
			if res, ok := evalFastTestBinary(a[1], a[2], a[3]); ok {
				if !res {
					return nil
				}
				return errTestFalse
			}
		}
	}
	t := testParser{ctx: ctx, hc: hc, toks: a}
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
	ctx   context.Context
	hc    interp.HandlerContext
	toks  []string
	pos   int
	depth int
}

func (t *testParser) peek() string {
	if t.pos < len(t.toks) {
		return t.toks[t.pos]
	}
	return ""
}

func (t *testParser) remTokens() int {
	if t.depth == 0 {
		return len(t.toks) - t.pos
	}
	avail := 0
	for t.pos+avail < len(t.toks) && t.toks[t.pos+avail] != ")" {
		avail++
	}
	return avail
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
		rem := t.remTokens()
		if rem == 1 {
			return t.primary()
		}
		if rem >= 2 && (t.toks[t.pos+1] == "-a" || t.toks[t.pos+1] == "-o") {
			return t.primary()
		}
		if rem >= 3 && isTestBinaryOp(t.toks[t.pos+1]) &&
			(rem == 3 || t.toks[t.pos+3] == "-a" || t.toks[t.pos+3] == "-o") {
			return t.primary()
		}
		t.pos++
		v, err := t.notExpr()
		return !v, err
	}
	return t.primary()
}

func (t *testParser) primary() (bool, error) {
	rem := t.remTokens()
	if rem == 0 {
		if t.peek() == ")" {
			return false, errors.New("unexpected `)'")
		}
		return false, errors.New("argument expected")
	}
	if t.peek() == "(" {
		isLiteral := rem == 1 ||
			(rem >= 2 && (t.toks[t.pos+1] == "-a" || t.toks[t.pos+1] == "-o")) ||
			(rem >= 3 && isTestBinaryOp(t.toks[t.pos+1]) &&
				(rem == 3 || t.toks[t.pos+3] == "-a" || t.toks[t.pos+3] == "-o"))
		if !isLiteral {
			t.pos++
			t.depth++
			v, err := t.orExpr()
			t.depth--
			if err != nil {
				return false, err
			}
			if t.peek() != ")" {
				return false, errors.New("expected `)'")
			}
			t.pos++
			return v, nil
		}
	}
	if t.peek() == ")" {
		return false, errors.New("unexpected `)'")
	}
	switch rem {
	case 1:
		tok := t.toks[t.pos]
		t.pos++
		return tok != "", nil
	case 2:
		op, arg := t.toks[t.pos], t.toks[t.pos+1]
		if arg == "-a" || arg == "-o" {
			t.pos++
			return op != "", nil
		}
		t.pos += 2
		return testUnary(t.ctx, t.hc, op, arg)
	default:
		a, op, b := t.toks[t.pos], t.toks[t.pos+1], t.toks[t.pos+2]
		if isTestBinaryOp(op) && (rem == 3 || t.toks[t.pos+3] == "-a" || t.toks[t.pos+3] == "-o") {
			t.pos += 3
			return testBinary(t.ctx, t.hc, a, op, b)
		}
		if isTestUnaryOp(a) && (b == "-a" || b == "-o") {
			t.pos += 2
			return testUnary(t.ctx, t.hc, a, op)
		}
		if op == "-a" || op == "-o" {
			t.pos++
			return a != "", nil
		}
		if isTestBinaryOp(op) {
			t.pos += 3
			return testBinary(t.ctx, t.hc, a, op, b)
		}
		return false, errors.New("binary operator expected")
	}
}

func isTestUnaryOp(op string) bool {
	switch op {
	case "-b", "-c", "-d", "-e", "-f", "-g", "-h", "-k", "-L", "-p",
		"-r", "-s", "-S", "-t", "-u", "-w", "-x", "-O", "-G", "-N",
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

func testStat(ctx context.Context, hc interp.HandlerContext, path string) (testFile, os.FileInfo) {
	if testDevNodes[strings.ToLower(path)] {
		fi, err := fsStat(ctx, hc.Dir, "test", path)
		if err == nil {
			return testFile{exists: true, mode: fi.Mode(), size: fi.Size(), modTime: fi.ModTime()}, fi
		}
		if CapabilityHookFromContext(ctx) == nil || !errors.Is(err, os.ErrPermission) {
			return testFile{exists: true, mode: os.ModeDevice | os.ModeCharDevice, synth: true}, nil
		}
		return testFile{}, nil
	}
	fi, err := fsStat(ctx, hc.Dir, "test", path)
	if err != nil {
		return testFile{}, nil
	}
	return testFile{exists: true, mode: fi.Mode(), size: fi.Size(), modTime: fi.ModTime()}, fi
}

func testUnary(ctx context.Context, hc interp.HandlerContext, op, arg string) (bool, error) {
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
	case "-h", "-L":
		if testDevNodes[strings.ToLower(arg)] {
			fi, err := fsLstat(ctx, hc.Dir, "test", arg)
			if err != nil {
				return false, nil
			}
			return fi.Mode()&os.ModeSymlink != 0, nil
		}
		fi, err := fsLstat(ctx, hc.Dir, "test", arg)
		if err != nil {
			return false, nil
		}
		return fi.Mode()&os.ModeSymlink != 0, nil
	}
	tf, _ := testStat(ctx, hc, arg)
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
	case "-p":
		return tf.mode&os.ModeNamedPipe != 0, nil
	case "-S":
		return tf.mode&os.ModeSocket != 0, nil
	case "-s":
		return tf.size > 0, nil
	case "-g":
		return tf.mode&os.ModeSetgid != 0, nil
	case "-u":
		return tf.mode&os.ModeSetuid != 0, nil
	case "-k":
		return tf.mode&os.ModeSticky != 0, nil
	case "-r":
		if tf.synth {
			return true, nil
		}
		f, err := fsOpen(ctx, hc.Dir, "test", arg)
		if err == nil {
			f.Close()
		}
		return err == nil, nil
	case "-w":
		if tf.synth {
			return true, nil
		}
		f, err := fsOpenFile(ctx, hc.Dir, "test", arg, os.O_WRONLY, 0)
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

func testBinary(ctx context.Context, hc interp.HandlerContext, a, op, b string) (bool, error) {
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
		ta, fa := testStat(ctx, hc, a)
		tb, fb := testStat(ctx, hc, b)
		if !ta.exists {
			return false, nil
		}
		if op == "-ef" {
			if !tb.exists || fa == nil || fb == nil {
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

func cmdWhich(ctx context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) < 2 {
		return flag.ErrHelp
	}
	jailed := CapabilityHookFromContext(ctx) != nil
	// Like /usr/bin/which: when PATH is explicitly set but contains no
	// existing directory, nothing can be found (exit 1), even builtins.
	pathSet := false
	pathUsable := false
	if v := hc.Env.Get("PATH"); v.IsSet() {
		pv := v.Str
		pathSet = true
		if jailed {
			for _, d := range strings.Split(pv, string(os.PathListSeparator)) {
				if strings.TrimSpace(d) != "" {
					pathUsable = true
					break
				}
			}
		} else {
			for _, d := range strings.Split(pv, string(os.PathListSeparator)) {
				if d == "" {
					d = "."
				}
				if fi, err := fsStat(ctx, hc.Dir, "which", d); err == nil && fi.IsDir() {
					pathUsable = true
					break
				}
			}
		}
	}
	code := 0
	for _, name := range args[1:] {
		if jailed && !strings.ContainsAny(name, `/\`) {
			if pathSet && !pathUsable {
				code = 1
				continue
			}
			if lookupExtra(name) != nil || isKnownCoreutil(name) || interp.IsBuiltin(name) {
				fmt.Fprintf(hc.Stdout, "%s: unish builtin\n", name)
				continue
			}
			if p, err := findExecCtx(ctx, hc, name); err == nil {
				fmt.Fprintln(hc.Stdout, p)
				continue
			}
			code = 1
			continue
		}
		if p, err := findExecCtx(ctx, hc, name); err == nil {
			fmt.Fprintln(hc.Stdout, p)
			continue
		}
		if pathSet && !pathUsable {
			code = 1
			continue
		}
		if !strings.ContainsAny(name, `/\`) && (lookupExtra(name) != nil || isKnownCoreutil(name) || interp.IsBuiltin(name)) {
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
	return findExecCtx(context.Background(), hc, name)
}

func findExecCtx(ctx context.Context, hc interp.HandlerContext, name string) (string, error) {
	if strings.ContainsAny(name, `/\`) || (runtime.GOOS == "windows" && isRawAbsOrMSYS(name)) {
		fi, err := fsStat(ctx, hc.Dir, "which", name)
		if err != nil {
			return "", err
		}
		if fi.IsDir() {
			return "", fmt.Errorf("is a directory")
		}
		if runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
			return "", os.ErrPermission
		}
		abs, err := filepath.Abs(resolveHostPath(hc.Dir, name))
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	jailed := CapabilityHookFromContext(ctx) != nil
	var path string
	if jailed {
		path = hc.Env.Get("PATH").String()
	} else {
		path = shellGetenv(hc, "PATH")
	}
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		for _, cand := range execCandidatesEnv(hc, jailed, dir, name) {
			if fi, err := fsStat(ctx, hc.Dir, "which", cand); err == nil && !fi.IsDir() && (runtime.GOOS == "windows" || fi.Mode()&0o111 != 0) {
				return resolveHostPath(hc.Dir, cand), nil
			}
		}
	}
	return "", fmt.Errorf("%s not found", name)
}

func execCandidates(dir, name string) []string {
	return execCandidatesEnv(interp.HandlerContext{}, false, dir, name)
}

func execCandidatesEnv(hc interp.HandlerContext, jailed bool, dir, name string) []string {
	if runtime.GOOS == "windows" {
		out := []string{filepath.Join(dir, name)}
		var pathext string
		if jailed {
			if hc.Env != nil {
				pathext = hc.Env.Get("PATHEXT").String()
			}
		} else if hc.Env != nil {
			pathext = shellGetenv(hc, "PATHEXT")
		} else {
			pathext = os.Getenv("PATHEXT")
		}
		if pathext == "" {
			pathext = ".COM;.EXE;.BAT;.CMD"
		}
		for _, e := range strings.Split(pathext, ";") {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, filepath.Join(dir, name+e))
			}
		}
		return out
	}
	return []string{filepath.Join(dir, name)}
}

var jailedShellBuiltinNames = []string{
	"alias", "bg", "cd", "command", "false", "fc", "fg", "getopts", "hash",
	"jobs", "kill", "newgrp", "pwd", "read", "true", "umask", "unalias", "wait",
	"break", ":", "continue", ".", "eval", "exec", "exit", "export", "readonly",
	"return", "set", "shift", "times", "trap", "unset",
	"source", "bind", "builtin", "caller", "compgen", "complete", "compopt",
	"declare", "typeset", "dirs", "disown", "enable", "history", "help",
	"let", "local", "logout", "mapfile", "popd", "pushd", "readarray", "shopt",
	"type", "echo", "printf", "test", "[", "ulimit",
}

func cmdPwdJailed(ctx context.Context, hc interp.HandlerContext, args []string) error {
	evalSymlinks := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if len(a) < 2 || a[0] != '-' {
			break
		}
		for j := 1; j < len(a); j++ {
			switch a[j] {
			case 'L':
				evalSymlinks = false
			case 'P':
				evalSymlinks = true
			default:
				fmt.Fprintf(hc.Stderr, "invalid option: %q\n", a)
				return interp.CallHandled(2)
			}
		}
	}
	physDir, err := checkFS(ctx, hc.Dir, "pwd", hc.Dir, OpStat)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "pwd: %v\n", err)
		return interp.CallHandled(1)
	}
	if evalSymlinks {
		fmt.Fprintln(hc.Stdout, physDir)
		return interp.CallHandled(0)
	}
	pwdEnv := hc.Env.Get("PWD").String()
	if pwdEnv != "" && !sameCleanPath(pwdEnv, hc.Dir) {
		if candPhys, cerr := checkFS(ctx, hc.Dir, "pwd", pwdEnv, OpStat); cerr == nil && sameCleanPath(candPhys, physDir) {
			fmt.Fprintln(hc.Stdout, pwdEnv)
			return interp.CallHandled(0)
		}
	}
	fmt.Fprintln(hc.Stdout, hc.Dir)
	return interp.CallHandled(0)
}

func handleCompgenJailed(ctx context.Context, hc interp.HandlerContext, args []string) ([]string, error) {
	action := ""
	prefix := ""
	i := 1
	for i < len(args) {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if len(a) < 2 || a[0] != '-' {
			break
		}
		if a == "-A" {
			if i+1 < len(args) {
				action = args[i+1]
				i += 2
				continue
			}
			action = ""
			i++
			continue
		}
		if strings.HasPrefix(a, "-A") && len(a) > 2 {
			action = a[2:]
			i++
			continue
		}
		for j := 1; j < len(a); j++ {
			switch a[j] {
			case 'c':
				action = "command"
			case 'b':
				action = "builtin"
			case 'a':
				action = "alias"
			default:
				fmt.Fprintf(hc.Stderr, "compgen: invalid option %q\n", a)
				return nil, interp.CallHandled(2)
			}
		}
		i++
	}
	if i < len(args) {
		prefix = args[i]
	}
	switch action {
	case "alias", "builtin", "function":
		// Purely in-memory actions in mvdan/sh.
		return args, nil
	case "command", "":
	default:
		fmt.Fprintf(hc.Stderr, "compgen: %s: invalid action\n", action)
		return nil, interp.CallHandled(2)
	}

	var words []string
	words = append(words, jailedShellBuiltinNames...)
	if reg := extraRegistryPtr.Load(); reg != nil {
		for name := range reg.byName {
			words = append(words, name)
		}
	}
	for _, dir := range filepath.SplitList(hc.Env.Get("PATH").String()) {
		if dir == "" {
			continue
		}
		entries, err := fsReadDir(ctx, hc.Dir, "compgen", dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if e.Type()&os.ModeSymlink != 0 {
				fi, serr := fsStat(ctx, hc.Dir, "compgen", filepath.Join(dir, name))
				if serr != nil || fi.IsDir() || (runtime.GOOS != "windows" && fi.Mode()&0o111 == 0) {
					continue
				}
			} else if runtime.GOOS != "windows" {
				fi, ierr := e.Info()
				if ierr != nil || fi.Mode()&0o111 == 0 {
					continue
				}
			}
			if runtime.GOOS == "windows" {
				ext := strings.ToLower(filepath.Ext(name))
				if ext != ".exe" && ext != ".bat" && ext != ".cmd" && ext != ".com" {
					continue
				}
				name = strings.TrimSuffix(name, filepath.Ext(name))
			}
			words = append(words, name)
		}
	}
	slices.Sort(words)
	anyMatch := false
	for _, w := range slices.Compact(words) {
		if prefix == "" || strings.HasPrefix(w, prefix) {
			fmt.Fprintln(hc.Stdout, w)
			anyMatch = true
		}
	}
	if !anyMatch {
		return nil, interp.CallHandled(1)
	}
	return nil, interp.CallHandled(0)
}

func resolveJailedSourceArgs(ctx context.Context, hc interp.HandlerContext, args []string) ([]string, error) {
	if len(args) < 2 {
		return args, nil
	}
	scriptArg := args[1]
	if scriptArg == "" {
		return args, nil
	}
	hasSep := strings.ContainsRune(scriptArg, '/') || (runtime.GOOS == "windows" && (strings.ContainsRune(scriptArg, '\\') || isRawAbsOrMSYS(scriptArg)))
	var resolved string
	if hasSep {
		resolved = resolveHostPath(hc.Dir, scriptArg)
	} else {
		for _, dir := range filepath.SplitList(hc.Env.Get("PATH").String()) {
			if dir == "" {
				continue
			}
			cand := filepath.Join(dir, scriptArg)
			if fi, err := fsStat(ctx, hc.Dir, args[0], cand); err == nil && !fi.IsDir() {
				resolved = resolveHostPath(hc.Dir, cand)
				break
			}
		}
		if resolved == "" {
			resolved = resolveHostPath(hc.Dir, scriptArg)
		}
	}
	if _, err := checkFS(ctx, hc.Dir, args[0], resolved, OpRead); err != nil {
		fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], err)
		return nil, interp.CallHandled(1)
	}
	if !strings.ContainsRune(resolved, '/') && (runtime.GOOS != "windows" || !strings.ContainsAny(resolved, `:\`)) {
		resolved = "." + string(filepath.Separator) + resolved
	}
	out := make([]string, len(args))
	copy(out, args)
	out[1] = resolved
	return out, nil
}

func cmdCommandShowJailed(ctx context.Context, hc interp.HandlerContext, verbose bool, queries []string) error {
	allOK := true
	for _, q := range queries {
		isExplicit := strings.ContainsAny(q, `/\`) || (runtime.GOOS == "windows" && isRawAbsOrMSYS(q))
		if isExplicit {
			if fi, err := fsStat(ctx, hc.Dir, "command", q); err == nil && !fi.IsDir() && (runtime.GOOS == "windows" || fi.Mode()&0o111 != 0) {
				resolved := resolveHostPath(hc.Dir, q)
				if verbose {
					fmt.Fprintf(hc.Stdout, "%s is %s\n", q, resolved)
				} else {
					fmt.Fprintln(hc.Stdout, resolved)
				}
			} else {
				if verbose {
					fmt.Fprintf(hc.Stderr, "command: %s: not found\n", q)
				}
				allOK = false
			}
			continue
		}
		if def, ok := hc.LookupAlias(q); ok {
			if verbose {
				fmt.Fprintf(hc.Stdout, "%s is aliased to `%s'\n", q, def)
			} else {
				fmt.Fprintf(hc.Stdout, "alias %s=%q\n", q, def)
			}
			continue
		}
		if syntax.IsKeyword(q) {
			if verbose {
				fmt.Fprintf(hc.Stdout, "%s is a shell keyword\n", q)
			} else {
				fmt.Fprintln(hc.Stdout, q)
			}
			continue
		}
		if hc.HasFunc(q) {
			if verbose {
				fmt.Fprintf(hc.Stdout, "%s is a function\n", q)
			} else {
				fmt.Fprintln(hc.Stdout, q)
			}
			continue
		}
		if lookupExtra(q) != nil {
			if verbose {
				fmt.Fprintf(hc.Stdout, "%s is a unish builtin\n", q)
			} else {
				fmt.Fprintln(hc.Stdout, q)
			}
			continue
		}
		if interp.IsBuiltin(q) {
			if verbose {
				fmt.Fprintf(hc.Stdout, "%s is a shell builtin\n", q)
			} else {
				fmt.Fprintln(hc.Stdout, q)
			}
			continue
		}
		if p, err := findExecCtx(ctx, hc, q); err == nil {
			if verbose {
				fmt.Fprintf(hc.Stdout, "%s is %s\n", q, p)
			} else {
				fmt.Fprintln(hc.Stdout, p)
			}
		} else {
			if verbose {
				fmt.Fprintf(hc.Stderr, "command: %s: not found\n", q)
			}
			allOK = false
		}
	}
	if !allOK {
		return interp.CallHandled(1)
	}
	return interp.CallHandled(0)
}

func cmdHashTableJailed(ctx context.Context, hc interp.HandlerContext, args []string) error {
	reset := false
	forget := false
	var names []string
	argv := args[1:]
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch a {
		case "-r":
			reset = true
		case "-d":
			forget = true
		case "-rd", "-dr":
			reset = true
			forget = true
		case "--":
			names = append(names, argv[i+1:]...)
			i = len(argv)
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				fmt.Fprintf(hc.Stderr, "hash: invalid option -- '%s'\n", strings.TrimPrefix(a, "-"))
				return flag.ErrHelp
			}
			names = append(names, a)
		}
	}
	if reset && len(names) == 0 {
		return nil
	}
	if forget {
		if len(names) == 0 {
			fmt.Fprintln(hc.Stderr, "hash: -d: option requires an argument")
			return flag.ErrHelp
		}
		return nil
	}
	if len(names) == 0 {
		fmt.Fprintln(hc.Stdout, "hash: hash table empty")
		return nil
	}
	for _, n := range names {
		if interp.IsBuiltin(n) {
			continue
		}
		if lookupExtra(n) != nil {
			fmt.Fprintf(hc.Stderr, "hash: %s: not found\n", n)
			return exitError{1}
		}
		if _, err := findExecCtx(ctx, hc, n); err != nil {
			fmt.Fprintf(hc.Stderr, "hash: %s: not found\n", n)
			return exitError{1}
		}
	}
	return nil
}

func cmdTypeDescJailed(ctx context.Context, hc interp.HandlerContext, args []string) error {
	mode := ""
	all := false
	noFuncs := false
	var names []string
	argv := args[1:]
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			names = append(names, argv[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 't':
					mode = "-t"
				case 'p':
					mode = "-p"
				case 'P':
					mode = "-P"
				case 'a':
					all = true
				case 'f':
					noFuncs = true
				default:
					fmt.Fprintf(hc.Stderr, "type: -%c: invalid option\n", a[j])
					return flag.ErrHelp
				}
			}
			continue
		}
		names = append(names, a)
	}
	if len(names) == 0 {
		return nil
	}
	code := 0
	for _, n := range names {
		if mode == "-P" {
			if p, err := findExecCtx(ctx, hc, n); err == nil {
				fmt.Fprintln(hc.Stdout, p)
			} else {
				code = 1
			}
			continue
		}
		found := false
		if def, ok := hc.LookupAlias(n); ok {
			found = true
			switch mode {
			case "-t":
				fmt.Fprintln(hc.Stdout, "alias")
			case "-p":
			default:
				fmt.Fprintf(hc.Stdout, "%s is aliased to `%s'\n", n, def)
			}
			if !all {
				continue
			}
		}
		if syntax.IsKeyword(n) {
			found = true
			switch mode {
			case "-t":
				fmt.Fprintln(hc.Stdout, "keyword")
			case "-p":
			default:
				fmt.Fprintf(hc.Stdout, "%s is a shell keyword\n", n)
			}
			if !all {
				continue
			}
		}
		if !noFuncs && hc.HasFunc(n) {
			found = true
			switch mode {
			case "-t":
				fmt.Fprintln(hc.Stdout, "function")
			case "-p":
			default:
				fmt.Fprintf(hc.Stdout, "%s is a function\n", n)
			}
			if !all {
				continue
			}
		}
		if isUnishCommand(n) && !strings.ContainsAny(n, `/\`) {
			found = true
			switch mode {
			case "-t":
				fmt.Fprintln(hc.Stdout, "builtin")
			case "-p":
			default:
				fmt.Fprintf(hc.Stdout, "%s is a unish builtin\n", n)
			}
			if !all {
				continue
			}
		} else if interp.IsBuiltin(n) {
			found = true
			switch mode {
			case "-t":
				fmt.Fprintln(hc.Stdout, "builtin")
			case "-p":
			default:
				fmt.Fprintf(hc.Stdout, "%s is a shell builtin\n", n)
			}
			if !all {
				continue
			}
		}
		if p, err := findExecCtx(ctx, hc, n); err == nil {
			found = true
			switch mode {
			case "-t":
				fmt.Fprintln(hc.Stdout, "file")
			case "-p":
				fmt.Fprintln(hc.Stdout, p)
			default:
				fmt.Fprintf(hc.Stdout, "%s is %s\n", n, p)
			}
		}
		if !found {
			if mode == "" {
				fmt.Fprintf(hc.Stderr, "type: %s: not found\n", n)
			}
			code = 1
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cmdUmaskJailed(ctx context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) == 1 {
		fmt.Fprintf(hc.Stdout, "%04o\n", shellUmask.Load())
		return nil
	}
	endOpts := false
	printed := false
	var modes []string
	for _, a := range args[1:] {
		if !endOpts {
			if a == "--" {
				endOpts = true
				continue
			}
			if a == "-S" {
				printed = true
				fmt.Fprintln(hc.Stdout, umaskSymbolic(int(shellUmask.Load())))
				continue
			}
			if a == "-p" {
				printed = true
				fmt.Fprintf(hc.Stdout, "umask %04o\n", shellUmask.Load())
				continue
			}
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				fmt.Fprintf(hc.Stderr, "umask: invalid option -- '%s'\n", strings.TrimPrefix(a, "-"))
				return flag.ErrHelp
			}
		}
		modes = append(modes, a)
	}
	if len(modes) == 0 {
		if !printed {
			fmt.Fprintf(hc.Stdout, "%04o\n", shellUmask.Load())
		}
		return nil
	}
	for _, a := range modes {
		v, err := strconv.ParseUint(a, 8, 32)
		if err != nil || v > 0o777 {
			fmt.Fprintf(hc.Stderr, "umask: '%s': invalid octal number\n", a)
			return exitError{1}
		}
	}
	if err := checkProc(ctx, "umask", os.Getpid(), "UMASK"); err != nil {
		fmt.Fprintf(hc.Stderr, "umask: %v\n", err)
		return exitError{1}
	}
	fmt.Fprintln(hc.Stderr, "umask: cannot modify host process umask in jailed mode")
	return exitError{1}
}

func cmdUlimitJailed(ctx context.Context, hc interp.HandlerContext, args []string) error {
	showAll := false
	resource := "n"
	var setVal *uint64
	argv := args[1:]
	endOpts := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !endOpts {
			switch {
			case a == "--":
				endOpts = true
				continue
			case a == "-a":
				showAll = true
				continue
			case a == "-n" || a == "-u" || a == "-f" || a == "-c" || a == "-d" || a == "-s" || a == "-v" || a == "-m":
				resource = strings.TrimPrefix(a, "-")
				continue
			case (len(a) == 3 && a[0] == '-' && (a[1] == 'H' || a[1] == 'S')) ||
				(len(a) == 4 && a[0] == '-' && a[1] == 'H' && a[2] == 'S') ||
				(len(a) == 4 && a[0] == '-' && a[1] == 'S' && a[2] == 'H'):
				resource = a[len(a)-1:]
				continue
			case a == "-H" || a == "-S":
				continue
			case strings.HasPrefix(a, "-"):
				fmt.Fprintf(hc.Stderr, "ulimit: invalid option -- '%s'\n", strings.TrimPrefix(a, "-"))
				return flag.ErrHelp
			}
		}
		if v, err := strconv.ParseUint(a, 10, 64); err == nil {
			setVal = &v
		} else {
			fmt.Fprintf(hc.Stderr, "ulimit: '%s': invalid number\n", a)
			return exitError{1}
		}
	}
	if showAll && setVal == nil {
		nofile, _ := ulimitNofile()
		fmt.Fprintf(hc.Stdout, "open files                      (-n) %d\n", nofile)
		return nil
	}
	if setVal != nil {
		if err := checkProc(ctx, "ulimit", os.Getpid(), "RLIMIT"); err != nil {
			fmt.Fprintf(hc.Stderr, "ulimit: %v\n", err)
			return exitError{1}
		}
		fmt.Fprintln(hc.Stderr, "ulimit: cannot modify limit: operation not permitted")
		return exitError{1}
	}
	switch resource {
	case "n":
		nofile, err := ulimitNofile()
		if err != nil {
			fmt.Fprintln(hc.Stderr, "ulimit:", err)
			return exitError{1}
		}
		fmt.Fprintln(hc.Stdout, nofile)
	default:
		fmt.Fprintln(hc.Stdout, "unlimited")
	}
	return nil
}

func cmdHistoryJailed(ctx context.Context, hc interp.HandlerContext, args []string) error {
	a := args[1:]
	optFileArg := func(idx int) string {
		if idx+1 < len(a) && !strings.HasPrefix(a[idx+1], "-") {
			return a[idx+1]
		}
		return ""
	}
	resolveHistTarget := func(explicit string) string {
		if explicit != "" {
			return explicit
		}
		envF := hc.Env.Get("HISTFILE").String()
		if isNullDevPath(envF) {
			return ""
		}
		return envF
	}
	for i := 0; i < len(a); i++ {
		switch a[i] {
		case "-c", "-s":
			return nil
		case "-d":
			if i+1 >= len(a) {
				fmt.Fprintln(hc.Stderr, "history: -d: option requires an argument")
				return exitError{1}
			}
			fmt.Fprintf(hc.Stderr, "history: %s: history position out of range\n", a[i+1])
			return exitError{1}
		case "-p":
			var out []string
			for j := i + 1; j < len(a); j++ {
				exp, _, err := expandBang(a[j], nil)
				if err != nil {
					fmt.Fprintln(hc.Stderr, err.Error())
					return exitError{1}
				}
				out = append(out, exp)
			}
			for _, l := range out {
				fmt.Fprintln(hc.Stdout, l)
			}
			return nil
		case "-r", "-n":
			target := resolveHistTarget(optFileArg(i))
			if target != "" && !isNullDevPath(target) {
				if _, err := fsReadFile(ctx, hc.Dir, "history", target); err != nil {
					fmt.Fprintf(hc.Stderr, "history: %v\n", err)
					return exitError{1}
				}
			}
			return nil
		case "-w":
			target := resolveHistTarget(optFileArg(i))
			if target != "" && !isNullDevPath(target) {
				if err := fsWriteFile(ctx, hc.Dir, "history", target, nil, 0o600); err != nil {
					fmt.Fprintf(hc.Stderr, "history: %v\n", err)
					return exitError{1}
				}
			}
			return nil
		case "-a":
			target := resolveHistTarget(optFileArg(i))
			if target != "" && !isNullDevPath(target) {
				f, err := fsOpenFile(ctx, hc.Dir, "history", target, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
				if err != nil {
					fmt.Fprintf(hc.Stderr, "history: %v\n", err)
					return exitError{1}
				}
				_ = f.Close()
			}
			return nil
		default:
			if strings.HasPrefix(a[i], "-") {
				fmt.Fprintf(hc.Stderr, "history: invalid option -- '%s'\n", strings.TrimPrefix(a[i], "-"))
				return exitError{1}
			}
			if cnt, err := strconv.Atoi(a[i]); err != nil || cnt < 0 {
				fmt.Fprintf(hc.Stderr, "history: %s: numeric argument required\n", a[i])
				return exitError{1}
			}
			return nil
		}
	}
	return nil
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
	if hc.Env == nil {
		return false
	}
	if _, ok := hc.Env.(explicitEnviron); ok {
		return false
	}
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
	if errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{
		"The pipe is being closed",
		"The pipe has been ended", // Go on Windows, head -c path
		"broken pipe",
		"closed pipe",
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

// standaloneCLIMode is true only while RunCLI is executing the standalone unish
// binary. When unish is embedded as a Go library inside a host binary, this
// stays false so background builtins run in-process instead of re-execing
// os.Executable() (which would invoke the host binary with "-c").
var standaloneCLIMode atomic.Bool

// runSelfTracked runs a command as a REAL child process by re-executing the
// unish binary with -c. Used for background statements: our embedded tools
// are external programs in bash, so bash forks them; doing the same gives
// $! a real pid and makes kill/wait/jobs work. Foreground calls keep the
// in-process fast path.
func runSelfTracked(ctx context.Context, args []string) (error, bool) {
	if !standaloneCLIMode.Load() || CapabilityHookFromContext(ctx) != nil {
		return nil, false
	}
	hc := interp.HandlerCtx(ctx)
	self, err := os.Executable()
	if err != nil {
		return nil, false
	}
	cmdline := shellQuoteArgs(args)
	cmd := exec.CommandContext(ctx, self, "-c", cmdline)
	cmd.Dir = hc.Dir
	cmd.Stdin = hc.Stdin
	cmd.Stdout = hc.Stdout
	cmd.Stderr = hc.Stderr
	cmd.Env = shellExecEnv(hc)
	afterStart, afterWait := interp.PrepareExecStdio(cmd)
	if err := cmd.Start(); err != nil {
		afterWait()
		return nil, false
	}
	afterStart()
	defer afterWait()
	hc.ReportBgStart(cmd.Process.Pid)
	j := jobsFromContext(ctx).track(cmd, args)
	<-j.done
	if j.err != nil {
		if isPipeClosed(j.err) {
			return nil, true
		}
		if ee, ok := j.err.(*exec.ExitError); ok {
			if code := signalExitCode(ee); code >= 0 {
				return interp.NewExitStatus(uint8(code)), true
			}
			return interp.NewExitStatus(uint8(ee.ExitCode())), true
		}
		return j.err, true
	}
	return nil, true
}

// shellQuoteArgs renders argv as a single-quoted shell command line.
func shellQuoteArgs(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}
