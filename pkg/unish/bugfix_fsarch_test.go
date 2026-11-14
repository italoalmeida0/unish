package unish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func runFsArchScriptTimeout(t *testing.T, dir, src string, timeout time.Duration) (string, string, error) {
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
	err = r.Run(ctx, prog)
	return stdout.String(), stderr.String(), err
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// 1. cp -r copying a directory into itself / its own subdirectory must fail
// cleanly instead of recursing infinitely until ENAMETOOLONG / stack overflow.
func TestBugfixFsArch_CpDirIntoSelf(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "d", "a.txt"), "hello\n")

	// cp -r d d (existing dir -> target becomes d/d)
	_, serr, err := runFsArchScriptTimeout(t, dir, "cp -r d d", 3*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("cp -r d d succeeded; expected error")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "d", "d")); !os.IsNotExist(statErr) {
		t.Fatalf("cp -r d d created recursive subdir d/d (stderr=%q)", serr)
	}

	// cp -r d d/sub/nested (non-existent descendant inside d)
	_, serr, err = runFsArchScriptTimeout(t, dir, "cp -r d d/sub/nested", 3*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("cp -r d d/sub/nested succeeded; expected error")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "d", "sub")); !os.IsNotExist(statErr) {
		t.Fatalf("cp -r d d/sub/nested created d/sub (stderr=%q)", serr)
	}

	// cp -r . ./backup (backing up cwd into a child of cwd)
	subWork := filepath.Join(dir, "d")
	_, _, err = runFsArchScriptTimeout(t, subWork, "cp -r . ./backup", 3*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("cp -r . ./backup succeeded; expected error")
	}
}

// 2. cp symlink preservation (-d, -a, -r on dir trees with symlinks / broken symlinks)
// and protection against truncating source via nested symlink at destination.
func TestBugfixFsArch_CpSymlinksAndSelfTruncate(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "src", "real.txt"), "precious-content\n")
	symlinkOrSkip(t, "real.txt", filepath.Join(dir, "src", "rel_link.txt"))
	symlinkOrSkip(t, "does_not_exist", filepath.Join(dir, "src", "broken_link.txt"))
	if err := os.MkdirAll(filepath.Join(dir, "src", "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, "subdir", filepath.Join(dir, "src", "dir_link"))

	// cp -d preserves symlink on single-file copy.
	_, serr, err := runFsArchScriptTimeout(t, dir, "cp -d src/rel_link.txt copied_link.txt", 5*time.Second)
	if exitCode(err) != 0 {
		t.Fatalf("cp -d failed: code=%d stderr=%q", exitCode(err), serr)
	}
	if lfi, lerr := os.Lstat(filepath.Join(dir, "copied_link.txt")); lerr != nil || lfi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("cp -d did not preserve symlink: fi=%v err=%v", lfi, lerr)
	}

	// cp -r on directory containing valid, broken, and directory symlinks succeeds and preserves links.
	_, serr, err = runFsArchScriptTimeout(t, dir, "cp -r src dst", 5*time.Second)
	if exitCode(err) != 0 {
		t.Fatalf("cp -r with symlinks failed: code=%d stderr=%q", exitCode(err), serr)
	}
	for _, name := range []string{"rel_link.txt", "broken_link.txt", "dir_link"} {
		lfi, lerr := os.Lstat(filepath.Join(dir, "dst", name))
		if lerr != nil || lfi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("cp -r dst/%s not a symlink: fi=%v err=%v", name, lfi, lerr)
		}
	}

	// Nested destination symlink pointing back to source file must NOT truncate source to 0 bytes.
	if err := os.MkdirAll(filepath.Join(dir, "trap", "s"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "s", "victim.txt"), "DO_NOT_LOSE_ME\n")
	symlinkOrSkip(t, filepath.Join(dir, "s", "victim.txt"), filepath.Join(dir, "trap", "s", "victim.txt"))
	_, _, _ = runFsArchScriptTimeout(t, dir, "cp -r s trap", 5*time.Second)
	got, rerr := os.ReadFile(filepath.Join(dir, "s", "victim.txt"))
	if rerr != nil || string(got) != "DO_NOT_LOSE_ME\n" {
		t.Fatalf("source file truncated/corrupted! got=%q err=%v", string(got), rerr)
	}
}

