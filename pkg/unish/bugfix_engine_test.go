package unish

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

var (
	engineBinOnce sync.Once
	engineBinPath string
	engineBinErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if engineBinPath != "" {
		_ = os.RemoveAll(filepath.Dir(engineBinPath))
	}
	os.Exit(code)
}

// engineTestBinary compiles unish once into an OS temp directory (outside the
// workspace) so CLI, signal, and self-kill behavior can be tested safely.
func engineTestBinary(t *testing.T) string {
	t.Helper()
	engineBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "unish-engine-testbin-*")
		if err != nil {
			engineBinErr = err
			return
		}
		bin := filepath.Join(dir, "unish")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-buildvcs=false", "-mod=vendor", "-o", bin, "../..")
		if out, err := cmd.CombinedOutput(); err != nil {
			engineBinErr = &os.PathError{Op: "go build", Path: string(out), Err: err}
			return
		}
		engineBinPath = bin
	})
	if engineBinErr != nil {
		t.Fatalf("build test binary: %v", engineBinErr)
	}
	return engineBinPath
}

func runEngineScriptCtx(t *testing.T, ctx context.Context, dir, script string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	r, err := interp.New(
		interp.StdIO(strings.NewReader(""), &stdout, &stderr),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
		interp.Dir(dir),
	)
	if err != nil {
		t.Fatalf("interp.New: %v", err)
	}
	prog, err := syntax.NewParser().Parse(strings.NewReader(script), "test.sh")
	if err != nil {
		t.Fatalf("parse %q: %v", script, err)
	}
	err = r.Run(ctx, prog)
	return stdout.String(), stderr.String(), err
}

func runEngineScript(t *testing.T, dir, script string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return runEngineScriptCtx(t, ctx, dir, script)
}

// 1. Parameter replacement ${var/pat}, ${var//pat}, ${var/pat/}, quoted #/% anchors, and quoted/escaped &.
func TestBugfixEngine_ParamReplaceNilAndAnchors(t *testing.T) {
	dir := t.TempDir()
	script := `
x="hello world world"
echo "del1:<${x/world}>"
echo "delall:<${x//world}>"
echo "delempty:<${x/world/}>"
echo "delspace:<${x// /}>"

h="a#b#c"
echo "qhash:<${h/"#"/X}>"
echo "sqhash:<${h/'#'/X}>"
echo "eschash:<${h/\#/X}>"
echo "anchhash:<${h/#a/Z}>"

p="a%b%c"
echo "qpct:<${p/"%"/Y}>"
echo "anchpct:<${p/%c/W}>"

v="abc"
echo "qamp:<${v/b/"&"}>"
echo "sqamp:<${v/b/'&'}>"
echo "bsamp:<${v/b/\\&}>"
`
	out, serr, err := runEngineScript(t, dir, script)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%q)", err, serr)
	}
	want := strings.Join([]string{
		"del1:<hello  world>",
		"delall:<hello  >",
		"delempty:<hello  world>",
		"delspace:<helloworldworld>",
		"qhash:<aXb#c>",
		"sqhash:<aXb#c>",
		"eschash:<aXb#c>",
		"anchhash:<Z#b#c>",
		"qpct:<aYb%c>",
		"anchpct:<a%b%W>",
		"qamp:<a&c>",
		"sqamp:<a&c>",
		`bsamp:<a\bc>`,
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

// 2. Arithmetic indexed/associative lvalues (( a[0] = 1 )), (( a[0]++ )), and overwriting non-numeric scalar (( x = 5 )).
func TestBugfixEngine_ArithArrayAndOverwriteNonNumeric(t *testing.T) {
	dir := t.TempDir()
	script := `
a=(10 20)
(( a[0] = 5 ))
(( a[0] += 3 ))
(( a[1]++ ))
(( ++a[1] ))
i=0
(( a[i++] += 10 ))
echo "arr:${a[0]},${a[1]},i=$i"

declare -A m
(( m[hits] = 4 ))
(( m[hits] += 2 ))
(( m[hits]++ ))
echo "map:${m[hits]}"

x="hello world"
(( x = 42 ))
echo "scalar:$x"
`
	out, serr, err := runEngineScript(t, dir, script)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%q)", err, serr)
	}
	want := "arr:18,22,i=1\nmap:7\nscalar:42\n"
	if out != want {
		t.Fatalf("got %q, want %q (stderr=%q)", out, want, serr)
	}
}

