package unish

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// TestSysnetHelperProcess is invoked as a subprocess by env/pgrep/pkill tests.
func TestSysnetHelperProcess(t *testing.T) {
	mode := os.Getenv("UNISH_SYSNET_HELPER_MODE")
	if mode == "" {
		// Also check command-line marker when env -i strips UNISH_SYSNET_HELPER_MODE.
		for _, a := range os.Args {
			if strings.HasPrefix(a, "--unish-helper-mode=") {
				mode = strings.TrimPrefix(a, "--unish-helper-mode=")
				break
			}
		}
	}
	switch mode {
	case "printenv":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
		os.Exit(0)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

func runSysnetScript(t *testing.T, stdin io.Reader, dir, src string) (string, string, error) {
	t.Helper()
	prog, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	var stdout, stderr bytes.Buffer
	opts := []interp.RunnerOption{
		interp.StdIO(stdin, &stdout, &stderr),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(extraHandler),
	}
	if dir != "" {
		opts = append(opts, interp.Dir(dir))
	}
	r, err := interp.New(opts...)
	if err != nil {
		t.Fatalf("interp.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = r.Run(ctx, prog)
	return stdout.String(), stderr.String(), err
}

// 1. nc with nil/closed stdin (`<&-` or StdIO(nil, ...)) must not panic,
// and slow/large responses after stdin EOF must not be truncated at 50ms.
func TestBugSysnetNcNilStdinAndSlowResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	payload := strings.Repeat("RESPONSE-LINE-0123456789ABCDEF\n", 1024) // ~31KB
	srvDone := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvDone <- ""
			return
		}
		defer conn.Close()
		req, _ := io.ReadAll(conn) // blocks until client sends CloseWrite/EOF
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(conn, payload)
		srvDone <- string(req)
	}()

	// First verify piped request + >50ms delayed response arrives completely.
	script := fmt.Sprintf("printf 'GET / HTTP/1.0\\r\\n\\r\\n' | nc 127.0.0.1 %d", port)
	stdout, stderr, err := runSysnetScript(t, nil, "", script)
	if err != nil {
		t.Fatalf("nc failed: %v (stderr=%q)", err, stderr)
	}
	if gotReq := <-srvDone; gotReq != "GET / HTTP/1.0\r\n\r\n" {
		t.Errorf("server got request %q, want GET", gotReq)
	}
	if stdout != payload {
		t.Fatalf("nc truncated slow response: got %d bytes, want %d bytes", len(stdout), len(payload))
	}

	// Second verify nil/closed stdin (`<&-`) receives server greeting without panicking.
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "BANNER-OK\n")
	}()
	stdout2, stderr2, err2 := runSysnetScript(t, nil, "", fmt.Sprintf("nc 127.0.0.1 %d <&-", port))
	if err2 != nil {
		t.Fatalf("nc <&- failed: %v (stderr=%q)", err2, stderr2)
	}
	if stdout2 != "BANNER-OK\n" {
		t.Fatalf("nc <&- stdout = %q, want %q", stdout2, "BANNER-OK\n")
	}
}

// 2. nc must not hang forever waiting on open stdin after remote server closes.
func TestBugSysnetNcRemoteCloseUnblocksOpenStdin(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.WriteString(conn, "220 GOODBYE\n")
		_ = conn.Close()
	}()

	pr, pw := io.Pipe()
	defer pw.Close()

	start := time.Now()
	stdout, stderr, err := runSysnetScript(t, pr, "", fmt.Sprintf("nc 127.0.0.1 %d", port))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("nc failed: %v (stderr=%q)", err, stderr)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("nc hung for %v waiting on open stdin after remote close", elapsed)
	}
	if stdout != "220 GOODBYE\n" {
		t.Fatalf("nc stdout = %q, want %q", stdout, "220 GOODBYE\n")
	}
}

// 3. nc -l must unblock and release the listening socket when ctx cancels (`timeout`),
// and must support `nc -l 127.0.0.1 <port>`.
func TestBugSysnetNcListenCancelAndHostPort(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	start := time.Now()
	_, _, err = runSysnetScript(t, nil, "", fmt.Sprintf("timeout 0.2 nc -l 127.0.0.1 %d", port))
	elapsed := time.Since(start)
	if exitCode(err) != 124 {
		t.Fatalf("timeout nc -l exit = %d (%v), want 124", exitCode(err), err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout nc -l hung for %v", elapsed)
	}

	// Port must be immediately reusable (listener must have been closed, not leaked).
	rebind, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("nc -l leaked bound socket on 127.0.0.1:%d after cancellation: %v", port, err)
	}
	_ = rebind.Close()
}

