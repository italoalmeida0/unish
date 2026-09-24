package main

// Zombie-process regression tests (written BEFORE the fix, TDD style).
// Each test fails on the unfixed code and passes after:
//
//   - Z1 job table grows forever (track() never prunes).
//   - Z2 `timeout D builtin` leaks the runner goroutine when the
//     builtin ignores ctx (select{} forever).
//   - Z3 `<(...)` producer/consumer handshake blocks forever when one
//     side errors early (pure <-chan, no ctx select).
//   - Z4 netcat pipeConn waits for only ONE copier; the other can block
//     forever and the conn is never closed on the done path.
//
// Conventions: hermetic, no real network (net.Pipe / 127.0.0.1 only),
// bounded time.

import (
	"context"
	"io"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func testGoroutines() int { return runtime.NumGoroutine() }

func testWaitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	dead := time.Now().Add(timeout)
	for time.Now().Before(dead) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// probeHC runs a probe command and captures its HandlerContext.
func probeHC(t *testing.T, stdin io.Reader, stdout, stderr io.Writer) interp.HandlerContext {
	t.Helper()
	var hc interp.HandlerContext
	r, err := interp.New(
		interp.StdIO(stdin, stdout, stderr),
		interp.ExecHandlers(func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
			return func(ctx context.Context, args []string) error {
				if len(args) > 0 && args[0] == "__probe_hc__" {
					hc = interp.HandlerCtx(ctx)
					return nil
				}
				return next(ctx, args)
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	prog, err := syntax.NewParser().Parse(strings.NewReader("__probe_hc__"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background(), prog); err != nil {
		t.Fatal(err)
	}
	return hc
}

// --- Z1: job table must not grow forever --------------------------------

func TestZombieJobTablePruned(t *testing.T) {
	globalJobs.mu.Lock()
	before := len(globalJobs.jobs)
	globalJobs.mu.Unlock()
	for i := 0; i < 5; i++ {
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd", "/c", "exit 0")
		} else {
			cmd = exec.Command("true")
		}
		if err := cmd.Start(); err != nil {
			t.Skipf("cannot spawn probe process: %v", err)
		}
		j := globalJobs.track(cmd, []string{"z1probe"})
		testWaitFor(t, "job done", 10*time.Second, func() bool {
			select {
			case <-j.done:
				return true
			default:
				return false
			}
		})
	}
	time.Sleep(300 * time.Millisecond)
	globalJobs.mu.Lock()
	after := len(globalJobs.jobs)
	globalJobs.mu.Unlock()
	if after-before >= 5 {
		t.Fatalf("Z1: job table grew by %d (before=%d after=%d): finished jobs are never reaped",
			after-before, before, after)
	}
}

type countWriter struct{ n *int64 }

func (w countWriter) Write(p []byte) (int, error) {
	atomic.AddInt64(w.n, int64(len(p)))
	return len(p), nil
}

// --- Z2: timeout + stuck builtin must not leak the goroutine ------------

func TestZombieTimeoutBuiltinNoLeak(t *testing.T) {
	// A builtin that ignores ctx entirely (like sort/cat blocked in
	// ReadAll on infinite input): `timeout` must still return AND the
	// runner goroutine must be reaped (no writes after return).
	var n int64
	hc := probeHC(t, nil, countWriter{&n}, io.Discard)
	released := make(chan struct{})
	stuck := func(ctx context.Context, hc interp.HandlerContext, args []string) error {
		defer close(released)
		_ = ctx // TRULY stuck: never observes ctx, like sort blocked
		// in ReadAll or yes in a write loop without ctx checks.
		for {
			if _, err := hc.Stdout.Write([]byte("x")); err != nil {
				return nil // write error (EPIPE after fix) reaps us
			}
		}
	}
	extraCommands = append(extraCommands, extraCmd{"__z2_stuck__", stuck})
	defer func() { extraCommands = extraCommands[:len(extraCommands)-1] }()
	err := cmdTimeout(context.Background(), hc, []string{"timeout", "0.2", "__z2_stuck__"})
	if err == nil {
		t.Fatalf("Z2: expected timeout exit, got nil")
	}
	// Unfixed code: select returns 124 but the stuck builtin keeps
	// writing forever (released never closes, bytes grow).
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatalf("Z2: stuck builtin goroutine never reaped after timeout (still writing)")
	}
	time.Sleep(200 * time.Millisecond)
	mark := atomic.LoadInt64(&n)
	time.Sleep(400 * time.Millisecond)
	if after := atomic.LoadInt64(&n); after != mark {
		t.Fatalf("Z2: stuck builtin still writing after reap (bytes %d -> %d)", mark, after)
	}
}

// --- Z3: procsubst must not deadlock when producer fails early ----------

func TestZombieProcSubstEarlyError(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	out, _, _ := runParityScript(t, dir, "cat <(exit 3); echo RC=$?")
	el := time.Since(start)
	if el > 10*time.Second {
		t.Fatalf("Z3: procsubst early-error took %v (deadlock until ctx timeout), out=%q", el, out)
	}
	if !strings.Contains(out, "RC=") {
		t.Fatalf("Z3: expected RC= line, got %q", out)
	}
	// Unit form: OpenConsumer on a CmdIn substitution whose producer
	// errored before finishProducer must fail fast, not hang forever.
	if runtime.GOOS == "windows" {
		psf, err := procSubstHandler(context.Background(), syntax.CmdIn)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			// Simulate producer death: close producerDone WITHOUT
			// success is impossible via public API on unfixed code;
			// instead just attempt OpenConsumer with a cancelled ctx:
			// after the fix it must respect ctx and return.
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, oerr := psf.OpenConsumer(cctx, 0)
			done <- oerr
		}()
		select {
		case oerr := <-done:
			if oerr == nil {
				// Producer actually ran? No producer here, so nil
				// would mean it opened a stale temp file: also bad.
				t.Fatalf("Z3: OpenConsumer succeeded with no producer (stale temp reuse)")
			}
		case <-time.After(8 * time.Second):
			t.Fatalf("Z3: OpenConsumer ignored ctx cancel (deadlock: pure <-chan with _ = ctx)")
		}
		_ = psf.Cleanup()
	}
}

// --- Z4: netcat pipeConn must not wait forever --------------------------

func TestZombieNetcatPipeConnBounds(t *testing.T) {
	// One direction EOFs, the other blocks forever (half-open socket):
	// pipeConn must return AND reap the stuck copier (close the conn).
	// A blocking conn whose Read never returns and whose Close
	// unblocks pending Reads models a half-open TCP stream; net.Pipe
	// can't model this (Close unblocks both ends at once).
	hc := probeHC(t, strings.NewReader(""), io.Discard, io.Discard)
	conn := &halfOpenConn{readBlock: make(chan struct{}), closed: make(chan struct{})}
	before := testGoroutines()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pipeConn(context.Background(), hc, conn)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Z4: pipeConn did not return after stdin EOF (waits for a copier forever)")
	}
	// pipeConn returned: the conn must be closed (unblocking the stuck
	// Read) and no copier goroutine may survive. Unfixed code returns
	// on the first done but never closes the conn: the second copier
	// blocks in Read forever.
	select {
	case <-conn.closed:
	case <-time.After(3 * time.Second):
		t.Fatalf("Z4: pipeConn returned without closing conn (stuck copier leaks)")
	}
	testWaitFor(t, "pipeConn copiers to drain", 4*time.Second, func() bool {
		return testGoroutines() <= before
	})
}

// halfOpenConn: Write discards, Read blocks until Close, Close is
// idempotent and observable via closed.
type halfOpenConn struct {
	readBlock chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *halfOpenConn) Read(b []byte) (int, error) {
	select {
	case <-c.readBlock:
		return 0, io.EOF
	case <-c.closed:
		return 0, io.EOF
	}
}

func (c *halfOpenConn) Write(b []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, io.ErrClosedPipe
	default:
		return len(b), nil
	}
}

func (c *halfOpenConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *halfOpenConn) LocalAddr() net.Addr                { return dummyAddr{} }
func (c *halfOpenConn) RemoteAddr() net.Addr               { return dummyAddr{} }
func (c *halfOpenConn) SetDeadline(t time.Time) error      { return nil }
func (c *halfOpenConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *halfOpenConn) SetWriteDeadline(t time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tcp" }
func (dummyAddr) String() string  { return "halfopen" }