// 3. mv data-loss & recursion hazards:
//   - mv -f missing.txt existing.txt must NOT delete existing.txt!
//   - mv d d and mv d d/sub must NOT infinitely recurse via copyDir!
//   - mv d1 d2 when d2/d1 is non-empty must fail without clobbering/merging d2/d1!
//   - mv foo.txt FOO.txt (case-only rename) must succeed on case-insensitive FS too!
func TestBugfixFsArch_MvSafetyAndCaseRename(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "precious.txt"), "keep-me-safe\n")

	// mv -f missing.txt precious.txt
	_, _, err := runFsArchScriptTimeout(t, dir, "mv -f missing.txt precious.txt", 5*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("mv -f missing.txt precious.txt unexpectedly succeeded")
	}
	got, rerr := os.ReadFile(filepath.Join(dir, "precious.txt"))
	if rerr != nil || string(got) != "keep-me-safe\n" {
		t.Fatalf("mv -f missing.txt precious.txt DELETED or corrupted precious.txt! got=%q err=%v", string(got), rerr)
	}

	// mv dir into itself
	mustWrite(t, filepath.Join(dir, "mdir", "f.txt"), "1\n")
	_, _, err = runFsArchScriptTimeout(t, dir, "mv mdir mdir", 3*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("mv mdir mdir succeeded; expected failure")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "mdir", "mdir")); !os.IsNotExist(statErr) {
		t.Fatalf("mv mdir mdir created recursive mdir/mdir")
	}
	_, _, err = runFsArchScriptTimeout(t, dir, "mv mdir mdir/sub", 3*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("mv mdir mdir/sub succeeded; expected failure")
	}

	// mv d1 d2 when d2/d1 is non-empty must fail and leave both intact.
	mustWrite(t, filepath.Join(dir, "d1", "src.txt"), "from-d1\n")
	mustWrite(t, filepath.Join(dir, "d2", "d1", "existing.txt"), "keep-d2\n")
	_, _, err = runFsArchScriptTimeout(t, dir, "mv d1 d2", 5*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("mv d1 d2 onto non-empty d2/d1 succeeded; expected failure")
	}
	if _, serr := os.Stat(filepath.Join(dir, "d1", "src.txt")); serr != nil {
		t.Fatalf("mv d1 d2 deleted source d1/src.txt despite failure: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(dir, "d2", "d1", "src.txt")); !os.IsNotExist(serr) {
		t.Fatalf("mv d1 d2 merged into non-empty d2/d1")
	}

	// Case-only rename: mv lower.txt LOWER.txt (and with -f) must work on all OSes.
	mustWrite(t, filepath.Join(dir, "lower.txt"), "casedata\n")
	_, serr, err := runFsArchScriptTimeout(t, dir, "mv -f lower.txt LOWER.txt", 5*time.Second)
	if exitCode(err) != 0 {
		t.Fatalf("mv -f lower.txt LOWER.txt failed: code=%d stderr=%q", exitCode(err), serr)
	}
	entries, _ := os.ReadDir(dir)
	foundUpper := false
	for _, e := range entries {
		if e.Name() == "LOWER.txt" {
			foundUpper = true
		}
	}
	if !foundUpper {
		t.Fatalf("LOWER.txt not found in dir entries after case rename")
	}
	got, rerr = os.ReadFile(filepath.Join(dir, "LOWER.txt"))
	if rerr != nil || string(got) != "casedata\n" {
		t.Fatalf("LOWER.txt content lost: got=%q err=%v", string(got), rerr)
	}
}

