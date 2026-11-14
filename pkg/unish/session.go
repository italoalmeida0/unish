package unish

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// ErrSessionClosed is returned when operations are invoked on a closed Session.
var ErrSessionClosed = errors.New("unish: session closed")

const internalRestoreExitCmd = "__unish_internal_restore_exit__"

type sessionCtxKey struct{}

func withSessionInContext(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, s)
}

func sessionFromContext(ctx context.Context) *Session {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(sessionCtxKey{}).(*Session)
	return s
}

var (
	sessionExtrasOnce sync.Once
	origKillExtraMain func(ctx context.Context, hc interp.HandlerContext, args []string) error
)

func ensureSessionExtrasRegistered() {
	sessionExtrasOnce.Do(func() {
		registerExtra("printenv", cmdSessionAwarePrintenv)
		if prev := lookupExtra("kill"); prev != nil {
			orig := prev.main
			origKillExtraMain = orig
			registerExtra("kill", func(ctx context.Context, hc interp.HandlerContext, args []string) error {
				if sess := sessionFromContext(ctx); sess != nil {
					code, _ := sess.runSessionKill(ctx, hc, args)
					if code != 0 {
						return exitError{code}
					}
					return nil
				}
				return orig(ctx, hc, args)
			})
		}
	})
}

func init() {
	registerExtra("printenv", cmdSessionAwarePrintenv)
}

// SessionConfig configures a persistent embedded unish Session.
type SessionConfig struct {
	WorkDir    string
	Env        []string
	EnvMap     map[string]string
	InheritEnv bool
	ScrubEnv   []string
	Jail       *Jail
	Hook       CapabilityHook
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
}

// SessionOption mutates a SessionConfig passed to NewSession or Exec.
type SessionOption func(*SessionConfig)

// WithConfig replaces the SessionConfig with cfg.
func WithConfig(cfg SessionConfig) SessionOption {
	return func(dst *SessionConfig) {
		*dst = cfg
		if len(cfg.Env) > 0 {
			dst.Env = append([]string(nil), cfg.Env...)
		}
		if len(cfg.EnvMap) > 0 {
			dst.EnvMap = maps.Clone(cfg.EnvMap)
		}
		if len(cfg.ScrubEnv) > 0 {
			dst.ScrubEnv = append([]string(nil), cfg.ScrubEnv...)
		}
	}
}

// WithWorkDir sets the initial working directory for the Session without
// modifying the host process working directory (os.Chdir is never called).
func WithWorkDir(dir string) SessionOption {
	return func(c *SessionConfig) {
		c.WorkDir = dir
	}
}

// WithEnv appends explicit KEY=VALUE environment bindings to the Session.
func WithEnv(env []string) SessionOption {
	return func(c *SessionConfig) {
		c.Env = append(c.Env, env...)
	}
}

// WithEnvMap overlays explicit key/value environment bindings onto the Session.
func WithEnvMap(m map[string]string) SessionOption {
	return func(c *SessionConfig) {
		if len(m) == 0 {
			return
		}
		if c.EnvMap == nil {
			c.EnvMap = make(map[string]string, len(m))
		}
		maps.Copy(c.EnvMap, m)
	}
}

// WithInheritEnv controls whether the Session inherits os.Environ() from the
// host process (enabled by default).
func WithInheritEnv(inherit bool) SessionOption {
	return func(c *SessionConfig) {
		c.InheritEnv = inherit
	}
}

// WithScrubEnv strips inherited host environment keys matching any of the
// provided wildcard patterns (matched case-insensitively, e.g. "*_KEY",
// "*_SECRET", "*_TOKEN"). Explicit bindings set via WithEnv or WithEnvMap are
// applied after scrubbing and are preserved.
func WithScrubEnv(patterns ...string) SessionOption {
	return func(c *SessionConfig) {
		c.ScrubEnv = append(c.ScrubEnv, patterns...)
	}
}

// WithJail attaches j as the Session's capability enforcement policy.
func WithJail(j *Jail) SessionOption {
	return func(c *SessionConfig) {
		c.Jail = j
		if j != nil {
			c.Hook = j
		}
	}
}

// WithHook attaches a custom CapabilityHook to the Session.
// (Note: WithCapabilityHook attaches a CapabilityHook to a context.Context.)
func WithHook(h CapabilityHook) SessionOption {
	return func(c *SessionConfig) {
		c.Hook = h
		if j, ok := h.(*Jail); ok {
			c.Jail = j
		}
	}
}

// WithSessionHook is an alias for WithHook.
func WithSessionHook(h CapabilityHook) SessionOption {
	return WithHook(h)
}

// WithStdin sets the default standard input reader for commands executed on the Session.
func WithStdin(r io.Reader) SessionOption {
	return func(c *SessionConfig) {
		c.Stdin = r
	}
}

// WithStdout sets an optional streaming writer that receives live standard
// output alongside ExecResult.Stdout capture.
func WithStdout(w io.Writer) SessionOption {
	return func(c *SessionConfig) {
		c.Stdout = w
	}
}

// WithStderr sets an optional streaming writer that receives live standard
// error alongside ExecResult.Stderr capture.
func WithStderr(w io.Writer) SessionOption {
	return func(c *SessionConfig) {
		c.Stderr = w
	}
}

// ExecOptions configures a single Session.ExecWith invocation.
type ExecOptions struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	WorkDir string
	Env     map[string]string
	Timeout time.Duration
}

// ExecResult captures the structured outcome of executing a script in a Session.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	WorkDir  string
	Duration time.Duration
	Denials  []AccessRequest
}

// Exec executes script in a fresh one-shot Session configured with opts and
// closes the Session before returning.
func Exec(ctx context.Context, script string, opts ...SessionOption) (*ExecResult, error) {
	sess, err := NewSession(opts...)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	return sess.Exec(ctx, script)
}

// Session is a thread-safe, stateful embedded unish shell instance that
// preserves working directory, shell & exported variables, functions, aliases,
// shell options, umask, and last exit status ($?) across Exec calls without
// mutating host process globals such as os.Chdir or syscall.Umask.
type Session struct {
	execMu sync.Mutex

	cancelMu      sync.Mutex
	currentCancel context.CancelFunc
	cancelledByUs *atomic.Bool
	closed        bool

	runner *interp.Runner
	jail   *Jail
	hook   CapabilityHook

	inheritEnv    bool
	scrubPatterns []string
	baseEnv       map[string]string
	baseEnvOrder  []string

	defaultStdin  io.Reader
	defaultStdout io.Writer
	defaultStderr io.Writer

	stdinProxy  *sessionStdinProxy
	stdoutProxy *syncCaptureWriter
	stderrProxy *syncCaptureWriter

	cwdPtr   atomic.Pointer[string]
	umask    atomic.Int32
	lastExit atomic.Int32

	// Internal fields accessed only while holding execMu.
	inInternalOp bool
	savedExit    int
}

