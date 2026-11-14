package unish

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func versionString(ver string) string {
	if ver == "" {
		return "dev"
	}
	return ver
}

type repeatableFlag []string

func (r *repeatableFlag) String() string {
	if r == nil {
		return ""
	}
	return strings.Join(*r, ",")
}

func (r *repeatableFlag) Set(val string) error {
	*r = append(*r, val)
	return nil
}

type cliJailFlags struct {
	jailRoots  repeatableFlag
	allowRead  repeatableFlag
	allowWrite repeatableFlag
	denyPaths  repeatableFlag
	askPaths   repeatableFlag
	outside    string
	netMode    string
	allowNet   repeatableFlag
	execMode   string
	allowExec  repeatableFlag
}

func (f *cliJailFlags) anySet() bool {
	return len(f.jailRoots) > 0 ||
		len(f.allowRead) > 0 ||
		len(f.allowWrite) > 0 ||
		len(f.denyPaths) > 0 ||
		len(f.askPaths) > 0 ||
		f.outside != "" ||
		f.netMode != "" ||
		len(f.allowNet) > 0 ||
		f.execMode != "" ||
		len(f.allowExec) > 0
}

func buildCLIJail(jf cliJailFlags, harnessMode bool) (*Jail, error) {
	if !jf.anySet() {
		return nil, nil
	}

	cfg := DefaultJailConfig(jf.jailRoots...)

	hasExplicitRootScope := len(jf.jailRoots) > 0 ||
		len(jf.allowRead) > 0 ||
		len(jf.allowWrite) > 0 ||
		jf.outside != ""
	if !hasExplicitRootScope {
		cfg.OutsideAction = DecisionAllow
	}

	for _, p := range jf.allowRead {
		if hasGlobMeta(p) {
			cfg.Rules = append(cfg.Rules, PathRule{
				Pattern:  p,
				Ops:      []AccessOp{OpRead, OpList, OpStat, OpChdir},
				Decision: DecisionAllow,
			})
		} else {
			cfg.ReadOnlyRoots = append(cfg.ReadOnlyRoots, p)
		}
	}

	for _, p := range jf.allowWrite {
		if hasGlobMeta(p) {
			cfg.Rules = append(cfg.Rules, PathRule{
				Pattern:  p,
				Decision: DecisionAllow,
			})
		} else {
			cfg.Roots = append(cfg.Roots, p)
		}
	}

	for _, p := range jf.denyPaths {
		cfg.Rules = append(cfg.Rules, PathRule{
			Pattern:  p,
			Decision: DecisionDeny,
		})
	}

	for _, p := range jf.askPaths {
		cfg.Rules = append(cfg.Rules, PathRule{
			Pattern:  p,
			Decision: DecisionAsk,
		})
	}

	if jf.outside != "" {
		switch strings.ToLower(strings.TrimSpace(jf.outside)) {
		case "deny":
			cfg.OutsideAction = DecisionDeny
		case "ask":
			cfg.OutsideAction = DecisionAsk
		case "allow":
			cfg.OutsideAction = DecisionAllow
		default:
			return nil, fmt.Errorf("invalid --outside mode %q (expected deny, ask, or allow)", jf.outside)
		}
	}

	if jf.netMode != "" {
		switch strings.ToLower(strings.TrimSpace(jf.netMode)) {
		case "deny":
			cfg.Net.Default = DecisionDeny
			cfg.Net.AllowLoopback = false
			cfg.Net.BlockPrivateLAN = true
			cfg.Net.AllowListen = DecisionDeny
		case "loopback":
			cfg.Net.Default = DecisionDeny
			cfg.Net.AllowLoopback = true
			cfg.Net.BlockPrivateLAN = true
			cfg.Net.AllowListen = DecisionDeny
		case "ask":
			cfg.Net.Default = DecisionAsk
			cfg.Net.AllowLoopback = false
			cfg.Net.BlockPrivateLAN = true
			cfg.Net.AllowListen = DecisionAsk
		case "allow":
			cfg.Net.Default = DecisionAllow
			cfg.Net.AllowLoopback = true
			cfg.Net.BlockPrivateLAN = false
			cfg.Net.AllowListen = DecisionAllow
		default:
			return nil, fmt.Errorf("invalid --net mode %q (expected deny, loopback, ask, or allow)", jf.netMode)
		}
	}

	for _, spec := range jf.allowNet {
		nr, err := parseAllowNetSpec(spec)
		if err != nil {
			return nil, err
		}
		cfg.Net.Rules = append(cfg.Net.Rules, nr)
	}

	if jf.execMode != "" {
		switch strings.ToLower(strings.TrimSpace(jf.execMode)) {
		case "builtins", "deny", "allowlist":
			cfg.Exec.ExecDefault = DecisionDeny
		case "ask":
			cfg.Exec.ExecDefault = DecisionAsk
		case "allow":
			cfg.Exec.ExecDefault = DecisionAllow
		default:
			return nil, fmt.Errorf("invalid --exec-mode %q (expected builtins, allowlist, ask, or allow)", jf.execMode)
		}
	}

	cfg.Exec.AllowBinaries = append(cfg.Exec.AllowBinaries, jf.allowExec...)

	// In non-harness interactive terminal runs, install a TTY prompt approver so
	// --outside ask / --net ask / --exec-mode ask work interactively on a TTY
	// while still failing closed when stdin is non-interactive.
	if !harnessMode && termIsTTY(os.Stdin) && termIsTTY(os.Stderr) {
		cfg.Approver = cliTTYApprover
	}

	return NewJail(cfg)
}