// 4. ln -f / ln -sf must not delete target when source is missing or identical to target.
func TestBugfixFsArch_LnForceSafety(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "keep.txt"), "important\n")

	// Hard link with missing source must NOT delete keep.txt.
	_, _, err := runFsArchScriptTimeout(t, dir, "ln -f does_not_exist.txt keep.txt", 5*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("ln -f missing keep.txt unexpectedly succeeded")
	}
	if got, rerr := os.ReadFile(filepath.Join(dir, "keep.txt")); rerr != nil || string(got) != "important\n" {
		t.Fatalf("ln -f missing keep.txt destroyed keep.txt: %q %v", string(got), rerr)
	}

	// Hard link onto itself (`ln -f keep.txt keep.txt` and `ln -f keep.txt .`) must NOT delete keep.txt.
	_, _, err = runFsArchScriptTimeout(t, dir, "ln -f keep.txt keep.txt", 5*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("ln -f keep.txt keep.txt unexpectedly succeeded")
	}
	if got, rerr := os.ReadFile(filepath.Join(dir, "keep.txt")); rerr != nil || string(got) != "important\n" {
		t.Fatalf("ln -f keep.txt keep.txt destroyed keep.txt: %q %v", string(got), rerr)
	}

	_, _, err = runFsArchScriptTimeout(t, dir, "ln -sf keep.txt .", 5*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("ln -sf keep.txt . unexpectedly succeeded")
	}
	if got, rerr := os.ReadFile(filepath.Join(dir, "keep.txt")); rerr != nil || string(got) != "important\n" {
		t.Fatalf("ln -sf keep.txt . destroyed keep.txt: %q %v", string(got), rerr)
	}
}

// 5. chmod symbolic modes starting with '-' (chmod -x, chmod -w) and chmod -R missing path.
func TestBugfixFsArch_ChmodMinusSymbolicAndRecursive(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "script.sh")
	mustWrite(t, f, "#!/bin/sh\n")
	_ = os.Chmod(f, 0o755)

	_, serr, err := runFsArchScriptTimeout(t, dir, "chmod -x script.sh", 5*time.Second)
	if exitCode(err) != 0 {
		t.Fatalf("chmod -x failed: code=%d stderr=%q", exitCode(err), serr)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(f)
		if fi.Mode().Perm()&0o111 != 0 {
			t.Fatalf("chmod -x left execute bits set: %04o", fi.Mode().Perm())
		}
	}

	// chmod -R on non-existent path must fail with non-zero exit code.
	_, _, err = runFsArchScriptTimeout(t, dir, "chmod -R 755 no_such_dir", 5*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("chmod -R 755 no_such_dir exited 0; want non-zero")
	}
}

// 6. xargs: -P concurrency without races, context cancellation stopping batches,
// quote/escape parsing, -d '\n', and builtin printf/test execution.
func TestBugfixFsArch_XargsConcurrencyCancelAndQuotes(t *testing.T) {
	dir := t.TempDir()

	// Quoted items & -d '\n' & builtin printf via xargs
	out, serr, err := runFsArchScriptTimeout(t, dir, `printf '"hello world" foo\\ bar\n' | xargs printf '[%s]\n'`, 5*time.Second)
	if exitCode(err) != 0 || out != "[hello world]\n[foo bar]\n" {
		t.Fatalf("xargs quoted + printf = %q (code=%d stderr=%q)", out, exitCode(err), serr)
	}

	out, serr, err = runFsArchScriptTimeout(t, dir, `printf 'a b\nc d\n' | xargs -d '\n' printf '<%s>\n'`, 5*time.Second)
	if exitCode(err) != 0 || out != "<a b>\n<c d>\n" {
		t.Fatalf("xargs -d '\\n' = %q (code=%d stderr=%q)", out, exitCode(err), serr)
	}

	// xargs -P 4 parallel execution: 4 x 250ms sleeps must finish well under 850ms
	// and concurrent echo writes must not race on stdout buffer.
	start := time.Now()
	_, serr, err = runFsArchScriptTimeout(t, dir, `printf '0.25\n0.25\n0.25\n0.25\n' | xargs -n1 -P4 sleep`, 5*time.Second)
	elapsed := time.Since(start)
	if exitCode(err) != 0 {
		t.Fatalf("xargs -P4 sleep failed: code=%d stderr=%q", exitCode(err), serr)
	}
	if elapsed >= 850*time.Millisecond {
		t.Fatalf("xargs -P4 ran serially in %v; expected parallel (~250ms)", elapsed)
	}

	out, serr, err = runFsArchScriptTimeout(t, dir, `seq 1 40 | xargs -n1 -P8 echo | wc -l`, 5*time.Second)
	if exitCode(err) != 0 || strings.TrimSpace(out) != "40" {
		t.Fatalf("xargs -P8 echo count = %q (code=%d stderr=%q)", out, exitCode(err), serr)
	}

	// Context cancellation stops xargs immediately instead of running remaining batches.
	start = time.Now()
	_, _, err = runFsArchScriptTimeout(t, dir, `seq 1 50 | xargs -n1 sleep`, 150*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("xargs ignored context cancellation, took %v", elapsed)
	}
	if err == nil {
		t.Fatalf("expected timeout error from canceled xargs")
	}
}

