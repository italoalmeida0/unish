package unish

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

var agentToolsRegMu sync.Mutex

func ensureAgentToolsRegistered() {
	agentToolsRegMu.Lock()
	defer agentToolsRegMu.Unlock()
	agentCmds := []extraCmd{
		{"rg", cmdRg},
		{"patch", cmdPatch},
		{"bc", cmdBc},
		{"sponge", cmdSponge},
		{"flock", cmdFlock},
		{"column", cmdColumn},
		{"xxd", cmdXxd},
	}
	for _, ac := range agentCmds {
		if lookupExtra(ac.name) == nil {
			extraCommands = append(extraCommands, ac)
		}
	}
}

func init() {
	ensureAgentToolsRegistered()
}

func runAgentScript(t *testing.T, dir, src string) (string, string, error) {
	t.Helper()
	return runAgentScriptCtx(t, context.Background(), 20*time.Second, dir, src)
}

func runAgentScriptCtx(t *testing.T, parent context.Context, timeout time.Duration, dir, src string) (string, string, error) {
	t.Helper()
	ensureAgentToolsRegistered()
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
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	err = r.Run(ctx, prog)
	return stdout.String(), stderr.String(), err
}

func agentExitCode(err error) int {
	if err == nil {
		return 0
	}
	if st, ok := interp.IsExitStatus(err); ok {
		return int(st)
	}
	if ee, ok := err.(exitError); ok {
		return ee.code
	}
	return -1
}

func writeAgentFile(t *testing.T, path string, content []byte, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, perm); err != nil {
		t.Fatal(err)
	}
}

// ============================================================================
// 1. rg tests
// ============================================================================

func TestRgBasicAndSmartDefaults(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, "alpha.go"), []byte("package main\nfunc Alpha() {}\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "sub", "beta.go"), []byte("package sub\n// Alpha call\nfunc Beta() {}\n"), 0o644)

	// Default target `.`: searches recursively in deterministic sorted order with filename + line numbers ON.
	out, stderr, err := runAgentScript(t, dir, `rg Alpha`)
	if agentExitCode(err) != 0 || stderr != "" {
		t.Fatalf("rg Alpha failed: code=%d stderr=%q out=%q", agentExitCode(err), stderr, out)
	}
	want := "alpha.go:2:func Alpha() {}\nsub/beta.go:2:// Alpha call\n"
	if out != want {
		t.Fatalf("rg Alpha got:\n%q\nwant:\n%q", out, want)
	}

	// Single file target: filename OFF by default, line number ON; -H forces filename, -N disables line numbers.
	out, _, err = runAgentScript(t, dir, `rg Alpha alpha.go`)
	if agentExitCode(err) != 0 || out != "2:func Alpha() {}\n" {
		t.Fatalf("rg single file got %q (code %d)", out, agentExitCode(err))
	}
	out, _, err = runAgentScript(t, dir, `rg -H -N Alpha alpha.go`)
	if agentExitCode(err) != 0 || out != "alpha.go:func Alpha() {}\n" {
		t.Fatalf("rg -H -N got %q (code %d)", out, agentExitCode(err))
	}

	// Stdin piped with no path: searches stdin, line numbers OFF unless -n.
	out, _, err = runAgentScript(t, dir, `printf 'one\ntwo match\nthree\n' | rg match`)
	if agentExitCode(err) != 0 || out != "two match\n" {
		t.Fatalf("rg stdin got %q (code %d)", out, agentExitCode(err))
	}
	out, _, err = runAgentScript(t, dir, `printf 'one\ntwo match\nthree\n' | rg -n match`)
	if agentExitCode(err) != 0 || out != "2:two match\n" {
		t.Fatalf("rg -n stdin got %q (code %d)", out, agentExitCode(err))
	}

	// Exit codes: 1 on no match, 2 on invalid regex.
	out, stderr, err = runAgentScript(t, dir, `rg NonExistentSymbolXYZ`)
	if agentExitCode(err) != 1 || out != "" || stderr != "" {
		t.Fatalf("rg no-match want exit 1 empty output, got code=%d out=%q stderr=%q", agentExitCode(err), out, stderr)
	}
	_, stderr, err = runAgentScript(t, dir, `rg '['`)
	if agentExitCode(err) != 2 || stderr == "" {
		t.Fatalf("rg bad regex want exit 2 + stderr, got code=%d stderr=%q", agentExitCode(err), stderr)
	}
}

