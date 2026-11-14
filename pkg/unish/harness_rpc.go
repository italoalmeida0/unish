package unish

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HarnessRequest represents an inbound JSONL message sent to a HarnessServer.
type HarnessRequest struct {
	ID        string            `json:"id,omitempty"`
	Type      string            `json:"type"`
	Script    string            `json:"script,omitempty"`
	Command   string            `json:"command,omitempty"`
	Stdin     string            `json:"stdin,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Dir       string            `json:"dir,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	TimeoutMs int64             `json:"timeout_ms,omitempty"`
	ReqID     string            `json:"req_id,omitempty"`
	Decision  string            `json:"decision,omitempty"`
	Verdict   string            `json:"verdict,omitempty"`
}

// HarnessAccessRequest is the snake_case JSON wire representation of AccessRequest.
type HarnessAccessRequest struct {
	Op         AccessOp `json:"op"`
	Command    string   `json:"command,omitempty"`
	Path       string   `json:"path,omitempty"`
	RawPath    string   `json:"raw_path,omitempty"`
	TargetPath string   `json:"target_path,omitempty"`
	Proto      string   `json:"proto,omitempty"`
	Host       string   `json:"host,omitempty"`
	Port       int      `json:"port,omitempty"`
	ResolvedIP string   `json:"resolved_ip,omitempty"`
	Binary     string   `json:"binary,omitempty"`
	Argv       []string `json:"argv,omitempty"`
	Cwd        string   `json:"cwd,omitempty"`
	PID        int      `json:"pid,omitempty"`
	Signal     string   `json:"signal,omitempty"`
}

func toHarnessAccessRequest(req AccessRequest) HarnessAccessRequest {
	var argv []string
	if len(req.Argv) > 0 {
		argv = append([]string(nil), req.Argv...)
	}
	return HarnessAccessRequest{
		Op:         req.Op,
		Command:    req.Command,
		Path:       req.Path,
		RawPath:    req.RawPath,
		TargetPath: req.TargetPath,
		Proto:      req.Proto,
		Host:       req.Host,
		Port:       req.Port,
		ResolvedIP: req.ResolvedIP,
		Binary:     req.Binary,
		Argv:       argv,
		Cwd:        req.WorkDir,
		PID:        req.PID,
		Signal:     req.Signal,
	}
}