// 7. find: -mindepth gating actions (-print/-delete/-exec), -delete post-order on
// non-empty and empty dirs, -L symlink traversal with cycle detection, -exec exit
// status filtering & {} substring replacement & + batching, unmatched parens, and ctx cancel.
func TestBugfixFsArch_FindBugs(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "keep_root_marker"), "x\n")
	mustWrite(t, filepath.Join(dir, "work", "a.txt"), "needle\n")
	mustWrite(t, filepath.Join(dir, "work", "b.txt"), "other\n")
	mustWrite(t, filepath.Join(dir, "work", "sub", "nested.txt"), "z\n")
	if err := os.MkdirAll(filepath.Join(dir, "work", "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}

	// -mindepth 1 with explicit -print must NOT print the starting point itself.
	out, serr, err := runFsArchScriptTimeout(t, dir, `find work -mindepth 1 -maxdepth 1 -print | sort`, 5*time.Second)
	if exitCode(err) != 0 {
		t.Fatalf("find -mindepth 1 -print failed: code=%d stderr=%q", exitCode(err), serr)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "work" {
			t.Fatalf("find work -mindepth 1 -print included root 'work': %q", out)
		}
	}

	// -exec predicate boolean exit status + {} embedded in argument ({}.bak) + {} + batching.
	out, serr, err = runFsArchScriptTimeout(t, dir, `find work -type f -exec grep -q needle {} \; -print`, 5*time.Second)
	if exitCode(err) != 0 || strings.TrimSpace(out) != "work/a.txt" {
		t.Fatalf("find -exec grep -q filter = %q (code=%d stderr=%q); want work/a.txt", out, exitCode(err), serr)
	}
	_, serr, err = runFsArchScriptTimeout(t, dir, `find work -name 'a.txt' -exec cp {} {}.bak \;`, 5*time.Second)
	if exitCode(err) != 0 {
		t.Fatalf("find -exec cp {}.bak failed: %q", serr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "work", "a.txt.bak")); statErr != nil {
		t.Fatalf("work/a.txt.bak was not created ({} substring not substituted): %v", statErr)
	}
	_ = os.Remove(filepath.Join(dir, "work", "a.txt.bak"))

	out, serr, err = runFsArchScriptTimeout(t, dir, `find work -type f -exec echo BATCH {} +`, 5*time.Second)
	if exitCode(err) != 0 || strings.Count(strings.TrimSpace(out), "\n") != 0 || !strings.HasPrefix(out, "BATCH ") {
		t.Fatalf("find -exec + did not batch into single invocation: out=%q code=%d stderr=%q", out, exitCode(err), serr)
	}

	// Unterminated -exec (missing \; or +) must fail with non-zero exit code.
	_, _, err = runFsArchScriptTimeout(t, dir, `find work -exec echo {}`, 5*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("find with unterminated -exec succeeded; want error")
	}

	// -type d -empty -delete must delete empty dir cleanly with exit 0 and no ENOENT on ReadDir.
	_, serr, err = runFsArchScriptTimeout(t, dir, `find work -type d -empty -delete`, 5*time.Second)
	if exitCode(err) != 0 || serr != "" {
		t.Fatalf("find -type d -empty -delete failed: code=%d stderr=%q", exitCode(err), serr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "work", "emptydir")); !os.IsNotExist(statErr) {
		t.Fatalf("emptydir still exists after find -delete")
	}

	// find work -mindepth 1 -delete must empty 'work' (including non-empty work/sub) while keeping 'work'.
	_, serr, err = runFsArchScriptTimeout(t, dir, `find work -mindepth 1 -delete`, 5*time.Second)
	if exitCode(err) != 0 || serr != "" {
		t.Fatalf("find work -mindepth 1 -delete failed: code=%d stderr=%q", exitCode(err), serr)
	}
	entries, rerr := os.ReadDir(filepath.Join(dir, "work"))
	if rerr != nil || len(entries) != 0 {
		t.Fatalf("work should exist and be empty; entries=%v err=%v", entries, rerr)
	}

	// Stray ')' or unclosed '(' must error instead of silently truncating the expression.
	_, _, err = runFsArchScriptTimeout(t, dir, `find . -true \) -false`, 5*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("find with unmatched ')' succeeded; want error")
	}
	_, _, err = runFsArchScriptTimeout(t, dir, `find . \( -true`, 5*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("find with unclosed '(' succeeded; want error")
	}
}

func TestBugfixFsArch_FindFollowSymlinkLoop(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "real", "target.txt"), "ok\n")
	if err := os.MkdirAll(filepath.Join(dir, "tree", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join(dir, "real"), filepath.Join(dir, "tree", "ext"))
	// Create a symlink cycle: tree/sub/loop -> .. (tree)
	symlinkOrSkip(t, "..", filepath.Join(dir, "tree", "sub", "loop"))

	out, serr, err := runFsArchScriptTimeout(t, dir, `find -L tree -name 'target.txt'`, 3*time.Second)
	if !strings.Contains(out, "tree/ext/target.txt") {
		t.Fatalf("find -L did not traverse symlinked dir: out=%q stderr=%q err=%v", out, serr, err)
	}
	if exitCode(err) == 0 || !strings.Contains(strings.ToLower(serr), "loop") {
		t.Fatalf("find -L did not report symlink loop: code=%d stderr=%q", exitCode(err), serr)
	}
}

