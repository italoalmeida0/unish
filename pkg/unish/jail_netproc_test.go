package unish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// TestNetprocHelperProcess acts as a lightweight cross-platform external binary
// when invoked with UNISH_NETPROC_HELPER=1.
func TestNetprocHelperProcess(t *testing.T) {
	if os.Getenv("UNISH_NETPROC_HELPER") != "1" {
		return
	}
	var args []string
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	mode := "echo"
	if len(args) > 0 {
		mode = args[0]
	}
	switch mode {
	case "echo":
		fmt.Printf("HELPER_OK:%s\n", strings.Join(args[1:], ","))
		os.Exit(0)
	case "sleep":
		if len(args) > 1 && args[1] != "" {
			_ = os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		time.Sleep(30 * time.Second)
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

type netprocScriptResult struct {
	stdout string
	stderr string
	code   int
	err    error
}

func runNetprocScript(t *testing.T, ctx context.Context, dir string, envPairs []string, stdin io.Reader, stdoutOverride io.Writer, src string) netprocScriptResult {
	t.Helper()
	prog, err := syntax.NewParser().Parse(strings.NewReader(src), "test.sh")
	if err != nil {
		t.Fatalf("parse script: %v\nsrc:\n%s", err, src)
	}
	var outBuf, errBuf bytes.Buffer
	outW := io.Writer(&outBuf)
	if stdoutOverride != nil {
		outW = stdoutOverride
	}
	opts := []interp.RunnerOption{
		interp.StdIO(stdin, outW, &errBuf),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	}
	if dir != "" {
		opts = append(opts, interp.Dir(dir))
	}
	if len(envPairs) > 0 {
		opts = append(opts, interp.Env(expand.ListEnviron(envPairs...)))
	}
	r, err := interp.New(opts...)
	if err != nil {
		t.Fatalf("interp.New: %v", err)
	}
	runErr := r.Run(ctx, prog)
	code := 0
	if runErr != nil {
		var es interp.ExitStatus
		if errors.As(runErr, &es) {
			code = int(es)
		} else {
			code = 1
		}
	}
	return netprocScriptResult{
		stdout: strings.ReplaceAll(outBuf.String(), "\r\n", "\n"),
		stderr: strings.ReplaceAll(errBuf.String(), "\r\n", "\n"),
		code:   code,
		err:    runErr,
	}
}

type customNetprocHook struct {
	mu          sync.Mutex
	requests    []AccessRequest
	netCalls    []string
	checkFn     func(ctx context.Context, req AccessRequest) error
	dialFn      func(ctx context.Context, cmdName, network, address string) (net.Conn, error)
	listenFn    func(ctx context.Context, cmdName, network, address string) (net.Listener, error)
	listenPktFn func(ctx context.Context, cmdName, network, address string) (net.PacketConn, error)
}

func (h *customNetprocHook) CheckAccess(ctx context.Context, req AccessRequest) error {
	h.mu.Lock()
	h.requests = append(h.requests, req)
	fn := h.checkFn
	h.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &AccessDenialError{Op: req.Op, Target: req.Path, Reason: "denied by default"}
}

func (h *customNetprocHook) DialContext(ctx context.Context, cmdName, network, address string) (net.Conn, error) {
	h.mu.Lock()
	h.netCalls = append(h.netCalls, fmt.Sprintf("dial:%s:%s:%s", cmdName, network, address))
	fn := h.dialFn
	h.mu.Unlock()
	if fn != nil {
		return fn(ctx, cmdName, network, address)
	}
	return nil, &AccessDenialError{Op: OpNetConnect, Target: address, Reason: "network dial denied"}
}

func (h *customNetprocHook) Listen(ctx context.Context, cmdName, network, address string) (net.Listener, error) {
	h.mu.Lock()
	h.netCalls = append(h.netCalls, fmt.Sprintf("listen:%s:%s:%s", cmdName, network, address))
	fn := h.listenFn
	h.mu.Unlock()
	if fn != nil {
		return fn(ctx, cmdName, network, address)
	}
	return nil, &AccessDenialError{Op: OpNetListen, Target: address, Reason: "network listen denied"}
}

func (h *customNetprocHook) ListenPacket(ctx context.Context, cmdName, network, address string) (net.PacketConn, error) {
	h.mu.Lock()
	h.netCalls = append(h.netCalls, fmt.Sprintf("listenPacket:%s:%s:%s", cmdName, network, address))
	fn := h.listenPktFn
	h.mu.Unlock()
	if fn != nil {
		return fn(ctx, cmdName, network, address)
	}
	return nil, &AccessDenialError{Op: OpNetListen, Target: address, Reason: "udp listen denied"}
}

func (h *customNetprocHook) snapshotRequests() []AccessRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]AccessRequest(nil), h.requests...)
}

func (h *customNetprocHook) snapshotNetCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.netCalls...)
}