// 4. nc -u -l (UDP listen + client exchange) must work instead of failing with unknown network udp.
func TestBugSysnetNcUDPListenAndClient(t *testing.T) {
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe udp: %v", err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	_ = probe.Close()

	type res struct {
		out, errOut string
		err         error
	}
	srvCh := make(chan res, 1)
	go func() {
		o, e, err := runSysnetScript(t, strings.NewReader("PONG\n"), "", fmt.Sprintf("nc -u -l 127.0.0.1 %d", port))
		srvCh <- res{o, e, err}
	}()

	time.Sleep(100 * time.Millisecond)
	cliOut, cliErr, err := runSysnetScript(t, strings.NewReader("PING\n"), "", fmt.Sprintf("nc -u 127.0.0.1 %d", port))
	if err != nil {
		t.Fatalf("nc -u client failed: %v (stderr=%q)", err, cliErr)
	}
	srv := <-srvCh
	if srv.err != nil {
		t.Fatalf("nc -u -l server failed: %v (stderr=%q)", srv.err, srv.errOut)
	}
	if srv.out != "PING\n" {
		t.Errorf("nc -u -l received %q, want %q", srv.out, "PING\n")
	}
	if cliOut != "PONG\n" {
		t.Errorf("nc -u client received %q, want %q", cliOut, "PONG\n")
	}
}

// 5. ps -o must write to hc.Stdout/hc.Stderr (not os.Stdout/os.Stderr),
// ps aux must reach BSD format, and ps -p must filter PIDs in all modes.
func TestBugSysnetPsOutputAndPidFilter(t *testing.T) {
	myPid := os.Getpid()
	stdout, stderr, err := runSysnetScript(t, nil, "", fmt.Sprintf("ps -p %d -o pid,comm", myPid))
	if err != nil {
		t.Fatalf("ps -o failed: %v (stderr=%q)", err, stderr)
	}
	if !strings.Contains(stdout, strconv.Itoa(myPid)) {
		t.Fatalf("ps -p %d -o pid,comm wrote nothing to captured stdout: %q", myPid, stdout)
	}

	// Unknown -o field must write error to hc.Stderr, not os.Stderr.
	_, badErr, err := runSysnetScript(t, nil, "", "ps -o bogus_field_xyz")
	if err == nil || !strings.Contains(badErr, "unknown field name") {
		t.Fatalf("ps -o bogus_field_xyz expected stderr error, got err=%v stderr=%q", err, badErr)
	}

	// ps aux must emit BSD USER/%CPU/%MEM header (was unreachable dead code).
	auxOut, _, err := runSysnetScript(t, nil, "", "ps aux")
	if err != nil {
		t.Fatalf("ps aux failed: %v", err)
	}
	if !strings.HasPrefix(auxOut, "USER") {
		t.Fatalf("ps aux did not use BSD header, got first line: %q", strings.SplitN(auxOut, "\n", 2)[0])
	}

	// ps -p <nonexistent> without -o must filter instead of dumping all processes.
	pOut, _, err := runSysnetScript(t, nil, "", "ps -p 99999999")
	lines := strings.Split(strings.TrimSpace(pOut), "\n")
	if err == nil && len(lines) > 1 {
		t.Fatalf("ps -p 99999999 ignored -p filter and returned %d lines", len(lines))
	}
}

// 6. env must pass modified/cleared environment to external processes AND nested env,
// and df must fail with exit 1 on nonexistent paths.
func TestBugSysnetEnvExternalAndDfMissing(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	exeSlash := filepath.ToSlash(exe)

	t.Setenv("UNISH_PARENT_SECRET", "should_not_leak")

	// External command under `env -i FOO=bar`: must see FOO=bar and NOT UNISH_PARENT_SECRET.
	cmd := fmt.Sprintf("env -i CUSTOM_VAR=hello_env %q -test.run=^TestSysnetHelperProcess$ -- --unish-helper-mode=printenv", exeSlash)
	stdout, stderr, err := runSysnetScript(t, nil, "", cmd)
	if err != nil {
		t.Fatalf("env -i external failed: %v (stderr=%q)", err, stderr)
	}
	if !strings.Contains(stdout, "CUSTOM_VAR=hello_env") {
		t.Fatalf("external child did not receive CUSTOM_VAR=hello_env; got:\n%s", stdout)
	}
	if strings.Contains(stdout, "UNISH_PARENT_SECRET=") {
		t.Fatalf("env -i leaked UNISH_PARENT_SECRET to external child:\n%s", stdout)
	}

	// Nested `env -i ONLY_THIS=1 env`: inner `env` must NOT resurrect os.Environ().
	nestedOut, _, err := runSysnetScript(t, nil, "", "env -i ONLY_THIS=1 env")
	if err != nil {
		t.Fatalf("nested env -i failed: %v", err)
	}
	if strings.TrimSpace(nestedOut) != "ONLY_THIS=1" {
		t.Fatalf("nested env -i resurrected parent env: got %q, want %q", nestedOut, "ONLY_THIS=1\n")
	}

	// df on nonexistent path must exit non-zero and report error to stderr.
	_, dfErr, err := runSysnetScript(t, nil, "", "df /nonexistent_unish_dir_987654321")
	if exitCode(err) == 0 || dfErr == "" {
		t.Fatalf("df on missing path succeeded with err=%v stderr=%q; want exit 1", err, dfErr)
	}
}

