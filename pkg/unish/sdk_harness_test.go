package unish

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionPersistentStateAndEnvScrubbing(t *testing.T) {
	hostOrigDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	t.Setenv("TEST_HOST_API_KEY", "super-secret-key")
	t.Setenv("TEST_HOST_AUTH_TOKEN", "super-secret-token")
	t.Setenv("TEST_HOST_SAFE_VAR", "safe-value")

	ws := t.TempDir()
	subDir := filepath.Join(ws, "subdir")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var liveOut, liveErr bytes.Buffer
	sess, err := NewSession(
		WithWorkDir(ws),
		WithInheritEnv(true),
		WithScrubEnv("*_KEY", "*_SECRET", "*_TOKEN"),
		WithEnv([]string{"EXPLICIT_A=alpha"}),
		WithEnvMap(map[string]string{
			"EXPLICIT_TOKEN": "explicit-allowed-token",
		}),
		WithStdout(&liveOut),
		WithStderr(&liveErr),
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	// Verify host env secrets were scrubbed while safe & explicit vars survived.
	if v, ok := sess.GetVar("TEST_HOST_API_KEY"); ok || v != "" {
		t.Fatalf("expected TEST_HOST_API_KEY scrubbed, got %q (ok=%v)", v, ok)
	}
	if v, ok := sess.GetVar("TEST_HOST_AUTH_TOKEN"); ok || v != "" {
		t.Fatalf("expected TEST_HOST_AUTH_TOKEN scrubbed, got %q (ok=%v)", v, ok)
	}
	if v, ok := sess.GetVar("TEST_HOST_SAFE_VAR"); !ok || v != "safe-value" {
		t.Fatalf("expected TEST_HOST_SAFE_VAR=safe-value, got %q (ok=%v)", v, ok)
	}
	if v, ok := sess.GetVar("EXPLICIT_TOKEN"); !ok || v != "explicit-allowed-token" {
		t.Fatalf("expected EXPLICIT_TOKEN preserved, got %q (ok=%v)", v, ok)
	}

	// 1. Define shell variable, exported variable, function, alias, umask, and cd.
	res1, err := sess.Exec(context.Background(), `
		SHELL_ONLY="local_val"
		export EXPORTED_VAR="exported_val"
		greet() { printf "hello:%s\n" "$1"; }
		alias sayhi='greet world'
		umask 0077
		cd subdir
	`)
	if err != nil {
		t.Fatalf("Exec 1 error: %v", err)
	}
	if res1.ExitCode != 0 {
		t.Fatalf("Exec 1 exit=%d stderr=%q", res1.ExitCode, res1.Stderr)
	}

	realSub, _ := canonicalizeForCheck(subDir)
	if !sameCleanPath(sess.Cwd(), realSub) {
		t.Fatalf("expected Cwd=%q, got %q", realSub, sess.Cwd())
	}
	if sess.Umask() != 0o077 {
		t.Fatalf("expected session umask 0077, got %04o", sess.Umask())
	}
	// Verify global shellUmask and host os.Getwd() were NOT mutated.
	if shellUmask.Load() != 0o022 {
		t.Fatalf("expected global shellUmask untouched (0022), got %04o", shellUmask.Load())
	}
	afterHostDir, _ := os.Getwd()
	if !sameCleanPath(hostOrigDir, afterHostDir) {
		t.Fatalf("host working directory was mutated: before=%q after=%q", hostOrigDir, afterHostDir)
	}

	// 2. Verify state persists in subsequent Exec calls, including $? across SetVar/SetCwd.
	resFail, err := sess.Exec(context.Background(), `(exit 37)`)
	if err != nil {
		t.Fatalf("Exec exit 37 error: %v", err)
	}
	if resFail.ExitCode != 37 || sess.LastExitCode() != 37 {
		t.Fatalf("expected exit 37, got res=%d last=%d", resFail.ExitCode, sess.LastExitCode())
	}

	specialVal := "quote'here\nline2 $NOT_EXPANDED"
	if err := sess.SetVar("INJECTED_VAR", specialVal, true); err != nil {
		t.Fatalf("SetVar: %v", err)
	}
	if v, ok := sess.GetVar("INJECTED_VAR"); !ok || v != specialVal {
		t.Fatalf("GetVar(INJECTED_VAR) = %q (ok=%v), want %q", v, ok, specialVal)
	}
	if sess.LastExitCode() != 37 {
		t.Fatalf("SetVar clobbered LastExitCode: got %d, want 37", sess.LastExitCode())
	}

	res2, err := sess.Exec(context.Background(), `
		prev_rc=$?
		sayhi
		printf "rc=%d shell=%s exp=%s umask=%s\n" "$prev_rc" "$SHELL_ONLY" "$EXPORTED_VAR" "$(umask)"
		printf "secret=%s\n" "${TEST_HOST_API_KEY:-missing}"
	`)
	if err != nil {
		t.Fatalf("Exec 2 error: %v", err)
	}
	if res2.ExitCode != 0 {
		t.Fatalf("Exec 2 exit=%d stderr=%q", res2.ExitCode, res2.Stderr)
	}
	wantLines := []string{
		"hello:world",
		"rc=37 shell=local_val exp=exported_val umask=0077",
		"secret=missing",
	}
	for _, want := range wantLines {
		if !strings.Contains(res2.Stdout, want) {
			t.Fatalf("Exec 2 stdout %q missing %q", res2.Stdout, want)
		}
	}
	if !strings.Contains(liveOut.String(), "hello:world") {
		t.Fatalf("WithStdout stream did not capture output: %q", liveOut.String())
	}

	// 3. Verify SetCwd and UnsetVar work cleanly.
	if err := sess.SetCwd(ws); err != nil {
		t.Fatalf("SetCwd: %v", err)
	}
	realWS, _ := canonicalizeForCheck(ws)
	if !sameCleanPath(sess.Cwd(), realWS) {
		t.Fatalf("after SetCwd expected %q, got %q", realWS, sess.Cwd())
	}
	if err := sess.UnsetVar("EXPORTED_VAR"); err != nil {
		t.Fatalf("UnsetVar: %v", err)
	}
	if _, ok := sess.GetVar("EXPORTED_VAR"); ok {
		t.Fatalf("expected EXPORTED_VAR unset")
	}

	// 4. Verify one-shot unish.Exec helper.
	oneShot, err := Exec(context.Background(), `echo "oneshot:$ONE_SHOT_K"`, WithWorkDir(ws), WithEnvMap(map[string]string{"ONE_SHOT_K": "ok"}))
	if err != nil {
		t.Fatalf("one-shot Exec: %v", err)
	}
	if strings.TrimSpace(oneShot.Stdout) != "oneshot:ok" {
		t.Fatalf("one-shot stdout=%q, want oneshot:ok", oneShot.Stdout)
	}
}

func TestSessionConcurrentIsolation(t *testing.T) {
	hostDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	const workers = 8
	var wg sync.WaitGroup
	errCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		i := i
		dir := t.TempDir()
		sub := filepath.Join(dir, fmt.Sprintf("w-%d", i))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			sess, err := NewSession(
				WithWorkDir(dir),
				WithInheritEnv(false),
				WithEnvMap(map[string]string{
					"WORKER_ID": fmt.Sprintf("%d", i),
				}),
			)
			if err != nil {
				errCh <- fmt.Errorf("worker %d NewSession: %w", i, err)
				return
			}
			defer sess.Close()

			script := fmt.Sprintf(`
				umask 00%d7
				cd w-%d
				fn() { echo "id=$WORKER_ID"; }
				echo "$(fn)" > marker.txt
				cat marker.txt
			`, i%8, i)
			res, err := sess.Exec(context.Background(), script)
			if err != nil {
				errCh <- fmt.Errorf("worker %d Exec: %w", i, err)
				return
			}
			if res.ExitCode != 0 {
				errCh <- fmt.Errorf("worker %d exit=%d stderr=%s", i, res.ExitCode, res.Stderr)
				return
			}
			want := fmt.Sprintf("id=%d", i)
			if strings.TrimSpace(res.Stdout) != want {
				errCh <- fmt.Errorf("worker %d stdout=%q, want %q", i, res.Stdout, want)
				return
			}
			data, err := os.ReadFile(filepath.Join(sub, "marker.txt"))
			if err != nil || strings.TrimSpace(string(data)) != want {
				errCh <- fmt.Errorf("worker %d marker.txt=%q err=%v", i, string(data), err)
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}

	afterDir, _ := os.Getwd()
	if !sameCleanPath(hostDir, afterDir) {
		t.Fatalf("concurrent sessions mutated host cwd: %q -> %q", hostDir, afterDir)
	}
}

func TestSessionCancelCurrentPreservesState(t *testing.T) {
	ws := t.TempDir()
	sub := filepath.Join(ws, "keepdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	sess, err := NewSession(WithWorkDir(ws))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	if _, err := sess.Exec(context.Background(), `
		SURVIVOR="still-here"
		alive_fn() { echo "fn-ok"; }
		cd keepdir
	`); err != nil {
		t.Fatalf("setup Exec: %v", err)
	}

	started := make(chan struct{})
	var once sync.Once
	stream := writerFunc(func(p []byte) (int, error) {
		if bytes.Contains(p, []byte("READY")) {
			once.Do(func() { close(started) })
		}
		return len(p), nil
	})

	done := make(chan struct{})
	var cancelRes *ExecResult
	var cancelErr error
	go func() {
		defer close(done)
		cancelRes, cancelErr = sess.ExecWith(context.Background(), `
			echo READY
			sleep 30
			echo NEVER_REACHED
		`, ExecOptions{Stdout: stream})
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for in-flight command to start")
	}

	if !sess.CancelCurrent() {
		t.Fatal("expected CancelCurrent() to return true for running command")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cancelled ExecWith to return")
	}

	if !errors.Is(cancelErr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", cancelErr)
	}
	if cancelRes == nil || cancelRes.ExitCode != 130 {
		t.Fatalf("expected ExitCode=130, got %+v", cancelRes)
	}
	if strings.Contains(cancelRes.Stdout, "NEVER_REACHED") {
		t.Fatalf("post-sleep statement ran despite cancellation: %q", cancelRes.Stdout)
	}

	// Verify session state survived CancelCurrent intact!
	after, err := sess.Exec(context.Background(), `printf "%s|%s|%s" "$SURVIVOR" "$(alive_fn)" "$(basename "$PWD")"`)
	if err != nil {
		t.Fatalf("post-cancel Exec failed: %v", err)
	}
	if after.Stdout != "still-here|fn-ok|keepdir" {
		t.Fatalf("session state lost after CancelCurrent: got %q", after.Stdout)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestJailFilesystemPolicyGlobsAndSymlinks(t *testing.T) {
	ws := t.TempDir()
	roDir := t.TempDir()
	outsideDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(ws, "ok.txt"), []byte("allowed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("SECRET=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "nested", ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "nested", ".env.production"), []byte("PROD_SECRET=2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "nested", ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roDir, "readonly.txt"), []byte("ro-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideSecret := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outsideSecret, []byte("top-secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultJailConfig(ws)
	cfg.ReadOnlyRoots = []string{roDir}
	cfg.Rules = []PathRule{
		{Pattern: "**/.env*", Decision: DecisionDeny, Reason: "dotenv files are protected"},
		{Pattern: "**/.git/hooks/**", Decision: DecisionDeny, Reason: "git hooks are protected"},
	}
	jail, err := NewJail(cfg)
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}

	sess, err := NewSession(WithWorkDir(ws), WithJail(jail))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	// Allowed read & write inside workspace root, and read of .git/HEAD.
	r1, err := sess.Exec(context.Background(), `
		echo "written" > created.txt
		cat < ok.txt
		cat < nested/.git/HEAD
	`)
	if err != nil || r1.ExitCode != 0 {
		t.Fatalf("allowed workspace ops failed: err=%v exit=%d stderr=%q", err, r1.ExitCode, r1.Stderr)
	}
	if !strings.Contains(r1.Stdout, "allowed") || !strings.Contains(r1.Stdout, "refs/heads/main") {
		t.Fatalf("unexpected stdout: %q", r1.Stdout)
	}

	// ReadOnlyRoot allows read/cd, denies write!
	roSlash := filepath.ToSlash(roDir)
	rRO, err := sess.Exec(context.Background(), fmt.Sprintf(`cat < "%s/readonly.txt"`, roSlash))
	if err != nil || rRO.ExitCode != 0 || strings.TrimSpace(rRO.Stdout) != "ro-data" {
		t.Fatalf("ReadOnlyRoot read failed: err=%v exit=%d out=%q stderr=%q", err, rRO.ExitCode, rRO.Stdout, rRO.Stderr)
	}
	rROWrite, err := sess.Exec(context.Background(), fmt.Sprintf(`echo nope > "%s/tamper.txt"`, roSlash))
	if err != nil {
		t.Fatalf("Exec error: %v", err)
	}
	if rROWrite.ExitCode == 0 || len(rROWrite.Denials) == 0 {
		t.Fatalf("expected ReadOnlyRoot write denied, got exit=%d denials=%+v", rROWrite.ExitCode, rROWrite.Denials)
	}

	// Deny PathRules (**/.env* and **/.git/hooks/**) override writable Roots!
	for _, blocked := range []string{
		`cat < .env`,
		`cat < nested/.env.production`,
		`echo pwn > nested/.git/hooks/pre-commit`,
	} {
		res, err := sess.Exec(context.Background(), blocked)
		if err != nil {
			t.Fatalf("Exec(%q): %v", blocked, err)
		}
		if res.ExitCode == 0 || len(res.Denials) == 0 {
			t.Fatalf("expected %q denied by PathRule, got exit=%d denials=%+v", blocked, res.ExitCode, res.Denials)
		}
	}

	// Outside root: redirect read, redirect write, cd, test/[ and [[ ]], and glob!
	outSlash := filepath.ToSlash(outsideDir)
	outSecretSlash := filepath.ToSlash(outsideSecret)
	for _, blocked := range []string{
		fmt.Sprintf(`cat < "%s"`, outSecretSlash),
		fmt.Sprintf(`echo pwn > "%s/pwn.txt"`, outSlash),
		fmt.Sprintf(`cd "%s"`, outSlash),
		fmt.Sprintf(`[ -f "%s" ]`, outSecretSlash),
		fmt.Sprintf(`[[ -f "%s" ]]`, outSecretSlash),
	} {
		res, err := sess.Exec(context.Background(), blocked)
		if err != nil {
			t.Fatalf("Exec(%q): %v", blocked, err)
		}
		if res.ExitCode == 0 || len(res.Denials) == 0 {
			t.Fatalf("expected outside op %q denied, got exit=%d denials=%+v", blocked, res.ExitCode, res.Denials)
		}
	}

	// Glob outside jail must NOT leak outside.txt!
	rGlob, err := sess.Exec(context.Background(), fmt.Sprintf(`echo "%s"/*`, outSlash))
	if err != nil {
		t.Fatalf("glob Exec: %v", err)
	}
	if strings.Contains(rGlob.Stdout, "outside.txt") {
		t.Fatalf("glob leaked outside filename: %q", rGlob.Stdout)
	}

	// Symlink escape defenses (leaf symlink, ancestor dir symlink, dangling symlink, and intra-ws denied alias).
	escapeLink := filepath.Join(ws, "escape_leaf")
	if err := os.Symlink(outsideSecret, escapeLink); err == nil {
		escapeDirLink := filepath.Join(ws, "escape_dir")
		_ = os.Symlink(outsideDir, escapeDirLink)
		danglingLink := filepath.Join(ws, "escape_dangling")
		_ = os.Symlink(filepath.Join(outsideDir, "dangling_new.txt"), danglingLink)
		dotenvAlias := filepath.Join(ws, "harmless_name.txt")
		_ = os.Symlink(filepath.Join(ws, ".env"), dotenvAlias)

		for _, symScript := range []string{
			`cat < escape_leaf`,
			`echo pwn > escape_dir/created_outside.txt`,
			`echo pwn > escape_dangling`,
			`cat < harmless_name.txt`,
		} {
			res, err := sess.Exec(context.Background(), symScript)
			if err != nil {
				t.Fatalf("symlink Exec(%q): %v", symScript, err)
			}
			if res.ExitCode == 0 || len(res.Denials) == 0 {
				t.Fatalf("symlink bypass %q was NOT blocked! exit=%d denials=%+v", symScript, res.ExitCode, res.Denials)
			}
		}
	}
}

func TestJailApproverSessionGrantsAndAudit(t *testing.T) {
	ws := t.TempDir()
	outsideDir := t.TempDir()
	secretFile := filepath.Join(outsideDir, "data.txt")
	if err := os.WriteFile(secretFile, []byte("approved-payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	var prompts atomic.Int32
	var nextVerdict atomic.Int32
	nextVerdict.Store(int32(DecisionDeny))

	var auditMu sync.Mutex
	var audits []Decision

	cfg := DefaultJailConfig(ws)
	cfg.OutsideAction = DecisionAsk
	cfg.Approver = func(ctx context.Context, req AccessRequest) (Decision, error) {
		prompts.Add(1)
		return Decision(nextVerdict.Load()), nil
	}
	cfg.OnAudit = func(req AccessRequest, dec Decision) {
		auditMu.Lock()
		audits = append(audits, dec)
		auditMu.Unlock()
	}

	jail, err := NewJail(cfg)
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	sess, err := NewSession(WithWorkDir(ws), WithJail(jail))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	cmd := fmt.Sprintf(`cat < "%s"`, filepath.ToSlash(secretFile))

	// 1. Deny via Approver.
	r1, err := sess.Exec(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if r1.ExitCode == 0 || prompts.Load() != 1 || len(r1.Denials) != 1 {
		t.Fatalf("expected 1 prompt + denial, got exit=%d prompts=%d denials=%+v", r1.ExitCode, prompts.Load(), r1.Denials)
	}
	if drained := jail.DrainDenials(); len(drained) != 1 {
		t.Fatalf("expected 1 drained denial on Jail, got %d", len(drained))
	}

	// 2. DecisionAllow (one-time) prompts on EVERY call.
	nextVerdict.Store(int32(DecisionAllow))
	r2, _ := sess.Exec(context.Background(), cmd)
	r3, _ := sess.Exec(context.Background(), cmd)
	if r2.ExitCode != 0 || r3.ExitCode != 0 || strings.TrimSpace(r2.Stdout) != "approved-payload" {
		t.Fatalf("DecisionAllow failed: r2=%+v r3=%+v", r2, r3)
	}
	if prompts.Load() != 3 {
		t.Fatalf("expected DecisionAllow to prompt twice (total 3), got %d", prompts.Load())
	}

	// 3. DecisionAllowSession caches grant so repeated accesses do NOT re-prompt!
	nextVerdict.Store(int32(DecisionAllowSession))
	r4, _ := sess.Exec(context.Background(), cmd)
	r5, _ := sess.Exec(context.Background(), cmd)
	r6, _ := sess.Exec(context.Background(), cmd)
	if r4.ExitCode != 0 || r5.ExitCode != 0 || r6.ExitCode != 0 {
		t.Fatalf("DecisionAllowSession failed")
	}
	if prompts.Load() != 4 {
		t.Fatalf("expected DecisionAllowSession to prompt ONLY once (total 4), got %d", prompts.Load())
	}

	// 4. RevokeSessionGrants clears cache so next access prompts again!
	jail.RevokeSessionGrants()
	nextVerdict.Store(int32(DecisionDeny))
	r7, _ := sess.Exec(context.Background(), cmd)
	if r7.ExitCode == 0 || prompts.Load() != 5 {
		t.Fatalf("after RevokeSessionGrants expected prompt #5 + deny, got exit=%d prompts=%d", r7.ExitCode, prompts.Load())
	}

	auditMu.Lock()
	auditCount := len(audits)
	auditMu.Unlock()
	if auditCount < 5 {
		t.Fatalf("expected OnAudit events recorded, got %d", auditCount)
	}
}

func TestJailNetPolicyAndDNSRebindingDefense(t *testing.T) {
	ws := t.TempDir()

	var lookupCalls atomic.Int32
	var dialedAddrs []string
	var dialMu sync.Mutex

	var mockIPs []net.IPAddr
	var flipSecondLookup bool

	cfg := DefaultJailConfig(ws)
	cfg.Net = NetPolicy{
		Default:         DecisionDeny,
		AllowLoopback:   false,
		BlockPrivateLAN: true,
		AllowListen:     DecisionDeny,
		Rules: []NetRule{
			{
				Proto:       "tcp",
				HostPattern: "*.trusted.example",
				Ports:       []int{443},
				Decision:    DecisionAllow,
			},
			{
				Proto:     "tcp",
				CIDR:      "203.0.113.0/24",
				PortRange: [2]int{8000, 8010},
				Decision:  DecisionAllow,
			},
		},
		LookupIPAddr: func(ctx context.Context, host string) ([]net.IPAddr, error) {
			n := lookupCalls.Add(1)
			if flipSecondLookup && n > 1 {
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			}
			return append([]net.IPAddr(nil), mockIPs...), nil
		},
		TransportDialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialMu.Lock()
			dialedAddrs = append(dialedAddrs, addr)
			dialMu.Unlock()
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		},
	}

	jail, err := NewJail(cfg)
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}

	// 1. Unlisted host fails closed WITHOUT even calling DNS LookupIPAddr? Wait:
	// NetRules exist, so LookupIPAddr runs, then CheckAccess denies unlisted host!
	mockIPs = []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}
	if _, err := jail.DialContext(context.Background(), "curl", "tcp", "evil.org:443"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected evil.org:443 denied, got %v", err)
	}
	if _, err := jail.DialContext(context.Background(), "curl", "tcp", "api.trusted.example:80"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected wrong port 80 denied, got %v", err)
	}

	// 2. DNS-Rebinding attacks on allowed wildcard domain (*.trusted.example:443):
	// Every private/metadata/loopback/IPv6-mapped target MUST be blocked before TransportDialer!
	rebindTargets := []string{
		"127.0.0.1",
		"10.0.0.42",
		"172.16.5.9",
		"192.168.1.1",
		"169.254.169.254",
		"::ffff:169.254.169.254",
		"fd00::1234",
		"fe80::1",
		"::1",
	}
	for _, badIP := range rebindTargets {
		mockIPs = []net.IPAddr{{IP: net.ParseIP(badIP)}}
		_, err := jail.DialContext(context.Background(), "curl", "tcp", "rebind.trusted.example:443")
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("DNS rebinding to %s was NOT blocked! err=%v", badIP, err)
		}
	}

	// Round-robin DNS rebinding (1 public IP + 1 metadata IP) must ALSO abort immediately!
	mockIPs = []net.IPAddr{
		{IP: net.ParseIP("93.184.216.34")},
		{IP: net.ParseIP("169.254.169.254")},
	}
	if _, err := jail.DialContext(context.Background(), "curl", "tcp", "rr.trusted.example:443"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("round-robin DNS rebinding was NOT blocked: %v", err)
	}
	dialMu.Lock()
	if len(dialedAddrs) != 0 {
		t.Fatalf("TransportDialer should NEVER have been called on blocked targets, got %v", dialedAddrs)
	}
	dialMu.Unlock()

	// 3. Valid public IP resolution: LookupIPAddr called ONCE, TransportDialer receives
	// pinned IP:port ("93.184.216.34:443") directly so DNS cannot flip on connect!
	lookupCalls.Store(0)
	flipSecondLookup = true
	mockIPs = []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}
	conn, err := jail.DialContext(context.Background(), "curl", "tcp", "api.trusted.example:443")
	if err != nil {
		t.Fatalf("expected valid public IP dial allowed, got %v", err)
	}
	_ = conn.Close()
	if lookupCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 DNS lookup (no second TOCTOU lookup), got %d", lookupCalls.Load())
	}
	dialMu.Lock()
	if len(dialedAddrs) != 1 || dialedAddrs[0] != "93.184.216.34:443" {
		t.Fatalf("expected TransportDialer to dial pinned IP 93.184.216.34:443, got %v", dialedAddrs)
	}
	dialMu.Unlock()

	// 4. CIDR + PortRange rule check.
	conn2, err := jail.DialContext(context.Background(), "nc", "tcp", "203.0.113.9:8005")
	if err != nil {
		t.Fatalf("expected CIDR+PortRange dial allowed, got %v", err)
	}
	_ = conn2.Close()
}

func TestJailExecPolicyAndSymlinkSpoofing(t *testing.T) {
	ws := t.TempDir()
	selfExe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	selfSlash := filepath.ToSlash(selfExe)

	// 1. Builtins-only jail (ExecDefault: DecisionDeny) blocks external binaries with exit 126.
	jailBuiltins, err := NewJail(DefaultJailConfig(ws))
	if err != nil {
		t.Fatal(err)
	}
	sess1, err := NewSession(WithWorkDir(ws), WithJail(jailBuiltins))
	if err != nil {
		t.Fatal(err)
	}
	defer sess1.Close()

	rBuiltin, err := sess1.Exec(context.Background(), `echo "pure-builtin" | tr 'a-z' 'A-Z'`)
	if err != nil || rBuiltin.ExitCode != 0 || strings.TrimSpace(rBuiltin.Stdout) != "PURE-BUILTIN" {
		t.Fatalf("builtin pipeline failed in builtins jail: err=%v res=%+v", err, rBuiltin)
	}

	rBlocked, err := sess1.Exec(context.Background(), fmt.Sprintf(`"%s" --help`, selfSlash))
	if err != nil {
		t.Fatal(err)
	}
	if rBlocked.ExitCode != 126 || len(rBlocked.Denials) == 0 || rBlocked.Denials[0].Op != OpExec {
		t.Fatalf("expected external binary exit=126 + OpExec denial, got exit=%d denials=%+v", rBlocked.ExitCode, rBlocked.Denials)
	}

	// 2. AllowBinaries allows listed binary, blocks unlisted binary AND workspace symlink spoofing!
	cfgAllow := DefaultJailConfig(ws)
	cfgAllow.OutsideAction = DecisionAllow
	cfgAllow.Exec = ExecPolicy{
		ExecDefault:   DecisionDeny,
		AllowBinaries: []string{"allowed_phantom_tool_xyz"},
	}
	jailSpoof, err := NewJail(cfgAllow)
	if err != nil {
		t.Fatal(err)
	}
	sessSpoof, err := NewSession(WithWorkDir(ws), WithJail(jailSpoof))
	if err != nil {
		t.Fatal(err)
	}
	defer sessSpoof.Close()

	spoofName := "allowed_phantom_tool_xyz"
	if runtime.GOOS == "windows" {
		spoofName += ".exe"
	}
	spoofPath := filepath.Join(ws, spoofName)
	if err := os.Symlink(selfExe, spoofPath); err == nil {
		rSpoof, err := sessSpoof.Exec(context.Background(), fmt.Sprintf(`"./%s" -test.run=^$`, spoofName))
		if err != nil {
			t.Fatal(err)
		}
		if rSpoof.ExitCode != 126 {
			t.Fatalf("workspace symlink spoofing AllowBinaries basename was NOT blocked! exit=%d", rSpoof.ExitCode)
		}
	}

	// Explicitly allowing selfExe by full path succeeds!
	cfgRealAllow := DefaultJailConfig(ws)
	cfgRealAllow.Exec = ExecPolicy{
		ExecDefault:   DecisionDeny,
		AllowBinaries: []string{selfExe},
	}
	jailRealAllow, err := NewJail(cfgRealAllow)
	if err != nil {
		t.Fatal(err)
	}
	sessAllow, err := NewSession(WithWorkDir(ws), WithJail(jailRealAllow))
	if err != nil {
		t.Fatal(err)
	}
	defer sessAllow.Close()

	rOK, err := sessAllow.Exec(context.Background(), fmt.Sprintf(`"%s" -test.run=^TestNonExistentNothing$`, selfSlash))
	if err != nil {
		t.Fatal(err)
	}
	if rOK.ExitCode != 0 || len(rOK.Denials) != 0 {
		t.Fatalf("expected explicitly allowed binary to run cleanly, got exit=%d stderr=%q denials=%+v", rOK.ExitCode, rOK.Stderr, rOK.Denials)
	}
}

func TestCLIFlagsJailAndScriptExecution(t *testing.T) {
	ws := t.TempDir()
	ro := t.TempDir()
	outside := t.TempDir()

	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("NOPE=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "info.txt"), []byte("ro-ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("hidden\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(ws, "job.sh")
	if err := os.WriteFile(scriptPath, []byte("echo \"arg=$1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Invalid flag value exits 2.
	if rc := RunCLI("test", []string{"unish", "--outside", "bogus", "-c", "true"}); rc != 2 {
		t.Fatalf("expected invalid --outside to return 2, got %d", rc)
	}

	// Script execution with --jail (without -c!) passes positional args and respects jail.
	rc := RunCLI("test", []string{
		"unish",
		"--jail", ws,
		"--allow-read", ro,
		"--deny-path", "**/.env*",
		"--net", "loopback",
		"--allow-net", "example.com:443",
		"--exec-mode", "builtins",
		scriptPath,
		"hello",
	})
	if rc != 0 {
		t.Fatalf("expected jailed script.sh to succeed with rc=0, got %d", rc)
	}

	// Reading denied .env via CLI -c returns non-zero.
	rcEnv := RunCLI("test", []string{
		"unish",
		"--jail", ws,
		"--deny-path", "**/.env*",
		"-c", fmt.Sprintf(`cat < "%s"`, filepath.ToSlash(filepath.Join(ws, ".env"))),
	})
	if rcEnv == 0 {
		t.Fatalf("expected --deny-path **/.env* to block redirect read in RunCLI")
	}

	// Executing a script located OUTSIDE --jail returns 126!
	outsideScript := filepath.Join(outside, "evil.sh")
	_ = os.WriteFile(outsideScript, []byte("echo pwned\n"), 0o644)
	if rcOut := RunCLI("test", []string{"unish", "--jail", ws, outsideScript}); rcOut != 126 {
		t.Fatalf("expected outside script blocked with rc=126, got %d", rcOut)
	}
}

