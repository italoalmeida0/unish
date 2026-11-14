package unish

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/interp"
)

func cmdTimeout(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("timeout", hc.Stderr)
	sigName := fs.String("s", "TERM", "")
	fs.StringVar(sigName, "signal", "TERM", "")
	killAfter := fs.String("k", "", "")
	fs.StringVar(killAfter, "kill-after", "", "")
	preserve := fs.Bool("preserve-status", false, "")
	fs.BoolVar(preserve, "p", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(hc.Stderr, "timeout: missing operand")
		return flag.ErrHelp
	}
	dur, err := parseDurationLoose(rest[0])
	if err != nil || dur < 0 {
		fmt.Fprintf(hc.Stderr, "timeout: invalid time interval '%s'\n", rest[0])
		return exitError{125}
	}
	if len(rest) < 2 {
		fmt.Fprintln(hc.Stderr, "timeout: missing command")
		return flag.ErrHelp
	}
	cmdArgs := rest[1:]
	if dur == 0 {
		// GNU: DURATION 0 disables the timeout; run directly while still
		// honoring parent context cancellation.
		if cmd := lookupExtra(cmdArgs[0]); cmd != nil {
			return invokeExtra(ctx, hc, cmd, cmdArgs)
		}
		tp, err := startTimeoutProc(ctx, hc, cmdArgs)
		if err != nil {
			return err
		}
		res := make(chan error, 1)
		go func() { res <- tp.wait() }()
		select {
		case err := <-res:
			return err
		case <-ctx.Done():
			_ = tp.signal("KILL")
			select {
			case <-res:
			case <-time.After(2 * time.Second):
			}
			return ctx.Err()
		}
	}
	sig := strings.ToUpper(strings.TrimPrefix(strings.TrimPrefix(*sigName, "-"), "SIG"))

	tctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()

	if cmd := lookupExtra(cmdArgs[0]); cmd != nil {
		// Bound the builtin: after the deadline its stdout/stderr
		// fail with EPIPE, so write-loops (yes, cat, ...) exit via
		// their error path instead of leaking forever. Truly-stuck
		// builtins blocked in Read (sort on infinite stdin) still
		// can't be killed (Go can't kill goroutines), but the gated
		// writer guarantees the common write-loop case is reaped.
		ghc, gate := gateHCOnDone(hc)
		res := make(chan error, 1)
		go func() {
			res <- invokeExtra(tctx, ghc, cmd, cmdArgs)
		}()
		select {
		case err := <-res:
			gate.close()
			return timeoutResult(err, false, *preserve, sig)
		case <-tctx.Done():
			gate.trip()
			select {
			case err := <-res:
				gate.close()
				return timeoutResult(err, true, *preserve, sig)
			case <-time.After(gracePeriod(*killAfter)):
				return exitError{124}
			}
		}
	}

	tp, err := startTimeoutProc(ctx, hc, cmdArgs)
	if err != nil {
		return err
	}
	res := make(chan error, 1)
	go func() { res <- tp.wait() }()
	select {
	case err := <-res:
		return timeoutResult(err, false, *preserve, sig)
	case <-tctx.Done():
	}
	_ = tp.signal(sig)
	grace := gracePeriod(*killAfter)
	select {
	case err := <-res:
		return timeoutResult(err, true, *preserve, sig)
	case <-time.After(grace):
	}
	_ = tp.signal("KILL")
	select {
	case err := <-res:
		return timeoutResult(err, true, *preserve, "KILL")
	case <-time.After(2 * time.Second):
	}
	return exitError{124}
}

func timeoutResult(err error, timedOut, preserve bool, sig string) error {
	if !timedOut {
		if err != nil {
			if ee, ok := err.(exitError); ok {
				return ee
			}
			if err == context.DeadlineExceeded || err == context.Canceled {
				return exitError{124}
			}
			return err
		}
		return nil
	}
	if preserve {
		if err != nil {
			if ee, ok := err.(exitError); ok {
				return ee
			}
			if err == context.DeadlineExceeded || err == context.Canceled {
				return exitError{128 + sigNumber(sig)}
			}
		}
		return exitError{128 + sigNumber(sig)}
	}
	if sig != "TERM" {
		return exitError{128 + sigNumber(sig)}
	}
	return exitError{124}
}

func sigNumber(sig string) int {
	switch sig {
	case "HUP":
		return 1
	case "INT":
		return 2
	case "QUIT":
		return 3
	case "KILL":
		return 9
	case "USR1":
		return 10
	case "USR2":
		return 12
	case "TERM":
		return 15
	default:
		if n, err := strconv.Atoi(sig); err == nil {
			return n
		}
		return 15
	}
}

func gracePeriod(s string) time.Duration {
	if s == "" {
		return 100 * time.Millisecond
	}
	if d, err := parseDurationLoose(s); err == nil && d >= 0 {
		return d
	}
	return 100 * time.Millisecond
}

type timeoutProc struct {
	cmd *exec.Cmd
	job *trackedJob
}

// epipeErr is returned by gateWriter after the timeout trips, so
// builtins blocked in a write loop exit via their error path.
type epipeErr struct{}

func (epipeErr) Error() string { return "write |1: broken pipe" }