// 7. ss -tln must report dual-stack / IPv6 TCP listeners (`:0` / `[::1]:0`) on Linux & Windows
// with zeroed LISTEN peer endpoints, and Windows/Linux procRSS must obtain positive RSS.
func TestBugSysnetSsIPv6AndProcRSS(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen :0: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	var ln6 net.Listener
	var port6 int
	if l, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		ln6 = l
		defer ln6.Close()
		port6 = ln6.Addr().(*net.TCPAddr).Port
	}

	stdout, stderr, err := runSysnetScript(t, nil, "", "ss -tln")
	if err != nil {
		t.Fatalf("ss -tln failed: %v (stderr=%q)", err, stderr)
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		if !strings.Contains(stdout, fmt.Sprintf(":%d ", port)) && !strings.Contains(stdout, fmt.Sprintf(":%d\n", port)) {
			t.Errorf("ss -tln missed dual-stack listener on port %d; output:\n%s", port, stdout)
		}
		if port6 > 0 && !strings.Contains(stdout, fmt.Sprintf("[::1]:%d", port6)) {
			t.Errorf("ss -tln missed IPv6 [::1]:%d listener; output:\n%s", port6, stdout)
		}
		for _, line := range strings.Split(strings.TrimSpace(stdout), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) >= 5 && fields[0] == "LISTEN" {
				peer := fields[4]
				if peer != "0.0.0.0:0" && peer != "[::]:0" {
					t.Errorf("LISTEN row has non-zero garbage peer %q in line: %s", peer, line)
				}
			}
		}
	}

	procs, err := listProcs()
	if err != nil {
		t.Fatalf("listProcs: %v", err)
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		myPid := os.Getpid()
		for _, p := range procs {
			if p.pid == myPid {
				if p.rssKB <= 0 {
					t.Errorf("self rssKB = %d, want > 0", p.rssKB)
				}
				break
			}
		}
	}
}