// 1. Network Jail: outbound TCP/UDP/scan and inbound TCP/UDP listeners denied with clean stderr.
func TestJailNetcat_BlockedOutboundAndInbound(t *testing.T) {
	hook := &customNetprocHook{}
	ctx := WithCapabilityHook(context.Background(), hook)

	cases := []struct {
		name     string
		script   string
		wantCall string
	}{
		{"tcp_client", "nc 127.0.0.1 19991", "dial:nc:tcp:127.0.0.1:19991"},
		{"tcp_scan", "nc -z 127.0.0.1 19992", "dial:nc:tcp:127.0.0.1:19992"},
		{"udp_client", "nc -u 127.0.0.1 19993", "dial:nc:udp:127.0.0.1:19993"},
		{"netcat_alias", "netcat 10.0.0.8 8080", "dial:nc:tcp:10.0.0.8:8080"},
		{"tcp_listen", "nc -l 19994", "listen:nc:tcp::19994"},
		{"tcp_listen_host", "nc -l 127.0.0.1 19995", "listen:nc:tcp:127.0.0.1:19995"},
		{"udp_listen", "nc -u -l 19996", "listenPacket:nc:udp::19996"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runNetprocScript(t, ctx, t.TempDir(), nil, nil, nil, tc.script)
			if res.code != 1 {
				t.Fatalf("%s: code = %d, want 1 (stderr=%q)", tc.script, res.code, res.stderr)
			}
			if !strings.Contains(strings.ToLower(res.stderr), "permission denied") {
				t.Fatalf("%s: expected clean permission denied error on stderr even without -v, got %q", tc.script, res.stderr)
			}
			calls := hook.snapshotNetCalls()
			found := false
			for _, c := range calls {
				if c == tc.wantCall {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s: hook calls %v missing %q", tc.script, calls, tc.wantCall)
			}
		})
	}
}

type pipeListener struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{
		ch:     make(chan net.Conn, 1),
		closed: make(chan struct{}),
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr {
	return pipeAddr("virtual:8443")
}

type pipeAddr string

func (a pipeAddr) Network() string { return "virtual" }
func (a pipeAddr) String() string  { return string(a) }

type virtualPacketConn struct {
	inCh   chan []byte
	closed chan struct{}
	once   sync.Once
}

func newVirtualPacketConn() *virtualPacketConn {
	return &virtualPacketConn{
		inCh:   make(chan []byte, 4),
		closed: make(chan struct{}),
	}
}

func (p *virtualPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case msg := <-p.inCh:
		n := copy(b, msg)
		return n, pipeAddr("peer:9000"), nil
	case <-p.closed:
		return 0, nil, net.ErrClosed
	}
}

func (p *virtualPacketConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	return len(b), nil
}

func (p *virtualPacketConn) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

func (p *virtualPacketConn) LocalAddr() net.Addr                { return pipeAddr("virtual-udp:8444") }
func (p *virtualPacketConn) SetDeadline(_ time.Time) error      { return nil }
func (p *virtualPacketConn) SetReadDeadline(_ time.Time) error  { return nil }
func (p *virtualPacketConn) SetWriteDeadline(_ time.Time) error { return nil }