// NewSession creates a new persistent unish Session configured with opts.
func NewSession(opts ...SessionOption) (*Session, error) {
	ensureSessionExtrasRegistered()
	cfg := SessionConfig{
		InheritEnv: true,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	hook := cfg.Hook
	if hook == nil && cfg.Jail != nil {
		hook = cfg.Jail
	}
	jail := cfg.Jail
	if jail == nil {
		if j, ok := hook.(*Jail); ok {
			jail = j
		}
	}

	workDir, err := resolveInitialSessionDir(cfg.WorkDir, jail)
	if err != nil {
		return nil, err
	}

	scrub := make([]string, 0, len(cfg.ScrubEnv))
	for _, p := range cfg.ScrubEnv {
		p = strings.TrimSpace(p)
		if p != "" {
			scrub = append(scrub, strings.ToLower(p))
		}
	}

	envMap, envOrder := buildInitialSessionEnv(cfg, workDir, scrub)
	envPairs := make([]string, 0, len(envOrder))
	for _, k := range envOrder {
		envPairs = append(envPairs, k+"="+envMap[k])
	}

	s := &Session{
		jail:          jail,
		hook:          wrapCapabilityHookForDenials(hook),
		inheritEnv:    cfg.InheritEnv,
		scrubPatterns: scrub,
		baseEnv:       envMap,
		baseEnvOrder:  envOrder,
		defaultStdin:  cfg.Stdin,
		defaultStdout: cfg.Stdout,
		defaultStderr: cfg.Stderr,
		stdinProxy:    &sessionStdinProxy{},
		stdoutProxy:   &syncCaptureWriter{},
		stderrProxy:   &syncCaptureWriter{},
	}
	s.umask.Store(0o022)
	s.storeCwd(workDir)

	r, err := interp.New(
		interp.Env(expand.ListEnviron(envPairs...)),
		interp.Dir(workDir),
		interp.StdIO(s.stdinProxy, s.stdoutProxy, s.stderrProxy),
		interp.BashOpts("-s", "expand_aliases"),
		interp.OpenHandler(s.openHandler),
		interp.ReadDirHandler2(s.readDirHandler),
		interp.StatHandler(s.statHandler),
		interp.AccessHandler(s.accessHandler),
		interp.CallHandler(s.callHandler),
		interp.ExecHandlers(
			s.execMiddleware,
			trackExec,
			extraHandler,
		),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		return nil, err
	}
	r.Reset()
	if r.Vars == nil {
		r.Vars = make(map[string]expand.Variable, len(envOrder))
	}
	for _, k := range envOrder {
		r.Vars[k] = expand.Variable{
			Set:      true,
			Exported: true,
			Kind:     expand.String,
			Str:      envMap[k],
		}
	}
	s.runner = r
	registerRunnerCompletionHook(r, s.hook)
	registerRunnerEnvIsolated(r, !s.inheritEnv || len(s.scrubPatterns) > 0)
	return s, nil
}

func resolveInitialSessionDir(rawDir string, jail *Jail) (string, error) {
	dir := strings.TrimSpace(rawDir)
	if dir == "" {
		wd, wdErr := os.Getwd()
		if jail != nil && len(jail.roots) > 0 {
			useJailRoot := wdErr != nil
			if !useJailRoot {
				realWD, _ := canonicalizeForCheck(wd)
				normWD := normalizeMatchPath(realWD)
				inRoot := false
				for _, r := range jail.roots {
					if isWithinNormalizedRoot(normWD, r) {
						inRoot = true
						break
					}
				}
				if !inRoot {
					for _, r := range jail.readOnlyRoots {
						if isWithinNormalizedRoot(normWD, r) {
							inRoot = true
							break
						}
					}
				}
				useJailRoot = !inRoot
			}
			if useJailRoot {
				dir = filepath.FromSlash(jail.roots[0])
			} else {
				dir = wd
			}
		} else {
			if wdErr != nil {
				return "", wdErr
			}
			dir = wd
		}
	} else {
		dir = resolveHostPath("", expandTildePath(dir))
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	realDir, _ := canonicalizeForCheck(absDir)
	fi, err := os.Stat(realDir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", &os.PathError{Op: "chdir", Path: rawDir, Err: syscall.ENOTDIR}
	}
	return realDir, nil
}

func buildInitialSessionEnv(cfg SessionConfig, workDir string, lowerScrub []string) (map[string]string, []string) {
	merged := make(map[string]string, 32)
	order := make([]string, 0, 32)
	put := func(k, v string) {
		if k == "" {
			return
		}
		if _, exists := merged[k]; !exists {
			order = append(order, k)
		}
		merged[k] = v
	}

	if cfg.InheritEnv {
		for _, kv := range os.Environ() {
			i := strings.IndexByte(kv, '=')
			if i <= 0 {
				continue
			}
			k, v := kv[:i], kv[i+1:]
			if matchesAnyScrubPattern(k, lowerScrub) {
				continue
			}
			put(k, v)
		}
	}

	for _, kv := range cfg.Env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		put(kv[:i], kv[i+1:])
	}

	if len(cfg.EnvMap) > 0 {
		keys := make([]string, 0, len(cfg.EnvMap))
		for k := range cfg.EnvMap {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			put(k, cfg.EnvMap[k])
		}
	}

	put("PWD", workDir)
	return merged, order
}

func matchesAnyScrubPattern(key string, lowerPatterns []string) bool {
	if len(lowerPatterns) == 0 {
		return false
	}
	lk := strings.ToLower(key)
	for _, pat := range lowerPatterns {
		if matchSingleSegmentGlob(pat, lk) {
			return true
		}
	}
	return false
}

// Jail returns the *Jail attached to s, or nil if none was configured.
func (s *Session) Jail() *Jail {
	return s.jail
}

// Cwd returns the Session's current working directory without consulting os.Getwd().
func (s *Session) Cwd() string {
	if p := s.cwdPtr.Load(); p != nil {
		return *p
	}
	return ""
}

func (s *Session) storeCwd(dir string) {
	d := dir
	s.cwdPtr.Store(&d)
}

// LastExitCode returns the exit status ($?) of the most recently executed statement.
func (s *Session) LastExitCode() int {
	return int(s.lastExit.Load())
}

// Umask returns the Session's current file mode creation mask (default 0022).
func (s *Session) Umask() int {
	return int(s.umask.Load())
}

// SetUmask updates the Session's file mode creation mask without altering the
// host process's OS-global umask.
func (s *Session) SetUmask(mask int) error {
	if mask < 0 || mask > 0o777 {
		return fmt.Errorf("unish: invalid umask %04o", mask)
	}
	s.umask.Store(int32(mask))
	return nil
}

// SetCwd changes the Session's working directory (updating PWD and OLDPWD while
// preserving $?) without calling os.Chdir. If a CapabilityHook/Jail is active
// on the Session, OpChdir is enforced before changing directories.
func (s *Session) SetCwd(dir string) error {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	if s.isClosed() {
		return ErrSessionClosed
	}
	return s.setCwdLocked(context.Background(), dir)
}

func (s *Session) setCwdLocked(ctx context.Context, dir string) error {
	if strings.TrimSpace(dir) == "" {
		return &os.PathError{Op: "chdir", Path: dir, Err: os.ErrInvalid}
	}
	expanded := expandTildePath(dir)
	resolved := resolveHostPath(s.runner.Dir, expanded)
	if !filepath.IsAbs(resolved) {
		if abs, err := filepath.Abs(resolved); err == nil {
			resolved = abs
		}
	}
	checkCtx := ctx
	if s.hook != nil {
		checkCtx = WithCapabilityHook(checkCtx, s.hook)
	}
	checked, err := checkChdir(checkCtx, s.runner.Dir, "cd", resolved)
	if err != nil {
		return err
	}
	realDir, _ := canonicalizeForCheck(checked)
	fi, err := os.Stat(realDir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return &os.PathError{Op: "chdir", Path: dir, Err: syscall.ENOTDIR}
	}

	oldPwd := s.runner.Dir
	s.runner.Dir = realDir
	s.storeCwd(realDir)
	_ = s.setVarLocked("OLDPWD", oldPwd, true)
	_ = s.setVarLocked("PWD", realDir, true)
	return nil
}

// GetVar retrieves a shell or exported variable by name from the Session.
func (s *Session) GetVar(key string) (string, bool) {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	if s.runner == nil || s.runner.Vars == nil {
		return "", false
	}
	vr, ok := s.runner.Vars[key]
	if !ok || !vr.IsSet() {
		return "", false
	}
	return vr.String(), true
}

// Vars returns a snapshot of all set variables (shell and exported) in the Session.
func (s *Session) Vars() map[string]string {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	out := make(map[string]string, len(s.runner.Vars))
	for k, vr := range s.runner.Vars {
		if vr.IsSet() {
			out[k] = vr.String()
		}
	}
	return out
}

// Env returns a sorted slice of KEY=VALUE strings for all exported variables in
// the Session.
func (s *Session) Env() []string {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	var keys []string
	for k, vr := range s.runner.Vars {
		if vr.IsSet() && vr.Exported {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+s.runner.Vars[k].String())
	}
	return out
}

// SetVar sets a shell or exported variable in the Session while preserving $?.
func (s *Session) SetVar(key, val string, exported bool) error {
	if !syntax.ValidName(key) {
		return fmt.Errorf("unish: invalid variable name %q", key)
	}
	s.execMu.Lock()
	defer s.execMu.Unlock()
	if s.isClosed() {
		return ErrSessionClosed
	}
	return s.setVarLocked(key, val, exported)
}

// UnsetVar unsets a variable in the Session while preserving $?.
func (s *Session) UnsetVar(key string) error {
	if !syntax.ValidName(key) {
		return fmt.Errorf("unish: invalid variable name %q", key)
	}
	s.execMu.Lock()
	defer s.execMu.Unlock()
	if s.isClosed() {
		return ErrSessionClosed
	}
	saved := int(s.lastExit.Load())
	s.inInternalOp = true
	s.savedExit = saved
	defer func() {
		s.inInternalOp = false
	}()

	unsetStmt := &syntax.Stmt{
		Cmd: &syntax.CallExpr{
			Args: []*syntax.Word{
				litWord("unset"),
				litWord("-v"),
				litWord(key),
			},
		},
	}
	s.stdinProxy.setReader(nil)
	s.stdoutProxy.reset(nil)
	s.stderrProxy.reset(nil)
	clear(s.runner.Vars)
	_ = s.runner.Run(context.Background(), unsetStmt)
	s.restoreExitLocked(saved)
	return nil
}

func (s *Session) setVarLocked(key, val string, exported bool) error {
	saved := int(s.lastExit.Load())
	s.inInternalOp = true
	s.savedExit = saved
	defer func() {
		s.inInternalOp = false
	}()

	s.stdinProxy.setReader(nil)
	s.stdoutProxy.reset(nil)
	s.stderrProxy.reset(nil)

	assign := &syntax.Assign{
		Name: &syntax.Lit{Value: key},
		Value: &syntax.Word{
			Parts: []syntax.WordPart{&syntax.SglQuoted{Value: val}},
		},
	}

	if exported {
		stmt := &syntax.Stmt{
			Cmd: &syntax.DeclClause{
				Variant: &syntax.Lit{Value: "export"},
				Args:    []*syntax.Assign{assign},
			},
		}
		clear(s.runner.Vars)
		if err := s.runner.Run(context.Background(), stmt); err != nil {
			s.restoreExitLocked(saved)
			return err
		}
	} else {
		if prev, ok := s.runner.Vars[key]; ok && prev.Exported {
			unsetStmt := &syntax.Stmt{
				Cmd: &syntax.CallExpr{
					Args: []*syntax.Word{
						litWord("unset"),
						litWord("-v"),
						litWord(key),
					},
				},
			}
			clear(s.runner.Vars)
			_ = s.runner.Run(context.Background(), unsetStmt)
		}
		stmt := &syntax.Stmt{
			Cmd: &syntax.CallExpr{
				Assigns: []*syntax.Assign{assign},
			},
		}
		clear(s.runner.Vars)
		if err := s.runner.Run(context.Background(), stmt); err != nil {
			s.restoreExitLocked(saved)
			return err
		}
	}

	s.restoreExitLocked(saved)
	return nil
}

func (s *Session) restoreExitLocked(saved int) {
	s.lastExit.Store(int32(saved))
	if saved == 0 {
		return
	}
	restoreStmt := &syntax.Stmt{
		Cmd: &syntax.CallExpr{
			Args: []*syntax.Word{litWord(internalRestoreExitCmd)},
		},
	}
	_ = s.runner.Run(context.Background(), restoreStmt)
}

func litWord(v string) *syntax.Word {
	return &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: v}}}
}

