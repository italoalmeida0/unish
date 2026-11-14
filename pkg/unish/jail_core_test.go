package unish

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io/fs"
	"net"
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

type testJailHook struct {
	mu          sync.Mutex
	allowedRoot string
	deniedPaths map[string]bool
	requests    []AccessRequest
	dialCalls   int
	listenCalls int
	packetCalls int
}

func newTestJailHook(t *testing.T, root string) *testJailHook {
	t.Helper()
	realRoot, _ := canonicalizeForCheck(root)
	return &testJailHook{
		allowedRoot: realRoot,
		deniedPaths: make(map[string]bool),
	}
}

func (h *testJailHook) isWithinAllowed(p string) bool {
	if p == "" {
		return false
	}
	clean := normalizeWinVerbatim(filepath.Clean(p))
	if h.deniedPaths[clean] {
		return false
	}
	if runtime.GOOS == "windows" {
		for k := range h.deniedPaths {
			if strings.EqualFold(k, clean) {
				return false
			}
		}
		if strings.EqualFold(clean, h.allowedRoot) {
			return true
		}
		prefix := h.allowedRoot
		if !strings.HasSuffix(prefix, `\`) {
			prefix += `\`
		}
		return len(clean) > len(prefix) && strings.EqualFold(clean[:len(prefix)], prefix)
	}
	if clean == h.allowedRoot {
		return true
	}
	prefix := h.allowedRoot
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return strings.HasPrefix(clean, prefix)
}

func (h *testJailHook) CheckAccess(_ context.Context, req AccessRequest) error {
	h.mu.Lock()
	h.requests = append(h.requests, req)
	h.mu.Unlock()

	switch req.Op {
	case OpExec, OpProc:
		if req.Op == OpExec && h.isWithinAllowed(req.Path) {
			return nil
		}
		target := req.Path
		if target == "" && req.PID != 0 {
			target = "pid"
		}
		return &AccessDenialError{Op: req.Op, Target: target, Reason: "blocked by test policy"}
	default:
		if !h.isWithinAllowed(req.Path) {
			return &AccessDenialError{Op: req.Op, Target: req.Path, Reason: "outside jail root"}
		}
		return nil
	}
}

func (h *testJailHook) DialContext(_ context.Context, _, _, address string) (net.Conn, error) {
	h.mu.Lock()
	h.dialCalls++
	h.mu.Unlock()
	return nil, &AccessDenialError{Op: OpNetConnect, Target: address, Reason: "outbound network disabled"}
}

func (h *testJailHook) Listen(_ context.Context, _, _, address string) (net.Listener, error) {
	h.mu.Lock()
	h.listenCalls++
	h.mu.Unlock()
	return nil, &AccessDenialError{Op: OpNetListen, Target: address, Reason: "listen disabled"}
}

func (h *testJailHook) ListenPacket(_ context.Context, _, _, address string) (net.PacketConn, error) {
	h.mu.Lock()
	h.packetCalls++
	h.mu.Unlock()
	return nil, &AccessDenialError{Op: OpNetListen, Target: address, Reason: "udp listen disabled"}
}

func TestJailCore_TypesErrorsAndContextPlumbing(t *testing.T) {
	valErr := AccessDenialError{Op: OpRead, Target: "/etc/passwd", Reason: "outside root"}
	ptrErr := &AccessDenialError{Op: OpWrite, Target: "/etc/shadow", Reason: "read-only"}

	if !errors.Is(valErr, os.ErrPermission) || !errors.Is(valErr, fs.ErrPermission) {
		t.Fatalf("value AccessDenialError must unwrap to os.ErrPermission / fs.ErrPermission: %v", valErr)
	}
	if !errors.Is(ptrErr, os.ErrPermission) || !errors.Is(ptrErr, fs.ErrPermission) {
		t.Fatalf("pointer AccessDenialError must unwrap to os.ErrPermission / fs.ErrPermission: %v", ptrErr)
	}
	if !strings.Contains(valErr.Error(), "permission denied") || !strings.Contains(valErr.Error(), "/etc/passwd") {
		t.Fatalf("unexpected Error() string: %q", valErr.Error())
	}

	if DecisionAsk.String() != "ask" || DecisionAllow.String() != "allow" ||
		DecisionAllowSession.String() != "allow_session" || DecisionDeny.String() != "deny" {
		t.Fatalf("unexpected Decision.String() values")
	}

	if CapabilityHookFromContext(nil) != nil || CapabilityHookFromContext(context.Background()) != nil {
		t.Fatal("expected nil CapabilityHook on plain background context")
	}

	root := t.TempDir()
	hook := newTestJailHook(t, root)
	ctx := WithCapabilityHook(context.Background(), hook)
	if got := CapabilityHookFromContext(ctx); got != hook {
		t.Fatalf("CapabilityHookFromContext = %v, want %v", got, hook)
	}
	cleared := WithCapabilityHook(ctx, nil)
	if got := CapabilityHookFromContext(cleared); got != nil {
		t.Fatalf("expected nil hook after clearing child context, got %v", got)
	}
}

func TestJailCore_FSWrappersAndSymlinkEscapes(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("TOP_SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	hook := newTestJailHook(t, ws)
	ctx := WithCapabilityHook(context.Background(), hook)

	// 1. Relative operations inside workspace succeed without touching os.Getwd().
	if err := fsWriteFile(ctx, ws, "tee", "hello.txt", []byte("world\n"), 0o644); err != nil {
		t.Fatalf("fsWriteFile inside jail failed: %v", err)
	}
	got, err := fsReadFile(ctx, ws, "cat", "hello.txt")
	if err != nil || string(got) != "world\n" {
		t.Fatalf("fsReadFile inside jail = %q, %v", string(got), err)
	}
	if _, err := fsStat(ctx, ws, "stat", "hello.txt"); err != nil {
		t.Fatalf("fsStat inside jail failed: %v", err)
	}
	entries, err := fsReadDir(ctx, ws, "ls", ".")
	if err != nil || len(entries) != 1 {
		t.Fatalf("fsReadDir inside jail = %d entries, err=%v", len(entries), err)
	}

	// 2. Direct path traversal (../outside/secret.txt) is denied.
	if _, err := fsReadFile(ctx, ws, "cat", filepath.Join("..", "outside", "secret.txt")); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected permission denial on ../outside/secret.txt, got %v", err)
	}

	// 3. Null device (/dev/null) read/write/stat works inside jail, but mutating metadata/unlinking is denied.
	if err := fsWriteFile(ctx, ws, "redirect", "/dev/null", []byte("discard"), 0o644); err != nil {
		t.Fatalf("writing /dev/null inside jail failed: %v", err)
	}
	if b, err := fsReadFile(ctx, ws, "cat", "/dev/null"); err != nil || len(b) != 0 {
		t.Fatalf("reading /dev/null inside jail = %q, %v", string(b), err)
	}
	if err := fsRemove(ctx, ws, "rm", "/dev/null"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected rm /dev/null to be denied by jail, got %v", err)
	}

	// 4. fsRename checks OpDelete on src + OpWrite on dst, and blocks escaping dst.
	if err := fsRename(ctx, ws, "mv", "hello.txt", filepath.Join("..", "outside", "stolen.txt")); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected fsRename outside jail to fail with ErrPermission, got %v", err)
	}
	if err := fsRename(ctx, ws, "mv", "hello.txt", "renamed.txt"); err != nil {
		t.Fatalf("fsRename within jail failed: %v", err)
	}

	// 5. Temp file/dir creation inside allowed workspace works, while default os.TempDir() outside jail is denied.
	if _, err := fsCreateTemp(ctx, ws, "mktemp", "", "unish-jail-*"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected fsCreateTemp in os.TempDir() outside jail to be denied, got %v", err)
	}
	tf, err := fsCreateTemp(ctx, ws, "mktemp", ".", "unish-jail-*")
	if err != nil {
		t.Fatalf("fsCreateTemp inside jail failed: %v", err)
	}
	tfName := tf.Name()
	_ = tf.Close()
	if err := fsRemove(ctx, ws, "rm", tfName); err != nil {
		t.Fatalf("fsRemove temp file failed: %v", err)
	}

	// 6. Symlink escapes (pre-existing target, dangling target, intermediate dir symlink, and intra-jail deny rule).
	escapeLink := filepath.Join(ws, "escape_read")
	if err := os.Symlink(secretFile, escapeLink); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable without privilege on this Windows host: %v", err)
		}
		t.Fatalf("os.Symlink: %v", err)
	}

	// Reading through pre-existing escape symlink MUST be denied!
	if _, err := fsOpen(ctx, ws, "cat", "escape_read"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("SECURITY: fsOpen followed symlink outside jail: %v", err)
	}

	// Dangling symlink pointing to non-existent file outside jail MUST be denied BEFORE creation!
	outsidePwn := filepath.Join(outside, "pwned_via_dangling.txt")
	danglingLink := filepath.Join(ws, "escape_dangling")
	if err := os.Symlink(outsidePwn, danglingLink); err != nil {
		t.Fatalf("os.Symlink dangling: %v", err)
	}
	if err := fsWriteFile(ctx, ws, "tee", "escape_dangling", []byte("PWNED"), 0o644); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("SECURITY: fsWriteFile through dangling escape symlink succeeded: %v", err)
	}
	if _, statErr := os.Stat(outsidePwn); !os.IsNotExist(statErr) {
		t.Fatalf("SECURITY: dangling symlink created file outside jail: %v", statErr)
	}

	// Intermediate directory symlink pointing outside jail with non-existent subpath MUST be denied!
	dirLink := filepath.Join(ws, "escape_dir")
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Fatalf("os.Symlink dir: %v", err)
	}
	if err := fsMkdirAll(ctx, ws, "mkdir", filepath.Join("escape_dir", "nested", "new"), 0o755); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("SECURITY: fsMkdirAll through directory symlink succeeded: %v", err)
	}

	// Creating an escape symlink via fsSymlink MUST be denied via OpLink!
	if err := fsSymlink(ctx, ws, "ln", filepath.Join("..", "outside", "secret.txt"), "new_escape"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("SECURITY: fsSymlink allowed creating escape symlink: %v", err)
	}

	// Intra-workspace symlink to a file works, and fsRename renames the symlink itself rather than its target!
	if err := fsSymlink(ctx, ws, "ln", "renamed.txt", "good_link"); err != nil {
		t.Fatalf("intra-workspace fsSymlink failed: %v", err)
	}
	if err := fsRename(ctx, ws, "mv", "good_link", "moved_link"); err != nil {
		t.Fatalf("fsRename on intra-workspace symlink failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(ws, "renamed.txt")); err != nil {
		t.Fatalf("fsRename on symlink accidentally moved target renamed.txt: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(ws, "moved_link")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("moved_link is not a symlink: fi=%v err=%v", fi, err)
	}

	// Lexical deny rule cannot be bypassed by symlinking denied .env -> public file!
	realEnvPath := filepath.Join(hook.allowedRoot, ".env")
	hook.deniedPaths[realEnvPath] = true
	if err := os.Symlink("renamed.txt", filepath.Join(ws, ".env")); err != nil {
		t.Fatalf("os.Symlink .env: %v", err)
	}
	if _, err := fsReadFile(ctx, ws, "cat", ".env"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("SECURITY: reading denied .env symlink succeeded: %v", err)
	}

	// Symlink cycle terminates cleanly without infinite recursion.
	loopA := filepath.Join(ws, "loop_a")
	loopB := filepath.Join(ws, "loop_b")
	_ = os.Symlink("loop_b", loopA)
	_ = os.Symlink("loop_a", loopB)
	done := make(chan struct{})
	go func() {
		_, _ = canonicalizeForCheck(loopA)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("canonicalizeForCheck hung on symlink loop")
	}
}

func TestJailCore_NetExecProcGateways(t *testing.T) {
	ws := t.TempDir()
	hook := newTestJailHook(t, ws)
	ctx := WithCapabilityHook(context.Background(), hook)

	if _, err := netDial(ctx, "nc", "tcp", "127.0.0.1:80", time.Second); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected netDial denial, got %v", err)
	}
	if _, err := netListen(ctx, "nc", "tcp", "127.0.0.1:0"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected netListen denial, got %v", err)
	}
	if _, err := netListenPacket(ctx, "nc", "udp", "127.0.0.1:0"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected netListenPacket denial, got %v", err)
	}
	if hook.dialCalls != 1 || hook.listenCalls != 1 || hook.packetCalls != 1 {
		t.Fatalf("unexpected net hook counts: dial=%d listen=%d packet=%d", hook.dialCalls, hook.listenCalls, hook.packetCalls)
	}

	argv := []string{"curl", "https://example.com"}
	if _, err := checkExec(ctx, "curl", "/usr/bin/curl", argv, ws); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected checkExec denial, got %v", err)
	}
	if err := checkProc(ctx, "kill", 12345, "TERM"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected checkProc denial, got %v", err)
	}
}

func TestEmbeddedLibrary_NoGlobalFlagsAndInProcessBackgroundBuiltin(t *testing.T) {
	// 1. Importing package unish and calling RunCLI must NOT register -c/-i/-V on global flag.CommandLine.
	for _, name := range []string{"c", "i", "V", "version"} {
		if f := flag.CommandLine.Lookup(name); f != nil {
			t.Fatalf("package unish polluted global flag.CommandLine with -%s", name)
		}
	}

	// Re-entrant RunCLI calls must work cleanly without polluting global flag.CommandLine.
	if rc := RunCLI("9.9.9-test", []string{"unish", "-c", "true"}); rc != 0 {
		t.Fatalf("RunCLI(-c true) = %d, want 0", rc)
	}
	if rc := RunCLI("9.9.9-test", []string{"unish", "-c", "(exit 17)"}); rc != 17 {
		t.Fatalf("RunCLI(-c 'exit 17') = %d, want 17", rc)
	}
	if standaloneCLIMode.Load() {
		t.Fatal("standaloneCLIMode leaked true after RunCLI returned")
	}

	// 2. When embedded as a Go library (!standaloneCLIMode), running a simple background
	// builtin (`sleep 0.05 &`) must NOT invoke `os.Executable() -c` (which in `go test` is
	// `unish.test` and would fail with `flag provided but not defined: -c`, exit status 2)
	// and must NOT block `$!` expansion while the background builtin runs!
	var stdout, stderr bytes.Buffer
	r, err := interp.New(
		interp.StdIO(strings.NewReader(""), &stdout, &stderr),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
		interp.Dir(t.TempDir()),
	)
	if err != nil {
		t.Fatalf("interp.New: %v", err)
	}
	script := `
sleep 0.25 &
p=$!
printf 'started\n'
wait $p
printf 'rc=%d\n' "$?"
`
	prog, err := syntax.NewParser().Parse(strings.NewReader(script), "embedded_bg.sh")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Run(ctx, prog); err != nil {
		t.Fatalf("embedded background builtin failed: %v (stderr=%q)", err, stderr.String())
	}
	if got := stdout.String(); got != "started\nrc=0\n" || stderr.String() != "" {
		t.Fatalf("embedded background builtin got out=%q stderr=%q, want %q", got, stderr.String(), "started\nrc=0\n")
	}
}
