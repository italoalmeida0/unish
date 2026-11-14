package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func perfTestNewRunner(tb testing.TB, stdout, stderr io.Writer) *interp.Runner {
	tb.Helper()
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	r, err := interp.New(
		interp.StdIO(nil, stdout, stderr),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(
			trackExec,
			extraHandler,
		),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		tb.Fatalf("interp.New: %v", err)
	}
	return r
}

func perfTestRunScript(tb testing.TB, src string) (stdout, stderr string, code int) {
	tb.Helper()
	prog, err := syntax.NewParser().Parse(strings.NewReader(src), "test")
	if err != nil {
		tb.Fatalf("parse %q: %v", src, err)
	}
	var outBuf, errBuf bytes.Buffer
	r := perfTestNewRunner(tb, &outBuf, &errBuf)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = r.Run(ctx, prog)
	if err != nil {
		var es interp.ExitStatus
		if errors.As(err, &es) {
			code = int(es)
		} else {
			tb.Fatalf("run %q unexpected fatal err: %v", src, err)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

func TestMemPipeBasicAndWrapAround(t *testing.T) {
	pr, pw := newMemPipe()
	const totalBytes = 2*1024*1024 + 3719 // >32 full 64 KB ring wrap-arounds
	want := make([]byte, totalBytes)
	for i := range want {
		want[i] = byte((i*131 + 17) & 0xff)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		off := 0
		step := 0
		sizes := []int{1, 137, 4099, 17389, 65536, 70001}
		for off < len(want) {
			n := sizes[step%len(sizes)]
			step++
			if off+n > len(want) {
				n = len(want) - off
			}
			if step%3 == 0 {
				nw, err := pw.WriteString(string(want[off : off+n]))
				if err != nil || nw != n {
					t.Errorf("WriteString(%d): nw=%d err=%v", n, nw, err)
					return
				}
			} else {
				nw, err := pw.Write(want[off : off+n])
				if err != nil || nw != n {
					t.Errorf("Write(%d): nw=%d err=%v", n, nw, err)
					return
				}
			}
			off += n
		}
		if err := pw.Close(); err != nil {
			t.Errorf("pw.Close: %v", err)
		}
	}()

	got := make([]byte, 0, totalBytes)
	readSizes := []int{1, 503, 8191, 32771}
	step := 0
	tmp := make([]byte, 32771)
	for {
		sz := readSizes[step%len(readSizes)]
		step++
		n, err := pr.Read(tmp[:sz])
		if n > 0 {
			got = append(got, tmp[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("pr.Read: %v", err)
		}
	}
	if err := pr.Close(); err != nil {
		t.Fatalf("pr.Close: %v", err)
	}
	wg.Wait()

	if !bytes.Equal(got, want) {
		t.Fatalf("stream mismatch: got %d bytes, want %d bytes", len(got), len(want))
	}
}

func TestMemPipeEarlyReaderCloseBrokenPipe(t *testing.T) {
	pr, pw := newMemPipe()
	errCh := make(chan error, 1)
	go func() {
		chunk := bytes.Repeat([]byte("abcdefgh"), 16*1024) // 128 KB > 64 KB buffer
		_, err := pw.Write(chunk)
		errCh <- err
	}()

	var small [5]byte
	n, err := pr.Read(small[:])
	if err != nil || string(small[:n]) != "abcde" {
		t.Fatalf("initial Read = (%d, %v), %q", n, err, small[:n])
	}
	if err := pr.Close(); err != nil {
		t.Fatalf("pr.Close: %v", err)
	}

	select {
	case werr := <-errCh:
		if !errors.Is(werr, io.ErrClosedPipe) {
			t.Fatalf("blocked Write after reader Close = %v, want io.ErrClosedPipe", werr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Write did not wake on reader Close")
	}

	if _, err := pw.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("subsequent Write = %v, want io.ErrClosedPipe", err)
	}
	if _, err := pw.WriteString("x"); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("subsequent WriteString = %v, want io.ErrClosedPipe", err)
	}
	_ = pw.Close()
}

func TestMemPipeReadDeadlineAndConcurrentClose(t *testing.T) {
	pr, pw := newMemPipe()
	readErr := make(chan error, 1)
	go func() {
		var buf [16]byte
		_, err := pr.Read(buf[:])
		readErr <- err
	}()

	time.Sleep(10 * time.Millisecond)
	if err := pr.SetReadDeadline(time.Now()); err != nil {
		t.Fatalf("SetReadDeadline(now): %v", err)
	}
	select {
	case err := <-readErr:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Read after immediate deadline = %v, want ErrDeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not wake on immediate deadline")
	}

	// Clear deadline and verify normal delivery still works.
	if err := pr.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear deadline: %v", err)
	}
	if _, err := pw.WriteString("ok"); err != nil {
		t.Fatalf("WriteString after deadline clear: %v", err)
	}
	var out [8]byte
	n, err := pr.Read(out[:])
	if err != nil || string(out[:n]) != "ok" {
		t.Fatalf("Read after deadline clear = (%d, %v, %q)", n, err, out[:n])
	}

	// Future timer deadline.
	if err := pr.SetReadDeadline(time.Now().Add(15 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline(future): %v", err)
	}
	_, err = pr.Read(out[:])
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read after timer deadline = %v, want ErrDeadlineExceeded", err)
	}
	_ = pr.Close()
	_ = pw.Close()

	// Concurrent close stress test.
	for iter := 0; iter < 32; iter++ {
		r, w := newMemPipe()
		var wg sync.WaitGroup
		wg.Add(4)
		go func() {
			defer wg.Done()
			buf := make([]byte, 4096)
			for i := 0; i < 64; i++ {
				if _, err := w.Write(buf); err != nil {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			buf := make([]byte, 1024)
			for i := 0; i < 64; i++ {
				if _, err := r.Read(buf); err != nil {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			_ = r.SetReadDeadline(time.Now().Add(time.Millisecond))
			_ = r.Close()
		}()
		go func() {
			defer wg.Done()
			_ = w.Close()
		}()
		wg.Wait()
	}
}

func TestPerfShellYesHeadBrokenPipeAndPipelines(t *testing.T) {
	start := time.Now()
	out, stderr, code := perfTestRunScript(t, "yes | head -n 1")
	if code != 0 || out != "y\n" || stderr != "" {
		t.Fatalf("yes | head -n 1 => code=%d out=%q stderr=%q", code, out, stderr)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("yes | head -n 1 took too long: %v", elapsed)
	}

	out, stderr, code = perfTestRunScript(t, "seq 1 50000 | head -n 3")
	if code != 0 || out != "1\n2\n3\n" || stderr != "" {
		t.Fatalf("seq | head => code=%d out=%q stderr=%q", code, out, stderr)
	}

	out, stderr, code = perfTestRunScript(t, `
		(exit 3) | (exit 0) | (exit 7)
		saved="${PIPESTATUS[*]}"
		echo "ps=$saved"
		echo "after=${PIPESTATUS[*]}"
		(exit 4) | (exit 6)
		(echo "sub=${PIPESTATUS[*]}")
		false
		echo "false=${PIPESTATUS[*]}"
		(exit 2) | (exit 5)
		echo "two=${PIPESTATUS[*]}"
	`)
	want := "ps=3 0 7\nafter=0\nsub=4 6\nfalse=1\ntwo=2 5\n"
	if code != 0 || out != want || stderr != "" {
		t.Fatalf("PIPESTATUS script:\ngot  code=%d out=%q stderr=%q\nwant out=%q", code, out, stderr, want)
	}
}

func TestPerfShellExternalStdioWithMemPipeAndDevNull(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil || goBin == "" {
		t.Skip("go binary not in PATH")
	}
	// 1. External command writing to virtual /dev/null.
	out, stderr, code := perfTestRunScript(t, "go version > /dev/null")
	if code != 0 || out != "" || stderr != "" {
		t.Fatalf("go version > /dev/null => code=%d out=%q stderr=%q", code, out, stderr)
	}

	// 2. Slow producer piped into an external command that exits immediately
	// without reading stdin (must NOT hang cmd.Wait!).
	done := make(chan struct{})
	go func() {
		defer close(done)
		o, e, c := perfTestRunScript(t, "(sleep 0.05; echo trailing) | go version > /dev/null")
		if c != 0 || o != "" || e != "" {
			t.Errorf("pipeline into go version => code=%d out=%q stderr=%q", c, o, e)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("external command with unread memPipe stdin hung")
	}
}

func TestPerfShellRunTestFastPathParity(t *testing.T) {
	cases := [][]string{
		{},
		{""},
		{"nonempty"},
		{"!"},
		{"("},
		{")"},
		{"-n", ""},
		{"-n", "x"},
		{"-z", ""},
		{"-z", "x"},
		{"!", ""},
		{"!", "x"},
		{"!", "!"},
		{"!", "("},
		{"!", ")"},
		{"a", "=", "a"},
		{"a", "=", "b"},
		{"a", "==", "a"},
		{"a", "!=", "b"},
		{"a", "!=", "a"},
		{"!", "=", "!"},
		{"(", "=", "("},
		{")", "=", ")"},
		{"0", "-eq", "0"},
		{"-42", "-lt", "5"},
		{"+15", "-ge", "15"},
		{" 7 ", "-eq", "7"},
		{"010", "-eq", "8"},
		{"0x10", "-eq", "16"},
		{"9223372036854775807", "-gt", "0"},
		{"bad", "-eq", "0"},
		{"0", "-eq", "bad"},
		{"!", "5", "-gt", "3"},
		{"!", "3", "-gt", "5"},
		{"!", "!", "a", "="},
		{"!", "-a", ""},
		{"a", "=", "a", "-a", "b", "=", "b"},
		{"a", "=", "b", "-o", "c", "=", "c"},
		{"(", "a", "=", "a", ")"},
	}

	slowEval := func(hc interp.HandlerContext, cmdName string, inner []string) error {
		if len(inner) == 0 {
			return errTestFalse
		}
		tp := testParser{hc: hc, toks: inner}
		ok, err := tp.orExpr()
		if err != nil {
			return errTestUsage
		}
		if tp.pos != len(tp.toks) {
			return errTestUsage
		}
		if !ok {
			return errTestFalse
		}
		return nil
	}

	for _, toks := range cases {
		var errFast, errSlow bytes.Buffer
		hcFast := interp.HandlerContext{Stderr: &errFast}
		hcSlow := interp.HandlerContext{Stderr: &errSlow}

		args := append([]string{"["}, toks...)
		args = append(args, "]")
		got := runTest(hcFast, args)
		want := slowEval(hcSlow, "[", toks)
		if got != want {
			t.Fatalf("runTest(%q) = %v, slow testParser = %v", args, got, want)
		}
	}
}

func TestPerfShellCallOverrideSinglePassAndSelfSignal(t *testing.T) {
	out, stderr, code := perfTestRunScript(t, `
		[ 2 -lt 5 ] &
		wait $!
		echo "bg=$?"
		trap 'got=trapped' INT
		kill -INT $$
		echo "sig=$got"
	`)
	if code != 0 || out != "bg=0\nsig=trapped\n" || stderr != "" {
		t.Fatalf("bg + self-signal => code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestPerfShellExtraRegistryAndZeroAllocHelpers(t *testing.T) {
	origLen := len(extraCommands)
	t.Cleanup(func() {
		extraCommands = extraCommands[:origLen]
		extraRegistryMu.Lock()
		rebuildExtraRegistryLocked()
		extraRegistryMu.Unlock()
	})

	if lookupExtra("cat") == nil || lookupExtra("grep") == nil {
		t.Fatal("lookupExtra missed built-in commands")
	}
	if lookupExtra("__nonexistent_perf_cmd__") != nil {
		t.Fatal("lookupExtra matched nonexistent command")
	}

	// Test registerExtra helper.
	registerExtra("__perf_custom__", func(_ context.Context, hc interp.HandlerContext, args []string) error {
		_, _ = io.WriteString(hc.Stdout, "custom:"+strings.Join(args[1:], ","))
		return nil
	})
	out, stderr, code := perfTestRunScript(t, "__perf_custom__ alpha beta")
	if code != 0 || out != "custom:alpha,beta" || stderr != "" {
		t.Fatalf("registerExtra command => code=%d out=%q stderr=%q", code, out, stderr)
	}

	// Test direct slice append compatibility.
	extraCommands = append(extraCommands, extraCmd{
		name: "__perf_appended__",
		main: func(_ context.Context, hc interp.HandlerContext, _ []string) error {
			_, _ = io.WriteString(hc.Stdout, "appended")
			return nil
		},
	})
	if lookupExtra("__perf_appended__") == nil {
		t.Fatal("lookupExtra did not rebuild after direct extraCommands append")
	}

	// Verify splitAttached equivalence on tricky flag permutations.
	splitCases := []struct {
		cmd  string
		args []string
		want []string
	}{
		{"grep", []string{"grep", "-c", "foo", "big.txt"}, []string{"grep", "-c", "foo", "big.txt"}},
		{"grep", []string{"grep", "-rn", "needle", "dir"}, []string{"grep", "-r", "-n", "needle", "dir"}},
		{"cut", []string{"cut", "-d,", "-f2", "a.csv"}, []string{"cut", "-d", ",", "-f", "2", "a.csv"}},
		{"grep", []string{"grep", "--count", "pat", "f"}, []string{"grep", "-c", "pat", "f"}},
		{"head", []string{"head", "file.txt", "-n", "-1"}, []string{"head", "-n", "-1", "file.txt"}},
	}
	for _, tc := range splitCases {
		got := splitAttached(tc.cmd, tc.args)
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("splitAttached(%q, %q) = %q, want %q", tc.cmd, tc.args, got, tc.want)
		}
	}

	// Zero-allocation assertions on hot paths.
	plainArgs := []string{"grep", "-c", "foo", "big.txt"}
	if allocs := testing.AllocsPerRun(100, func() {
		_ = hasVersionFlag(plainArgs)
		_ = splitAttached("grep", plainArgs)
		_ = lookupExtra("grep")
	}); allocs != 0 {
		t.Fatalf("hot arg/registry helpers allocated %.1f times/op, want 0", allocs)
	}

	testArgs := []string{"[", "42", "-lt", "10000", "]"}
	hc := interp.HandlerContext{Stdout: sharedDevNull, Stderr: io.Discard}
	if allocs := testing.AllocsPerRun(100, func() {
		_ = runTest(hc, testArgs)
	}); allocs != 0 {
		t.Fatalf("runTest fast path allocated %.1f times/op, want 0", allocs)
	}

	printfArgs := []string{"printf", "%s", "hello"}
	if allocs := testing.AllocsPerRun(100, func() {
		_ = cmdPrintf(context.Background(), hc, printfArgs)
	}); allocs != 0 {
		t.Fatalf("cmdPrintf %%s fast path allocated %.1f times/op, want 0", allocs)
	}
}

func TestPerfShellPrintfAndExpansionFastPaths(t *testing.T) {
	// Ensure `--` does not mutate caller slice in-place.
	callerSlice := []string{"printf", "--", "%s\n", "hi"}
	var outBuf bytes.Buffer
	if err := cmdPrintf(context.Background(), interp.HandlerContext{Stdout: &outBuf, Stderr: io.Discard}, callerSlice); err != nil {
		t.Fatalf("cmdPrintf --: %v", err)
	}
	if callerSlice[1] != "--" || outBuf.String() != "hi\n" {
		t.Fatalf("cmdPrintf mutated caller slice or wrong output: slice=%q out=%q", callerSlice, outBuf.String())
	}

	out, stderr, code := perfTestRunScript(t, `
		printf '%s' a b c
		printf '\n'
		printf '%s\n' x y
		printf '%d %d %d\n' 0 -42 16384
		printf '%d\n' 010
		printf '%d\n' 0x10
		u=abcdef
		echo "${u#*d}|${u##*b}|${u%d*}|${u%%b*}|${u/ef/EF}|${#u}"
		v=$(printf 'hello\n\n')
		echo "cs=$v"
	`)
	want := "abc\nx\ny\n0 -42 16384\n8\n16\nef|cdef|abc|a|abcdEF|6\ncs=hello\n"
	if code != 0 || out != want || stderr != "" {
		t.Fatalf("printf/expansion parity:\ngot  code=%d out=%q stderr=%q\nwant out=%q", code, out, stderr, want)
	}

	// Invalid numeric operand must still emit diagnostic and exit 1.
	out, stderr, code = perfTestRunScript(t, `printf '%d\n' 12 nope`)
	if code != 1 || out != "12\n0\n" || stderr == "" {
		t.Fatalf("printf invalid int => code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func BenchmarkPerfLoopTest(b *testing.B) {
	prog, _ := syntax.NewParser().Parse(strings.NewReader(`i=0; while [ $i -lt 200 ]; do :; i=$((i+1)); done`), "bench")
	r := perfTestNewRunner(b, io.Discard, io.Discard)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.Run(ctx, prog); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPerfEchoDevNull(b *testing.B) {
	prog, _ := syntax.NewParser().Parse(strings.NewReader(`i=0; while [ $i -lt 200 ]; do echo x > /dev/null; i=$((i+1)); done`), "bench")
	r := perfTestNewRunner(b, io.Discard, io.Discard)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.Run(ctx, prog); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPerfPrintfDevNull(b *testing.B) {
	prog, _ := syntax.NewParser().Parse(strings.NewReader(`i=0; while [ $i -lt 200 ]; do printf %s x > /dev/null; i=$((i+1)); done`), "bench")
	r := perfTestNewRunner(b, io.Discard, io.Discard)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.Run(ctx, prog); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPerfVarExp(b *testing.B) {
	prog, _ := syntax.NewParser().Parse(strings.NewReader(`u=abcdef; i=0; while [ $i -lt 200 ]; do x=${u#*d}; y=${u%%b*}; z=${u/ef/EF}; i=$((i+1)); done`), "bench")
	r := perfTestNewRunner(b, io.Discard, io.Discard)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.Run(ctx, prog); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPerfCmdSubst(b *testing.B) {
	prog, _ := syntax.NewParser().Parse(strings.NewReader(`i=0; while [ $i -lt 50 ]; do x=$(echo hi); i=$((i+1)); done`), "bench")
	r := perfTestNewRunner(b, io.Discard, io.Discard)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.Run(ctx, prog); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPerfMemPipeThroughput(b *testing.B) {
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 2048) // 32 KB
	readBuf := make([]byte, 32*1024)
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	b.ResetTimer()
	pr, pw := newMemPipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, err := pr.Read(readBuf)
			if err != nil {
				return
			}
		}
	}()
	for i := 0; i < b.N; i++ {
		if _, err := pw.Write(chunk); err != nil {
			b.Fatal(err)
		}
	}
	_ = pw.Close()
	<-done
	_ = pr.Close()
}