func parseAllowNetSpec(raw string) (NetRule, error) {
	spec := strings.TrimSpace(raw)
	if spec == "" {
		return NetRule{}, fmt.Errorf("empty --allow-net pattern")
	}
	if _, err := netip.ParsePrefix(spec); err == nil {
		return NetRule{
			CIDR:     spec,
			Decision: DecisionAllow,
		}, nil
	}
	if host, portSpec, err := net.SplitHostPort(spec); err == nil {
		nr := NetRule{
			Decision: DecisionAllow,
		}
		if _, pErr := netip.ParsePrefix(host); pErr == nil {
			nr.CIDR = host
		} else {
			nr.HostPattern = host
		}
		if portSpec != "" && portSpec != "*" {
			if loStr, hiStr, hasRange := strings.Cut(portSpec, "-"); hasRange {
				lo, err1 := strconv.Atoi(strings.TrimSpace(loStr))
				hi, err2 := strconv.Atoi(strings.TrimSpace(hiStr))
				if err1 != nil || err2 != nil || lo < 0 || hi > 65535 || lo > hi {
					return NetRule{}, fmt.Errorf("invalid --allow-net port range %q", raw)
				}
				nr.PortRange = [2]int{lo, hi}
			} else {
				p, err := strconv.Atoi(portSpec)
				if err != nil || p < 0 || p > 65535 {
					return NetRule{}, fmt.Errorf("invalid --allow-net port in %q", raw)
				}
				nr.Ports = []int{p}
			}
		}
		return nr, nil
	}
	return NetRule{
		HostPattern: spec,
		Decision:    DecisionAllow,
	}, nil
}

func cliTTYApprover(ctx context.Context, req AccessRequest) (Decision, error) {
	if ctx.Err() != nil {
		return DecisionDeny, ctx.Err()
	}
	fmt.Fprintf(os.Stderr, "[unish jail] allow %s on %s? [y=once / s=session / N=deny]: ", req.Op, requestTarget(req))
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && len(line) == 0 {
		return DecisionDeny, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "allow":
		return DecisionAllow, nil
	case "s", "session", "always", "allow_session":
		return DecisionAllowSession, nil
	default:
		return DecisionDeny, nil
	}
}

