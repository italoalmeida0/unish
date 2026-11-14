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