func TestRgGitignoreAndHiddenBinary(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, ".gitignore"), []byte("*.log\n!important.log\nbuild/\n/root_only.txt\ndocs/**/*.tmp\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, ".ignore"), []byte("extra_ignored.txt\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "app.log"), []byte("NEEDLE app log\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "important.log"), []byte("NEEDLE important log\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "build", "out.go"), []byte("NEEDLE build out\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "root_only.txt"), []byte("NEEDLE root only\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "sub", "root_only.txt"), []byte("NEEDLE in sub allowed\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "sub", ".gitignore"), []byte("secret_*.txt\n!secret_public.txt\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "sub", "secret_priv.txt"), []byte("NEEDLE priv\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "sub", "secret_public.txt"), []byte("NEEDLE pub\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "docs", "a", "b", "draft.tmp"), []byte("NEEDLE draft\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "extra_ignored.txt"), []byte("NEEDLE extra ignored\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, ".hidden.txt"), []byte("NEEDLE hidden\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, ".git", "config"), []byte("NEEDLE gitconfig\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "bin.dat"), []byte("NEEDLE binary\x00tail\n"), 0o644)

	// Default search respects hierarchical .gitignore + negation + .ignore, skips hidden & .git & binary.
	out, _, err := runAgentScript(t, dir, `rg -l NEEDLE`)
	if agentExitCode(err) != 0 {
		t.Fatalf("rg -l NEEDLE failed: %v", err)
	}
	wantFiles := "important.log\nsub/root_only.txt\nsub/secret_public.txt\n"
	if out != wantFiles {
		t.Fatalf("rg -l NEEDLE got:\n%q\nwant:\n%q", out, wantFiles)
	}

	// --hidden includes dotfiles (.hidden.txt) while still skipping .git/ and .gitignore rules.
	out, _, err = runAgentScript(t, dir, `rg --hidden -l NEEDLE`)
	if agentExitCode(err) != 0 {
		t.Fatalf("rg --hidden failed: %v", err)
	}
	wantHidden := ".hidden.txt\nimportant.log\nsub/root_only.txt\nsub/secret_public.txt\n"
	if out != wantHidden {
		t.Fatalf("rg --hidden got:\n%q\nwant:\n%q", out, wantHidden)
	}

	// -a / --text searches binary files too.
	out, _, err = runAgentScript(t, dir, `rg -a -l NEEDLE bin.dat`)
	if agentExitCode(err) != 0 || out != "bin.dat\n" {
		t.Fatalf("rg -a bin.dat got %q (code %d)", out, agentExitCode(err))
	}

	// -uuu implies --no-ignore --hidden --text (still skips .git directory unless explicitly targeted).
	out, _, err = runAgentScript(t, dir, `rg -uuu -l NEEDLE`)
	if agentExitCode(err) != 0 {
		t.Fatalf("rg -uuu failed: %v", err)
	}
	for _, mustHave := range []string{"app.log", "bin.dat", "build/out.go", ".hidden.txt", "extra_ignored.txt", "sub/secret_priv.txt"} {
		if !strings.Contains(out, mustHave+"\n") {
			t.Errorf("rg -uuu missing %q in:\n%s", mustHave, out)
		}
	}
}