// CancelCurrent cancels any currently executing Exec or ExecWith call on s
// without destroying the Session's variables, functions, or working directory.
// It returns true if an in-flight execution was cancelled.
func (s *Session) CancelCurrent() bool {
	s.cancelMu.Lock()
	cancel := s.currentCancel
	cancelledFlag := s.cancelledByUs
	s.cancelMu.Unlock()
	if cancel == nil {
		return false
	}
	if cancelledFlag != nil {
		cancelledFlag.Store(true)
	}
	cancel()
	return true
}

// Close cancels any in-flight execution, waits for the Runner to quiesce, and
// marks the Session closed. Close is idempotent.
func (s *Session) Close() error {
	s.cancelMu.Lock()
	s.closed = true
	cancel := s.currentCancel
	cancelledFlag := s.cancelledByUs
	s.cancelMu.Unlock()
	if cancelledFlag != nil {
		cancelledFlag.Store(true)
	}
	if cancel != nil {
		cancel()
	}
	s.execMu.Lock()
	unregisterRunnerCompletionHook(s.runner)
	unregisterRunnerEnvIsolated(s.runner)
	s.stdinProxy.setReader(nil)
	s.stdoutProxy.reset(nil)
	s.stderrProxy.reset(nil)
	s.execMu.Unlock()
	return nil
}

func (s *Session) isClosed() bool {
	s.cancelMu.Lock()
	closed := s.closed
	s.cancelMu.Unlock()
	return closed
}

// Exec executes script within the Session, preserving all shell state across calls.
func (s *Session) Exec(ctx context.Context, script string) (*ExecResult, error) {
	return s.ExecWith(ctx, script, ExecOptions{})
}

// ExecWith executes script within the Session using per-call ExecOptions.
func (s *Session) ExecWith(ctx context.Context, script string, opts ExecOptions) (*ExecResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.execMu.Lock()
	defer s.execMu.Unlock()

	if s.isClosed() {
		return nil, ErrSessionClosed
	}

	start := time.Now()
	execCtx := ctx
	if s.hook != nil {
		execCtx = WithCapabilityHook(execCtx, s.hook)
	}
	execCtx = withSessionInContext(execCtx, s)
	execCtx = withChdirApprovalStore(execCtx)
	if !s.inheritEnv || len(s.scrubPatterns) > 0 {
		execCtx = withSessionEnvIsolated(execCtx)
	}
	execCtx, dc := withDenialCollector(execCtx)

	var timeoutCancel context.CancelFunc
	if opts.Timeout > 0 {
		execCtx, timeoutCancel = context.WithTimeout(execCtx, opts.Timeout)
		defer timeoutCancel()
	}

	execCtx, cancel := context.WithCancel(execCtx)
	cancelledByUs := &atomic.Bool{}

	s.cancelMu.Lock()
	if s.closed {
		s.cancelMu.Unlock()
		cancel()
		return nil, ErrSessionClosed
	}
	s.currentCancel = cancel
	s.cancelledByUs = cancelledByUs
	s.cancelMu.Unlock()

	defer func() {
		s.cancelMu.Lock()
		s.currentCancel = nil
		s.cancelledByUs = nil
		s.cancelMu.Unlock()
		cancel()
		s.stdinProxy.setReader(nil)
		s.stdoutProxy.reset(nil)
		s.stderrProxy.reset(nil)
	}()

	if opts.WorkDir != "" {
		if err := s.setCwdLocked(execCtx, opts.WorkDir); err != nil {
			res := &ExecResult{
				ExitCode: 1,
				Stderr:   fmt.Sprintf("cd: %v\n", err),
				WorkDir:  s.runner.Dir,
				Duration: time.Since(start),
				Denials:  dc.snapshot(),
			}
			s.lastExit.Store(1)
			return res, nil
		}
	}

	if len(opts.Env) > 0 {
		keys := make([]string, 0, len(opts.Env))
		for k := range opts.Env {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if err := s.setVarLocked(k, opts.Env[k], true); err != nil {
				return nil, err
			}
		}
	}

	inReader := opts.Stdin
	if inReader == nil {
		inReader = s.defaultStdin
	}
	s.stdinProxy.setReader(inReader)

	outStream := combineStreamWriters(s.defaultStdout, opts.Stdout)
	errStream := combineStreamWriters(s.defaultStderr, opts.Stderr)
	s.stdoutProxy.reset(outStream)
	s.stderrProxy.reset(errStream)

	prog, parseErr := syntax.NewParser().Parse(strings.NewReader(script), "")
	if parseErr != nil {
		msg := parseErr.Error() + "\n"
		_, _ = s.stderrProxy.WriteString(msg)
		s.lastExit.Store(2)
		return &ExecResult{
			ExitCode: 2,
			Stdout:   s.stdoutProxy.String(),
			Stderr:   s.stderrProxy.String(),
			WorkDir:  s.runner.Dir,
			Duration: time.Since(start),
			Denials:  dc.snapshot(),
		}, parseErr
	}

	var runErr error
	exitCode := 0
	if len(prog.Stmts) == 0 {
		exitCode = 0
	} else {
		for _, stmt := range prog.Stmts {
			if execCtx.Err() != nil {
				runErr = execCtx.Err()
				break
			}
			clear(s.runner.Vars)
			runErr = s.runner.Run(execCtx, stmt)
			s.storeCwd(s.runner.Dir)
			if s.runner.Exited() || execCtx.Err() != nil {
				break
			}
		}
		exitCode, runErr = classifyStmtOutcome(runErr, execCtx, cancelledByUs.Load())
		s.lastExit.Store(int32(exitCode))
	}
	s.storeCwd(s.runner.Dir)

	res := &ExecResult{
		ExitCode: exitCode,
		Stdout:   s.stdoutProxy.String(),
		Stderr:   s.stderrProxy.String(),
		WorkDir:  s.runner.Dir,
		Duration: time.Since(start),
		Denials:  dc.snapshot(),
	}
	return res, runErr
}

func classifyStmtOutcome(err error, execCtx context.Context, cancelledByUs bool) (int, error) {
	if cancelledByUs {
		return 130, context.Canceled
	}
	if ctxErr := execCtx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return 124, context.DeadlineExceeded
		}
		return 130, context.Canceled
	}
	if err == nil {
		return 0, nil
	}
	var es interp.ExitStatus
	if errors.As(err, &es) {
		return int(es), nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 124, context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return 130, context.Canceled
	}
	return 1, nil
}

// --- Session Runner Handlers ---

func (s *Session) openHandler(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	if isWriteOnlyDevNull(path, flag) {
		return sharedDevNull, nil
	}
	mc := interp.HandlerCtx(ctx)
	maskedPerm := perm &^ os.FileMode(s.umask.Load())
	f, err := fsOpenFile(withApprovalMemo(ctx), mc.Dir, "open", path, flag, maskedPerm)
	if err != nil {
		return nil, ensurePathError("open", path, err)
	}
	return f, nil
}

func ensurePathError(op, path string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if _, ok := err.(*os.PathError); ok {
		return err
	}
	return &os.PathError{Op: op, Path: path, Err: err}
}

func (s *Session) readDirHandler(ctx context.Context, path string) ([]fs.DirEntry, error) {
	hc := interp.HandlerCtx(ctx)
	return fsReadDir(ctx, hc.Dir, "glob", path)
}

type chdirApprovalStoreKey struct{}

type chdirTicket struct {
	statRem   int
	accessRem int
}

type chdirApprovalStore struct {
	mu      sync.Mutex
	tickets map[string]*chdirTicket
}

func withChdirApprovalStore(ctx context.Context) context.Context {
	return context.WithValue(ctx, chdirApprovalStoreKey{}, &chdirApprovalStore{})
}

func grantChdirApprovalTicket(ctx context.Context, normPath string) {
	if ctx == nil || normPath == "" {
		return
	}
	st, ok := ctx.Value(chdirApprovalStoreKey{}).(*chdirApprovalStore)
	if !ok || st == nil {
		return
	}
	st.mu.Lock()
	if st.tickets == nil {
		st.tickets = make(map[string]*chdirTicket)
	}
	tk := st.tickets[normPath]
	if tk == nil {
		tk = &chdirTicket{}
		st.tickets[normPath] = tk
	}
	tk.statRem++
	tk.accessRem++
	st.mu.Unlock()
}

