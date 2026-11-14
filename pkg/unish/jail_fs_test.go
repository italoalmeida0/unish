package unish

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

type fsPolicyHook struct {
	base      *testJailHook
	mu        sync.Mutex
	deniedOps map[AccessOp]bool
}

func newFSPolicyHook(t *testing.T, root string) *fsPolicyHook {
	t.Helper()
	return &fsPolicyHook{
		base:      newTestJailHook(t, root),
		deniedOps: make(map[AccessOp]bool),
	}
}

func (h *fsPolicyHook) denyPath(p string) {
	realP, lexP := canonicalizeForCheck(p)
	h.base.mu.Lock()
	if realP != "" {
		h.base.deniedPaths[realP] = true
	}
	if lexP != "" {
		h.base.deniedPaths[lexP] = true
	}
	h.base.mu.Unlock()
}

func (h *fsPolicyHook) setDenyOp(op AccessOp, deny bool) {
	h.mu.Lock()
	if deny {
		h.deniedOps[op] = true
	} else {
		delete(h.deniedOps, op)
	}
	h.mu.Unlock()
}

func (h *fsPolicyHook) CheckAccess(ctx context.Context, req AccessRequest) error {
	h.mu.Lock()
	opDenied := h.deniedOps[req.Op]
	h.mu.Unlock()
	if opDenied {
		return &AccessDenialError{Op: req.Op, Target: req.Path, Reason: "operation denied by policy"}
	}
	return h.base.CheckAccess(ctx, req)
}

func (h *fsPolicyHook) DialContext(ctx context.Context, cmdName, network, address string) (net.Conn, error) {
	return h.base.DialContext(ctx, cmdName, network, address)
}

func (h *fsPolicyHook) Listen(ctx context.Context, cmdName, network, address string) (net.Listener, error) {
	return h.base.Listen(ctx, cmdName, network, address)
}

func (h *fsPolicyHook) ListenPacket(ctx context.Context, cmdName, network, address string) (net.PacketConn, error) {
	return h.base.ListenPacket(ctx, cmdName, network, address)
}