func TestRgFlagsTypesGlobsContextReplace(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, "main.go"), []byte(strings.Join([]string{
		"package main",
		"func helper() {",
		"    foo.bar(baz)[0]",
		"    FooCaseCheck()",
		"    word wholeword word_ext",
		"    user_id=42",
		"}",
	}, "\n")+"\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "main_gen.go"), []byte("package main\n// foo.bar(baz)[0]\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "script.py"), []byte("def run():\n    return 'word'\n"), 0o644)
	writeAgentFile(t, filepath.Join(dir, "web.js"), []byte("console.log('word');\n"), 0o644)

	// -F fixed-strings with glob exclusion -g '!*_gen.go'
	out, _, err := runAgentScript(t, dir, `rg -F 'foo.bar(baz)[0]' -g '!*_gen.go'`)
	if agentExitCode(err) != 0 || out != "main.go:3:    foo.bar(baz)[0]\n" {
		t.Fatalf("rg -F -g got %q (code %d)", out, agentExitCode(err))
	}

	// -S smart-case: all-lowercase matches case-insensitively; uppercase stays case-sensitive.
	out, _, err = runAgentScript(t, dir, `rg -S foocasecheck main.go`)
	if agentExitCode(err) != 0 || out != "4:    FooCaseCheck()\n" {
		t.Fatalf("rg -S lowercase got %q", out)
	}
	out, _, err = runAgentScript(t, dir, `rg -S FooCASECheck main.go`)
	if agentExitCode(err) != 1 || out != "" {
		t.Fatalf("rg -S uppercase mismatch got %q (code %d)", out, agentExitCode(err))
	}

	// -w word-regexp + -t type / -T type-not
	out, _, err = runAgentScript(t, dir, `rg -w word -t py -t js -T js`)
	if agentExitCode(err) != 0 || out != "script.py:2:    return 'word'\n" {
		t.Fatalf("rg -w -t/-T got %q (code %d)", out, agentExitCode(err))
	}

	// -C 1 context lines
	out, _, err = runAgentScript(t, dir, `rg -C 1 FooCaseCheck main.go`)
	wantCtx := "3-    foo.bar(baz)[0]\n4:    FooCaseCheck()\n5-    word wholeword word_ext\n"
	if agentExitCode(err) != 0 || out != wantCtx {
		t.Fatalf("rg -C 1 got:\n%q\nwant:\n%q", out, wantCtx)
	}

	// --replace / -r with $1 and $0 capture replacement + -o only-matching
	out, _, err = runAgentScript(t, dir, `rg -o 'user_id=([0-9]+)' -r 'uid:$1 (raw:$0)' main.go`)
	if agentExitCode(err) != 0 || out != "6:uid:42 (raw:user_id=42)\n" {
		t.Fatalf("rg -o -r got %q (code %d)", out, agentExitCode(err))
	}

	// --vimgrep & --files
	out, _, err = runAgentScript(t, dir, `rg --vimgrep FooCaseCheck main.go`)
	if agentExitCode(err) != 0 || out != "main.go:4:5:    FooCaseCheck()\n" {
		t.Fatalf("rg --vimgrep got %q (code %d)", out, agentExitCode(err))
	}
	out, _, err = runAgentScript(t, dir, `rg --files -t go -g '!*_gen.go'`)
	if agentExitCode(err) != 0 || out != "main.go\n" {
		t.Fatalf("rg --files got %q (code %d)", out, agentExitCode(err))
	}
}

// ============================================================================
// 2. patch tests
// ============================================================================