// 3. Associative array subscripts containing arithmetic operator characters like hyphens (foo-bar, $k-bar).
func TestBugfixEngine_AssocSubscriptExprNoPanic(t *testing.T) {
	dir := t.TempDir()
	script := `
declare -A m=([foo-bar]=10 [one+two]=20)
m[foo-bar]=15
k=svc
m[$k-port]=8080
: "${m[lazy-key]=77}"
echo "${m[foo-bar]}|${m[one+two]}|${m[$k-port]}|${m[lazy-key]}"
`
	out, serr, err := runEngineScript(t, dir, script)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%q)", err, serr)
	}
	if out != "15|20|8080|77\n" {
		t.Fatalf("got %q, want %q (stderr=%q)", out, "15|20|8080|77\n", serr)
	}
}

// 4. declare -i / local -i preserving local scope, export (-x), and readonly (-r) attributes.
func TestBugfixEngine_DeclareIntegerLocalExportReadonly(t *testing.T) {
	dir := t.TempDir()
	script := `
x=outer
f() {
	local -i x=2+3
	echo "in:$x"
}
f
echo "out:$x"
declare -ix EXP_NUM=4+6
echo "${EXP_NUM}:${EXP_NUM@a}"
declare -ir RO_NUM=3+4
echo "ro:$RO_NUM:${RO_NUM@a}"
RO_NUM=99 2>/dev/null
echo "ro_rc:$?,ro_after:$RO_NUM"
`
	out, serr, err := runEngineScript(t, dir, script)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%q)", err, serr)
	}
	want := "in:5\nout:outer\n10:x\nro:7:r\nro_rc:1,ro_after:7\n"
	if out != want {
		t.Fatalf("got %q, want %q (stderr=%q)", out, want, serr)
	}
}

// 5. extraFds / extraInFds isolation across subshells, concurrent pipelines, and non-exec statement scope.
func TestBugfixEngine_ExtraFdsSubshellAndStmtScope(t *testing.T) {
	dir := t.TempDir()
	script := `
exec 3>parent.txt
echo "p1" >&3
(
	exec 3>&-
	if echo "sub_bad" 2>/dev/null >&3; then
		echo "BUG_SUB_FD_OPEN"
	else
		echo "sub_closed_ok"
	fi
)
echo "p2" >&3

# Temporary statement redirection on fd 3 must restore parent's fd 3 afterwards.
echo "inner_out" 3>temp3.txt >&3
echo "p3" >&3
exec 3>&-

# After closing fd 3, a non-exec redirect on fd 4 must not stay open afterwards.
echo "hi" 4>temp4.txt >&4
if echo "leak" 2>/dev/null >&4; then
	echo "BUG_FD4_LEAKED"
else
	echo "fd4_restored_ok"
fi

# Concurrent pipeline stages opening/closing fd 3 must not race on extraFds map.
for n in 1 2 3 4 5 6 7 8 9 10; do
	( exec 3>left.txt; echo L >&3; exec 3>&- ) | ( exec 3>right.txt; echo R >&3; exec 3>&- )
done
cat parent.txt
cat temp3.txt
`
	out, serr, err := runEngineScript(t, dir, script)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%q)", err, serr)
	}
	want := "sub_closed_ok\nfd4_restored_ok\np1\np2\np3\ninner_out\n"
	if out != want {
		t.Fatalf("got %q, want %q (stderr=%q)", out, want, serr)
	}
}

