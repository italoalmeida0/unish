package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// runTimedScript executes src with a strict context deadline and fails fast if
// a builtin ignores ctx.Done() / downstream pipe closure and hangs.
func runTimedScript(t *testing.T, dir string, timeout time.Duration, src string) (string, string, error) {
	t.Helper()
	prog, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	var stdout, stderr bytes.Buffer
	r, err := interp.New(
		interp.Dir(dir),
		interp.StdIO(nil, &stdout, &stderr),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		t.Fatalf("interpreter creation error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	type res struct {
		out string
		err string
		rc  error
	}
	ch := make(chan res, 1)
	go func() {
		runErr := r.Run(ctx, prog)
		ch <- res{out: stdout.String(), err: stderr.String(), rc: runErr}
	}()

	select {
	case got := <-ch:
		return got.out, got.err, got.rc
	case <-time.After(timeout + 750*time.Millisecond):
		t.Fatalf("script hung past %v timeout (ignored ctx/pipe close): %s", timeout, src)
		return "", "", nil
	}
}

func TestBugfixTestBuiltinCompoundAndPrecedence(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "f1.txt"), "hello")
	mustWrite(t, filepath.Join(dir, "f2.txt"), "world")

	cases := []struct {
		expr string
		want int
	}{
		{`[ -z "" -a -z "" ]`, 0},
		{`[ -n "abc" -a -z "" ]`, 0},
		{`[ -n "abc" -a -z "xyz" ]`, 1},
		{`[ -n "" -o -n "" ]`, 1},
		{`[ -n "" -o -z "" ]`, 0},
		{`[ -f f1.txt -a -f f2.txt ]`, 0},
		{`[ -f f1.txt -a -f missing.txt ]`, 1},
		{`[ ! -z "abc" -a ! -n "" ]`, 0},
		{`[ "foo" -a "bar" ]`, 0},
		{`[ "foo" -a "" ]`, 1},
		{`[ "" -o "bar" ]`, 0},
		{`[ "" -o "" ]`, 1},
		{`[ ! ]`, 0},
		{`[ ! = ! ]`, 0},
		{`[ ! != ! ]`, 1},
		{`[ -z "=" -a -z "" ]`, 1},
		{`[ -z "" -a -z "" ]`, 0},
		{`[ "(" = "(" ]`, 0},
		{`[ "(" != ")" ]`, 0},
		{`[ "(" -z "" -a -n "x" ")" -o -n "" ]`, 0},
		{`test -z "" -a -n "ok"`, 0},
	}

	for _, tc := range cases {
		_, errOut, err := runParityScript(t, dir, tc.expr)
		if got := exitCode(err); got != tc.want {
			t.Errorf("%s: exit=%d, want %d (stderr=%q)", tc.expr, got, tc.want, errOut)
		}
	}
}

func TestBugfixLsColorNoStateLeakOrRace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "plain.txt"), "hi\n")

	// Sequential invocations: --color=always must not leak into subsequent `ls`.
	out1, _, err := runParityScript(t, dir, "ls --color=always\n")
	if err != nil {
		t.Fatalf("ls --color=always failed: %v", err)
	}
	if !strings.Contains(out1, "\x1b[") {
		t.Fatalf("expected ANSI escape in --color=always output, got %q", out1)
	}

	out2, _, err := runParityScript(t, dir, "ls\n")
	if err != nil {
		t.Fatalf("plain ls failed: %v", err)
	}
	if strings.Contains(out2, "\x1b[") {
		t.Fatalf("lsColorMode leaked into plain ls output: %q", out2)
	}

	// Concurrent invocations must not race or cross-contaminate color output.
	var wg sync.WaitGroup
	errCh := make(chan string, 40)
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			o, _, e := runParityScript(t, dir, "ls --color=always\n")
			if e != nil || !strings.Contains(o, "\x1b[") {
				errCh <- fmt.Sprintf("color=always failed: err=%v out=%q", e, o)
			}
		}()
		go func() {
			defer wg.Done()
			o, _, e := runParityScript(t, dir, "ls --color=never\n")
			if e != nil || strings.Contains(o, "\x1b[") {
				errCh <- fmt.Sprintf("color=never contaminated: err=%v out=%q", e, o)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Error(msg)
	}
}