// 8. pgrep / pkill live matching, flag splitting recovery (-if, -int, -sigterm),
// signal 0 safety on Windows, invalid signal rejection, and -g nonexistent job guard.
func TestBugSysnetPgrepPkillMatching(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	tmpDir := t.TempDir()
	binName := "unish_target_proc_77"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	helperBin := filepath.Join(tmpDir, binName)
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read test exe: %v", err)
	}
	if err := os.WriteFile(helperBin, data, 0o755); err != nil {
		t.Fatalf("write helper exe: %v", err)
	}

	cmd := exec.Command(helperBin, "-test.run=^TestSysnetHelperProcess$", "--", "--unish-helper-mode=sleep", "/decoy/nginx_fake_arg")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	defer func() {
		_ = cmd.Process.Kill()
	}()
	pidStr := strconv.Itoa(cmd.Process.Pid)

	time.Sleep(60 * time.Millisecond)

	// pgrep -x with regex alternation AND stem name must find the helper process.
	out, stderr, err := runSysnetScript(t, nil, "", "pgrep -x 'unish_target_proc_77|other_nonexistent_proc'")
	if err != nil {
		t.Fatalf("pgrep -x alternation failed: %v (stderr=%q)", err, stderr)
	}
	if !strings.Contains(out, pidStr) {
		t.Errorf("pgrep -x regex alternation did not match pid %s; got %q", pidStr, out)
	}

	// Extra operand (`pgrep pat1 pat2`) must fail with exit status 2, not silently ignore pat2.
	_, _, err = runSysnetScript(t, nil, "", "pgrep unish_target_proc_77 extra_bad_arg")
	if exitCode(err) != 2 {
		t.Errorf("pgrep with extra operand exit = %d, want 2", exitCode(err))
	}

	// pkill with invalid signal must reject before killing.
	_, badSigStderr, err := runSysnetScript(t, nil, "", "pkill -s BOGUS_SIGNAL unish_target_proc_77")
	if exitCode(err) == 0 || !strings.Contains(badSigStderr, "invalid signal") {
		t.Errorf("pkill -s BOGUS_SIGNAL got err=%v stderr=%q, want invalid signal error", err, badSigStderr)
	}

	// pkill -g on nonexistent job must exit 1 without falling through to kill anything.
	_, _, err = runSysnetScript(t, nil, "", "pkill -g 99999")
	if exitCode(err) != 1 {
		t.Errorf("pkill -g 99999 exit = %d, want 1", exitCode(err))
	}

	// pkill -0 must probe existence (exit 0) WITHOUT terminating the process (even on Windows).
	_, pkill0Stderr, err := runSysnetScript(t, nil, "", "pkill -0 unish_target_proc_77")
	if err != nil {
		t.Fatalf("pkill -0 failed: %v (stderr=%q)", err, pkill0Stderr)
	}
	select {
	case werr := <-waitCh:
		t.Fatalf("pkill -0 terminated helper process prematurely (%v)", werr)
	case <-time.After(60 * time.Millisecond):
	}

	// pkill -if (previously mangled by splitAttached("pkill", ...) into "-i" "f" <pat>)
	// must match case-insensitively and terminate ONLY unish_target_proc_77.
	_, pkillStderr, err := runSysnetScript(t, nil, "", "pkill -if UNISH_TARGET_PROC_77")
	if err != nil {
		t.Fatalf("pkill -if UNISH_TARGET_PROC_77 failed: %v (stderr=%q)", err, pkillStderr)
	}
	select {
	case <-waitCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("pkill -if UNISH_TARGET_PROC_77 did not terminate helper process")
	}
}

