package unish

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestIntegrationHelperProcess provides a portable, zero-CGO external binary
// (via os.Executable() + UNISH_INTEGRATION_HELPER_MODE) for testing real OS
// child process tracking, cross-session PID isolation, and scrubbed env inheritance.
func TestIntegrationHelperProcess(t *testing.T) {
	mode := os.Getenv("UNISH_INTEGRATION_HELPER_MODE")
	if mode == "" {
		return
	}
	switch mode {
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "dumpenv":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
		os.Exit(0)
	case "cli":
		var args []string
		if raw := os.Getenv("UNISH_INTEGRATION_CLI_ARGS"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &args)
		}
		os.Exit(RunCLI("1.2.0-test", args))
	}
}

func TestIntegrationSessionStatefulProcSubstAndJobs(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	wsA := filepath.Join(base, "wsA")
	wsB := filepath.Join(base, "wsB")
	if err := os.MkdirAll(filepath.Join(wsA, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wsB, 0o755); err != nil {
		t.Fatal(err)
	}

	selfExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	cfgA := DefaultJailConfig(wsA)
	cfgA.Exec = ExecPolicy{
		Mode:          ExecAllowlist,
		AllowBinaries: []string{selfExe},
	}
	cfgA.Proc = ProcPolicy{
		Mode: ProcOwnSessionOnly,
	}
	jailA, err := NewJail(cfgA)
	if err != nil {
		t.Fatalf("NewJail A: %v", err)
	}

	cfgB := DefaultJailConfig(wsB)
	cfgB.Proc = ProcPolicy{
		Mode: ProcOwnSessionOnly,
	}
	jailB, err := NewJail(cfgB)
	if err != nil {
		t.Fatalf("NewJail B: %v", err)
	}

	sessA, err := NewSession(WithWorkDir(wsA), WithJail(jailA))
	if err != nil {
		t.Fatalf("NewSession A: %v", err)
	}
	defer sessA.Close()

	sessB, err := NewSession(WithWorkDir(wsB), WithJail(jailB))
	if err != nil {
		t.Fatalf("NewSession B: %v", err)
	}
	defer sessB.Close()

	// 1. Persistent state across sequential Exec calls: cd, vars, exports, functions, aliases, $?.
	r1, err := sessA.Exec(ctx, `
		cd sub
		LOCAL_VAR="local-ok"
		export EXPORT_VAR="export-ok"
		greet() { printf "%s:%s:%s" "$1" "$LOCAL_VAR" "$EXPORT_VAR"; }
		alias hi='greet world'
		(exit 17)
	`)
	if err != nil {
		t.Fatalf("Exec 1 err: %v", err)
	}
	if r1.ExitCode != 17 || sessA.LastExitCode() != 17 {
		t.Fatalf("expected exit code 17, got res=%d last=%d", r1.ExitCode, sessA.LastExitCode())
	}

	r2, err := sessA.Exec(ctx, `printf "prev=%d out=%s dir=%s" "$?" "$(hi)" "$(basename "$PWD")"`)
	if err != nil || r2.ExitCode != 0 {
		t.Fatalf("Exec 2 failed: err=%v res=%+v", err, r2)
	}
	if r2.Stdout != "prev=17 out=world:local-ok:export-ok dir=sub" {
		t.Fatalf("unexpected stateful output: %q", r2.Stdout)
	}

	// 2. Process substitution `<(cmd)` and `>(cmd)` inside jailed Session (temp/FIFO outside Roots!).
	rProc, err := sessA.Exec(ctx, `
		diff -u <(printf "one\ntwo\n") <(printf "one\ntwo\n")
		JVAL=$(jq -r '.msg' <(echo '{"msg":"procsubst-jq"}'))
		AVAL=$(awk '{print $2}' <(echo "first procsubst-awk"))
		printf "hello\n" | tee >(tr 'a-z' 'A-Z' > upper.txt) >/dev/null
		wait
		printf "%s|%s|%s" "$JVAL" "$AVAL" "$(cat upper.txt)"
	`)
	if err != nil || rProc.ExitCode != 0 {
		t.Fatalf("jailed procsubst failed: err=%v res=%+v", err, rProc)
	}
	if strings.TrimSpace(rProc.Stdout) != "procsubst-jq|procsubst-awk|HELLO" {
		t.Fatalf("unexpected procsubst output: %q", rProc.Stdout)
	}

	// Also verify `<(cmd)` in an unjailed Session.
	unjailedSess, err := NewSession(WithWorkDir(wsB))
	if err != nil {
		t.Fatal(err)
	}
	defer unjailedSess.Close()
	rUnjProc, err := unjailedSess.Exec(ctx, `cat <(printf "unjailed-procsubst")`)
	if err != nil || rUnjProc.ExitCode != 0 || rUnjProc.Stdout != "unjailed-procsubst" {
		t.Fatalf("unjailed procsubst failed: err=%v res=%+v", err, rUnjProc)
	}

	// 3. Per-session in-process background jobs (`sleep 10 &`, `jobs`, `bg`, `kill %1`, `wait`, `fg`).
	if _, err := sessA.Exec(ctx, `sleep 10 &`); err != nil {
		t.Fatalf("launch bg sleep in sessA: %v", err)
	}

	rJobsA, err := sessA.Exec(ctx, `jobs`)
	if err != nil || !strings.Contains(rJobsA.Stdout, "Running") || !strings.Contains(rJobsA.Stdout, "sleep 10") {
		t.Fatalf("expected sleep 10 in sessA jobs, got %+v", rJobsA)
	}

	// Session B's job table MUST be completely empty!
	rJobsB, err := sessB.Exec(ctx, `jobs`)
	if err != nil || strings.TrimSpace(rJobsB.Stdout) != "" {
		t.Fatalf("expected empty jobs in sessB, got %q", rJobsB.Stdout)
	}
	rKillB, _ := sessB.Exec(ctx, `kill %1`)
	if rKillB.ExitCode == 0 {
		t.Fatalf("sessB kill %%1 should fail when sessB has no jobs")
	}

	// `bg %1` and `kill %1` + `wait` in sessA.
	rBgA, err := sessA.Exec(ctx, `bg %1`)
	if err != nil || rBgA.ExitCode != 0 || !strings.Contains(rBgA.Stdout, "sleep 10") {
		t.Fatalf("bg %%1 failed in sessA: err=%v res=%+v", err, rBgA)
	}
	rKillA, err := sessA.Exec(ctx, `kill -9 %1; wait %1; printf "rc=%d" "$?"`)
	if err != nil {
		t.Fatalf("kill/wait %%1 failed: %v", err)
	}
	if rKillA.Stdout != "rc=137" {
		t.Fatalf("expected wait %%1 after kill -9 to report rc=137, got %q (stderr=%q)", rKillA.Stdout, rKillA.Stderr)
	}
	rJobsAfter, _ := sessA.Exec(ctx, `jobs`)
	if strings.TrimSpace(rJobsAfter.Stdout) != "" {
		t.Fatalf("expected empty jobs after kill+wait, got %q", rJobsAfter.Stdout)
	}

	// `fg` brings a running background job to the foreground and waits for completion.
	rFg, err := sessA.Exec(ctx, `sleep 0.1 & fg %1`)
	if err != nil || rFg.ExitCode != 0 || !strings.Contains(rFg.Stdout, "sleep 0.1") {
		t.Fatalf("fg %%1 failed: err=%v res=%+v", err, rFg)
	}

	// 4. Real external child PID: ProcOwnSessionOnly allows signaling own live child
	//    while blocking other sessions from signaling that same OS PID!
	selfSlash := filepath.ToSlash(selfExe)
	rSpawn, err := sessA.Exec(ctx, fmt.Sprintf(
		`UNISH_INTEGRATION_HELPER_MODE=sleep %q -test.run=^TestIntegrationHelperProcess$ & printf "%%s" "$!"`,
		selfSlash,
	))
	if err != nil || rSpawn.ExitCode != 0 {
		t.Fatalf("spawn external helper in sessA failed: err=%v res=%+v", err, rSpawn)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(rSpawn.Stdout))
	if err != nil || childPID <= 0 {
		t.Fatalf("expected real OS child PID in $!, got %q", rSpawn.Stdout)
	}

	// sessB MUST be denied when attempting to probe/signal sessA's live child PID!
	rCrossKill, _ := sessB.Exec(ctx, fmt.Sprintf(`kill -0 %d`, childPID))
	if rCrossKill.ExitCode == 0 || len(rCrossKill.Denials) == 0 || rCrossKill.Denials[0].Op != OpProc {
		t.Fatalf("expected sessB kill -0 %d to be denied by OpProc, got %+v", childPID, rCrossKill)
	}

	// sessA CAN probe and kill its own live child PID without OpProc denial!
	rOwnProbe, err := sessA.Exec(ctx, fmt.Sprintf(`kill -0 %d`, childPID))
	if err != nil || rOwnProbe.ExitCode != 0 || len(rOwnProbe.Denials) != 0 {
		t.Fatalf("expected sessA kill -0 own child %d to succeed, got %+v", childPID, rOwnProbe)
	}
	rOwnKill, err := sessA.Exec(ctx, fmt.Sprintf(`kill -9 %d; wait %d 2>/dev/null; true`, childPID, childPID))
	if err != nil || rOwnKill.ExitCode != 0 {
		t.Fatalf("expected sessA kill -9 own child %d to succeed, got %+v", childPID, rOwnKill)
	}

	// 5. Session.Close() cleans up any remaining background jobs cleanly.
	sessCloseTest, err := NewSession(WithWorkDir(wsB), WithJail(jailB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessCloseTest.Exec(ctx, `sleep 30 &`); err != nil {
		t.Fatal(err)
	}
	closeStart := time.Now()
	if err := sessCloseTest.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if elapsed := time.Since(closeStart); elapsed > 3*time.Second {
		t.Fatalf("Session.Close hung on background job cleanup (%v)", elapsed)
	}
}

func TestIntegrationAllSubsystemsFSJailEnforcement(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	roDir := filepath.Join(base, "ro")
	outside := filepath.Join(base, "outside")

	for _, d := range []string{filepath.Join(ws, "sub"), roDir, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	outsideSecret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("OUTSIDE_SECRET_99\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "rc.sh"), []byte("PWNED=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	roFile := filepath.Join(roDir, "readonly.txt")
	if err := os.WriteFile(roFile, []byte("READONLY_OK\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	insideFile := filepath.Join(ws, "inside.txt")
	if err := os.WriteFile(insideFile, []byte("alpha line\nbeta line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "rc.sh"), []byte("INSIDE_RC=loaded\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build a malicious tar archive containing "../escaped_tar.txt".
	evilTarPath := filepath.Join(ws, "evil.tar")
	{
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		payload := []byte("tar-escape\n")
		if err := tw.WriteHeader(&tar.Header{
			Name: "../escaped_tar.txt",
			Mode: 0o644,
			Size: int64(len(payload)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(evilTarPath, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := DefaultJailConfig(ws)
	cfg.ReadOnlyRoots = []string{roDir}
	jail, err := NewJail(cfg)
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	sess, err := NewSession(WithWorkDir(ws), WithJail(jail))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	outSlash := filepath.ToSlash(outside)
	secretSlash := filepath.ToSlash(outsideSecret)
	roSlash := filepath.ToSlash(roFile)

	// Verify read-only root allows reading but blocks writing.
	resRO, err := sess.Exec(ctx, fmt.Sprintf(`cat %q`, roSlash))
	if err != nil || resRO.ExitCode != 0 || strings.TrimSpace(resRO.Stdout) != "READONLY_OK" {
		t.Fatalf("expected ReadOnlyRoot read to succeed, got %+v", resRO)
	}
	resROWrite, _ := sess.Exec(ctx, fmt.Sprintf(`echo nope >> %q`, roSlash))
	if resROWrite.ExitCode == 0 || len(resROWrite.Denials) == 0 {
		t.Fatalf("expected ReadOnlyRoot write to be denied, got %+v", resROWrite)
	}

	// Glob outside jail must NEVER reveal "secret.txt" or "rc.sh".
	resGlob, _ := sess.Exec(ctx, fmt.Sprintf(`echo %s/*`, outSlash))
	if strings.Contains(resGlob.Stdout, "secret.txt") || strings.Contains(resGlob.Stdout, "rc.sh") {
		t.Fatalf("glob leaked outside directory entries: %q", resGlob.Stdout)
	}

	// Table of blocked escape commands across redirects, test/[[, cd, source, cp/mv/rm/ln,
	// find, tar, rg, grep -r, sed -i, awk >, jq --rawfile, patch, sponge, flock.
	deniedScripts := []struct {
		name   string
		script string
	}{
		{"redirect-write", fmt.Sprintf(`echo pwn > %q`, outSlash+"/pwn.txt")},
		{"redirect-read", fmt.Sprintf(`cat < %q`, secretSlash)},
		{"redirect-fd3", fmt.Sprintf(`exec 3> %q`, outSlash+"/fd3.txt")},
		{"test-f", fmt.Sprintf(`[ -f %q ]`, secretSlash)},
		{"cond-f", fmt.Sprintf(`[[ -f %q ]]`, secretSlash)},
		{"cd-outside", fmt.Sprintf(`cd %q`, outSlash)},
		{"source-outside", fmt.Sprintf(`source %q`, outSlash+"/rc.sh")},
		{"cp-outside", fmt.Sprintf(`cp %q copied.txt`, secretSlash)},
		{"mv-outside", fmt.Sprintf(`mv inside.txt %q`, outSlash+"/moved.txt")},
		{"rm-outside", fmt.Sprintf(`rm %q`, secretSlash)},
		{"ln-s-outside", fmt.Sprintf(`ln -s %q sym_escape`, secretSlash)},
		{"ln-s-dangling-outside", fmt.Sprintf(`ln -s %q sym_dangling`, outSlash+"/new_escape.txt")},
		{"find-outside", fmt.Sprintf(`find %q`, outSlash)},
		{"tar-create-outside", fmt.Sprintf(`tar -cf leak.tar %q`, secretSlash)},
		{"tar-extract-escape", `tar -xf evil.tar`},
		{"rg-outside", fmt.Sprintf(`rg OUTSIDE %q`, outSlash)},
		{"grep-r-outside", fmt.Sprintf(`grep -r OUTSIDE %q`, outSlash)},
		{"sed-i-outside", fmt.Sprintf(`sed -i 's/OUTSIDE/X/' %q`, secretSlash)},
		{"awk-write-outside", fmt.Sprintf(`awk 'BEGIN { print "x" > %q }'`, outSlash+"/awk.txt")},
		{"awk-getline-outside", fmt.Sprintf(`awk 'BEGIN { if ((getline x < %q) > 0) print x; exit 1 }'`, secretSlash)},
		{"jq-rawfile-outside", fmt.Sprintf(`jq -n --rawfile s %q '$s'`, secretSlash)},
		{"patch-outside", fmt.Sprintf(`patch %q <<'EOF'
--- a/secret.txt
+++ b/secret.txt
@@ -1 +1 @@
-OUTSIDE_SECRET_99
+HACKED
EOF`, secretSlash)},
		{"sponge-outside", fmt.Sprintf(`echo x | sponge %q`, outSlash+"/sponge.txt")},
		{"flock-outside", fmt.Sprintf(`flock %q -c "echo bad"`, outSlash+"/lock")},
	}

	for _, tc := range deniedScripts {
		res, _ := sess.Exec(ctx, tc.script)
		if res.ExitCode == 0 {
			t.Errorf("[%s] expected non-zero exit code for %s, got stdout=%q stderr=%q", tc.name, tc.script, res.Stdout, res.Stderr)
		}
		if strings.Contains(res.Stdout, "OUTSIDE_SECRET_99") {
			t.Fatalf("[%s] leaked secret stdout: %q", tc.name, res.Stdout)
		}
	}

	// Pre-planted host symlink & dangling symlink inside workspace pointing outside:
	// verify neither reading nor writing through them can escape the jail.
	if canCreateSymlinks(t, ws) {
		liveLink := filepath.Join(ws, "host_live_sym")
		danglingTarget := filepath.Join(outside, "dangling_created.txt")
		danglingLink := filepath.Join(ws, "host_dangling_sym")
		if err := os.Symlink(outsideSecret, liveLink); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(danglingTarget, danglingLink); err != nil {
			t.Fatal(err)
		}

		for _, symCmd := range []string{
			`cat host_live_sym`,
			`cat < host_live_sym`,
			`grep -r OUTSIDE .`,
			`sed -i 's/OUTSIDE/X/' host_live_sym`,
			`jq -n --rawfile s host_live_sym '$s'`,
			`echo pwn > host_dangling_sym`,
			`cp inside.txt host_dangling_sym`,
			`echo pwn | sponge host_dangling_sym`,
		} {
			res, _ := sess.Exec(ctx, symCmd)
			if res.ExitCode == 0 || strings.Contains(res.Stdout, "OUTSIDE_SECRET_99") {
				t.Errorf("symlink escape succeeded for %q: %+v", symCmd, res)
			}
		}
		if _, err := os.Stat(danglingTarget); !os.IsNotExist(err) {
			t.Fatalf("dangling symlink created outside file %s!", danglingTarget)
		}
		_ = os.Remove(liveLink)
		_ = os.Remove(danglingLink)
	}

	// Verify outside directory was never mutated and secret is untouched.
	gotSecret, err := os.ReadFile(outsideSecret)
	if err != nil || string(gotSecret) != "OUTSIDE_SECRET_99\n" {
		t.Fatalf("outside secret modified: err=%v data=%q", err, string(gotSecret))
	}
	if _, err := os.Stat(filepath.Join(base, "escaped_tar.txt")); !os.IsNotExist(err) {
		t.Fatalf("tar escaped workspace to %s", filepath.Join(base, "escaped_tar.txt"))
	}

	// Verify legitimate in-workspace operations across ALL those same tools succeed!
	resOK, err := sess.Exec(ctx, `
		set -e
		source ./rc.sh
		[ "$INSIDE_RC" = "loaded" ]
		[ -f inside.txt ] && [[ -f inside.txt ]]
		sed -i 's/alpha/ALPHA/' inside.txt
		rg -q '^ALPHA line$' inside.txt
		grep -rq 'ALPHA' .
		awk '{ print $1 }' inside.txt > col1.txt
		jq -rn --rawfile s col1.txt '$s | split("\n")[0]' > jq_out.txt
		printf "patched\n" | sponge sponge_out.txt
		flock my.lock -c 'printf "locked:%s" "$(cat jq_out.txt)" > lock_res.txt'
		tar -cf good.tar jq_out.txt sponge_out.txt
		mkdir untar_dir && tar -xf good.tar -C untar_dir
		cat lock_res.txt
	`)
	if err != nil || resOK.ExitCode != 0 || resOK.Stdout != "locked:ALPHA" {
		t.Fatalf("legitimate in-jail pipeline failed: err=%v res=%+v", err, resOK)
	}
}

func TestIntegrationPathRuleDenyAndInteractiveApproverGrants(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(ws, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("DB_PASS=topsecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "config", ".env.production"), []byte("API_SECRET=prod99\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "app.txt"), []byte("public-app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	onceFile := filepath.Join(outside, "once.txt")
	cachedFile := filepath.Join(outside, "cached.txt")
	if err := os.WriteFile(onceFile, []byte("ONCE_OK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachedFile, []byte("CACHED_OK"), 0o644); err != nil {
		t.Fatal(err)
	}

	var promptCount atomic.Int32
	var nextDecision atomic.Int32
	nextDecision.Store(int32(DecisionDeny))

	cfg := DefaultJailConfig(ws)
	cfg.OutsideAction = DecisionAsk
	cfg.Rules = []PathRule{
		{
			Pattern:  "**/.env*",
			Decision: DecisionDeny,
			Reason:   "dotenv secrets protected",
		},
	}
	cfg.Approver = func(ctx context.Context, req AccessRequest) (Decision, error) {
		promptCount.Add(1)
		return Decision(nextDecision.Load()), nil
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

	// 1. `**/.env*` inside allowed workspace root is denied WITHOUT calling Approver!
	for _, cmd := range []string{
		`cat .env`,
		`cat config/.env.production`,
		`jq -n --rawfile s .env '$s'`,
		`echo OVERWRITE > .env`,
	} {
		res, _ := sess.Exec(ctx, cmd)
		if res.ExitCode == 0 || strings.Contains(res.Stdout, "topsecret") || strings.Contains(res.Stdout, "prod99") {
			t.Fatalf("expected %q to be denied by PathRule, got %+v", cmd, res)
		}
	}
	if got := promptCount.Load(); got != 0 {
		t.Fatalf("explicit Deny PathRule must not invoke Approver, got %d calls", got)
	}

	// Normal file inside workspace succeeds with 0 prompts.
	resPub, err := sess.Exec(ctx, `cat app.txt`)
	if err != nil || strings.TrimSpace(resPub.Stdout) != "public-app" || promptCount.Load() != 0 {
		t.Fatalf("expected public file to succeed without prompt, got %+v prompts=%d", resPub, promptCount.Load())
	}

	onceSlash := filepath.ToSlash(onceFile)
	cachedSlash := filepath.ToSlash(cachedFile)

	// 2. OutsideAction: DecisionAsk -> DecisionDeny
	nextDecision.Store(int32(DecisionDeny))
	resDeny, _ := sess.Exec(ctx, fmt.Sprintf(`cat %q`, onceSlash))
	if resDeny.ExitCode == 0 || len(resDeny.Denials) == 0 || promptCount.Load() != 1 {
		t.Fatalf("expected DecisionDeny on outside read, got res=%+v prompts=%d", resDeny, promptCount.Load())
	}

	// 3. DecisionAllow permits one call, and prompts again on the next call!
	nextDecision.Store(int32(DecisionAllow))
	resAllow1, err := sess.Exec(ctx, fmt.Sprintf(`cat %q`, onceSlash))
	if err != nil || resAllow1.Stdout != "ONCE_OK" || promptCount.Load() != 2 {
		t.Fatalf("expected one-shot DecisionAllow to succeed, got res=%+v prompts=%d", resAllow1, promptCount.Load())
	}
	resAllow2, err := sess.Exec(ctx, fmt.Sprintf(`cat %q`, onceSlash))
	if err != nil || resAllow2.Stdout != "ONCE_OK" || promptCount.Load() != 3 {
		t.Fatalf("expected second DecisionAllow read to re-prompt (3), got res=%+v prompts=%d", resAllow2, promptCount.Load())
	}

	// 4. DecisionAllowSession caches grant across subsequent Exec calls & builtins until revoked!
	nextDecision.Store(int32(DecisionAllowSession))
	resSess1, err := sess.Exec(ctx, fmt.Sprintf(`cat %q`, cachedSlash))
	if err != nil || resSess1.Stdout != "CACHED_OK" || promptCount.Load() != 4 {
		t.Fatalf("expected initial DecisionAllowSession read to succeed, got res=%+v prompts=%d", resSess1, promptCount.Load())
	}
	// Flip nextDecision to Deny to prove cached grant bypasses Approver!
	nextDecision.Store(int32(DecisionDeny))
	resSess2, err := sess.Exec(ctx, fmt.Sprintf(`printf "%%s|%%s" "$(cat < %q)" "$(jq -rn --rawfile s %q '$s')"`, cachedSlash, cachedSlash))
	if err != nil || resSess2.ExitCode != 0 || resSess2.Stdout != "CACHED_OK|CACHED_OK" || promptCount.Load() != 4 {
		t.Fatalf("expected cached session grant to satisfy cat & jq without prompting, got res=%+v prompts=%d", resSess2, promptCount.Load())
	}

	// Revoke grants -> prompts again (and denies since nextDecision == DecisionDeny).
	jail.RevokeSessionGrants()
	resAfterRevoke, _ := sess.Exec(ctx, fmt.Sprintf(`cat %q`, cachedSlash))
	if resAfterRevoke.ExitCode == 0 || promptCount.Load() != 5 {
		t.Fatalf("expected re-prompt & deny after RevokeSessionGrants, got res=%+v prompts=%d", resAfterRevoke, promptCount.Load())
	}
}

func TestIntegrationNetworkPolicyLoopbackAndDNSRebinding(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	// Start real loopback TCP & UDP servers.
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	defer tcpLn.Close()
	tcpPort := tcpLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := tcpLn.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, "LOOPBACK_PONG\n")
			_ = c.Close()
		}
	}()

	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("udp listen: %v", err)
	}
	defer udpConn.Close()
	udpPort := udpConn.LocalAddr().(*net.UDPAddr).Port

	// 1. DefaultJailConfig blocks TCP and UDP even to loopback.
	strictJail, err := NewJail(DefaultJailConfig(ws))
	if err != nil {
		t.Fatal(err)
	}
	strictSess, err := NewSession(WithWorkDir(ws), WithJail(strictJail))
	if err != nil {
		t.Fatal(err)
	}
	defer strictSess.Close()

	resTCPDeny, _ := strictSess.Exec(ctx, fmt.Sprintf(`nc -z -w 1 127.0.0.1 %d`, tcpPort))
	if resTCPDeny.ExitCode == 0 || len(resTCPDeny.Denials) == 0 || resTCPDeny.Denials[0].Op != OpNetConnect {
		t.Fatalf("expected default jail to block TCP loopback, got %+v", resTCPDeny)
	}
	resUDPDeny, _ := strictSess.Exec(ctx, fmt.Sprintf(`nc -u -z -w 1 127.0.0.1 %d`, udpPort))
	if resUDPDeny.ExitCode == 0 || len(resUDPDeny.Denials) == 0 || resUDPDeny.Denials[0].Op != OpNetConnect {
		t.Fatalf("expected default jail to block UDP loopback, got %+v", resUDPDeny)
	}

	// 2. AllowLoopback: true permits loopback TCP and UDP while still blocking public/LAN IPs.
	loopCfg := DefaultJailConfig(ws)
	loopCfg.Net.AllowLoopback = true
	loopJail, err := NewJail(loopCfg)
	if err != nil {
		t.Fatal(err)
	}
	loopSess, err := NewSession(WithWorkDir(ws), WithJail(loopJail))
	if err != nil {
		t.Fatal(err)
	}
	defer loopSess.Close()

	resTCPOK, err := loopSess.Exec(ctx, fmt.Sprintf(`nc -w 2 127.0.0.1 %d`, tcpPort))
	if err != nil || resTCPOK.ExitCode != 0 || strings.TrimSpace(resTCPOK.Stdout) != "LOOPBACK_PONG" {
		t.Fatalf("expected AllowLoopback TCP to succeed, got err=%v res=%+v", err, resTCPOK)
	}
	resUDPOK, err := loopSess.Exec(ctx, fmt.Sprintf(`nc -u -z -w 2 127.0.0.1 %d`, udpPort))
	if err != nil || resUDPOK.ExitCode != 0 {
		t.Fatalf("expected AllowLoopback UDP -z to succeed, got err=%v res=%+v", err, resUDPOK)
	}

	// 3. DNS-rebinding defense: allowed hostname resolving to loopback, RFC1918, or link-local
	//    MUST be blocked before TransportDialer is ever called!
	var dialAttempts atomic.Int32
	var mu sync.Mutex
	resolvedIPStr := "127.0.0.1"

	rebindCfg := DefaultJailConfig(ws)
	rebindCfg.Net = NetPolicy{
		Default:         DecisionDeny,
		AllowLoopback:   false,
		BlockPrivateLAN: true,
		AllowListen:     DecisionDeny,
		Rules: []NetRule{
			{
				HostPattern: "api.trusted.example",
				Ports:       []int{443},
				Decision:    DecisionAllow,
			},
		},
		LookupIPAddr: func(ctx context.Context, host string) ([]net.IPAddr, error) {
			mu.Lock()
			ip := net.ParseIP(resolvedIPStr)
			mu.Unlock()
			return []net.IPAddr{{IP: ip}}, nil
		},
		TransportDialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialAttempts.Add(1)
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		},
	}
	rebindJail, err := NewJail(rebindCfg)
	if err != nil {
		t.Fatal(err)
	}
	rebindSess, err := NewSession(WithWorkDir(ws), WithJail(rebindJail))
	if err != nil {
		t.Fatal(err)
	}
	defer rebindSess.Close()

	for _, badIP := range []string{
		"127.0.0.1",
		"::1",
		"10.24.0.5",
		"172.16.10.2",
		"192.168.1.10",
		"169.254.169.254",
		"fe80::1",
	} {
		mu.Lock()
		resolvedIPStr = badIP
		mu.Unlock()
		res, _ := rebindSess.Exec(ctx, `nc -z -w 1 api.trusted.example 443`)
		if res.ExitCode == 0 {
			t.Fatalf("DNS rebinding to %s was NOT blocked!", badIP)
		}
	}
	if dialAttempts.Load() != 0 {
		t.Fatalf("TransportDialer should never have been called for rebound private/loopback IPs (got %d)", dialAttempts.Load())
	}

	// When DNS resolves to a real public IP, the allowlisted host:port succeeds!
	mu.Lock()
	resolvedIPStr = "93.184.216.34"
	mu.Unlock()
	resPubNet, err := rebindSess.Exec(ctx, `nc -z -w 2 api.trusted.example 443`)
	if err != nil || resPubNet.ExitCode != 0 || dialAttempts.Load() != 1 {
		t.Fatalf("expected public IP resolution for allowlisted host to succeed, got err=%v res=%+v dials=%d", err, resPubNet, dialAttempts.Load())
	}
}

func TestIntegrationExecProcAndScrubEnvIsolation(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	// Set host secrets that MUST NEVER leak when scrubbed or InheritEnv=false.
	t.Setenv("UNISH_SECRET_API_KEY", "sk-live-999999")
	t.Setenv("CLOUD_ACCESS_TOKEN", "tok-ultra-secret")
	t.Setenv("PUBLIC_SAFE_VAR", "safe-visible")

	// 1. ExecBuiltinsOnly returns rc=126 on external binary execution, while builtins work.
	builtinsCfg := DefaultJailConfig(ws)
	builtinsCfg.Exec = ExecPolicy{Mode: ExecBuiltinsOnly}
	builtinsJail, err := NewJail(builtinsCfg)
	if err != nil {
		t.Fatal(err)
	}
	builtinsSess, err := NewSession(WithWorkDir(ws), WithJail(builtinsJail))
	if err != nil {
		t.Fatal(err)
	}
	defer builtinsSess.Close()

	selfExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	resBlockedExec, _ := builtinsSess.Exec(ctx, fmt.Sprintf(`%q --version`, filepath.ToSlash(selfExe)))
	if resBlockedExec.ExitCode != 126 || len(resBlockedExec.Denials) == 0 || resBlockedExec.Denials[0].Op != OpExec {
		t.Fatalf("expected ExecBuiltinsOnly to fail with rc=126 + OpExec denial, got %+v", resBlockedExec)
	}

	// 2. Writable-root binary spoofing (`./git` and `PATH="$PWD:$PATH" git`) MUST be blocked
	//    even when `"git"` basename is in AllowBinaries!
	spoofCfg := DefaultJailConfig(ws)
	spoofCfg.Exec = ExecPolicy{
		Mode:          ExecAllowlist,
		AllowBinaries: []string{"git"},
	}
	spoofJail, err := NewJail(spoofCfg)
	if err != nil {
		t.Fatal(err)
	}
	spoofSess, err := NewSession(WithWorkDir(ws), WithJail(spoofJail))
	if err != nil {
		t.Fatal(err)
	}
	defer spoofSess.Close()

	if runtime.GOOS == "windows" {
		fakeGit := filepath.Join(ws, "git.bat")
		if err := os.WriteFile(fakeGit, []byte("@echo SPOOFED_GIT\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		fakeGit := filepath.Join(ws, "git")
		if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\necho SPOOFED_GIT\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, spoofCmd := range []string{
		`./git`,
		`PATH="$PWD" git`,
	} {
		resSpoof, _ := spoofSess.Exec(ctx, spoofCmd)
		if resSpoof.ExitCode == 0 || strings.Contains(resSpoof.Stdout, "SPOOFED_GIT") {
			t.Fatalf("writable-root binary spoofing succeeded for %q: %+v", spoofCmd, resSpoof)
		}
	}

	// 3. `kill -9 $$`, `kill -9 0$$`, `kill -9 0`, `kill -9 -1`, and `kill -9 <hostPID>`
	//    MUST NEVER harm the host Go test process!
	for _, killSelfScript := range []string{
		`kill -9 0`,
		`kill -9 -1`,
		fmt.Sprintf(`kill -9 %d`, os.Getpid()),
		`kill -9 $$`,
		`kill -9 0$$`,
	} {
		sKill, err := NewSession(WithWorkDir(ws), WithJail(builtinsJail))
		if err != nil {
			t.Fatal(err)
		}
		resK, _ := sKill.Exec(ctx, killSelfScript)
		_ = sKill.Close()
		if resK.ExitCode == 0 {
			t.Fatalf("expected %q to exit non-zero without harming host process, got %+v", killSelfScript, resK)
		}
	}

	// 4. WithScrubEnv & WithInheritEnv(false) end-to-end across shell, printenv, env, awk, jq, flock, and external child!
	envCfg := DefaultJailConfig(ws)
	envCfg.Exec = ExecPolicy{
		Mode:          ExecAllowlist,
		AllowBinaries: []string{selfExe},
	}
	envJail, err := NewJail(envCfg)
	if err != nil {
		t.Fatal(err)
	}

	scrubSess, err := NewSession(
		WithWorkDir(ws),
		WithJail(envJail),
		WithScrubEnv("UNISH_SECRET_*", "*_TOKEN"),
		WithEnvMap(map[string]string{"EXPLICIT_KEY": "allowed-123"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer scrubSess.Close()

	leakCheckScript := fmt.Sprintf(`
		echo "SHELL:$UNISH_SECRET_API_KEY:$CLOUD_ACCESS_TOKEN:$PUBLIC_SAFE_VAR:$EXPLICIT_KEY"
		printenv
		env
		awk 'BEGIN { for (k in ENVIRON) print "AWK:" k "=" ENVIRON[k] }'
		jq -rn 'env | to_entries[] | "JQENV:\(.key)=\(.value)"'
		jq -rn '$ENV | to_entries[] | "JQDENV:\(.key)=\(.value)"'
		flock env.lock -c 'env'
		UNISH_INTEGRATION_HELPER_MODE=dumpenv %q -test.run=^TestIntegrationHelperProcess$
	`, filepath.ToSlash(selfExe))

	resScrub, err := scrubSess.Exec(ctx, leakCheckScript)
	if err != nil || resScrub.ExitCode != 0 {
		t.Fatalf("scrub env check failed: err=%v res=%+v", err, resScrub)
	}
	if strings.Contains(resScrub.Stdout, "sk-live-999999") || strings.Contains(resScrub.Stdout, "tok-ultra-secret") {
		t.Fatalf("SCRUBBED HOST SECRET LEAKED!\n%s", resScrub.Stdout)
	}
	if !strings.Contains(resScrub.Stdout, "SHELL:::safe-visible:allowed-123") ||
		!strings.Contains(resScrub.Stdout, "AWK:EXPLICIT_KEY=allowed-123") ||
		!strings.Contains(resScrub.Stdout, "JQENV:EXPLICIT_KEY=allowed-123") {
		t.Fatalf("expected non-scrubbed/explicit vars to remain visible:\n%s", resScrub.Stdout)
	}

	cleanEnvSess, err := NewSession(
		WithWorkDir(ws),
		WithJail(envJail),
		WithInheritEnv(false),
		WithEnvMap(map[string]string{"CLEAN_ONLY": "yes"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanEnvSess.Close()

	resClean, err := cleanEnvSess.Exec(ctx, `
		printenv
		awk 'BEGIN { for (k in ENVIRON) print "AWK:" k "=" ENVIRON[k] }'
		jq -rn '$ENV | to_entries[] | "JQ:\(.key)=\(.value)"'
	`)
	if err != nil || resClean.ExitCode != 0 {
		t.Fatalf("clean env check failed: err=%v res=%+v", err, resClean)
	}
	if strings.Contains(resClean.Stdout, "safe-visible") || strings.Contains(resClean.Stdout, "sk-live-999999") {
		t.Fatalf("WithInheritEnv(false) leaked host env:\n%s", resClean.Stdout)
	}
	if !strings.Contains(resClean.Stdout, "CLEAN_ONLY=yes") || !strings.Contains(resClean.Stdout, "JQ:CLEAN_ONLY=yes") {
		t.Fatalf("WithInheritEnv(false) lost explicit env:\n%s", resClean.Stdout)
	}
}

func TestIntegrationHarnessJSONLRPCAndCLISandboxFlags(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	roDir := filepath.Join(base, "ro")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{ws, roDir, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	outsideAsk := filepath.Join(outside, "ask.txt")
	if err := os.WriteFile(outsideAsk, []byte("HARNESS_APPROVED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("DENIED_DOTENV"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roDir, "guide.txt"), []byte("RO_GUIDE"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Launch `--harness` via RunCLI flags over pipes!
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()

	cfg := DefaultJailConfig(ws)
	cfg.ReadOnlyRoots = []string{roDir}
	cfg.OutsideAction = DecisionAsk
	cfg.Rules = []PathRule{{Pattern: "**/.env*", Decision: DecisionDeny}}
	jail, err := NewJail(cfg)
	if err != nil {
		t.Fatal(err)
	}

	srvDone := make(chan error, 1)
	go func() {
		srvDone <- RunHarness(ctx, inR, outW, WithWorkDir(ws), WithJail(jail))
		_ = outW.Close()
	}()

	evCh := make(chan HarnessEvent, 64)
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var ev HarnessEvent
			if err := json.Unmarshal(sc.Bytes(), &ev); err == nil {
				evCh <- ev
			}
		}
		close(evCh)
	}()

	send := func(msg map[string]any) {
		t.Helper()
		b, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	nextEv := func() HarnessEvent {
		t.Helper()
		select {
		case ev, ok := <-evCh:
			if !ok {
				t.Fatal("harness event stream closed unexpectedly")
			}
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for harness event")
			return HarnessEvent{}
		}
	}

	// ping -> pong
	send(map[string]any{"id": "p1", "type": "ping"})
	if ev := nextEv(); ev.Type != "pong" || ev.ID != "p1" {
		t.Fatalf("expected pong, got %+v", ev)
	}

	// set_env -> state, then verify inside exec
	send(map[string]any{
		"id":   "env1",
		"type": "set_env",
		"env":  map[string]string{"HARNESS_TOKEN": "v12-ok", "DROP_ME": "temp"},
	})
	if ev := nextEv(); ev.Type != "state" || ev.ID != "env1" {
		t.Fatalf("expected state from set_env, got %+v", ev)
	}
	send(map[string]any{
		"id":    "env2",
		"type":  "set_env",
		"unset": []string{"DROP_ME"},
	})
	if ev := nextEv(); ev.Type != "state" || ev.ID != "env2" {
		t.Fatalf("expected state from set_env unset, got %+v", ev)
	}

	// exec with ask_permission -> permission_reply(allow_session) + streaming stdout/stderr + result
	askSlash := filepath.ToSlash(outsideAsk)
	send(map[string]any{
		"id":     "ex1",
		"type":   "exec",
		"script": fmt.Sprintf(`printf "err-msg" >&2; printf "%%s:%%s:%%s" "$HARNESS_TOKEN" "${DROP_ME:-gone}" "$(cat %q)"`, askSlash),
	})

	var sawStderr, sawAsk bool
	for {
		ev := nextEv()
		switch ev.Type {
		case "stderr":
			if strings.Contains(ev.Data, "err-msg") {
				sawStderr = true
			}
		case "ask_permission":
			sawAsk = true
			send(map[string]any{
				"type":     "permission_reply",
				"req_id":   ev.ReqID,
				"decision": "allow_session",
			})
		case "stdout":
		case "result":
			if !sawAsk || !sawStderr {
				t.Fatalf("expected both stderr stream and ask_permission before result (ask=%v stderr=%v)", sawAsk, sawStderr)
			}
			if ev.ExitCode == nil || *ev.ExitCode != 0 || ev.Stdout == nil || *ev.Stdout != "v12-ok:gone:HARNESS_APPROVED" {
				t.Fatalf("unexpected ex1 result: %+v", ev)
			}
			goto ex1Done
		default:
			t.Fatalf("unexpected event in ex1: %+v", ev)
		}
	}
ex1Done:

	// mid-flight cancel
	send(map[string]any{"id": "ex-cancel", "type": "exec", "script": `echo READY; sleep 30`})
	for {
		ev := nextEv()
		if ev.Type == "stdout" && strings.Contains(ev.Data, "READY") {
			break
		}
	}
	send(map[string]any{"type": "cancel", "req_id": "ex-cancel"})
	for {
		ev := nextEv()
		if ev.Type == "result" {
			if !ev.Cancelled || ev.ExitCode == nil || *ev.ExitCode != 130 {
				t.Fatalf("expected cancelled result 130, got %+v", ev)
			}
			break
		}
	}

	_ = inW.Close()
	if err := <-srvDone; err != nil {
		t.Fatalf("RunHarness returned error: %v", err)
	}

	// 2. CLI Sandbox flags (`--jail`, `--jail-ro`, `--deny-path`, `--ask-path`,
	//    `--outside-action`, `--net`, `--allow-net`, `--exec-mode`, `--allow-exec`).
	rcAllowed := RunCLI("1.2.0", []string{
		"unish",
		"--jail", ws,
		"--jail-ro", roDir,
		"--deny-path", "**/.env*",
		"--ask-path", filepath.ToSlash(filepath.Join(ws, "confirm.txt")),
		"--outside-action", "deny",
		"--net", "deny",
		"--allow-net", "example.com:443",
		"--exec-mode", "builtins",
		"-c", fmt.Sprintf(`[ "$(cat %q)" = "RO_GUIDE" ] && echo cli-ok > %q`, filepath.ToSlash(filepath.Join(roDir, "guide.txt")), filepath.ToSlash(filepath.Join(ws, "cli_out.txt"))),
	})
	if rcAllowed != 0 {
		t.Fatalf("expected RunCLI jailed in-scope script to exit 0, got %d", rcAllowed)
	}

	// Non-TTY `--ask-path` fails closed (rc != 0) without hanging!
	if err := os.WriteFile(filepath.Join(ws, "confirm.txt"), []byte("ask-me"), 0o644); err != nil {
		t.Fatal(err)
	}
	rcAskFailClosed := RunCLI("1.2.0", []string{
		"unish",
		"--jail", ws,
		"--ask-path", "**/confirm.txt",
		"-c", fmt.Sprintf(`cat %q`, filepath.ToSlash(filepath.Join(ws, "confirm.txt"))),
	})
	if rcAskFailClosed == 0 {
		t.Fatalf("expected non-TTY --ask-path to fail closed")
	}

	rcDenyDotenv := RunCLI("1.2.0", []string{
		"unish",
		"--jail", ws,
		"--deny-path", "**/.env*",
		"-c", fmt.Sprintf(`cat %q`, filepath.ToSlash(filepath.Join(ws, ".env"))),
	})
	if rcDenyDotenv == 0 {
		t.Fatalf("expected --deny-path **/.env* to block cat .env")
	}
}

func TestIntegrationConcurrentMultiTenantSessions(t *testing.T) {
	const tenants = 16
	ctx := context.Background()

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	sharedSecretDir := t.TempDir()
	sharedSecret := filepath.Join(sharedSecretDir, "secret.txt")
	if err := os.WriteFile(sharedSecret, []byte("HOST_MASTER_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretSlash := filepath.ToSlash(sharedSecret)

	tenantDirs := make([]string, tenants)
	for i := 0; i < tenants; i++ {
		d := filepath.Join(t.TempDir(), fmt.Sprintf("tenant-%02d", i))
		if err := os.MkdirAll(filepath.Join(d, "work"), 0o755); err != nil {
			t.Fatal(err)
		}
		tenantDirs[i] = d
	}

	var wg sync.WaitGroup
	errCh := make(chan error, tenants)

	for i := 0; i < tenants; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			myRoot := tenantDirs[i]
			otherRoot := tenantDirs[(i+1)%tenants]

			jail, err := NewJail(DefaultJailConfig(myRoot))
			if err != nil {
				errCh <- fmt.Errorf("tenant %d NewJail: %w", i, err)
				return
			}
			sess, err := NewSession(
				WithWorkDir(myRoot),
				WithJail(jail),
				WithInheritEnv(false),
				WithEnvMap(map[string]string{
					"TENANT_ID": fmt.Sprintf("T%02d", i),
					"HOME":      myRoot,
				}),
			)
			if err != nil {
				errCh <- fmt.Errorf("tenant %d NewSession: %w", i, err)
				return
			}
			defer sess.Close()

			// Step 1: cd, procsubst, background job, text pipeline.
			r1, err := sess.Exec(ctx, `
				cd work
				sleep 0.05 &
				JOUT=$(jobs)
				wait
				cat <(printf "%s:%s" "$TENANT_ID" "$(basename "$PWD")") > stamp.txt
				jq -rn --rawfile s stamp.txt '$s'
			`)
			if err != nil || r1.ExitCode != 0 {
				errCh <- fmt.Errorf("tenant %d step 1 failed: err=%v res=%+v", i, err, r1)
				return
			}
			want := fmt.Sprintf("T%02d:work", i)
			if strings.TrimSpace(r1.Stdout) != want {
				errCh <- fmt.Errorf("tenant %d stdout=%q, want %q", i, r1.Stdout, want)
				return
			}

			// Step 2: attempt to read neighbor tenant's directory and shared secret -> MUST fail!
			r2, _ := sess.Exec(ctx, fmt.Sprintf(`cat %q 2>/dev/null || ls %q 2>/dev/null`, secretSlash, filepath.ToSlash(otherRoot)))
			if r2.ExitCode == 0 || strings.Contains(r2.Stdout, "HOST_MASTER_SECRET") {
				errCh <- fmt.Errorf("tenant %d escaped jail: %+v", i, r2)
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
	if err != nil {
		t.Fatal(err)
	}
	if afterWD != origWD {
		t.Fatalf("host working directory mutated from %q to %q!", origWD, afterWD)
	}
}