// 2. Virtual Network via CapabilityHook + net.Pipe() with zero OS sockets.
func TestJailNetcat_VirtualNetPipeClientAndListener(t *testing.T) {
	// Part A: Outbound `nc` routed over in-memory net.Pipe().
	hook := &customNetprocHook{
		dialFn: func(_ context.Context, cmdName, network, address string) (net.Conn, error) {
			if cmdName != "nc" || network != "tcp" || address != "virtual.mesh:443" {
				return nil, &AccessDenialError{Op: OpNetConnect, Target: address}
			}
			clientConn, serverConn := net.Pipe()
			go func() {
				defer serverConn.Close()
				buf := make([]byte, 256)
				n, _ := serverConn.Read(buf)
				_, _ = serverConn.Write([]byte("ACK:" + string(buf[:n])))
			}()
			return clientConn, nil
		},
	}
	ctx := WithCapabilityHook(context.Background(), hook)
	res := runNetprocScript(t, ctx, t.TempDir(), nil, strings.NewReader("hello-pipe\n"), nil, "nc virtual.mesh 443")
	if res.code != 0 || res.stdout != "ACK:hello-pipe\n" {
		t.Fatalf("virtual nc client: code=%d stdout=%q stderr=%q", res.code, res.stdout, res.stderr)
	}

	// Part B: Inbound `nc -l` routed over in-memory PipeListener.
	pln := newPipeListener()
	clientSide, serverSide := net.Pipe()
	var peerReceived bytes.Buffer
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		_, _ = clientSide.Write([]byte("from-virtual-peer\n"))
		buf := make([]byte, 256)
		n, _ := clientSide.Read(buf)
		if n > 0 {
			peerReceived.Write(buf[:n])
		}
		_ = clientSide.Close()
	}()
	pln.ch <- serverSide

	listenHook := &customNetprocHook{
		listenFn: func(_ context.Context, cmdName, network, address string) (net.Listener, error) {
			if cmdName == "nc" && network == "tcp" && address == ":8443" {
				return pln, nil
			}
			return nil, &AccessDenialError{Op: OpNetListen, Target: address}
		},
	}
	lctx := WithCapabilityHook(context.Background(), listenHook)
	lres := runNetprocScript(t, lctx, t.TempDir(), nil, strings.NewReader("reply-from-listener\n"), nil, "nc -l 8443")
	<-peerDone
	if lres.code != 0 || lres.stdout != "from-virtual-peer\n" {
		t.Fatalf("virtual nc -l: code=%d stdout=%q stderr=%q", lres.code, lres.stdout, lres.stderr)
	}
	if peerReceived.String() != "reply-from-listener\n" {
		t.Fatalf("virtual nc -l peer got %q, want %q", peerReceived.String(), "reply-from-listener\n")
	}

	// Part C: Inbound `nc -u -l` routed over in-memory PacketConn.
	vpc := newVirtualPacketConn()
	vpc.inCh <- []byte("virtual-udp-payload\n")
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = vpc.Close()
	}()
	udpHook := &customNetprocHook{
		listenPktFn: func(_ context.Context, cmdName, network, address string) (net.PacketConn, error) {
			if cmdName == "nc" && network == "udp" && address == ":8444" {
				return vpc, nil
			}
			return nil, &AccessDenialError{Op: OpNetListen, Target: address}
		},
	}
	uctx := WithCapabilityHook(context.Background(), udpHook)
	ures := runNetprocScript(t, uctx, t.TempDir(), nil, nil, nil, "nc -u -l -w 1 8444")
	if ures.code != 0 || ures.stdout != "virtual-udp-payload\n" {
		t.Fatalf("virtual nc -u -l: code=%d stdout=%q stderr=%q", ures.code, ures.stdout, ures.stderr)
	}

	// Part D: Policy allowing 127.0.0.1 loopback while blocking external hosts.
	realLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen loopback: %v", err)
	}
	defer realLn.Close()
	_, portStr, _ := net.SplitHostPort(realLn.Addr().String())
	go func() {
		c, aerr := realLn.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("loopback-ok\n"))
	}()

	loopbackHook := &customNetprocHook{
		dialFn: func(dctx context.Context, _, network, address string) (net.Conn, error) {
			h, _, _ := net.SplitHostPort(address)
			if h == "127.0.0.1" {
				var d net.Dialer
				return d.DialContext(dctx, network, address)
			}
			return nil, &AccessDenialError{Op: OpNetConnect, Target: address, Reason: "only loopback allowed"}
		},
	}
	lbCtx := WithCapabilityHook(context.Background(), loopbackHook)
	okLB := runNetprocScript(t, lbCtx, t.TempDir(), nil, nil, nil, fmt.Sprintf("nc -w 2 127.0.0.1 %s", portStr))
	if okLB.code != 0 || okLB.stdout != "loopback-ok\n" {
		t.Fatalf("allowed loopback nc: code=%d stdout=%q stderr=%q", okLB.code, okLB.stdout, okLB.stderr)
	}
	denyExt := runNetprocScript(t, lbCtx, t.TempDir(), nil, nil, nil, fmt.Sprintf("nc 10.255.255.1 %s", portStr))
	if denyExt.code != 1 || !strings.Contains(strings.ToLower(denyExt.stderr), "permission denied") {
		t.Fatalf("blocked non-loopback nc: code=%d stderr=%q", denyExt.code, denyExt.stderr)
	}
}