// 9. Unit & live regression tests for matchProcsFromList and recoverPkillSplitArgs
// (including -stop/-segv/-sys/-abrt/-alrm and rejecting pkill -i pat1 pat2).
func TestBugSysnetMatchProcsAndPkillSplitRecovery(t *testing.T) {
	// Verify splitAttached("pkill", ...) + recoverPkillSplitArgs round-trip.
	cases := []struct {
		raw  []string
		want []string
	}{
		{
			raw:  []string{"pkill", "-if", "my_daemon"},
			want: []string{"pkill", "-if", "my_daemon"},
		},
		{
			raw:  []string{"pkill", "-int", "my_daemon"},
			want: []string{"pkill", "-int", "my_daemon"},
		},
		{
			raw:  []string{"pkill", "-sigterm", "my_daemon"},
			want: []string{"pkill", "-sigterm", "my_daemon"},
		},
		{
			raw:  []string{"pkill", "-stop", "my_daemon"},
			want: []string{"pkill", "-stop", "my_daemon"},
		},
		{
			raw:  []string{"pkill", "-segv", "my_daemon"},
			want: []string{"pkill", "-segv", "my_daemon"},
		},
		{
			raw:  []string{"pkill", "-sys", "my_daemon"},
			want: []string{"pkill", "-sys", "my_daemon"},
		},
		{
			raw:  []string{"pkill", "-abrt", "my_daemon"},
			want: []string{"pkill", "-abrt", "my_daemon"},
		},
		{
			raw:  []string{"pkill", "-alrm", "my_daemon"},
			want: []string{"pkill", "-alrm", "my_daemon"},
		},
		{
			raw:  []string{"pkill", "-s", "stop", "my_daemon"},
			want: []string{"pkill", "-s", "stop", "my_daemon"},
		},
		{
			raw:  []string{"pkill", "-i", "nt"},
			want: []string{"pkill", "-i", "nt"},
		},
		{
			raw:  []string{"pkill", "-i", "pat1", "pat2"},
			want: []string{"pkill", "-i", "pat1", "pat2"},
		},
	}
	for _, tc := range cases {
		mangled := splitAttached("pkill", tc.raw)
		got := recoverPkillSplitArgs(mangled)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("recoverPkillSplitArgs(splitAttached(%v)) = %v (mangled=%v), want %v", tc.raw, got, mangled, tc.want)
		}
	}

	// Live shell verification: -stop/-segv/-sys must parse as valid signals (exit 1 on non-matching pattern, no "invalid signal"),
	// and `pkill -i pat1 pat2` must exit 2 instead of merging into `-ipat1`.
	for _, sigFlag := range []string{"-stop", "-segv", "-sys"} {
		_, stderr, err := runSysnetScript(t, nil, "", fmt.Sprintf("pkill %s nonexistent_unish_target_999888", sigFlag))
		if exitCode(err) != 1 || stderr != "" {
			t.Errorf("pkill %s nonexistent got exit=%d stderr=%q, want exit=1 empty stderr", sigFlag, exitCode(err), stderr)
		}
	}
	_, extraStderr, err := runSysnetScript(t, nil, "", "pkill -i pat1 pat2")
	if exitCode(err) != 2 || !strings.Contains(extraStderr, "only one pattern") {
		t.Errorf("pkill -i pat1 pat2 got exit=%d stderr=%q, want exit=2 'only one pattern'", exitCode(err), extraStderr)
	}

	// Verify command-name extraction never matches slash-bearing arguments unless -f is set.
	procs := []procInfo{
		{pid: 100, comm: "unish", cmd: "/usr/local/bin/unish"},
		{pid: 201, comm: "vim", cmd: "/usr/bin/vim /etc/nginx/nginx.conf"},
		{pid: 202, comm: "", cmd: "/usr/bin/vim /etc/nginx/nginx.conf"},
		{pid: 203, comm: "kthreadd", cmd: "[kthreadd]"},
	}

	// Without -f, "nginx" must NOT match vim editing /etc/nginx/nginx.conf.
	m, err := matchProcsFromList(procs, 100, "nginx", false, false, false)
	if err != nil || len(m) != 0 {
		t.Fatalf("matchProcsFromList('nginx', full=false) = %v, err=%v; want 0 matches", m, err)
	}

	// Without -f, "vim" MUST match both pid 201 and pid 202.
	m, err = matchProcsFromList(procs, 100, "vim", false, false, false)
	if err != nil || len(m) != 2 {
		t.Fatalf("matchProcsFromList('vim', full=false) = %v, err=%v; want 2 matches", m, err)
	}

	// With -f, "nginx.conf" MUST match pid 201 and pid 202.
	m, err = matchProcsFromList(procs, 100, "nginx\\.conf", true, false, false)
	if err != nil || len(m) != 2 {
		t.Fatalf("matchProcsFromList('nginx\\.conf', full=true) = %v, err=%v; want 2 matches", m, err)
	}

	// With -fx, exact full command line matches, whereas bare command name does not.
	m, err = matchProcsFromList(procs, 100, "/usr/bin/vim /etc/nginx/nginx\\.conf", true, false, true)
	if err != nil || len(m) != 2 {
		t.Fatalf("matchProcsFromList(-fx full cmd) = %v, err=%v; want 2 matches", m, err)
	}
	m, err = matchProcsFromList(procs, 100, "vim", true, false, true)
	if err != nil || len(m) != 0 {
		t.Fatalf("matchProcsFromList(-fx 'vim') = %v, err=%v; want 0 matches", m, err)
	}

	// Self PID (100) must always be excluded.
	m, err = matchProcsFromList(procs, 100, "unish", true, false, false)
	if err != nil || len(m) != 0 {
		t.Fatalf("matchProcsFromList matched self pid 100: %v", m)
	}
}