// HarnessEvent represents an outbound JSONL message emitted by a HarnessServer.
type HarnessEvent struct {
	ID           string                 `json:"id,omitempty"`
	Type         string                 `json:"type"`
	Data         string                 `json:"data,omitempty"`
	ExitCode     *int                   `json:"exit_code,omitempty"`
	Stdout       *string                `json:"stdout,omitempty"`
	Stderr       *string                `json:"stderr,omitempty"`
	Cwd          string                 `json:"cwd,omitempty"`
	DurationMs   *int64                 `json:"duration_ms,omitempty"`
	Cancelled    bool                   `json:"cancelled,omitempty"`
	Denials      []HarnessAccessRequest `json:"denials,omitempty"`
	Env          []string               `json:"env,omitempty"`
	LastExitCode *int                   `json:"last_exit_code,omitempty"`
	Umask        string                 `json:"umask,omitempty"`
	ReqID        string                 `json:"req_id,omitempty"`
	Op           AccessOp               `json:"op,omitempty"`
	Path         string                 `json:"path,omitempty"`
	Host         string                 `json:"host,omitempty"`
	Port         int                    `json:"port,omitempty"`
	Binary       string                 `json:"binary,omitempty"`
	Request      *HarnessAccessRequest  `json:"request,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

// HarnessServer serves the bidirectional JSONL AI-agent harness protocol over
// an input reader and mutex-serialized output writer.
//
// Commands ("exec", "set_cwd", "get_state") execute sequentially on a dedicated
// background worker goroutine so the stdin reader loop remains non-blocking and
// can process mid-command "permission_reply", "cancel", and "ping" messages
// without head-of-line deadlock.
type HarnessServer struct {
	in   io.Reader
	out  io.Writer
	sess *Session

	writeMu sync.Mutex

	queueMu     sync.Mutex
	queueCond   *sync.Cond
	queue       []HarnessRequest
	queueClosed bool

	activeMu     sync.Mutex
	activeExecID string

	pendingMu  sync.Mutex
	pendingSeq atomic.Uint64
	pending    map[string]chan Decision

	stdinDone chan struct{}
}

// NewHarnessServer constructs a HarnessServer backed by a new persistent Session
// configured with opts. If the Session has a *Jail without a pre-configured
// Approver, the server wires its interactive JSONL ask_permission Approver.
func NewHarnessServer(in io.Reader, out io.Writer, opts ...SessionOption) (*HarnessServer, error) {
	sess, err := NewSession(opts...)
	if err != nil {
		return nil, err
	}
	return NewHarnessServerWithSession(in, out, sess), nil
}

// NewHarnessServerWithSession constructs a HarnessServer wrapping an existing Session.
func NewHarnessServerWithSession(in io.Reader, out io.Writer, sess *Session) *HarnessServer {
	s := &HarnessServer{
		in:        in,
		out:       out,
		sess:      sess,
		pending:   make(map[string]chan Decision),
		stdinDone: make(chan struct{}),
	}
	s.queueCond = sync.NewCond(&s.queueMu)
	if j := sess.Jail(); j != nil {
		j.mu.Lock()
		hasApprover := j.approver != nil
		j.mu.Unlock()
		if !hasApprover {
			j.SetApprover(s.promptPermission)
		}
	}
	return s
}

// RunHarness creates a HarnessServer over in/out with opts and runs it until
// EOF or context cancellation, closing the underlying Session on return.
func RunHarness(ctx context.Context, in io.Reader, out io.Writer, opts ...SessionOption) error {
	srv, err := NewHarnessServer(in, out, opts...)
	if err != nil {
		return err
	}
	defer srv.sess.Close()
	return srv.Serve(ctx)
}

// Session returns the underlying persistent *Session managed by s.
func (s *HarnessServer) Session() *Session {
	return s.sess
}

// Serve runs the JSONL read/dispatch loop until in reaches EOF or ctx is cancelled,
// waiting for any queued commands to finish before returning.
func (s *HarnessServer) Serve(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		s.workerLoop(ctx)
	}()

	stopCancelWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.sess.CancelCurrent()
			s.closeQueue()
		case <-stopCancelWatch:
		}
	}()

	reader := bufio.NewReaderSize(s.in, 64*1024)
	var readErr error
	for {
		if ctx.Err() != nil {
			readErr = ctx.Err()
			break
		}
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				s.dispatchLine(trimmed)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}

	close(s.stdinDone)
	s.failAllPendingPermissions()
	s.closeQueue()
	<-workerDone
	close(stopCancelWatch)
	return readErr
}

func (s *HarnessServer) dispatchLine(raw []byte) {
	var req HarnessRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		_ = s.sendEvent(HarnessEvent{
			Type:  "error",
			Error: fmt.Sprintf("invalid JSON message: %v", err),
		})
		return
	}

	switch strings.ToLower(strings.TrimSpace(req.Type)) {
	case "ping":
		_ = s.sendEvent(HarnessEvent{
			ID:   req.ID,
			Type: "pong",
		})

	case "permission_reply":
		s.handlePermissionReply(req)

	case "cancel":
		s.handleCancel(req)

	case "exec", "set_cwd", "get_state", "revoke_grants":
		s.enqueue(req)

	default:
		_ = s.sendEvent(HarnessEvent{
			ID:    req.ID,
			Type:  "error",
			Error: fmt.Sprintf("unknown message type %q", req.Type),
		})
	}
}

func (s *HarnessServer) enqueue(req HarnessRequest) {
	s.queueMu.Lock()
	if !s.queueClosed {
		s.queue = append(s.queue, req)
		s.queueCond.Signal()
	}
	s.queueMu.Unlock()
}

func (s *HarnessServer) closeQueue() {
	s.queueMu.Lock()
	if !s.queueClosed {
		s.queueClosed = true
		s.queueCond.Broadcast()
	}
	s.queueMu.Unlock()
}

func (s *HarnessServer) dequeue() (HarnessRequest, bool) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	for len(s.queue) == 0 && !s.queueClosed {
		s.queueCond.Wait()
	}
	if len(s.queue) == 0 {
		return HarnessRequest{}, false
	}
	req := s.queue[0]
	s.queue = s.queue[1:]
	if len(s.queue) == 0 {
		s.queue = nil
	}
	return req, true
}

func (s *HarnessServer) workerLoop(ctx context.Context) {
	for {
		req, ok := s.dequeue()
		if !ok {
			return
		}
		if ctx.Err() != nil {
			return
		}
		switch strings.ToLower(strings.TrimSpace(req.Type)) {
		case "exec":
			s.runExec(ctx, req)
		case "set_cwd":
			s.runSetCwd(req)
		case "get_state":
			s.runGetState(req)
		case "revoke_grants":
			if j := s.sess.Jail(); j != nil {
				j.RevokeSessionGrants()
			}
			s.runGetState(req)
		}
	}
}

func (s *HarnessServer) runExec(ctx context.Context, req HarnessRequest) {
	script := req.Script
	if script == "" && req.Command != "" {
		script = req.Command
	}
	workDir := req.Cwd
	if workDir == "" && req.Dir != "" {
		workDir = req.Dir
	}

	s.activeMu.Lock()
	s.activeExecID = req.ID
	s.activeMu.Unlock()
	defer func() {
		s.activeMu.Lock()
		s.activeExecID = ""
		s.activeMu.Unlock()
	}()

	var execOpts ExecOptions
	if req.Stdin != "" {
		execOpts.Stdin = strings.NewReader(req.Stdin)
	}
	if workDir != "" {
		execOpts.WorkDir = workDir
	}
	if len(req.Env) > 0 {
		execOpts.Env = req.Env
	}
	if req.TimeoutMs > 0 {
		execOpts.Timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	execOpts.Stdout = &harnessChunkWriter{srv: s, id: req.ID, streamType: "stdout"}
	execOpts.Stderr = &harnessChunkWriter{srv: s, id: req.ID, streamType: "stderr"}

	res, err := s.sess.ExecWith(ctx, script, execOpts)
	if res == nil {
		errMsg := "execution failed"
		if err != nil {
			errMsg = err.Error()
		}
		_ = s.sendEvent(HarnessEvent{
			ID:    req.ID,
			Type:  "error",
			Error: errMsg,
		})
		return
	}

	cancelled := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	var denials []HarnessAccessRequest
	if len(res.Denials) > 0 {
		denials = make([]HarnessAccessRequest, len(res.Denials))
		for i := range res.Denials {
			denials[i] = toHarnessAccessRequest(res.Denials[i])
		}
	}

	exitCode := res.ExitCode
	stdout := res.Stdout
	stderr := res.Stderr
	durMs := res.Duration.Milliseconds()
	_ = s.sendEvent(HarnessEvent{
		ID:         req.ID,
		Type:       "result",
		ExitCode:   &exitCode,
		Stdout:     &stdout,
		Stderr:     &stderr,
		Cwd:        res.WorkDir,
		DurationMs: &durMs,
		Cancelled:  cancelled,
		Denials:    denials,
	})
}

func (s *HarnessServer) runSetCwd(req HarnessRequest) {
	dir := req.Cwd
	if dir == "" {
		dir = req.Dir
	}
	if err := s.sess.SetCwd(dir); err != nil {
		_ = s.sendEvent(HarnessEvent{
			ID:    req.ID,
			Type:  "error",
			Error: err.Error(),
		})
		return
	}
	s.runGetState(req)
}

func (s *HarnessServer) runGetState(req HarnessRequest) {
	lastExit := s.sess.LastExitCode()
	_ = s.sendEvent(HarnessEvent{
		ID:           req.ID,
		Type:         "state",
		Cwd:          s.sess.Cwd(),
		Env:          s.sess.Env(),
		LastExitCode: &lastExit,
		Umask:        fmt.Sprintf("%04o", s.sess.Umask()),
	})
}

func (s *HarnessServer) handleCancel(req HarnessRequest) {
	target := strings.TrimSpace(req.ReqID)
	if target != "" {
		s.activeMu.Lock()
		active := s.activeExecID
		s.activeMu.Unlock()
		if active != "" && active != target {
			return
		}
	}
	// Remove any queued-but-not-yet-started exec matching target or ID.
	if target != "" {
		s.queueMu.Lock()
		filtered := s.queue[:0]
		for _, q := range s.queue {
			if q.Type == "exec" && q.ID == target {
				exitCode := 130
				empty := ""
				zero := int64(0)
				_ = s.sendEvent(HarnessEvent{
					ID:         q.ID,
					Type:       "result",
					ExitCode:   &exitCode,
					Stdout:     &empty,
					Stderr:     &empty,
					Cwd:        s.sess.Cwd(),
					DurationMs: &zero,
					Cancelled:  true,
				})
				continue
			}
			filtered = append(filtered, q)
		}
		s.queue = filtered
		s.queueMu.Unlock()
	}
	s.sess.CancelCurrent()
}

func (s *HarnessServer) promptPermission(ctx context.Context, accessReq AccessRequest) (Decision, error) {
	seq := s.pendingSeq.Add(1)
	reqID := fmt.Sprintf("perm-%d", seq)
	replyCh := make(chan Decision, 1)

	s.pendingMu.Lock()
	s.pending[reqID] = replyCh
	s.pendingMu.Unlock()

	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, reqID)
		s.pendingMu.Unlock()
	}()

	s.activeMu.Lock()
	execID := s.activeExecID
	s.activeMu.Unlock()

	wireReq := toHarnessAccessRequest(accessReq)
	if err := s.sendEvent(HarnessEvent{
		ID:      execID,
		Type:    "ask_permission",
		ReqID:   reqID,
		Op:      wireReq.Op,
		Path:    wireReq.Path,
		Host:    wireReq.Host,
		Port:    wireReq.Port,
		Binary:  wireReq.Binary,
		Request: &wireReq,
	}); err != nil {
		return DecisionDeny, err
	}

	select {
	case dec := <-replyCh:
		return dec, nil
	case <-ctx.Done():
		return DecisionDeny, ctx.Err()
	case <-s.stdinDone:
		return DecisionDeny, nil
	}
}

func (s *HarnessServer) handlePermissionReply(req HarnessRequest) {
	reqID := strings.TrimSpace(req.ReqID)
	if reqID == "" {
		reqID = strings.TrimSpace(req.ID)
	}
	rawDec := req.Decision
	if rawDec == "" {
		rawDec = req.Verdict
	}
	dec, ok := parseHarnessDecision(rawDec)
	if !ok {
		_ = s.sendEvent(HarnessEvent{
			ID:    req.ID,
			ReqID: reqID,
			Type:  "error",
			Error: fmt.Sprintf("invalid permission decision %q (expected allow, allow_session, or deny)", rawDec),
		})
		dec = DecisionDeny
	}

	s.pendingMu.Lock()
	var ch chan Decision
	if reqID != "" {
		ch = s.pending[reqID]
		if ch != nil {
			delete(s.pending, reqID)
		}
	} else if len(s.pending) == 1 {
		for k, v := range s.pending {
			ch = v
			delete(s.pending, k)
		}
	}
	s.pendingMu.Unlock()

	if ch != nil {
		select {
		case ch <- dec:
		default:
		}
	}
}

func (s *HarnessServer) failAllPendingPermissions() {
	s.pendingMu.Lock()
	pending := s.pending
	s.pending = make(map[string]chan Decision)
	s.pendingMu.Unlock()

	for _, ch := range pending {
		select {
		case ch <- DecisionDeny:
		default:
		}
	}
}

func parseHarnessDecision(raw string) (Decision, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "allow", "once", "yes":
		return DecisionAllow, true
	case "allow_session", "session", "always":
		return DecisionAllowSession, true
	case "deny", "no", "block":
		return DecisionDeny, true
	default:
		return DecisionDeny, false
	}
}

func (s *HarnessServer) sendEvent(ev HarnessEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	s.writeMu.Lock()
	_, werr := s.out.Write(data)
	s.writeMu.Unlock()
	return werr
}

type harnessChunkWriter struct {
	srv        *HarnessServer
	id         string
	streamType string
}

func (w *harnessChunkWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	_ = w.srv.sendEvent(HarnessEvent{
		ID:   w.id,
		Type: w.streamType,
		Data: string(p),
	})
	return len(p), nil
}