// 3. Self-Signal Safety: `kill $$`, `kill -9 $$`, `kill -TERM $$`, process-group PIDs
// MUST NEVER kill the host Go test process in library or jailed mode!
func TestJailSelfSignal_NeverKillsHostProcess(t *testing.T) {
	hook := &customNetprocHook{
		checkFn: func(_ context.Context, _ AccessRequest) error {
			// Even if a permissive hook allows OpProc, host PID & group <=0 must never kill host!
			return nil
		},
	}

	for _, mode := range []struct {
		name string
		ctx  context.Context
	}{
		{"embedded_library", context.Background()},
		{"jailed_hook", WithCapabilityHook(context.Background(), hook)},
	} {
		t.Run(mode.name, func(t *testing.T) {
			// 1) Untrapped `kill -9 $$` halts script with 137 without killing host Go process.
			r1 := runNetprocScript(t, mode.ctx, t.TempDir(), nil, nil, nil, "kill -9 $$; echo MUST_NOT_RUN")
			if r1.code != 137 || strings.Contains(r1.stdout, "MUST_NOT_RUN") {
				t.Fatalf("kill -9 $$: code=%d stdout=%q stderr=%q", r1.code, r1.stdout, r1.stderr)
			}

			// 2) Untrapped `kill -TERM $$` (and bare `kill $$`) halts script with 143 without killing host.
			r2 := runNetprocScript(t, mode.ctx, t.TempDir(), nil, nil, nil, "kill $$; echo MUST_NOT_RUN")
			if r2.code != 143 || strings.Contains(r2.stdout, "MUST_NOT_RUN") {
				t.Fatalf("kill $$: code=%d stdout=%q stderr=%q", r2.code, r2.stdout, r2.stderr)
			}

			// 3) Untrapped `kill -USR1 $$` halts script with 128+SIGUSR1 without killing host.
			rUsr := runNetprocScript(t, mode.ctx, t.TempDir(), nil, nil, nil, "kill -USR1 $$; echo MUST_NOT_RUN")
			if rUsr.code <= 128 || strings.Contains(rUsr.stdout, "MUST_NOT_RUN") {
				t.Fatalf("kill -USR1 $$: code=%d stdout=%q stderr=%q", rUsr.code, rUsr.stdout, rUsr.stderr)
			}

			// 4) Subshell `( kill -9 $$; echo SUB_DEAD ); echo "after:$?"` exits only subshell!
			rSub := runNetprocScript(t, mode.ctx, t.TempDir(), nil, nil, nil, "( kill -9 $$; echo SUB_DEAD ); echo \"after:$?\"")
			if rSub.code != 0 || rSub.stdout != "after:137\n" {
				t.Fatalf("subshell kill -9 $$: code=%d stdout=%q stderr=%q", rSub.code, rSub.stdout, rSub.stderr)
			}

			// 5) Trapped TERM & numeric INT (-2) run shell trap handlers cleanly; SIGKILL cannot be trapped.
			rTrap := runNetprocScript(t, mode.ctx, t.TempDir(), nil, nil, nil, `
trap 'echo got_term' TERM
trap 'echo got_int' INT
kill -TERM $$
kill -2 $$
kill -0 $$
echo trap_done
`)
			wantTrap := "got_term\ngot_int\ntrap_done\n"
			if rTrap.code != 0 || rTrap.stdout != wantTrap {
				t.Fatalf("trapped self-signal: code=%d stdout=%q want=%q stderr=%q", rTrap.code, rTrap.stdout, wantTrap, rTrap.stderr)
			}

			rKillTrap := runNetprocScript(t, mode.ctx, t.TempDir(), nil, nil, nil, `
trap 'echo BAD_KILL_TRAP' KILL
kill -KILL $$
echo NEVER
`)
			if rKillTrap.code != 137 || rKillTrap.stdout != "" {
				t.Fatalf("KILL trap must not catch SIGKILL: code=%d stdout=%q", rKillTrap.code, rKillTrap.stdout)
			}

			// 6) Wrapper invocations (`env kill -9 $$`, `timeout 5 kill -9 $$`) also protect host.
			rEnv := runNetprocScript(t, mode.ctx, t.TempDir(), nil, nil, nil, "env kill -9 $$; echo NEVER")
			if rEnv.code != 137 || rEnv.stdout != "" {
				t.Fatalf("env kill -9 $$: code=%d stdout=%q stderr=%q", rEnv.code, rEnv.stdout, rEnv.stderr)
			}
			rTo := runNetprocScript(t, mode.ctx, t.TempDir(), nil, nil, nil, "timeout 5 kill -9 $$; echo NEVER")
			if rTo.code != 137 || rTo.stdout != "" {
				t.Fatalf("timeout kill -9 $$: code=%d stdout=%q stderr=%q", rTo.code, rTo.stdout, rTo.stderr)
			}

			// 7) Dangerous broadcast/group PIDs (0, -1, -$$) never kill the host process!
			for _, bad := range []string{"kill -9 0", "kill -9 -- -1", "kill -TERM -- -$$"} {
				rb := runNetprocScript(t, mode.ctx, t.TempDir(), nil, nil, nil, bad)
				if rb.code != 1 {
					t.Fatalf("%s: expected code=1, got code=%d stderr=%q", bad, rb.code, rb.stderr)
				}
			}
		})
	}

	// Even when standaloneCLIMode is true, an active CapabilityHook MUST protect the host process!
	prevCLI := standaloneCLIMode.Swap(true)
	defer standaloneCLIMode.Store(prevCLI)
	rCLIJailed := runNetprocScript(t, WithCapabilityHook(context.Background(), hook), t.TempDir(), nil, nil, nil, "kill -9 $$; echo NEVER")
	if rCLIJailed.code != 137 || rCLIJailed.stdout != "" {
		t.Fatalf("jailed CLI self-kill -9: code=%d stdout=%q stderr=%q", rCLIJailed.code, rCLIJailed.stdout, rCLIJailed.stderr)
	}
}

