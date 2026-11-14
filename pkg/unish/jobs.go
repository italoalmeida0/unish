package unish

// Job-control bypass for mvdan/sh.
//
// Background statements (`foo &`) run as in-process goroutines and $!
// expands to a fake id ("g1"), because a custom exec handler is
// registered (see interp/runner.go: only the DEFAULT exec handler may
// report a real PID). Real shells fork, so $! is always a killable,
// waitable OS pid.
//
// This file implements a job table keyed by REAL OS pids, fed by a
// tracking exec middleware (trackExec) that wraps extraHandler and
// records every external process start. Shell builtins that need job
// semantics (kill, wait, jobs) consult the table first:
//
//   - `kill $!` / `kill %1` / `kill %n` / `kill %%` / `kill %+` / `kill %-`
//     resolve fake ids and jobspecs to the newest live child and signal
//     THAT process, instead of failing with `strconv.Atoi: parsing "g1"`.
//   - `wait $!` returns the real exit status of the tracked process.
//   - `jobs` lists tracked background processes.
//
// Design notes:
//   - The table is best-effort: entries are added on process START
//     (before Wait), keyed by pid, with the argv for %jobspec matching.
//   - Fake ids ("g1", "g2", ...) map positionally to start order, like
//     mvdan/sh numbers them (1-indexed in launch order).
//   - wait/$! semantics: mvdan/sh already waits for the background
//     goroutine and sets $?'s value from it; the table only needs to
//     make kill deliver the signal and make `jobs` informative. For
//     `wait <pid>` with a real pid we wait on the tracked process and
//     return its exit code.
//   - No cgo, no platform-specific code: os.Process + exec.Cmd only.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func init() {
	registerExtra("kill", cmdKillNetproc)
	interp.RegisterFastCallHandler(callOverride, isOverriddenCallNetproc, fastCallOverrideNetproc)
}

func isOverriddenCallNetproc(name string) bool {
	if len(name) == 0 {
		return false
	}
	if name == "kill" || name == "jobs" {
		return true
	}
	return isOverriddenCall(name)
}

func fastCallOverrideNetproc(ctx context.Context, hc interp.HandlerContext, args []string) ([]string, error) {
	if len(args) == 0 {
		return args, nil
	}
	switch args[0] {
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
	case "jobs":
		if err := cmdJobsCtx(ctx, hc, args); err != nil {
			if ee, ok := err.(exitError); ok {
				return nil, interp.CallHandled(uint8(ee.code))
			}
			return nil, interp.CallHandled(1)
		}
		return nil, interp.CallHandled(0)
	}
	return fastCallOverride(ctx, hc, args)
}

// trackedJob is one external process started by this shell.
type trackedJob struct {
	id      int
	pid     int
	argv    []string
	cmd     *exec.Cmd
	proc    *os.Process
	done    chan struct{}
	code    int // exit code once done; -1 while running
	err     error
	started time.Time
}

// jobTable is a registry of external children for a shell session or process.
type jobTable struct {
	mu   sync.Mutex
	jobs []*trackedJob // launch order; each live job keeps a stable id
}

// JobTable is an isolated per-session registry of external child processes.
type JobTable = jobTable

type jobTableKey struct{}

var (
	globalJobs         = &jobTable{}
	hasCustomJobTables atomic.Bool
)

// NewJobTable creates a fresh per-session JobTable so concurrent embedded
// sessions do not share `jobs`, `wait`, or `kill %n` state.
func NewJobTable() *JobTable {
	return &jobTable{}
}

// WithJobTable attaches jt to ctx (pass nil to fall back to globalJobs).
func WithJobTable(ctx context.Context, jt *JobTable) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if jt != nil {
		hasCustomJobTables.Store(true)
	}
	return context.WithValue(ctx, jobTableKey{}, jt)
}

func jobsFromContext(ctx context.Context) *jobTable {
	if !hasCustomJobTables.Load() || ctx == nil {
		return globalJobs
	}
	if jt, _ := ctx.Value(jobTableKey{}).(*jobTable); jt != nil {
		return jt
	}
	return globalJobs
}

// maxTrackedJobs bounds the registry: finished jobs are reaped, and if
// more than this many are still RUNNING, the oldest finished slots are
// reused. Prevents unbounded growth in long agent sessions.
const maxTrackedJobs = 256