// RunCLI executes the standalone unish command-line interface using a fresh
// local FlagSet so importing package unish never pollutes global flag state.
func RunCLI(version string, args []string) int {
	prevCLI := standaloneCLIMode.Swap(true)
	defer standaloneCLIMode.Store(prevCLI)

	progName := "unish"
	var rawArgs []string
	if len(args) > 0 {
		if args[0] != "" {
			progName = args[0]
		}
		rawArgs = args[1:]
	}

	fs := flag.NewFlagSet(progName, flag.ContinueOnError)
	command := fs.String("c", "", "entire bash command to run")
	interactiveFlag := fs.Bool("i", false, "force interactive shell")
	harnessFlag := fs.Bool("harness", false, "run persistent JSONL AI-agent harness server over stdio")

	var jf cliJailFlags
	fs.Var(&jf.jailRoots, "jail", "add allowed read/write jail root directory (repeatable)")
	fs.Var(&jf.allowRead, "jail-ro", "add allowed read-only jail root directory or glob (repeatable)")
	fs.Var(&jf.allowRead, "allow-read", "add allowed read-only path or glob (repeatable)")
	fs.Var(&jf.allowWrite, "allow-write", "add allowed writable path or glob (repeatable)")
	fs.Var(&jf.denyPaths, "deny-path", "deny access to matching path or glob (repeatable)")
	fs.Var(&jf.askPaths, "ask-path", "prompt for interactive approval on matching path or glob (repeatable)")
	fs.StringVar(&jf.outside, "outside", "", "policy for paths outside jail roots: deny, ask, or allow")
	fs.StringVar(&jf.outside, "outside-action", "", "policy for paths outside jail roots: deny, ask, or allow")
	fs.StringVar(&jf.netMode, "net", "", "network policy: deny, loopback, ask, or allow")
	fs.Var(&jf.allowNet, "allow-net", "allow network access to host[:port] or CIDR (repeatable)")
	fs.StringVar(&jf.execMode, "exec-mode", "", "external binary policy: builtins, allowlist, ask, or allow")
	fs.Var(&jf.allowExec, "allow-exec", "allow executing named or absolute external binary (repeatable)")

	showVersion := false
	fs.BoolFunc("version", "print version and exit", func(string) error {
		showVersion = true
		return nil
	})
	fs.BoolFunc("V", "print version and exit", func(string) error {
		showVersion = true
		return nil
	})

	if err := fs.Parse(rawArgs); err != nil {
		if showVersion {
			fmt.Println("unish " + versionString(version))
			return 0
		}
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if showVersion {
		fmt.Println("unish " + versionString(version))
		return 0
	}

	hasCommandFlag := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "c" {
			hasCommandFlag = true
		}
	})

	jail, err := buildCLIJail(jf, *harnessFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unish:", err)
		return 2
	}

	if *harnessFlag {
		var sessOpts []SessionOption
		if jail != nil {
			sessOpts = append(sessOpts, WithJail(jail))
		}
		if err := RunHarness(context.Background(), os.Stdin, os.Stdout, sessOpts...); err != nil {
			fmt.Fprintln(os.Stderr, "unish --harness:", err)
			return 1
		}
		return 0
	}

	var src, name string
	var params []string
	if *interactiveFlag && !hasCommandFlag && len(fs.Args()) == 0 {
		if jail != nil {
			return runJailedInteractive(jail, os.Stdin, os.Stdout, os.Stderr, termIsTTY(os.Stdout))
		}
		return runInteractive()
	}

	startDir := ""
	if jail != nil {
		if resolvedDir, derr := resolveInitialSessionDir("", jail); derr == nil {
			startDir = resolvedDir
		}
	}

	switch {
	case hasCommandFlag:
		src = *command
		if len(fs.Args()) > 0 {
			name = fs.Args()[0]
			params = fs.Args()[1:]
		} else {
			name = "bash"
		}
		if strings.TrimSpace(src) == "" {
			return 0
		}
	default:
		rest := fs.Args()
		if len(rest) > 0 {
			var data []byte
			var rerr error
			if jail != nil {
				jctx := WithCapabilityHook(context.Background(), jail)
				data, rerr = fsReadFile(jctx, startDir, "unish", rest[0])
			} else {
				data, rerr = os.ReadFile(rest[0])
			}
			if rerr != nil {
				if errors.Is(rerr, os.ErrPermission) {
					fmt.Fprintf(os.Stderr, "bash: %s: %v\n", rest[0], rerr)
					return 126
				}
				fmt.Fprintf(os.Stderr, "bash: %s: No such file or directory\n", rest[0])
				return 127
			}
			src = string(data)
			name = rest[0]
			params = rest[1:]
		} else if termIsTTY(os.Stdin) {
			// No args and stdin is a real terminal TTY: start the interactive
			// shell (like bash with no args). Non-TTY char devices such as
			// /dev/null or NUL must NOT enter the interactive REPL.
			if jail != nil {
				return runJailedInteractive(jail, os.Stdin, os.Stdout, os.Stderr, termIsTTY(os.Stdout))
			}
			return runInteractive()
		} else {
			if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
				fmt.Fprintln(os.Stderr, "usage: unish -c \"command\" | unish script.sh [args] | unish < script")
				fs.PrintDefaults()
				return 2
			}
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintln(os.Stderr, "bash:", err)
				return 1
			}
			src = string(data)
			name = ""
			params = nil
		}
	}
	if strings.TrimSpace(src) == "" {
		return 0
	}

	prog, err := syntax.NewParser().Parse(strings.NewReader(src), name)
	if err != nil {
		// Parse errors already carry "name:line:col:" (e.g. bash:1:10: ...),
		// like bash's "bash: -c: line 1: ..."; print raw, no double prefix.
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if jail != nil {
		ctx = WithCapabilityHook(ctx, jail)
		ctx = withChdirApprovalStore(ctx)
	}

	var runnerOpts []interp.RunnerOption
	if jail == nil {
		runnerOpts = []interp.RunnerOption{
			interp.StdIO(os.Stdin, os.Stdout, os.Stderr),
			interp.CallHandler(callOverride),
			interp.OpenHandler(shellOpenHandler()),
			interp.ReadDirHandler2(shellReadDirHandler()),
			interp.StatHandler(shellStatHandler()),
			interp.AccessHandler(shellAccessHandler()),
			interp.ExecHandlers(
				trackExec,
				extraHandler,
			),
			interp.ProcSubstHandler(procSubstHandler),
		}
	} else {
		ensureSessionExtrasRegistered()
		cliJobs := NewJobTable()
		defer cliJobs.terminateAll()
		cliSess := &Session{
			sessCtx:    ctx,
			jail:       jail,
			hook:       jail,
			jobTable:   cliJobs,
			inheritEnv: true,
		}
		ctx = WithJobTable(ctx, cliJobs)
		ctx = withSessionInContext(ctx, cliSess)
		cliSess.umask.Store(shellUmask.Load())
		if startDir != "" {
			cliSess.storeCwd(startDir)
			runnerOpts = append(runnerOpts, interp.Dir(startDir))
		}
		runnerOpts = append(runnerOpts,
			interp.StdIO(os.Stdin, os.Stdout, os.Stderr),
			interp.OpenHandler(cliSess.openHandler),
			interp.ReadDirHandler2(cliSess.readDirHandler),
			interp.StatHandler(cliSess.statHandler),
			interp.AccessHandler(cliSess.accessHandler),
			interp.CallHandler(cliSess.callHandler),
			interp.ExecHandlers(
				cliSess.execMiddleware,
				trackExec,
				extraHandler,
			),
			interp.ProcSubstHandler(procSubstHandler),
		)
	}
	if len(params) > 0 {
		runnerOpts = append(runnerOpts, interp.Params(append([]string{"--"}, params...)...))
	}
	r, err := interp.New(runnerOpts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bash:", err)
		return 1
	}
	if jail != nil {
		registerRunnerCompletionHook(r, jail)
		defer unregisterRunnerCompletionHook(r)
	}

	// Let kill enqueue signals for this runner's own traps.
	setActiveRunner(r)
	defer setActiveRunner(nil)

	// Ctrl-C: run the script's INT trap when it has one (bash semantics),
	// otherwise interrupt the shell like before.
	sigCh := make(chan os.Signal, 1)
	// Catch the signals a script can trap, not just Ctrl-C: bash runs the
	// INT/TERM/HUP/QUIT trap when the shell receives one. Without Notify the
	// default action kills the process before any trap can run.
	signal.Notify(sigCh, trapSignals()...)
	defer signal.Stop(sigCh)
	sigDone := make(chan struct{})
	defer close(sigDone)
	go func() {
		for {
			select {
			case <-sigDone:
				return
			case sig := <-sigCh:
				// setSigTrap already registered signal.Notify(r.sigCh, sig);
				// checking TrapRegistered avoids queueing OS signals twice.
				if r.TrapRegistered(sig) {
					continue
				}
				cancel()
			}
		}
	}()

	if err := r.Run(ctx, prog); err != nil {
		var es interp.ExitStatus
		if errors.As(err, &es) {
			return int(es)
		}
		if errors.Is(err, context.Canceled) {
			return 130
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 124
		}
		fmt.Fprintln(os.Stderr, "bash:", err)
		return 1
	}
	return 0
}