// 8. tar create/extract bugs:
//   - tar -cf out.tar . must not archive out.tar into itself
//   - tar -cf with symlinks must not fail with "archive/tar: write too long"
//   - tar -cf --exclude must exclude nested directory entries
//   - tar -cvf - must send verbose listing to stderr, not corrupting stdout archive
//   - tar -xf must block symlink-escape attacks and honor -k (--keep-old-files)
func TestBugfixFsArch_TarCreateExtractAndSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "proj", "keep.txt"), "hello\n")
	mustWrite(t, filepath.Join(dir, "proj", ".git", "config"), "secret\n")
	mustWrite(t, filepath.Join(dir, "proj", "sub", "ignore.log"), "noise\n")

	// --exclude on nested directory and glob pattern
	out, serr, err := runFsArchScriptTimeout(t, dir, `tar -cf p.tar --exclude=.git --exclude='*.log' proj && tar -tf p.tar`, 5*time.Second)
	if exitCode(err) != 0 {
		t.Fatalf("tar --exclude failed: code=%d stderr=%q", exitCode(err), serr)
	}
	if strings.Contains(out, ".git") || strings.Contains(out, "ignore.log") || !strings.Contains(out, "proj/keep.txt") {
		t.Fatalf("tar --exclude wrong member list: %q", out)
	}

	// tar -cvf - must keep stdout a valid archive (verbose names go to stderr)
	if err := os.MkdirAll(filepath.Join(dir, "pipe_out"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, serr, err = runFsArchScriptTimeout(t, dir, `tar -czvf - proj | tar -xzf - -C pipe_out`, 5*time.Second)
	if exitCode(err) != 0 {
		t.Fatalf("tar -czvf - | tar -xzf - failed (stdout corrupted by -v): code=%d stderr=%q", exitCode(err), serr)
	}
	if got, rerr := os.ReadFile(filepath.Join(dir, "pipe_out", "proj", "keep.txt")); rerr != nil || string(got) != "hello\n" {
		t.Fatalf("piped tar extract mismatch: %q %v", string(got), rerr)
	}

	// tar -cf self.tar . must NOT include self.tar inside itself
	sub := filepath.Join(dir, "selfdir")
	mustWrite(t, filepath.Join(sub, "big.txt"), strings.Repeat("A", 300*1024))
	out, serr, err = runFsArchScriptTimeout(t, sub, `tar -cf self.tar . && tar -tf self.tar`, 5*time.Second)
	if exitCode(err) != 0 {
		t.Fatalf("tar -cf self.tar . failed: code=%d stderr=%q", exitCode(err), serr)
	}
	if strings.Contains(out, "self.tar") {
		t.Fatalf("self.tar archived itself: %q", out)
	}

	// tar -xf -k (--keep-old-files) must not clobber existing file, and directory filter `tar -xf p.tar proj/sub`
	mustWrite(t, filepath.Join(dir, "kdest", "proj", "keep.txt"), "OLD_CONTENT\n")
	_, serr, err = runFsArchScriptTimeout(t, dir, `tar -xf p.tar -k -C kdest`, 5*time.Second)
	if got, _ := os.ReadFile(filepath.Join(dir, "kdest", "proj", "keep.txt")); string(got) != "OLD_CONTENT\n" {
		t.Fatalf("tar -xf -k overwrote existing file: got=%q stderr=%q err=%v", string(got), serr, err)
	}

	// Symlink-escape (Zip-Slip via symlink member `esc -> ..` + `esc/pwned.txt`) MUST be refused!
	var malicious bytes.Buffer
	tw := tar.NewWriter(&malicious)
	_ = tw.WriteHeader(&tar.Header{
		Name:     "esc",
		Typeflag: tar.TypeSymlink,
		Linkname: "..",
		Mode:     0o777,
	})
	payload := []byte("ESCAPED!\n")
	_ = tw.WriteHeader(&tar.Header{
		Name:     "esc/pwned.txt",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(payload)),
	})
	_, _ = tw.Write(payload)
	_ = tw.Close()

	evilTar := filepath.Join(dir, "evil.tar")
	if err := os.WriteFile(evilTar, malicious.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	extractSub := filepath.Join(dir, "safe_box")
	if err := os.MkdirAll(extractSub, 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, _ = runFsArchScriptTimeout(t, dir, `tar -xf evil.tar -C safe_box`, 5*time.Second)
	if _, statErr := os.Stat(filepath.Join(dir, "pwned.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("SECURITY BUG: tar extraction escaped destination via symlink and wrote ../pwned.txt!")
	}
}

func TestBugfixFsArch_TarSymlinkRoundtrip(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "sdir", "target.txt"), "non-empty-content\n")
	symlinkOrSkip(t, "target.txt", filepath.Join(dir, "sdir", "link.txt"))

	_, serr, err := runFsArchScriptTimeout(t, dir, `tar -cf sym.tar sdir && mkdir out && tar -xf sym.tar -C out`, 5*time.Second)
	if exitCode(err) != 0 {
		t.Fatalf("tar symlink roundtrip failed: code=%d stderr=%q", exitCode(err), serr)
	}
	lfi, lerr := os.Lstat(filepath.Join(dir, "out", "sdir", "link.txt"))
	if lerr != nil || lfi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("extracted link.txt is not a symlink: fi=%v err=%v", lfi, lerr)
	}
	dest, _ := os.Readlink(filepath.Join(dir, "out", "sdir", "link.txt"))
	if filepath.ToSlash(dest) != "target.txt" {
		t.Fatalf("extracted symlink target = %q; want target.txt", dest)
	}
}