func startExternalSleeper(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	readyFile := filepath.Join(dir, fmt.Sprintf("ready_%d.pid", time.Now().UnixNano()))
	cmd := exec.Command(exe, "-test.run=^TestNetprocHelperProcess$", "--", "sleep", readyFile)
	cmd.Env = append(os.Environ(), "UNISH_NETPROC_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, rerr := os.ReadFile(readyFile); rerr == nil && len(data) > 0 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	return cmd
}

// 4. Process Inspection (`ps`, `pgrep`, `ss`) and External Signaling (`kill`, `pkill`) Jail.
func TestJailProc_KillPkillPgrepPsSsGating(t *testing.T) {
	tmp := t.TempDir()
	sleeper := startExternalSleeper(t, tmp)
	extPID := sleeper.Process.Pid

	// Phase 1: Deny all OpProc.
	denyHook := &customNetprocHook{
		checkFn: func(_ context.Context, req AccessRequest) error {
			if req.Op == OpProc {
				return &AccessDenialError{Op: OpProc, Target: strconv.Itoa(req.PID), Reason: "proc access denied"}
			}
			return nil
		},
	}
	denyCtx := WithCapabilityHook(context.Background(), denyHook)

	for _, cmdLine := range []struct {
		script  string
		wantCmd string
		wantPID int
		wantSig string
	}{
		{"ps", "ps", 0, "list"},
		{"ps aux", "ps", 0, "list"},
		{"pgrep unish", "pgrep", 0, "list"},
		{"ss -tln", "ss", 0, "list"},
		{"pkill -f TestNetprocHelperProcess", "pkill", 0, "list"},
		{fmt.Sprintf("kill -TERM %d", extPID), "kill", extPID, "TERM"},
		{fmt.Sprintf("kill -0 %d", extPID), "kill", extPID, "0"},
	} {
		res := runNetprocScript(t, denyCtx, tmp, nil, nil, nil, cmdLine.script)
		if res.code != 1 {
			t.Fatalf("%q under deny OpProc: code=%d, want 1 (stdout=%q stderr=%q)", cmdLine.script, res.code, res.stdout, res.stderr)
		}
		if res.stderr == "" {
			t.Fatalf("%q under deny OpProc: expected diagnostic on stderr", cmdLine.script)
		}
		reqs := denyHook.snapshotRequests()
		last := reqs[len(reqs)-1]
		if last.Op != OpProc || last.Command != cmdLine.wantCmd || last.PID != cmdLine.wantPID || last.Signal != cmdLine.wantSig {
			t.Fatalf("%q recorded %+v, want Op=proc Command=%q PID=%d Signal=%q",
				cmdLine.script, last, cmdLine.wantCmd, cmdLine.wantPID, cmdLine.wantSig)
		}
	}

	// Verify sleeper is still alive after blocked kill/pkill!
	if err := killProc(sleeper.Process, "0"); err != nil {
		t.Fatalf("external sleeper died despite OpProc denial: %v", err)
	}

	// Phase 2: Allow OpProc "list" (PID=0), still deny signaling specific external PIDs!
	listOnlyHook := &customNetprocHook{
		checkFn: func(_ context.Context, req AccessRequest) error {
			if req.Op == OpProc && req.PID == 0 && req.Signal == "list" {
				return nil
			}
			return &AccessDenialError{Op: OpProc, Target: strconv.Itoa(req.PID), Reason: "signaling external pid denied"}
		},
	}
	listCtx := WithCapabilityHook(context.Background(), listOnlyHook)

	psRes := runNetprocScript(t, listCtx, tmp, nil, nil, nil, "ps")
	if psRes.code != 0 || !strings.Contains(psRes.stdout, "PID") {
		t.Fatalf("ps with list allowed: code=%d stdout=%q stderr=%q", psRes.code, psRes.stdout, psRes.stderr)
	}
	ssRes := runNetprocScript(t, listCtx, tmp, nil, nil, nil, "ss -tln")
	if ssRes.code != 0 {
		t.Fatalf("ss with list allowed: code=%d stderr=%q", ssRes.code, ssRes.stderr)
	}
	// pkill can now enumerate processes, MUST STILL check OpProc per matched external PID and block!
	pkRes := runNetprocScript(t, listCtx, tmp, nil, nil, nil, "pkill -f TestNetprocHelperProcess")
	if pkRes.code != 1 {
		t.Fatalf("pkill with per-PID OpProc denied: code=%d, want 1", pkRes.code)
	}
	if err := killProc(sleeper.Process, "0"); err != nil {
		t.Fatalf("external sleeper died when pkill per-PID OpProc was denied: %v", err)
	}

	// Phase 3: Allow OpProc on extPID and verify kill succeeds.
	allowKillHook := &customNetprocHook{
		checkFn: func(_ context.Context, req AccessRequest) error {
			if req.Op == OpProc && req.PID == extPID {
				return nil
			}
			return &AccessDenialError{Op: OpProc, Target: "other"}
		},
	}
	okRes := runNetprocScript(t, WithCapabilityHook(context.Background(), allowKillHook), tmp, nil, nil, nil, fmt.Sprintf("kill -KILL %d", extPID))
	if okRes.code != 0 {
		t.Fatalf("kill allowed external PID failed: code=%d stderr=%q", okRes.code, okRes.stderr)
	}
}

// 5. Wrapper OpExec Gating across `env`, `nice`, `nohup`, and `timeout` (rc=126 on denial).
func TestJailWrappers_OpExecGating(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	exeBytes, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read test exe: %v", err)
	}

	allowedDir := t.TempDir()
	blockedDir := t.TempDir()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	allowedBin := filepath.Join(allowedDir, "allowed_tool"+ext)
	blockedBin := filepath.Join(blockedDir, "blocked_tool"+ext)
	if err := os.WriteFile(allowedBin, exeBytes, 0o755); err != nil {
		t.Fatalf("write allowedBin: %v", err)
	}
	if err := os.WriteFile(blockedBin, exeBytes, 0o755); err != nil {
		t.Fatalf("write blockedBin: %v", err)
	}

	jailHook := newTestJailHook(t, allowedDir)
	ctx := WithCapabilityHook(context.Background(), jailHook)

	envPairs := append(os.Environ(),
		"UNISH_NETPROC_HELPER=1",
		"PATH="+blockedDir+string(os.PathListSeparator)+allowedDir,
	)

	// 1) All 4 wrappers (`env`, `nice`, `nohup`, `timeout` + `timeout 0`) MUST block external binary in blockedDir with exit 126!
	blockedCases := []struct {
		wrapper string
		script  string
	}{
		{"env", "env blocked_tool -test.run=^TestNetprocHelperProcess$ -- echo hi"},
		{"env", fmt.Sprintf("env %q -test.run=^TestNetprocHelperProcess$ -- echo hi", filepath.ToSlash(blockedBin))},
		{"nice", "nice -n 5 blocked_tool -test.run=^TestNetprocHelperProcess$ -- echo hi"},
		{"nohup", "nohup blocked_tool -test.run=^TestNetprocHelperProcess$ -- echo hi"},
		{"timeout", "timeout 5 blocked_tool -test.run=^TestNetprocHelperProcess$ -- echo hi"},
		{"timeout", "timeout 0 blocked_tool -test.run=^TestNetprocHelperProcess$ -- echo hi"},
		{"timeout", "env nice nohup timeout 5 blocked_tool -test.run=^TestNetprocHelperProcess$ -- echo hi"},
	}
	for _, tc := range blockedCases {
		before := len(jailHook.requests)
		res := runNetprocScript(t, ctx, allowedDir, envPairs, nil, nil, tc.script)
		if res.code != 126 {
			t.Fatalf("%q: code=%d, want 126 (stdout=%q stderr=%q)", tc.script, res.code, res.stdout, res.stderr)
		}
		if res.stderr == "" || strings.Contains(res.stdout, "HELPER_OK") {
			t.Fatalf("%q: expected stderr error and no execution (stdout=%q stderr=%q)", tc.script, res.stdout, res.stderr)
		}
		foundOpExec := false
		for _, r := range jailHook.requests[before:] {
			if r.Op == OpExec && r.Command == tc.wrapper {
				foundOpExec = true
				break
			}
		}
		if !foundOpExec {
			t.Fatalf("%q: expected OpExec AccessRequest with Command=%q, got %+v", tc.script, tc.wrapper, jailHook.requests[before:])
		}
	}

	// 2) Builtin subcommands under wrappers succeed without OpExec.
	for _, bcmd := range []string{
		"env FOO=bar printenv FOO",
		"nice echo nice_ok",
		"nohup echo nohup_ok",
		"timeout 5 echo timeout_ok",
	} {
		res := runNetprocScript(t, ctx, allowedDir, envPairs, nil, nil, bcmd)
		if res.code != 0 {
			t.Fatalf("builtin wrapper %q failed: code=%d stderr=%q", bcmd, res.code, res.stderr)
		}
	}

	// 3) Allowed binary inside allowedDir runs cleanly through all 4 wrappers!
	for _, acmd := range []string{
		"env allowed_tool -test.run=^TestNetprocHelperProcess$ -- echo from_env",
		"nice allowed_tool -test.run=^TestNetprocHelperProcess$ -- echo from_nice",
		"nohup allowed_tool -test.run=^TestNetprocHelperProcess$ -- echo from_nohup",
		"timeout 5 allowed_tool -test.run=^TestNetprocHelperProcess$ -- echo from_timeout",
	} {
		res := runNetprocScript(t, ctx, allowedDir, envPairs, nil, nil, acmd)
		if res.code != 0 || !strings.Contains(res.stdout, "HELPER_OK:from_") {
			t.Fatalf("allowed wrapper %q: code=%d stdout=%q stderr=%q", acmd, res.code, res.stdout, res.stderr)
		}
	}
}