// gateWriter passes writes through until tripped; afterwards every
// Write fails, unblocking write-loop builtins after a timeout.
type gateWriter struct {
	mu      sync.Mutex
	dst     io.Writer
	peer    *gateWriter
	tripped bool
	closed  bool
}

func (w *gateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tripped || w.closed {
		return 0, epipeErr{}
	}
	return w.dst.Write(p)
}

func (w *gateWriter) trip() {
	w.mu.Lock()
	w.tripped = true
	peer := w.peer
	w.mu.Unlock()
	if peer != nil {
		peer.mu.Lock()
		peer.tripped = true
		peer.mu.Unlock()
	}
}

func (w *gateWriter) close() {
	w.mu.Lock()
	w.closed = true
	peer := w.peer
	w.mu.Unlock()
	if peer != nil {
		peer.mu.Lock()
		peer.closed = true
		peer.mu.Unlock()
	}
}

// gateHCOnDone copies hc with stdout/stderr wrapped so a timeout can
// fail further writes on both streams. Returns the wrapped context and
// the linked gate (trip it on deadline; close it on clean return).
func gateHCOnDone(hc interp.HandlerContext) (interp.HandlerContext, *gateWriter) {
	eg := &gateWriter{dst: hc.Stderr}
	og := &gateWriter{dst: hc.Stdout, peer: eg}
	ghc := hc
	ghc.Stdout = og
	ghc.Stderr = eg
	return ghc, og
}

func resolveTimeoutPathExec(hc interp.HandlerContext, name string) (string, error) {
	pathVal := shellGetenv(hc, "PATH")
	for _, dir := range strings.Split(pathVal, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		searchDir := dir
		if !filepath.IsAbs(searchDir) {
			searchDir = resolveHostPath(hc.Dir, searchDir)
		}
		for _, cand := range execCandidates(searchDir, name) {
			if fi, err := os.Stat(cand); err == nil && isExecutableFileInfo(fi) {
				return cand, nil
			}
		}
	}
	return "", fmt.Errorf("%s not found", name)
}

func startTimeoutProc(ctx context.Context, hc interp.HandlerContext, cmdArgs []string) (*timeoutProc, error) {
	for _, a := range cmdArgs[1:] {
		waitProcSubstProducerCtx(ctx, a)
	}
	rawCmd := cmdArgs[0]
	var execPath string
	var alreadyCheckedCanon string
	var checkedBin string
	if isAbsOrRel(rawCmd) {
		cb, cerr := checkExec(ctx, "timeout", rawCmd, cmdArgs, hc.Dir)
		if cerr != nil {
			fmt.Fprintf(hc.Stderr, "timeout: failed to run command '%s': %v\n", rawCmd, cerr)
			return nil, exitError{126}
		}
		alreadyCheckedCanon, _ = canonicalizeForCheck(resolveHostPath(hc.Dir, rawCmd))
		checkedBin = cb
		lp, err := resolveDirectExecPath(hc.Dir, rawCmd)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "timeout: failed to run command '%s': %v\n", rawCmd, err)
			if strings.Contains(err.Error(), "Is a directory") || errors.Is(err, os.ErrPermission) {
				return nil, exitError{126}
			}
			return nil, exitError{127}
		}
		execPath = lp
	} else {
		lp, err := resolveTimeoutPathExec(hc, rawCmd)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "timeout: failed to run command '%s': No such file or directory\n", rawCmd)
			return nil, exitError{127}
		}
		execPath = lp
	}
	pathCanon, _ := canonicalizeForCheck(resolveHostPath(hc.Dir, execPath))
	if alreadyCheckedCanon == "" || !sameCleanPath(pathCanon, alreadyCheckedCanon) {
		cb, err := checkExec(ctx, "timeout", execPath, cmdArgs, hc.Dir)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "timeout: failed to run command '%s': %v\n", rawCmd, err)
			return nil, exitError{126}
		}
		checkedBin = cb
	} else if CapabilityHookFromContext(ctx) == nil {
		checkedBin = execPath
	}
	c := exec.Command(checkedBin, cmdArgs[1:]...)
	c.Stdin = hc.Stdin
	c.Stdout = hc.Stdout
	c.Stderr = hc.Stderr
	c.Dir = hc.Dir
	c.Env = shellExecEnv(hc)
	if err := c.Start(); err != nil {
		fmt.Fprintf(hc.Stderr, "timeout: failed to run command '%s': %v\n", rawCmd, err)
		if os.IsNotExist(err) {
			return nil, exitError{127}
		}
		return nil, exitError{126}
	}
	j := jobsFromContext(ctx).track(c, cmdArgs)
	return &timeoutProc{cmd: c, job: j}, nil
}

func (p *timeoutProc) wait() error {
	var err error
	if p.job != nil {
		<-p.job.done
		err = p.job.err
	} else {
		err = p.cmd.Wait()
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if code := signalExitCode(ee); code >= 0 {
				return exitError{code}
			}
			return exitError{ee.ExitCode()}
		}
		return err
	}
	return nil
}

func (p *timeoutProc) signal(sig string) error {
	if p.cmd.Process == nil {
		return nil
	}
	return killProc(p.cmd.Process, sig)
}

func isAbsOrRel(p string) bool {
	return strings.ContainsAny(p, `/\`) || (runtime.GOOS == "windows" && len(p) > 1 && p[1] == ':')
}