func consumeChdirStatApproval(ctx context.Context, normPath string) bool {
	if ctx == nil || normPath == "" {
		return false
	}
	st, ok := ctx.Value(chdirApprovalStoreKey{}).(*chdirApprovalStore)
	if !ok || st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	tk := st.tickets[normPath]
	if tk == nil || tk.statRem <= 0 {
		return false
	}
	tk.statRem--
	if tk.statRem <= 0 && tk.accessRem <= 0 {
		delete(st.tickets, normPath)
	}
	return true
}

func consumeChdirAccessApproval(ctx context.Context, normPath string) bool {
	if ctx == nil || normPath == "" {
		return false
	}
	st, ok := ctx.Value(chdirApprovalStoreKey{}).(*chdirApprovalStore)
	if !ok || st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	tk := st.tickets[normPath]
	if tk == nil || tk.accessRem <= 0 {
		return false
	}
	tk.accessRem--
	if tk.statRem <= 0 && tk.accessRem <= 0 {
		delete(st.tickets, normPath)
	}
	return true
}

type sessionEnvIsolatedKey struct{}

func withSessionEnvIsolated(ctx context.Context) context.Context {
	return context.WithValue(ctx, sessionEnvIsolatedKey{}, true)
}

func isSessionEnvIsolated(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(sessionEnvIsolatedKey{}).(bool)
	return v
}

func cmdSessionAwarePrintenv(ctx context.Context, hc interp.HandlerContext, args []string) error {
	if isSessionEnvIsolated(ctx) {
		return runIsolatedPrintenv(hc, args)
	}
	return cmdPrintenv(ctx, hc, args)
}

func runIsolatedPrintenv(hc interp.HandlerContext, args []string) error {
	sep := "\n"
	var keys []string
	endOpts := false
	for _, a := range args[1:] {
		if !endOpts {
			if a == "--" {
				endOpts = true
				continue
			}
			if a == "-0" || a == "--null" {
				sep = "\x00"
				continue
			}
		}
		keys = append(keys, a)
	}
	if len(keys) == 0 {
		hc.Env.Each(func(name string, v expand.Variable) bool {
			if v.IsSet() && v.Exported {
				fmt.Fprintf(hc.Stdout, "%s=%s%s", name, v.Str, sep)
			}
			return true
		})
		return nil
	}
	code := 0
	for _, k := range keys {
		if v := hc.Env.Get(k); v.IsSet() && v.Exported {
			fmt.Fprintf(hc.Stdout, "%s%s", v.Str, sep)
		} else {
			code = 1
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func (s *Session) statHandler(ctx context.Context, path string, followSymlinks bool) (fs.FileInfo, error) {
	hc := interp.HandlerCtx(ctx)
	if testDevNodes[strings.ToLower(path)] {
		if fi, err := os.Stat(resolveHostPath(hc.Dir, path)); err == nil {
			return fi, nil
		}
		return synthDevFileInfo{name: filepath.Base(path)}, nil
	}
	realP, _ := canonicalizeForCheck(resolveHostPath(hc.Dir, path))
	if consumeChdirStatApproval(ctx, normalizeMatchPath(realP)) {
		if !followSymlinks {
			return os.Lstat(realP)
		}
		return os.Stat(realP)
	}
	if !followSymlinks {
		return fsLstat(ctx, hc.Dir, "stat", path)
	}
	return fsStat(ctx, hc.Dir, "stat", path)
}

func (s *Session) accessHandler(ctx context.Context, path string, mode interp.AccessMode) error {
	if CapabilityHookFromContext(ctx) == nil {
		return interp.DefaultAccessHandler()(ctx, path, mode)
	}
	hc := interp.HandlerCtx(ctx)
	if mode == interp.AccessExec {
		realP, _ := canonicalizeForCheck(resolveHostPath(hc.Dir, path))
		if consumeChdirAccessApproval(ctx, normalizeMatchPath(realP)) {
			return interp.DefaultAccessHandler()(ctx, realP, mode)
		}
	}

	ctx = withApprovalMemo(ctx)
	var checkedPath string
	var err error
	if mode&interp.AccessRead != 0 {
		checkedPath, err = checkFS(ctx, hc.Dir, "access", path, OpRead)
		if err != nil {
			return err
		}
	}
	if mode&interp.AccessWrite != 0 {
		checkedPath, err = checkFS(ctx, hc.Dir, "access", path, OpWrite)
		if err != nil {
			return err
		}
	}
	if mode&interp.AccessExec != 0 {
		fi, serr := fsStat(ctx, hc.Dir, "access", path)
		if serr != nil {
			return serr
		}
		if fi.IsDir() {
			checkedPath, err = checkChdir(ctx, hc.Dir, "cd", path)
			if err != nil {
				return err
			}
		} else {
			checkedPath = resolveHostPath(hc.Dir, path)
		}
	}
	if checkedPath == "" {
		checkedPath, err = checkFS(ctx, hc.Dir, "access", path, OpStat)
		if err != nil {
			return err
		}
	}
	return interp.DefaultAccessHandler()(ctx, checkedPath, mode)
}

func (s *Session) callHandler(ctx context.Context, args []string) ([]string, error) {
	if len(args) == 0 {
		return args, nil
	}

	if s.inInternalOp && len(args) == 1 && args[0] == internalRestoreExitCmd {
		return nil, interp.CallHandled(uint8(s.savedExit))
	}

	hc := interp.HandlerCtx(ctx)
	switch args[0] {
	case "umask":
		return s.handleSessionUmaskCall(hc, args)

	case "ulimit":
		return s.handleSessionUlimitCall(ctx, hc, args)

	case "history":
		return s.handleSessionHistoryCall(ctx, hc, args)

	case "cd", "pushd":
		return s.handleSessionCdCall(ctx, hc, args, nil)

	case "popd":
		return args, nil

	case "source", ".":
		return s.handleSessionSourceCall(ctx, hc, args, nil)

	case "kill":
		return s.handleSessionKill(ctx, hc, args)

	case "type":
		if CapabilityHookFromContext(ctx) != nil {
			return s.handleJailedTypeCall(ctx, hc, args)
		}
		return callOverride(ctx, args)

	case "hash":
		if CapabilityHookFromContext(ctx) != nil {
			return s.handleJailedHashCall(ctx, hc, args)
		}
		return callOverride(ctx, args)

	case "test", "[":
		// When a CapabilityHook is active, let mvdan/sh's builtin test/[ run
		// through s.statHandler and s.accessHandler so every file probe is
		// capability-gated uniformly.
		if CapabilityHookFromContext(ctx) != nil {
			return args, nil
		}
		return callOverride(ctx, args)

	case "builtin":
		if len(args) >= 2 {
			switch args[1] {
			case "umask":
				return s.handleSessionUmaskCall(hc, args[1:])
			case "ulimit":
				return s.handleSessionUlimitCall(ctx, hc, args[1:])
			case "history":
				return s.handleSessionHistoryCall(ctx, hc, args[1:])
			case "cd", "pushd":
				return s.handleSessionCdCall(ctx, hc, args[1:], args[:1])
			case "source", ".":
				return s.handleSessionSourceCall(ctx, hc, args[1:], args[:1])
			case "kill":
				return s.handleSessionKill(ctx, hc, args[1:])
			case "type":
				if CapabilityHookFromContext(ctx) != nil {
					return s.handleJailedTypeCall(ctx, hc, args[1:])
				}
			case "hash":
				if CapabilityHookFromContext(ctx) != nil {
					return s.handleJailedHashCall(ctx, hc, args[1:])
				}
			case "test", "[":
				if CapabilityHookFromContext(ctx) != nil {
					return args, nil
				}
			}
		}

	case "command":
		if CapabilityHookFromContext(ctx) != nil {
			if handled, code := s.handleJailedCommandShow(ctx, hc, args); handled {
				return nil, interp.CallHandled(code)
			}
		}
		if prefix, sub, ok := splitPlainCommandBuiltin(args); ok {
			switch sub[0] {
			case "umask":
				return s.handleSessionUmaskCall(hc, sub)
			case "ulimit":
				return s.handleSessionUlimitCall(ctx, hc, sub)
			case "history":
				return s.handleSessionHistoryCall(ctx, hc, sub)
			case "cd", "pushd":
				return s.handleSessionCdCall(ctx, hc, sub, prefix)
			case "source", ".":
				return s.handleSessionSourceCall(ctx, hc, sub, prefix)
			case "kill":
				return s.handleSessionKill(ctx, hc, sub)
			case "type":
				if CapabilityHookFromContext(ctx) != nil {
					return s.handleJailedTypeCall(ctx, hc, sub)
				}
			case "hash":
				if CapabilityHookFromContext(ctx) != nil {
					return s.handleJailedHashCall(ctx, hc, sub)
				}
			case "test", "[":
				if CapabilityHookFromContext(ctx) != nil {
					return args, nil
				}
			}
		}
	}

	return callOverride(ctx, args)
}

func (s *Session) handleSessionUmaskCall(hc interp.HandlerContext, args []string) ([]string, error) {
	if err := s.runSessionUmask(hc, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, interp.CallHandled(2)
		}
		return nil, interp.CallHandled(1)
	}
	return nil, interp.CallHandled(0)
}

func (s *Session) handleSessionUlimitCall(ctx context.Context, hc interp.HandlerContext, args []string) ([]string, error) {
	endOpts := false
	hasSetArg := false
	for _, a := range args[1:] {
		if !endOpts {
			if a == "--" {
				endOpts = true
				continue
			}
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				continue
			}
		}
		hasSetArg = true
		break
	}
	if hasSetArg {
		if CapabilityHookFromContext(ctx) != nil {
			if cerr := checkProc(ctx, "ulimit", os.Getpid(), "RLIMIT"); cerr != nil {
				fmt.Fprintf(hc.Stderr, "ulimit: %v\n", cerr)
				return nil, interp.CallHandled(1)
			}
		}
		fmt.Fprintln(hc.Stderr, "ulimit: cannot modify host process limit in embedded session: Operation not permitted")
		return nil, interp.CallHandled(1)
	}
	return callOverride(ctx, args)
}

func (s *Session) handleSessionHistoryCall(ctx context.Context, hc interp.HandlerContext, args []string) ([]string, error) {
	// Never read or mutate process-global replHist from an embedded/jailed Session.
	argv := args[1:]
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			break
		}
		if (a == "-r" || a == "-n") && CapabilityHookFromContext(ctx) != nil {
			target := hc.Env.Get("HISTFILE").String()
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
				target = argv[i]
			}
			if target != "" {
				if _, err := checkFS(ctx, hc.Dir, "history", target, OpRead); err != nil {
					fmt.Fprintf(hc.Stderr, "history: %v\n", err)
					return nil, interp.CallHandled(1)
				}
			}
			continue
		}
		if (a == "-w" || a == "-a") && CapabilityHookFromContext(ctx) != nil {
			target := hc.Env.Get("HISTFILE").String()
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
				target = argv[i]
			}
			if target != "" {
				if _, err := checkFS(ctx, hc.Dir, "history", target, OpWrite); err != nil {
					fmt.Fprintf(hc.Stderr, "history: %v\n", err)
					return nil, interp.CallHandled(1)
				}
			}
			continue
		}
		if a == "-d" {
			if i+1 >= len(argv) {
				fmt.Fprintln(hc.Stderr, "history: -d: option requires an argument")
				return nil, interp.CallHandled(2)
			}
			i++
		}
	}
	return nil, interp.CallHandled(0)
}