type simulatedTTYWriter struct {
	buf bytes.Buffer
}

func (w *simulatedTTYWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *simulatedTTYWriter) IsTTY() bool                 { return true }

// 6. Filesystem Jail & hc.Dir Isolation in `stat`, `df`, and `nohup` (`nohup.out`).
func TestJailFS_StatDfAndNohupOut(t *testing.T) {
	workspace := t.TempDir()
	subDir := filepath.Join(workspace, "sub")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	insideFile := filepath.Join(subDir, "note.txt")
	if err := os.WriteFile(insideFile, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("topsecret"), 0o644); err != nil {
		t.Fatal(err)
	}

	jailHook := newTestJailHook(t, workspace)
	ctx := WithCapabilityHook(context.Background(), jailHook)

	// 1) Relative `stat note.txt` and `df .` resolve against hc.Dir (subDir), NOT os.Getwd()!
	r1 := runNetprocScript(t, ctx, subDir, nil, nil, nil, "stat -c '%s %n' note.txt && df . >/dev/null")
	if r1.code != 0 || r1.stdout != "5 note.txt\n" {
		t.Fatalf("stat/df inside subDir: code=%d stdout=%q stderr=%q", r1.code, r1.stdout, r1.stderr)
	}

	// 2) `stat` and `df` outside workspace are blocked with OpStat + Permission denied.
	rStatOut := runNetprocScript(t, ctx, subDir, nil, nil, nil, fmt.Sprintf("stat %q", filepath.ToSlash(outsideFile)))
	if rStatOut.code != 1 || !strings.Contains(rStatOut.stderr, "Permission denied") {
		t.Fatalf("stat outside: code=%d stderr=%q", rStatOut.code, rStatOut.stderr)
	}
	rDfOut := runNetprocScript(t, ctx, subDir, nil, nil, nil, fmt.Sprintf("df %q", filepath.ToSlash(outside)))
	if rDfOut.code != 1 || !strings.Contains(rDfOut.stderr, "Permission denied") {
		t.Fatalf("df outside: code=%d stderr=%q", rDfOut.code, rDfOut.stderr)
	}

	// 3) Symlink escape via `stat -L` (when OS allows symlink creation).
	escLink := filepath.Join(subDir, "escape_sym")
	if err := os.Symlink(outsideFile, escLink); err == nil {
		rDeref := runNetprocScript(t, ctx, subDir, nil, nil, nil, "stat -L escape_sym")
		if rDeref.code != 1 || !strings.Contains(rDeref.stderr, "Permission denied") {
			t.Fatalf("stat -L symlink escape must fail: code=%d stderr=%q", rDeref.code, rDeref.stderr)
		}
	}

	// 4) `nohup` with TTY stdout writes `nohup.out` inside hc.Dir via fsOpenFile!
	ttyOut := &simulatedTTYWriter{}
	rNohup := runNetprocScript(t, ctx, subDir, []string{"HOME=" + outside}, nil, ttyOut, "nohup echo hello_nohup")
	if rNohup.code != 0 {
		t.Fatalf("nohup allowed workspace: code=%d stderr=%q", rNohup.code, rNohup.stderr)
	}
	gotNohupBytes, err := os.ReadFile(filepath.Join(subDir, "nohup.out"))
	if err != nil || strings.ReplaceAll(string(gotNohupBytes), "\r\n", "\n") != "hello_nohup\n" {
		t.Fatalf("expected subDir/nohup.out with hello_nohup, err=%v data=%q", err, string(gotNohupBytes))
	}

	// 5) Read-only jail blocks `nohup.out` creation in both hc.Dir and $HOME (`rc=125`).
	roDir := t.TempDir()
	roHook := &customNetprocHook{
		checkFn: func(_ context.Context, req AccessRequest) error {
			if req.Op == OpWrite {
				return &AccessDenialError{Op: OpWrite, Target: req.Path, Reason: "read-only workspace"}
			}
			return nil
		},
	}
	ttyOutRO := &simulatedTTYWriter{}
	rNohupRO := runNetprocScript(t, WithCapabilityHook(context.Background(), roHook), roDir, []string{"HOME=" + outside}, nil, ttyOutRO, "nohup echo blocked_out")
	if rNohupRO.code != 125 {
		t.Fatalf("nohup read-only jail: code=%d, want 125 (stderr=%q)", rNohupRO.code, rNohupRO.stderr)
	}
	if _, err := os.Stat(filepath.Join(roDir, "nohup.out")); err == nil {
		t.Fatal("nohup.out must not be created when OpWrite is denied")
	}
	if _, err := os.Stat(filepath.Join(outside, "nohup.out")); err == nil {
		t.Fatal("HOME/nohup.out must not be created outside jail")
	}
}