func TestHarnessJSONLRPCStreamingPermissionsAndCancel(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	otherOutsideFile := filepath.Join(outside, "other.txt")
	if err := os.WriteFile(outsideFile, []byte("vault-42"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherOutsideFile, []byte("vault-99"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultJailConfig(ws)
	cfg.OutsideAction = DecisionAsk
	jail, err := NewJail(cfg)
	if err != nil {
		t.Fatal(err)
	}

	clientInR, clientInW := io.Pipe()
	serverOutR, serverOutW := io.Pipe()

	srv, err := NewHarnessServer(clientInR, serverOutW, WithWorkDir(ws), WithJail(jail))
	if err != nil {
		t.Fatal(err)
	}

	srvDone := make(chan error, 1)
	go func() {
		err := srv.Serve(context.Background())
		_ = serverOutW.Close()
		srvDone <- err
	}()
	defer func() {
		_ = clientInW.Close()
		<-srvDone
		_ = srv.Session().Close()
	}()

	send := func(msg map[string]any) {
		t.Helper()
		b, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b = append(b, '\n')
		if _, err := clientInW.Write(b); err != nil {
			t.Fatalf("write to harness stdin: %v", err)
		}
	}

	eventsCh := make(chan HarnessEvent, 64)
	go func() {
		sc := bufio.NewScanner(serverOutR)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var ev HarnessEvent
			if err := json.Unmarshal(sc.Bytes(), &ev); err == nil {
				eventsCh <- ev
			}
		}
		close(eventsCh)
	}()

	nextEvent := func(timeout time.Duration) HarnessEvent {
		t.Helper()
		select {
		case ev, ok := <-eventsCh:
			if !ok {
				t.Fatal("harness event stream closed unexpectedly")
			}
			return ev
		case <-time.After(timeout):
			t.Fatalf("timed out after %v waiting for harness event", timeout)
			return HarnessEvent{}
		}
	}

	// 1. Ping -> Pong
	send(map[string]any{"id": "p1", "type": "ping"})
	if ev := nextEvent(3 * time.Second); ev.Type != "pong" || ev.ID != "p1" {
		t.Fatalf("expected pong p1, got %+v", ev)
	}

	// 2. Exec with live stdout/stderr streaming & state persistence + get_state
	if err := os.MkdirAll(filepath.Join(ws, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	send(map[string]any{
		"id":     "e1",
		"type":   "exec",
		"script": `export HARNESS_KEY="persisted"; cd inner; echo "out-line"; echo "err-line" >&2`,
	})
	var sawStdoutChunk, sawStderrChunk bool
	for {
		ev := nextEvent(5 * time.Second)
		switch ev.Type {
		case "stdout":
			if strings.Contains(ev.Data, "out-line") {
				sawStdoutChunk = true
			}
		case "stderr":
			if strings.Contains(ev.Data, "err-line") {
				sawStderrChunk = true
			}
		case "result":
			if ev.ID != "e1" || ev.ExitCode == nil || *ev.ExitCode != 0 {
				t.Fatalf("unexpected e1 result: %+v", ev)
			}
			if !sawStdoutChunk || !sawStderrChunk {
				t.Fatalf("expected live stdout and stderr chunks before result (out=%v err=%v)", sawStdoutChunk, sawStderrChunk)
			}
			goto doneE1
		default:
			t.Fatalf("unexpected event during e1: %+v", ev)
		}
	}
doneE1:

	send(map[string]any{"id": "s1", "type": "get_state"})
	if st := nextEvent(3 * time.Second); st.Type != "state" || !strings.HasSuffix(filepath.ToSlash(st.Cwd), "/inner") {
		t.Fatalf("expected state in /inner, got %+v", st)
	}

	// 3. Mid-command ask_permission -> ping while paused -> permission_reply(allow_session)!
	outFileSlash := filepath.ToSlash(outsideFile)
	send(map[string]any{
		"id":     "e2",
		"type":   "exec",
		"script": fmt.Sprintf(`cat < "%s"`, outFileSlash),
	})
	permEv := nextEvent(5 * time.Second)
	if permEv.Type != "ask_permission" || permEv.ID != "e2" || permEv.ReqID == "" || permEv.Op != OpRead {
		t.Fatalf("expected ask_permission for e2, got %+v", permEv)
	}

	// Prove stdin loop is completely non-blocking while worker is parked in promptPermission!
	send(map[string]any{"id": "p-mid", "type": "ping"})
	if pongMid := nextEvent(3 * time.Second); pongMid.Type != "pong" || pongMid.ID != "p-mid" {
		t.Fatalf("expected mid-permission pong, got %+v", pongMid)
	}

	send(map[string]any{
		"type":     "permission_reply",
		"req_id":   permEv.ReqID,
		"decision": "allow_session",
	})
	for {
		ev := nextEvent(5 * time.Second)
		if ev.Type == "stdout" {
			continue
		}
		if ev.Type == "result" {
			if ev.ExitCode == nil || *ev.ExitCode != 0 || ev.Stdout == nil || *ev.Stdout != "vault-42" {
				t.Fatalf("expected approved read vault-42, got %+v", ev)
			}
			break
		}
		t.Fatalf("unexpected event after permission_reply: %+v", ev)
	}

	// 4. Repeat same outside read: must succeed WITHOUT emitting ask_permission (allow_session cached)!
	send(map[string]any{
		"id":     "e3",
		"type":   "exec",
		"script": fmt.Sprintf(`cat < "%s"`, outFileSlash),
	})
	for {
		ev := nextEvent(5 * time.Second)
		if ev.Type == "ask_permission" {
			t.Fatalf("unexpected re-prompt after allow_session: %+v", ev)
		}
		if ev.Type == "stdout" {
			continue
		}
		if ev.Type == "result" {
			if ev.ExitCode == nil || *ev.ExitCode != 0 || ev.Stdout == nil || *ev.Stdout != "vault-42" {
				t.Fatalf("expected cached read vault-42, got %+v", ev)
			}
			break
		}
	}

	// 5. Mid-command cancel while command is running (`sleep 30`), AND mid-command cancel while parked in ask_permission!
	send(map[string]any{
		"id":     "e-slow",
		"type":   "exec",
		"script": `echo "SPINNING"; sleep 30; echo "UNREACHABLE"`,
	})
	for {
		ev := nextEvent(5 * time.Second)
		if ev.Type == "stdout" && strings.Contains(ev.Data, "SPINNING") {
			break
		}
	}
	send(map[string]any{"type": "cancel", "req_id": "e-slow"})
	for {
		ev := nextEvent(5 * time.Second)
		if ev.Type == "stdout" || ev.Type == "stderr" {
			continue
		}
		if ev.Type == "result" {
			if !ev.Cancelled || ev.ExitCode == nil || *ev.ExitCode != 130 {
				t.Fatalf("expected cancelled result (130), got %+v", ev)
			}
			break
		}
	}

	// Cancel while blocked inside ask_permission!
	send(map[string]any{
		"id":     "e-perm-cancel",
		"type":   "exec",
		"script": fmt.Sprintf(`cat < "%s"`, filepath.ToSlash(otherOutsideFile)),
	})
	permEv2 := nextEvent(5 * time.Second)
	if permEv2.Type != "ask_permission" {
		t.Fatalf("expected ask_permission for otherOutsideFile, got %+v", permEv2)
	}
	send(map[string]any{"type": "cancel", "req_id": "e-perm-cancel"})
	for {
		ev := nextEvent(5 * time.Second)
		if ev.Type == "stderr" {
			continue
		}
		if ev.Type == "result" {
			if !ev.Cancelled || ev.ExitCode == nil || *ev.ExitCode != 130 {
				t.Fatalf("expected cancelled result from ask_permission cancel, got %+v", ev)
			}
			break
		}
	}

	// 6. Verify session env & cwd survived both cancellations!
	send(map[string]any{
		"id":     "e-after",
		"type":   "exec",
		"script": `printf "%s:%s" "$HARNESS_KEY" "$(basename "$PWD")"`,
	})
	for {
		ev := nextEvent(5 * time.Second)
		if ev.Type == "stdout" {
			continue
		}
		if ev.Type == "result" {
			if ev.Stdout == nil || *ev.Stdout != "persisted:inner" {
				t.Fatalf("session state corrupted after cancel: %+v", ev)
			}
			break
		}
	}
}

func TestCompletionRespectsSessionDirAndJail(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()

	if err := os.WriteFile(filepath.Join(ws, "unique_ws_target.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret_outside_comp.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	jail, err := NewJail(DefaultJailConfig(ws))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewSession(WithWorkDir(ws), WithJail(jail))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Completes file inside sess.runner.Dir without os.Chdir!
	line := "cat unique_ws_"
	rep, cands := completeWord(line, len([]rune(line)), sess.runner, nil)
	if rep != "unique_ws_target.txt" || len(cands) != 1 {
		t.Fatalf("expected completion unique_ws_target.txt in session dir, got rep=%q cands=%v", rep, cands)
	}

	// Does NOT leak completions from outside the jail!
	outLine := "cat " + filepath.ToSlash(outside) + "/secret_"
	repOut, candsOut := completeWord(outLine, len([]rune(outLine)), sess.runner, nil)
	if repOut != "" || len(candsOut) != 0 {
		t.Fatalf("completion leaked outside-jail file! rep=%q cands=%v", repOut, candsOut)
	}
}

func TestJailProcLoopbackTildeAndCustomHook(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	sshDir := filepath.Join(fakeHome, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	idRSA := filepath.Join(sshDir, "id_rsa")
	if err := os.WriteFile(idRSA, []byte("PRIVATE-KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultJailConfig(fakeHome)
	cfg.Rules = []PathRule{
		{Pattern: "~/.ssh/**", Decision: DecisionDeny, Reason: "ssh keys denied"},
	}
	cfg.Net.AllowLoopback = true
	jail, err := NewJail(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Tilde PathRule (~/.ssh/**) blocks reading ~/.ssh/id_rsa even when HOME is in Roots!
	sess, err := NewSession(WithWorkDir(fakeHome), WithJail(jail))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	rSSH, err := sess.Exec(context.Background(), fmt.Sprintf(`cat < "%s"`, filepath.ToSlash(idRSA)))
	if err != nil {
		t.Fatal(err)
	}
	if rSSH.ExitCode == 0 || len(rSSH.Denials) == 0 {
		t.Fatalf("expected ~/.ssh/** rule to block id_rsa, got exit=%d denials=%+v", rSSH.ExitCode, rSSH.Denials)
	}

	// 2. ProcPolicy (ProcDefault: DecisionDeny) blocks signaling arbitrary host PIDs!
	rKill, err := sess.Exec(context.Background(), `kill -TERM 999999`)
	if err != nil {
		t.Fatal(err)
	}
	if rKill.ExitCode == 0 || len(rKill.Denials) == 0 || rKill.Denials[0].Op != OpProc {
		t.Fatalf("expected kill host PID blocked with OpProc denial, got exit=%d denials=%+v", rKill.ExitCode, rKill.Denials)
	}

	// 3. AllowLoopback allows 127.0.0.1 Listen + DialContext, while denying 0.0.0.0 wildcard Listen!
	if _, err := jail.Listen(context.Background(), "nc", "tcp", "0.0.0.0:0"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected 0.0.0.0:0 listen denied in loopback-only mode, got %v", err)
	}
	ln, err := jail.Listen(context.Background(), "nc", "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("expected 127.0.0.1:0 listen allowed in loopback mode, got %v", err)
	}
	defer ln.Close()
	go func() {
		if c, aerr := ln.Accept(); aerr == nil {
			_ = c.Close()
		}
	}()
	c, err := jail.DialContext(context.Background(), "nc", "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("expected loopback dial to %s allowed, got %v", ln.Addr().String(), err)
	}
	_ = c.Close()

	// 4. GrantSession / HasSessionGrant programmatic API.
	outsideTarget := filepath.Join(t.TempDir(), "grant.txt")
	jail.GrantSession(OpRead, outsideTarget)
	if !jail.HasSessionGrant(OpRead, outsideTarget) {
		t.Fatal("expected HasSessionGrant true after GrantSession")
	}
	jail.RevokeSessionGrants()
	if jail.HasSessionGrant(OpRead, outsideTarget) {
		t.Fatal("expected HasSessionGrant false after RevokeSessionGrants")
	}

	// 5. Custom CapabilityHook via WithHook populates ExecResult.Denials.
	wsCustom := t.TempDir()
	customHook := newTestJailHook(t, wsCustom)
	sessCustom, err := NewSession(WithWorkDir(wsCustom), WithHook(customHook))
	if err != nil {
		t.Fatal(err)
	}
	defer sessCustom.Close()

	rCustom, err := sessCustom.Exec(context.Background(), fmt.Sprintf(`cat < "%s"`, filepath.ToSlash(idRSA)))
	if err != nil {
		t.Fatal(err)
	}
	if rCustom.ExitCode == 0 || len(rCustom.Denials) == 0 {
		t.Fatalf("expected custom CapabilityHook denial recorded in ExecResult.Denials, got %+v", rCustom)
	}
}

func copyExecutableForTest(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.Link(src, dst); err == nil {
		return
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read executable %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatalf("write executable %s: %v", dst, err)
	}
}

func TestRedTeam1_WritableRootBinarySpoofing(t *testing.T) {
	selfExe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}

	ws := t.TempDir()
	roBinDir := t.TempDir()
	extraWritable := t.TempDir()

	binName := "git"
	if runtime.GOOS == "windows" {
		binName = "git.exe"
	}

	wsFakeGit := filepath.Join(ws, binName)
	roRealGit := filepath.Join(roBinDir, binName)
	ruleFakeGit := filepath.Join(extraWritable, binName)
	copyExecutableForTest(t, selfExe, wsFakeGit)
	copyExecutableForTest(t, selfExe, roRealGit)
	copyExecutableForTest(t, selfExe, ruleFakeGit)

	// Also drop a POSIX executable script in ws/bin/git to verify non-.exe script spoofing.
	wsScriptDir := filepath.Join(ws, "bin")
	if err := os.MkdirAll(wsScriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wsScriptGit := filepath.Join(wsScriptDir, "git")
	if err := os.WriteFile(wsScriptGit, []byte("#!/bin/sh\necho PWNED\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultJailConfig(ws)
	cfg.ReadOnlyRoots = []string{roBinDir}
	cfg.Rules = []PathRule{
		{
			Pattern:  filepath.ToSlash(extraWritable) + "/**",
			Ops:      []AccessOp{OpRead, OpWrite, OpStat, OpList},
			Decision: DecisionAllow,
		},
	}
	cfg.Exec = ExecPolicy{
		ExecDefault:   DecisionDeny,
		AllowBinaries: []string{"git"},
	}
	jail, err := NewJail(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Direct CheckAccess on writable-root binary or script MUST deny bare-basename AllowBinaries!
	for _, spoof := range []string{wsFakeGit, wsScriptGit, ruleFakeGit} {
		err := jail.CheckAccess(context.Background(), AccessRequest{
			Op:      OpExec,
			Command: "git",
			Path:    spoof,
			RawPath: spoof,
		})
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("expected writable-root binary %s denied despite AllowBinaries:[\"git\"], got %v", spoof, err)
		}
	}

	// 2. Even a bare-basename GrantSession(OpExec, "git") MUST NOT allow executing binaries inside writable roots!
	jail.GrantSession(OpExec, "git")
	if err := jail.CheckAccess(context.Background(), AccessRequest{
		Op:      OpExec,
		Command: "git",
		Path:    wsFakeGit,
		RawPath: wsFakeGit,
	}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("GrantSession(OpExec, \"git\") allowed writable-root binary spoofing: %v", err)
	}

	// 3. End-to-end Session.Exec: ./git and PATH-prepended ws git fail with 126, while read-only system git succeeds!
	sess, err := NewSession(WithWorkDir(ws), WithJail(jail))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	rSpoofRel, err := sess.Exec(context.Background(), fmt.Sprintf(`"./%s" -test.run=^TestNothing$`, binName))
	if err != nil {
		t.Fatal(err)
	}
	if rSpoofRel.ExitCode != 126 || len(rSpoofRel.Denials) == 0 {
		t.Fatalf("expected ./%s inside writable root blocked (126), got exit=%d denials=%+v", binName, rSpoofRel.ExitCode, rSpoofRel.Denials)
	}

	rSpoofPath, err := sess.Exec(context.Background(), fmt.Sprintf(`PATH=%q git -test.run=^TestNothing$`, ws+string(os.PathListSeparator)+roBinDir))
	if err != nil {
		t.Fatal(err)
	}
	if rSpoofPath.ExitCode != 126 {
		t.Fatalf("expected PATH-hijacked git in writable root blocked (126), got exit=%d out=%q err=%q", rSpoofPath.ExitCode, rSpoofPath.Stdout, rSpoofPath.Stderr)
	}

	rTrusted, err := sess.Exec(context.Background(), fmt.Sprintf(`"%s" -test.run=^TestNonExistentNothing$`, filepath.ToSlash(roRealGit)))
	if err != nil {
		t.Fatal(err)
	}
	if rTrusted.ExitCode != 0 || len(rTrusted.Denials) != 0 {
		t.Fatalf("expected read-only root binary allowed by AllowBinaries:[\"git\"], got exit=%d stderr=%q denials=%+v", rTrusted.ExitCode, rTrusted.Stderr, rTrusted.Denials)
	}

	// 4. Explicit full-path entry in AllowBinaries DOES allow that specific path inside a writable root.
	cfgFull := DefaultJailConfig(ws)
	cfgFull.Exec = ExecPolicy{
		ExecDefault:   DecisionDeny,
		AllowBinaries: []string{wsFakeGit},
	}
	jailFull, err := NewJail(cfgFull)
	if err != nil {
		t.Fatal(err)
	}
	sessFull, err := NewSession(WithWorkDir(ws), WithJail(jailFull))
	if err != nil {
		t.Fatal(err)
	}
	defer sessFull.Close()

	rExplicit, err := sessFull.Exec(context.Background(), fmt.Sprintf(`"./%s" -test.run=^TestNonExistentNothing$`, binName))
	if err != nil {
		t.Fatal(err)
	}
	if rExplicit.ExitCode != 0 {
		t.Fatalf("expected explicit full-path AllowBinaries entry to succeed, got exit=%d stderr=%q", rExplicit.ExitCode, rExplicit.Stderr)
	}
}

func TestRedTeam2_JailedCLIInteractiveMode(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	fakeHome := t.TempDir()

	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	outsideSecret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("HOST_SECRET_DATA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rcTouch := filepath.Join(outside, "rc_executed.txt")
	unishrc := filepath.Join(fakeHome, ".unishrc")
	rcBody := fmt.Sprintf("echo UNISHRC_LEAKED\necho pwn > %q\n", filepath.ToSlash(rcTouch))
	if err := os.WriteFile(unishrc, []byte(rcBody), 0o644); err != nil {
		t.Fatal(err)
	}

	insideOut := filepath.Join(ws, "interactive_ok.txt")
	scriptLines := strings.Join([]string{
		fmt.Sprintf(`cat < "%s"`, filepath.ToSlash(outsideSecret)),
		fmt.Sprintf(`echo escaped > "%s"`, filepath.ToSlash(filepath.Join(outside, "escaped.txt"))),
		"if true; then",
		fmt.Sprintf(`  echo "jailed-repl-ok" > "%s"`, filepath.ToSlash(insideOut)),
		"fi",
		"exit 23",
		"echo AFTER_EXIT_UNREACHABLE",
	}, "\n") + "\n"

	inFile, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inFile.WriteString(scriptLines); err != nil {
		t.Fatal(err)
	}
	if _, err := inFile.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	defer inFile.Close()

	outFile, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	errFile, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer errFile.Close()

	origIn, origOut, origErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inFile, outFile, errFile
	rc := RunCLI("test", []string{"unish", "-i", "--jail", ws})
	os.Stdin, os.Stdout, os.Stderr = origIn, origOut, origErr

	if rc != 23 {
		t.Fatalf("expected jailed interactive RunCLI exit code 23, got %d", rc)
	}

	outBytes, _ := os.ReadFile(outFile.Name())
	outStr := string(outBytes)
	if strings.Contains(outStr, "UNISHRC_LEAKED") || strings.Contains(outStr, "HOST_SECRET_DATA") || strings.Contains(outStr, "AFTER_EXIT_UNREACHABLE") {
		t.Fatalf("jailed interactive mode leaked host rc/secret or ignored exit: stdout=%q", outStr)
	}
	if _, err := os.Stat(rcTouch); err == nil {
		t.Fatal("jailed interactive mode executed ~/.unishrc outside jail!")
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped.txt")); err == nil {
		t.Fatal("jailed interactive mode allowed write outside jail!")
	}
	if _, err := os.Stat(filepath.Join(fakeHome, ".unish_history")); err == nil {
		t.Fatal("jailed interactive mode wrote ~/.unish_history outside jail!")
	}
	gotInside, err := os.ReadFile(insideOut)
	if err != nil || strings.TrimSpace(string(gotInside)) != "jailed-repl-ok" {
		t.Fatalf("expected inside write in jailed interactive mode, err=%v got=%q", err, string(gotInside))
	}

	// Verify EXIT trap executes on clean EOF in runJailedInteractive.
	jailEOF, err := NewJail(DefaultJailConfig(ws))
	if err != nil {
		t.Fatal(err)
	}
	trapOut := filepath.Join(ws, "eof_trap.txt")
	eofIn := strings.NewReader(fmt.Sprintf("trap 'echo trap_fired > \"%s\"' EXIT\n(exit 7)\n", filepath.ToSlash(trapOut)))
	var eofStdout, eofStderr bytes.Buffer
	rcEOF := runJailedInteractive(jailEOF, eofIn, &eofStdout, &eofStderr, false)
	if rcEOF != 7 {
		t.Fatalf("expected EOF exit status 7, got %d (stderr=%q)", rcEOF, eofStderr.String())
	}
	trapBytes, err := os.ReadFile(trapOut)
	if err != nil || strings.TrimSpace(string(trapBytes)) != "trap_fired" {
		t.Fatalf("expected EXIT trap to fire on EOF, err=%v got=%q", err, string(trapBytes))
	}
}

func TestRedTeam3_SessionScrubEnvAndNoInheritLeakDefense(t *testing.T) {
	ws := t.TempDir()
	hostBinDir := t.TempDir()

	leakCmdName := "rt_host_secret_path_cmd"
	leakCmdFile := leakCmdName
	if runtime.GOOS == "windows" {
		leakCmdFile += ".exe"
	}
	if err := os.WriteFile(filepath.Join(hostBinDir, leakCmdFile), []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RT_SCRUB_API_KEY", "sk-top-secret-key-val")
	t.Setenv("RT_SCRUB_DB_SECRET", "db-ultra-secret-val")
	t.Setenv("RT_PUBLIC_HOST_VAR", "public-host-ok")
	t.Setenv("PATH", hostBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Case A: WithInheritEnv(true) + WithScrubEnv("*_KEY", "*_SECRET")
	sessScrub, err := NewSession(
		WithWorkDir(ws),
		WithInheritEnv(true),
		WithScrubEnv("*_KEY", "*_SECRET"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer sessScrub.Close()

	for _, cmd := range []string{
		"printenv RT_SCRUB_API_KEY",
		"printenv RT_SCRUB_DB_SECRET",
		"env printenv RT_SCRUB_API_KEY",
		"nice printenv RT_SCRUB_DB_SECRET",
	} {
		res, err := sessScrub.Exec(context.Background(), cmd)
		if err != nil {
			t.Fatalf("%s error: %v", cmd, err)
		}
		if res.ExitCode == 0 || strings.Contains(res.Stdout, "secret") {
			t.Fatalf("%s leaked scrubbed host var! exit=%d stdout=%q", cmd, res.ExitCode, res.Stdout)
		}
	}

	for _, dumpCmd := range []string{"printenv", "env", "env EXTRA_TMP=1 env", "nice env"} {
		res, err := sessScrub.Exec(context.Background(), dumpCmd)
		if err != nil {
			t.Fatalf("%s error: %v", dumpCmd, err)
		}
		if strings.Contains(res.Stdout, "RT_SCRUB_API_KEY") ||
			strings.Contains(res.Stdout, "sk-top-secret-key-val") ||
			strings.Contains(res.Stdout, "RT_SCRUB_DB_SECRET") ||
			strings.Contains(res.Stdout, "db-ultra-secret-val") {
			t.Fatalf("%s leaked scrubbed secret! stdout=%q", dumpCmd, res.Stdout)
		}
		if !strings.Contains(res.Stdout, "RT_PUBLIC_HOST_VAR=public-host-ok") {
			t.Fatalf("%s missing non-scrubbed var RT_PUBLIC_HOST_VAR", dumpCmd)
		}
	}

	// Completion on scrubbed session must NOT suggest scrubbed host variable names!
	_, varCands := completeWord("echo $RT_SCRUB_", len("echo $RT_SCRUB_"), sessScrub.runner, nil)
	if len(varCands) != 0 {
		t.Fatalf("completeWord leaked scrubbed host variable names: %v", varCands)
	}
	repPub, pubCands := completeWord("echo $RT_PUBLIC_HOST_", len("echo $RT_PUBLIC_HOST_"), sessScrub.runner, nil)
	if repPub != "$RT_PUBLIC_HOST_VAR" && len(pubCands) == 0 {
		t.Fatalf("expected non-scrubbed session var to complete, got rep=%q cands=%v", repPub, pubCands)
	}

	// Case B: WithInheritEnv(false) without PATH
	sessIsolated, err := NewSession(
		WithWorkDir(ws),
		WithInheritEnv(false),
		WithEnv([]string{"ONLY_EXPLICIT=present"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer sessIsolated.Close()

	rIsoPrint, err := sessIsolated.Exec(context.Background(), "printenv RT_PUBLIC_HOST_VAR")
	if err != nil {
		t.Fatal(err)
	}
	if rIsoPrint.ExitCode == 0 || rIsoPrint.Stdout != "" {
		t.Fatalf("WithInheritEnv(false) printenv leaked host var: exit=%d stdout=%q", rIsoPrint.ExitCode, rIsoPrint.Stdout)
	}

	rIsoEnv, err := sessIsolated.Exec(context.Background(), "env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rIsoEnv.Stdout, "RT_PUBLIC_HOST_VAR") || !strings.Contains(rIsoEnv.Stdout, "ONLY_EXPLICIT=present") {
		t.Fatalf("WithInheritEnv(false) env mismatch: %q", rIsoEnv.Stdout)
	}

	// Command completion with empty session PATH must NOT fall back to os.Getenv("PATH")!
	repCmd, cmdCands := completeWord("rt_host_secret_", len("rt_host_secret_"), sessIsolated.runner, nil)
	if repCmd != "" || len(cmdCands) != 0 {
		t.Fatalf("WithInheritEnv(false) completeWord leaked host PATH binary: rep=%q cands=%v", repCmd, cmdCands)
	}
}

func TestRedTeam4_ConcurrentSubshellCdMSYSAndSelfKillSafety(t *testing.T) {
	ws := t.TempDir()
	extA := t.TempDir()
	extB := t.TempDir()
	deniedDir := t.TempDir()

	realA, _ := canonicalizeForCheck(extA)
	realB, _ := canonicalizeForCheck(extB)
	realWS, _ := canonicalizeForCheck(ws)

	// 4a. Concurrent subshell `cd` with per-request Approver (DecisionAllow, NOT cached) — zero race!
	cfgAsk := DefaultJailConfig(ws)
	cfgAsk.OutsideAction = DecisionAsk
	cfgAsk.Approver = func(ctx context.Context, req AccessRequest) (Decision, error) {
		return DecisionAllow, nil
	}
	jailAsk, err := NewJail(cfgAsk)
	if err != nil {
		t.Fatal(err)
	}
	sessRace, err := NewSession(WithWorkDir(ws), WithJail(jailAsk))
	if err != nil {
		t.Fatal(err)
	}
	defer sessRace.Close()

	subshellScript := fmt.Sprintf(`(cd "%s" && pwd) & (cd "%s" && pwd) & wait`, filepath.ToSlash(extA), filepath.ToSlash(extB))
	for i := range 20 {
		res, err := sessRace.Exec(context.Background(), subshellScript)
		if err != nil {
			t.Fatalf("iter %d err: %v", i, err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("iter %d exit=%d stderr=%q", i, res.ExitCode, res.Stderr)
		}
		outNorm := normalizeMatchPath(res.Stdout)
		if !strings.Contains(outNorm, normalizeMatchPath(realA)) || !strings.Contains(outNorm, normalizeMatchPath(realB)) {
			t.Fatalf("iter %d missing expected subshell pwd output: got %q (want %q and %q)", i, res.Stdout, realA, realB)
		}
		if normalizeMatchPath(sessRace.Cwd()) != normalizeMatchPath(realWS) {
			t.Fatalf("iter %d parent cwd mutated: %q", i, sessRace.Cwd())
		}
	}

	// Verify `builtin cd` and `command cd` are also gated by OpChdir, and `builtin umask` stays virtualized.
	jailStrict, err := NewJail(DefaultJailConfig(ws))
	if err != nil {
		t.Fatal(err)
	}
	sessStrict, err := NewSession(WithWorkDir(ws), WithJail(jailStrict))
	if err != nil {
		t.Fatal(err)
	}
	defer sessStrict.Close()

	for _, cdVariant := range []string{
		fmt.Sprintf(`builtin cd "%s"`, filepath.ToSlash(deniedDir)),
		fmt.Sprintf(`command cd "%s"`, filepath.ToSlash(deniedDir)),
	} {
		res, err := sessStrict.Exec(context.Background(), cdVariant)
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode == 0 || len(res.Denials) == 0 || res.Denials[0].Op != OpChdir {
			t.Fatalf("expected %q blocked with OpChdir, got exit=%d denials=%+v", cdVariant, res.ExitCode, res.Denials)
		}
	}

	rUmask, err := sessStrict.Exec(context.Background(), "builtin umask 0077 && command umask")
	if err != nil || rUmask.ExitCode != 0 || strings.TrimSpace(rUmask.Stdout) != "0077" || sessStrict.Umask() != 0o077 {
		t.Fatalf("builtin/command umask virtualization failed: err=%v res=%+v umask=%04o", err, rUmask, sessStrict.Umask())
	}

	// Verify ulimit setting, history -r outside, pushd -n ../.. -> popd, and PATH=/outside oracles are blocked!
	outsideBin := filepath.Join(deniedDir, "secret_probe_bin")
	if runtime.GOOS == "windows" {
		outsideBin += ".exe"
	}
	_ = os.WriteFile(outsideBin, []byte("stub"), 0o755)
	for _, guardCmd := range []string{
		"ulimit -n 8",
		fmt.Sprintf(`history -r "%s"`, filepath.ToSlash(outsideBin)),
		fmt.Sprintf(`PATH=%q command -v secret_probe_bin`, deniedDir),
		fmt.Sprintf(`PATH=%q type -P secret_probe_bin`, deniedDir),
		fmt.Sprintf(`PATH=%q hash secret_probe_bin`, deniedDir),
		fmt.Sprintf(`PATH=%q source secret_probe_bin`, deniedDir),
		`mkdir -p sub1/sub2 && cd sub1/sub2 && pushd -n ../.. >/dev/null && cd ../.. && popd >/dev/null && cd ..`,
	} {
		rG, err := sessStrict.Exec(context.Background(), guardCmd)
		if err != nil {
			t.Fatalf("guard %q error: %v", guardCmd, err)
		}
		if rG.ExitCode == 0 {
			t.Fatalf("expected guard %q to fail in jailed session, got exit=0 stdout=%q", guardCmd, rG.Stdout)
		}
	}
	_ = sessStrict.SetCwd(ws)

	// 4b. Windows MSYS path normalization (`cd /c/...`) in Session.
	if runtime.GOOS == "windows" && len(realWS) >= 3 && realWS[1] == ':' {
		msysSub := filepath.Join(realWS, "msys_child")
		if err := os.MkdirAll(msysSub, 0o755); err != nil {
			t.Fatal(err)
		}
		drive := strings.ToLower(string(realWS[0]))
		msysTarget := "/" + drive + filepath.ToSlash(msysSub[2:])
		rMSYS, err := sessStrict.Exec(context.Background(), fmt.Sprintf(`cd "%s" && pwd`, msysTarget))
		if err != nil {
			t.Fatal(err)
		}
		if rMSYS.ExitCode != 0 || normalizeMatchPath(sessStrict.Cwd()) != normalizeMatchPath(msysSub) {
			t.Fatalf("Windows MSYS cd %q failed: exit=%d stderr=%q cwd=%q", msysTarget, rMSYS.ExitCode, rMSYS.Stderr, sessStrict.Cwd())
		}
	}

	// 4c. Self-PID and broadcast `kill` safety in BOTH unjailed and ProcDefault:Allow jailed Sessions!
	cfgProcAllow := DefaultJailConfig(ws)
	cfgProcAllow.Proc.ProcDefault = DecisionAllow
	jailProcAllow, err := NewJail(cfgProcAllow)
	if err != nil {
		t.Fatal(err)
	}

	unjailedSess, err := NewSession(WithWorkDir(ws))
	if err != nil {
		t.Fatal(err)
	}
	defer unjailedSess.Close()

	jailedProcSess, err := NewSession(WithWorkDir(ws), WithJail(jailProcAllow))
	if err != nil {
		t.Fatal(err)
	}
	defer jailedProcSess.Close()

	for name, sess := range map[string]*Session{
		"unjailed":   unjailedSess,
		"proc_allow": jailedProcSess,
		"proc_deny":  sessStrict,
	} {
		// Subshell self-SIGKILL in canonical and non-canonical numeric forms + `command kill` + `env kill`:
		// MUST NEVER kill the Go test process, and MUST exit subshell with 137!
		rSub, err := sess.Exec(context.Background(), `(kill -9 $$); a=$?; (kill -9 0$$); b=$?; (kill -9 +$$); c=$?; (command kill -9 $$); d=$?; (env kill -9 $$); e=$?; printf "%d,%d,%d,%d,%d" "$a" "$b" "$c" "$d" "$e"`)
		if err != nil {
			t.Fatalf("[%s] subshell self-kill err: %v", name, err)
		}
		if rSub.Stdout != "137,137,137,137,137" {
			t.Fatalf("[%s] expected subshell self-kills 137,137,137,137,137, got %q (stderr=%q)", name, rSub.Stdout, rSub.Stderr)
		}

		// Broadcast / group kills (0, -1, -- -1) must NEVER signal host process group!
		for _, bcast := range []string{"kill -9 0", "kill -9 -1", "kill -9 -- -1"} {
			rB, err := sess.Exec(context.Background(), bcast)
			if err != nil {
				t.Fatalf("[%s] %s err: %v", name, bcast, err)
			}
			if rB.ExitCode == 0 {
				t.Fatalf("[%s] expected %q to fail safely with non-zero exit", name, bcast)
			}
			if name == "proc_deny" && (len(rB.Denials) == 0 || rB.Denials[0].Op != OpProc) {
				t.Fatalf("[%s] expected %q to record OpProc denial, got %+v", name, bcast, rB.Denials)
			}
		}

		// Trapped self-signal runs trap handler in-process without exiting!
		rTrap, err := sess.Exec(context.Background(), `trap 'printf "TRAP_OK:"' TERM; kill -TERM $$; printf "AFTER_OK"`)
		if err != nil {
			t.Fatalf("[%s] trapped self-kill err: %v", name, err)
		}
		if rTrap.ExitCode != 0 || rTrap.Stdout != "TRAP_OK:AFTER_OK" {
			t.Fatalf("[%s] expected TRAP_OK:AFTER_OK, got exit=%d stdout=%q stderr=%q", name, rTrap.ExitCode, rTrap.Stdout, rTrap.Stderr)
		}
		_, _ = sess.Exec(context.Background(), `trap - TERM`)

		// Top-level `kill -9 $$` halts current Exec with 137, AND leaves Session alive for subsequent Execs!
		rTop, err := sess.Exec(context.Background(), `kill -9 $$; echo UNREACHABLE`)
		if err != nil {
			t.Fatalf("[%s] top-level self-kill err: %v", name, err)
		}
		if rTop.ExitCode != 137 || strings.Contains(rTop.Stdout, "UNREACHABLE") {
			t.Fatalf("[%s] expected top-level kill -9 $$ exit=137, got exit=%d stdout=%q", name, rTop.ExitCode, rTop.Stdout)
		}

		rRecover, err := sess.Exec(context.Background(), `echo RECOVERED`)
		if err != nil || rRecover.ExitCode != 0 || strings.TrimSpace(rRecover.Stdout) != "RECOVERED" {
			t.Fatalf("[%s] session did not recover after top-level kill -9 $$: err=%v res=%+v", name, err, rRecover)
		}
	}
}

func TestRedTeam5_HarnessCancelQueuedAndStaleTargetIsolation(t *testing.T) {
	ws := t.TempDir()
	clientInR, clientInW := io.Pipe()
	serverOutR, serverOutW := io.Pipe()

	srv, err := NewHarnessServer(clientInR, serverOutW, WithWorkDir(ws))
	if err != nil {
		t.Fatal(err)
	}

	srvDone := make(chan error, 1)
	go func() {
		err := srv.Serve(context.Background())
		_ = serverOutW.Close()
		srvDone <- err
	}()
	defer func() {
		_ = clientInW.Close()
		<-srvDone
		_ = srv.Session().Close()
	}()

	send := func(msg map[string]any) {
		t.Helper()
		b, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, '\n')
		if _, err := clientInW.Write(b); err != nil {
			t.Fatal(err)
		}
	}

	eventsCh := make(chan HarnessEvent, 64)
	go func() {
		sc := bufio.NewScanner(serverOutR)
		for sc.Scan() {
			var ev HarnessEvent
			if err := json.Unmarshal(sc.Bytes(), &ev); err == nil {
				eventsCh <- ev
			}
		}
		close(eventsCh)
	}()

	nextEvent := func(timeout time.Duration) HarnessEvent {
		t.Helper()
		select {
		case ev, ok := <-eventsCh:
			if !ok {
				t.Fatal("harness stream closed unexpectedly")
			}
			return ev
		case <-time.After(timeout):
			t.Fatalf("timed out after %v waiting for event", timeout)
			return HarnessEvent{}
		}
	}

	// Enqueue e1 (slow) and e2 (queued behind e1) back-to-back.
	send(map[string]any{
		"id":     "e1",
		"type":   "exec",
		"script": `echo "e1_running"; sleep 0.35; echo "e1_completed"`,
	})
	send(map[string]any{
		"id":     "e2",
		"type":   "exec",
		"script": `echo "e2_should_never_execute"`,
	})

	// Wait until e1 has started running so e2 is guaranteed queued.
	for {
		ev := nextEvent(5 * time.Second)
		if ev.Type == "stdout" && ev.ID == "e1" && strings.Contains(ev.Data, "e1_running") {
			break
		}
	}

	// Cancel queued e2 AND send a bogus req_id cancel — neither may cancel active e1!
	send(map[string]any{"type": "cancel", "req_id": "e2"})
	send(map[string]any{"type": "cancel", "req_id": "unrelated-id"})

	var gotE1Result, gotE2Result *HarnessEvent
	for gotE1Result == nil || gotE2Result == nil {
		ev := nextEvent(5 * time.Second)
		if ev.Type == "stdout" && strings.Contains(ev.Data, "e2_should_never_execute") {
			t.Fatalf("queued cancelled e2 actually executed! ev=%+v", ev)
		}
		if ev.Type == "result" {
			evCopy := ev
			switch ev.ID {
			case "e1":
				gotE1Result = &evCopy
			case "e2":
				gotE2Result = &evCopy
			}
		}
	}

	if !gotE2Result.Cancelled || gotE2Result.ExitCode == nil || *gotE2Result.ExitCode != 130 {
		t.Fatalf("expected queued e2 cancelled result (130), got %+v", gotE2Result)
	}
	if gotE1Result.Cancelled || gotE1Result.ExitCode == nil || *gotE1Result.ExitCode != 0 || gotE1Result.Stdout == nil || !strings.Contains(*gotE1Result.Stdout, "e1_completed") {
		t.Fatalf("cancelling queued e2 erroneously disturbed active e1: %+v", gotE1Result)
	}

	// Now start e3 and send a late cancel referencing already-finished e1 — e3 must NOT be cancelled!
	send(map[string]any{
		"id":     "e3",
		"type":   "exec",
		"script": `echo "e3_running"; sleep 0.25; echo "e3_completed"`,
	})
	for {
		ev := nextEvent(5 * time.Second)
		if ev.Type == "stdout" && ev.ID == "e3" && strings.Contains(ev.Data, "e3_running") {
			break
		}
	}
	send(map[string]any{"type": "cancel", "req_id": "e1"})

	for {
		ev := nextEvent(5 * time.Second)
		if ev.Type == "result" && ev.ID == "e3" {
			if ev.Cancelled || ev.ExitCode == nil || *ev.ExitCode != 0 || ev.Stdout == nil || !strings.Contains(*ev.Stdout, "e3_completed") {
				t.Fatalf("late cancel for e1 cancelled subsequent e3! %+v", ev)
			}
			break
		}
	}
}

func TestRedTeam6_NetPolicyUnspecifiedLoopbackCGNATAndListenRebinding(t *testing.T) {
	ws := t.TempDir()
	noopDial := func(ctx context.Context, network, address string) (net.Conn, error) {
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}

	// 6a. `--net loopback` (AllowLoopback: true, Default: DecisionDeny):
	// MUST block 0.0.0.0, 0.1.2.3, and :: on BOTH DialContext and Listen!
	cfgLoop := DefaultJailConfig(ws)
	cfgLoop.Net = NetPolicy{
		Default:         DecisionDeny,
		AllowLoopback:   true,
		BlockPrivateLAN: true,
		TransportDialer: noopDial,
	}
	jailLoop, err := NewJail(cfgLoop)
	if err != nil {
		t.Fatal(err)
	}
	for _, badAddr := range []string{"0.0.0.0:8080", "0.1.2.3:8080", "[::]:8080"} {
		if _, err := jailLoop.DialContext(context.Background(), "nc", "tcp", badAddr); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("expected DialContext(%s) denied in loopback mode, got %v", badAddr, err)
		}
		if _, err := jailLoop.Listen(context.Background(), "nc", "tcp", badAddr); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("expected Listen(%s) denied in loopback mode, got %v", badAddr, err)
		}
	}
	cLoop, err := jailLoop.DialContext(context.Background(), "nc", "tcp", "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("expected 127.0.0.1:8080 allowed in loopback mode, got %v", err)
	}
	_ = cLoop.Close()

	// 6b. BlockPrivateLAN: true blocks CGNAT 100.64.0.0/10 and NAT64 64:ff9b::/96!
	cfgLAN := DefaultJailConfig(ws)
	cfgLAN.Net = NetPolicy{
		Default:         DecisionAllow,
		AllowLoopback:   false,
		BlockPrivateLAN: true,
		TransportDialer: noopDial,
	}
	jailLAN, err := NewJail(cfgLAN)
	if err != nil {
		t.Fatal(err)
	}
	for _, blocked := range []string{
		"100.64.0.1:443",
		"100.100.100.200:80",
		"100.127.255.254:443",
		"[64:ff9b::a00:1]:443",
		"[64:ff9b::c0a8:101]:80",
		"0.0.0.0:80",
		"[::]:80",
	} {
		if _, err := jailLAN.DialContext(context.Background(), "curl", "tcp", blocked); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("expected BlockPrivateLAN to deny %s, got %v", blocked, err)
		}
	}
	// Public IP just outside 100.64.0.0/10 (100.63.255.254) succeeds.
	cPub, err := jailLAN.DialContext(context.Background(), "curl", "tcp", "100.63.255.254:443")
	if err != nil {
		t.Fatalf("expected public IP 100.63.255.254:443 allowed, got %v", err)
	}
	_ = cPub.Close()

	// 6c. AllowLoopback: false with BlockPrivateLAN: false STILL blocks loopback (direct & DNS-rebound)!
	cfgNoLoop := DefaultJailConfig(ws)
	cfgNoLoop.Net = NetPolicy{
		Default:         DecisionAllow,
		AllowLoopback:   false,
		BlockPrivateLAN: false,
		TransportDialer: noopDial,
	}
	jailNoLoop, err := NewJail(cfgNoLoop)
	if err != nil {
		t.Fatal(err)
	}
	jailNoLoop.lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	for _, loopTarget := range []string{"127.0.0.1:8080", "[::1]:8080", "rebind.attacker.example:8080"} {
		if _, err := jailNoLoop.DialContext(context.Background(), "curl", "tcp", loopTarget); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("expected AllowLoopback=false (BlockPrivateLAN=false) to deny %s, got %v", loopTarget, err)
		}
	}

	// 6d. Jail.Listen / ListenPacket hostname resolution & DNS-rebinding defense!
	cfgListen := DefaultJailConfig(ws)
	cfgListen.Net = NetPolicy{
		Default:         DecisionDeny,
		AllowLoopback:   true,
		BlockPrivateLAN: true,
		AllowListen:     DecisionDeny,
		Rules: []NetRule{
			{
				HostPattern: "bind.svc.local",
				Ops:         []AccessOp{OpNetListen},
				Decision:    DecisionAllow,
			},
		},
	}
	jailListen, err := NewJail(cfgListen)
	if err != nil {
		t.Fatal(err)
	}

	// Hostname resolving to 0.0.0.0 wildcard MUST be rejected before net.Listen!
	jailListen.lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("0.0.0.0")}}, nil
	}
	if _, err := jailListen.Listen(context.Background(), "nc", "tcp", "bind.svc.local:0"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected Listen rebinding to 0.0.0.0 denied, got %v", err)
	}
	if _, err := jailListen.ListenPacket(context.Background(), "nc", "udp", "bind.svc.local:0"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected ListenPacket rebinding to 0.0.0.0 denied, got %v", err)
	}

	// Hostname resolving to 127.0.0.1 succeeds AND binds pinned 127.0.0.1:port directly!
	var listenLookups atomic.Int32
	jailListen.lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		listenLookups.Add(1)
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	ln, err := jailListen.Listen(context.Background(), "nc", "tcp", "bind.svc.local:0")
	if err != nil {
		t.Fatalf("expected Listen on bind.svc.local -> 127.0.0.1 allowed, got %v", err)
	}
	defer ln.Close()
	if listenLookups.Load() != 1 || !strings.HasPrefix(ln.Addr().String(), "127.0.0.1:") {
		t.Fatalf("expected 1 lookup and pinned 127.0.0.1 listener, got lookups=%d addr=%s", listenLookups.Load(), ln.Addr().String())
	}
}