// runJailedInteractive runs an interactive statement loop inside a jailed
// Session without reading or writing host ~/.unishrc or ~/.unish_history.
func runJailedInteractive(jail *Jail, in io.Reader, out, errOut io.Writer, showPrompt bool) int {
	sess, err := NewSession(
		WithJail(jail),
		WithStdin(in),
		WithStdout(out),
		WithStderr(errOut),
	)
	if err != nil {
		fmt.Fprintln(errOut, "bash:", err)
		return 1
	}
	defer sess.Close()

	setActiveRunner(sess.runner)
	defer setActiveRunner(nil)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, trapSignals()...)
	sigDone := make(chan struct{})
	defer func() {
		signal.Stop(sigCh)
		close(sigDone)
	}()
	go func() {
		for {
			select {
			case <-sigDone:
				return
			case sig := <-sigCh:
				if sess.runner != nil && sess.runner.QueueSignal(sig) {
					continue
				}
				if sig == os.Interrupt {
					sess.CancelCurrent()
				}
			}
		}
	}()

	exitedExplicitly := false
	for {
		if showPrompt {
			p := "$ "
			if sess.LastExitCode() != 0 {
				p = "# "
			}
			fmt.Fprint(out, p)
		}
		line, ok := readPlainLine(in)
		if !ok {
			if showPrompt {
				fmt.Fprintln(out)
			}
			break
		}
		full := line
		for incomplete(full) {
			if showPrompt {
				fmt.Fprint(out, "> ")
			}
			cont, ok2 := readPlainLine(in)
			if !ok2 {
				break
			}
			full += "\n" + cont
		}
		if strings.TrimSpace(full) == "" {
			continue
		}
		_, _ = sess.Exec(context.Background(), full)
		if sess.runner.Exited() {
			exitedExplicitly = true
			break
		}
	}

	if !exitedExplicitly {
		_, _ = sess.Exec(context.Background(), fmt.Sprintf("builtin exit %d", sess.LastExitCode()&0xff))
	}
	return sess.LastExitCode()
}