// 6. Process substitution >(cmd) non-deadlock with builtins/commands and zero temp-file leaks across all forms.
func TestBugfixEngine_ProcSubstCmdOutAndLeaks(t *testing.T) {
	dir := t.TempDir()
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "unish-procsub-*"))
	beforeSet := map[string]bool{}
	for _, p := range before {
		beforeSet[p] = true
	}

	script := `
printf 'alpha\n' | tee >(cat > out_tee.txt) >/dev/null
wait
cat out_tee.txt

read -r line < <(printf 'redir_in\n')
echo "rd:$line"

echo <(printf 'unconsumed_in\n') >/dev/null
echo >(cat >/dev/null) >/dev/null
wait

exec 3< <(printf 'fd3_in\n')
read -r fdline <&3
exec 3<&-
echo "fd3:$fdline"
`
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, serr, err := runEngineScriptCtx(t, ctx, dir, script)
	if err != nil {
		t.Fatalf("procsubst script failed/timed out: %v (stderr=%q)", err, serr)
	}
	want := "alpha\nrd:redir_in\nfd3:fd3_in\n"
	if out != want {
		t.Fatalf("got %q, want %q (stderr=%q)", out, want, serr)
	}

	// Give background cleanup a brief moment, then assert zero new leaked files.
	deadline := time.Now().Add(time.Second)
	var leaked []string
	for {
		leaked = nil
		after, _ := filepath.Glob(filepath.Join(os.TempDir(), "unish-procsub-*"))
		for _, p := range after {
			if !beforeSet[p] {
				leaked = append(leaked, p)
			}
		}
		if len(leaked) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(leaked) > 0 {
		for _, p := range leaked {
			_ = os.Remove(p)
		}
		t.Fatalf("leaked procsubst temp files: %v", leaked)
	}
}

// 7. timeout external double-Wait race, stderr gateWriter reaping, and timeout 0 parent context cancellation.
func TestBugfixEngine_TimeoutDoubleWaitStderrGateAndZeroCtx(t *testing.T) {
	bin := engineTestBinary(t)
	dir := t.TempDir()

	// Part A: repeated fast external command under timeout must never fail with "exec: Wait was already called".
	for i := 0; i < 10; i++ {
		var out, serr bytes.Buffer
		hc := probeHC(t, nil, &out, &serr)
		if err := cmdTimeout(context.Background(), hc, []string{"timeout", "5", bin, "-c", "true"}); err != nil {
			t.Fatalf("iter %d: timeout 5 fast external failed: %v (stderr=%q)", i, err, serr.String())
		}
		if err := cmdTimeout(context.Background(), hc, []string{"timeout", "0", bin, "-c", "true"}); err != nil {
			t.Fatalf("iter %d: timeout 0 fast external failed: %v (stderr=%q)", i, err, serr.String())
		}
	}

	// Part B: builtin stuck writing to Stderr must be gated and reaped on timeout.
	var errBytes int64
	hc := probeHC(t, nil, io.Discard, countWriter{&errBytes})
	released := make(chan struct{})
	stuckStderr := func(ctx context.Context, hc interp.HandlerContext, args []string) error {
		defer close(released)
		_ = ctx
		for {
			if _, err := hc.Stderr.Write([]byte("e")); err != nil {
				return nil
			}
		}
	}
	extraCommands = append(extraCommands, extraCmd{"__bugfix_stderr_stuck__", stuckStderr})
	defer func() {
		for i := range extraCommands {
			if extraCommands[i].name == "__bugfix_stderr_stuck__" {
				extraCommands = append(extraCommands[:i], extraCommands[i+1:]...)
				break
			}
		}
	}()
	_ = cmdTimeout(context.Background(), hc, []string{"timeout", "0.1", "__bugfix_stderr_stuck__"})
	select {
	case <-released:
	case <-time.After(1500 * time.Millisecond):
		t.Fatalf("stderr-writing builtin goroutine leaked after timeout")
	}
	mark := atomic.LoadInt64(&errBytes)
	time.Sleep(80 * time.Millisecond)
	if after := atomic.LoadInt64(&errBytes); after != mark {
		t.Fatalf("stderr-writing builtin kept writing after reap (%d -> %d)", mark, after)
	}

	// Part C: timeout 0 with external command must respect parent ctx cancellation.
	pctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	hc2 := probeHC(t, nil, io.Discard, io.Discard)
	_ = cmdTimeout(pctx, hc2, []string{"timeout", "0", bin, "-c", "sleep 10"})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout 0 ignored parent ctx cancellation (took %v)", elapsed)
	}
	_ = dir
}

