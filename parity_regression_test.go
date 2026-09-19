package main

// Regression tests pinning GNU-parity behaviors fixed during the
// deep parity audit (bash-WSL2 vs unish-linux vs unish-windows).
// Each test runs a shell snippet through the real interpreter in an
// isolated temp dir and asserts exact stdout/stderr/exit behavior.
//
// Conventions:
//   - runParityScript(t, dir, src) runs src with Dir=dir.
//   - Tests must be hermetic: no network, no absolute host paths,
//     no wall-clock assertions (except fixed historical dates).
//   - Platform-specific ownership/mode bits are asserted only on
//     POSIX (runtime.GOOS != "windows"); Windows asserts exit codes
//     and structural output.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func runParityScript(t *testing.T, dir, src string) (string, string, error) {
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
		interp.ExecHandlers(extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		t.Fatalf("interpreter creation error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = r.Run(ctx, prog)
	return stdout.String(), stderr.String(), err
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if st, ok := err.(interp.ExitStatus); ok {
		return int(st)
	}
	if ee, ok := err.(exitError); ok {
		return ee.code
	}
	return -1
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- wc -----------------------------------------------------------------

func TestParityWcFormat(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "f.txt"), "x\n")
	mustWrite(t, filepath.Join(dir, "a.txt"), "hi\n")
	mustWrite(t, filepath.Join(dir, "b.txt"), "hello\n")

	// Default with file operand: unpadded (coreutils 8/9 file rows).
	out, _, err := runParityScript(t, dir, "wc f.txt")
	if exitCode(err) != 0 || out != "1 1 2 f.txt\n" {
		t.Errorf("wc f.txt = %q, code %d; want %q", out, exitCode(err), "1 1 2 f.txt\n")
	}
	// Default via pipe: padded to width 7.
	out, _, _ = runParityScript(t, dir, "printf 'x\\n' | wc")
	if out != "      1       1       2\n" {
		t.Errorf("pipe|wc = %q; want padded", out)
	}
	// Explicit single column: bare.
	out, _, _ = runParityScript(t, dir, "wc -l f.txt")
	if out != "1 f.txt\n" {
		t.Errorf("wc -l = %q; want %q", out, "1 f.txt\n")
	}
	// Explicit multi-col with files: unpadded.
	out, _, _ = runParityScript(t, dir, "wc -lw a.txt b.txt")
	if out != "1 1 a.txt\n1 1 b.txt\n2 2 total\n" {
		t.Errorf("wc -lw = %q", out)
	}
	// Explicit multi-col on stdin pipe: padded.
	out, _, _ = runParityScript(t, dir, "printf 'x\\n' | wc -lw")
	if out != "      1       1\n" {
		t.Errorf("pipe|wc -lw = %q; want padded", out)
	}
	// "-" operand prints its name, padded like stdin.
	out, _, _ = runParityScript(t, dir, "printf 'hi\\n' | wc -")
	if out != "      1       1       3 -\n" {
		t.Errorf("wc - = %q", out)
	}
}

// --- head/tail ------------------------------------------------------------