// 9. gzip / gunzip:
//   - corrupt mid-stream gzip on stdin must NOT fatally abort shell script (must exit 1 and run `||`)
//   - corrupt mid-stream gunzip file must NOT leave partial corrupt output or destroy existing target
//   - gunzip -f on missing file must fail with exit 1 (not silently exit 0)
//   - gunzip -cdf / zcat -f on uncompressed data passes through to stdout
func TestBugfixFsArch_GzipIntegrityAndForce(t *testing.T) {
	dir := t.TempDir()

	// Build a valid gzip header followed by truncated/corrupt deflate payload.
	var validBuf bytes.Buffer
	gw := gzip.NewWriter(&validBuf)
	_, _ = gw.Write([]byte(strings.Repeat("hello world ", 200)))
	_ = gw.Close()
	raw := validBuf.Bytes()
	truncated := raw[:len(raw)-6] // corrupt/missing trailer
	truncPath := filepath.Join(dir, "trunc.txt.gz")
	if err := os.WriteFile(truncPath, truncated, 0o644); err != nil {
		t.Fatal(err)
	}

	// Corrupt gzip on stdin must NOT kill the shell (the `|| echo survived` branch must execute!).
	out, _, err := runFsArchScriptTimeout(t, dir, `gunzip < trunc.txt.gz || echo survived`, 5*time.Second)
	if exitCode(err) != 0 || !strings.Contains(out, "survived") {
		t.Fatalf("corrupt stdin gunzip killed shell script instead of returning exit 1: out=%q err=%v", out, err)
	}

	// Existing trunc.txt must NOT be clobbered, and if absent, partial output must be cleaned up.
	_, _, err = runFsArchScriptTimeout(t, dir, `gunzip -f trunc.txt.gz`, 5*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("gunzip -f on truncated .gz exited 0; want error")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "trunc.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("gunzip left corrupted partial output trunc.txt on disk")
	}

	// gunzip -f missing.gz must fail with exit 1, not exit 0!
	_, _, err = runFsArchScriptTimeout(t, dir, `gunzip -f no_such_file.gz`, 5*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("gunzip -f no_such_file.gz exited 0; want 1")
	}

	// gunzip -cdf / zcat -f passes uncompressed input through to stdout.
	mustWrite(t, filepath.Join(dir, "plain.txt"), "plain-passthrough\n")
	out, serr, err := runFsArchScriptTimeout(t, dir, `gunzip -cdf plain.txt`, 5*time.Second)
	if exitCode(err) != 0 || out != "plain-passthrough\n" {
		t.Fatalf("gunzip -cdf plain.txt = %q (code=%d stderr=%q)", out, exitCode(err), serr)
	}
}