func TestPatchMultiFileStripReverseDryRun(t *testing.T) {
	dir := t.TempDir()
	origMod := "line1\nold_value\nline3\n"
	origDel := "obsolete1\nobsolete2\n"
	writeAgentFile(t, filepath.Join(dir, "pkg", "mod.txt"), []byte(origMod), 0o755)
	writeAgentFile(t, filepath.Join(dir, "pkg", "remove.txt"), []byte(origDel), 0o644)

	diffText := strings.Join([]string{
		"diff --git a/pkg/mod.txt b/pkg/mod.txt",
		"--- a/pkg/mod.txt",
		"+++ b/pkg/mod.txt",
		"@@ -1,3 +1,4 @@",
		" line1",
		"-old_value",
		"+new_value",
		"+extra_line",
		" line3",
		"diff --git a/pkg/added.txt b/pkg/added.txt",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/pkg/added.txt",
		"@@ -0,0 +1,2 @@",
		"+created_1",
		"+created_2",
		"diff --git a/pkg/remove.txt b/pkg/remove.txt",
		"deleted file mode 100644",
		"--- a/pkg/remove.txt",
		"+++ /dev/null",
		"@@ -1,2 +0,0 @@",
		"-obsolete1",
		"-obsolete2",
		"",
	}, "\n")
	writeAgentFile(t, filepath.Join(dir, "changes.patch"), []byte(diffText), 0o644)

	// 1. --dry-run leaves everything untouched.
	_, stderr, err := runAgentScript(t, dir, `patch -p1 --dry-run < changes.patch`)
	if agentExitCode(err) != 0 {
		t.Fatalf("patch --dry-run failed: code=%d stderr=%q", agentExitCode(err), stderr)
	}
	gotMod, _ := os.ReadFile(filepath.Join(dir, "pkg", "mod.txt"))
	if string(gotMod) != origMod {
		t.Fatalf("dry-run mutated mod.txt: %q", string(gotMod))
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg", "added.txt")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created added.txt")
	}

	// 2. Apply multi-file patch -p1.
	_, stderr, err = runAgentScript(t, dir, `patch -p1 -s -i changes.patch`)
	if agentExitCode(err) != 0 {
		t.Fatalf("patch -p1 apply failed: code=%d stderr=%q", agentExitCode(err), stderr)
	}
	gotMod, _ = os.ReadFile(filepath.Join(dir, "pkg", "mod.txt"))
	if string(gotMod) != "line1\nnew_value\nextra_line\nline3\n" {
		t.Fatalf("mod.txt after patch = %q", string(gotMod))
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, "pkg", "mod.txt"))
		if err != nil || fi.Mode().Perm() != 0o755 {
			t.Fatalf("mod.txt permissions not preserved: %v", fi.Mode().Perm())
		}
	}
	gotAdded, err := os.ReadFile(filepath.Join(dir, "pkg", "added.txt"))
	if err != nil || string(gotAdded) != "created_1\ncreated_2\n" {
		t.Fatalf("added.txt = %q, err=%v", string(gotAdded), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg", "remove.txt")); !os.IsNotExist(err) {
		t.Fatalf("remove.txt should have been deleted")
	}

	// 3. -N (--forward) refuses to re-apply an already-applied patch and exits 1.
	_, _, err = runAgentScript(t, dir, `patch -p1 -N < changes.patch`)
	if agentExitCode(err) != 1 {
		t.Fatalf("patch -N on already-applied patch want exit 1, got %d", agentExitCode(err))
	}

	// 4. -R (--reverse) restores original state cleanly!
	_, stderr, err = runAgentScript(t, dir, `patch -p1 -R -s < changes.patch`)
	if agentExitCode(err) != 0 {
		t.Fatalf("patch -p1 -R failed: code=%d stderr=%q", agentExitCode(err), stderr)
	}
	gotMod, _ = os.ReadFile(filepath.Join(dir, "pkg", "mod.txt"))
	if string(gotMod) != origMod {
		t.Fatalf("mod.txt after -R = %q, want %q", string(gotMod), origMod)
	}
	gotDel, err := os.ReadFile(filepath.Join(dir, "pkg", "remove.txt"))
	if err != nil || string(gotDel) != origDel {
		t.Fatalf("remove.txt after -R = %q, err=%v", string(gotDel), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg", "added.txt")); !os.IsNotExist(err) {
		t.Fatalf("added.txt should be removed after -R")
	}
}

func TestPatchFuzzyOffsetCRLFAndRejectExitCode(t *testing.T) {
	dir := t.TempDir()
	// Target file has 4 prepended lines (offset +4) AND CRLF line endings!
	driftedCRLF := strings.Join([]string{
		"header_a",
		"header_b",
		"header_c",
		"header_d",
		"ctx_before",
		"target_old",
		"ctx_after",
	}, "\r\n") + "\r\n"
	writeAgentFile(t, filepath.Join(dir, "crlf.txt"), []byte(driftedCRLF), 0o644)

	patchContent := strings.Join([]string{
		"--- a/crlf.txt",
		"+++ b/crlf.txt",
		"@@ -1,3 +1,3 @@",
		" ctx_before",
		"-target_old",
		"+target_new",
		" ctx_after",
		"",
	}, "\n")
	writeAgentFile(t, filepath.Join(dir, "drift.patch"), []byte(patchContent), 0o644)

	_, stderr, err := runAgentScript(t, dir, `patch -p1 -b < drift.patch`)
	if agentExitCode(err) != 0 {
		t.Fatalf("fuzzy CRLF patch failed: code=%d stderr=%q", agentExitCode(err), stderr)
	}
	updated, _ := os.ReadFile(filepath.Join(dir, "crlf.txt"))
	if !strings.Contains(string(updated), "target_new\r\n") {
		t.Fatalf("expected CRLF preserved with target_new, got %q", string(updated))
	}
	if _, err := os.Stat(filepath.Join(dir, "crlf.txt.orig")); err != nil {
		t.Fatalf("expected backup crlf.txt.orig from -b: %v", err)
	}

	// Conflicting hunk must fail with exit status 1.
	badPatch := strings.Join([]string{
		"--- a/crlf.txt",
		"+++ b/crlf.txt",
		"@@ -1,2 +1,2 @@",
		"-completely_missing_line",
		"+replacement",
		" nope",
		"",
	}, "\n")
	writeAgentFile(t, filepath.Join(dir, "bad.patch"), []byte(badPatch), 0o644)
	_, _, err = runAgentScript(t, dir, `patch -p1 < bad.patch`)
	if agentExitCode(err) != 1 {
		t.Fatalf("failing hunk want exit 1, got %d", agentExitCode(err))
	}
}

// ============================================================================
// 3. bc tests
// ============================================================================

func TestBcArbitraryPrecisionMathlibBasesAndLang(t *testing.T) {
	dir := t.TempDir()

	// 1. 2^256 exact integer + scale division.
	out, stderr, err := runAgentScript(t, dir, `printf '2^256\nscale=10; 22/7\n' | bc`)
	if agentExitCode(err) != 0 || stderr != "" {
		t.Fatalf("bc basic failed: code=%d stderr=%q", agentExitCode(err), stderr)
	}
	want256 := "115792089237316195423570985008687907853269984665640564039457584007913129639936\n3.1428571428\n"
	if out != want256 {
		t.Fatalf("bc 2^256 got:\n%q\nwant:\n%q", out, want256)
	}

	// 2. bc -l high-precision 50-digit pi = 4*a(1), plus s(0), c(0), l(e(1)), sqrt(2).
	out, stderr, err = runAgentScript(t, dir, `printf 'scale=50; 4*a(1)\nscale=10; s(0)\nc(0)\nl(e(1))\nsqrt(2)\n' | bc -l`)
	if agentExitCode(err) != 0 || stderr != "" {
		t.Fatalf("bc -l failed: code=%d stderr=%q", agentExitCode(err), stderr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 {
		t.Fatalf("bc -l expected 5 lines, got %d: %q", len(lines), out)
	}
	wantPiPrefix := "3.141592653589793238462643383279502884197169399375"
	if !strings.HasPrefix(lines[0], wantPiPrefix) {
		t.Fatalf("bc -l 50-digit pi = %q, want prefix %q", lines[0], wantPiPrefix)
	}
	if lines[1] != "0" && !strings.HasPrefix(lines[1], "0.000000000") {
		t.Fatalf("s(0) = %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "1.000000000") && lines[2] != "1" {
		t.Fatalf("c(0) = %q", lines[2])
	}
	if !strings.HasPrefix(lines[3], "1.000000000") && !strings.HasPrefix(lines[3], ".999999999") {
		t.Fatalf("l(e(1)) = %q", lines[3])
	}
	if lines[4] != "1.4142135623" {
		t.Fatalf("sqrt(2) = %q, want 1.4142135623", lines[4])
	}

	// 3. ibase / obase hex & binary conversions + POSIX ordering semantics
	// (note: once ibase=16, '10' is 16 in hex so 'ibase=A' resets ibase to decimal ten!).
	out, stderr, err = runAgentScript(t, dir, `printf 'obase=2; 42\nobase=10; ibase=16; FF\nibase=A; obase=16; 255\n' | bc`)
	if agentExitCode(err) != 0 || stderr != "" {
		t.Fatalf("bc bases failed: code=%d stderr=%q", agentExitCode(err), stderr)
	}
	if out != "101010\n255\nFF\n" {
		t.Fatalf("bc bases got %q, want %q", out, "101010\n255\nFF\n")
	}

	// 4. Functions (define / auto / return), arrays, loops, break/continue, comments, last (.).
	bcProg := `
/* factorial + array accumulation */
define fact(n) {
    auto r, i
    if (n <= 1) return (1)
    r = 1
    for (i = 2; i <= n; i++) {
        r *= i
    }
    return (r)
}
# store into array
arr[3] = fact(6)
arr[3] + 80
. + 1
quit
ignored_after_quit
`
	writeAgentFile(t, filepath.Join(dir, "prog.bc"), []byte(bcProg), 0o644)
	out, stderr, err = runAgentScript(t, dir, `bc -q prog.bc`)
	if agentExitCode(err) != 0 || stderr != "" || out != "800\n801\n" {
		t.Fatalf("bc lang got out=%q stderr=%q code=%d", out, stderr, agentExitCode(err))
	}
}

// ============================================================================
// 4. sponge tests
// ============================================================================

func TestSpongeAtomicInPlaceAndAppend(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		if i%2 == 0 {
			fmt.Fprintf(&sb, "KEEP line %04d padding_payload_abcdefghijklmnopqrstuvwxyz\n", i)
		} else {
			fmt.Fprintf(&sb, "DROP line %04d padding_payload_abcdefghijklmnopqrstuvwxyz\n", i)
		}
	}
	target := filepath.Join(dir, "data.txt")
	writeAgentFile(t, target, []byte(sb.String()), 0o751)

	// Pipeline reading and overwriting the exact same large file safely!
	_, stderr, err := runAgentScript(t, dir, `rg '^KEEP' data.txt -N | sponge data.txt`)
	if agentExitCode(err) != 0 || stderr != "" {
		t.Fatalf("sponge pipeline failed: code=%d stderr=%q", agentExitCode(err), stderr)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(lines) != 1000 || !strings.HasPrefix(lines[0], "KEEP line 0000") || !strings.HasPrefix(lines[999], "KEEP line 1998") {
		t.Fatalf("sponge in-place corrupted data: lines=%d", len(lines))
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(target)
		if err != nil || fi.Mode().Perm() != 0o751 {
			t.Fatalf("sponge did not preserve perms: %v", fi.Mode().Perm())
		}
	}

	// -a append mode + stdout mode when no file given.
	_, stderr, err = runAgentScript(t, dir, `printf 'FINAL_LINE\n' | sponge -a data.txt`)
	if agentExitCode(err) != 0 || stderr != "" {
		t.Fatalf("sponge -a failed: code=%d stderr=%q", agentExitCode(err), stderr)
	}
	got2, _ := os.ReadFile(target)
	if !strings.HasSuffix(string(got2), "FINAL_LINE\n") {
		t.Fatalf("sponge -a missing appended line")
	}

	out, _, err := runAgentScript(t, dir, `printf 'hello sponge\n' | sponge`)
	if agentExitCode(err) != 0 || out != "hello sponge\n" {
		t.Fatalf("sponge stdout got %q (code %d)", out, agentExitCode(err))
	}
}

// ============================================================================
// 5. flock tests
// ============================================================================

func TestFlockContentionTimeoutSharedAndFD(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "job.lock")

	// Hold an exclusive lock directly via openFlockFile + tryFlockFile, then verify
	// concurrent `unish` flock invocations contend, fail fast with -n / -E, timeout with -w,
	// and succeed immediately once the holder unlocks!
	holder, err := openFlockFile(lockFile)
	if err != nil {
		t.Fatalf("openFlockFile: %v", err)
	}
	defer holder.Close()

	locked, err := tryFlockFile(holder, false)
	if err != nil || !locked {
		t.Fatalf("initial exclusive lock failed: locked=%v err=%v", locked, err)
	}

	// 1. -n non-blocking contention exits 1 (and custom -E 42 exits 42).
	out, _, err := runAgentScript(t, dir, `flock -n job.lock echo should_not_run`)
	if agentExitCode(err) != 1 || out != "" {
		t.Fatalf("flock -n under contention want exit 1 empty out, got code=%d out=%q", agentExitCode(err), out)
	}
	out, _, err = runAgentScript(t, dir, `flock -n -E 42 job.lock -c 'echo should_not_run'`)
	if agentExitCode(err) != 42 || out != "" {
		t.Fatalf("flock -n -E 42 want exit 42, got code=%d out=%q", agentExitCode(err), out)
	}

	// 2. -w short timeout expires while lock remains held.
	start := time.Now()
	out, _, err = runAgentScript(t, dir, `flock -w 0.08 -E 75 job.lock echo nope`)
	elapsed := time.Since(start)
	if agentExitCode(err) != 75 || out != "" || elapsed < 50*time.Millisecond {
		t.Fatalf("flock -w 0.08 want exit 75 after ~80ms, got code=%d elapsed=%v out=%q", agentExitCode(err), elapsed, out)
	}

	// 3. Release lock asynchronously after 100ms while `flock -w 3` waits; must acquire and run `-c` script!
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = unlockFlockFile(holder)
	}()
	out, stderr, err := runAgentScript(t, dir, `flock -w 3 job.lock -c 'echo acquired_ok'`)
	if agentExitCode(err) != 0 || out != "acquired_ok\n" {
		t.Fatalf("flock -w 3 after release failed: code=%d out=%q stderr=%q", agentExitCode(err), out, stderr)
	}

	// 4. Shared (-s) locks coexist with each other, while exclusive (-x -n) is rejected!
	shLocked, err := tryFlockFile(holder, true)
	if err != nil || !shLocked {
		t.Fatalf("shared lock failed: %v", err)
	}
	out, _, err = runAgentScript(t, dir, `flock -s -n job.lock echo shared_ok`)
	if agentExitCode(err) != 0 || out != "shared_ok\n" {
		t.Fatalf("shared + shared want exit 0, got code=%d out=%q", agentExitCode(err), out)
	}
	out, _, err = runAgentScript(t, dir, `flock -x -n job.lock echo excl_fail`)
	if agentExitCode(err) != 1 || out != "" {
		t.Fatalf("shared + exclusive want exit 1, got code=%d out=%q", agentExitCode(err), out)
	}
	_ = unlockFlockFile(holder)

	// 5. FD form: `( flock -n 0; echo fd_locked ) < job.lock`
	out, stderr, err = runAgentScript(t, dir, `( flock -n 0 && echo fd_locked && flock -u 0 ) < job.lock`)
	if agentExitCode(err) != 0 || out != "fd_locked\n" {
		t.Fatalf("flock FD form failed: code=%d out=%q stderr=%q", agentExitCode(err), out, stderr)
	}
}

// ============================================================================
// 6. column tests
// ============================================================================

func TestColumnTableUTF8AndFillMode(t *testing.T) {
	dir := t.TempDir()
	// Mix ASCII, accented UTF-8 ("café" = 4 cells, 5 bytes), CJK ("日本語" = 6 cells, 9 bytes), emoji ("🚀" = 2 cells).
	input := strings.Join([]string{
		"NAME:CITY:ROLE",
		"Alice:Tokyo:Eng",
		"日本語:café:Ops",
		"🚀:NY:QA",
	}, "\n") + "\n"
	writeAgentFile(t, filepath.Join(dir, "table.txt"), []byte(input), 0o644)

	out, stderr, err := runAgentScript(t, dir, `column -t -s : -o ' | ' table.txt`)
	if agentExitCode(err) != 0 || stderr != "" {
		t.Fatalf("column -t failed: code=%d stderr=%q", agentExitCode(err), stderr)
	}
	// Max display widths: col0 = 6 ("日本語"), col1 = 5 ("Tokyo"), col2 = unpadded last col!
	want := strings.Join([]string{
		"NAME   | CITY  | ROLE",
		"Alice  | Tokyo | Eng",
		"日本語 | café  | Ops",
		"🚀     | NY    | QA",
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("column -t UTF-8 alignment mismatch:\ngot:\n%s\nwant:\n%s", out, want)
	}

	// -n preserves empty adjacent fields instead of merging delimiters.
	out, _, err = runAgentScript(t, dir, `printf 'a,,c\n1,22,3\n' | column -t -n -s , -o '|'`)
	wantNoMerge := "a|  |c\n1|22|3\n"
	if agentExitCode(err) != 0 || out != wantNoMerge {
		t.Fatalf("column -t -n got:\n%q\nwant:\n%q", out, wantNoMerge)
	}

	// Non-table fill mode (down columns by default, across rows with -x).
	out, _, err = runAgentScript(t, dir, `printf 'a\nb\nc\nd\n' | column -c 6`)
	if agentExitCode(err) != 0 || out != "a  c\nb  d\n" {
		t.Fatalf("column fill down got %q (code %d)", out, agentExitCode(err))
	}
	out, _, err = runAgentScript(t, dir, `printf 'a\nb\nc\nd\n' | column -x -c 6`)
	if agentExitCode(err) != 0 || out != "a  b\nc  d\n" {
		t.Fatalf("column -x fill across got %q (code %d)", out, agentExitCode(err))
	}
}

// ============================================================================
// 7. xxd tests
// ============================================================================

func TestXxdDumpFlagsAndBinaryRoundtrips(t *testing.T) {
	dir := t.TempDir()

	// Canonical single-line check + -u uppercase + -s seek + -l len.
	out, stderr, err := runAgentScript(t, dir, `printf 'Hello\n' | xxd`)
	if agentExitCode(err) != 0 || stderr != "" {
		t.Fatalf("xxd basic failed: code=%d stderr=%q", agentExitCode(err), stderr)
	}
	wantHello := "00000000: 4865 6c6c 6f0a                           Hello.\n"
	if out != wantHello {
		t.Fatalf("xxd Hello got:\n%q\nwant:\n%q", out, wantHello)
	}

	outU, _, err := runAgentScript(t, dir, `printf 'Hello\n' | xxd -u -g 1 -c 8 -s 1 -l 4`)
	wantU := "00000001: 65 6C 6C 6F              ello\n"
	if agentExitCode(err) != 0 || outU != wantU {
		t.Fatalf("xxd -u -g1 -c8 -s1 -l4 got:\n%q\nwant:\n%q", outU, wantU)
	}

	// Full 0x00..0xff binary payload (includes NULs and ASCII hex lookalikes "abcdef0123456789"!).
	raw := make([]byte, 512)
	for i := range raw {
		raw[i] = byte(i & 0xff)
	}
	copy(raw[32:], []byte("abcdef0123456789ABCDEF"))
	binPath := filepath.Join(dir, "allbytes.bin")
	writeAgentFile(t, binPath, raw, 0o644)

	// Roundtrip 1: canonical `xxd | xxd -r`
	_, stderr, err = runAgentScript(t, dir, `xxd allbytes.bin | xxd -r > rt_canon.bin`)
	if agentExitCode(err) != 0 || stderr != "" {
		t.Fatalf("xxd canonical roundtrip failed: code=%d stderr=%q", agentExitCode(err), stderr)
	}
	gotCanon, err := os.ReadFile(filepath.Join(dir, "rt_canon.bin"))
	if err != nil || !bytes.Equal(gotCanon, raw) {
		t.Fatalf("xxd canonical roundtrip mismatch: got %d bytes, want %d", len(gotCanon), len(raw))
	}

	// Roundtrip 2: plain `xxd -p | xxd -r -p` (and `-ps` / `-rp`)
	_, stderr, err = runAgentScript(t, dir, `xxd -ps allbytes.bin | xxd -rp - rt_plain.bin`)
	if agentExitCode(err) != 0 || stderr != "" {
		t.Fatalf("xxd plain roundtrip failed: code=%d stderr=%q", agentExitCode(err), stderr)
	}
	gotPlain, err := os.ReadFile(filepath.Join(dir, "rt_plain.bin"))
	if err != nil || !bytes.Equal(gotPlain, raw) {
		t.Fatalf("xxd plain roundtrip mismatch: got %d bytes, want %d", len(gotPlain), len(raw))
	}

	// Negative seek `-s -4` from end of file.
	outTail, _, err := runAgentScript(t, dir, `xxd -p -s -4 allbytes.bin`)
	if agentExitCode(err) != 0 || outTail != "fcfdfeff\n" {
		t.Fatalf("xxd -p -s -4 got %q, want %q", outTail, "fcfdfeff\n")
	}
}

// ============================================================================
// 8. Cancellation & EPIPE tests
// ============================================================================

func TestAgentToolsCancelAndEPIPE(t *testing.T) {
	dir := t.TempDir()
	var big strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&big, "match_row_%04d col_b col_c\n", i)
	}
	writeAgentFile(t, filepath.Join(dir, "big.txt"), []byte(big.String()), 0o644)

	// Downstream `head -n 1` closes pipe early: rg, column, xxd, and streaming bc must exit 0 with zero stderr noise!
	for _, pipeline := range []string{
		`rg match_row big.txt | head -n 1`,
		`column -t big.txt | head -n 1`,
		`xxd big.txt | head -n 1`,
		`yes '2+2' | bc | head -n 1`,
	} {
		out, stderr, err := runAgentScript(t, dir, pipeline)
		if agentExitCode(err) != 0 || stderr != "" || strings.Count(out, "\n") != 1 {
			t.Fatalf("EPIPE pipeline %q failed: code=%d stderr=%q out=%q", pipeline, agentExitCode(err), stderr, out)
		}
	}

	// Unbounded producer -> streaming rg stdin -> head -n 2 must stream immediately and exit 0 without buffering until EOF!
	for _, tc := range []struct {
		script string
		want   string
	}{
		{`yes NEEDLE | rg NEEDLE | head -n 2`, "NEEDLE\nNEEDLE\n"},
		{`yes NEEDLE | rg -n NEEDLE - /dev/null | head -n 2`, "<stdin>:1:NEEDLE\n<stdin>:2:NEEDLE\n"},
		{`yes NEEDLE | rg --passthru OTHER | head -n 2`, "NEEDLE\nNEEDLE\n"},
	} {
		outS, stderrS, errS := runAgentScript(t, dir, tc.script)
		if agentExitCode(errS) != 0 || stderrS != "" || outS != tc.want {
			t.Fatalf("streaming stdin %q failed: code=%d stderr=%q got=%q want=%q", tc.script, agentExitCode(errS), stderrS, outS, tc.want)
		}
	}

	// /dev/null cross-platform safety (including classic `rg pat file /dev/null` filename trick on Windows!)
	outDevNull, stderrDN, errDN := runAgentScript(t, dir, `
		rg match_row_0000 big.txt /dev/null
		printf 'discard\n' | sponge /dev/null
		column -t /dev/null
		xxd /dev/null
		printf '2+2\n' | bc /dev/null
	`)
	if errDN != nil || stderrDN != "" {
		t.Fatalf("/dev/null safety failed: err=%v stderr=%q", errDN, stderrDN)
	}
	if outDevNull != "big.txt:1:match_row_0000 col_b col_c\n4\n" {
		t.Fatalf("/dev/null output mismatch: got %q", outDevNull)
	}

	// Context cancellation stops an infinite `bc` loop promptly!
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := runAgentScriptCtx(t, ctx, 5*time.Second, dir, `printf 'while (1) { x = x + 1 }\n' | bc`)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("bc infinite loop did not respect ctx cancellation: err=%v elapsed=%v", err, time.Since(start))
	}
}