// 8. Stable job IDs across sweep, exact non-zero exit status from wait $!, and non-stalling empty wait/jobs.
func TestBugfixEngine_JobsStableIDsWaitCodeAndFastEmpty(t *testing.T) {
	bin := engineTestBinary(t)
	dir := t.TempDir()

	// Ensure clean baseline.
	globalJobs.sweep(0)

	// Part A: empty jobs / wait should not stall 500ms per invocation.
	t0 := time.Now()
	_, _, err := runEngineScript(t, dir, "jobs; wait")
	if err != nil {
		t.Fatalf("jobs; wait failed: %v", err)
	}
	if d := time.Since(t0); d > 350*time.Millisecond {
		t.Fatalf("jobs; wait stalled on empty job table (%v)", d)
	}

	// Part B: job 1 finishes and is swept while job 2 is still running -> %2 must still resolve to job 2.
	c1 := exec.Command(bin, "-c", "sleep 10")
	if err := c1.Start(); err != nil {
		t.Fatalf("start c1: %v", err)
	}
	j1 := globalJobs.track(c1, []string{"job_one"})

	c2 := exec.Command(bin, "-c", "sleep 10")
	if err := c2.Start(); err != nil {
		_ = j1.proc.Kill()
		<-j1.done
		globalJobs.sweep(0)
		t.Fatalf("start c2: %v", err)
	}
	j2 := globalJobs.track(c2, []string{"job_two"})
	defer func() {
		_ = j1.proc.Kill()
		<-j1.done
		_ = j2.proc.Kill()
		<-j2.done
		globalJobs.sweep(0)
	}()

	_ = j1.proc.Kill()
	<-j1.done
	globalJobs.sweep(0)

	if got1 := globalJobs.byIndex(1); got1 != nil {
		t.Fatalf("expected %%1 to be nil after job 1 swept, got %v", got1.argv)
	}
	if got2 := globalJobs.byIndex(2); got2 != j2 {
		t.Fatalf("expected %%2 to resolve to job 2 after job 1 swept, got %v", got2)
	}
	_ = j2.proc.Kill()
	<-j2.done
	globalJobs.sweep(0)

	// Part C: wait $! must preserve exact exit code (e.g. 42), not collapse to 1.
	script := `"` + filepath.ToSlash(bin) + `" -c "exit 42" & p=$!; wait $p; echo "rc=$?"`
	wcmd := exec.Command(bin, "-c", script)
	wcmd.Dir = dir
	wout, err := wcmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wait $! script error: %v (out=%q)", err, string(wout))
	}
	if strings.TrimSpace(string(wout)) != "rc=42" {
		t.Fatalf("wait $! lost exit status: got %q, want %q", string(wout), "rc=42\n")
	}
}

// 9. kill -0 probe (must not kill live child or $$), self-signal traps in fresh runners/subshells, trap ''/SIGINT/0/-p/-l, and single OS trap execution.
func TestBugfixEngine_KillZeroProbeAndSelfTrap(t *testing.T) {
	bin := engineTestBinary(t)
	dir := t.TempDir()

	// Part A: kill -0 on a live child must NOT terminate it; kill -0 on dead child must fail.
	child := exec.Command(bin, "-c", "sleep 10")
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	j := globalJobs.track(child, []string{"probe_sleep"})
	defer func() {
		_ = j.proc.Kill()
		<-j.done
		globalJobs.sweep(0)
	}()

	if err := killProc(j.proc, "0"); err != nil {
		t.Fatalf("killProc -0 on live child failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if !j.alive() {
		t.Fatalf("kill -0 terminated the live child process!")
	}
	_ = j.proc.Kill()
	<-j.done
	globalJobs.sweep(0)

	// Part B: kill -0 $$ and trap + kill -TERM $$ inside subprocess & subshell.
	cmd := exec.Command(bin, "-c", `
kill -0 $$ || exit 91
trap 'echo caught_term' SIGTERM
kill -TERM $$
echo after_self_term
(
	trap 'echo sub_int' INT
	kill -INT $$
	echo after_sub_int
)
(
	trap 'exit 37' TERM
	kill -TERM $$
	echo SHOULD_NOT_RUN
)
echo "trap_exit:$?"
trap '' TERM
kill -TERM $$
echo after_ignored_term
trap -p TERM
trap -l >/dev/null || exit 92
trap 'echo on_zero' 0
`)
	rawOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("subprocess self-signal/trap test failed: %v\noutput:\n%s", err, string(rawOut))
	}
	got := strings.ReplaceAll(string(rawOut), "\r\n", "\n")
	if strings.Contains(got, "SHOULD_NOT_RUN") {
		t.Fatalf("trap 'exit 37' did not exit subshell:\n%s", got)
	}
	for _, wantLine := range []string{
		"caught_term",
		"after_self_term",
		"sub_int",
		"after_sub_int",
		"trap_exit:37",
		"after_ignored_term",
		"trap -- '' TERM",
		"on_zero",
	} {
		if !strings.Contains(got, wantLine) {
			t.Fatalf("missing %q in trap output:\n%s", wantLine, got)
		}
	}
	_ = dir
}