// track registers a just-started external process. Finished jobs are
// reaped (removed) once observed, so the table never grows forever.
func (t *jobTable) track(cmd *exec.Cmd, argv []string) *trackedJob {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// Sweep finished jobs BEFORE assigning the new job ID so the new ID
	// follows bash's rule: 1 when no active jobs remain, else max(id)+1.
	t.sweep(0)
	j := &trackedJob{
		pid:     cmd.Process.Pid,
		argv:    append([]string(nil), argv...),
		cmd:     cmd,
		proc:    cmd.Process,
		done:    make(chan struct{}),
		code:    -1,
		started: time.Now(),
	}
	t.mu.Lock()
	if len(t.jobs) == 0 {
		j.id = 1
	} else {
		j.id = t.jobs[len(t.jobs)-1].id + 1
	}
	t.jobs = append(t.jobs, j)
	t.mu.Unlock()
	interp.NoteBgTrackedPID(j.pid)
	go func() {
		err := cmd.Wait()
		j.err = err
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				j.code = ee.ExitCode()
			} else {
				j.code = 1
			}
		} else {
			j.code = 0
		}
		close(j.done)
	}()
	return j
}

// sweep drops finished jobs, keeping at most keepRecent completions.
// Waiters hold the *trackedJob pointer, so codes stay readable after
// the entry leaves the table; with keepRecent=0 every observe/track
// point reaps the dead, bounding the table by the live set. `wait $!`
// still works: the job is present while running, and the code is
// returned before the next sweep drops it.
func (t *jobTable) sweep(keepRecent int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Count finished from oldest; drop all but the newest keepRecent.
	fin := 0
	for _, j := range t.jobs {
		select {
		case <-j.done:
			fin++
		default:
		}
	}
	drop := fin - keepRecent
	if drop <= 0 {
		// Still enforce the hard cap on TOTAL entries.
		if len(t.jobs) > maxTrackedJobs {
			kept := t.jobs[len(t.jobs)-maxTrackedJobs:]
			t.jobs = append([]*trackedJob(nil), kept...)
		}
		return
	}
	kept := t.jobs[:0]
	for _, j := range t.jobs {
		select {
		case <-j.done:
			if drop > 0 {
				drop--
				continue
			}
		default:
		}
		kept = append(kept, j)
	}
	t.jobs = kept
}

// byFakeID resolves "g1".."gN" (what $! expands to) to a job.
func (t *jobTable) byFakeID(id string) *trackedJob {
	if !strings.HasPrefix(id, "g") {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(id, "g"))
	if err != nil || n < 1 {
		return nil
	}
	return t.byIndex(n)
}

// byPID resolves a real pid string to the newest matching job.
func (t *jobTable) byPID(pid int) *trackedJob {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.jobs) - 1; i >= 0; i-- {
		if t.jobs[i].pid == pid {
			return t.jobs[i]
		}
	}
	return nil
}

// newest returns the most recently started job, or nil.
func (t *jobTable) newest() *trackedJob {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.jobs) == 0 {
		return nil
	}
	return t.jobs[len(t.jobs)-1]
}

// previous returns the second-most-recently started job (%-), or nil.
func (t *jobTable) previous() *trackedJob {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.jobs) < 2 {
		return nil
	}
	return t.jobs[len(t.jobs)-2]
}

// byIndex resolves %n to the tracked job carrying job ID n.
func (t *jobTable) byIndex(n int) *trackedJob {
	if n < 1 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, j := range t.jobs {
		if j.id == n {
			return j
		}
	}
	return nil
}

// alive reports whether the job's process is still running.
func (j *trackedJob) alive() bool {
	if j == nil {
		return false
	}
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

// waitForLaunch waits only while an actual `&` background subshell is in the
// middle of starting its external process; returns immediately (0ms) when no
// background launch is in flight.
func (t *jobTable) waitForLaunch() {
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		n, ch := interp.PendingBgStarts()
		if n <= 0 {
			return
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			return
		}
		select {
		case <-ch:
		case <-time.After(rem):
			return
		}
	}
}

// waitForFresh waits for any in-flight `&` launch to register before resolving %%/\%+.
func (t *jobTable) waitForFresh() {
	t.waitForLaunch()
}