func (s *Session) handleSessionSourceCall(ctx context.Context, hc interp.HandlerContext, srcArgs []string, prefix []string) ([]string, error) {
	if CapabilityHookFromContext(ctx) == nil || len(srcArgs) < 2 {
		if len(prefix) == 0 {
			return srcArgs, nil
		}
		return append(slices.Clone(prefix), srcArgs...), nil
	}
	idx := 1
	if srcArgs[1] == "--" {
		if len(srcArgs) < 3 {
			return append(slices.Clone(prefix), srcArgs...), nil
		}
		idx = 2
	}
	name := srcArgs[idx]
	if name != "" && !strings.ContainsAny(name, `/\`) && !filepath.IsAbs(name) {
		resolved := resolveHostPath(hc.Dir, name)
		if p, ok := s.searchPathFileJailed(ctx, hc, srcArgs[0], name, false); ok {
			resolved = p
		}
		rewritten := slices.Clone(srcArgs)
		rewritten[idx] = resolved
		srcArgs = rewritten
	}
	if len(prefix) == 0 {
		return srcArgs, nil
	}
	out := make([]string, 0, len(prefix)+len(srcArgs))
	out = append(out, prefix...)
	out = append(out, srcArgs...)
	return out, nil
}

func (s *Session) handleJailedCommandShow(ctx context.Context, hc interp.HandlerContext, args []string) (bool, uint8) {
	showMode := byte(0) // 'v' or 'V'
	endOpts := false
	var operands []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		if !endOpts {
			if a == "--" {
				endOpts = true
				continue
			}
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				for j := 1; j < len(a); j++ {
					switch a[j] {
					case 'v':
						showMode = 'v'
					case 'V':
						showMode = 'V'
					case 'p':
						// accepted
					default:
						return false, 0
					}
				}
				continue
			}
		}
		operands = append(operands, a)
	}
	if showMode == 0 {
		return false, 0
	}
	if len(operands) == 0 {
		return true, 0
	}
	var last uint8
	for _, q := range operands {
		switch {
		case !strings.ContainsAny(q, `/\`) && hc.HasFunc(q):
			if showMode == 'V' {
				fmt.Fprintf(hc.Stdout, "%s is a function\n", q)
			} else {
				fmt.Fprintln(hc.Stdout, q)
			}
		case !strings.ContainsAny(q, `/\`) && (interp.IsBuiltin(q) || lookupExtra(q) != nil):
			if showMode == 'V' {
				fmt.Fprintf(hc.Stdout, "%s is a shell builtin\n", q)
			} else {
				fmt.Fprintln(hc.Stdout, q)
			}
		default:
			if p, err := s.lookPathJailed(ctx, hc, "command", q); err == nil {
				if showMode == 'V' {
					fmt.Fprintf(hc.Stdout, "%s is %s\n", q, p)
				} else {
					fmt.Fprintln(hc.Stdout, p)
				}
			} else {
				if showMode == 'V' {
					fmt.Fprintf(hc.Stderr, "command: %s: not found\n", q)
				}
				last = 1
			}
		}
	}
	return true, last
}

func (s *Session) handleJailedTypeCall(ctx context.Context, hc interp.HandlerContext, args []string) ([]string, error) {
	mode := ""
	all := false
	noFuncs := false
	var names []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			names = append(names, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 't':
					mode = "-t"
				case 'p':
					mode = "-p"
				case 'P':
					mode = "-P"
				case 'a':
					all = true
				case 'f':
					noFuncs = true
				default:
					fmt.Fprintf(hc.Stderr, "type: -%c: invalid option\n", a[j])
					return nil, interp.CallHandled(2)
				}
			}
			continue
		}
		names = append(names, a)
	}
	if len(names) == 0 {
		return nil, interp.CallHandled(0)
	}
	code := uint8(0)
	for _, n := range names {
		matched := false
		if mode != "-P" {
			if !noFuncs && !strings.ContainsAny(n, `/\`) && hc.HasFunc(n) {
				matched = true
				switch mode {
				case "-t":
					fmt.Fprintln(hc.Stdout, "function")
				case "-p":
				default:
					fmt.Fprintf(hc.Stdout, "%s is a function\n", n)
				}
				if !all {
					continue
				}
			}
			if !strings.ContainsAny(n, `/\`) && interp.IsBuiltin(n) {
				matched = true
				switch mode {
				case "-t":
					fmt.Fprintln(hc.Stdout, "builtin")
				case "-p":
				default:
					fmt.Fprintf(hc.Stdout, "%s is a shell builtin\n", n)
				}
				if !all {
					continue
				}
			}
			if !strings.ContainsAny(n, `/\`) && lookupExtra(n) != nil {
				matched = true
				switch mode {
				case "-t":
					fmt.Fprintln(hc.Stdout, "file")
				case "-p":
					fmt.Fprintf(hc.Stdout, "/usr/bin/%s\n", n)
				default:
					fmt.Fprintf(hc.Stdout, "%s is /usr/bin/%s\n", n, n)
				}
				if !all {
					continue
				}
			}
		}
		if p, err := s.lookPathJailed(ctx, hc, "type", n); err == nil {
			matched = true
			switch mode {
			case "-t":
				fmt.Fprintln(hc.Stdout, "file")
			case "-p", "-P":
				fmt.Fprintln(hc.Stdout, p)
			default:
				fmt.Fprintf(hc.Stdout, "%s is %s\n", n, p)
			}
		}
		if !matched {
			if mode == "" {
				fmt.Fprintf(hc.Stderr, "type: %s: not found\n", n)
			}
			code = 1
		}
	}
	return nil, interp.CallHandled(code)
}

func (s *Session) handleJailedHashCall(ctx context.Context, hc interp.HandlerContext, args []string) ([]string, error) {
	var names []string
	forget := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-r":
		case "-d", "-rd", "-dr":
			forget = true
		case "--":
			names = append(names, args[i+1:]...)
			i = len(args)
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				fmt.Fprintf(hc.Stderr, "hash: invalid option -- '%s'\n", strings.TrimPrefix(a, "-"))
				return nil, interp.CallHandled(2)
			}
			names = append(names, a)
		}
	}
	if forget && len(names) == 0 {
		fmt.Fprintln(hc.Stderr, "hash: -d: option requires an argument")
		return nil, interp.CallHandled(2)
	}
	if forget {
		return nil, interp.CallHandled(0)
	}
	if len(names) == 0 {
		fmt.Fprintln(hc.Stdout, "hash: hash table empty")
		return nil, interp.CallHandled(0)
	}
	for _, n := range names {
		if interp.IsBuiltin(n) {
			continue
		}
		if lookupExtra(n) != nil {
			fmt.Fprintf(hc.Stderr, "hash: %s: not found\n", n)
			return nil, interp.CallHandled(1)
		}
		if _, err := s.lookPathJailed(ctx, hc, "hash", n); err != nil {
			fmt.Fprintf(hc.Stderr, "hash: %s: not found\n", n)
			return nil, interp.CallHandled(1)
		}
	}
	return nil, interp.CallHandled(0)
}

func (s *Session) handleSessionCdCall(ctx context.Context, hc interp.HandlerContext, cdArgs []string, prefix []string) ([]string, error) {
	if runtime.GOOS == "windows" {
		cdArgs = rewriteWindowsMSYSCdArgs(cdArgs)
	}
	if CapabilityHookFromContext(ctx) != nil {
		if target, ok := resolveCdTargetArg(hc, cdArgs); ok {
			checked, err := checkChdir(withApprovalMemo(ctx), hc.Dir, cdArgs[0], target)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "%s: %v\n", cdArgs[0], err)
				return nil, interp.CallHandled(1)
			}
			realDir, _ := canonicalizeForCheck(checked)
			if fi, serr := os.Stat(realDir); serr == nil && fi.IsDir() {
				grantChdirApprovalTicket(ctx, normalizeMatchPath(realDir))
			}
			if cdArgs[0] == "pushd" {
				cdArgs = rewriteFirstDirOperand(cdArgs, resolveHostPath(hc.Dir, target))
			}
		}
	}
	if len(prefix) == 0 {
		return cdArgs, nil
	}
	out := make([]string, 0, len(prefix)+len(cdArgs))
	out = append(out, prefix...)
	out = append(out, cdArgs...)
	return out, nil
}

func rewriteFirstDirOperand(args []string, replacement string) []string {
	endOpts := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		if !endOpts {
			if a == "--" {
				endOpts = true
				continue
			}
			if strings.HasPrefix(a, "-") && a != "-" {
				continue
			}
		}
		copied := slices.Clone(args)
		copied[i] = replacement
		return copied
	}
	return args
}

func splitPlainCommandBuiltin(args []string) (prefix []string, sub []string, ok bool) {
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 >= len(args) {
				return nil, nil, false
			}
			return args[:i+1], args[i+1:], true
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			for _, ch := range a[1:] {
				if ch == 'v' || ch == 'V' {
					return nil, nil, false
				}
			}
			continue
		}
		return args[:i], args[i:], true
	}
	return nil, nil, false
}

func rewriteWindowsMSYSCdArgs(args []string) []string {
	var copied []string
	endOpts := false
	rewrote := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		if !endOpts {
			if a == "--" {
				endOpts = true
				continue
			}
			if strings.HasPrefix(a, "-") && a != "-" {
				continue
			}
		}
		if !rewrote && a != "" && a != "-" && (a[0] == '/' || a[0] == '\\') {
			if w, ok := msysPath(a); ok {
				if copied == nil {
					copied = slices.Clone(args)
				}
				copied[i] = w
				rewrote = true
			}
		}
		rewrote = true
	}
	if copied != nil {
		return copied
	}
	return args
}

func resolveCdTargetArg(hc interp.HandlerContext, args []string) (string, bool) {
	var operands []string
	endOpts := false
	for _, a := range args[1:] {
		if !endOpts {
			if a == "--" {
				endOpts = true
				continue
			}
			if strings.HasPrefix(a, "-") && a != "-" {
				continue
			}
		}
		operands = append(operands, a)
	}
	if len(operands) == 0 {
		if args[0] == "pushd" {
			return "", false
		}
		home := hc.Env.Get("HOME").String()
		if home == "" && runtime.GOOS == "windows" {
			home = hc.Env.Get("USERPROFILE").String()
		}
		if home == "" {
			return "", false
		}
		return home, true
	}
	target := operands[0]
	if target == "-" {
		old := hc.Env.Get("OLDPWD").String()
		if old == "" {
			return "", false
		}
		return old, true
	}
	return target, true
}

func (s *Session) handleSessionKill(ctx context.Context, hc interp.HandlerContext, args []string) ([]string, error) {
	code, exitShell := s.runSessionKill(ctx, hc, args)
	if exitShell {
		return nil, interp.Exit(code)
	}
	return nil, interp.CallHandled(uint8(code))
}

func (s *Session) runSessionKill(ctx context.Context, hc interp.HandlerContext, args []string) (int, bool) {
	kargs := splitAttached("kill", args)
	if len(kargs) > 1 && (kargs[1] == "-l" || kargs[1] == "-L" || kargs[1] == "--list") {
		if origKillExtraMain != nil {
			hc.PushActive()
			err := origKillExtraMain(interp.WithHandlerContext(ctx, hc), hc, kargs)
			hc.PopActive()
			if err != nil {
				return 1, false
			}
			return 0, false
		}
		fmt.Fprintln(hc.Stdout, "HUP INT QUIT ILL TRAP ABRT BUS FPE KILL USR1 SEGV USR2 PIPE ALRM TERM")
		return 0, false
	}

	sig := "TERM"
	sawSigFlag := false
	endOpts := false
	var operands []string

	for i := 1; i < len(kargs); i++ {
		a := kargs[i]
		if !endOpts {
			if a == "--" {
				endOpts = true
				continue
			}
			if a == "-s" || a == "-n" {
				if i+1 >= len(kargs) {
					fmt.Fprintf(hc.Stderr, "kill: %s: option requires an argument\n", a)
					return 1, false
				}
				i++
				raw := kargs[i]
				name := strings.TrimPrefix(strings.ToUpper(raw), "SIG")
				if a == "-n" && !isValidSessionKillSigNum(name) {
					fmt.Fprintf(hc.Stderr, "kill: %s: invalid signal specification\n", raw)
					return 1, false
				}
				if a == "-s" && !isValidSessionKillSig(name) {
					fmt.Fprintf(hc.Stderr, "kill: %s: invalid signal specification\n", raw)
					return 1, false
				}
				sig = name
				sawSigFlag = true
				continue
			}
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				body := strings.TrimPrefix(a, "-")
				name := strings.TrimPrefix(strings.ToUpper(body), "SIG")
				if allASCIIUint(name) {
					if !sawSigFlag {
						if isValidSessionKillSigNum(name) {
							sig = name
							sawSigFlag = true
							continue
						}
						fmt.Fprintf(hc.Stderr, "kill: %s: invalid signal specification\n", body)
						return 1, false
					}
					// Once a signal flag was already given (e.g. `kill -9 -1`),
					// a negative integer is a process-group/broadcast PID operand.
				} else if a[1] < '0' || a[1] > '9' {
					if !isValidSessionKillSig(name) {
						fmt.Fprintf(hc.Stderr, "kill: %s: invalid signal specification\n", body)
						return 1, false
					}
					sig = name
					sawSigFlag = true
					continue
				}
			}
		}
		operands = append(operands, a)
	}

	if len(operands) == 0 {
		fmt.Fprintln(hc.Stderr, "kill: missing operand")
		return 1, false
	}

	hook := CapabilityHookFromContext(ctx)
	code := 0
	myPID := os.Getpid()

	for _, rawOp := range operands {
		op := strings.TrimSpace(rawOp)
		if pid, err := strconv.Atoi(op); err == nil {
			// 1. Self-PID (including non-canonical forms like 0$$ or +$$):
			// NEVER call os.FindProcess / killProc / os.Exit on the host Go process!
			if pid == myPID {
				if sig == "0" {
					continue
				}
				if sig == "9" || sig == "KILL" {
					return 137, true
				}
				if sig == "STOP" {
					return 128 + 19, true
				}
				hc.PushActive()
				handled := queueSelfSignal(sig)
				hc.PopActive()
				if handled {
					continue
				}
				if ec := sessionFatalSelfExitCode(sig); ec > 0 {
					return ec, true
				}
				continue
			}

			// 2. Broadcast / process-group PIDs (0, -1, -pgid): never broadcast OS
			// signals to the embedding host process group.
			if pid <= 0 {
				if hook != nil {
					if cerr := checkProc(ctx, "kill", pid, sig); cerr != nil {
						fmt.Fprintf(hc.Stderr, "kill: %v\n", cerr)
						code = 1
						continue
					}
				}
				fmt.Fprintf(hc.Stderr, "kill: (%d) - Operation not permitted\n", pid)
				code = 1
				continue
			}

			// 3. Positive PID: exempt live tracked child jobs; dead/reaped jobs fall
			// through to checkProc so OS-reused PIDs cannot bypass OpProc.
			if j := globalJobs.byPID(pid); j != nil && j.alive() && j.proc != nil {
				if err := killProc(j.proc, sig); err != nil {
					fmt.Fprintf(hc.Stderr, "kill: (%d) - %v\n", pid, err)
					code = 1
				}
				continue
			}

			// External host PID: gate with OpProc when hooked (single prompt), then
			// signal directly via os.FindProcess + killProc.
			if hook != nil {
				if cerr := checkProc(ctx, "kill", pid, sig); cerr != nil {
					fmt.Fprintf(hc.Stderr, "kill: %v\n", cerr)
					code = 1
					continue
				}
			}
			proc, ferr := os.FindProcess(pid)
			if ferr != nil {
				fmt.Fprintf(hc.Stderr, "kill: (%d) - No such process\n", pid)
				code = 1
				continue
			}
			if kerr := killProc(proc, sig); kerr != nil {
				fmt.Fprintf(hc.Stderr, "kill: (%d) - %v\n", pid, kerr)
				code = 1
			}
			continue
		}

		// 4. Jobspec (%1, %%, %+) or fake background ID (g1): require live job!
		if j := globalJobs.resolveJobSpec(op); j != nil {
			if !j.alive() || j.proc == nil {
				fmt.Fprintf(hc.Stderr, "kill: (%s) - No such process\n", op)
				code = 1
				continue
			}
			if err := killProc(j.proc, sig); err != nil {
				fmt.Fprintf(hc.Stderr, "kill: (%d) - %v\n", j.pid, err)
				code = 1
			}
			continue
		}
		if strings.HasPrefix(op, "%") {
			fmt.Fprintf(hc.Stderr, "kill: %s: no such job\n", op)
			code = 1
			continue
		}
		fmt.Fprintf(hc.Stderr, "kill: %s: arguments must be process or job IDs\n", op)
		code = 1
	}

	if code != 0 {
		return 1, false
	}
	return 0, false
}

func isValidSessionKillSigNum(s string) bool {
	if !allASCIIUint(s) {
		return false
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 0 && n <= 64
}

func isValidSessionKillSig(s string) bool {
	if isValidSessionKillSigNum(s) {
		return true
	}
	if isSignalName(s) {
		return true
	}
	switch s {
	case "IOT", "CLD", "POLL":
		return true
	}
	return false
}

func sessionFatalSelfExitCode(sig string) int {
	switch sig {
	case "0", "CHLD", "CLD", "CONT", "URG", "WINCH", "IO", "POLL":
		return 0
	case "HUP":
		return 128 + 1
	case "INT":
		return 128 + 2
	case "QUIT":
		return 128 + 3
	case "ILL":
		return 128 + 4
	case "TRAP":
		return 128 + 5
	case "ABRT", "IOT":
		return 128 + 6
	case "BUS":
		return 128 + 7
	case "FPE":
		return 128 + 8
	case "KILL":
		return 128 + 9
	case "USR1":
		return 128 + 10
	case "SEGV":
		return 128 + 11
	case "USR2":
		return 128 + 12
	case "PIPE":
		return 128 + 13
	case "ALRM":
		return 128 + 14
	case "TERM":
		return 128 + 15
	case "XCPU":
		return 128 + 24
	case "XFSZ":
		return 128 + 25
	case "VTALRM":
		return 128 + 26
	case "PROF":
		return 128 + 27
	case "SYS":
		return 128 + 31
	}
	if allASCIIUint(sig) {
		if n, err := strconv.Atoi(sig); err == nil && n > 0 && n <= 64 {
			return 128 + n
		}
	}
	return 128 + 15
}

func (s *Session) runSessionUmask(hc interp.HandlerContext, args []string) error {
	if len(args) == 1 {
		fmt.Fprintf(hc.Stdout, "%04o\n", s.umask.Load())
		return nil
	}
	endOpts := false
	printed := false
	var modes []string
	for _, a := range args[1:] {
		if !endOpts {
			if a == "--" {
				endOpts = true
				continue
			}
			if a == "-S" {
				printed = true
				fmt.Fprintln(hc.Stdout, umaskSymbolic(int(s.umask.Load())))
				continue
			}
			if a == "-p" {
				printed = true
				fmt.Fprintf(hc.Stdout, "umask %04o\n", s.umask.Load())
				continue
			}
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				fmt.Fprintf(hc.Stderr, "umask: invalid option -- '%s'\n", strings.TrimPrefix(a, "-"))
				return flag.ErrHelp
			}
		}
		modes = append(modes, a)
	}
	if len(modes) == 0 && !printed {
		fmt.Fprintf(hc.Stdout, "%04o\n", s.umask.Load())
		return nil
	}
	for _, a := range modes {
		v, err := strconv.ParseUint(a, 8, 32)
		if err != nil || v > 0o777 {
			fmt.Fprintf(hc.Stderr, "umask: '%s': invalid octal number\n", a)
			return exitError{1}
		}
		s.umask.Store(int32(v))
	}
	return nil
}

func (s *Session) execMiddleware(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if len(args) == 0 {
			return next(ctx, args)
		}
		hc := interp.HandlerCtx(ctx)
		if interp.IsBuiltin(args[0]) || hc.HasFunc(args[0]) {
			return next(ctx, args)
		}

		needsEnvIsolation := !s.inheritEnv || len(s.scrubPatterns) > 0
		if lookupExtra(args[0]) != nil {
			if args[0] == "kill" && !hasVersionFlag(args) {
				if hc.InBackground() {
					hc.ReportBgStart(0)
				}
				_, kerr := s.handleSessionKill(ctx, hc, args)
				return kerr
			}
			if needsEnvIsolation {
				if args[0] == "printenv" && !hasVersionFlag(args) {
					if hc.InBackground() {
						hc.ReportBgStart(0)
					}
					if err := runIsolatedPrintenv(hc, splitAttached("printenv", args)); err != nil {
						if ee, ok := err.(exitError); ok {
							return interp.NewExitStatus(uint8(ee.code))
						}
						return err
					}
					return nil
				}
				if _, alreadyExplicit := hc.Env.(explicitEnviron); !alreadyExplicit {
					hc.Env = explicitEnviron{
						Environ: hc.Env,
						pairs:   s.execChildEnv(hc),
						cleared: true,
					}
					ctx = interp.WithHandlerContext(ctx, hc)
				}
			}
			return next(ctx, args)
		}

		hook := CapabilityHookFromContext(ctx)
		if hook == nil && !needsEnvIsolation {
			return next(ctx, args)
		}

		var execTarget string
		if hook != nil {
			if strings.ContainsAny(args[0], `/\`) || filepath.IsAbs(args[0]) {
				hostCandidate := resolveHostPath(hc.Dir, args[0])
				checked, cerr := checkExec(withApprovalMemo(ctx), args[0], hostCandidate, args, hc.Dir)
				if cerr != nil {
					if errors.Is(cerr, context.Canceled) || errors.Is(cerr, context.DeadlineExceeded) {
						return cerr
					}
					fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], cerr)
					return interp.ExitStatus(126)
				}
				fi, serr := os.Stat(checked)
				if serr != nil && runtime.GOOS == "windows" && errors.Is(serr, os.ErrNotExist) {
					for _, cand := range sessionWindowsExecCandidates(hc, checked) {
						if cfi, cerr2 := os.Stat(cand); cerr2 == nil && !cfi.IsDir() {
							checked = cand
							fi = cfi
							serr = nil
							break
						}
					}
				}
				if serr != nil {
					if errors.Is(serr, os.ErrNotExist) {
						fmt.Fprintf(hc.Stderr, "%s: No such file or directory\n", args[0])
						return interp.ExitStatus(127)
					}
					fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], serr)
					return interp.ExitStatus(126)
				}
				if fi.IsDir() || (runtime.GOOS != "windows" && fi.Mode()&0o111 == 0) {
					fmt.Fprintf(hc.Stderr, "%s: Permission denied\n", args[0])
					return interp.ExitStatus(126)
				}
				execTarget = checked
			} else {
				resolvedPath, lookErr := s.lookPathJailed(ctx, hc, args[0], args[0])
				if lookErr != nil {
					fmt.Fprintf(hc.Stderr, "%s: command not found\n", args[0])
					return interp.ExitStatus(127)
				}
				checked, cerr := checkExec(withApprovalMemo(ctx), args[0], resolvedPath, args, hc.Dir)
				if cerr != nil {
					if errors.Is(cerr, context.Canceled) || errors.Is(cerr, context.DeadlineExceeded) {
						return cerr
					}
					fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], cerr)
					return interp.ExitStatus(126)
				}
				execTarget = checked
			}
		} else {
			resolvedPath, lookErr := interp.LookPathDir(hc.Dir, hc.Env, args[0])
			if lookErr != nil {
				fmt.Fprintf(hc.Stderr, "%s: command not found\n", args[0])
				return interp.ExitStatus(127)
			}
			execTarget = resolvedPath
		}

		for _, a := range args[1:] {
			waitProcSubstProducerCtx(ctx, a)
		}
		return s.runIsolatedExternal(ctx, hc, execTarget, args)
	}
}

func (s *Session) lookPathJailed(ctx context.Context, hc interp.HandlerContext, cmdName, file string) (string, error) {
	if file == "" {
		return "", os.ErrNotExist
	}
	if strings.ContainsAny(file, `/\`) || filepath.IsAbs(file) {
		for _, cand := range sessionWindowsExecCandidates(hc, resolveHostPath(hc.Dir, file)) {
			fi, err := fsStat(ctx, hc.Dir, cmdName, cand)
			if err != nil || fi.IsDir() {
				continue
			}
			if runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
				continue
			}
			return cand, nil
		}
		return "", os.ErrNotExist
	}
	if p, ok := s.searchPathFileJailed(ctx, hc, cmdName, file, true); ok {
		return p, nil
	}
	return "", os.ErrNotExist
}

func (s *Session) searchPathFileJailed(ctx context.Context, hc interp.HandlerContext, cmdName, file string, requireExec bool) (string, bool) {
	pathVal := hc.Env.Get("PATH").String()
	if pathVal == "" {
		return "", false
	}
	for _, dir := range filepath.SplitList(pathVal) {
		if dir == "" {
			dir = "."
		}
		base := filepath.Join(resolveHostPath(hc.Dir, dir), file)
		cands := []string{base}
		if requireExec {
			cands = sessionWindowsExecCandidates(hc, base)
		}
		for _, cand := range cands {
			fi, err := fsStat(ctx, hc.Dir, cmdName, cand)
			if err != nil || fi.IsDir() {
				continue
			}
			if requireExec && runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
				continue
			}
			return cand, true
		}
	}
	return "", false
}

func sessionWindowsExecCandidates(hc interp.HandlerContext, base string) []string {
	if runtime.GOOS != "windows" {
		return []string{base}
	}
	pathext := hc.Env.Get("PATHEXT").String()
	if pathext == "" {
		pathext = ".COM;.EXE;.BAT;.CMD"
	}
	var exts []string
	for _, e := range strings.Split(strings.ToLower(pathext), ";") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if e[0] != '.' {
			e = "." + e
		}
		exts = append(exts, e)
	}
	lowerBase := strings.ToLower(base)
	for _, e := range exts {
		if strings.HasSuffix(lowerBase, e) {
			return []string{base}
		}
	}
	cands := make([]string, 0, 1+len(exts))
	cands = append(cands, base)
	for _, e := range exts {
		cands = append(cands, base+e)
	}
	return cands
}

func (s *Session) runIsolatedExternal(ctx context.Context, hc interp.HandlerContext, binPath string, args []string) error {
	cmd := exec.CommandContext(ctx, binPath, args[1:]...)
	cmd.Dir = hc.Dir
	cmd.Stdin = hc.Stdin
	cmd.Stdout = hc.Stdout
	cmd.Stderr = hc.Stderr
	cmd.Env = s.execChildEnv(hc)
	afterStart, afterWait := interp.PrepareExecStdio(cmd)
	if err := cmd.Start(); err != nil {
		afterWait()
		fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], err)
		if errors.Is(err, os.ErrNotExist) {
			return interp.ExitStatus(127)
		}
		return interp.ExitStatus(126)
	}
	afterStart()
	defer afterWait()
	hc.ReportBgStart(cmd.Process.Pid)
	j := globalJobs.track(cmd, args)
	if CapabilityHookFromContext(ctx) == nil {
		hashRecord(args[0], binPath)
	}
	<-j.done
	if j.err != nil {
		if isPipeClosed(j.err) {
			return nil
		}
		if ctx.Err() == context.DeadlineExceeded {
			return interp.ExitStatus(124)
		}
		if ctx.Err() == context.Canceled {
			return interp.ExitStatus(130)
		}
		if ee, ok := j.err.(*exec.ExitError); ok {
			return interp.ExitStatus(uint8(ee.ExitCode()))
		}
		return j.err
	}
	return nil
}

func (s *Session) execChildEnv(hc interp.HandlerContext) []string {
	merged := make(map[string]string, len(s.baseEnv)+16)
	order := make([]string, 0, len(s.baseEnvOrder)+16)

	if s.inheritEnv && len(s.scrubPatterns) == 0 {
		for _, kv := range os.Environ() {
			if i := strings.IndexByte(kv, '='); i > 0 {
				k := kv[:i]
				if _, ok := merged[k]; !ok {
					order = append(order, k)
				}
				merged[k] = kv[i+1:]
			}
		}
	} else {
		for _, k := range s.baseEnvOrder {
			if v, ok := s.baseEnv[k]; ok {
				if _, exists := merged[k]; !exists {
					order = append(order, k)
				}
				merged[k] = v
			}
		}
	}

	hc.Env.Each(func(name string, vr expand.Variable) bool {
		if !vr.IsSet() {
			delete(merged, name)
			return true
		}
		if vr.Exported && vr.Kind == expand.String {
			if _, ok := merged[name]; !ok {
				order = append(order, name)
			}
			merged[name] = vr.Str
		}
		return true
	})

	if runtime.GOOS == "windows" {
		if _, ok := merged["PYTHONIOENCODING"]; !ok {
			order = append(order, "PYTHONIOENCODING")
			merged["PYTHONIOENCODING"] = "utf-8"
		}
		if _, ok := merged["PYTHONUTF8"]; !ok {
			order = append(order, "PYTHONUTF8")
			merged["PYTHONUTF8"] = "1"
		}
	}

	out := make([]string, 0, len(order))
	for _, k := range order {
		if v, ok := merged[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// --- CapabilityHook Denial Recorder Wrapper ---

type recordingCapabilityHook struct {
	inner CapabilityHook
}

func wrapCapabilityHookForDenials(h CapabilityHook) CapabilityHook {
	if h == nil {
		return nil
	}
	if _, isJail := h.(*Jail); isJail {
		return h
	}
	return &recordingCapabilityHook{inner: h}
}

func (r *recordingCapabilityHook) CheckAccess(ctx context.Context, req AccessRequest) error {
	err := r.inner.CheckAccess(ctx, req)
	if err != nil && errors.Is(err, os.ErrPermission) {
		recordContextDenial(ctx, req)
	}
	return err
}

func (r *recordingCapabilityHook) DialContext(ctx context.Context, cmdName, network, address string) (net.Conn, error) {
	conn, err := r.inner.DialContext(ctx, cmdName, network, address)
	if err != nil && errors.Is(err, os.ErrPermission) {
		host, portStr, _ := net.SplitHostPort(address)
		port, _ := strconv.Atoi(portStr)
		recordContextDenial(ctx, AccessRequest{
			Op:      OpNetConnect,
			Command: cmdName,
			Proto:   network,
			Host:    host,
			Port:    port,
		})
	}
	return conn, err
}

func (r *recordingCapabilityHook) Listen(ctx context.Context, cmdName, network, address string) (net.Listener, error) {
	ln, err := r.inner.Listen(ctx, cmdName, network, address)
	if err != nil && errors.Is(err, os.ErrPermission) {
		host, portStr, _ := net.SplitHostPort(address)
		port, _ := strconv.Atoi(portStr)
		recordContextDenial(ctx, AccessRequest{
			Op:      OpNetListen,
			Command: cmdName,
			Proto:   network,
			Host:    host,
			Port:    port,
		})
	}
	return ln, err
}

func (r *recordingCapabilityHook) ListenPacket(ctx context.Context, cmdName, network, address string) (net.PacketConn, error) {
	pc, err := r.inner.ListenPacket(ctx, cmdName, network, address)
	if err != nil && errors.Is(err, os.ErrPermission) {
		host, portStr, _ := net.SplitHostPort(address)
		port, _ := strconv.Atoi(portStr)
		recordContextDenial(ctx, AccessRequest{
			Op:      OpNetListen,
			Command: cmdName,
			Proto:   network,
			Host:    host,
			Port:    port,
		})
	}
	return pc, err
}

// --- Thread-Safe Stdio Proxies ---

type sessionStdinProxy struct {
	mu sync.Mutex
	r  io.Reader
}

var _ interp.StdinPipe = (*sessionStdinProxy)(nil)

func (p *sessionStdinProxy) setReader(r io.Reader) {
	p.mu.Lock()
	p.r = r
	p.mu.Unlock()
}

func (p *sessionStdinProxy) Read(b []byte) (int, error) {
	p.mu.Lock()
	r := p.r
	p.mu.Unlock()
	if r == nil {
		return 0, io.EOF
	}
	return r.Read(b)
}

func (p *sessionStdinProxy) Close() error {
	return nil
}

func (p *sessionStdinProxy) SetReadDeadline(t time.Time) error {
	p.mu.Lock()
	r := p.r
	p.mu.Unlock()
	if sp, ok := r.(interface{ SetReadDeadline(time.Time) error }); ok {
		return sp.SetReadDeadline(t)
	}
	return nil
}

type syncCaptureWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	stream io.Writer
}

func (w *syncCaptureWriter) reset(stream io.Writer) {
	w.mu.Lock()
	w.buf.Reset()
	w.stream = stream
	w.mu.Unlock()
}

func (w *syncCaptureWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	_, _ = w.buf.Write(p)
	stream := w.stream
	w.mu.Unlock()
	if stream != nil {
		_, _ = stream.Write(p)
	}
	return len(p), nil
}

func (w *syncCaptureWriter) WriteString(s string) (int, error) {
	if len(s) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	_, _ = w.buf.WriteString(s)
	stream := w.stream
	w.mu.Unlock()
	if stream != nil {
		_, _ = io.WriteString(stream, s)
	}
	return len(s), nil
}

func (w *syncCaptureWriter) String() string {
	w.mu.Lock()
	s := w.buf.String()
	w.mu.Unlock()
	return s
}

func combineStreamWriters(a, b io.Writer) io.Writer {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		return b
	case b == nil:
		return a
	default:
		return io.MultiWriter(a, b)
	}
}

type synthDevFileInfo struct {
	name string
}

func (d synthDevFileInfo) Name() string       { return d.name }
func (d synthDevFileInfo) Size() int64        { return 0 }
func (d synthDevFileInfo) Mode() fs.FileMode  { return os.ModeDevice | os.ModeCharDevice | 0o666 }
func (d synthDevFileInfo) ModTime() time.Time { return time.Time{} }
func (d synthDevFileInfo) IsDir() bool        { return false }
func (d synthDevFileInfo) Sys() any           { return nil }