func runJailedScript(t *testing.T, ctx context.Context, dir string, extraEnv []string, script string) (stdout, stderr string, code int) {
	t.Helper()
	devNull, nerr := os.Open(os.DevNull)
	if nerr != nil {
		t.Fatalf("open DevNull: %v", nerr)
	}
	defer devNull.Close()

	var outBuf, errBuf bytes.Buffer
	envPairs := append(os.Environ(), extraEnv...)
	r, err := interp.New(
		interp.Dir(dir),
		interp.Env(expand.ListEnviron(envPairs...)),
		interp.StdIO(devNull, &outBuf, &errBuf),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		t.Fatalf("interp.New: %v", err)
	}
	prog, err := syntax.NewParser().Parse(strings.NewReader(script), "jail_fs_test.sh")
	if err != nil {
		t.Fatalf("parse script %q: %v", script, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	runErr := r.Run(runCtx, prog)
	if runErr == nil {
		return outBuf.String(), errBuf.String(), 0
	}
	if es, ok := interp.IsExitStatus(runErr); ok {
		return outBuf.String(), errBuf.String(), int(es)
	}
	var ee exitError
	if errors.As(runErr, &ee) {
		return outBuf.String(), errBuf.String(), ee.code
	}
	t.Fatalf("unexpected non-exit error running %q: %v (stderr=%q)", script, runErr, errBuf.String())
	return "", "", -1
}

func findHostExternalBin(t *testing.T) string {
	t.Helper()
	candidates := []string{"go", "git"}
	if runtime.GOOS == "windows" {
		candidates = append(candidates, "cmd")
	} else {
		candidates = append(candidates, "env", "uname")
	}
	for _, c := range candidates {
		if lookupExtra(c) != nil {
			continue
		}
		if p, err := exec.LookPath(c); err == nil && p != "" {
			return c
		}
	}
	t.Skip("no suitable external binary found on PATH")
	return ""
}

func canCreateSymlinks(t *testing.T, dir string) bool {
	t.Helper()
	probeTarget := filepath.Join(dir, ".symlink_probe_target")
	probeLink := filepath.Join(dir, ".symlink_probe_link")
	_ = os.WriteFile(probeTarget, []byte("x"), 0o644)
	defer os.Remove(probeTarget)
	if err := os.Symlink(probeTarget, probeLink); err != nil {
		return false
	}
	_ = os.Remove(probeLink)
	return true
}

func TestJailFS_FileOpsAndXargsEnforcement(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("TOP_SECRET_PAYLOAD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "sub", "item.txt"), []byte("subitem\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hook := newFSPolicyHook(t, ws)
	ctx := WithCapabilityHook(context.Background(), hook)

	// 1. Inside-workspace fileops work cleanly via hc.Dir without os.Chdir.
	out, stderr, code := runJailedScript(t, ctx, ws, nil, `
cp a.txt b.txt
cp -r sub sub_copy
mv b.txt b_moved.txt
mkdir -p nested/dir
touch nested/dir/touched.txt
chmod 644 nested/dir/touched.txt
base64 a.txt
`)
	if code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("inside-jail fileops failed: code=%d out=%q stderr=%q", code, out, stderr)
	}

	// 2. Direct outside path traversal blocked across cp, mv, rm (-f!), mkdir, touch (-c/-r), chmod, base64.
	deniedCmds := []string{
		`cp ../outside/secret.txt stolen.txt`,
		`cp a.txt ../outside/pwn_cp.txt`,
		`mv a.txt ../outside/pwn_mv.txt`,
		`mv ../outside/secret.txt stolen_mv.txt`,
		`rm ../outside/secret.txt`,
		`rm -f ../outside/secret.txt`,
		`rm -rf ../outside`,
		`del ../outside/secret.txt`,
		`dir ../outside`,
		`mkdir ../outside/pwn_dir`,
		`mkdir -p ../outside/pwn_dir/sub`,
		`touch ../outside/pwn_touch.txt`,
		`touch -c ../outside/secret.txt`,
		`touch -r ../outside/secret.txt a.txt`,
		`chmod 666 ../outside/secret.txt`,
		`base64 ../outside/secret.txt`,
	}
	for _, cmd := range deniedCmds {
		_, _, rc := runJailedScript(t, ctx, ws, nil, cmd)
		if rc == 0 {
			t.Fatalf("SECURITY: expected jail denial for %q, got exit 0", cmd)
		}
	}
	if b, err := os.ReadFile(secretFile); err != nil || string(b) != "TOP_SECRET_PAYLOAD\n" {
		t.Fatalf("outside secret modified or removed: %q, err=%v", string(b), err)
	}

	// 3. Intra-workspace denied child (.env) blocks recursive rm -rf, mv dir, cp -r, and chmod -R!
	envDir := filepath.Join(ws, "appdir")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(envDir, ".env")
	pubFile := filepath.Join(envDir, "public.txt")
	_ = os.WriteFile(envFile, []byte("API_KEY=secret\n"), 0o600)
	_ = os.WriteFile(pubFile, []byte("ok\n"), 0o644)
	hook.denyPath(envFile)

	if _, _, rc := runJailedScript(t, ctx, ws, nil, `mv appdir appdir_renamed`); rc == 0 {
		t.Fatal("SECURITY: mv of directory containing denied .env succeeded")
	}
	if _, _, rc := runJailedScript(t, ctx, ws, nil, `cp -r appdir appdir_copy`); rc == 0 {
		t.Fatal("SECURITY: cp -r of directory containing denied .env succeeded without error")
	}
	if _, _, rc := runJailedScript(t, ctx, ws, nil, `chmod -R 644 appdir`); rc == 0 {
		t.Fatal("SECURITY: chmod -R of directory containing denied .env succeeded without error")
	}
	if _, _, rc := runJailedScript(t, ctx, ws, nil, `rm -rf appdir`); rc == 0 {
		t.Fatal("SECURITY: rm -rf of directory containing denied .env succeeded")
	}
	if _, err := os.Stat(envFile); err != nil {
		t.Fatalf("SECURITY: rm -rf deleted denied intra-workspace .env: %v", err)
	}

	// 4. Symlink escapes (existing outside target, dangling outside target, and relative depth-shift!).
	if canCreateSymlinks(t, ws) {
		escapeRead := filepath.Join(ws, "escape_read")
		outsideDangling := filepath.Join(outside, "dangling_pwn.txt")
		escapeDangling := filepath.Join(ws, "escape_dangling")
		_ = os.Symlink(secretFile, escapeRead)
		_ = os.Symlink(outsideDangling, escapeDangling)

		for _, cmd := range []string{
			`cp escape_read leaked.txt`,
			`cp a.txt escape_dangling`,
			`touch escape_dangling`,
			`chmod 644 escape_read`,
			`base64 escape_read`,
		} {
			if _, _, rc := runJailedScript(t, ctx, ws, nil, cmd); rc == 0 {
				t.Fatalf("SECURITY: symlink escape %q succeeded", cmd)
			}
		}
		if _, err := os.Stat(outsideDangling); !os.IsNotExist(err) {
			t.Fatalf("SECURITY: dangling symlink created file outside jail: %v", err)
		}

		// Relative symlink depth-shift: sub/rel_up -> ../a.txt is valid inside sub/,
		// whereas moving or cp -P'ing it to ws/shifted_up makes it point to base/a.txt OUTSIDE ws!
		relUp := filepath.Join(ws, "sub", "rel_up")
		if err := os.Symlink(filepath.Join("..", "a.txt"), relUp); err == nil {
			if _, _, rc := runJailedScript(t, ctx, ws, nil, `cp -P sub/rel_up shifted_cp`); rc == 0 {
				t.Fatal("SECURITY: cp -P allowed relative symlink depth-shift escape outside jail")
			}
			if _, _, rc := runJailedScript(t, ctx, ws, nil, `mv sub/rel_up shifted_mv`); rc == 0 {
				t.Fatal("SECURITY: mv allowed relative symlink depth-shift escape outside jail")
			}
		}
	}

	// 5. xargs external command execution blocked via checkExec (exit 126), including -P parallel mode.
	extBin := findHostExternalBin(t)
	if _, errStr, rc := runJailedScript(t, ctx, ws, nil, fmt.Sprintf("printf 'x\\n' | xargs %s", extBin)); rc != 126 {
		t.Fatalf("expected xargs external exec denial rc=126, got rc=%d (stderr=%q)", rc, errStr)
	}
	if _, errStr, rc := runJailedScript(t, ctx, ws, nil, fmt.Sprintf("printf 'x\\ny\\n' | xargs -n 1 -P 2 %s", extBin)); rc != 126 {
		t.Fatalf("expected xargs -P 2 external exec denial rc=126, got rc=%d (stderr=%q)", rc, errStr)
	}
	// Explicit path to outside non-existent binary must fail with 126 (permission denied), never 127 probe leak!
	if _, errStr, rc := runJailedScript(t, ctx, ws, nil, `printf 'x\n' | xargs ../outside/no_such_bin`); rc != 126 {
		t.Fatalf("expected xargs ../outside/no_such_bin rc=126, got rc=%d (stderr=%q)", rc, errStr)
	}
	// Allowed relative executable inside ws must trigger CheckAccess(OpExec) only once (never double-prompting Approvers).
	localBinName := "local_ok.sh"
	localBinBody := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		localBinName = "local_ok.bat"
		localBinBody = "@echo off\r\nexit /b 0\r\n"
	}
	if err := os.WriteFile(filepath.Join(ws, localBinName), []byte(localBinBody), 0o755); err != nil {
		t.Fatal(err)
	}
	hook.base.mu.Lock()
	beforeReqs := len(hook.base.requests)
	hook.base.mu.Unlock()
	if _, errStr, rc := runJailedScript(t, ctx, ws, nil, fmt.Sprintf("printf 'x\\n' | xargs ./%s", localBinName)); rc != 0 {
		t.Fatalf("expected xargs ./%s inside jail to succeed, got rc=%d (stderr=%q)", localBinName, rc, errStr)
	}
	hook.base.mu.Lock()
	newReqs := append([]AccessRequest(nil), hook.base.requests[beforeReqs:]...)
	hook.base.mu.Unlock()
	execChecks := 0
	for _, r := range newReqs {
		if r.Op == OpExec && r.Command == "xargs" {
			execChecks++
		}
	}
	if execChecks != 1 {
		t.Fatalf("expected exactly 1 OpExec check for xargs ./%s, got %d", localBinName, execChecks)
	}
}

func TestJailFS_FileProcFindLnReadlinkMktempHashes(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(ws, "many", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(outside, "secret.txt")
	_ = os.WriteFile(secretFile, []byte("OUTSIDE_HASH_SECRET\n"), 0o600)
	_ = os.WriteFile(filepath.Join(ws, "many", "a.txt"), []byte("hello\n"), 0o644)
	_ = os.WriteFile(filepath.Join(ws, "many", "sub", "b.txt"), []byte("world\n"), 0o644)
	_ = os.WriteFile(filepath.Join(ws, "keep.txt"), []byte("DO_NOT_DELETE\n"), 0o644)

	hook := newFSPolicyHook(t, ws)
	ctx := WithCapabilityHook(context.Background(), hook)

	// 1. find preserves clean user-requested relative path prefixes in BOTH unhooked and hooked modes!
	for _, tc := range []struct {
		name string
		c    context.Context
	}{
		{"unhooked", context.Background()},
		{"hooked", ctx},
	} {
		out, errStr, rc := runJailedScript(t, tc.c, ws, nil, `find many -type f | sort`)
		if rc != 0 {
			t.Fatalf("find (%s) failed: rc=%d stderr=%q", tc.name, rc, errStr)
		}
		got := strings.TrimSpace(strings.ReplaceAll(out, "\r\n", "\n"))
		want := "many/a.txt\nmany/sub/b.txt"
		if got != want {
			t.Fatalf("find (%s) relative output regression: got %q, want %q", tc.name, got, want)
		}
	}

	// 2. find denials: outside root, -delete under OpDelete deny, and -exec external binary.
	if _, _, rc := runJailedScript(t, ctx, ws, nil, `find ../outside`); rc == 0 {
		t.Fatal("SECURITY: find ../outside succeeded")
	}

	hook.setDenyOp(OpDelete, true)
	if _, _, rc := runJailedScript(t, ctx, ws, nil, `find many -name a.txt -delete`); rc == 0 {
		t.Fatal("SECURITY: find -delete succeeded when OpDelete was denied")
	}
	hook.setDenyOp(OpDelete, false)
	if _, err := os.Stat(filepath.Join(ws, "many", "a.txt")); err != nil {
		t.Fatalf("find -delete removed file despite OpDelete denial: %v", err)
	}

	extBin := findHostExternalBin(t)
	if _, _, rc := runJailedScript(t, ctx, ws, nil, fmt.Sprintf(`find many -name a.txt -exec %s {} \;`, extBin)); rc == 0 {
		t.Fatal("SECURITY: find -exec external binary succeeded inside jail")
	}

	// 3. ln, readlink, realpath, du, date -r, hashes.
	// CRITICAL: ln -sf ../outside/secret.txt keep.txt must NOT unlink keep.txt before rejecting the link!
	if _, _, rc := runJailedScript(t, ctx, ws, nil, `ln ../outside/secret.txt hard_escape.txt`); rc == 0 {
		t.Fatal("SECURITY: hardlink to outside file succeeded")
	}
	if _, _, rc := runJailedScript(t, ctx, ws, nil, `ln -sf ../outside/secret.txt keep.txt`); rc == 0 {
		t.Fatal("SECURITY: ln -sf to outside file succeeded")
	}
	if b, err := os.ReadFile(filepath.Join(ws, "keep.txt")); err != nil || string(b) != "DO_NOT_DELETE\n" {
		t.Fatalf("SECURITY: failed ln -sf clobbered/deleted existing keep.txt! data=%q err=%v", string(b), err)
	}

	if canCreateSymlinks(t, ws) {
		out, errStr, rc := runJailedScript(t, ctx, ws, nil, `
ln -s keep.txt valid_link.txt
readlink valid_link.txt
realpath valid_link.txt >/dev/null
`)
		if rc != 0 || strings.TrimSpace(out) != "keep.txt" {
			t.Fatalf("intra-jail ln -s / readlink / realpath failed: rc=%d out=%q stderr=%q", rc, out, errStr)
		}

		escapeSym := filepath.Join(ws, "escape_sym")
		_ = os.Symlink(secretFile, escapeSym)
		if _, _, rc := runJailedScript(t, ctx, ws, nil, `realpath escape_sym`); rc == 0 {
			t.Fatal("SECURITY: realpath followed symlink outside jail")
		}
		if _, _, rc := runJailedScript(t, ctx, ws, nil, `readlink -f escape_sym`); rc == 0 {
			t.Fatal("SECURITY: readlink -f followed symlink outside jail")
		}
		if _, _, rc := runJailedScript(t, ctx, ws, nil, `find -L . >/dev/null`); rc == 0 {
			t.Fatal("SECURITY: find -L followed symlink outside jail without error")
		}
		_ = os.Remove(escapeSym)
	}

	for _, cmd := range []string{
		`readlink ../outside/secret.txt`,
		`realpath ../outside/secret.txt`,
		`du ../outside`,
		`date -r ../outside/secret.txt +%s`,
		`md5sum ../outside/secret.txt`,
		`sha1sum ../outside/secret.txt`,
		`sha256sum ../outside/secret.txt`,
		`shasum ../outside/secret.txt`,
	} {
		if _, _, rc := runJailedScript(t, ctx, ws, nil, cmd); rc == 0 {
			t.Fatalf("SECURITY: %q succeeded outside jail", cmd)
		}
	}

	// Valid hash generation & -c verification inside jail.
	if _, errStr, rc := runJailedScript(t, ctx, ws, nil, `sha256sum keep.txt > sums.sha && sha256sum -c sums.sha`); rc != 0 {
		t.Fatalf("sha256sum -c inside jail failed: rc=%d stderr=%q", rc, errStr)
	}

	// 4. mktemp: default os.TempDir() outside jail is denied; -p . or TMPDIR=. inside jail succeeds!
	if _, _, rc := runJailedScript(t, ctx, ws, []string{"TMPDIR="}, `mktemp`); rc == 0 {
		t.Fatal("SECURITY: mktemp in host os.TempDir() outside jail succeeded")
	}
	if _, _, rc := runJailedScript(t, ctx, ws, []string{"TMPDIR="}, `mktemp -d`); rc == 0 {
		t.Fatal("SECURITY: mktemp -d in host os.TempDir() outside jail succeeded")
	}
	if _, _, rc := runJailedScript(t, ctx, ws, []string{"TMPDIR="}, `mktemp -u`); rc == 0 {
		t.Fatal("SECURITY: mktemp -u in host os.TempDir() outside jail succeeded")
	}
	if _, errStr, rc := runJailedScript(t, ctx, ws, nil, `
f=$(mktemp -p . tmp.XXXXXX)
d=$(mktemp -d -p . tmpd.XXXXXX)
test -f "$f" && test -d "$d"
rm -rf "$f" "$d"
`); rc != 0 {
		t.Fatalf("mktemp -p . inside jail failed: rc=%d stderr=%q", rc, errStr)
	}
}

func buildMaliciousTar(t *testing.T, entries []tar.Header, bodies map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range entries {
		body := bodies[hdr.Name]
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == 0 {
			hdr.Size = int64(len(body))
			if hdr.Mode == 0 {
				hdr.Mode = 0o644
			}
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("tar WriteHeader(%s): %v", hdr.Name, err)
		}
		if len(body) > 0 {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatalf("tar Write(%s): %v", hdr.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	return buf.Bytes()
}

func TestJailFS_ArchiveLsAndRg(t *testing.T) {
	base := t.TempDir()
	// Place a .gitignore in base ABOVE ws that ignores *.txt — rg inside jailed ws MUST NOT read it!
	if err := os.WriteFile(filepath.Join(base, ".gitignore"), []byte("*.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(outside, "secret.txt")
	_ = os.WriteFile(secretFile, []byte("NEEDLE_OUTSIDE_SECRET\n"), 0o600)
	_ = os.WriteFile(filepath.Join(ws, "src", "match.txt"), []byte("NEEDLE_INSIDE_OK\n"), 0o644)
	deniedEnv := filepath.Join(ws, "src", ".env")
	_ = os.WriteFile(deniedEnv, []byte("NEEDLE_DENIED_ENV\n"), 0o600)

	hook := newFSPolicyHook(t, ws)
	hook.denyPath(deniedEnv)
	ctx := WithCapabilityHook(context.Background(), hook)

	// 1. ls: works at jail root including -la (graceful .. fallback), preserves clean relative names, blocks outside!
	lsOut, lsErr, lsRC := runJailedScript(t, ctx, ws, nil, `ls -la && ls src`)
	if lsRC != 0 || !strings.Contains(lsOut, "match.txt") {
		t.Fatalf("ls inside jail failed: rc=%d out=%q stderr=%q", lsRC, lsOut, lsErr)
	}
	if _, _, rc := runJailedScript(t, ctx, ws, nil, `ls ../outside`); rc == 0 {
		t.Fatal("SECURITY: ls ../outside succeeded")
	}

	// 2. rg: clean relative paths, skips denied .env, ignores parent ../.gitignore outside jail, exits 2 on explicit outside path!
	rgOut, rgErr, rgRC := runJailedScript(t, ctx, ws, nil, `rg --no-messages -uu NEEDLE`)
	if rgRC != 0 {
		t.Fatalf("rg inside jail failed: rc=%d out=%q stderr=%q", rgRC, rgOut, rgErr)
	}
	if strings.TrimSpace(rgOut) != "src/match.txt:1:NEEDLE_INSIDE_OK" {
		t.Fatalf("rg output mismatch (must be relative and omit .env): got %q", rgOut)
	}
	// Default rg (without -u) MUST still find src/match.txt despite base/.gitignore having "*.txt"!
	rgDefOut, rgDefErr, rgDefRC := runJailedScript(t, ctx, ws, nil, `rg NEEDLE`)
	if rgDefRC != 0 || strings.TrimSpace(rgDefOut) != "src/match.txt:1:NEEDLE_INSIDE_OK" {
		t.Fatalf("rg leaked/applied parent ../.gitignore above jail or leaked .env: rc=%d out=%q stderr=%q", rgDefRC, rgDefOut, rgDefErr)
	}
	// rg --files omits denied .env and keeps relative path.
	rgFilesOut, _, rgFilesRC := runJailedScript(t, ctx, ws, nil, `rg --no-messages -uu --files`)
	if rgFilesRC != 0 || strings.Contains(rgFilesOut, ".env") || !strings.Contains(rgFilesOut, "src/match.txt") {
		t.Fatalf("rg --files unexpected output: rc=%d out=%q", rgFilesRC, rgFilesOut)
	}
	// Explicit denied operand MUST exit 2!
	if _, _, rc := runJailedScript(t, ctx, ws, nil, `rg NEEDLE ../outside/secret.txt`); rc != 2 {
		t.Fatalf("expected rg explicit outside operand to exit 2, got %d", rc)
	}
	if _, _, rc := runJailedScript(t, ctx, ws, nil, `rg NEEDLE src/.env`); rc != 2 {
		t.Fatalf("expected rg explicit denied .env operand to exit 2, got %d", rc)
	}

	// 3. tar & gzip/gunzip/zcat inside jail vs escape attempts.
	_ = os.MkdirAll(filepath.Join(ws, "clean"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, "clean", "hello.txt"), []byte("archive-me\n"), 0o644)

	tarOut, tarErr, tarRC := runJailedScript(t, ctx, ws, nil, `
tar -cf clean.tar clean
tar -tf clean.tar
mkdir extracted
tar -xf clean.tar -C extracted
cat extracted/clean/hello.txt
cp clean/hello.txt gz_me.txt
gzip gz_me.txt
zcat gz_me.txt.gz
gunzip gz_me.txt.gz
`)
	if tarRC != 0 || !strings.Contains(tarOut, "clean/hello.txt") || !strings.Contains(tarOut, "archive-me") {
		t.Fatalf("tar/gzip inside jail failed: rc=%d out=%q stderr=%q", tarRC, tarOut, tarErr)
	}

	for _, cmd := range []string{
		`tar -cf ../outside/out.tar clean`,
		`tar -cf bad.tar ../outside/secret.txt`,
		`tar -cf bad.tar -C ../outside secret.txt`,
		`tar -tf ../outside/secret.txt`,
		`gzip ../outside/secret.txt`,
		`zcat ../outside/secret.txt`,
	} {
		if _, _, rc := runJailedScript(t, ctx, ws, nil, cmd); rc == 0 {
			t.Fatalf("SECURITY: %q succeeded outside jail", cmd)
		}
	}

	// Craft malicious tar archives attempting:
	// (a) -P absolute/parent path write outside jail
	outsideTarPwn := filepath.Join(outside, "tar_pwn.txt")
	evilPathTar := buildMaliciousTar(t, []tar.Header{
		{Name: "../outside/tar_pwn.txt", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"../outside/tar_pwn.txt": "PWNED\n"})
	_ = os.WriteFile(filepath.Join(ws, "evil_path.tar"), evilPathTar, 0o644)
	if _, _, rc := runJailedScript(t, ctx, ws, nil, `tar -xPf evil_path.tar`); rc == 0 {
		t.Fatal("SECURITY: tar -xPf extracted ../outside/tar_pwn.txt outside jail")
	}
	if _, err := os.Stat(outsideTarPwn); !os.IsNotExist(err) {
		t.Fatalf("SECURITY: tar -xPf created outside file: %v", err)
	}

	// (b) symlink entry pointing outside jail
	evilSymTar := buildMaliciousTar(t, []tar.Header{
		{Name: "tar_sym_escape", Typeflag: tar.TypeSymlink, Linkname: secretFile, Mode: 0o777},
	}, nil)
	_ = os.WriteFile(filepath.Join(ws, "evil_sym.tar"), evilSymTar, 0o644)
	if _, _, rc := runJailedScript(t, ctx, ws, nil, `tar -xf evil_sym.tar`); rc == 0 {
		t.Fatal("SECURITY: tar extracted symlink pointing outside jail")
	}

	// (c) hardlink entry pointing outside jail
	evilLinkTar := buildMaliciousTar(t, []tar.Header{
		{Name: "tar_hard_escape", Typeflag: tar.TypeLink, Linkname: secretFile, Mode: 0o644},
	}, nil)
	_ = os.WriteFile(filepath.Join(ws, "evil_hard.tar"), evilLinkTar, 0o644)
	if _, _, rc := runJailedScript(t, ctx, ws, nil, `tar -xPf evil_hard.tar`); rc == 0 {
		t.Fatal("SECURITY: tar extracted hardlink pointing outside jail")
	}

	// (d) archive member traversing a pre-existing directory symlink inside ws -> ../outside
	if canCreateSymlinks(t, ws) {
		outsideViaSymDir := filepath.Join(outside, "via_dir_sym.txt")
		_ = os.Symlink(outside, filepath.Join(ws, "pre_dir_sym"))
		evilDirSymTar := buildMaliciousTar(t, []tar.Header{
			{Name: "pre_dir_sym/via_dir_sym.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string]string{"pre_dir_sym/via_dir_sym.txt": "PWNED\n"})
		_ = os.WriteFile(filepath.Join(ws, "evil_dirsym.tar"), evilDirSymTar, 0o644)
		if _, _, rc := runJailedScript(t, ctx, ws, nil, `tar -xf evil_dirsym.tar`); rc == 0 {
			t.Fatal("SECURITY: tar extracted file through directory symlink outside jail")
		}
		if _, err := os.Stat(outsideViaSymDir); !os.IsNotExist(err) {
			t.Fatalf("SECURITY: tar directory symlink escape created outside file: %v", err)
		}
		_ = os.Remove(filepath.Join(ws, "pre_dir_sym"))
	}
}

func TestJailFS_PatchAutoToolsAndProcSubst(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(outside, "secret.txt")
	_ = os.WriteFile(secretFile, []byte("line1\n"), 0o600)
	_ = os.WriteFile(filepath.Join(ws, "doc.txt"), []byte("hello\n"), 0o644)
	_ = os.WriteFile(filepath.Join(ws, "table.txt"), []byte("name role\nalice admin\n"), 0o644)

	hook := newFSPolicyHook(t, ws)
	ctx := WithCapabilityHook(context.Background(), hook)

	// 1. patch, sponge, flock, column, xxd inside workspace jail — including write-allowed / OpDelete-denied mode!
	hook.setDenyOp(OpDelete, true)
	out, errStr, rc := runJailedScript(t, ctx, ws, nil, `
patch doc.txt <<'EOF'
--- doc.txt
+++ doc.txt
@@ -1 +1 @@
-hello
+patched
EOF
cat doc.txt
printf 'sponged\n' | sponge sp.txt
cat sp.txt
xxd -p sp.txt sp.hex
xxd -r -p sp.hex sp_rt.txt
cat sp_rt.txt
column -t table.txt
flock -n app.lock -c 'printf "flock_ok\n"'
`)
	hook.setDenyOp(OpDelete, false)
	if rc != 0 {
		t.Fatalf("patch/sponge/xxd/column/flock inside jail failed: rc=%d out=%q stderr=%q", rc, out, errStr)
	}
	for _, want := range []string{"patched", "sponged", "alice", "flock_ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output %q", want, out)
		}
	}

	// 2. Escape denials across patch (including --dry-run!), sponge, flock, column, xxd.
	escapeCmds := []string{
		`patch -d ../outside </dev/null`,
		`patch -i ../outside/secret.txt doc.txt`,
		`patch -o ../outside/pwn_out.txt doc.txt <<'EOF'
--- doc.txt
+++ doc.txt
@@ -1 +1 @@
-patched
+evil
EOF`,
		`patch --dry-run <<'EOF'
--- ../outside/secret.txt
+++ ../outside/secret.txt
@@ -1 +1 @@
-line1
+evil
EOF`,
		`patch <<'EOF'
--- /dev/null
+++ ../outside/new_evil.txt
@@ -0,0 +1 @@
+evil
EOF`,
		`printf 'evil\n' | sponge ../outside/pwn_sponge.txt`,
		`flock -n ../outside/evil.lock -c 'true'`,
		`column -t ../outside/secret.txt`,
		`xxd ../outside/secret.txt`,
		`xxd doc.txt ../outside/pwn.hex`,
		`xxd -r -p sp.hex ../outside/pwn.bin`,
	}
	for _, cmd := range escapeCmds {
		if _, _, code := runJailedScript(t, ctx, ws, nil, cmd); code == 0 {
			t.Fatalf("SECURITY: %q succeeded outside jail", cmd)
		}
	}

	// Dangling symlink escape on patch, sponge, and xxd output!
	if canCreateSymlinks(t, ws) {
		outsideDangling := filepath.Join(outside, "dangling_auto.txt")
		_ = os.Symlink(outsideDangling, filepath.Join(ws, "dang_out"))
		for _, cmd := range []string{
			`printf 'evil\n' | sponge dang_out`,
			`xxd doc.txt dang_out`,
			`patch dang_out <<'EOF'
--- /dev/null
+++ dang_out
@@ -0,0 +1 @@
+evil
EOF`,
		} {
			if _, _, code := runJailedScript(t, ctx, ws, nil, cmd); code == 0 {
				t.Fatalf("SECURITY: dangling symlink write %q succeeded", cmd)
			}
		}
		if _, err := os.Stat(outsideDangling); !os.IsNotExist(err) {
			t.Fatalf("SECURITY: dangling symlink created outside file: %v", err)
		}
	}

	// flock external subcommand must be checked via checkExec and return exit 126!
	extBin := findHostExternalBin(t)
	if _, _, code := runJailedScript(t, ctx, ws, nil, fmt.Sprintf(`flock -n app.lock %s`, extBin)); code != 126 {
		t.Fatalf("expected flock external exec denial rc=126, got %d", code)
	}

	// 3. Process substitution <(...) and >(...) MUST work inside strict workspace-only jails
	// (even though os.TempDir() is outside ws!), while commands INSIDE <(...) still inherit ctx!
	psOut, psErr, psRC := runJailedScript(t, ctx, ws, nil, `
base64 <(printf 'proc-alpha')
sha256sum <(printf 'proc-beta') >/dev/null
rg -f <(printf 'patched\n') doc.txt
column -t <(printf 'k v\nx y\n')
xxd -p <(printf 'OK')
patch -i <(printf -- '--- doc.txt\n+++ doc.txt\n@@ -1 +1 @@\n-patched\n+proc_patched\n') doc.txt
cat doc.txt
printf 'cmdout_payload\n' | sponge >(sponge cmdout_saved.txt)
wait
cat cmdout_saved.txt
`)
	if psRC != 0 {
		t.Fatalf("process substitution inside strict workspace jail failed: rc=%d out=%q stderr=%q", psRC, psOut, psErr)
	}
	for _, want := range []string{"cHJvYy1hbHBoYQ==", "4f4b", "proc_patched", "cmdout_payload"} {
		if !strings.Contains(psOut, want) {
			t.Fatalf("process substitution output missing %q: got %q", want, psOut)
		}
	}

	// Escape inside <(...) subshell must still be blocked by inherited CapabilityHook!
	leakOut, _, _ := runJailedScript(t, ctx, ws, nil, `
column <(base64 ../outside/secret.txt)
column <(xxd -p ../outside/secret.txt)
column <(rg line1 ../outside/secret.txt)
`)
	if strings.TrimSpace(leakOut) != "" {
		t.Fatalf("SECURITY: command inside <(...) bypassed jail and leaked secret: %q", leakOut)
	}
}

func TestJailFS_ConcurrentMultiDirIsolationWithoutChdir(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	const numSessions = 16
	dirs := make([]string, numSessions)
	for i := 0; i < numSessions; i++ {
		dirs[i] = t.TempDir()
	}

	var wg sync.WaitGroup
	errCh := make(chan error, numSessions)

	for i := 0; i < numSessions; i++ {
		i := i
		ws := dirs[i]
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Exercise both CapabilityHook-enabled and zero-overhead unhooked sessions concurrently!
			ctx := context.Background()
			if i%2 == 0 {
				hook := newFSPolicyHook(t, ws)
				ctx = WithCapabilityHook(ctx, hook)
			}

			token := fmt.Sprintf("SESSION_TOKEN_%02d", i)
			script := fmt.Sprintf(`
set -e
mkdir -p tree/sub
printf '%s\n' > tree/sub/data.txt
cp tree/sub/data.txt tree/copy.txt
mv tree/copy.txt tree/moved.txt
chmod 644 tree/moved.txt
ln tree/moved.txt tree/hard.txt
touch tree/touched.txt

find tree -type f | sort > found.list
ls tree > ls.list
rg --no-heading '%s' tree > rg.list

tar -cf bundle.tar tree
mkdir unpacked
tar -xf bundle.tar -C unpacked

cp tree/moved.txt gz.txt
gzip gz.txt
gunzip gz.txt.gz

patch tree/moved.txt <<'EOF'
--- tree/moved.txt
+++ tree/moved.txt
@@ -1 +1 @@
-%s
+%s_PATCHED
EOF

printf '%s_SPONGED\n' | sponge sponge.txt
xxd -p sponge.txt sponge.hex
xxd -r -p sponge.hex sponge_rt.txt
sha256sum sponge_rt.txt > check.sha256
sha256sum -c check.sha256 >/dev/null
flock -n session.lock -c 'cat unpacked/tree/sub/data.txt'
`, token, token, token, token, token)

			var outBuf, errBuf bytes.Buffer
			r, nerr := interp.New(
				interp.Dir(ws),
				interp.Env(expand.ListEnviron(os.Environ()...)),
				interp.StdIO(strings.NewReader(""), &outBuf, &errBuf),
				interp.CallHandler(callOverride),
				interp.OpenHandler(shellOpenHandler()),
				interp.ExecHandlers(trackExec, extraHandler),
				interp.ProcSubstHandler(procSubstHandler),
			)
			if nerr != nil {
				errCh <- fmt.Errorf("session %d interp.New: %w", i, nerr)
				return
			}
			prog, perr := syntax.NewParser().Parse(strings.NewReader(script), fmt.Sprintf("sess_%d.sh", i))
			if perr != nil {
				errCh <- fmt.Errorf("session %d parse: %w", i, perr)
				return
			}
			if rerr := r.Run(ctx, prog); rerr != nil {
				errCh <- fmt.Errorf("session %d run failed: %v (stderr=%q)", i, rerr, errBuf.String())
				return
			}
			wantOut := "patching file tree/moved.txt\n" + token
			if got := strings.TrimSpace(strings.ReplaceAll(outBuf.String(), "\r\n", "\n")); got != wantOut {
				errCh <- fmt.Errorf("session %d unexpected stdout %q, want %q", i, got, wantOut)
				return
			}

			foundBytes, ferr := os.ReadFile(filepath.Join(ws, "found.list"))
			if ferr != nil {
				errCh <- fmt.Errorf("session %d read found.list: %w", i, ferr)
				return
			}
			wantFound := "tree/hard.txt\ntree/moved.txt\ntree/sub/data.txt\ntree/touched.txt"
			if got := strings.TrimSpace(strings.ReplaceAll(string(foundBytes), "\r\n", "\n")); got != wantFound {
				errCh <- fmt.Errorf("session %d find output = %q, want %q", i, got, wantFound)
				return
			}

			rgBytes, gerr := os.ReadFile(filepath.Join(ws, "rg.list"))
			if gerr != nil {
				errCh <- fmt.Errorf("session %d read rg.list: %w", i, gerr)
				return
			}
			for _, line := range strings.Split(strings.TrimSpace(string(rgBytes)), "\n") {
				if !strings.HasPrefix(line, "tree/") || !strings.Contains(line, token) {
					errCh <- fmt.Errorf("session %d bad rg line: %q", i, line)
					return
				}
			}

			curWD, werr := os.Getwd()
			if werr != nil || curWD != origWD {
				errCh <- fmt.Errorf("session %d mutated process cwd: got %q, want %q (err=%v)", i, curWD, origWD, werr)
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	afterWD, err := os.Getwd()
	if err != nil || afterWD != origWD {
		t.Fatalf("process cwd changed after concurrent sessions: got %q, want %q", afterWD, origWD)
	}
}

func TestJailFS_FlockShellHandlersAndNumericFD(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("FLOCK_OUTSIDE_SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "inside.txt"), []byte("inside_ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "script.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	hook := newFSPolicyHook(t, ws)
	ctx := WithCapabilityHook(context.Background(), hook)

	// 1. Glob expansion inside `flock -c` must enforce OpList via flockReadDirHandler.
	out, _, rc := runJailedScript(t, ctx, ws, nil, `flock -n app.lock -c 'echo ../outside/*'`)
	if rc != 0 {
		t.Fatalf("flock -c unexpanded glob exited %d", rc)
	}
	if got := strings.TrimSpace(out); got != "../outside/*" {
		t.Fatalf("flock -c leaked outside directory listing via glob: got %q, want literal %q", got, "../outside/*")
	}

	// 2. [[ -f / -e / -r / -w ]] and redirections inside `flock -c` must enforce OpStat / OpRead / OpWrite.
	escapedChecks := []string{
		`flock -n app.lock -c '[[ -e ../outside/secret.txt ]]'`,
		`flock -n app.lock -c '[[ -f ../outside/secret.txt ]]'`,
		`flock -n app.lock -c '[[ -r ../outside/secret.txt ]]'`,
		`flock -n app.lock -c '[[ -w ../outside/secret.txt ]]'`,
		`flock -n app.lock -c 'cd ../outside'`,
		`flock -n app.lock -c 'echo pwn > ../outside/pwn.txt'`,
		`flock -n app.lock -c 'read line < ../outside/secret.txt && printf "%s" "$line"'`,
	}
	for _, sc := range escapedChecks {
		o, _, code := runJailedScript(t, ctx, ws, nil, sc)
		if code == 0 {
			t.Fatalf("expected flock -c escape check to fail, succeeded: %s (stdout=%q)", sc, o)
		}
		if strings.Contains(o, "FLOCK_OUTSIDE_SECRET") {
			t.Fatalf("flock -c leaked secret on %s: %q", sc, o)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "pwn.txt")); !os.IsNotExist(err) {
		t.Fatalf("flock -c redirection wrote outside jail: err=%v", err)
	}

	// 3. Inside-workspace glob, stat, read, and `cd subdir` work even when OpExec is denied,
	// while external exec and [[ -x file ]] inside `flock -c` are blocked by OpExec.
	hook.setDenyOp(OpExec, true)
	out, stderr, rc := runJailedScript(t, ctx, ws, nil, `flock -n app.lock -c '[[ -f inside.txt && -r inside.txt ]] && cd sub && [[ -d . ]] && echo sub_ok'`)
	if rc != 0 || strings.TrimSpace(out) != "sub_ok" {
		t.Fatalf("flock -c legitimate workspace ops failed under OpExec deny: rc=%d out=%q stderr=%q", rc, out, stderr)
	}
	if _, _, code := runJailedScript(t, ctx, ws, nil, `flock -n app.lock -c '[[ -x script.sh ]]'`); code == 0 {
		t.Fatalf("expected [[ -x script.sh ]] inside flock -c to fail when OpExec is denied")
	}
	extBin := findHostExternalBin(t)
	if _, _, code := runJailedScript(t, ctx, ws, nil, fmt.Sprintf(`flock -n app.lock %s version`, extBin)); code != 126 {
		t.Fatalf("expected flock external subcommand %s to exit 126 when OpExec is denied, got %d", extBin, code)
	}
	if _, _, code := runJailedScript(t, ctx, ws, nil, fmt.Sprintf(`flock -n app.lock -c 'xargs %s < inside.txt'`, extBin)); code == 0 {
		t.Fatalf("expected xargs %s inside flock -c to fail when OpExec is denied", extBin)
	}
	hook.setDenyOp(OpExec, false)

	// 4. Numeric FD mode (`flock <fd>`): hold a real open host *os.File outside the jail
	// and verify neither jailed sessions nor embedded library sessions can probe/lock/unlock it.
	hostFile, err := os.CreateTemp(outside, "host-open-fd-*")
	if err != nil {
		t.Fatal(err)
	}
	defer hostFile.Close()
	hostFD := int(hostFile.Fd())

	for _, tc := range []struct {
		name string
		ctx  context.Context
		cmd  string
	}{
		{"jailed_host_fd_lock", ctx, fmt.Sprintf("flock -n %d", hostFD)},
		{"jailed_host_fd_shared", ctx, fmt.Sprintf("flock -s -n %d", hostFD)},
		{"jailed_host_fd_unlock", ctx, fmt.Sprintf("flock -u %d", hostFD)},
		{"jailed_fd9", ctx, "flock -n 9"},
		{"embedded_unjailed_host_fd_lock", context.Background(), fmt.Sprintf("flock -n %d", hostFD)},
		{"embedded_unjailed_host_fd_unlock", context.Background(), fmt.Sprintf("flock -u %d", hostFD)},
		{"embedded_unjailed_fd9", context.Background(), "flock -n 9"},
	} {
		_, errOut, code := runJailedScript(t, tc.ctx, ws, nil, tc.cmd)
		if code != 1 {
			t.Fatalf("%s (%q): got exit code %d, want 1 (stderr=%q)", tc.name, tc.cmd, code, errOut)
		}
		if !strings.Contains(strings.ToLower(errOut), "bad file descriptor") {
			t.Fatalf("%s (%q): expected bad file descriptor diagnostic, got %q", tc.name, tc.cmd, errOut)
		}
	}
}

// nonComparableClosureHook is a value-receiver CapabilityHook containing a closure,
// a map, and a slice so reflect.TypeOf(h).Comparable() is guaranteed false.
type nonComparableClosureHook struct {
	check func(context.Context, AccessRequest) error
	tags  map[string]bool
	rules []string
}

func makeNonComparableClosureHook(t *testing.T, root string) nonComparableClosureHook {
	t.Helper()
	base := newTestJailHook(t, root)
	return nonComparableClosureHook{
		check: func(ctx context.Context, req AccessRequest) error {
			return base.CheckAccess(ctx, req)
		},
		tags:  map[string]bool{"jailed": true},
		rules: []string{root},
	}
}

func (h nonComparableClosureHook) CheckAccess(ctx context.Context, req AccessRequest) error {
	return h.check(ctx, req)
}

func (h nonComparableClosureHook) DialContext(ctx context.Context, cmdName, network, address string) (net.Conn, error) {
	return nil, &AccessDenialError{Op: OpNetConnect, Target: address, Reason: "network disabled"}
}

func (h nonComparableClosureHook) Listen(ctx context.Context, cmdName, network, address string) (net.Listener, error) {
	return nil, &AccessDenialError{Op: OpNetListen, Target: address, Reason: "listen disabled"}
}

func (h nonComparableClosureHook) ListenPacket(ctx context.Context, cmdName, network, address string) (net.PacketConn, error) {
	return nil, &AccessDenialError{Op: OpNetListen, Target: address, Reason: "listen disabled"}
}

func waitForPublishedPath(t *testing.T, file string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(file); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for published procsubst path in %s", file)
	return ""
}

func TestJailFS_ProcSubstCrossSessionIsolationAndNonComparableHooks(t *testing.T) {
	base := t.TempDir()
	wsA := filepath.Join(base, "wsA")
	wsB := filepath.Join(base, "wsB")
	wsUnjailed := filepath.Join(base, "wsUnjailed")
	for _, d := range []string{wsA, wsB, wsUnjailed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wsB, "dummy.txt"), []byte("dummy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsB, "b_in.txt"), []byte("evil_hex_source\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hookA := newFSPolicyHook(t, wsA)
	hookB := newFSPolicyHook(t, wsB)
	ctxA := WithCapabilityHook(context.Background(), hookA)
	ctxB := WithCapabilityHook(context.Background(), hookB)

	// 1. Cross-hook `<(...)` (CmdIn) isolation: Session A holds `<(...)` alive while Session B probes it.
	doneA := make(chan struct{})
	var outA, errA string
	var rcA int
	go func() {
		defer close(doneA)
		outA, errA, rcA = runJailedScript(t, ctxA, wsA, nil, `
hold_in() {
	local p="$1"
	printf '%s\n' "$p" > ps_in.tmp
	mv ps_in.tmp ps_in.txt
	until [[ -f release_in.flag ]]; do
		sleep 0.01
	done
	xxd -p "$p" > self_in.hex
}
hold_in <(printf 'TENANT_A_SECRET')
`)
	}()

	procInA := waitForPublishedPath(t, filepath.Join(wsA, "ps_in.txt"))
	if !isActiveProcSubstPath(ctxA, procInA) {
		t.Fatalf("expected procInA %q to be active for ctxA", procInA)
	}
	if isActiveProcSubstPath(ctxB, procInA) {
		t.Fatalf("procInA %q must NOT be active for ctxB", procInA)
	}

	// Session B attempts to read Session A's live `<(...)` path through every procsubst-aware builtin.
	for _, cmd := range []string{
		`xxd -p "$PS_TARGET"`,
		`column "$PS_TARGET"`,
		`rg '' "$PS_TARGET"`,
		`patch -i "$PS_TARGET" dummy.txt`,
		`tar -tf "$PS_TARGET"`,
	} {
		bOut, _, bRC := runJailedScript(t, ctxB, wsB, []string{"PS_TARGET=" + procInA}, cmd)
		if bRC == 0 {
			t.Fatalf("Session B unexpectedly succeeded reading Session A procsubst via %q (out=%q)", cmd, bOut)
		}
		if strings.Contains(bOut, "TENANT_A_SECRET") || strings.Contains(strings.ToLower(bOut), "54454e414e54") {
			t.Fatalf("Session B leaked Session A procsubst content via %q: %q", cmd, bOut)
		}
	}

	if err := os.WriteFile(filepath.Join(wsA, "release_in.flag"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	<-doneA
	if rcA != 0 {
		t.Fatalf("Session A CmdIn holder failed: rc=%d out=%q stderr=%q", rcA, outA, errA)
	}
	hexA, err := os.ReadFile(filepath.Join(wsA, "self_in.hex"))
	if err != nil || !strings.Contains(strings.ToLower(string(hexA)), "54454e414e545f415f534543524554") {
		t.Fatalf("Session A failed to consume its own procsubst: hex=%q err=%v", string(hexA), err)
	}

	// 2. Cross-hook `>(...)` (CmdOut) isolation: Session A holds `>(...)` alive while Session B tries to inject into it.
	doneOutA := make(chan struct{})
	var errOutA string
	var rcOutA int
	go func() {
		defer close(doneOutA)
		_, errOutA, rcOutA = runJailedScript(t, ctxA, wsA, nil, `
hold_out() {
	local p="$1"
	printf '%s\n' "$p" > ps_out.tmp
	mv ps_out.tmp ps_out.txt
	until [[ -f release_out.flag ]]; do
		sleep 0.01
	done
	printf 'LEGIT_FROM_A\n' | sponge "$p"
}
hold_out >(sponge sink_a.txt)
wait
`)
	}()

	procOutA := waitForPublishedPath(t, filepath.Join(wsA, "ps_out.txt"))
	for _, cmd := range []string{
		`printf 'INJECTED_BY_B\n' | sponge "$PS_TARGET"`,
		`xxd -p b_in.txt "$PS_TARGET"`,
	} {
		_, _, bRC := runJailedScript(t, ctxB, wsB, []string{"PS_TARGET=" + procOutA}, cmd)
		if bRC == 0 {
			t.Fatalf("Session B unexpectedly wrote to Session A CmdOut procsubst via %q", cmd)
		}
	}

	if err := os.WriteFile(filepath.Join(wsA, "release_out.flag"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	<-doneOutA
	if rcOutA != 0 {
		t.Fatalf("Session A CmdOut holder failed: rc=%d stderr=%q", rcOutA, errOutA)
	}
	sinkBytes, err := os.ReadFile(filepath.Join(wsA, "sink_a.txt"))
	if err != nil || strings.TrimSpace(string(sinkBytes)) != "LEGIT_FROM_A" {
		t.Fatalf("unexpected sink_a.txt content=%q err=%v", string(sinkBytes), err)
	}

	// 3. Unjailed session's `<(...)` path MUST NOT be readable by a jailed session.
	doneUnjailed := make(chan struct{})
	go func() {
		defer close(doneUnjailed)
		_, _, _ = runJailedScript(t, context.Background(), wsUnjailed, nil, `
hold_u() {
	local p="$1"
	printf '%s\n' "$p" > ps_u.tmp
	mv ps_u.tmp ps_u.txt
	until [[ -f release_u.flag ]]; do
		sleep 0.01
	done
	xxd -p "$p" >/dev/null
}
hold_u <(printf 'UNJAILED_HOST_SECRET')
`)
	}()
	procUnjailed := waitForPublishedPath(t, filepath.Join(wsUnjailed, "ps_u.txt"))
	if isActiveProcSubstPath(ctxB, procUnjailed) {
		t.Fatalf("unjailed procsubst %q must NOT be treated as active for jailed ctxB", procUnjailed)
	}
	if bOut, _, bRC := runJailedScript(t, ctxB, wsB, []string{"PS_TARGET=" + procUnjailed}, `xxd -p "$PS_TARGET"`); bRC == 0 || strings.Contains(strings.ToLower(bOut), "554e4a41494c4544") {
		t.Fatalf("jailed Session B accessed unjailed procsubst: rc=%d out=%q", bRC, bOut)
	}
	_ = os.WriteFile(filepath.Join(wsUnjailed, "release_u.flag"), []byte("ok"), 0o644)
	<-doneUnjailed

	// 4. Non-comparable CapabilityHook concrete type (closure + map + slice):
	// must NOT panic, must work within its own session (including flock -c), and must isolate across two closures!
	ncHookA := makeNonComparableClosureHook(t, wsA)
	ncHookB := makeNonComparableClosureHook(t, wsB)
	if reflect.TypeOf(ncHookA).Comparable() {
		t.Fatalf("expected nonComparableClosureHook to be non-comparable")
	}
	ncCtxA := WithCapabilityHook(context.Background(), ncHookA)
	ncCtxB := WithCapabilityHook(context.Background(), ncHookB)

	ncOut, ncErr, ncRC := runJailedScript(t, ncCtxA, wsA, nil, `
set -e
xxd -p <(printf 'NC_OK') > nc.hex
printf 'NC_SINK\n' | sponge >(sponge nc_sink.txt)
wait
flock -n nc.lock -c 'xxd -p <(printf "FLOCK_NC")'
`)
	if ncRC != 0 {
		t.Fatalf("non-comparable hook procsubst failed: rc=%d out=%q stderr=%q", ncRC, ncOut, ncErr)
	}
	if !strings.Contains(strings.ToLower(ncOut), "464c4f434b5f4e43") {
		t.Fatalf("unexpected flock -c procsubst output under non-comparable hook: %q", ncOut)
	}

	doneNCA := make(chan struct{})
	go func() {
		defer close(doneNCA)
		_, _, _ = runJailedScript(t, ncCtxA, wsA, nil, `
hold_nc() {
	local p="$1"
	printf '%s\n' "$p" > ps_nc.tmp
	mv ps_nc.tmp ps_nc.txt
	until [[ -f release_nc.flag ]]; do
		sleep 0.01
	done
	xxd -p "$p" >/dev/null
}
hold_nc <(printf 'NC_CLOSURE_SECRET_A')
`)
	}()
	procNCA := waitForPublishedPath(t, filepath.Join(wsA, "ps_nc.txt"))
	if bOut, _, bRC := runJailedScript(t, ncCtxB, wsB, []string{"PS_TARGET=" + procNCA}, `xxd -p "$PS_TARGET"`); bRC == 0 || strings.Contains(strings.ToLower(bOut), "4e435f434c4f53555245") {
		t.Fatalf("ncCtxB bypassed closure hook isolation to read ncCtxA procsubst: rc=%d out=%q", bRC, bOut)
	}
	_ = os.WriteFile(filepath.Join(wsA, "release_nc.flag"), []byte("ok"), 0o644)
	<-doneNCA
}

type customErrDeleteDenyHook struct {
	base *testJailHook
}

func (h *customErrDeleteDenyHook) CheckAccess(ctx context.Context, req AccessRequest) error {
	if req.Op == OpDelete {
		// Deliberately does NOT wrap os.ErrPermission to ensure fsAtomicWriteFile still avoids temp orphans.
		return errors.New("custom policy: delete not permitted")
	}
	return h.base.CheckAccess(ctx, req)
}

func (h *customErrDeleteDenyHook) DialContext(ctx context.Context, cmdName, network, address string) (net.Conn, error) {
	return h.base.DialContext(ctx, cmdName, network, address)
}

func (h *customErrDeleteDenyHook) Listen(ctx context.Context, cmdName, network, address string) (net.Listener, error) {
	return h.base.Listen(ctx, cmdName, network, address)
}

func (h *customErrDeleteDenyHook) ListenPacket(ctx context.Context, cmdName, network, address string) (net.PacketConn, error) {
	return h.base.ListenPacket(ctx, cmdName, network, address)
}

func TestJailFS_AtomicWriteDeleteDeniedNoOrphanTempFiles(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "doc.txt"), []byte("old_line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hook := newFSPolicyHook(t, ws)
	hook.setDenyOp(OpDelete, true) // OpWrite allowed, OpDelete denied!
	ctx := WithCapabilityHook(context.Background(), hook)

	script := `
set -e
patch doc.txt <<'EOF'
--- doc.txt
+++ doc.txt
@@ -1 +1 @@
-old_line
+new_line
EOF
printf 'sponge_first\n' | sponge sp.txt
printf 'sponge_second\n' | sponge -a sp.txt
xxd -p sp.txt sp.hex
xxd -r -p sp.hex sp_rt.txt
`
	out, stderr, rc := runJailedScript(t, ctx, ws, nil, script)
	if rc != 0 {
		t.Fatalf("atomic writes with OpDelete denied failed: rc=%d out=%q stderr=%q", rc, out, stderr)
	}

	gotDoc, err := os.ReadFile(filepath.Join(ws, "doc.txt"))
	if err != nil || string(gotDoc) != "new_line\n" {
		t.Fatalf("doc.txt = %q (err=%v), want %q", string(gotDoc), err, "new_line\n")
	}
	gotRT, err := os.ReadFile(filepath.Join(ws, "sp_rt.txt"))
	if err != nil || string(gotRT) != "sponge_first\nsponge_second\n" {
		t.Fatalf("sp_rt.txt = %q (err=%v)", string(gotRT), err)
	}

	// Also verify custom non-ErrPermission OpDelete denial leaves zero orphan temp files.
	customCtx := WithCapabilityHook(context.Background(), &customErrDeleteDenyHook{base: newTestJailHook(t, ws)})
	if _, cErr, cRC := runJailedScript(t, customCtx, ws, nil, `printf 'custom_ok\n' | sponge custom.txt`); cRC != 0 {
		t.Fatalf("custom delete-deny hook sponge failed: rc=%d stderr=%q", cRC, cErr)
	}

	var orphans []string
	err = filepath.WalkDir(ws, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if strings.HasPrefix(d.Name(), ".unish-atomic-") {
			orphans = append(orphans, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) > 0 {
		t.Fatalf("found orphan .unish-atomic-* temp files when OpDelete is denied: %v", orphans)
	}
}