// 10. readlink -m / -f, mktemp -u dry-run on non-existent dir & shell TMPDIR, base64 empty.
func TestBugfixFsArch_ReadlinkMktempBase64(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}

	// readlink -f when parent exists and leaf does not exist yet
	out, serr, err := runFsArchScriptTimeout(t, dir, `readlink -f real/future.txt`, 5*time.Second)
	if exitCode(err) != 0 || !strings.Contains(out, "future.txt") {
		t.Fatalf("readlink -f real/future.txt failed: out=%q code=%d stderr=%q", out, exitCode(err), serr)
	}

	// readlink -m when intermediate directories do not exist
	out, serr, err = runFsArchScriptTimeout(t, dir, `readlink -m no_such_a/no_such_b/../leaf.txt`, 5*time.Second)
	if exitCode(err) != 0 || !strings.Contains(out, "leaf.txt") || strings.Contains(out, "no_such_b") {
		t.Fatalf("readlink -m failed: out=%q code=%d stderr=%q", out, exitCode(err), serr)
	}

	// mktemp honors shell TMPDIR and mktemp -u does not touch disk even if target dir doesn't exist
	customTmp := filepath.Join(dir, "my_tmp")
	if err := os.MkdirAll(customTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	out, serr, err = runFsArchScriptTimeout(t, dir, `TMPDIR=my_tmp mktemp`, 5*time.Second)
	if exitCode(err) != 0 || !strings.Contains(filepath.ToSlash(out), "my_tmp/") {
		t.Fatalf("TMPDIR=my_tmp mktemp did not use shell TMPDIR: out=%q code=%d stderr=%q", out, exitCode(err), serr)
	}
	out, serr, err = runFsArchScriptTimeout(t, dir, `mktemp -u -p non_existent_tmp_dir`, 5*time.Second)
	if exitCode(err) != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("mktemp -u -p non_existent_tmp_dir failed: out=%q code=%d stderr=%q", out, exitCode(err), serr)
	}

	// base64 on empty input produces 0 bytes, not a stray newline
	out, _, err = runFsArchScriptTimeout(t, dir, `printf '' | base64`, 5*time.Second)
	if exitCode(err) != 0 || out != "" {
		t.Fatalf("printf '' | base64 = %q; want empty", out)
	}
}

// 11. ls -R context cancellation / pipe close and symlink-to-dir operand.
func TestBugfixFsArch_LsCancelAndSymlinkDir(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "sub", "item.txt"), "x\n")

	// Canceled context must abort ls -R immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	err := cmdLs(ctx, interp.HandlerContext{
		Dir:    dir,
		Stdout: &stdout,
		Stderr: &stderr,
	}, []string{"ls", "-R", "."})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cmdLs with canceled ctx returned %v; want context.Canceled", err)
	}

	// Symlink to directory without -l/-d/-F lists directory contents.
	symlinkOrSkip(t, "sub", filepath.Join(dir, "linksub"))
	out, serr, err := runFsArchScriptTimeout(t, dir, `ls linksub`, 5*time.Second)
	if exitCode(err) != 0 || strings.TrimSpace(out) != "item.txt" {
		t.Fatalf("ls linksub = %q (code=%d stderr=%q); want item.txt", out, exitCode(err), serr)
	}
}