func (t *jobTable) resolveJobSpec(arg string) *trackedJob {
	t.waitForLaunch()
	if arg == "%%" || arg == "%+" {
		t.waitForFresh()
	}
	if arg == "" {
		return nil
	}
	if arg == "%%" || arg == "%+" {
		return t.newest()
	}
	if arg == "%-" {
		return t.previous()
	}
	if strings.HasPrefix(arg, "%") {
		rest := strings.TrimPrefix(arg, "%")
		if n, err := strconv.Atoi(rest); err == nil {
			return t.byIndex(n)
		}
		if strings.HasPrefix(rest, "?") {
			sub := rest[1:]
			t.mu.Lock()
			defer t.mu.Unlock()
			for i := len(t.jobs) - 1; i >= 0; i-- {
				for _, a := range t.jobs[i].argv {
					if strings.Contains(a, sub) {
						return t.jobs[i]
					}
				}
			}
			return nil
		}
		// %name: prefix match on argv[0] basename.
		t.mu.Lock()
		defer t.mu.Unlock()
		for i := len(t.jobs) - 1; i >= 0; i-- {
			if len(t.jobs[i].argv) > 0 {
				base := t.jobs[i].argv[0]
				if idx := strings.LastIndexAny(base, "/\\"); idx >= 0 {
					base = base[idx+1:]
				}
				if strings.HasPrefix(base, rest) {
					return t.jobs[i]
				}
			}
		}
		return nil
	}
	if j := t.byFakeID(arg); j != nil {
		return j
	}
	if pid, err := strconv.Atoi(arg); err == nil {
		return t.byPID(pid)
	}
	return nil
}

// trackExec is an exec middleware kept for chain completeness and per-session
// job/capability isolation when a custom JobTable or CapabilityHook is active.
func trackExec(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		for _, a := range args[1:] {
			waitProcSubstProducerCtx(ctx, a)
		}
		jt := jobsFromContext(ctx)
		if (jt != globalJobs || CapabilityHookFromContext(ctx) != nil) &&
			len(args) > 0 && !interp.IsBuiltin(args[0]) && lookupExtra(args[0]) == nil {
			if err, handled := runExternalTrackedInTable(ctx, jt, args); handled {
				return err
			}
		}
		return next(ctx, args)
	}
}

func isExecTextBusy(err error) bool {
	return runtime.GOOS != "windows" && errors.Is(err, syscall.ETXTBSY)
}

func isExecFormatError(err error) bool {
	return runtime.GOOS != "windows" && errors.Is(err, syscall.ENOEXEC)
}