// 7. Concurrent Multi-Session Per-Context JobTable Isolation.
func TestConcurrentSessions_PerContextJobTableIsolation(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	exeBytes, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read test exe: %v", err)
	}

	dirA := t.TempDir()
	dirB := t.TempDir()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	binA := filepath.Join(dirA, "worker_a"+ext)
	binB := filepath.Join(dirB, "worker_b"+ext)
	if err := os.WriteFile(binA, exeBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binB, exeBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	jtA := NewJobTable()
	jtB := NewJobTable()
	ctxA := WithJobTable(WithCapabilityHook(context.Background(), newTestJailHook(t, dirA)), jtA)
	ctxB := WithJobTable(WithCapabilityHook(context.Background(), newTestJailHook(t, dirB)), jtB)

	readyA := filepath.Join(dirA, "a.pid")
	readyB := filepath.Join(dirB, "b.pid")

	// Create two persistent Runner instances (simulating two concurrent unish.Session instances).
	makeRunner := func(dir string, envExtra []string, out, serr *bytes.Buffer) *interp.Runner {
		r, rerr := interp.New(
			interp.StdIO(nil, out, serr),
			interp.Dir(dir),
			interp.Env(expand.ListEnviron(envExtra...)),
			interp.CallHandler(callOverride),
			interp.OpenHandler(shellOpenHandler()),
			interp.ExecHandlers(trackExec, extraHandler),
			interp.ProcSubstHandler(procSubstHandler),
		)
		if rerr != nil {
			t.Fatalf("interp.New: %v", rerr)
		}
		return r
	}

	var outA, errA, outB, errB bytes.Buffer
	envA := append(os.Environ(), "UNISH_NETPROC_HELPER=1", "PATH="+dirA)
	envB := append(os.Environ(), "UNISH_NETPROC_HELPER=1", "PATH="+dirB)
	rA := makeRunner(dirA, envA, &outA, &errA)
	rB := makeRunner(dirB, envB, &outB, &errB)

	runStep := func(ctx context.Context, r *interp.Runner, out, serr *bytes.Buffer, codeSrc string) (string, string, int) {
		out.Reset()
		serr.Reset()
		prog, perr := syntax.NewParser().Parse(strings.NewReader(codeSrc), "step.sh")
		if perr != nil {
			t.Fatalf("parse: %v", perr)
		}
		err := r.Run(ctx, prog)
		rc := 0
		if err != nil {
			var es interp.ExitStatus
			if errors.As(err, &es) {
				rc = int(es)
			} else {
				rc = 1
			}
		}
		return strings.ReplaceAll(out.String(), "\r\n", "\n"), strings.ReplaceAll(serr.String(), "\r\n", "\n"), rc
	}

	// Launch background jobs concurrently in Session A and Session B!
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, s, rc := runStep(ctxA, rA, &outA, &errA, fmt.Sprintf("worker_a -test.run=^TestNetprocHelperProcess$ -- sleep %q &", filepath.ToSlash(readyA)))
		if rc != 0 {
			t.Errorf("Session A bg start rc=%d stderr=%q", rc, s)
		}
	}()
	go func() {
		defer wg.Done()
		_, s, rc := runStep(ctxB, rB, &outB, &errB, fmt.Sprintf("worker_b -test.run=^TestNetprocHelperProcess$ -- sleep %q &", filepath.ToSlash(readyB)))
		if rc != 0 {
			t.Errorf("Session B bg start rc=%d stderr=%q", rc, s)
		}
	}()
	wg.Wait()

	waitPID := func(pidFile string) int {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if b, rerr := os.ReadFile(pidFile); rerr == nil && len(b) > 0 {
				if p, aerr := strconv.Atoi(strings.TrimSpace(string(b))); aerr == nil && p > 0 {
					return p
				}
			}
			time.Sleep(15 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", pidFile)
		return 0
	}
	pidA := waitPID(readyA)
	pidB := waitPID(readyB)
	t.Cleanup(func() {
		if p, e := os.FindProcess(pidA); e == nil {
			_ = p.Kill()
		}
		if p, e := os.FindProcess(pidB); e == nil {
			_ = p.Kill()
		}
	})

	// 1) Each session's `jobs -l` sees ONLY its own `[1]` job (never the other session's job!).
	jobsA, _, rcA := runStep(ctxA, rA, &outA, &errA, "jobs -l")
	jobsB, _, rcB := runStep(ctxB, rB, &outB, &errB, "jobs -l")
	if rcA != 0 || !strings.Contains(jobsA, "[1]") || !strings.Contains(jobsA, "worker_a") || strings.Contains(jobsA, "worker_b") {
		t.Fatalf("Session A jobs isolation broken: %q", jobsA)
	}
	if rcB != 0 || !strings.Contains(jobsB, "[1]") || !strings.Contains(jobsB, "worker_b") || strings.Contains(jobsB, "worker_a") {
		t.Fatalf("Session B jobs isolation broken: %q", jobsB)
	}

	// 2) Cross-session PID kill MUST be blocked by OpProc because pidB is NOT in jtA!
	_, crossErr, crossRC := runStep(ctxA, rA, &outA, &errA, fmt.Sprintf("kill -KILL %d", pidB))
	if crossRC != 1 {
		t.Fatalf("Session A killing Session B PID %d should fail with rc=1, got rc=%d stderr=%q", pidB, crossRC, crossErr)
	}

	// 3) Session A `kill -KILL %1 && wait %1` kills ONLY worker_a; worker_b in Session B stays Running!
	_, killAErr, _ := runStep(ctxA, rA, &outA, &errA, "kill -KILL %1; wait %1; true")
	if killAErr != "" {
		t.Fatalf("Session A kill %%1 unexpected stderr: %q", killAErr)
	}

	jobsBAfter, _, _ := runStep(ctxB, rB, &outB, &errB, "jobs")
	if !strings.Contains(jobsBAfter, "Running") || !strings.Contains(jobsBAfter, "worker_b") {
		t.Fatalf("Session B worker_b must still be Running after Session A killed %%1, got: %q", jobsBAfter)
	}

	// 4) Session B kills its own job cleanly via `pkill -KILL -g 1` (own tracked job) while `pkill -g 99999` is blocked by OpProc!
	_, pkDenyErr, pkDenyRC := runStep(ctxB, rB, &outB, &errB, "pkill -KILL -g 99999")
	if pkDenyRC != 1 || pkDenyErr == "" {
		t.Fatalf("Session B pkill -g 99999 should be denied by OpProc: rc=%d stderr=%q", pkDenyRC, pkDenyErr)
	}
	_, killBErr, killBRC := runStep(ctxB, rB, &outB, &errB, "pkill -KILL -g 1 && wait $!; true")
	if killBRC != 0 || killBErr != "" {
		t.Fatalf("Session B pkill -g 1 / wait $! unexpected rc=%d stderr=%q", killBRC, killBErr)
	}
}