// 10. Unit regression tests for Linux /proc/[pid]/stat, /proc/stat btime, and Darwin kern.procargs2 parsers.
func TestBugSysnetProcStatBtimeAndDarwinArgs(t *testing.T) {
	// 21 fields after "(comm)" (one field short of rss at rest[21]) previously panicked with index out of range [21] with length 21.
	shortFields := make([]string, 21)
	for i := range shortFields {
		shortFields[i] = "0"
	}
	shortFields[0] = "S"
	shortFields[1] = "42"
	shortFields[2] = "84"
	shortStat := "1234 (short_stat) " + strings.Join(shortFields, " ")
	comm, statCh, ppid, pgid, _, _, rssKB, _, ok := parseProcStat(shortStat, 1000, 4096)
	if !ok || comm != "short_stat" || statCh != "S" || ppid != 42 || pgid != 84 || rssKB != -1 {
		t.Fatalf("parseProcStat(len(rest)==21) = (%q, %q, ppid=%d, pgid=%d, rss=%d, ok=%v)", comm, statCh, ppid, pgid, rssKB, ok)
	}

	// Full /proc/[pid]/stat with spaces & nested parens in comm + 64KiB page size (ARM64 Linux).
	fullFields := make([]string, 25)
	for i := range fullFields {
		fullFields[i] = "0"
	}
	fullFields[0] = "R"
	fullFields[1] = "777"
	fullFields[2] = "888"      // pgrp / pgid
	fullFields[11] = "200"     // utime ticks
	fullFields[12] = "100"     // stime ticks
	fullFields[19] = "5000"    // starttime ticks
	fullFields[20] = "1048576" // vsize bytes (1024 KiB)
	fullFields[21] = "10"      // rss pages (10 * 64KiB = 640 KiB)
	fullStat := "5678 (Web (Content) Helper) " + strings.Join(fullFields, " ")
	comm, statCh, ppid, pgid, _, vszKB, rssKB, cpuTime, ok := parseProcStat(fullStat, 100, 65536)
	if !ok || comm != "Web (Content) Helper" || statCh != "R" || ppid != 777 || pgid != 888 || vszKB != 1024 || rssKB != 640 || cpuTime != "00:00:03" {
		t.Fatalf("parseProcStat(64K) = (%q, %q, ppid=%d, pgid=%d, vsz=%d, rss=%d, time=%q, ok=%v)", comm, statCh, ppid, pgid, vszKB, rssKB, cpuTime, ok)
	}

	// Malformed /proc/stat "btime   " (no value field) previously panicked with index out of range [1].
	if _, ok := parseProcBtime("cpu  1 2 3\nbtime   \nprocesses 9\n"); ok {
		t.Fatalf("parseProcBtime on truncated btime line returned ok=true")
	}
	if bt, ok := parseProcBtime("cpu  1 2 3\nbtime 1700000456\n"); !ok || bt != 1700000456 {
		t.Fatalf("parseProcBtime valid = (%d, %v), want (1700000456, true)", bt, ok)
	}

	// Darwin kern.procargs2 binary layout: int32 argc + exec_path\0 + padding\0 + argv[0]\0...argv[argc-1]\0.
	var raw bytes.Buffer
	var argcBuf [4]byte
	binary.LittleEndian.PutUint32(argcBuf[:], 3)
	raw.Write(argcBuf[:])
	raw.WriteString("/Applications/Test.app/Contents/MacOS/very_long_daemon_process_name\x00\x00\x00")
	raw.WriteString("very_long_daemon_process_name\x00--flag\x00/var/db/config.json\x00ENV=1\x00")
	execBase, cmdLine := parseDarwinProcArgs2(raw.Bytes())
	if execBase != "very_long_daemon_process_name" {
		t.Errorf("parseDarwinProcArgs2 execBase = %q, want very_long_daemon_process_name", execBase)
	}
	if cmdLine != "very_long_daemon_process_name --flag /var/db/config.json" {
		t.Errorf("parseDarwinProcArgs2 cmdLine = %q", cmdLine)
	}
}

// 11. Unit regression test for Linux /proc/net/{tcp,udp}{,6} hex endpoint decoding.
func TestBugSysnetProcNetIPv6Endian(t *testing.T) {
	cases := []struct {
		hex    string
		le     bool
		expect string
	}{
		// Little-endian x86_64 / arm64 Linux kernel /proc/net/tcp6 & tcp entries:
		{"00000000000000000000000001000000:1F90", true, "[::1]:8080"},
		{"000080FE000000000000000001000000:01BB", true, "[fe80::1]:443"},
		{"0000000000000000FFFF00000100007F:2328", true, "[::ffff:127.0.0.1]:9000"},
		{"0100007F:1F90", true, "127.0.0.1:8080"},
		// Big-endian s390x / ppc64 Linux kernel entries:
		{"00000000000000000000000000000001:0050", false, "[::1]:80"},
		{"7F000001:0050", false, "127.0.0.1:80"},
		// Malformed inputs must not panic:
		{"MALFORMED", true, "MALFORMED"},
		{"ZZZZZZZZ:0050", true, "ZZZZZZZZ:0050"},
	}
	for _, tc := range cases {
		if got := parseProcNetAddrEndian(tc.hex, tc.le); got != tc.expect {
			t.Errorf("parseProcNetAddrEndian(%q, le=%v) = %q, want %q", tc.hex, tc.le, got, tc.expect)
		}
	}
}