func TestBugfixSplitAttachedLongBoolsAndParseN(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "cols.txt"), "a:b:c\n")
	mustWrite(t, filepath.Join(dir, "spaces.txt"), "    hello    world\n")
	mustWrite(t, filepath.Join(dir, "-2"), "line1\nline2\nline3\n")

	out, errOut, err := runParityScript(t, dir, "cut -d: -f 2 --complement cols.txt")
	if exitCode(err) != 0 || out != "a:c\n" {
		t.Errorf("cut --complement: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runParityScript(t, dir, "unexpand -t 4 --first-only spaces.txt")
	if exitCode(err) != 0 || out != "\thello    world\n" {
		t.Errorf("unexpand --first-only: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runParityScript(t, dir, "md5sum -b --tag cols.txt")
	if exitCode(err) != 0 || errOut != "" || !strings.Contains(out, "cols.txt") {
		t.Errorf("md5sum --tag: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// File named "-2" after "--" must not be consumed as "-n 2" by parseN.
	out, errOut, err = runParityScript(t, dir, "head -- -2")
	if exitCode(err) != 0 || out != "line1\nline2\nline3\n" {
		t.Errorf("head -- -2: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}
}

func TestBugfixVersionFlagNotInterceptingOperands(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "ver.txt"), "alpha\n--version\nbeta\n")
	mustWrite(t, filepath.Join(dir, "--version"), "inside-version-file\n")

	out, errOut, err := runParityScript(t, dir, "grep -- --version ver.txt")
	if exitCode(err) != 0 || out != "--version\n" {
		t.Errorf("grep -- --version: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runParityScript(t, dir, "grep -e --version ver.txt")
	if exitCode(err) != 0 || out != "--version\n" {
		t.Errorf("grep -e --version: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runParityScript(t, dir, "cat -- --version")
	if exitCode(err) != 0 || out != "inside-version-file\n" {
		t.Errorf("cat -- --version: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// Direct --version flag on coreutils must still work.
	out, errOut, err = runParityScript(t, dir, "uname --version")
	if exitCode(err) != 0 || !strings.Contains(out, "uname (unish coreutils)") {
		t.Errorf("uname --version: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}
}

func TestBugfixCatBrokenPipeAndCtxCancel(t *testing.T) {
	dir := t.TempDir()
	var big strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&big, "line-%d-padding-to-exceed-os-pipe-buffer-size-easily\n", i)
	}
	mustWrite(t, filepath.Join(dir, "big.txt"), big.String())

	out, errOut, err := runTimedScript(t, dir, 2*time.Second, "cat big.txt | head -n 1")
	if exitCode(err) != 0 || errOut != "" || !strings.HasPrefix(out, "line-1-") {
		t.Errorf("cat big.txt | head -n 1: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runTimedScript(t, dir, 2*time.Second, "yes | cat -n | head -n 2")
	if exitCode(err) != 0 || errOut != "" || out != "     1\ty\n     2\ty\n" {
		t.Errorf("yes | cat -n | head -n 2: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}
}

func TestBugfixPrintfStarWidthAndNegativePrecision(t *testing.T) {
	dir := t.TempDir()

	// Must not panic on negative '*' precision for %s.
	out, errOut, err := runParityScript(t, dir, `printf '%.*s\n' -3 hello`)
	if exitCode(err) != 0 || out != "hello\n" {
		t.Errorf("printf negative .*s: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// Negative '*' width means '-' flag + abs(width).
	out, errOut, err = runParityScript(t, dir, `printf '[%*s][%-*s][%*d]\n' -5 hi -5 hi -5 42`)
	if exitCode(err) != 0 || out != "[hi   ][hi   ][42   ]\n" {
		t.Errorf("printf negative * width: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// Negative '*' precision on floats means precision omitted (default 6).
	out, errOut, err = runParityScript(t, dir, `printf '%.*f|%.*g\n' -2 3.5 -2 3.14`)
	if exitCode(err) != 0 || out != "3.500000|3.14\n" {
		t.Errorf("printf negative .*f/.*g: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// Invalid numeric width argument emits warning, exits 1, treats width as 0.
	out, errOut, err = runParityScript(t, dir, `printf '[%*s]\n' bad hi`)
	if exitCode(err) != 1 || out != "[hi]\n" || errOut == "" {
		t.Errorf("printf invalid * width: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// Preserve bare-dot '%0.sx' parity behavior (parity/edge_cases.py).
	out, errOut, err = runParityScript(t, dir, `printf '%0.sx' 1 2 3`)
	if exitCode(err) != 0 || out != "1x2x3x" {
		t.Errorf("printf %%0.sx: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}
}

func TestBugfixSedStreamingPipeCancelAndMultiFileLastLine(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "f1.txt"), "a1\na2\n")
	mustWrite(t, filepath.Join(dir, "f2.txt"), "b1\nb2\n")

	out, errOut, err := runTimedScript(t, dir, 2*time.Second, "yes | sed 1q")
	if exitCode(err) != 0 || out != "y\n" {
		t.Errorf("yes | sed 1q: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runTimedScript(t, dir, 2*time.Second, "yes | sed 's/y/z/' | head -n 2")
	if exitCode(err) != 0 || errOut != "" || out != "z\nz\n" {
		t.Errorf("yes | sed | head: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runTimedScript(t, dir, 2*time.Second, "echo x | sed ':a; p; b a' | head -n 3")
	if exitCode(err) != 0 || out != "x\nx\nx\n" {
		t.Errorf("sed infinite branch | head: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// Infinite silent branch loop must respect context cancellation.
	_, _, err = runTimedScript(t, dir, 300*time.Millisecond, "echo x | sed ':a; b a'")
	if err == nil {
		t.Errorf("expected non-nil error on timed-out infinite sed loop")
	}

	// '$' address across multiple files matches only the last line of the final file.
	out, errOut, err = runParityScript(t, dir, "sed '$d' f1.txt f2.txt")
	if exitCode(err) != 0 || out != "a1\na2\nb1\n" {
		t.Errorf("sed '$d' multi-file: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// Concurrent sed range commands must not race on range state.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, _, e := runParityScript(t, dir, "seq 1 10 | sed -n '2,5p' | sed -n '2,3p'")
			if exitCode(e) != 0 || o != "3\n4\n" {
				t.Errorf("concurrent sed range: exit=%d out=%q", exitCode(e), o)
			}
		}()
	}
	wg.Wait()
}

func TestBugfixSedUTF8AndZeroLengthGlobalSub(t *testing.T) {
	dir := t.TempDir()

	out, errOut, err := runParityScript(t, dir, "printf 'hi\\n' | sed 's/hi/café &/'")
	if exitCode(err) != 0 || out != "café hi\n" {
		t.Errorf("sed UTF-8 replacement with &: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runParityScript(t, dir, "printf 'abc\\n' | sed 's/^/> &/g'")
	if exitCode(err) != 0 || out != "> abc\n" {
		t.Errorf("sed zero-length ^ global sub: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runParityScript(t, dir, "printf 'ab\\n' | sed 's/x*/[&]/g'")
	if exitCode(err) != 0 || out != "[]a[]b[]\n" {
		t.Errorf("sed zero-length x* global sub with &: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runParityScript(t, dir, "printf 'é\\n' | sed 's/./[&]/g'")
	if exitCode(err) != 0 || out != "[é]\n" {
		t.Errorf("sed multi-byte . global sub: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runParityScript(t, dir, "printf 'café\\n' | sed 'y/é/e/'")
	if exitCode(err) != 0 || out != "cafe\n" {
		t.Errorf("sed Unicode y///: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}
}

func TestBugfixSedParserBlocksBranchesAndCycleLineNo(t *testing.T) {
	dir := t.TempDir()

	out, errOut, err := runParityScript(t, dir, "printf 'a;b\\nc\\n' | sed '/a;b/d'")
	if exitCode(err) != 0 || out != "c\n" {
		t.Errorf("sed /a;b/d: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runParityScript(t, dir, "printf 'sys\\nok\\n' | sed '/sys/d; s/ok/good/'")
	if exitCode(err) != 0 || out != "good\n" {
		t.Errorf("sed /sys/d; s/ok/good/: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runParityScript(t, dir, "printf '{\\nx\\n' | sed '/{/d; s/x/y/'")
	if exitCode(err) != 0 || out != "y\n" {
		t.Errorf("sed /{/d; s/x/y/: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// Label inside { ... } block.
	out, errOut, err = runTimedScript(t, dir, 2*time.Second, `printf 'start\na\nb\nend\nafter\n' | sed '/start/{ :loop; N; /end/!b loop; s/\n/-/g }'`)
	if exitCode(err) != 0 || out != "start-a-b-end\nafter\n" {
		t.Errorf("sed label inside block: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// Branch from inside { ... } block to outer label must not infinite-loop.
	out, errOut, err = runTimedScript(t, dir, 2*time.Second, `printf 'x\n' | sed ':done; /x/{ s/x/y/; b done; s/y/z/ }'`)
	if exitCode(err) != 0 || out != "y\n" {
		t.Errorf("sed branch out of block: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// D restarts cycle without reading a new input line, so line number '=' must not increment.
	out, errOut, err = runParityScript(t, dir, `printf 'a\nb\nc\n' | sed -n '1{N;=;D}; ='`)
	if exitCode(err) != 0 || out != "2\n2\n3\n" {
		t.Errorf("sed D lineNo: exit=%d out=%q, want \"2\\n2\\n3\\n\" (stderr=%q)", exitCode(err), out, errOut)
	}
}

func TestBugfixSedInplaceSafetyAndFlags(t *testing.T) {
	dir := t.TempDir()

	// Runtime error must NOT clobber target file!
	pErr := filepath.Join(dir, "keep.txt")
	mustWrite(t, pErr, "precious content\n")
	_, _, err := runParityScript(t, dir, "sed -i 's//x/' keep.txt")
	if exitCode(err) == 0 {
		t.Errorf("expected sed -i 's//x/' to fail")
	}
	gotBytes, readErr := os.ReadFile(pErr)
	if readErr != nil || string(gotBytes) != "precious content\n" {
		t.Errorf("sed -i clobbered file on error: %q (err=%v)", string(gotBytes), readErr)
	}

	// sed -i 'd' on non-empty file must produce 0 bytes, not a phantom newline.
	pDel := filepath.Join(dir, "del.txt")
	mustWrite(t, pDel, "line1\nline2\n")
	_, errOut, err := runParityScript(t, dir, "sed -i 'd' del.txt")
	if exitCode(err) != 0 {
		t.Fatalf("sed -i 'd' failed: %v (%s)", err, errOut)
	}
	gotBytes, _ = os.ReadFile(pDel)
	if len(gotBytes) != 0 {
		t.Errorf("sed -i 'd' left %q, want empty file", string(gotBytes))
	}

	// Combined -Ei flag must edit in-place with ERE.
	pERE := filepath.Join(dir, "ere.txt")
	mustWrite(t, pERE, "foofoo\n")
	_, errOut, err = runParityScript(t, dir, "sed -Ei 's/(foo)+/bar/' ere.txt")
	if exitCode(err) != 0 {
		t.Fatalf("sed -Ei failed: %v (%s)", err, errOut)
	}
	gotBytes, _ = os.ReadFile(pERE)
	if string(gotBytes) != "bar\n" {
		t.Errorf("sed -Ei produced %q, want \"bar\\n\"", string(gotBytes))
	}

	// File named "-input.txt" after "--" must NOT trigger -i in-place mode.
	pDash := filepath.Join(dir, "-input.txt")
	mustWrite(t, pDash, "alpha\n")
	out, errOut, err := runParityScript(t, dir, "sed 's/alpha/beta/' -- -input.txt")
	if exitCode(err) != 0 || out != "beta\n" {
		t.Errorf("sed -- -input.txt: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}
	gotBytes, _ = os.ReadFile(pDash)
	if string(gotBytes) != "alpha\n" {
		t.Errorf("sed -- -input.txt modified file in-place: %q", string(gotBytes))
	}
}

func TestBugfixGrepStreamingBinaryAndCombinedFlags(t *testing.T) {
	dir := t.TempDir()

	out, errOut, err := runTimedScript(t, dir, 2*time.Second, "yes hello | grep hello | head -n 2")
	if exitCode(err) != 0 || errOut != "" || out != "hello\nhello\n" {
		t.Errorf("yes | grep | head: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	binFile := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(binFile, []byte("hdr\x00x\nneedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, err = runParityScript(t, dir, "grep -na needle bin.dat")
	if exitCode(err) != 0 || out != "2:needle\n" {
		t.Errorf("grep -na on binary file: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// Unterminated binary file match (exercises (n, nil) + (0, io.EOF) chunk boundary in streamLines).
	untermBin := filepath.Join(dir, "unterm.dat")
	if err := os.WriteFile(untermBin, []byte("foo\x00bar"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, err = runParityScript(t, dir, "grep foo unterm.dat")
	if exitCode(err) != 0 || !strings.Contains(errOut, "binary file matches") {
		t.Errorf("grep unterminated binary: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}
}

func TestBugfixSeqStreamingWidthAndOverflow(t *testing.T) {
	dir := t.TempDir()

	out, errOut, err := runTimedScript(t, dir, 2*time.Second, "seq 1 1000000000 | head -n 3")
	if exitCode(err) != 0 || errOut != "" || out != "1\n2\n3\n" {
		t.Errorf("seq 1 1e9 | head: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runTimedScript(t, dir, 2*time.Second, "seq -w 1 1000000000 | head -n 2")
	if exitCode(err) != 0 || errOut != "" || out != "0000000001\n0000000002\n" {
		t.Errorf("seq -w 1 1e9 | head: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runTimedScript(t, dir, 2*time.Second, "seq 9223372036854775806 9223372036854775807")
	if exitCode(err) != 0 || out != "9223372036854775806\n9223372036854775807\n" {
		t.Errorf("seq MaxInt64: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runTimedScript(t, dir, 2*time.Second, "seq -w -- -1 2")
	if exitCode(err) != 0 || out != "-1\n00\n01\n02\n" {
		t.Errorf("seq -w -1 2: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runTimedScript(t, dir, 2*time.Second, "seq -w -- -1 100 | head -n 3")
	if exitCode(err) != 0 || out != "-01\n000\n001\n" {
		t.Errorf("seq -w -1 100: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// ULP underflow in float64 must not loop forever when step << ULP(first).
	_, _, err = runTimedScript(t, dir, 2*time.Second, "seq 1e30 1 1e30")
	if exitCode(err) != 0 {
		t.Errorf("seq 1e30 1 1e30: exit=%d", exitCode(err))
	}
}

func TestBugfixTrEmptySet2AndHighByteRanges(t *testing.T) {
	dir := t.TempDir()

	// Must return an error (no slice-bounds panic) when SET2 is empty without -d.
	_, _, err := runParityScript(t, dir, "printf 'abc\n' | tr 'a' ''")
	if exitCode(err) == 0 {
		t.Errorf("expected tr 'a' '' to fail with non-zero exit")
	}

	// High-byte octal range ending at \377 (0xff) must not wrap uint8 and loop forever.
	inFile := filepath.Join(dir, "hi.bin")
	if err := os.WriteFile(inFile, []byte{'a', 0xff, 'b', '\n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := runTimedScript(t, dir, 2*time.Second, `tr '\200-\377' 'X' < hi.bin`)
	if exitCode(err) != 0 || out != "aXb\n" {
		t.Errorf("tr \\200-\\377: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	ctrlFile := filepath.Join(dir, "ctrl.bin")
	if err := os.WriteFile(ctrlFile, []byte{0x01, 'o', 'k', 0x1f}, 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, err = runTimedScript(t, dir, 2*time.Second, `tr -d '\000-\037' < ctrl.bin`)
	if exitCode(err) != 0 || out != "ok" {
		t.Errorf("tr -d \\000-\\037: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}
}

func TestBugfixStreamingPipelinesTextAndText2(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		src  string
		want string
	}{
		{`yes "a:b:c" | cut -d: -f2 | head -n 2`, "b\nb\n"},
		{`seq 1 1000000000 | uniq | head -n 2`, "1\n2\n"},
		{`yes "item" | paste - | head -n 2`, "item\nitem\n"},
		{`yes | hexdump -C -n 4`, "00000000  79 0a 79 0a                                       |y.y.|\n00000004\n"},
		{`yes | hexdump -C | head -n 1`, "00000000  79 0a 79 0a 79 0a 79 0a  79 0a 79 0a 79 0a 79 0a  |y.y.y.y.y.y.y.y.|\n"},
		{`yes | od -N 4 -t x1`, "0000000 79 0a 79 0a\n0000004\n"},
		{`yes | od -t x1 | head -n 1`, "0000000 79 0a 79 0a 79 0a 79 0a 79 0a 79 0a 79 0a 79 0a\n"},
		{`yes "hello world" | fold -w 5 | head -n 2`, "hello\n worl\n"},
		{`yes "abc" | nl | head -n 2`, "     1\tabc\n     2\tabc\n"},
		{`yes "abc" | rev | head -n 2`, "cba\ncba\n"},
		{`yes "        x" | unexpand | head -n 2`, "\tx\n\tx\n"},
		{`yes "line" | tail -n +2 | head -n 2`, "line\nline\n"},
		{`yes "hello" | strings | head -n 2`, "hello\nhello\n"},
	}

	for _, tc := range cases {
		out, errOut, err := runTimedScript(t, dir, 2*time.Second, tc.src)
		if exitCode(err) != 0 || errOut != "" || out != tc.want {
			t.Errorf("%s:\n  exit=%d stderr=%q\n  got =%q\n  want=%q", tc.src, exitCode(err), errOut, out, tc.want)
		}
	}

	// Invalid cut 0 / decreasing range must fail with exit 1.
	for _, bad := range []string{
		`printf 'a:b\n' | cut -d: -f0`,
		`printf 'a:b:c\n' | cut -d: -f3-1`,
	} {
		_, _, err := runParityScript(t, dir, bad)
		if exitCode(err) == 0 {
			t.Errorf("expected failure for %s", bad)
		}
	}
}

func TestBugfixSortHumanNumericAndLargeDiff(t *testing.T) {
	dir := t.TempDir()

	out, errOut, err := runParityScript(t, dir, `printf '2E\n500P\n10T\n' | sort -h`)
	if exitCode(err) != 0 || out != "10T\n500P\n2E\n" {
		t.Errorf("sort -h Exabytes: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	out, errOut, err = runParityScript(t, dir, `printf '2026-11-13\n100\n50\n' | sort -h`)
	if exitCode(err) != 0 || out != "50\n100\n2026-11-13\n" {
		t.Errorf("sort -h date prefix: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// Large diff (12,000 lines each, differing in the middle and without shared prefix/suffix):
	// previously allocated 12001*12001*8 bytes (~1.15 GB) in a 2D slice!
	var a, b strings.Builder
	a.WriteString("head-a\n")
	b.WriteString("head-b\n")
	for i := 0; i < 12000; i++ {
		fmt.Fprintf(&a, "shared-line-%d\n", i)
		fmt.Fprintf(&b, "shared-line-%d\n", i)
	}
	a.WriteString("tail-a\n")
	b.WriteString("tail-b\n")
	mustWrite(t, filepath.Join(dir, "large1.txt"), a.String())
	mustWrite(t, filepath.Join(dir, "large2.txt"), b.String())

	out, errOut, err = runTimedScript(t, dir, 4*time.Second, "diff -u large1.txt large2.txt")
	if exitCode(err) != 1 || !strings.Contains(out, "-head-a") || !strings.Contains(out, "+tail-b") {
		t.Errorf("large diff -u: exit=%d stderr=%q out_len=%d", exitCode(err), errOut, len(out))
	}
}

func TestBugfixText2UnexpandUTF8JoinUnpairedAndTacRegex(t *testing.T) {
	dir := t.TempDir()

	// 'é' is 1 column (2 UTF-8 bytes) + 7 spaces = column 8 (single tab stop).
	out, errOut, err := runParityScript(t, dir, "printf 'é       x\\n' | unexpand -a")
	if exitCode(err) != 0 || out != "é\tx\n" {
		t.Errorf("unexpand UTF-8 column: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// join -a 2 when -1 1 -2 2 must skip column 2 (the join key) of unpaired file-2 rows, not column 1!
	mustWrite(t, filepath.Join(dir, "j1.txt"), "k1 a1\n")
	mustWrite(t, filepath.Join(dir, "j2.txt"), "b1 k1 c1\nb2 k2 c2\n")
	out, errOut, err = runParityScript(t, dir, "join -a 2 -1 1 -2 2 j1.txt j2.txt")
	if exitCode(err) != 0 || out != "k1 a1 b1 c1\nk2 b2 c2\n" {
		t.Errorf("join -a 2 -1 1 -2 2: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}

	// tac -r -s REGEX must treat -s as a regular expression separator.
	out, errOut, err = runParityScript(t, dir, `printf 'rec1:1\nrec2:2\n' | tac -r -s ':[0-9]\n'`)
	if exitCode(err) != 0 || out != "rec2:2\nrec1:1\n" {
		t.Errorf("tac -r -s: exit=%d out=%q stderr=%q", exitCode(err), out, errOut)
	}
}

func TestBugfixNilStdinNoPanic(t *testing.T) {
	dir := t.TempDir()
	// Embedded interpreter callers may pass nil Stdin via interp.StdIO(nil, ...).
	// Builtins reading from stdin must treat nil Stdin as empty EOF rather than panicking.
	cmds := []string{
		"cat",
		"head -n 5",
		"tail -n 5",
		"sed 's/a/b/'",
		"tr 'a' 'b'",
		"cut -f1",
		"paste",
		"uniq",
		"wc -l",
		"od -An",
		"hexdump -C",
		"nl",
		"rev",
		"fold",
		"expand",
		"unexpand",
		"tac",
	}
	for _, c := range cmds {
		_, errOut, err := runTimedScript(t, dir, 2*time.Second, c)
		if exitCode(err) != 0 || errOut != "" {
			t.Errorf("nil stdin %q: exit=%d stderr=%q", c, exitCode(err), errOut)
		}
	}
}