// 10. main.go positional parameters starting with '-' or '--' must not be parsed as set flags.
func TestBugfixEngine_MainPositionalDashArgs(t *testing.T) {
	bin := engineTestBinary(t)
	dir := t.TempDir()

	scriptPath := filepath.Join(dir, "echo_args.sh")
	if err := os.WriteFile(scriptPath, []byte(`printf '%s\n' "$1" "$2" "$3" "$4"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, scriptPath, "-n", "--help", "-", "-z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script with dash positional args failed: %v\nout=%q", err, string(out))
	}
	got := strings.ReplaceAll(string(out), "\r\n", "\n")
	want := "-n\n--help\n-\n-z\n"
	if got != want {
		t.Fatalf("script positional args: got %q, want %q", got, want)
	}

	cmd2 := exec.Command(bin, "-c", `printf '%s|' "$0" "$@"`, "myprog", "-e", "--foo", "-")
	out2, err := cmd2.CombinedOutput()
	if err != nil {
		t.Fatalf("-c with dash positional args failed: %v\nout=%q", err, string(out2))
	}
	if string(out2) != "myprog|-e|--foo|-|" {
		t.Fatalf("-c positional args: got %q, want %q", string(out2), "myprog|-e|--foo|-|")
	}
}

// 11. hash `--` end-of-options and `type` support for functions, keywords, -t, -p, -P, and `--`.
func TestBugfixEngine_HashDashDashAndTypeBuiltin(t *testing.T) {
	dir := t.TempDir()
	script := `
hash -r
hash -- go
hash -d -- go
hash -d -- -nonexistent

myfn() { echo hello; }
type myfn
type -t myfn
type if
type -t if
type -t ls
type -t cd
type -p ls
	type -P go >/dev/null && echo "typeP_go:ok"
	type -- -nonexistent 2>/dev/null || echo "dash_notfound:$?"
	ulimit -n -- >/dev/null && echo "ulimit_dash:ok"
	orig_mask=$(umask)
	for i in 1 2 3 4 5; do
		( umask 077 ) | ( umask -S >/dev/null )
	done
	umask "$orig_mask"
`
	out, serr, err := runEngineScript(t, dir, script)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%q)", err, serr)
	}
	want := strings.Join([]string{
		"myfn is a function",
		"function",
		"if is a shell keyword",
		"keyword",
		"builtin",
		"builtin",
		"typeP_go:ok",
		"dash_notfound:1",
		"ulimit_dash:ok",
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s\n(stderr=%q)", out, want, serr)
	}
}

// 12. Subshell trap inheritance (sigIgnored + sigNames), subshell EXIT traps across (...)/$(...)/<(...)/|/&, and subshell os/signal teardown.
func TestBugfixEngine_SubshellTrapsIgnoredExitAndTeardown(t *testing.T) {
	bin := engineTestBinary(t)
	dir := t.TempDir()

	// Part A: inherited `trap '' TERM` keeps signal name in `( trap )` and is not wiped by setting another trap in the subshell.
	cmdA := exec.Command(bin, "-c", `
trap '' TERM
( trap )
echo "---"
(
	trap 'echo got_int' INT
	trap
	kill -TERM $$
	kill -INT $$
	echo "after_sub_signals"
)
`)
	cmdA.Dir = dir
	rawA, err := cmdA.CombinedOutput()
	if err != nil {
		t.Fatalf("Part A subshell sigIgnored failed: %v\noutput:\n%s", err, string(rawA))
	}
	gotA := strings.ReplaceAll(string(rawA), "\r\n", "\n")
	wantA := strings.Join([]string{
		"trap -- '' TERM",
		"---",
		"trap -- 'echo got_int' INT",
		"trap -- '' TERM",
		"got_int",
		"after_sub_signals",
	}, "\n") + "\n"
	if gotA != wantA {
		t.Fatalf("Part A got:\n%s\nwant:\n%s", gotA, wantA)
	}

	// Part B: subshell EXIT traps in (...), $(...), <(...), pipeline, and { ...; } &.
	scriptB := `
( trap 'echo "sub_false:$?"' EXIT; false )
echo "rc1:$?"
( trap 'echo "sub_exit:$?"' EXIT; exit 3; echo NEVER )
echo "rc2:$?"
( trap 'echo "sub_override:$?"; exit 9; echo NEVER_IN_TRAP' EXIT; exit 3 )
echo "rc3:$?"
cs=$( trap 'echo "cs_exit:$?"' EXIT; echo body; (exit 4) )
echo "cs_rc:$? cs_val:${cs//$'\n'/|}"
{ trap 'echo "bg_norm:$?" > bg1.txt' EXIT; false; } &
p1=$!
wait $p1
echo "bg1_rc:$? bg1:$(cat bg1.txt)"
{ trap 'echo "bg_exp:$?" >> bg2.txt' EXIT; exit 5; } &
p2=$!
wait $p2
echo "bg2_rc:$? bg2:$(cat bg2.txt)"
read -r psline < <( trap 'echo "ps_exit:$?"' EXIT; false )
echo "ps:$psline"
( trap 'echo "pipe_exit:$?"' EXIT; echo pbody ) | tr '\n' ':'
echo ""
`
	outB, serrB, err := runEngineScript(t, dir, scriptB)
	if err != nil {
		t.Fatalf("Part B subshell EXIT traps failed: %v (stderr=%q)", err, serrB)
	}
	wantB := strings.Join([]string{
		"sub_false:1",
		"rc1:1",
		"sub_exit:3",
		"rc2:3",
		"sub_override:3",
		"rc3:9",
		"cs_rc:4 cs_val:body|cs_exit:4",
		"bg1_rc:1 bg1:bg_norm:1",
		"bg2_rc:5 bg2:bg_exp:5",
		"ps:ps_exit:1",
		"pbody:pipe_exit:0:",
	}, "\n") + "\n"
	if outB != wantB {
		t.Fatalf("Part B got:\n%s\nwant:\n%s\n(stderr=%q)", outB, wantB, serrB)
	}

	// Part C: subshell signal traps across all 5 subshell forms must unregister their os/signal channels on exit.
	baseChans := interp.ActiveSignalChannels()
	teardownScript := `
for i in 1 2 3; do
	( trap 'echo x' INT TERM )
	v=$( trap 'echo x' INT; echo ok )
	read -r u < <( trap 'echo x' INT; echo ok )
	( trap 'echo x' INT; echo ok ) | cat >/dev/null
	{ trap 'echo x' INT; true; } &
	wait
done
`
	if _, serrC, err := runEngineScript(t, dir, teardownScript); err != nil {
		t.Fatalf("Part C teardown script failed: %v (stderr=%q)", err, serrC)
	}
	if afterChans := interp.ActiveSignalChannels(); afterChans != baseChans {
		t.Fatalf("subshell signal channels leaked: before=%d after=%d", baseChans, afterChans)
	}
	if runtime.GOOS != "windows" {
		cmdU := exec.Command(bin, "-c", `( trap 'echo x' USR1 ); kill -USR1 $$; echo SHOULD_NOT_SURVIVE`)
		outU, errU := cmdU.CombinedOutput()
		if errU == nil || strings.Contains(string(outU), "SHOULD_NOT_SURVIVE") {
			t.Fatalf("expected unhandled SIGUSR1 after subshell exit to terminate process, err=%v out=%q", errU, string(outU))
		}
	}
}