// 12. nice must not mutate the unish shell process's own priority, and stat must use Lstat on symlinks.
func TestBugSysnetNiceIsolationAndStatSymlink(t *testing.T) {
	before := currentNice()
	out, stderr, err := runSysnetScript(t, nil, "", "nice -n 5 echo nice_child_ok")
	if err != nil {
		t.Fatalf("nice failed: %v (stderr=%q)", err, stderr)
	}
	if out != "nice_child_ok\n" {
		t.Errorf("nice output = %q, want %q", out, "nice_child_ok\n")
	}
	if after := currentNice(); after != before {
		t.Fatalf("cmdNice mutated parent shell process priority from %d to %d", before, after)
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write real.txt: %v", err)
	}
	validLink := filepath.Join(dir, "valid_link")
	danglingLink := filepath.Join(dir, "dangling_link")
	if err := os.Symlink(target, validLink); err == nil {
		_ = os.Symlink(filepath.Join(dir, "missing_target"), danglingLink)
		stOut, stErr, err := runSysnetScript(t, nil, dir, "stat -c '%F %N' valid_link dangling_link")
		if err != nil {
			t.Fatalf("stat on symlinks failed: %v (stderr=%q)", err, stErr)
		}
		if strings.Count(stOut, "symbolic link") != 2 {
			t.Errorf("stat did not identify both valid and dangling symlinks: %q", stOut)
		}
	}
}

// 13. pkill -g OS PGID matching (including -g 0 caller PGID + pattern filter as in parity/sandbox.py:68).
func TestBugSysnetPkillPgidMatching(t *testing.T) {
	// Unit tests for parsePkillPgids and filterProcsByPgid + matchProcsFromList.
	want0, err := parsePkillPgids("0", 4321)
	if err != nil || !want0[4321] || len(want0) != 1 {
		t.Fatalf("parsePkillPgids('0', 4321) = (%v, %v), want {4321:true}", want0, err)
	}
	wantList, err := parsePkillPgids("100,200", 4321)
	if err != nil || !wantList[100] || !wantList[200] || len(wantList) != 2 {
		t.Fatalf("parsePkillPgids('100,200', 4321) = (%v, %v)", wantList, err)
	}
	for _, bad := range []string{"", "abc", "-5", "100,,200"} {
		if _, err := parsePkillPgids(bad, 4321); err == nil {
			t.Errorf("parsePkillPgids(%q) succeeded, want error", bad)
		}
	}

	procs := []procInfo{
		{pid: 500, pgid: 4321, comm: "unish", cmd: "/tmp/unish -c pkill -g 0 -f 'sleep 30'"},
		{pid: 501, pgid: 4321, comm: "sleep", cmd: "sleep 30"},
		{pid: 502, pgid: 9999, comm: "sleep", cmd: "sleep 30"},
		{pid: 503, pgid: 4321, comm: "sh", cmd: "sh"},
	}
	pgFiltered := filterProcsByPgid(procs, 500, want0, 0)
	if len(pgFiltered) != 2 || pgFiltered[0].pid != 501 || pgFiltered[1].pid != 503 {
		t.Fatalf("filterProcsByPgid = %v, want pids [501, 503]", pgFiltered)
	}
	matched, err := matchProcsFromList(pgFiltered, 500, "sleep 30", true, false, false)
	if err != nil || len(matched) != 1 || matched[0].pid != 501 {
		t.Fatalf("pkill -g 0 -f 'sleep 30' simulation matched %v (err=%v), want [pid 501]", matched, err)
	}

	// Invalid -g argument must fail with exit status 2.
	_, badStderr, err := runSysnetScript(t, nil, "", "pkill -g not_a_pgid")
	if exitCode(err) != 2 || !strings.Contains(badStderr, "invalid process group") {
		t.Errorf("pkill -g not_a_pgid got exit=%d stderr=%q, want exit=2", exitCode(err), badStderr)
	}

	// On Unix, live-test `pkill -g 0 -f ...` against a child in our own OS process group.
	if runtime.GOOS != "windows" && selfPgid() > 0 {
		exe, err := os.Executable()
		if err != nil {
			t.Fatalf("os.Executable: %v", err)
		}
		helperBin := filepath.Join(t.TempDir(), "unish_pgid_victim_88")
		data, err := os.ReadFile(exe)
		if err != nil {
			t.Fatalf("read test exe: %v", err)
		}
		if err := os.WriteFile(helperBin, data, 0o755); err != nil {
			t.Fatalf("write helper: %v", err)
		}
		cmd := exec.Command(helperBin, "-test.run=^TestSysnetHelperProcess$", "--", "--unish-helper-mode=sleep")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper: %v", err)
		}
		waitCh := make(chan error, 1)
		go func() { waitCh <- cmd.Wait() }()
		defer func() { _ = cmd.Process.Kill() }()

		time.Sleep(60 * time.Millisecond)
		_, stderr, err := runSysnetScript(t, nil, "", "pkill -g 0 -f unish_pgid_victim_88")
		if err != nil {
			t.Fatalf("pkill -g 0 -f unish_pgid_victim_88 failed: %v (stderr=%q)", err, stderr)
		}
		select {
		case <-waitCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("pkill -g 0 -f did not terminate same-PGID child")
		}
	}
}