func jailedOpenHandler(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	if isWriteOnlyDevNull(path, flag) {
		return sharedDevNull, nil
	}
	hc := interp.HandlerCtx(ctx)
	f, err := fsOpenFile(ctx, hc.Dir, "open", path, flag, perm)
	if err != nil {
		var pe *os.PathError
		if !errors.As(err, &pe) {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		return nil, err
	}
	return f, nil
}

func jailedReadDirHandler(ctx context.Context, path string) ([]fs.DirEntry, error) {
	hc := interp.HandlerCtx(ctx)
	return fsReadDir(ctx, hc.Dir, "glob", path)
}

func jailedStatHandler(ctx context.Context, path string, followSymlinks bool) (fs.FileInfo, error) {
	hc := interp.HandlerCtx(ctx)
	if followSymlinks {
		return fsStat(ctx, hc.Dir, "stat", path)
	}
	return fsLstat(ctx, hc.Dir, "stat", path)
}

func jailedAccessHandler(ctx context.Context, path string, mode interp.AccessMode) error {
	hc := interp.HandlerCtx(ctx)
	op := OpStat
	switch {
	case mode&interp.AccessWrite != 0:
		op = OpWrite
	case mode&interp.AccessRead != 0:
		op = OpRead
	}
	checked, err := checkFS(ctx, hc.Dir, "access", path, op)
	if err != nil {
		return err
	}
	return interp.DefaultAccessHandler()(ctx, checked, mode)
}

// runJailedENOEXEC executes a shebang-less +x script inside a fully jailed
// sub-runner (never mvdan/sh's unjailed DefaultExecHandler.runScriptENOEXEC).
func runJailedENOEXEC(ctx context.Context, hc interp.HandlerContext, checkedBin string, args []string) error {
	b, err := fsReadFile(ctx, hc.Dir, args[0], checkedBin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
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
	opts := []interp.RunnerOption{
		interp.Dir(hc.Dir),
		interp.Env(expand.ListEnviron(shellExecEnv(hc)...)),
		interp.StdIO(hc.Stdin, hc.Stdout, hc.Stderr),
		interp.CallHandler(callOverride),
		interp.OpenHandler(jailedOpenHandler),
		interp.ReadDirHandler2(jailedReadDirHandler),
		interp.StatHandler(jailedStatHandler),
		interp.AccessHandler(jailedAccessHandler),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	}
	if len(args) > 1 {
		opts = append(opts, interp.Params(append([]string{"--"}, args[1:]...)...))
	}
	r, err := interp.New(opts...)
	if err != nil {
		return err
	}
	return r.Run(ctx, file)
}

func runExternalTrackedInTable(ctx context.Context, jt *jobTable, args []string) (error, bool) {
	hc := interp.HandlerCtx(ctx)
	var alreadyCheckedCanon string
	var checkedBin string
	if isAbsOrRel(args[0]) {
		cb, cerr := checkExec(ctx, args[0], args[0], args, hc.Dir)
		if cerr != nil {
			if hc.InBackground() {
				hc.ReportBgStart(0)
			}
			fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], cerr)
			return interp.NewExitStatus(126), true
		}
		alreadyCheckedCanon, _ = canonicalizeForCheck(resolveHostPath(hc.Dir, args[0]))
		checkedBin = cb
	}
	path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
	if err != nil {
		if isAbsOrRel(args[0]) {
			if hc.InBackground() {
				hc.ReportBgStart(0)
			}
			resolved := resolveHostPath(hc.Dir, args[0])
			if fi, serr := os.Stat(resolved); serr == nil {
				if fi.IsDir() {
					fmt.Fprintf(hc.Stderr, "%s: Is a directory\n", args[0])
				} else {
					fmt.Fprintf(hc.Stderr, "%s: Permission denied\n", args[0])
				}
				return interp.NewExitStatus(126), true
			}
			fmt.Fprintf(hc.Stderr, "%s: No such file or directory\n", args[0])
			return interp.NewExitStatus(127), true
		}
		return nil, false
	}
	pathCanon, _ := canonicalizeForCheck(resolveHostPath(hc.Dir, path))
	if alreadyCheckedCanon == "" || !sameCleanPath(pathCanon, alreadyCheckedCanon) {
		cb, cerr := checkExec(ctx, args[0], path, args, hc.Dir)
		if cerr != nil {
			if hc.InBackground() {
				hc.ReportBgStart(0)
			}
			fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], cerr)
			return interp.NewExitStatus(126), true
		}
		checkedBin = cb
	} else if CapabilityHookFromContext(ctx) == nil {
		checkedBin = path
	}

	newCmd := func() (*exec.Cmd, func(), func()) {
		c := exec.CommandContext(ctx, checkedBin, args[1:]...)
		c.Dir = hc.Dir
		c.Stdin = hc.Stdin
		c.Stdout = hc.Stdout
		c.Stderr = hc.Stderr
		c.Env = shellExecEnv(hc)
		as, aw := interp.PrepareExecStdio(c)
		return c, as, aw
	}
	cmd, afterStart, afterWait := newCmd()
	err = cmd.Start()
	for delay := time.Millisecond; err != nil && isExecTextBusy(err) && delay < 300*time.Millisecond; delay *= 2 {
		afterWait()
		time.Sleep(delay)
		cmd, afterStart, afterWait = newCmd()
		err = cmd.Start()
	}
	if err != nil {
		afterWait()
		if isExecFormatError(err) {
			if hc.InBackground() {
				hc.ReportBgStart(0)
			}
			return runJailedENOEXEC(ctx, hc, checkedBin, args), true
		}
		if hc.InBackground() {
			hc.ReportBgStart(0)
		}
		fmt.Fprintf(hc.Stderr, "%v\n", err)
		var execErr *exec.Error
		if errors.As(err, &execErr) || os.IsNotExist(err) {
			return interp.NewExitStatus(127), true
		}
		return interp.NewExitStatus(126), true
	}
	afterStart()
	defer afterWait()
	hc.ReportBgStart(cmd.Process.Pid)
	j := jt.track(cmd, args)
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