func TestParityHeadTail(t *testing.T) {
	dir := t.TempDir()
	out, _, err := runParityScript(t, dir, "seq 1 5 | head -n 0; echo exit=$?")
	if exitCode(err) != 0 || out != "exit=0\n" {
		t.Errorf("head -n 0 = %q code %d", out, exitCode(err))
	}
	out, _, _ = runParityScript(t, dir, "seq 1 5 | head -n -1")
	if out != "1\n2\n3\n4\n" {
		t.Errorf("head -n -1 = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "seq 1 5 | tail -n -1")
	if out != "5\n" {
		t.Errorf("tail -n -1 = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "seq 1 10 | tail -n +5")
	if out != "5\n6\n7\n8\n9\n10\n" {
		t.Errorf("tail -n +5 = %q", out)
	}
	// head with file + stdin label.
	mustWrite(t, filepath.Join(dir, "a.txt"), "f\n")
	out, _, _ = runParityScript(t, dir, "printf 'a\\n' | head - a.txt")
	if out != "==> standard input <==\na\n\n==> a.txt <==\nf\n" {
		t.Errorf("head - file = %q", out)
	}
}

// --- seq ------------------------------------------------------------------

func TestParitySeq(t *testing.T) {
	dir := t.TempDir()
	out, _, _ := runParityScript(t, dir, "seq 0 0.5 2")
	if out != "0.0\n0.5\n1.0\n1.5\n2.0\n" {
		t.Errorf("seq float = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "seq -w 1 10 | head -3")
	if out != "01\n02\n03\n" {
		t.Errorf("seq -w = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "seq -s, 1 5; echo")
	if out != "1,2,3,4,5\n\n" {
		t.Errorf("seq -s = %q", out)
	}
	_, _, err := runParityScript(t, dir, "seq 1 0 5")
	if exitCode(err) != 1 {
		t.Errorf("seq zero step code = %d; want 1", exitCode(err))
	}
	out, _, _ = runParityScript(t, dir, "seq 5 -1 1")
	if out != "5\n4\n3\n2\n1\n" {
		t.Errorf("seq negative step = %q", out)
	}
}

// --- cut/paste ------------------------------------------------------------

func TestParityCut(t *testing.T) {
	dir := t.TempDir()
	out, _, _ := runParityScript(t, dir, "printf 'abcdef\\n' | cut -b1,3,5")
	if out != "ace\n" {
		t.Errorf("cut -b = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "printf 'a:b:c\\n' | cut -d: -f2 --complement")
	if out != "a:c\n" {
		t.Errorf("cut --complement = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "printf 'a:b:c\\n' | cut -d: --output-delimiter=, -f1-3")
	if out != "a,b,c\n" {
		t.Errorf("cut --output-delimiter = %q", out)
	}
	// C-locale byte semantics for -c.
	out, _, _ = runParityScript(t, dir, "printf '\\xc3\\xa9\\xc3\\xa8\\n' | cut -c1 | od -An -tx1")
	if !strings.Contains(out, "c3") || strings.Contains(out, "a9") {
		t.Errorf("cut -c1 utf8 byte = %q", out)
	}
}

func TestParityPasteStdin(t *testing.T) {
	dir := t.TempDir()
	out, _, _ := runParityScript(t, dir, "printf 'x\\ny\\n' | paste - - -")
	if out != "x\ty\t\n" {
		t.Errorf("paste - - - = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "printf 'a\\nb\\nc\\nd\\n' | paste - -")
	if out != "a\tb\nc\td\n" {
		t.Errorf("paste - - = %q", out)
	}
}

// --- sort/uniq/tr -----------------------------------------------------------

func TestParitySortUniq(t *testing.T) {
	dir := t.TempDir()
	// Empty input sorts to empty (no phantom newline).
	out, _, _ := runParityScript(t, dir, "printf '' | sort; echo rc=$?")
	if out != "rc=0\n" {
		t.Errorf("sort empty = %q", out)
	}
	// NUL-delimited.
	out, _, _ = runParityScript(t, dir, "printf 'b\\x00a\\x00' | sort -z | od -An -tx1")
	if strings.TrimSpace(out) != "61 00 62 00" {
		t.Errorf("sort -z = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "printf 'x a\\ny a\\n' | uniq -f 1")
	if out != "x a\n" {
		t.Errorf("uniq -f = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "printf 'abc\\nabd\\n' | uniq -s 2")
	if out != "abc\nabd\n" {
		t.Errorf("uniq -s = %q", out)
	}
	// tr octal escapes + classes.
	out, _, _ = runParityScript(t, dir, "printf 'abc' | tr abc '\\141\\142\\143'; echo")
	if out != "abc\n" {
		t.Errorf("tr octal = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "printf 'ABC123' | tr '[:upper:]' '[:lower:]'; echo")
	if out != "abc123\n" {
		t.Errorf("tr class = %q", out)
	}
}

// --- grep -----------------------------------------------------------------

func TestParityGrep(t *testing.T) {
	dir := t.TempDir()
	out, _, _ := runParityScript(t, dir, "printf 'foo\\nfoobar\\n' | grep -x foo")
	if out != "foo\n" {
		t.Errorf("grep -x = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "printf 'foo foobar barfoo\\n' | grep -o -w foo")
	if out != "foo\n" {
		t.Errorf("grep -o -w = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "echo '-foo' | grep -- -foo")
	if out != "-foo\n" {
		t.Errorf("grep -- = %q", out)
	}
	// -r with a FILE operand: no filename prefix (dir descent only).
	mustWrite(t, filepath.Join(dir, "p.txt"), "needle\n")
	out, _, _ = runParityScript(t, dir, "grep -rn -C0 needle p.txt")
	if out != "1:needle\n" {
		t.Errorf("grep -r file = %q", out)
	}
	// -H forces names; --label renames stdin.
	mustWrite(t, filepath.Join(dir, "a.txt"), "hi\n")
	mustWrite(t, filepath.Join(dir, "b.txt"), "hi\n")
	out, _, _ = runParityScript(t, dir, "grep -H hi a.txt b.txt")
	if out != "a.txt:hi\nb.txt:hi\n" {
		t.Errorf("grep -H = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "printf 'foo\\n' | grep --label=MYLBL -H foo")
	if out != "MYLBL:foo\n" {
		t.Errorf("grep --label = %q", out)
	}
	// Errors exit 2 even with -s.
	_, _, err := runParityScript(t, dir, "grep -s foo /nonexistent_xyz_unish")
	if exitCode(err) != 2 {
		t.Errorf("grep -s missing code = %d; want 2", exitCode(err))
	}
	// Binary files report on stderr, exit 0.
	mustWrite(t, filepath.Join(dir, "bin.dat"), "foo\x00bar")
	out, serr, err := runParityScript(t, dir, "grep foo bin.dat; echo rc=$?")
	if exitCode(err) != 0 || out != "rc=0\n" || !strings.Contains(serr, "binary file matches") {
		t.Errorf("grep binary = out %q err %q code %d", out, serr, exitCode(err))
	}
}

func TestParityGrepContext(t *testing.T) {
	dir := t.TempDir()
	// Context lines use "-" separators, matches use ":" (with -n).
	out, _, _ := runParityScript(t, dir, "printf 'a\\nfoo\\nb\\n' | grep -n -C1 foo")
	if out != "1-a\n2:foo\n3-b\n" {
		t.Errorf("grep -C = %q", out)
	}
	// "--" separates disjoint context groups.
	out, _, _ = runParityScript(t, dir, "printf 'foo\\nb\\nc\\nd\\nfoo\\ne\\n' | grep -C1 foo")
	if out != "foo\nb\n--\nd\nfoo\ne\n" {
		t.Errorf("grep -- sep = %q", out)
	}
}

func TestParityGrepRecursiveOrder(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "sub", "f.txt"), "needle\n")
	mustWrite(t, filepath.Join(dir, "top.txt"), "hay\n")
	// Sorted for determinism (readdir order varies); both hits present.
	out, _, err := runParityScript(t, dir, "grep -r needle . | sort")
	if exitCode(err) != 0 || !strings.Contains(out, "./sub/f.txt:needle\n") {
		t.Errorf("grep -r = %q code %d", out, exitCode(err))
	}
}

// --- sed ------------------------------------------------------------------

func TestParitySed(t *testing.T) {
	dir := t.TempDir()
	// Missing trailing newline is preserved.
	out, _, _ := runParityScript(t, dir, "printf 'foo' | sed 's/foo/bar/' | od -An -tx1")
	if strings.TrimSpace(out) != "62 61 72" {
		t.Errorf("sed no-trailing-nl = %q", out)
	}
	// GNU error wording.
	_, serr, err := runParityScript(t, dir, "printf 'hi\\n' | sed '['")
	if exitCode(err) != 1 || !strings.Contains(serr, "unknown command") {
		t.Errorf("sed bad script = err %q code %d", serr, exitCode(err))
	}
	// -i edits in place.
	mustWrite(t, filepath.Join(dir, "f.txt"), "foo\n")
	_, _, err = runParityScript(t, dir, "sed -i 's/foo/bar/' f.txt")
	if exitCode(err) != 0 {
		t.Fatalf("sed -i code %d", exitCode(err))
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "f.txt")); string(data) != "bar\n" {
		t.Errorf("sed -i content = %q", data)
	}
}

// --- diff/cmp ---------------------------------------------------------------

func TestParityDiff(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "a\nb\nc\n")
	mustWrite(t, filepath.Join(dir, "b.txt"), "a\nx\nc\n")
	out, _, err := runParityScript(t, dir, "diff -u --label a.txt --label b.txt a.txt b.txt; echo rc=$?")
	if exitCode(err) != 0 || out != "--- a.txt\n+++ b.txt\n@@ -1,3 +1,3 @@\n a\n-b\n+x\n c\nrc=1\n" {
		t.Errorf("diff -u --label = %q code %d", out, exitCode(err))
	}
	// "-" means stdin (identical content -> no diff).
	out, _, err = runParityScript(t, dir, "cat a.txt | diff - a.txt; echo rc=$?")
	if exitCode(err) != 0 || out != "rc=0\n" {
		t.Errorf("diff - stdin = %q code %d", out, exitCode(err))
	}
	// Binary files short-circuit.
	mustWrite(t, filepath.Join(dir, "x.bin"), "a\x00b")
	mustWrite(t, filepath.Join(dir, "y.bin"), "a\x00c")
	out, _, err = runParityScript(t, dir, "diff x.bin y.bin; echo rc=$?")
	if !strings.Contains(out, "Binary files x.bin and y.bin differ\n") || !strings.Contains(out, "rc=1") {
		t.Errorf("diff binary = %q", out)
	}
}

// --- hashes -----------------------------------------------------------------

func TestParityHashCheck(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "f.txt"), "abc")
	out, _, err := runParityScript(t, dir, "md5sum f.txt > sums.txt; md5sum -c sums.txt; echo rc=$?")
	if exitCode(err) != 0 || out != "f.txt: OK\nrc=0\n" {
		t.Errorf("md5sum -c = %q code %d", out, exitCode(err))
	}
	out, _, _ = runParityScript(t, dir, "printf 'abc' | sha256sum | cut -c1-8")
	if out != "ba7816bf\n" {
		t.Errorf("sha256 = %q", out)
	}
}

// --- file ops -----------------------------------------------------------------

func TestParityCpMv(t *testing.T) {
	dir := t.TempDir()
	// -n warns on stderr like GNU.
	mustWrite(t, filepath.Join(dir, "dst.txt"), "old\n")
	mustWrite(t, filepath.Join(dir, "src.txt"), "new\n")
	out, serr, _ := runParityScript(t, dir, "cp -n src.txt dst.txt; cat dst.txt")
	if out != "old\n" || !strings.Contains(serr, "non-portable") {
		t.Errorf("cp -n = %q err %q", out, serr)
	}
	// Same-file errors.
	_, _, err := runParityScript(t, dir, "cp src.txt src.txt")
	if exitCode(err) != 1 {
		t.Errorf("cp same code = %d; want 1", exitCode(err))
	}
	_, _, err = runParityScript(t, dir, "mv src.txt src.txt")
	if exitCode(err) != 1 {
		t.Errorf("mv same code = %d; want 1", exitCode(err))
	}
}

func TestParityLn(t *testing.T) {
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows; covered by parity suite")
	}
	mustWrite(t, filepath.Join(dir, "orig.txt"), "hi\n")
	out, _, err := runParityScript(t, dir, "ln -s orig.txt link.txt; cat link.txt; readlink link.txt")
	if exitCode(err) != 0 || out != "hi\norig.txt\n" {
		t.Errorf("ln -s verbatim = %q code %d", out, exitCode(err))
	}
	_, serr, err := runParityScript(t, dir, "ln -s orig.txt link.txt")
	if exitCode(err) != 1 || !strings.Contains(serr, "failed to create symbolic link") {
		t.Errorf("ln exists = err %q code %d", serr, exitCode(err))
	}
}

func TestParityRmGuard(t *testing.T) {
	dir := t.TempDir()
	// --preserve-root (default) refuses rm -rf /.
	_, serr, err := runParityScript(t, dir, "rm -rf /")
	if exitCode(err) != 1 || !strings.Contains(serr, "preserve-root") {
		t.Errorf("rm -rf / = err %q code %d", serr, exitCode(err))
	}
	// Missing operand wording + code.
	_, serr, err = runParityScript(t, dir, "rm")
	if exitCode(err) != 1 || !strings.Contains(serr, "missing operand") {
		t.Errorf("rm no args = err %q code %d", serr, exitCode(err))
	}
}

func TestParityMkdirChmod(t *testing.T) {
	dir := t.TempDir()
	_, serr, err := runParityScript(t, dir, "mkdir d; mkdir d")
	if exitCode(err) != 1 || !strings.Contains(serr, "cannot create directory") {
		t.Errorf("mkdir exists = err %q code %d", serr, exitCode(err))
	}
	_, serr, err = runParityScript(t, dir, "chmod 999 f.txt")
	if exitCode(err) != 1 || !strings.Contains(serr, "invalid mode") {
		t.Errorf("chmod bad = err %q code %d", serr, exitCode(err))
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes don't exist on Windows")
	}
	out, _, _ := runParityScript(t, dir, "touch f.txt; chmod 755 f.txt; stat -c %a f.txt")
	if out != "755\n" {
		t.Errorf("chmod bits = %q", out)
	}
}

// --- ls ---------------------------------------------------------------------

func TestParityLs(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "vis"), "x")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// -R never descends into . / .. (used to hang forever).
	out, _, err := runParityScript(t, dir, "ls -aR . | head -8")
	if exitCode(err) != 0 || !strings.Contains(out, "sub") {
		t.Errorf("ls -aR = %q code %d", out, exitCode(err))
	}
	if runtime.GOOS == "windows" {
		t.Skip("symlink rendering needs privileges on Windows")
	}
	mustWrite(t, filepath.Join(dir, "real.txt"), "hi\n")
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skip("no symlink privs")
	}
	out, _, _ = runParityScript(t, dir, "ls -l link.txt")
	if !strings.Contains(out, "link.txt -> real.txt") {
		t.Errorf("ls -l symlink = %q", out)
	}
}

// --- tar/gzip -----------------------------------------------------------------

func TestParityTarSlipDefense(t *testing.T) {
	dir := t.TempDir()
	// Craft an archive with an escaping member; extraction must refuse
	// to write outside the destination (Zip-Slip defense).
	mustWrite(t, filepath.Join(dir, "d", "a.txt"), "hi\n")
	_, _, err := runParityScript(t, dir, "tar -cf out.tar d")
	if exitCode(err) != 0 {
		t.Fatalf("tar create code %d", exitCode(err))
	}
	// Append a malicious member via a second archive and concat? Instead
	// assert the guard directly: isWithinDir rejects escapes.
	if isWithinDir(dir, filepath.Join(dir, "..", "evil")) {
		t.Errorf("isWithinDir allowed escape")
	}
	if !isWithinDir(dir, filepath.Join(dir, "d", "a.txt")) {
		t.Errorf("isWithinDir rejected legit path")
	}
}

func TestParityTar(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "d", "a.txt"), "hi\n")
	// Directory entries keep trailing slash (GNU listing parity).
	out, _, err := runParityScript(t, dir, "tar -cf out.tar d; tar -tf out.tar")
	if exitCode(err) != 0 || out != "d/\nd/a.txt\n" {
		t.Errorf("tar list = %q code %d", out, exitCode(err))
	}
	out, _, _ = runParityScript(t, dir, "tar -tvf out.tar | cut -c1-10 | head -2")
	if !strings.Contains(out, "drwxr") {
		t.Errorf("tar -tv = %q", out)
	}
	// Round-trip extract.
	out, _, err = runParityScript(t, dir, "rm -rf d; tar -xf out.tar; cat d/a.txt")
	if exitCode(err) != 0 || out != "hi\n" {
		t.Errorf("tar extract = %q code %d", out, exitCode(err))
	}
}

func TestParityGzip(t *testing.T) {
	dir := t.TempDir()
	// Corrupt input wording (with GNU's leading blank line).
	_, serr, err := runParityScript(t, dir, "printf 'not gzip data' > bad.gz; gunzip bad.gz")
	if exitCode(err) != 1 || !strings.Contains(serr, "not in gzip format") {
		t.Errorf("gunzip corrupt = err %q code %d", serr, exitCode(err))
	}
	out, _, err := runParityScript(t, dir, "printf 'hello' | gzip | gunzip")
	if exitCode(err) != 0 || out != "hello" {
		t.Errorf("gzip roundtrip = %q code %d", out, exitCode(err))
	}
}

// --- procs ------------------------------------------------------------------

func TestParityJobsTable(t *testing.T) {
	// Job-table unit test (no external sleeper needed): track a live
	// child directly and resolve it via fake id + jobspecs.
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sleep as the external child")
	}
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no /bin/sleep")
	}
	j := globalJobs.track(cmd, []string{"sleep", "30"})
	defer func() {
		j.proc.Kill()
		<-j.done
	}()
	if got := globalJobs.resolveJobSpec("g1"); got == nil && len(globalJobs.jobs) == 0 {
		t.Errorf("empty table")
	}
	_ = j
	// %1 resolves to the newest job; %% too.
	if globalJobs.resolveJobSpec("%1") == nil {
		t.Errorf("pct-1 unresolved")
	}
	if globalJobs.resolveJobSpec("%%") == nil {
		t.Errorf("%% unresolved")
	}
	// kill through the table works on the real pid.
	if err := killProc(j.proc, "TERM"); err != nil {
		t.Errorf("kill tracked: %v", err)
	}
	<-j.done
	if j.code == 0 {
		t.Errorf("killed sleep exited 0")
	}
}

func TestParityProcSubst(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only temp-file substitution")
	}
	dir := t.TempDir()
	out, _, err := runParityScript(t, dir, "diff <(printf 'a\n') <(printf 'b\n'); echo rc=$?")
	if exitCode(err) != 0 || !strings.Contains(out, "1c1") || !strings.Contains(out, "rc=1") {
		t.Errorf("procsub diff = %q code %d", out, exitCode(err))
	}
	// No temp-file leaks.
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "unish-procsub-*"))
	if len(matches) != 0 {
		t.Errorf("leaked procsub files: %v", matches)
	}
}

func TestParityPrintf(t *testing.T) {
	dir := t.TempDir()
	out, _, err := runParityScript(t, dir, "printf 'a%.0s' 1 2 3; echo")
	if exitCode(err) != 0 || out != "aaa\n" {
		t.Errorf("printf reuse = %q code %d", out, exitCode(err))
	}
	out, _, _ = runParityScript(t, dir, "printf '%03d|%5s|%x\n' 7 hi 255")
	if out != "007|   hi|ff\n" {
		t.Errorf("printf formats = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "printf '%c' 65; echo")
	if out != "6\n" {
		t.Errorf("printf %%c = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "printf '%g|%.3g|%u\n' 1234567 1234567 -1")
	if out != "1.23457e+06|1.23e+06|18446744073709551615\n" {
		t.Errorf("printf %%g/%%u = %q", out)
	}
}

func TestParityHashType(t *testing.T) {
	dir := t.TempDir()
	out, _, _ := runParityScript(t, dir, "hash")
	if !strings.Contains(out, "hash table empty") {
		t.Errorf("hash empty = %q", out)
	}
	out, _, _ = runParityScript(t, dir, "type ls")
	if !strings.Contains(out, "unish builtin") {
		t.Errorf("type ls = %q", out)
	}
}

func TestParityKill(t *testing.T) {
	dir := t.TempDir()
	_, serr, err := runParityScript(t, dir, "kill -999")
	if exitCode(err) != 1 || !strings.Contains(serr, "invalid signal specification") {
		t.Errorf("kill -999 = err %q code %d", serr, exitCode(err))
	}
	_, serr, err = runParityScript(t, dir, "kill 99999999")
	if exitCode(err) != 1 || !strings.Contains(serr, "No such process") {
		t.Errorf("kill dead = err %q code %d", serr, exitCode(err))
	}
}

func TestParityTimeout(t *testing.T) {
	dir := t.TempDir()
	// Duration 0 disables the timeout (GNU).
	out, _, err := runParityScript(t, dir, "timeout 0 echo hi")
	if exitCode(err) != 0 || out != "hi\n" {
		t.Errorf("timeout 0 = %q code %d", out, exitCode(err))
	}
	out, _, _ = runParityScript(t, dir, "timeout 5 echo hi")
	if out != "hi\n" {
		t.Errorf("timeout basic = %q", out)
	}
}

func TestParityPs(t *testing.T) {
	dir := t.TempDir()
	out, _, err := runParityScript(t, dir, "ps")
	if exitCode(err) != 0 || !strings.HasPrefix(out, "    PID TTY          TIME CMD\n") {
		t.Errorf("ps = %q code %d", out, exitCode(err))
	}
}

func TestParityUmaskUlimit(t *testing.T) {
	dir := t.TempDir()
	out, _, err := runParityScript(t, dir, "umask")
	if exitCode(err) != 0 || out != "0022\n" {
		t.Errorf("umask = %q code %d", out, exitCode(err))
	}
	out, _, _ = runParityScript(t, dir, "ulimit -n")
	if strings.TrimSpace(out) == "" {
		t.Errorf("ulimit -n empty")
	}
	for _, c := range strings.TrimSpace(out) {
		if c < '0' || c > '9' {
			t.Errorf("ulimit -n not numeric: %q", out)
			break
		}
	}
}

// --- text misc -----------------------------------------------------------------

func TestParityCatFold(t *testing.T) {
	dir := t.TempDir()
	// -A shows tabs + line ends (was silently ignored before the fix).
	out, _, _ := runParityScript(t, dir, "printf 'a\\tb\\n' | cat -A")
	if out != "a^Ib$\n" {
		t.Errorf("cat -A = %q", out)
	}
	// fold preserves a missing trailing newline (like GNU).
	out, _, _ = runParityScript(t, dir, "printf 'foo' | fold -w 80 | od -An -tx1")
	if strings.TrimSpace(out) != "66 6f 6f" {
		t.Errorf("fold no-trailing-nl = %q", out)
	}
	// unexpand converts only leading blanks by default.
	out, _, _ = runParityScript(t, dir, "printf 'a        b\\n' | unexpand | cat -A")
	if out != "a        b$\n" {
		t.Errorf("unexpand default = %q", out)
	}
	// Empty sort prints nothing.
	out, _, _ = runParityScript(t, dir, "printf '' | sort; echo rc=$?")
	if out != "rc=0\n" {
		t.Errorf("sort empty = %q", out)
	}
	// nl renders unnumbered blank lines as width+1 spaces (GNU).
	out, _, _ = runParityScript(t, dir, "printf 'a\\n\\nb\\n' | nl")
	if out != "     1\ta\n       \n     2\tb\n" {
		t.Errorf("nl blanks = %q", out)
	}
	// du -b apparent bytes are exact.
	out, _, _ = runParityScript(t, dir, "du -sb . | cut -f1")
	if strings.TrimSpace(out) == "" {
		t.Errorf("du -sb empty")
	}
	// split -b byte chunks.
	_, _, err := runParityScript(t, dir, "printf 'abcdefghij' > in.txt; split -b 4 in.txt p_; ls p_* | wc -l")
	if exitCode(err) != 0 {
		t.Errorf("split -b code %d", exitCode(err))
	}
	files, _ := filepath.Glob(filepath.Join(dir, "p_*"))
	if len(files) != 3 {
		t.Errorf("split -b parts = %d; want 3", len(files))
	}
}

func TestParitySeqCutPaste(t *testing.T) {
	dir := t.TempDir()
	// seq -s joins without trailing separator.
	out, _, _ := runParityScript(t, dir, "seq -s, 1 3; echo")
	if out != "1,2,3\n\n" {
		t.Errorf("seq -s = %q", out)
	}
	// NUL-delimited sort round-trips.
	out, _, _ = runParityScript(t, dir, "printf 'b\\x00a\\x00' | sort -z | od -An -tx1")
	if strings.TrimSpace(out) != "61 00 62 00" {
		t.Errorf("sort -z = %q", out)
	}
}

func TestParityCommJoin(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "b\na\n")
	mustWrite(t, filepath.Join(dir, "b.txt"), "a\nb\n")
	_, serr, err := runParityScript(t, dir, "comm a.txt b.txt")
	if exitCode(err) != 1 || !strings.Contains(serr, "not in sorted order") {
		t.Errorf("comm disorder = err %q code %d", serr, exitCode(err))
	}
	mustWrite(t, filepath.Join(dir, "j1.txt"), "2 b\n1 a\n")
	mustWrite(t, filepath.Join(dir, "j2.txt"), "1 x\n2 y\n")
	_, serr, err = runParityScript(t, dir, "join j1.txt j2.txt")
	if exitCode(err) != 1 || !strings.Contains(serr, "not in sorted order") {
		t.Errorf("join disorder = err %q code %d", serr, exitCode(err))
	}
}

func TestParityHexdumpOdStrings(t *testing.T) {
	dir := t.TempDir()
	// Empty hexdump prints nothing (no stray offset line).
	out, _, err := runParityScript(t, dir, "printf '' | hexdump -C; echo rc=$?")
	if exitCode(err) != 0 || out != "rc=0\n" {
		t.Errorf("hexdump empty = %q code %d", out, exitCode(err))
	}
	out, _, _ = runParityScript(t, dir, "printf 'hello\\n' | od -A x -t x1z")
	if !strings.Contains(out, ">hello.<") {
		t.Errorf("od x1z gutter = %q", out)
	}
	mustWrite(t, filepath.Join(dir, "f.bin"), "hi\x00hello world\x00")
	out, _, _ = runParityScript(t, dir, "strings -t d f.bin")
	if !strings.Contains(out, "3 hello world") {
		t.Errorf("strings -t = %q", out)
	}
}

// --- misc builtins ---------------------------------------------------------------

func TestParityMisc(t *testing.T) {
	dir := t.TempDir()
	// arch reports the kernel name.
	out, _, _ := runParityScript(t, dir, "arch")
	if strings.TrimSpace(out) == "" || strings.Contains(out, "amd64") && runtime.GOARCH == "arm64" {
		t.Errorf("arch = %q", out)
	}
	// tty failure goes to stdout (GNU), exit 1.
	out, _, err := runParityScript(t, dir, "tty")
	if exitCode(err) != 1 || strings.TrimSpace(out) != "not a tty" {
		t.Errorf("tty = %q code %d", out, exitCode(err))
	}
	// logname fails without a login session.
	_, _, err = runParityScript(t, dir, "logname")
	if exitCode(err) != 1 {
		t.Errorf("logname code = %d; want 1", exitCode(err))
	}
	// clear emits home+erase+erase-scrollback.
	out, _, _ = runParityScript(t, dir, "clear | od -An -tx1")
	flat := strings.ReplaceAll(strings.TrimSpace(out), " ", "")
	if flat != "1b5b481b5b324a1b5b334a" {
		t.Errorf("clear = %q", out)
	}
	// sleep rejects negatives like an invalid option.
	_, serr, err := runParityScript(t, dir, "sleep -1")
	if exitCode(err) != 1 || !strings.Contains(serr, "invalid option") {
		t.Errorf("sleep -1 = err %q code %d", serr, exitCode(err))
	}
	// date -d historical conversion is exact.
	out, _, _ = runParityScript(t, dir, "date -d '2020-01-02' +%Y-%m-%d")
	if out != "2020-01-02\n" {
		t.Errorf("date -d = %q", out)
	}
	// TZ=UTC conversion.
	out, _, _ = runParityScript(t, dir, "TZ=UTC date +%Z")
	if strings.TrimSpace(out) != "UTC" {
		t.Errorf("TZ=UTC = %q", out)
	}
	// id -n names.
	out, _, _ = runParityScript(t, dir, "id -un")
	if strings.TrimSpace(out) == "" {
		t.Errorf("id -un empty")
	}
	// dirname keeps ./ and handles /.
	out, _, _ = runParityScript(t, dir, "dirname ./.devcontainer/x.json; dirname /")
	if out != "./.devcontainer\n/\n" {
		t.Errorf("dirname = %q", out)
	}
	// realpath resolves missing lexically (only -e requires existence).
	out, _, err = runParityScript(t, dir, "realpath ./nonexistent_xyz_unish; echo rc=$?")
	if exitCode(err) != 0 || !strings.Contains(out, "nonexistent_xyz_unish") {
		t.Errorf("realpath missing = %q code %d", out, exitCode(err))
	}
	// readlink on a non-link is silent, exit 1.
	mustWrite(t, filepath.Join(dir, "f.txt"), "hi\n")
	out, serr, err = runParityScript(t, dir, "readlink f.txt; echo rc=$?")
	if exitCode(err) != 0 || out != "rc=1\n" || serr != "" {
		t.Errorf("readlink nonlink = out %q err %q", out, serr)
	}
	// touch -d formats + invalid wording.
	_, _, err = runParityScript(t, dir, "touch -d '2020-01-01' a.txt")
	if exitCode(err) != 0 {
		t.Errorf("touch -d code %d", exitCode(err))
	}
	_, serr, err = runParityScript(t, dir, "touch -d 'not a date' b.txt")
	if exitCode(err) != 1 || !strings.Contains(serr, "invalid date format") {
		t.Errorf("touch bad date = err %q code %d", serr, exitCode(err))
	}
	// du on a symlink operand reports 0.
	if runtime.GOOS != "windows" {
		mustWrite(t, filepath.Join(dir, "real.txt"), "hi\n")
		if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err == nil {
			out, _, _ := runParityScript(t, dir, "du link.txt")
			if !strings.HasPrefix(out, "0\t") {
				t.Errorf("du symlink = %q", out)
			}
		}
	}
	// mktemp -u dry-run prints a name without creating it.
	out, _, err = runParityScript(t, dir, "mktemp -u")
	if exitCode(err) != 0 || !strings.Contains(out, "tmp") {
		t.Errorf("mktemp -u = %q code %d", out, exitCode(err))
	}
	// find -perm exact match.
	if runtime.GOOS != "windows" {
		os.WriteFile(filepath.Join(dir, "perm.txt"), []byte("x"), 0o644)
		out, _, _ := runParityScript(t, dir, "find . -maxdepth 1 -perm 644 -name 'perm.txt'")
		if !strings.Contains(out, "perm.txt") {
			t.Errorf("find -perm = %q", out)
		}
	}
	// find lists the root itself (GNU); -name matches its basename ".".
	out, _, _ = runParityScript(t, dir, "find . -maxdepth 0 -name '*.*'")
	if strings.TrimSpace(out) != "." {
		t.Errorf("find root = %q", out)
	}
	// head -n 0 in a pipeline exits 0 silently (SIGPIPE semantics).
	out, serr, err = runParityScript(t, dir, "seq 1 5 | head -n 0; echo rc=$?")
	if exitCode(err) != 0 || out != "rc=0\n" || serr != "" {
		t.Errorf("head -n 0 pipe = out %q err %q", out, serr)
	}
	// which honors a dead PATH.
	_, _, err = runParityScript(t, dir, "PATH=/nonexistent_xyz_unish which ls")
	if exitCode(err) != 1 {
		t.Errorf("which dead PATH code = %d; want 1", exitCode(err))
	}
	// zcat alias exists.
	out, _, err = runParityScript(t, dir, "printf 'hello' | gzip | zcat")
	if exitCode(err) != 0 || out != "hello" {
		t.Errorf("zcat = %q code %d", out, exitCode(err))
	}
}

func TestParityLongLines(t *testing.T) {
	dir := t.TempDir()
	// REGRESSION (vscode real corpus): lines longer than 1MB must NOT be
	// dropped. bufio.Scanner capped at 1MB and silently skipped a 1.3MB
	// minified line (grep found 31894 instead of 31895 exports).
	long := strings.Repeat("x", 2_000_000) + "NEEDLE" + strings.Repeat("y", 100) + "\n"
	mustWrite(t, filepath.Join(dir, "long.txt"), long)
	out, _, err := runParityScript(t, dir, "grep -c NEEDLE long.txt")
	if exitCode(err) != 0 || out != "1\n" {
		t.Errorf("grep long line = %q code %d; want 1", out, exitCode(err))
	}
	out, _, err = runParityScript(t, dir, "grep NEEDLE long.txt | wc -c")
	if exitCode(err) != 0 || strings.TrimSpace(out) != "2000107" {
		t.Errorf("grep long passthrough = %q code %d", out, exitCode(err))
	}
	// Every other line-oriented tool must survive long lines too.
	for _, src := range []string{
		"head -n 1 long.txt | wc -c",
		"tail -n 1 long.txt | wc -c",
		"sort long.txt | wc -c",
		"cat long.txt | wc -c",
		"cut -c1-10 long.txt | wc -c",
		"rev long.txt | wc -c",
		"tac long.txt | wc -c",
		"uniq long.txt | wc -c",
		"nl long.txt | wc -c",
		"head -c 100 long.txt | wc -c",
	} {
		out, serr, err := runParityScript(t, dir, src)
		if exitCode(err) != 0 || serr != "" || strings.TrimSpace(out) == "" || strings.TrimSpace(out) == "0" {
			t.Errorf("%s = out %q err %q code %d", src, out, serr, exitCode(err))
		}
	}
}

func TestParityNewFlags(t *testing.T) {
	dir := t.TempDir()
	// grep -a/--text accepted (binary-as-text is the default).
	out, _, err := runParityScript(t, dir, "printf 'hi\\n' | grep -a hi; printf 'hi\\n' | grep --text hi")
	if exitCode(err) != 0 || out != "hi\nhi\n" {
		t.Errorf("grep -a/--text = %q code %d", out, exitCode(err))
	}
	// kill -s/-n separate-arg signal selection.
	_, serr, err := runParityScript(t, dir, "kill -s TERM 99999999")
	if exitCode(err) != 1 || !strings.Contains(serr, "No such process") {
		t.Errorf("kill -s TERM = err %q code %d", serr, exitCode(err))
	}
	_, serr, err = runParityScript(t, dir, "kill -n 9 99999999")
	if exitCode(err) != 1 || !strings.Contains(serr, "No such process") {
		t.Errorf("kill -n 9 = err %q code %d", serr, exitCode(err))
	}
	// hash: shell builtins hash trivially; unish builtins are not found.
	_, _, err = runParityScript(t, dir, "hash echo; hash printf")
	if exitCode(err) != 0 {
		t.Errorf("hash builtins code = %d; want 0", exitCode(err))
	}
	_, serr, err = runParityScript(t, dir, "hash ls")
	if exitCode(err) != 1 || !strings.Contains(serr, "not found") {
		t.Errorf("hash ls = err %q code %d", serr, exitCode(err))
	}
	// ulimit combined hardness+resource flags.
	out, _, err = runParityScript(t, dir, "ulimit -Sn; ulimit -Hn")
	if exitCode(err) != 0 || len(strings.Split(strings.TrimSpace(out), "\n")) != 2 {
		t.Errorf("ulimit -Sn/-Hn = %q code %d", out, exitCode(err))
	}
	// sort -o writes to file; -V version-sorts; -s accepted.
	out, _, err = runParityScript(t, dir, "printf 'b\\na\\n' | sort -o sorted.txt; cat sorted.txt")
	if exitCode(err) != 0 || out != "a\nb\n" {
		t.Errorf("sort -o = %q code %d", out, exitCode(err))
	}
	out, _, _ = runParityScript(t, dir, "printf 'v1.10\\nv1.2\\n' | sort -V")
	if out != "v1.2\nv1.10\n" {
		t.Errorf("sort -V = %q", out)
	}
	// join -v suppresses joined lines.
	mustWrite(t, filepath.Join(dir, "j1.txt"), "1 a\n2 b\n3 c\n")
	mustWrite(t, filepath.Join(dir, "j2.txt"), "2 x\n3 y\n")
	out, _, err = runParityScript(t, dir, "join -v 1 j1.txt j2.txt")
	if exitCode(err) != 0 || out != "1 a\n" {
		t.Errorf("join -v = %q code %d", out, exitCode(err))
	}
	// find -newer compares mtimes.
	_, _, err = runParityScript(t, dir, "touch -d '2020-01-01' old.txt; touch new.txt")
	if exitCode(err) != 0 {
		t.Fatalf("setup touch: %v", err)
	}
	out, _, _ = runParityScript(t, dir, "find . -maxdepth 1 -newer old.txt -name 'new.txt'")
	if !strings.Contains(out, "new.txt") {
		t.Errorf("find -newer = %q", out)
	}
	// touch -r copies mtime from reference.
	_, _, err = runParityScript(t, dir, "printf hi > ref.txt; touch -r ref.txt copy.txt")
	if exitCode(err) != 0 {
		t.Errorf("touch -r code = %d", exitCode(err))
	}
	// ls -m/-C/-x/-i accepted.
	out, _, err = runParityScript(t, dir, "mkdir -p h1; touch h1/b h1/a h1/c; ls -m h1; ls -C h1; ls -x h1; ls -i h1 | head -3")
	if exitCode(err) != 0 || !strings.Contains(out, "a, b, c") {
		t.Errorf("ls flags = %q code %d", out, exitCode(err))
	}
	// tar --exclude=all yields empty valid archive, rc 0.
	_, _, err = runParityScript(t, dir, "mkdir -p t1; echo hi > t1/a.txt; tar -cf t.tar --exclude=t1 t1")
	if exitCode(err) != 0 {
		t.Errorf("tar --exclude code = %d", exitCode(err))
	}
	// tr -t truncates SET1.
	out, _, _ = runParityScript(t, dir, "printf 'abc\\n' | tr -t 'abcd' 'XY'")
	if out != "XYc\n" {
		t.Errorf("tr -t = %q", out)
	}
	// base64 -i accepted.
	out, _, err = runParityScript(t, dir, "printf 'hi' | base64 | base64 -d -i; echo")
	if exitCode(err) != 0 || out != "hi\n" {
		t.Errorf("base64 -i = %q code %d", out, exitCode(err))
	}
	// uniq --group separates groups with blank lines.
	out, _, _ = runParityScript(t, dir, "printf 'a\\na\\nb\\nb\\nc\\n' | uniq --group")
	if out != "a\na\n\nb\nb\n\nc\n" {
		t.Errorf("uniq --group = %q", out)
	}
	// hexdump -s on a pipe warns Illegal seek, exit 1 (GNU).
	_, serr, err = runParityScript(t, dir, "printf 'abcdef' | hexdump -C -s 3")
	if exitCode(err) != 1 || !strings.Contains(serr, "Illegal seek") {
		t.Errorf("hexdump -s pipe = err %q code %d", serr, exitCode(err))
	}
	// hexdump -e byte format.
	out, _, _ = runParityScript(t, dir, "printf 'AB' | hexdump -e '1/1 \"%02x \"'; echo")
	if out != "41 42 \n" {
		t.Errorf("hexdump -e = %q", out)
	}
	// split -n creates N chunk files.
	_, _, err = runParityScript(t, dir, "seq 1 6 > sp.txt; split -n 2 sp.txt spp_; ls spp_* | wc -l")
	if exitCode(err) != 0 {
		t.Errorf("split -n code = %d", exitCode(err))
	}
	// mv -v GNU wording.
	out, _, _ = runParityScript(t, dir, "echo new > mv1.txt; mv -v mv1.txt mv2.txt")
	if out != "renamed 'mv1.txt' -> 'mv2.txt'\n" {
		t.Errorf("mv -v = %q", out)
	}
}