// 14. env -u PATH must NOT resurrect os.Getenv("PATH"), and Windows Path= must match case-insensitively.
func TestBugSysnetEnvUnsetPathAndCaseFolding(t *testing.T) {
	// Cross-OS unit test of case-insensitive Path= set/unset (Windows semantics).
	winEnv := []string{"SystemRoot=C:\\Windows", "Path=C:\\Windows\\System32"}
	replaced := setEnvVarCase(append([]string(nil), winEnv...), "PATH", "C:\\Custom\\bin", true)
	if !reflect.DeepEqual(replaced, []string{"SystemRoot=C:\\Windows", "PATH=C:\\Custom\\bin"}) {
		t.Fatalf("setEnvVarCase(foldCase=true) = %v, want single PATH entry", replaced)
	}
	removed := unsetEnvVarCase(append([]string(nil), winEnv...), "PATH", true)
	if !reflect.DeepEqual(removed, []string{"SystemRoot=C:\\Windows"}) {
		t.Fatalf("unsetEnvVarCase(foldCase=true) = %v, want Path removed", removed)
	}

	// Build a custom executable placed ONLY in a temporary directory on PATH.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	customDir := t.TempDir()
	binBase := "unish_path_probe_bin_91"
	binFile := binBase
	if runtime.GOOS == "windows" {
		binFile += ".exe"
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read test exe: %v", err)
	}
	if err := os.WriteFile(filepath.Join(customDir, binFile), data, 0o755); err != nil {
		t.Fatalf("write custom bin: %v", err)
	}

	t.Setenv("PATH", customDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// 1) With inherited PATH (which on Windows is stored as `Path=` in os.Environ()),
	// `env EXTRA_FLAG=1 unish_path_probe_bin_91 ...` MUST find and run the binary.
	out, stderr, err := runSysnetScript(t, nil, "", fmt.Sprintf("env EXTRA_FLAG=ok %s -test.run=^TestSysnetHelperProcess$ -- --unish-helper-mode=printenv", binBase))
	if err != nil {
		t.Fatalf("env with inherited PATH failed to locate %s: %v (stderr=%q)", binBase, err, stderr)
	}
	if !strings.Contains(out, "EXTRA_FLAG=ok") {
		t.Fatalf("expected EXTRA_FLAG=ok in output, got:\n%s", out)
	}

	// 2) With `env -u PATH` (without `-i`), `findSubcommandExec` must NOT fall back to
	// `os.Getenv("PATH")`, so `unish_path_probe_bin_91` must NOT be found (exit 127).
	_, unsetStderr, err := runSysnetScript(t, nil, "", fmt.Sprintf("env -u PATH %s -test.run=^TestSysnetHelperProcess$ -- --unish-helper-mode=printenv", binBase))
	if exitCode(err) != 127 {
		t.Fatalf("env -u PATH %s exited %d (err=%v, stderr=%q), want 127", binBase, exitCode(err), err, unsetStderr)
	}

	// 3) `env -u PATH env` must emit zero PATH/Path entries, and `env PATH=<customDir> env`
	// must emit exactly one PATH entry (never duplicate Path= + PATH= on Windows).
	unsetEnvOut, _, err := runSysnetScript(t, nil, "", "env -u PATH env")
	if err != nil {
		t.Fatalf("env -u PATH env failed: %v", err)
	}
	for _, line := range strings.Split(unsetEnvOut, "\n") {
		if envKeyMatch(line, "PATH") {
			t.Fatalf("env -u PATH env still contained PATH entry: %q", line)
		}
	}

	overrideOut, _, err := runSysnetScript(t, nil, "", fmt.Sprintf("env PATH=%q env", filepath.ToSlash(customDir)))
	if err != nil {
		t.Fatalf("env PATH=... env failed: %v", err)
	}
	pathCount := 0
	for _, line := range strings.Split(overrideOut, "\n") {
		if envKeyMatch(line, "PATH") {
			pathCount++
		}
	}
	if pathCount != 1 {
		t.Fatalf("env PATH=... env had %d PATH/Path entries, want 1", pathCount)
	}
}
