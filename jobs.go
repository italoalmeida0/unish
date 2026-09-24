package main

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
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// trackedJob is one external process started by this shell.
type trackedJob struct {
	pid     int
	argv    []string
	cmd     *exec.Cmd
	proc    *os.Process
	done    chan struct{}
	code    int // exit code once done; -1 while running
	err     error
	started time.Time
}

// jobTable is the process-wide registry of external children.
type jobTable struct {
	mu   sync.Mutex
	jobs []*trackedJob // launch order; index i == fake id "g{i+1}"
}

var globalJobs = &jobTable{}

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
	t.jobs = append(t.jobs, j)
	t.mu.Unlock()
	t.sweep(0)
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
			copy(kept, t.jobs[len(t.jobs)-maxTrackedJobs:])
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
	t.mu.Lock()
	defer t.mu.Unlock()
	if n > len(t.jobs) {
		return nil
	}
	return t.jobs[n-1]
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

// byIndex resolves %n (1-indexed launch order) to a job.
func (t *jobTable) byIndex(n int) *trackedJob {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n < 1 || n > len(t.jobs) {
		return nil
	}
	return t.jobs[n-1]
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

// waitForLaunch polls briefly for a background launch to register.
// `cmd &` continues the main shell immediately while the background
// goroutine starts (and tracks) the process; without this, `kill $!`
// right after `&` would race the registration.
func (t *jobTable) waitForLaunch() {
	for i := 0; i < 50; i++ {
		t.mu.Lock()
		n := len(t.jobs)
		t.mu.Unlock()
		if n > 0 {
			return
		}
		select {
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// resolveJobSpec resolves kill/wait/jobs operands to a tracked job:
// fake ids ("g1"), real pids ("1234"), and jobspecs ("%1", "%%", "%+",
// "%-", "%name", "%?substr").
// waitForFresh polls until the newest job is alive (or 500ms pass).
// Covers `cmd & kill %%` where the new process hasn't registered yet
// while the previous newest is already done.
func (t *jobTable) waitForFresh() {
	for i := 0; i < 50; i++ {
		t.mu.Lock()
		var newest *trackedJob
		if len(t.jobs) > 0 {
			newest = t.jobs[len(t.jobs)-1]
		}
		t.mu.Unlock()
		if newest != nil && (newest.alive() || time.Since(newest.started) > time.Second) {
			return
		}
		select {
		case <-time.After(10 * time.Millisecond):
		}
	}
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

// trackExec is an exec middleware kept for chain completeness.
// NOTE: mvdan/sh resolves builtins (jobs, wait, kill, ...) BEFORE the
// exec-handler chain, so job-control interception lives in callOverride.
// External commands are tracked in extraHandler.runExternalTracked.
func trackExec(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		return next(ctx, args)
	}
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

// cmdJobs implements `jobs [-l]`: list tracked background processes.
func cmdJobs(hc interp.HandlerContext, args []string) error {
	globalJobs.waitForLaunch()
	globalJobs.sweep(0)
	long := false
	for _, a := range args[1:] {
		if a == "-l" {
			long = true
		}
	}
	globalJobs.mu.Lock()
	defer globalJobs.mu.Unlock()
	for i, j := range globalJobs.jobs {
		state := "Done"
		select {
		case <-j.done:
		default:
			state = "Running"
		}
		if long {
			fmt.Fprintf(hc.Stdout, "[%d] %d %s %s\n", i+1, j.pid, state, strings.Join(j.argv, " "))
		} else {
			fmt.Fprintf(hc.Stdout, "[%d] %s %s\n", i+1, state, strings.Join(j.argv, " "))
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
	hc := interp.HandlerCtx(ctx)
	if len(args) == 1 {
		// `wait` with no args: mvdan/sh already waits for background
		// goroutines; additionally wait for tracked externals.
		globalJobs.mu.Lock()
		jobs := append([]*trackedJob(nil), globalJobs.jobs...)
		globalJobs.mu.Unlock()
		for _, j := range jobs {
			select {
			case <-j.done:
			case <-ctx.Done():
				fmt.Fprintln(hc.Stderr, "wait:", ctx.Err())
				return 1, true
			}
		}
		globalJobs.sweep(0)
		return 0, true
	}
	code := 0
	for _, a := range args[1:] {
		if a == "-n" || a == "-p" {
			fmt.Fprintf(hc.Stderr, "wait: unsupported option %q\n", a)
			return 2, true
		}
		j := globalJobs.resolveJobSpec(a)
		if j == nil {
			// Not a tracked external: leave to mvdan/sh's builtin
			// (in-process jobs like `sleep &`, real pids, errors).
			return 0, false
		}
		select {
		case <-j.done:
			if j.code != 0 {
				code = j.code
			}
		case <-ctx.Done():
			fmt.Fprintln(hc.Stderr, "wait:", ctx.Err())
			return 1, true
		}
	}
	globalJobs.sweep(0)
	return code, true
}