// trackCmd starts an external command with job-table tracking. Used by
// in-shell starters that need real pids (timeout -s KILL paths, etc.).
func trackCmd(hc interp.HandlerContext, name string, argv []string) (*trackedJob, error) {
	path, err := findExec(hc, name)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Dir = hc.Dir
	cmd.Stdin = hc.Stdin
	cmd.Stdout = hc.Stdout
	cmd.Stderr = hc.Stderr
	cmd.Env = shellExecEnv(hc)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return globalJobs.track(cmd, argv), nil
}

// cmdJobs implements `jobs [-l]` against globalJobs (kept for signature compatibility).
func cmdJobs(hc interp.HandlerContext, args []string) error {
	return cmdJobsCtx(context.Background(), hc, args)
}

// cmdJobsCtx implements `jobs [-l]` against the session's JobTable in ctx.
func cmdJobsCtx(ctx context.Context, hc interp.HandlerContext, args []string) error {
	jt := jobsFromContext(ctx)
	jt.waitForLaunch()
	jt.sweep(0)
	long := false
	for _, a := range args[1:] {
		if a == "-l" {
			long = true
		}
	}
	jt.mu.Lock()
	defer jt.mu.Unlock()
	for _, j := range jt.jobs {
		state := "Done"
		select {
		case <-j.done:
		default:
			state = "Running"
		}
		if long {
			fmt.Fprintf(hc.Stdout, "[%d] %d %s %s\n", j.id, j.pid, state, strings.Join(j.argv, " "))
		} else {
			fmt.Fprintf(hc.Stdout, "[%d] %s %s\n", j.id, state, strings.Join(j.argv, " "))
		}
	}
	return nil
}

// cmdWaitTracked implements `wait [pid|$!|%jobspec...]` against real
// processes. Returns (code, true) if it handled the invocation.
func cmdWaitTracked(ctx context.Context, args []string) (int, bool) {
	if len(args) < 1 || args[0] != "wait" {
		return 0, false
	}
	jt := jobsFromContext(ctx)
	hc := interp.HandlerCtx(ctx)
	var operands []string
	endOpts := false
	for _, a := range args[1:] {
		if !endOpts {
			if a == "--" {
				endOpts = true
				continue
			}
			if strings.HasPrefix(a, "-") && a != "-" {
				return 0, false
			}
		}
		operands = append(operands, a)
	}
	if len(operands) == 0 {
		jt.waitForLaunch()
		jt.mu.Lock()
		jobs := append([]*trackedJob(nil), jt.jobs...)
		jt.mu.Unlock()
		for _, j := range jobs {
			select {
			case <-j.done:
			case <-ctx.Done():
				fmt.Fprintln(hc.Stderr, "wait:", ctx.Err())
				return 1, true
			}
		}
		jt.sweep(0)
		// Let callOverride pass bare `wait` / `wait --` through to mvdan/sh's
		// builtin so in-process background goroutines (subshells, procsubst)
		// are awaited as well.
		return 0, false
	}
	code := 0
	allInRunner := true
	for _, a := range operands {
		inRunner := !strings.HasPrefix(a, "%") && hc.HasBgProc(a)
		if !inRunner {
			allInRunner = false
		}
		j := jt.resolveJobSpec(a)
		if j == nil {
			if inRunner {
				continue
			}
			// Not a tracked external: leave to mvdan/sh's builtin
			// (in-process jobs like `sleep &`, real pids, errors).
			return 0, false
		}
		select {
		case <-j.done:
			code = j.code
		case <-ctx.Done():
			fmt.Fprintln(hc.Stderr, "wait:", ctx.Err())
			return 1, true
		}
	}
	jt.sweep(0)
	if allInRunner {
		// Delegate exit-status reporting to mvdan/sh's wait builtin so exact
		// 0..255 exit statuses (e.g. 42) are preserved instead of collapsing
		// to callOverride's ["false"] (1).
		return 0, false
	}
	return code, true
}