// 12. scanlines.go: lineReader must not drop buffered partial data when the
// underlying reader returns data together with a non-EOF error, while still
// surfacing that error via Err().
type partialErrReader struct {
	sent bool
}

func (p *partialErrReader) Read(b []byte) (int, error) {
	if !p.sent {
		p.sent = true
		n := copy(b, "line1\ntrailing_before_err")
		return n, io.ErrUnexpectedEOF
	}
	return 0, io.ErrUnexpectedEOF
}

func TestBugfixFsArch_LineReaderPartialDataBeforeError(t *testing.T) {
	lr := newLineReader(&partialErrReader{})
	var got []string
	for lr.Scan() {
		got = append(got, lr.Text())
	}
	if len(got) != 2 || got[0] != "line1" || got[1] != "trailing_before_err" {
		t.Fatalf("lineReader dropped partial data before error: got=%v", got)
	}
	if !errors.Is(lr.Err(), io.ErrUnexpectedEOF) {
		t.Fatalf("lineReader lost error: got=%v; want ErrUnexpectedEOF", lr.Err())
	}
}

// 13. rm -f on read-only files (Windows FILE_ATTRIBUTE_READONLY), tar hardlink
// extraction & outside-destination hardlink block, and sha256sum -c --strict.
func TestBugfixFsArch_RmReadonlyTarHardlinkAndHashStrict(t *testing.T) {
	dir := t.TempDir()
	ro := filepath.Join(dir, "readonly.txt")
	mustWrite(t, ro, "locked\n")
	_, serr, err := runFsArchScriptTimeout(t, dir, `chmod 444 readonly.txt && rm -f readonly.txt`, 5*time.Second)
	if exitCode(err) != 0 {
		t.Fatalf("rm -f on chmod 444 file failed: code=%d stderr=%q", exitCode(err), serr)
	}
	if _, statErr := os.Stat(ro); !os.IsNotExist(statErr) {
		t.Fatalf("readonly.txt still exists after rm -f")
	}

	// Out-of-order valid hardlink inside archive succeeds, while hardlink escaping outside fails.
	var hbuf bytes.Buffer
	tw := tar.NewWriter(&hbuf)
	_ = tw.WriteHeader(&tar.Header{
		Name:     "alias.txt",
		Typeflag: tar.TypeLink,
		Linkname: "orig.txt",
		Mode:     0o644,
	})
	body := []byte("hardlink-data\n")
	_ = tw.WriteHeader(&tar.Header{
		Name:     "orig.txt",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(body)),
	})
	_, _ = tw.Write(body)
	_ = tw.Close()
	if err := os.WriteFile(filepath.Join(dir, "hlink.tar"), hbuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "hout"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, serr, err = runFsArchScriptTimeout(t, dir, `tar -xf hlink.tar -C hout`, 5*time.Second)
	if exitCode(err) != 0 {
		t.Fatalf("tar out-of-order hardlink extract failed: code=%d stderr=%q", exitCode(err), serr)
	}
	if got, rerr := os.ReadFile(filepath.Join(dir, "hout", "alias.txt")); rerr != nil || string(got) != "hardlink-data\n" {
		t.Fatalf("extracted hardlink content = %q err=%v", string(got), rerr)
	}

	// sha256sum -c --strict fails when malformed lines appear alongside valid lines.
	mustWrite(t, filepath.Join(dir, "good.txt"), "abc\n")
	_, serr, err = runFsArchScriptTimeout(t, dir, `sha256sum good.txt > sums.txt && echo 'GARBAGE LINE' >> sums.txt && sha256sum -c --strict sums.txt`, 5*time.Second)
	if exitCode(err) == 0 {
		t.Fatalf("sha256sum -c --strict exited 0 despite malformed line (stderr=%q)", serr)
	}
}
