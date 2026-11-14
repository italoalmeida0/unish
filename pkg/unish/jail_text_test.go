package unish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

type testWorkspaceJail struct {
	root            string
	allowExec       bool
	allowExecInRoot bool
	allowProc       bool
	denyDelete      bool

	mu      sync.Mutex
	denials []AccessRequest
}

func newTestWorkspaceJail(t testing.TB, root string) *testWorkspaceJail {
	t.Helper()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = filepath.Clean(root)
	}
	return &testWorkspaceJail{
		root: filepath.Clean(realRoot),
	}
}

func isPathWithinJailRoot(candidate, root string) bool {
	if candidate == "" || root == "" {
		return false
	}
	c := filepath.Clean(candidate)
	r := filepath.Clean(root)
	if runtime.GOOS == "windows" {
		c = strings.ToLower(c)
		r = strings.ToLower(r)
	}
	if c == r {
		return true
	}
	sep := string(filepath.Separator)
	if strings.HasSuffix(r, sep) {
		return strings.HasPrefix(c, r)
	}
	return strings.HasPrefix(c, r+sep)
}

func (j *testWorkspaceJail) recordDenial(req AccessRequest) {
	j.mu.Lock()
	j.denials = append(j.denials, req)
	j.mu.Unlock()
}

func (j *testWorkspaceJail) resetDenials() {
	j.mu.Lock()
	j.denials = nil
	j.mu.Unlock()
}

func (j *testWorkspaceJail) snapshotDenials() []AccessRequest {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]AccessRequest, len(j.denials))
	copy(out, j.denials)
	return out
}

func (j *testWorkspaceJail) hasDeniedOp(op AccessOp) bool {
	for _, d := range j.snapshotDenials() {
		if d.Op == op {
			return true
		}
	}
	return false
}

func (j *testWorkspaceJail) CheckAccess(_ context.Context, req AccessRequest) error {
	switch req.Op {
	case OpExec:
		if j.allowExec {
			return nil
		}
		if j.allowExecInRoot && isPathWithinJailRoot(req.Path, j.root) {
			return nil
		}
		j.recordDenial(req)
		return AccessDenialError{
			Op:     OpExec,
			Target: req.Binary,
			Reason: "external binary execution blocked by workspace jail",
		}
	case OpProc:
		if j.allowProc {
			return nil
		}
		j.recordDenial(req)
		return AccessDenialError{
			Op:     OpProc,
			Target: req.Command,
			Reason: "process control blocked by workspace jail",
		}
	case OpRead, OpWrite, OpDelete, OpList, OpStat, OpChdir, OpLink:
		if req.Op == OpDelete && j.denyDelete {
			j.recordDenial(req)
			return AccessDenialError{
				Op:     OpDelete,
				Target: req.RawPath,
				Reason: "delete blocked by workspace policy",
			}
		}
		if isNullDevPath(req.Path) || isNullDevPath(req.RawPath) {
			return nil
		}
		if !isPathWithinJailRoot(req.Path, j.root) {
			j.recordDenial(req)
			return AccessDenialError{
				Op:     req.Op,
				Target: req.RawPath,
				Reason: "outside workspace jail",
			}
		}
		if req.TargetPath != "" && !isPathWithinJailRoot(req.TargetPath, j.root) {
			j.recordDenial(req)
			return AccessDenialError{
				Op:     req.Op,
				Target: req.TargetPath,
				Reason: "target outside workspace jail",
			}
		}
		return nil
	default:
		j.recordDenial(req)
		return AccessDenialError{
			Op:     req.Op,
			Target: req.RawPath,
			Reason: "operation denied by workspace jail",
		}
	}
}

func (j *testWorkspaceJail) DialContext(_ context.Context, cmdName, network, address string) (net.Conn, error) {
	req := AccessRequest{Op: OpNetConnect, Command: cmdName, Proto: network, Host: address}
	j.recordDenial(req)
	return nil, AccessDenialError{Op: OpNetConnect, Target: address, Reason: "network disabled"}
}

func (j *testWorkspaceJail) Listen(_ context.Context, cmdName, network, address string) (net.Listener, error) {
	req := AccessRequest{Op: OpNetListen, Command: cmdName, Proto: network, Host: address}
	j.recordDenial(req)
	return nil, AccessDenialError{Op: OpNetListen, Target: address, Reason: "listen disabled"}
}

func (j *testWorkspaceJail) ListenPacket(_ context.Context, cmdName, network, address string) (net.PacketConn, error) {
	req := AccessRequest{Op: OpNetListen, Command: cmdName, Proto: network, Host: address}
	j.recordDenial(req)
	return nil, AccessDenialError{Op: OpNetListen, Target: address, Reason: "listen disabled"}
}

func runJailedShell(t testing.TB, dir string, jail CapabilityHook, extraEnv []string, script string) (string, string, int) {
	t.Helper()
	prog, err := syntax.NewParser().Parse(strings.NewReader(script), "jail_test.sh")
	if err != nil {
		t.Fatalf("parse script %q: %v", script, err)
	}
	var stdout, stderr bytes.Buffer
	baseEnv := []string{
		"HOME=" + dir,
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + dir,
		"TEMP=" + dir,
		"TMP=" + dir,
	}
	baseEnv = append(baseEnv, extraEnv...)

	r, err := interp.New(
		interp.Dir(dir),
		interp.Env(expand.ListEnviron(baseEnv...)),
		interp.StdIO(strings.NewReader(""), &stdout, &stderr),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ReadDirHandler2(shellReadDirHandler()),
		interp.StatHandler(shellStatHandler()),
		interp.AccessHandler(shellAccessHandler()),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		t.Fatalf("interp.New: %v", err)
	}

	ctx := context.Background()
	if jail != nil {
		ctx = WithCapabilityHook(ctx, jail)
	}
	runErr := r.Run(ctx, prog)
	code := 0
	if runErr != nil {
		var es interp.ExitStatus
		var ee exitError
		switch {
		case errors.As(runErr, &es):
			code = int(es)
		case errors.As(runErr, &ee):
			code = ee.code
		default:
			code = 1
		}
	}
	return strings.ReplaceAll(stdout.String(), "\r\n", "\n"), strings.ReplaceAll(stderr.String(), "\r\n", "\n"), code
}

func setupJailFixture(t *testing.T) (workDir, outsideDir string, jail *testWorkspaceJail) {
	t.Helper()
	parent := t.TempDir()
	workDir = filepath.Join(parent, "workspace")
	outsideDir = filepath.Join(parent, "outside")
	if err := os.MkdirAll(filepath.Join(workDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "local.txt"), []byte("hello local\nalpha line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "sub", "nested.txt"), []byte("needle line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("TOP_SECRET_CANARY_99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.json"), []byte(`{"secret":"TOP_SECRET_CANARY_99"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "evil.sh"), []byte("echo EVIL_SOURCED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jail = newTestWorkspaceJail(t, workDir)
	return workDir, outsideDir, jail
}

func TestJailShellRedirectsAndSource(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)
	outSlash := filepath.ToSlash(outsideDir)

	cases := []struct {
		name   string
		script string
		wantOp AccessOp
	}{
		{
			name:   "write_redirect_outside",
			script: fmt.Sprintf(`echo pwn > %q`, outSlash+"/pwn.txt"),
			wantOp: OpWrite,
		},
		{
			name:   "append_redirect_outside",
			script: fmt.Sprintf(`echo pwn >> %q`, outSlash+"/secret.txt"),
			wantOp: OpWrite,
		},
		{
			name:   "read_redirect_outside",
			script: fmt.Sprintf(`cat < %q`, outSlash+"/secret.txt"),
			wantOp: OpRead,
		},
		{
			name:   "exec_fd3_read_outside",
			script: fmt.Sprintf(`exec 3< %q`, outSlash+"/secret.txt"),
			wantOp: OpRead,
		},
		{
			name:   "exec_fd3_write_outside",
			script: fmt.Sprintf(`exec 3> %q`, outSlash+"/fd3.txt"),
			wantOp: OpWrite,
		},
		{
			name:   "all_redirect_outside",
			script: fmt.Sprintf(`echo pwn &> %q`, outSlash+"/all.txt"),
			wantOp: OpWrite,
		},
		{
			name:   "append_all_redirect_outside",
			script: fmt.Sprintf(`echo pwn &>> %q`, outSlash+"/all_append.txt"),
			wantOp: OpWrite,
		},
		{
			name:   "dotdot_escape_redirect",
			script: `echo pwn > ../escape.txt`,
			wantOp: OpWrite,
		},
		{
			name:   "source_outside",
			script: fmt.Sprintf(`source %q`, outSlash+"/evil.sh"),
			wantOp: OpRead,
		},
		{
			name:   "dot_source_outside",
			script: fmt.Sprintf(`. %q`, outSlash+"/evil.sh"),
			wantOp: OpRead,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jail.resetDenials()
			out, _, code := runJailedShell(t, workDir, jail, nil, tc.script)
			if code == 0 {
				t.Fatalf("expected non-zero exit for %s, got 0 (stdout=%q)", tc.name, out)
			}
			if strings.Contains(out, "TOP_SECRET_CANARY") || strings.Contains(out, "EVIL_SOURCED") {
				t.Fatalf("leaked outside content: %q", out)
			}
			if !jail.hasDeniedOp(tc.wantOp) {
				t.Fatalf("expected %s denial, got denials=%+v", tc.wantOp, jail.snapshotDenials())
			}
		})
	}

	// Also verify <> redirect fails in shell and O_RDWR is gated in shellOpenHandlerFn.
	if out, _, code := runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`exec 3<> %q`, outSlash+"/secret.txt")); code == 0 || strings.Contains(out, "TOP_SECRET_CANARY") {
		t.Fatalf("exec 3<> outside should fail: code=%d out=%q", code, out)
	}
	jail.resetDenials()
	rdwrProbe, _ := syntax.NewParser().Parse(strings.NewReader("probe_rdwr"), "")
	rdwrRunner, err := interp.New(
		interp.Dir(workDir),
		interp.CallHandler(func(ctx context.Context, args []string) ([]string, error) {
			f, oerr := shellOpenHandlerFn(ctx, outSlash+"/secret.txt", os.O_RDWR|os.O_CREATE, 0o644)
			if oerr == nil {
				_ = f.Close()
				t.Errorf("shellOpenHandlerFn O_RDWR outside unexpectedly succeeded")
			}
			return nil, interp.CallHandled(0)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = rdwrRunner.Run(WithCapabilityHook(context.Background(), jail), rdwrProbe)
	if !jail.hasDeniedOp(OpRead) {
		t.Fatalf("expected OpRead denial on O_RDWR outside, got %+v", jail.snapshotDenials())
	}

	// Verify outside files were untouched and no new files created.
	if _, err := os.Stat(filepath.Join(outsideDir, "pwn.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside pwn.txt should not exist, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "fd3.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside fd3.txt should not exist, err=%v", err)
	}
	secretData, err := os.ReadFile(filepath.Join(outsideDir, "secret.txt"))
	if err != nil || string(secretData) != "TOP_SECRET_CANARY_99\n" {
		t.Fatalf("outside secret.txt modified: %q (err=%v)", string(secretData), err)
	}

	// Symlink redirect escape check when host OS permits symlink creation.
	symIn := filepath.Join(workDir, "sym_secret.txt")
	if err := os.Symlink(filepath.Join(outsideDir, "secret.txt"), symIn); err == nil {
		jail.resetDenials()
		out, _, code := runJailedShell(t, workDir, jail, nil, `cat < sym_secret.txt`)
		if code == 0 || strings.Contains(out, "TOP_SECRET_CANARY") || !jail.hasDeniedOp(OpRead) {
			t.Fatalf("symlink redirect escaped jail: code=%d out=%q denials=%+v", code, out, jail.snapshotDenials())
		}
		_ = os.Remove(symIn)
	}

	// Verify allowed inside-jail redirects, source, and zero-syscall /dev/null fast-path.
	if err := os.WriteFile(filepath.Join(workDir, "sub", "lib.sh"), []byte("LIB_VAR=inside_ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jail.resetDenials()
	allowedScript := `
echo first > sub/redir.txt
echo second >> sub/redir.txt
exec 3> sub/fd3_ok.txt
echo fd3_line >&3
exec 3>&-
source sub/lib.sh
echo fast > /dev/null
printf "%s\n" fast2 > /dev/null
cat < sub/redir.txt
cat sub/fd3_ok.txt
echo "$LIB_VAR"
`
	out, stderr, code := runJailedShell(t, workDir, jail, nil, allowedScript)
	if code != 0 {
		t.Fatalf("inside-jail redirects failed (code=%d): stderr=%q", code, stderr)
	}
	want := "first\nsecond\nfd3_line\ninside_ok\n"
	if out != want {
		t.Fatalf("got stdout %q, want %q", out, want)
	}
	if len(jail.snapshotDenials()) != 0 {
		t.Fatalf("unexpected denials for inside-jail ops: %+v", jail.snapshotDenials())
	}
}

func TestJailGlobNoFilenameLeak(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)
	outSlash := filepath.ToSlash(outsideDir)

	jail.resetDenials()
	out, _, code := runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`echo %s/*`, outSlash))
	if code != 0 {
		t.Fatalf("echo with unmatched glob should exit 0, got %d", code)
	}
	if strings.Contains(out, "secret.txt") || strings.Contains(out, "evil.sh") {
		t.Fatalf("glob leaked outside filenames: %q", out)
	}
	if strings.TrimSpace(out) != outSlash+"/*" {
		t.Fatalf("expected literal unexpanded pattern %q, got %q", outSlash+"/*", strings.TrimSpace(out))
	}
	if !jail.hasDeniedOp(OpList) {
		t.Fatalf("expected OpList denial for outside glob, got %+v", jail.snapshotDenials())
	}

	jail.resetDenials()
	out, _, _ = runJailedShell(t, workDir, jail, nil, `shopt -s nullglob; echo ../*`)
	if strings.Contains(out, "outside") || !jail.hasDeniedOp(OpList) {
		t.Fatalf("parent glob leaked sibling dir: out=%q denials=%+v", out, jail.snapshotDenials())
	}

	// Inside-workspace globs work normally.
	jail.resetDenials()
	out, stderr, code := runJailedShell(t, workDir, jail, nil, `echo sub/*.txt`)
	if code != 0 || strings.TrimSpace(out) != "sub/nested.txt" {
		t.Fatalf("inside glob failed: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestJailChdirPushdPopd(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)
	outSlash := filepath.ToSlash(outsideDir)

	escapeCases := []struct {
		name   string
		script string
		env    []string
	}{
		{"cd_outside", fmt.Sprintf(`cd %q`, outSlash), nil},
		{"cd_dotdot", `cd ..`, nil},
		{"cd_L_dotdot", `cd -L ..`, nil},
		{"cd_P_dotdot", `cd -P ..`, nil},
		{"cd_home_outside", `cd`, []string{"HOME=" + outsideDir}},
		{"cd_oldpwd_outside", `cd -`, []string{"OLDPWD=" + outsideDir}},
		{"builtin_cd_outside", fmt.Sprintf(`builtin cd %q`, outSlash), nil},
		{"command_cd_outside", fmt.Sprintf(`command cd %q`, outSlash), nil},
		{"pushd_outside", fmt.Sprintf(`pushd %q`, outSlash), nil},
		{"pushd_n_outside", fmt.Sprintf(`pushd -n %q`, outSlash), nil},
	}

	for _, tc := range escapeCases {
		t.Run(tc.name, func(t *testing.T) {
			jail.resetDenials()
			_, _, code := runJailedShell(t, workDir, jail, tc.env, tc.script)
			if code == 0 {
				t.Fatalf("%s unexpectedly succeeded", tc.name)
			}
			if !jail.hasDeniedOp(OpChdir) {
				t.Fatalf("%s did not record OpChdir denial: %+v", tc.name, jail.snapshotDenials())
			}
		})
	}

	// Verify failed cd keeps shell working directory inside workspace.
	out, _, _ := runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`cd %q 2>/dev/null; cat local.txt`, outSlash))
	if !strings.Contains(out, "hello local") {
		t.Fatalf("working directory changed after denied cd: out=%q", out)
	}

	// Symlink directory escape check when supported.
	symDir := filepath.Join(workDir, "sym_outside_dir")
	if err := os.Symlink(outsideDir, symDir); err == nil {
		jail.resetDenials()
		_, _, code := runJailedShell(t, workDir, jail, nil, `cd sym_outside_dir`)
		if code == 0 || !jail.hasDeniedOp(OpChdir) {
			t.Fatalf("cd via symlink dir escaped jail: code=%d denials=%+v", code, jail.snapshotDenials())
		}
		_ = os.Remove(symDir)
	}

	// Allowed inside-jail cd / pushd / popd.
	jail.resetDenials()
	out, stderr, code := runJailedShell(t, workDir, jail, nil, `
cd sub && cat nested.txt && cd .. && pushd sub >/dev/null && cat nested.txt && popd >/dev/null && cat local.txt
`)
	if code != 0 {
		t.Fatalf("inside-jail cd/pushd/popd failed (code=%d): %q", code, stderr)
	}
	if !strings.Contains(out, "needle line\nneedle line\nhello local\n") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestJailTestAndDoubleBracketPredicates(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)
	secretSlash := filepath.ToSlash(filepath.Join(outsideDir, "secret.txt"))
	outSlash := filepath.ToSlash(outsideDir)

	outsideChecks := []string{
		fmt.Sprintf(`[ -e %q ]`, secretSlash),
		fmt.Sprintf(`[ -f %q ]`, secretSlash),
		fmt.Sprintf(`[ -d %q ]`, outSlash),
		fmt.Sprintf(`[ -s %q ]`, secretSlash),
		fmt.Sprintf(`[ -r %q ]`, secretSlash),
		fmt.Sprintf(`[ -w %q ]`, secretSlash),
		fmt.Sprintf(`[ -L %q ]`, secretSlash),
		fmt.Sprintf(`[ %q -nt local.txt ]`, secretSlash),
		fmt.Sprintf(`[ local.txt -ot %q ]`, secretSlash),
		fmt.Sprintf(`[ local.txt -ef %q ]`, secretSlash),
		fmt.Sprintf(`test -f %q`, secretSlash),
		fmt.Sprintf(`builtin [ -f %q ]`, secretSlash),
		fmt.Sprintf(`command test -f %q`, secretSlash),
		fmt.Sprintf(`[[ -e %q ]]`, secretSlash),
		fmt.Sprintf(`[[ -f %q ]]`, secretSlash),
		fmt.Sprintf(`[[ -d %q ]]`, outSlash),
		fmt.Sprintf(`[[ -r %q ]]`, secretSlash),
		fmt.Sprintf(`[[ -w %q ]]`, secretSlash),
		fmt.Sprintf(`[[ -x %q ]]`, outSlash),
	}

	for _, cmd := range outsideChecks {
		jail.resetDenials()
		_, _, code := runJailedShell(t, workDir, jail, nil, cmd)
		if code != 1 {
			t.Fatalf("expected exit 1 (false, no existence leak) for %s, got %d", cmd, code)
		}
		if len(jail.snapshotDenials()) == 0 {
			t.Fatalf("expected capability denial recorded for %s", cmd)
		}
	}

	// Pure string/integer predicates do not touch FS and succeed inside jail with 0 denials.
	jail.resetDenials()
	_, stderr, code := runJailedShell(t, workDir, jail, nil, `
[ -n "abc" ] && [ -z "" ] && [ "x" = "x" ] && [ 5 -gt 3 ] &&
[ -f local.txt ] && [[ -f local.txt ]] && [ -d sub ] && [[ -d sub ]] && [ -e /dev/null ]
`)
	if code != 0 {
		t.Fatalf("inside-jail test predicates failed (code=%d): %q", code, stderr)
	}
	if len(jail.snapshotDenials()) != 0 {
		t.Fatalf("unexpected denials for valid predicates: %+v", jail.snapshotDenials())
	}
}

func TestJailExternalBinaryBlocked126(t *testing.T) {
	workDir, _, jail := setupJailFixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	exeSlash := filepath.ToSlash(exe)

	jail.resetDenials()
	_, stderr, code := runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`%q -version`, exeSlash))
	if code != 126 {
		t.Fatalf("expected exit 126 when external binary blocked by jail, got %d (stderr=%q)", code, stderr)
	}
	if stderr == "" {
		t.Fatalf("expected diagnostic on stderr when external binary blocked")
	}
	if !jail.hasDeniedOp(OpExec) {
		t.Fatalf("expected OpExec denial, got %+v", jail.snapshotDenials())
	}

	// Background execution of blocked external binary must also fail cleanly (126) without deadlocking $!/wait.
	jail.resetDenials()
	_, _, bgCode := runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`%q -version & bg=$!; wait "$bg"`, exeSlash))
	if bgCode != 126 || !jail.hasDeniedOp(OpExec) {
		t.Fatalf("expected background external binary block 126, got %d (denials=%+v)", bgCode, jail.snapshotDenials())
	}
}

func TestJailExtraCatAndWhich(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)
	secretSlash := filepath.ToSlash(filepath.Join(outsideDir, "secret.txt"))

	jail.resetDenials()
	out, _, code := runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`cat %q`, secretSlash))
	if code == 0 || strings.Contains(out, "TOP_SECRET_CANARY") || !jail.hasDeniedOp(OpRead) {
		t.Fatalf("cat outside not blocked: code=%d out=%q denials=%+v", code, out, jail.snapshotDenials())
	}

	jail.resetDenials()
	out, _, code = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`which %q`, secretSlash))
	if code == 0 || out != "" || !jail.hasDeniedOp(OpStat) {
		t.Fatalf("which outside path leaked: code=%d out=%q denials=%+v", code, out, jail.snapshotDenials())
	}

	jail.resetDenials()
	out, _, code = runJailedShell(t, workDir, jail, nil, `which grep`)
	if code != 0 || !strings.Contains(out, "grep: unish builtin") {
		t.Fatalf("which builtin failed inside jail: code=%d out=%q", code, out)
	}
}

func TestJailSedCapabilities(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)
	outSlash := filepath.ToSlash(outsideDir)
	secretSlash := outSlash + "/secret.txt"
	if err := os.WriteFile(filepath.Join(outsideDir, "script.sed"), []byte("s/hello/pwned/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. -f outside script
	jail.resetDenials()
	out, _, code := runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`sed -f %q local.txt`, outSlash+"/script.sed"))
	if code == 0 || !jail.hasDeniedOp(OpRead) {
		t.Fatalf("sed -f outside should fail: code=%d out=%q denials=%+v", code, out, jail.snapshotDenials())
	}

	// 2. input file outside
	jail.resetDenials()
	out, _, code = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`sed 's/TOP/X/' %q`, secretSlash))
	if code == 0 || strings.Contains(out, "SECRET_CANARY") || !jail.hasDeniedOp(OpRead) {
		t.Fatalf("sed input outside should fail: code=%d out=%q", code, out)
	}

	// 3. sed -i outside
	jail.resetDenials()
	_, _, code = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`sed -i 's/TOP/PWN/' %q`, secretSlash))
	if code == 0 {
		t.Fatalf("sed -i outside should fail")
	}
	if data, _ := os.ReadFile(filepath.Join(outsideDir, "secret.txt")); string(data) != "TOP_SECRET_CANARY_99\n" {
		t.Fatalf("sed -i modified outside file: %q", string(data))
	}

	// 4. sed -i through workspace symlink pointing outside
	symTarget := filepath.Join(workDir, "sym_sed.txt")
	if err := os.Symlink(filepath.Join(outsideDir, "secret.txt"), symTarget); err == nil {
		jail.resetDenials()
		_, _, code = runJailedShell(t, workDir, jail, nil, `sed -i 's/TOP/PWN/' sym_sed.txt`)
		if code == 0 {
			t.Fatalf("sed -i symlink escape succeeded")
		}
		if data, _ := os.ReadFile(filepath.Join(outsideDir, "secret.txt")); string(data) != "TOP_SECRET_CANARY_99\n" {
			t.Fatalf("sed -i symlink modified outside file: %q", string(data))
		}
		_ = os.Remove(symTarget)
	}

	// 5. sed 'r /outside/secret.txt' must not leak outside file contents
	jail.resetDenials()
	out, _, _ = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`sed '1r %s' local.txt`, secretSlash))
	if strings.Contains(out, "TOP_SECRET_CANARY") || !jail.hasDeniedOp(OpRead) {
		t.Fatalf("sed r command leaked outside file: out=%q denials=%+v", out, jail.snapshotDenials())
	}

	// 6. sed 'w /outside/pwn.txt' and 's/.../.../w /outside/pwn2.txt'
	jail.resetDenials()
	_, _, code = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`sed 'w %s/sed_w.txt' local.txt`, outSlash))
	if code == 0 || !jail.hasDeniedOp(OpWrite) {
		t.Fatalf("sed w outside should fail: code=%d denials=%+v", code, jail.snapshotDenials())
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "sed_w.txt")); !os.IsNotExist(err) {
		t.Fatalf("sed w created outside file")
	}

	jail.resetDenials()
	_, _, code = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`sed 's/hello/hi/w %s/sed_sw.txt' local.txt`, outSlash))
	if code == 0 || !jail.hasDeniedOp(OpWrite) {
		t.Fatalf("sed s///w outside should fail: code=%d denials=%+v", code, jail.snapshotDenials())
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "sed_sw.txt")); !os.IsNotExist(err) {
		t.Fatalf("sed s///w created outside file")
	}

	// 7. Allowed inside-jail sed -i.bak, r, w
	if err := os.WriteFile(filepath.Join(workDir, "edit.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jail.resetDenials()
	out, stderr, code := runJailedShell(t, workDir, jail, nil, `
sed -i.bak 's/before/after/' edit.txt
sed -n 's/after/written/w sed_out.txt' edit.txt
cat edit.txt edit.txt.bak sed_out.txt
`)
	if code != 0 {
		t.Fatalf("inside-jail sed failed (code=%d): %q", code, stderr)
	}
	if out != "after\nbefore\nwritten\n" {
		t.Fatalf("unexpected inside-jail sed output: %q", out)
	}
}

func TestJailAwkCapabilities(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)
	outSlash := filepath.ToSlash(outsideDir)
	secretSlash := outSlash + "/secret.txt"
	if err := os.WriteFile(filepath.Join(outsideDir, "prog.awk"), []byte("BEGIN { print \"OUTSIDE_PROG\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. -f progfile outside
	jail.resetDenials()
	out, _, code := runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`awk -f %q`, outSlash+"/prog.awk"))
	if code == 0 || strings.Contains(out, "OUTSIDE_PROG") || !jail.hasDeniedOp(OpRead) {
		t.Fatalf("awk -f outside not blocked: code=%d out=%q", code, out)
	}

	// 2. input file outside
	jail.resetDenials()
	out, _, code = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`awk '{print}' %q`, secretSlash))
	if code == 0 || strings.Contains(out, "TOP_SECRET_CANARY") || !jail.hasDeniedOp(OpRead) {
		t.Fatalf("awk input outside not blocked: code=%d out=%q", code, out)
	}

	// 3. print > outside and print >> outside
	jail.resetDenials()
	_, _, code = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`awk -v p=%q 'BEGIN { print "pwn" > p }'`, outSlash+"/awk_out.txt"))
	if code == 0 || !jail.hasDeniedOp(OpWrite) {
		t.Fatalf("awk > outside not blocked: code=%d denials=%+v", code, jail.snapshotDenials())
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "awk_out.txt")); !os.IsNotExist(err) {
		t.Fatalf("awk > created outside file")
	}

	jail.resetDenials()
	_, _, code = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`awk -v p=%q 'BEGIN { print "pwn" >> p }'`, secretSlash))
	if code == 0 || !jail.hasDeniedOp(OpWrite) {
		t.Fatalf("awk >> outside not blocked: code=%d denials=%+v", code, jail.snapshotDenials())
	}

	// 4. getline < outside
	jail.resetDenials()
	out, _, _ = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`awk -v p=%q 'BEGIN { rc = (getline x < p); print rc, x }'`, secretSlash))
	if strings.Contains(out, "TOP_SECRET_CANARY") || !strings.HasPrefix(strings.TrimSpace(out), "-1") || !jail.hasDeniedOp(OpRead) {
		t.Fatalf("awk getline < outside not blocked: out=%q denials=%+v", out, jail.snapshotDenials())
	}

	// 5. system() inherits CapabilityHook + ReadDirHandler + StatHandler
	jail.resetDenials()
	out, _, _ = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`awk -v p=%q 'BEGIN { rc = system("cat " p); print "rc=" rc }'`, secretSlash))
	if strings.Contains(out, "TOP_SECRET_CANARY") || !strings.Contains(out, "rc=1") || !jail.hasDeniedOp(OpRead) {
		t.Fatalf("awk system(cat outside) bypassed jail: out=%q denials=%+v", out, jail.snapshotDenials())
	}

	jail.resetDenials()
	out, _, _ = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`awk -v d=%q 'BEGIN { system("echo " d "/*") }'`, outSlash))
	if strings.Contains(out, "secret.txt") || !jail.hasDeniedOp(OpList) {
		t.Fatalf("awk system(glob outside) leaked filenames: out=%q denials=%+v", out, jail.snapshotDenials())
	}

	jail.resetDenials()
	out, _, _ = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`awk -v p=%q 'BEGIN { rc = system("[[ -f " p " ]]"); print "rc=" rc }'`, secretSlash))
	if !strings.Contains(out, "rc=1") || !jail.hasDeniedOp(OpStat) {
		t.Fatalf("awk system([[ -f outside ]]) bypassed StatHandler: out=%q denials=%+v", out, jail.snapshotDenials())
	}

	// 6. cmd | getline and print | cmd
	jail.resetDenials()
	out, _, _ = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`awk -v p=%q 'BEGIN { cmd = "cat " p; rc = (cmd | getline x); close(cmd); print rc, x }'`, secretSlash))
	if strings.Contains(out, "TOP_SECRET_CANARY") || !jail.hasDeniedOp(OpRead) {
		t.Fatalf("awk cmd | getline bypassed jail: out=%q denials=%+v", out, jail.snapshotDenials())
	}

	jail.resetDenials()
	_, _, _ = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`awk -v p=%q 'BEGIN { cmd = "cat > " p; print "pwn" | cmd; close(cmd) }'`, outSlash+"/awk_pipe.txt"))
	if _, err := os.Stat(filepath.Join(outsideDir, "awk_pipe.txt")); !os.IsNotExist(err) {
		t.Fatalf("awk print | cmd created outside file")
	}

	// 7. Allowed inside-jail awk operations
	jail.resetDenials()
	out, stderr, code := runJailedShell(t, workDir, jail, nil, `
awk 'BEGIN { print "awk_ok" > "sub/awk.txt" }'
awk 'BEGIN { getline x < "sub/awk.txt"; print x; system("cat sub/nested.txt") }'
`)
	if code != 0 || out != "awk_ok\nneedle line\n" {
		t.Fatalf("inside-jail awk failed: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestJailJqCapabilities(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)
	outSlash := filepath.ToSlash(outsideDir)
	if err := os.WriteFile(filepath.Join(outsideDir, "f.jq"), []byte(".secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	escapeCmds := []string{
		fmt.Sprintf(`jq -n -f %q`, outSlash+"/f.jq"),
		fmt.Sprintf(`jq -n --rawfile s %q '$s'`, outSlash+"/secret.txt"),
		fmt.Sprintf(`jq -n --slurpfile s %q '$s'`, outSlash+"/secret.json"),
		fmt.Sprintf(`jq -n --argfile s %q '$s'`, outSlash+"/secret.json"),
		fmt.Sprintf(`jq -r .secret %q`, outSlash+"/secret.json"),
	}
	for _, cmd := range escapeCmds {
		jail.resetDenials()
		out, _, code := runJailedShell(t, workDir, jail, nil, cmd)
		if code == 0 || strings.Contains(out, "TOP_SECRET_CANARY") {
			t.Fatalf("jq escape succeeded for %s: code=%d out=%q", cmd, code, out)
		}
		if len(jail.snapshotDenials()) == 0 {
			t.Fatalf("expected denial recorded for %s", cmd)
		}
	}

	// Allowed inside-jail jq
	if err := os.WriteFile(filepath.Join(workDir, "in.json"), []byte(`{"k":"v"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "f.jq"), []byte(`{k: .k, raw: ($raw | gsub("\n"; ""))}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jail.resetDenials()
	out, stderr, code := runJailedShell(t, workDir, jail, nil, `jq -c --rawfile raw sub/nested.txt -f f.jq in.json`)
	if code != 0 || strings.TrimSpace(out) != `{"k":"v","raw":"needle line"}` {
		t.Fatalf("inside-jail jq failed: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestJailTextAndStreamBuiltins(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)
	outSlash := filepath.ToSlash(outsideDir)
	secretSlash := outSlash + "/secret.txt"

	blockedCmds := []struct {
		name string
		cmd  string
	}{
		{"grep_file", fmt.Sprintf(`grep TOP %q`, secretSlash)},
		{"grep_f_pattern", fmt.Sprintf(`grep -f %q local.txt`, secretSlash)},
		{"grep_r_outside", fmt.Sprintf(`grep -r TOP %q`, outSlash)},
		{"head_outside", fmt.Sprintf(`head %q`, secretSlash)},
		{"tail_outside", fmt.Sprintf(`tail %q`, secretSlash)},
		{"sort_input_outside", fmt.Sprintf(`sort %q`, secretSlash)},
		{"sort_o_outside", fmt.Sprintf(`sort -o %q local.txt`, outSlash+"/sorted.txt")},
		{"uniq_input_outside", fmt.Sprintf(`uniq %q`, secretSlash)},
		{"uniq_output_outside", fmt.Sprintf(`uniq local.txt %q`, outSlash+"/uniq.txt")},
		{"wc_outside", fmt.Sprintf(`wc %q`, secretSlash)},
		{"tee_outside", fmt.Sprintf(`echo pwn | tee %q`, outSlash+"/tee.txt")},
		{"tee_append_outside", fmt.Sprintf(`echo pwn | tee -a %q`, secretSlash)},
		{"split_input_outside", fmt.Sprintf(`split -l 1 %q sub/chunk_`, secretSlash)},
		{"split_prefix_outside", fmt.Sprintf(`split -l 1 local.txt %q`, outSlash+"/chunk_")},
		{"split_number_outside", fmt.Sprintf(`split -n 2 local.txt %q`, outSlash+"/nchunk_")},
		{"diff_outside", fmt.Sprintf(`diff local.txt %q`, secretSlash)},
		{"diff_u_outside", fmt.Sprintf(`diff -u local.txt %q`, secretSlash)},
		{"cmp_outside", fmt.Sprintf(`cmp local.txt %q`, secretSlash)},
		{"hexdump_outside", fmt.Sprintf(`hexdump -C %q`, secretSlash)},
		{"strings_outside", fmt.Sprintf(`strings %q`, secretSlash)},
		{"od_outside", fmt.Sprintf(`od -c %q`, secretSlash)},
		{"nl_outside", fmt.Sprintf(`nl %q`, secretSlash)},
		{"tac_outside", fmt.Sprintf(`tac %q`, secretSlash)},
		{"rev_outside", fmt.Sprintf(`rev %q`, secretSlash)},
		{"fold_outside", fmt.Sprintf(`fold %q`, secretSlash)},
		{"expand_outside", fmt.Sprintf(`expand %q`, secretSlash)},
		{"unexpand_outside", fmt.Sprintf(`unexpand %q`, secretSlash)},
		{"join_outside", fmt.Sprintf(`join local.txt %q`, secretSlash)},
		{"comm_outside", fmt.Sprintf(`comm local.txt %q`, secretSlash)},
		{"paste_outside", fmt.Sprintf(`paste local.txt %q`, secretSlash)},
		{"cut_outside", fmt.Sprintf(`cut -c1-5 %q`, secretSlash)},
	}

	for _, tc := range blockedCmds {
		t.Run(tc.name, func(t *testing.T) {
			jail.resetDenials()
			out, _, code := runJailedShell(t, workDir, jail, nil, tc.cmd)
			if code == 0 {
				t.Fatalf("%s unexpectedly exited 0 (out=%q)", tc.name, out)
			}
			if strings.Contains(out, "TOP_SECRET_CANARY") {
				t.Fatalf("%s leaked secret: %q", tc.name, out)
			}
			if len(jail.snapshotDenials()) == 0 {
				t.Fatalf("%s recorded no capability denials", tc.name)
			}
		})
	}

	// Verify grep -r does not escape via symlink inside workspace.
	symEscape := filepath.Join(workDir, "sub", "sym_out")
	if err := os.Symlink(outsideDir, symEscape); err == nil {
		jail.resetDenials()
		out, _, _ := runJailedShell(t, workDir, jail, nil, `grep -r TOP sub`)
		if strings.Contains(out, "TOP_SECRET_CANARY") {
			t.Fatalf("grep -r leaked via symlink: %q", out)
		}
		_ = os.Remove(symEscape)
	}

	// Verify display filenames stay relative inside hc.Dir and process substitution works.
	jail.resetDenials()
	script := `
grep -r needle sub
grep -r needle .
wc -l sub/nested.txt
sort -o sub/sorted.txt local.txt
cat sub/sorted.txt
echo tee_val | tee sub/tee.txt >/dev/null
cat sub/tee.txt
split -l 1 local.txt sub/sp_
cat sub/sp_aa sub/sp_ab
cat <(echo procsubst_ok)
`
	out, stderr, code := runJailedShell(t, workDir, jail, nil, script)
	if code != 0 {
		t.Fatalf("inside-jail text builtins failed (code=%d): stderr=%q", code, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("unexpected short output: %q", out)
	}
	if lines[0] != "sub/nested.txt:needle line" {
		t.Fatalf("grep -r sub display path mismatch: got %q, want %q", lines[0], "sub/nested.txt:needle line")
	}
	if lines[1] != "./sub/nested.txt:needle line" {
		t.Fatalf("grep -r . display path mismatch: got %q, want %q", lines[1], "./sub/nested.txt:needle line")
	}
	if !strings.HasSuffix(lines[2], " sub/nested.txt") {
		t.Fatalf("wc -l display path mismatch: got %q", lines[2])
	}
	if !strings.Contains(out, "alpha line\nhello local\ntee_val\nhello local\nalpha line\nprocsubst_ok") {
		t.Fatalf("unexpected text builtin output: %q", out)
	}
}

func TestJailConcurrentMultiDirSessions(t *testing.T) {
	const numSessions = 16
	baseDir := t.TempDir()

	dirs := make([]string, numSessions)
	jails := make([]*testWorkspaceJail, numSessions)
	for i := 0; i < numSessions; i++ {
		d := filepath.Join(baseDir, fmt.Sprintf("sess-%02d", i))
		if err := os.MkdirAll(filepath.Join(d, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		token := fmt.Sprintf("TOKEN_SESSION_%02d", i)
		if err := os.WriteFile(filepath.Join(d, "data.txt"), []byte("z_"+token+"\na_"+token+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "sub", "item.txt"), []byte("match_"+token+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "meta.json"), []byte(fmt.Sprintf(`{"id":%d}`, i)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dirs[i] = d
		jails[i] = newTestWorkspaceJail(t, d)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, numSessions)

	for i := 0; i < numSessions; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			myDir := dirs[idx]
			myJail := jails[idx]
			peerDir := filepath.ToSlash(dirs[(idx+1)%numSessions])
			myToken := fmt.Sprintf("TOKEN_SESSION_%02d", idx)
			peerToken := fmt.Sprintf("TOKEN_SESSION_%02d", (idx+1)%numSessions)

			script := fmt.Sprintf(`
if [ -f %q ]; then echo PEER_STAT_LEAK; exit 91; fi
if cat %q 2>/dev/null; then echo PEER_CAT_LEAK; exit 92; fi
if cd %q 2>/dev/null; then echo PEER_CD_LEAK; exit 93; fi

echo "redir_%d" > redir.txt
sort -o sorted.txt data.txt
sed -i 's/match_/sed_/' sub/item.txt
grep -r sed_ sub > grep.txt
awk '{ print "awk:" $0 > "awk.txt" }' redir.txt
jq -c --rawfile g grep.txt '{id: .id, g: ($g | gsub("\n"; ""))}' meta.json | tee out.json
split -l 1 sorted.txt chunk_
cat chunk_aa chunk_ab awk.txt
`, peerDir+"/data.txt", peerDir+"/data.txt", peerDir, idx)

			out, stderr, code := runJailedShell(t, myDir, myJail, nil, script)
			if code != 0 {
				errCh <- fmt.Errorf("session %d failed (code=%d): out=%q stderr=%q", idx, code, out, stderr)
				return
			}
			if strings.Contains(out, peerToken) || strings.Contains(out, "LEAK") {
				errCh <- fmt.Errorf("session %d leaked peer data: %q", idx, out)
				return
			}
			wantJSON := fmt.Sprintf(`{"id":%d,"g":"sub/item.txt:sed_%s"}`, idx, myToken)
			wantTail := fmt.Sprintf("a_%s\nz_%s\nawk:redir_%d\n", myToken, myToken, idx)
			want := wantJSON + "\n" + wantTail
			if out != want {
				errCh <- fmt.Errorf("session %d output mismatch:\ngot:  %q\nwant: %q", idx, out, want)
				return
			}
			if !myJail.hasDeniedOp(OpStat) || !myJail.hasDeniedOp(OpRead) || !myJail.hasDeniedOp(OpChdir) {
				errCh <- fmt.Errorf("session %d missing expected cross-session denials: %+v", idx, strconv.Itoa(len(myJail.snapshotDenials())))
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func writeDummyExecFile(t testing.TB, dir, baseName string) string {
	t.Helper()
	name := baseName
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		name += ".exe"
	}
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, []byte("#!/bin/sh\necho SHOULD_NOT_EXEC\n"), 0o755); err != nil {
		t.Fatalf("writeDummyExecFile %s: %v", full, err)
	}
	return full
}

func TestJailExecNoStatOracleMSYSAndENOEXEC(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)
	jail.allowExecInRoot = true
	outSlash := filepath.ToSlash(outsideDir)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	_ = writeDummyExecFile(t, outsideDir, "secret_exec")

	// 1. Explicit outside paths (existing exec, non-exec file, directory, nonexistent, MSYS)
	// MUST all hit checkExec FIRST and return 126 with OpExec denial (never 127 or os.Stat oracle).
	explicitTargets := []string{
		outSlash + "/secret_exec",
		outSlash + "/nonexistent_secret_probe",
		outSlash + "/secret.txt",
		outSlash,
		filepath.ToSlash(exe),
	}
	if runtime.GOOS == "windows" && len(outsideDir) >= 3 && outsideDir[1] == ':' {
		drive := strings.ToLower(outsideDir[:1])
		tail := strings.ReplaceAll(outsideDir[2:], `\`, "/")
		explicitTargets = append(explicitTargets, "/"+drive+tail+"/nonexistent_msys_probe")
		explicitTargets = append(explicitTargets, "/"+drive+tail+"/secret.txt")
	}

	for _, target := range explicitTargets {
		jail.resetDenials()
		_, stderr, code := runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`%q`, target))
		if code != 126 {
			t.Fatalf("explicit outside exec %q: expected 126 (no stat oracle), got %d (stderr=%q)", target, code, stderr)
		}
		if !jail.hasDeniedOp(OpExec) {
			t.Fatalf("explicit outside exec %q: expected OpExec denial, got %+v", target, jail.snapshotDenials())
		}
		if jail.hasDeniedOp(OpStat) {
			t.Fatalf("explicit outside exec %q: unexpected OpStat before checkExec: %+v", target, jail.snapshotDenials())
		}
	}

	// 2. Bare command lookup must ONLY use fsStat on $PATH (never raw LookPathDir)
	// and must not execute <hc.Dir>/<bare> when not found on $PATH.
	_ = writeDummyExecFile(t, workDir, "cwd_only_bin")
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// With OpExec allowed globally, bare command not on $PATH returns 127 ("command not found")
	// even if a file of that name exists in hc.Dir.
	jailAllowAllExec := newTestWorkspaceJail(t, workDir)
	jailAllowAllExec.allowExec = true
	out, stderr, code := runJailedShell(t, workDir, jailAllowAllExec, []string{"PATH=" + binDir}, `cwd_only_bin`)
	if code != 127 || strings.Contains(out, "SHOULD_NOT_EXEC") {
		t.Fatalf("bare command not on PATH: expected 127, got %d (out=%q stderr=%q)", code, out, stderr)
	}

	// With PATH pointing outside, bare lookup records OpStat denial on outsideDir and never leaks/executes.
	jailAllowAllExec.resetDenials()
	out, _, code = runJailedShell(t, workDir, jailAllowAllExec, []string{"PATH=" + outsideDir}, `secret_exec`)
	if code != 127 || strings.Contains(out, "SHOULD_NOT_EXEC") || !jailAllowAllExec.hasDeniedOp(OpStat) {
		t.Fatalf("bare command on outside PATH: expected 127 + OpStat denial, got code=%d out=%q denials=%+v", code, out, jailAllowAllExec.snapshotDenials())
	}

	// With OpExec denied by jail, unknown bare command returns 126 with OpExec denial.
	jailDenyExec := newTestWorkspaceJail(t, workDir)
	_, _, code = runJailedShell(t, workDir, jailDenyExec, []string{"PATH=" + binDir}, `nonexistent_bare_xyz`)
	if code != 126 || !jailDenyExec.hasDeniedOp(OpExec) {
		t.Fatalf("bare command with OpExec denied: expected 126 + OpExec denial, got code=%d denials=%+v", code, jailDenyExec.snapshotDenials())
	}

	// 3. Unix ENOEXEC shebang-less +x script inside allowed root must run in a jailed sub-runner.
	if runtime.GOOS != "windows" {
		noShebang := filepath.Join(binDir, "noshebang_script")
		scriptBody := fmt.Sprintf(
			"echo \"arg1=$1\"\necho %s/*\ncat %q 2>/dev/null || echo \"cat_blocked\"\n",
			outSlash, outSlash+"/secret.txt",
		)
		if err := os.WriteFile(noShebang, []byte(scriptBody), 0o755); err != nil {
			t.Fatal(err)
		}
		jail.resetDenials()
		out, stderr, code := runJailedShell(t, workDir, jail, []string{"PATH=" + binDir}, `noshebang_script world`)
		if code != 0 {
			t.Fatalf("jailed ENOEXEC script failed (code=%d): out=%q stderr=%q", code, out, stderr)
		}
		if strings.Contains(out, "TOP_SECRET_CANARY") || strings.Contains(out, "secret.txt") {
			t.Fatalf("jailed ENOEXEC sub-runner leaked outside info: %q", out)
		}
		if !strings.Contains(out, "arg1=world\n") || !strings.Contains(out, outSlash+"/*\n") || !strings.Contains(out, "cat_blocked\n") {
			t.Fatalf("unexpected jailed ENOEXEC output: %q", out)
		}
		if !jail.hasDeniedOp(OpList) || !jail.hasDeniedOp(OpRead) {
			t.Fatalf("expected OpList and OpRead denials from jailed ENOEXEC sub-runner, got %+v", jail.snapshotDenials())
		}
	}
}

func TestJailCompgenAndPwd(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)
	_ = writeDummyExecFile(t, outsideDir, "secret_outside_tool")
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = writeDummyExecFile(t, binDir, "allowed_inside_tool")

	// 1. compgen / compgen -c / compgen -A command must not read outside $PATH dirs.
	for _, cmd := range []string{`compgen`, `compgen -c`, `compgen -A command`} {
		jail.resetDenials()
		out, stderr, code := runJailedShell(t, workDir, jail, []string{"PATH=" + outsideDir}, cmd)
		if code != 0 {
			t.Fatalf("%s failed (code=%d): stderr=%q", cmd, code, stderr)
		}
		if strings.Contains(out, "secret_outside_tool") || strings.Contains(out, "secret.txt") || strings.Contains(out, "evil.sh") {
			t.Fatalf("%s leaked outside PATH filenames: %q", cmd, out)
		}
		if !strings.Contains(out, "echo\n") || !strings.Contains(out, "grep\n") {
			t.Fatalf("%s missing expected builtins/extras: %q", cmd, out)
		}
		if !jail.hasDeniedOp(OpList) {
			t.Fatalf("%s did not record OpList denial on outside PATH: %+v", cmd, jail.snapshotDenials())
		}
	}

	// Allowed inside-jail PATH directory works in compgen -c.
	jail.resetDenials()
	out, stderr, code := runJailedShell(t, workDir, jail, []string{"PATH=" + binDir}, `compgen -c allowed_inside_`)
	if code != 0 || strings.TrimSpace(out) != "allowed_inside_tool" {
		t.Fatalf("compgen -c inside PATH failed: code=%d out=%q stderr=%q", code, out, stderr)
	}
	// In-memory compgen actions (-b, -A function) work cleanly.
	out, stderr, code = runJailedShell(t, workDir, jail, nil, `my_jail_fn() { :; }; compgen -b true; compgen -A function my_jail_`)
	if code != 0 || out != "true\nmy_jail_fn\n" {
		t.Fatalf("in-memory compgen failed: code=%d out=%q stderr=%q", code, out, stderr)
	}

	// 2. pwd / pwd -P with forged outside $PWD or outside symlink must never leak outside target.
	realWork, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		realWork = filepath.Clean(workDir)
	}
	for _, cmd := range []string{`pwd`, `pwd -P`, `builtin pwd -P`, `command pwd -P`} {
		jail.resetDenials()
		script := fmt.Sprintf(`PWD=%q; %s`, filepath.ToSlash(outsideDir), cmd)
		out, stderr, code := runJailedShell(t, workDir, jail, nil, script)
		if code != 0 {
			t.Fatalf("%s failed (code=%d): stderr=%q", cmd, code, stderr)
		}
		got := filepath.Clean(strings.TrimSpace(out))
		if strings.Contains(strings.ToLower(got), "outside") {
			t.Fatalf("%s leaked forged outside PWD: %q", cmd, got)
		}
		if got != filepath.Clean(workDir) && got != filepath.Clean(realWork) {
			t.Fatalf("%s unexpected output %q (want %q or %q)", cmd, got, workDir, realWork)
		}
	}

	secretRealDir := filepath.Join(outsideDir, "real_secret_target_dir")
	if err := os.MkdirAll(secretRealDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideSym := filepath.Join(outsideDir, "forged_pwd_link")
	if err := os.Symlink(secretRealDir, outsideSym); err == nil {
		symSlash := filepath.ToSlash(outsideSym)
		jail.resetDenials()
		out, _, code := runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`PWD=%q; pwd -P`, symSlash))
		if code != 0 || strings.Contains(out, "real_secret_target_dir") || strings.Contains(out, "outside") {
			t.Fatalf("pwd -P leaked outside symlink target: code=%d out=%q", code, out)
		}
		jail.resetDenials()
		out, _, code = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`PWD=%q; pwd`, symSlash))
		if code != 0 || strings.Contains(out, "real_secret_target_dir") || strings.Contains(out, "forged_pwd_link") || !jail.hasDeniedOp(OpStat) {
			t.Fatalf("pwd -L leaked forged outside symlink: code=%d out=%q denials=%+v", code, out, jail.snapshotDenials())
		}
	}
}

func TestJailCommandVTypeAndHash(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)
	outsideBin := writeDummyExecFile(t, outsideDir, "secret_bin")
	outsideBinSlash := filepath.ToSlash(outsideBin)

	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = writeDummyExecFile(t, binDir, "local_bin")

	outsideProbes := []string{
		`command -v secret_bin`,
		`command -V secret_bin`,
		fmt.Sprintf(`command -v %q`, outsideBinSlash),
		fmt.Sprintf(`command -V %q`, outsideBinSlash),
		`type secret_bin`,
		`type -t secret_bin`,
		`type -p secret_bin`,
		`type -P secret_bin`,
		fmt.Sprintf(`type -P %q`, outsideBinSlash),
		`hash secret_bin`,
	}
	for _, cmd := range outsideProbes {
		jail.resetDenials()
		out, _, code := runJailedShell(t, workDir, jail, []string{"PATH=" + outsideDir}, cmd)
		if code == 0 {
			t.Fatalf("%s unexpectedly exited 0 (out=%q)", cmd, out)
		}
		if out != "" {
			t.Fatalf("%s leaked path on stdout: %q", cmd, out)
		}
		if !jail.hasDeniedOp(OpStat) {
			t.Fatalf("%s did not record OpStat denial: %+v", cmd, jail.snapshotDenials())
		}
	}

	// Process-global globalHash must neither leak into nor be mutated by jailed sessions.
	hashRecord("host_cached_secret", outsideBin)
	defer func() {
		globalHash.mu.Lock()
		delete(globalHash.entries, "host_cached_secret")
		globalHash.mu.Unlock()
	}()

	out, _, code := runJailedShell(t, workDir, jail, []string{"PATH=" + binDir}, `hash`)
	if code != 0 || out != "hash: hash table empty\n" {
		t.Fatalf("jailed hash leaked globalHash: code=%d out=%q", code, out)
	}
	_, _, code = runJailedShell(t, workDir, jail, []string{"PATH=" + binDir}, `type host_cached_secret`)
	if code == 0 {
		t.Fatalf("jailed type leaked host_cached_secret from globalHash")
	}
	_, _, _ = runJailedShell(t, workDir, jail, []string{"PATH=" + binDir}, `hash -r; hash -d host_cached_secret`)
	globalHash.mu.Lock()
	hostEntry, hostOK := globalHash.entries["host_cached_secret"]
	globalHash.mu.Unlock()
	if !hostOK || hostEntry.path != outsideBin {
		t.Fatalf("jailed hash -r/-d mutated host globalHash: ok=%v entry=%+v", hostOK, hostEntry)
	}

	// Allowed inside-jail lookups work without polluting host globalHash.
	jail.resetDenials()
	out, stderr, code := runJailedShell(t, workDir, jail, []string{"PATH=" + binDir}, `
command -v local_bin >/dev/null
command -V grep
type -t grep
type -P local_bin >/dev/null
hash local_bin
`)
	if code != 0 {
		t.Fatalf("inside-jail command/type/hash failed (code=%d): stderr=%q", code, stderr)
	}
	if out != "grep is a unish builtin\nbuiltin\n" {
		t.Fatalf("unexpected inside-jail command/type output: %q", out)
	}
	globalHash.mu.Lock()
	_, polluted := globalHash.entries["local_bin"]
	if polluted {
		delete(globalHash.entries, "local_bin")
	}
	globalHash.mu.Unlock()
	if polluted {
		t.Fatalf("jailed hash local_bin polluted process-global globalHash")
	}
}

func TestJailSourcePathOracleAndPushdPopdStack(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)

	// 1. source / . with bare name and outside $PATH must check $PATH via fsStat (OpStat denied)
	// and fall back to <hc.Dir>/<name>, giving no existence oracle on outside files.
	jail.resetDenials()
	_, errExisting, codeExisting := runJailedShell(t, workDir, jail, []string{"PATH=" + outsideDir}, `source secret.txt`)
	if codeExisting == 0 || !jail.hasDeniedOp(OpStat) {
		t.Fatalf("PATH=outside source secret.txt expected non-zero + OpStat denial: code=%d denials=%+v", codeExisting, jail.snapshotDenials())
	}

	jail.resetDenials()
	_, errMissing, codeMissing := runJailedShell(t, workDir, jail, []string{"PATH=" + outsideDir}, `source definitely_absent_999.txt`)
	if codeMissing == 0 || !jail.hasDeniedOp(OpStat) {
		t.Fatalf("PATH=outside source missing expected non-zero + OpStat denial: code=%d denials=%+v", codeMissing, jail.snapshotDenials())
	}
	if strings.Contains(strings.ToLower(errExisting), "permission denied") || strings.Contains(strings.ToLower(errExisting), "outside") {
		t.Fatalf("PATH=outside source secret.txt leaked outside existence via error message: %q", errExisting)
	}
	if codeExisting != codeMissing {
		t.Fatalf("source exit code oracle: existing=%d vs missing=%d (err1=%q err2=%q)", codeExisting, codeMissing, errExisting, errMissing)
	}

	// Verify in-shell source semantics (caller vars, positional params, return) remain intact.
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "sourced_mod.sh"), []byte("MOD_OUT=\"$1:$2\"\nreturn 7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jail.resetDenials()
	out, stderr, code := runJailedShell(t, workDir, jail, []string{"PATH=" + binDir}, `
set -- orig1 orig2
source sourced_mod.sh alpha beta
rc=$?
echo "rc=$rc mod=$MOD_OUT orig=$1:$2"
`)
	if code != 0 || out != "rc=7 mod=alpha:beta orig=orig1:orig2\n" {
		t.Fatalf("in-shell source semantics broken: code=%d out=%q stderr=%q", code, out, stderr)
	}

	// 2. pushd -n ../.. inside sub/a/b followed by cd ../.. && popd must NOT escape workDir!
	realWork, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		realWork = filepath.Clean(workDir)
	}
	jail.resetDenials()
	out, stderr, code = runJailedShell(t, workDir, jail, nil, `
mkdir -p a/b && cd a/b && pushd -n ../.. >/dev/null && cd ../.. && popd >/dev/null && pwd -P
`)
	if code != 0 {
		t.Fatalf("pushd -n ../.. -> popd sequence failed (code=%d): stderr=%q", code, stderr)
	}
	gotPwd := filepath.Clean(strings.TrimSpace(out))
	if gotPwd != filepath.Clean(realWork) && gotPwd != filepath.Clean(workDir) {
		t.Fatalf("pushd -n ../.. -> popd escaped workspace! got pwd=%q, want %q", gotPwd, realWork)
	}
}

func TestJailUlimitUmaskAndHistoryIsolation(t *testing.T) {
	workDir, outsideDir, jail := setupJailFixture(t)
	outSlash := filepath.ToSlash(outsideDir)

	origUmask := shellUmask.Load()
	origNofile, origNofileErr := ulimitNofile()

	// 1. Read-only ulimit and umask succeed inside jail.
	jail.resetDenials()
	out, stderr, code := runJailedShell(t, workDir, jail, nil, `ulimit; ulimit -n >/dev/null; ulimit -a >/dev/null; umask; umask -S; umask -p`)
	if code != 0 {
		t.Fatalf("read-only ulimit/umask failed in jail (code=%d): out=%q stderr=%q", code, out, stderr)
	}

	// 2. Setting ulimit or umask is blocked (exit 1 + OpProc denial) and never mutates host state.
	jail.resetDenials()
	_, _, code = runJailedShell(t, workDir, jail, nil, `ulimit -n 8`)
	if code == 0 || !jail.hasDeniedOp(OpProc) {
		t.Fatalf("jailed ulimit -n 8 should fail with OpProc denial: code=%d denials=%+v", code, jail.snapshotDenials())
	}
	jail.resetDenials()
	_, _, code = runJailedShell(t, workDir, jail, nil, `umask 0777`)
	if code == 0 || !jail.hasDeniedOp(OpProc) {
		t.Fatalf("jailed umask 0777 should fail with OpProc denial: code=%d denials=%+v", code, jail.snapshotDenials())
	}

	// Even if a hook permits OpProc, jailed mode must NEVER mutate host RLIMIT_NOFILE or shellUmask.
	jailAllowProc := newTestWorkspaceJail(t, workDir)
	jailAllowProc.allowProc = true
	if _, _, code = runJailedShell(t, workDir, jailAllowProc, nil, `ulimit -n 8`); code == 0 {
		t.Fatalf("jailed ulimit -n 8 succeeded even though host process limits must be protected")
	}
	if _, _, code = runJailedShell(t, workDir, jailAllowProc, nil, `umask 0777`); code == 0 {
		t.Fatalf("jailed umask 0777 succeeded even though host process umask must be protected")
	}

	if gotUmask := shellUmask.Load(); gotUmask != origUmask {
		t.Fatalf("host shellUmask mutated: got %04o, want %04o", gotUmask, origUmask)
	}
	if origNofileErr == nil {
		if gotNofile, err := ulimitNofile(); err == nil && gotNofile != origNofile {
			t.Fatalf("host RLIMIT_NOFILE mutated: got %d, want %d", gotNofile, origNofile)
		}
	}

	// 3. Jailed history is isolated from replHist and enforces OpRead/OpWrite on file flags.
	jail.resetDenials()
	if _, _, code = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`history -r %q`, outSlash+"/secret.txt")); code == 0 || !jail.hasDeniedOp(OpRead) {
		t.Fatalf("history -r outside not blocked: code=%d denials=%+v", code, jail.snapshotDenials())
	}
	jail.resetDenials()
	if _, _, code = runJailedShell(t, workDir, jail, nil, fmt.Sprintf(`history -w %q`, outSlash+"/hist_w.txt")); code == 0 || !jail.hasDeniedOp(OpWrite) {
		t.Fatalf("history -w outside not blocked: code=%d denials=%+v", code, jail.snapshotDenials())
	}
	jail.resetDenials()
	if _, _, code = runJailedShell(t, workDir, jail, []string{"HISTFILE=" + filepath.Join(outsideDir, "hist_a.txt")}, `history -a`); code == 0 || !jail.hasDeniedOp(OpWrite) {
		t.Fatalf("history -a outside HISTFILE not blocked: code=%d denials=%+v", code, jail.snapshotDenials())
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "hist_w.txt")); !os.IsNotExist(err) {
		t.Fatalf("history -w created outside file")
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "hist_a.txt")); !os.IsNotExist(err) {
		t.Fatalf("history -a created outside file")
	}

	// Inside-jail history file operations succeed cleanly.
	jail.resetDenials()
	if _, stderr, code = runJailedShell(t, workDir, jail, nil, `history -w sub/hist.txt && history -a sub/hist.txt && history -r sub/hist.txt`); code != 0 {
		t.Fatalf("inside-jail history failed (code=%d): stderr=%q", code, stderr)
	}
}

func TestJailSedInplaceWriteAllowedDeleteDenied(t *testing.T) {
	workDir, _, _ := setupJailFixture(t)
	jailNoDelete := newTestWorkspaceJail(t, workDir)
	jailNoDelete.denyDelete = true

	target := filepath.Join(workDir, "inplace_nodelete.txt")
	if err := os.WriteFile(target, []byte("before_val\nsecond_line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runJailedShell(t, workDir, jailNoDelete, nil, `sed -i 's/before_val/after_val/' inplace_nodelete.txt`)
	if code != 0 {
		t.Fatalf("sed -i with OpWrite allowed and OpDelete denied failed (code=%d): stderr=%q", code, stderr)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after_val\nsecond_line\n" {
		t.Fatalf("sed -i direct write content mismatch: got %q", string(got))
	}
	if !jailNoDelete.hasDeniedOp(OpDelete) {
		t.Fatalf("expected OpDelete probe denial recorded, got %+v", jailNoDelete.snapshotDenials())
	}

	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sed-") {
			t.Fatalf("orphan sed temp file left behind when OpDelete denied: %s", e.Name())
		}
	}
}

func TestJailExplicitEnvNoHostSecretLeak(t *testing.T) {
	workDir, _, jail := setupJailFixture(t)
	t.Setenv("UNISH_HOST_SECRET_CANARY", "TOP_HOST_SECRET_777")

	script := `
env -u UNISH_HOST_SECRET_CANARY awk 'BEGIN { print "awk=" ENVIRON["UNISH_HOST_SECRET_CANARY"] }'
env -u UNISH_HOST_SECRET_CANARY jq -rn '"jq=" + ($ENV.UNISH_HOST_SECRET_CANARY // "")'
env -u UNISH_HOST_SECRET_CANARY printenv UNISH_HOST_SECRET_CANARY || echo "printenv=absent"
env -u UNISH_HOST_SECRET_CANARY awk 'BEGIN { system("printenv UNISH_HOST_SECRET_CANARY || echo sub_absent") }'
env -i KEEP_VAR=ok awk 'BEGIN { print "awk_i=" ENVIRON["KEEP_VAR"] ":" ENVIRON["UNISH_HOST_SECRET_CANARY"] }'
env -i KEEP_VAR=ok jq -rn '"jq_i=" + ($ENV.KEEP_VAR // "") + ":" + ($ENV.UNISH_HOST_SECRET_CANARY // "")'
`
	out, stderr, code := runJailedShell(t, workDir, jail, nil, script)
	if code != 0 {
		t.Fatalf("explicitEnviron test script failed (code=%d): out=%q stderr=%q", code, out, stderr)
	}
	if strings.Contains(out, "TOP_HOST_SECRET_777") {
		t.Fatalf("leaked unset/scrubbed host secret in output: %q", out)
	}
	want := "awk=\njq=\nprintenv=absent\nsub_absent\nawk_i=ok:\njq_i=ok:\n"
	if out != want {
		t.Fatalf("explicitEnviron output mismatch:\ngot:  %q\nwant: %q", out, want)
	}
}
