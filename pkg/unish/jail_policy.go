package unish

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Approver synchronously decides an AccessRequest whose static policy yielded
// DecisionAsk. Returning DecisionAllow permits the single request; returning
// DecisionAllowSession caches the grant on the Jail for the remainder of the
// session; returning DecisionDeny (or an error) denies the request.
type Approver = func(ctx context.Context, req AccessRequest) (Decision, error)

// PathRule defines a filesystem access policy rule matched against canonical
// and workspace-lexical paths.
type PathRule struct {
	Pattern  string
	Ops      []AccessOp // empty = all filesystem operations
	Decision Decision
	Reason   string
}

// NetRule defines an outbound connect or inbound listen network policy rule.
type NetRule struct {
	Proto       string     // "tcp", "udp", or "" (any)
	HostPattern string     // e.g. "example.com", "*.example.com", "*"
	CIDR        string     // e.g. "10.0.0.0/8", "2001:db8::/32"
	Ports       []int      // specific allowed/denied ports; empty = any (unless PortRange set)
	PortRange   [2]int     // inclusive [min, max] when PortRange[0] > 0 || PortRange[1] > 0
	Ops         []AccessOp // OpNetConnect, OpNetListen; empty = any network op
	Decision    Decision
	Reason      string
}

// NetPolicy configures network capability enforcement on a Jail.
type NetPolicy struct {
	// Default governs outbound network connections unmatched by Rules/Loopback.
	// When Approver is nil, DecisionAsk fails closed as DecisionDeny.
	Default         Decision
	AllowLoopback   bool
	BlockPrivateLAN bool
	AllowListen     Decision
	Rules           []NetRule
	// TransportDialer optionally overrides the underlying socket dialer (for
	// example, to route jailed connections over virtualized net.Pipe transports).
	TransportDialer func(ctx context.Context, network, addr string) (net.Conn, error)
	// LookupIPAddr optionally overrides DNS resolution during DialContext
	// (defaults to net.DefaultResolver.LookupIPAddr).
	LookupIPAddr func(ctx context.Context, host string) ([]net.IPAddr, error)
}

// ExecPolicy configures external OS process execution within a Jail.
type ExecPolicy struct {
	// ExecDefault governs external binary execution when unmatched by
	// DenyBinaries or AllowBinaries. When Approver is nil, DecisionAsk fails
	// closed as DecisionDeny (pure builtins-only jail).
	ExecDefault   Decision
	AllowBinaries []string // matches basename (e.g. "git") or canonical full path
	DenyBinaries  []string // matches basename or canonical full path; wins over AllowBinaries
}

// ProcPolicy configures host process inspection and signaling within a Jail.
type ProcPolicy struct {
	ProcDefault Decision
}

// JailConfig defines the declarative + interactive security policy for NewJail.
//
// Note on Decision zero values: because DecisionAsk is the zero value of
// Decision, any domain left at DecisionAsk automatically fails closed as
// DecisionDeny whenever Approver is nil. When attaching an Approver while
// wanting non-interactive domains to deny immediately without prompting, set
// those domains explicitly to DecisionDeny (or start from DefaultJailConfig).
type JailConfig struct {
	Roots               []string
	ReadOnlyRoots       []string
	Rules               []PathRule
	OutsideAction       Decision
	ReadOnlyWriteAction Decision
	ReadOnlyWriteAsk    bool
	Net                 NetPolicy
	Exec                ExecPolicy
	Proc                ProcPolicy
	Approver            Approver
	OnAudit             func(req AccessRequest, dec Decision)
}

// DefaultJailConfig returns a strict fail-closed JailConfig rooted at roots
// with OutsideAction, Net, Exec, and Proc all explicitly set to DecisionDeny
// and BlockPrivateLAN enabled.
func DefaultJailConfig(roots ...string) JailConfig {
	return JailConfig{
		Roots:               append([]string(nil), roots...),
		OutsideAction:       DecisionDeny,
		ReadOnlyWriteAction: DecisionDeny,
		Net: NetPolicy{
			Default:         DecisionDeny,
			BlockPrivateLAN: true,
			AllowListen:     DecisionDeny,
		},
		Exec: ExecPolicy{
			ExecDefault: DecisionDeny,
		},
		Proc: ProcPolicy{
			ProcDefault: DecisionDeny,
		},
	}
}

type compiledPathRule struct {
	raw        PathRule
	ops        map[AccessOp]bool
	hasGlob    bool
	exactRoots []string   // normalized slash-separated prefixes when !hasGlob
	globSegs   [][]string // normalized slash-separated segment patterns when hasGlob
}

type compiledNetRule struct {
	raw         NetRule
	proto       string
	hostPattern string
	hasPrefix   bool
	prefix       netip.Prefix
	ports       map[int]bool
	portMin     int
	portMax     int
	ops         map[AccessOp]bool
}

type compiledBinPattern struct {
	isPath   bool
	fullPath string
	baseName string
}

type denialCollectorKey struct{}

type denialCollector struct {
	mu      sync.Mutex
	denials []AccessRequest
}

func withDenialCollector(ctx context.Context) (context.Context, *denialCollector) {
	dc := &denialCollector{}
	return context.WithValue(ctx, denialCollectorKey{}, dc), dc
}

func recordContextDenial(ctx context.Context, req AccessRequest) {
	if ctx == nil {
		return
	}
	if dc, ok := ctx.Value(denialCollectorKey{}).(*denialCollector); ok && dc != nil {
		dc.mu.Lock()
		dc.denials = append(dc.denials, cloneAccessRequest(req))
		dc.mu.Unlock()
	}
}

func (dc *denialCollector) snapshot() []AccessRequest {
	if dc == nil {
		return nil
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if len(dc.denials) == 0 {
		return nil
	}
	out := make([]AccessRequest, len(dc.denials))
	for i := range dc.denials {
		out[i] = cloneAccessRequest(dc.denials[i])
	}
	return out
}

type contextApprovalMemoKey struct{}

type contextApprovalMemo struct {
	mu       sync.Mutex
	approved map[string]bool
}

func withApprovalMemo(ctx context.Context) context.Context {
	if ctx != nil {
		if _, ok := ctx.Value(contextApprovalMemoKey{}).(*contextApprovalMemo); ok {
			return ctx
		}
	}
	return context.WithValue(ctx, contextApprovalMemoKey{}, &contextApprovalMemo{
		approved: make(map[string]bool, 4),
	})
}

func markContextApproved(ctx context.Context, key string) {
	if ctx == nil || key == "" {
		return
	}
	if m, ok := ctx.Value(contextApprovalMemoKey{}).(*contextApprovalMemo); ok && m != nil {
		m.mu.Lock()
		m.approved[key] = true
		m.mu.Unlock()
	}
}

func isContextApproved(ctx context.Context, key string) bool {
	if ctx == nil || key == "" {
		return false
	}
	if m, ok := ctx.Value(contextApprovalMemoKey{}).(*contextApprovalMemo); ok && m != nil {
		m.mu.Lock()
		ok2 := m.approved[key]
		m.mu.Unlock()
		return ok2
	}
	return false
}

// Jail enforces declarative and interactive filesystem, network, process, and
// external binary execution policies. It implements CapabilityHook and is safe
// for concurrent use across goroutines and Sessions.
type Jail struct {
	roots               []string // normalized slash-separated canonical roots
	readOnlyRoots       []string // normalized slash-separated canonical read-only roots
	rules               []compiledPathRule
	outsideAction       Decision
	readOnlyWriteAction Decision
	readOnlyWriteAsk    bool

	netDefault       Decision
	allowLoopback    bool
	blockPrivateLAN  bool
	allowListen      Decision
	netRules         []compiledNetRule
	transportDialer  func(ctx context.Context, network, addr string) (net.Conn, error)
	lookupIPAddr     func(ctx context.Context, host string) ([]net.IPAddr, error)

	execDefault   Decision
	allowBinaries []compiledBinPattern
	denyBinaries  []compiledBinPattern

	procDefault Decision

	mu            sync.Mutex
	approver      Approver
	onAudit       func(req AccessRequest, dec Decision)
	sessionGrants map[string]bool
	denials       []AccessRequest
}

var _ CapabilityHook = (*Jail)(nil)

// NewJail validates cfg, canonicalizes configured roots and rules, and returns
// a thread-safe *Jail implementing CapabilityHook.
func NewJail(cfg JailConfig) (*Jail, error) {
	j := &Jail{
		outsideAction:       cfg.OutsideAction,
		readOnlyWriteAction: cfg.ReadOnlyWriteAction,
		readOnlyWriteAsk:    cfg.ReadOnlyWriteAsk,
		netDefault:          cfg.Net.Default,
		allowLoopback:       cfg.Net.AllowLoopback,
		blockPrivateLAN:     cfg.Net.BlockPrivateLAN,
		allowListen:         cfg.Net.AllowListen,
		transportDialer:     cfg.Net.TransportDialer,
		lookupIPAddr:        cfg.Net.LookupIPAddr,
		execDefault:         cfg.Exec.ExecDefault,
		procDefault:         cfg.Proc.ProcDefault,
		approver:            cfg.Approver,
		onAudit:             cfg.OnAudit,
		sessionGrants:       make(map[string]bool),
	}

	var baseRoots []string
	for _, r := range cfg.Roots {
		if strings.TrimSpace(r) == "" {
			continue
		}
		expanded := expandTildePath(r)
		hostPath := resolveHostPath("", expanded)
		realRoot, lexRoot := canonicalizeForCheck(hostPath)
		normReal := normalizeMatchPath(realRoot)
		if normReal != "" && !slices.Contains(j.roots, normReal) {
			j.roots = append(j.roots, normReal)
			baseRoots = append(baseRoots, realRoot)
		}
		normLex := normalizeMatchPath(lexRoot)
		if normLex != "" && !slices.Contains(j.roots, normLex) {
			j.roots = append(j.roots, normLex)
		}
	}

	for _, r := range cfg.ReadOnlyRoots {
		if strings.TrimSpace(r) == "" {
			continue
		}
		expanded := expandTildePath(r)
		hostPath := resolveHostPath("", expanded)
		realRoot, lexRoot := canonicalizeForCheck(hostPath)
		normReal := normalizeMatchPath(realRoot)
		if normReal != "" && !slices.Contains(j.readOnlyRoots, normReal) {
			j.readOnlyRoots = append(j.readOnlyRoots, normReal)
			baseRoots = append(baseRoots, realRoot)
		}
		normLex := normalizeMatchPath(lexRoot)
		if normLex != "" && !slices.Contains(j.readOnlyRoots, normLex) {
			j.readOnlyRoots = append(j.readOnlyRoots, normLex)
		}
	}

	for _, rule := range cfg.Rules {
		cr, err := compilePathRule(rule, baseRoots)
		if err != nil {
			return nil, err
		}
		j.rules = append(j.rules, cr)
	}

	for _, nr := range cfg.Net.Rules {
		cnr, err := compileNetRule(nr)
		if err != nil {
			return nil, err
		}
		j.netRules = append(j.netRules, cnr)
	}

	for _, b := range cfg.Exec.AllowBinaries {
		if bp, ok := compileBinPattern(b); ok {
			j.allowBinaries = append(j.allowBinaries, bp)
		}
	}
	for _, b := range cfg.Exec.DenyBinaries {
		if bp, ok := compileBinPattern(b); ok {
			j.denyBinaries = append(j.denyBinaries, bp)
		}
	}

	return j, nil
}

// SetApprover updates the interactive Approver callback on j.
func (j *Jail) SetApprover(fn Approver) {
	j.mu.Lock()
	j.approver = fn
	j.mu.Unlock()
}

// GrantSession records a session-scoped permission grant for (op, target) so
// subsequent matching operations succeed without invoking Approver.
func (j *Jail) GrantSession(op AccessOp, target string) {
	keys := sessionGrantKeysForTarget(op, target)
	if len(keys) == 0 {
		return
	}
	j.mu.Lock()
	for _, k := range keys {
		j.sessionGrants[k] = true
	}
	j.mu.Unlock()
}

// GrantSessionRequest records a session-scoped grant corresponding to req.
func (j *Jail) GrantSessionRequest(req AccessRequest) {
	keys := sessionGrantKeysForRequest(req)
	if len(keys) == 0 {
		return
	}
	j.mu.Lock()
	for _, k := range keys {
		j.sessionGrants[k] = true
	}
	j.mu.Unlock()
}

// HasSessionGrant reports whether (op, target) is currently granted in the
// session cache.
func (j *Jail) HasSessionGrant(op AccessOp, target string) bool {
	keys := sessionGrantKeysForTarget(op, target)
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, k := range keys {
		if j.sessionGrants[k] {
			return true
		}
	}
	return false
}

// RevokeSessionGrants clears all cached DecisionAllowSession grants on j.
func (j *Jail) RevokeSessionGrants() {
	j.mu.Lock()
	clear(j.sessionGrants)
	j.mu.Unlock()
}

// Denials returns a copy of all denied AccessRequests currently buffered on j.
func (j *Jail) Denials() []AccessRequest {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.denials) == 0 {
		return nil
	}
	out := make([]AccessRequest, len(j.denials))
	for i := range j.denials {
		out[i] = cloneAccessRequest(j.denials[i])
	}
	return out
}

// DrainDenials atomically returns and clears all denied AccessRequests on j.
func (j *Jail) DrainDenials() []AccessRequest {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.denials) == 0 {
		return nil
	}
	out := make([]AccessRequest, len(j.denials))
	for i := range j.denials {
		out[i] = cloneAccessRequest(j.denials[i])
	}
	j.denials = j.denials[:0]
	return out
}

// ClearDenials discards any buffered denied AccessRequests on j.
func (j *Jail) ClearDenials() {
	j.mu.Lock()
	j.denials = j.denials[:0]
	j.mu.Unlock()
}

func cloneAccessRequest(req AccessRequest) AccessRequest {
	cp := req
	if len(req.Argv) > 0 {
		cp.Argv = append([]string(nil), req.Argv...)
	}
	return cp
}

// CheckAccess evaluates j's policy for req, consulting session grants and
// Approver when policy yields DecisionAsk, recording denials and audit events.
func (j *Jail) CheckAccess(ctx context.Context, req AccessRequest) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var dec Decision
	var reason string

	switch req.Op {
	case OpRead, OpWrite, OpDelete, OpList, OpStat, OpChdir, OpLink:
		dec, reason = j.evaluateFS(req)
	case OpNetConnect, OpNetListen:
		dec, reason = j.evaluateNet(req)
	case OpExec:
		dec, reason = j.evaluateExec(req)
	case OpProc:
		dec, reason = j.evaluateProc(req)
	default:
		dec, reason = DecisionDeny, fmt.Sprintf("unknown access operation %q", req.Op)
	}

	finalDec, finalReason, err := j.resolveDecision(ctx, req, dec, reason)
	if err != nil {
		return err
	}

	j.mu.Lock()
	onAudit := j.onAudit
	if finalDec == DecisionDeny {
		j.denials = append(j.denials, cloneAccessRequest(req))
	}
	j.mu.Unlock()

	if finalDec == DecisionDeny {
		recordContextDenial(ctx, req)
	}
	if onAudit != nil {
		onAudit(cloneAccessRequest(req), finalDec)
	}
	if finalDec == DecisionDeny {
		return &AccessDenialError{
			Op:     req.Op,
			Target: requestTarget(req),
			Reason: finalReason,
		}
	}
	return nil
}

func (j *Jail) resolveDecision(ctx context.Context, req AccessRequest, dec Decision, reason string) (Decision, string, error) {
	switch dec {
	case DecisionAllow, DecisionAllowSession:
		return dec, reason, nil
	case DecisionDeny:
		if reason == "" {
			reason = "denied by jail policy"
		}
		return DecisionDeny, reason, nil
	case DecisionAsk:
		memoKey := contextMemoKey(req)
		if isContextApproved(ctx, memoKey) {
			return DecisionAllow, reason, nil
		}
		if j.isGrantedInSession(req) {
			return DecisionAllowSession, reason, nil
		}
		j.mu.Lock()
		approver := j.approver
		j.mu.Unlock()
		if approver == nil {
			if reason == "" {
				reason = defaultDenyReason(req.Op)
			}
			return DecisionDeny, reason, nil
		}
		verdict, err := approver(ctx, cloneAccessRequest(req))
		if err != nil {
			if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return DecisionDeny, err.Error(), ctx.Err()
			}
			return DecisionDeny, err.Error(), nil
		}
		switch verdict {
		case DecisionAllow:
			markContextApproved(ctx, memoKey)
			return DecisionAllow, reason, nil
		case DecisionAllowSession:
			j.GrantSessionRequest(req)
			markContextApproved(ctx, memoKey)
			return DecisionAllowSession, reason, nil
		default:
			if reason == "" {
				reason = "denied by interactive approver"
			}
			return DecisionDeny, reason, nil
		}
	default:
		return DecisionDeny, "denied by jail policy", nil
	}
}

func defaultDenyReason(op AccessOp) string {
	switch op {
	case OpNetConnect:
		return "outbound network access disabled by jail"
	case OpNetListen:
		return "network listen disabled by jail"
	case OpExec:
		return "external binary execution disabled by jail"
	case OpProc:
		return "host process access disabled by jail"
	default:
		return "outside allowed jail roots"
	}
}

func requestTarget(req AccessRequest) string {
	switch req.Op {
	case OpNetConnect, OpNetListen:
		h := req.Host
		if h == "" {
			h = req.ResolvedIP
		}
		if req.Port > 0 {
			return net.JoinHostPort(h, strconv.Itoa(req.Port))
		}
		return h
	case OpExec:
		if req.RawPath != "" {
			return req.RawPath
		}
		if req.Binary != "" {
			return req.Binary
		}
		return req.Path
	case OpProc:
		if req.PID > 0 {
			return fmt.Sprintf("pid %d", req.PID)
		}
		if req.Signal != "" {
			return req.Signal
		}
		return req.Command
	default:
		if req.RawPath != "" {
			return req.RawPath
		}
		return req.Path
	}
}

func contextMemoKey(req AccessRequest) string {
	switch req.Op {
	case OpNetConnect, OpNetListen:
		return string(req.Op) + "|" + strings.ToLower(req.Host) + "|" + strconv.Itoa(req.Port)
	case OpExec:
		bin := req.Binary
		if bin == "" {
			bin = req.Path
		}
		return "exec|" + normalizeMatchPath(bin)
	case OpProc:
		return fmt.Sprintf("proc|%d|%s", req.PID, req.Signal)
	default:
		realP, _ := canonicalizeForCheck(resolveHostPath("", req.Path))
		return string(req.Op) + "|" + normalizeMatchPath(realP)
	}
}

func (j *Jail) isGrantedInSession(req AccessRequest) bool {
	keys := j.sessionLookupKeysForRequest(req)
	if len(keys) == 0 {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, k := range keys {
		if j.sessionGrants[k] {
			return true
		}
	}
	return false
}

func sessionGrantKeysForTarget(op AccessOp, target string) []string {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	switch op {
	case OpNetConnect, OpNetListen:
		if h, p, err := net.SplitHostPort(target); err == nil {
			return []string{string(op) + "|" + strings.ToLower(h) + ":" + p}
		}
		return []string{string(op) + "|" + strings.ToLower(target)}
	case OpExec:
		if strings.ContainsAny(target, `/\`) || filepath.IsAbs(target) || strings.HasPrefix(target, "~") {
			realBin, _ := canonicalizeForCheck(resolveHostPath("", expandTildePath(target)))
			return []string{"exec|path:" + normalizeMatchPath(realBin)}
		}
		return []string{"exec|base:" + normalizeBinBase(target)}
	case OpProc:
		return []string{"proc|" + strings.ToLower(target)}
	default:
		realP, lexP := canonicalizeForCheck(resolveHostPath("", expandTildePath(target)))
		out := []string{string(op) + "|" + normalizeMatchPath(realP)}
		if nl := normalizeMatchPath(lexP); nl != "" && nl != normalizeMatchPath(realP) {
			out = append(out, string(op)+"|"+nl)
		}
		return out
	}
}

func sessionGrantKeysForRequest(req AccessRequest) []string {
	switch req.Op {
	case OpNetConnect, OpNetListen:
		h := strings.ToLower(req.Host)
		if h == "" {
			h = strings.ToLower(req.ResolvedIP)
		}
		if h == "" {
			return nil
		}
		if req.Port > 0 {
			return []string{string(req.Op) + "|" + h + ":" + strconv.Itoa(req.Port)}
		}
		return []string{string(req.Op) + "|" + h}
	case OpExec:
		var out []string
		bin := req.Binary
		if bin == "" {
			bin = req.Path
		}
		if bin != "" {
			realBin, _ := canonicalizeForCheck(resolveHostPath("", bin))
			out = append(out, "exec|path:"+normalizeMatchPath(realBin))
		}
		if req.Path != "" {
			out = append(out, "exec|path:"+normalizeMatchPath(req.Path))
		}
		return out
	case OpProc:
		if req.PID > 0 {
			return []string{fmt.Sprintf("proc|%d", req.PID)}
		}
		return []string{"proc|" + strings.ToLower(req.Signal)}
	default:
		realP, lexP := canonicalizeForCheck(resolveHostPath("", req.Path))
		out := []string{string(req.Op) + "|" + normalizeMatchPath(realP)}
		if nl := normalizeMatchPath(lexP); nl != "" && nl != normalizeMatchPath(realP) {
			out = append(out, string(req.Op)+"|"+nl)
		}
		return out
	}
}

func (j *Jail) sessionLookupKeysForRequest(req AccessRequest) []string {
	keys := sessionGrantKeysForRequest(req)
	switch req.Op {
	case OpNetConnect, OpNetListen:
		h := strings.ToLower(req.Host)
		if h == "" {
			h = strings.ToLower(req.ResolvedIP)
		}
		if h != "" && req.Port > 0 {
			keys = append(keys, string(req.Op)+"|"+h)
		}
	case OpExec:
		bin := req.Binary
		if bin == "" {
			bin = req.Path
		}
		if bin != "" {
			realBin, _ := canonicalizeForCheck(resolveHostPath("", bin))
			np := normalizeMatchPath(realBin)
			if np != "" && !j.isInsideWritableRoot(np) {
				keys = append(keys, "exec|base:"+normalizeBinBase(filepath.Base(realBin)))
			}
		}
	case OpProc:
		keys = append(keys, "proc|*")
	case OpStat:
		// If a path was already session-approved for read, write, list, or
		// chdir, allow stat checks on that exact path without double-prompting.
		realP, _ := canonicalizeForCheck(resolveHostPath("", req.Path))
		np := normalizeMatchPath(realP)
		if np != "" {
			keys = append(keys,
				string(OpRead)+"|"+np,
				string(OpWrite)+"|"+np,
				string(OpList)+"|"+np,
				string(OpChdir)+"|"+np,
			)
		}
	}
	return keys
}

// --- Filesystem Policy Evaluation ---

func (j *Jail) evaluateFS(req AccessRequest) (Decision, string) {
	if req.Path == "" {
		return DecisionDeny, "empty path"
	}
	hostPath := resolveHostPath("", req.Path)
	realPath, lexPath := canonicalizeForCheck(hostPath)

	cands := []string{normalizeMatchPath(realPath)}
	if nl := normalizeMatchPath(lexPath); nl != "" && nl != cands[0] {
		cands = append(cands, nl)
	}

	// If OpLink carries a TargetPath, evaluate policy on TargetPath as OpWrite
	// too if it violates a deny rule or root boundary.
	if req.TargetPath != "" {
		decT, reasonT := j.evaluateSingleFSPath(OpWrite, req.TargetPath)
		if decT == DecisionDeny {
			return DecisionDeny, reasonT
		}
	}

	// Evaluate both real canonical path and lexical path; strictest decision wins
	// (Deny > Ask > Allow) so neither symlink escape outside roots nor symlink
	// alias of a denied workspace file can bypass policy.
	bestDec := DecisionAllow
	bestReason := ""
	first := true
	for _, cand := range cands {
		dec, reason := j.evaluateNormalizedFS(req.Op, cand)
		if first || decisionSeverity(dec) > decisionSeverity(bestDec) {
			bestDec = dec
			bestReason = reason
			first = false
		}
	}
	return bestDec, bestReason
}

func (j *Jail) evaluateSingleFSPath(op AccessOp, raw string) (Decision, string) {
	realPath, _ := canonicalizeForCheck(resolveHostPath("", raw))
	return j.evaluateNormalizedFS(op, normalizeMatchPath(realPath))
}

func decisionSeverity(d Decision) int {
	switch d {
	case DecisionDeny:
		return 3
	case DecisionAsk:
		return 2
	default:
		return 1
	}
}

func (j *Jail) evaluateNormalizedFS(op AccessOp, normPath string) (Decision, string) {
	if normPath == "" {
		return DecisionDeny, "empty path"
	}

	// 1. Explicit PathRules take precedence over Roots / ReadOnlyRoots / OutsideAction.
	// Among matching rules, explicit DecisionDeny always wins over DecisionAsk,
	// which wins over DecisionAllow.
	matchedAllow := false
	allowReason := ""
	matchedAsk := false
	askReason := ""
	for i := range j.rules {
		r := &j.rules[i]
		if len(r.ops) > 0 && !r.ops[op] {
			continue
		}
		if !r.matches(normPath) {
			continue
		}
		switch r.raw.Decision {
		case DecisionDeny:
			reason := r.raw.Reason
			if reason == "" {
				reason = fmt.Sprintf("matched deny rule %q", r.raw.Pattern)
			}
			return DecisionDeny, reason
		case DecisionAsk:
			matchedAsk = true
			askReason = r.raw.Reason
		case DecisionAllow, DecisionAllowSession:
			matchedAllow = true
			allowReason = r.raw.Reason
		}
	}
	if matchedAsk {
		return DecisionAsk, askReason
	}
	if matchedAllow {
		return DecisionAllow, allowReason
	}

	// 2. Check Roots and ReadOnlyRoots using longest-prefix specificity (ties
	// favor writable Roots so `--jail /ws --allow-read /ws` stays writable,
	// while `--jail /ws --allow-read /ws/vendor` keeps /ws/vendor read-only,
	// and `--allow-read /ws --allow-write /ws/out` makes /ws/out writable).
	bestWriteLen := -1
	for _, root := range j.roots {
		if isWithinNormalizedRoot(normPath, root) && len(root) > bestWriteLen {
			bestWriteLen = len(root)
		}
	}
	bestROLen := -1
	for _, ro := range j.readOnlyRoots {
		if isWithinNormalizedRoot(normPath, ro) && len(ro) > bestROLen {
			bestROLen = len(ro)
		}
	}

	if bestWriteLen >= 0 && bestWriteLen >= bestROLen {
		return DecisionAllow, ""
	}
	if bestROLen >= 0 {
		switch op {
		case OpRead, OpList, OpStat, OpChdir:
			return DecisionAllow, ""
		default:
			if j.readOnlyWriteAsk {
				return DecisionAsk, "write access inside read-only root"
			}
			if j.readOnlyWriteAction == DecisionAllow || j.readOnlyWriteAction == DecisionAllowSession {
				return DecisionAllow, ""
			}
			return DecisionDeny, "read-only jail root"
		}
	}

	// 3. Outside all configured roots.
	return j.outsideAction, "outside allowed jail roots"
}

// --- Network Policy Evaluation & DNS-Rebinding Safe Dialer ---

func (j *Jail) evaluateNet(req AccessRequest) (Decision, string) {
	proto := normalizeNetProto(req.Proto)
	host := normalizeHostname(req.Host)

	var addr netip.Addr
	hasAddr := false
	if req.ResolvedIP != "" {
		if a, ok := parseNormalizedIP(req.ResolvedIP); ok {
			addr = a
			hasAddr = true
		}
	} else if host != "" {
		if a, ok := parseNormalizedIP(host); ok {
			addr = a
			hasAddr = true
		}
	}

	// 1. Evaluate explicit NetRules:
	//    - Any matching DecisionDeny rule wins immediately.
	//    - Track whether an IP/CIDR-specific rule (vs hostname-only rule)
	//      explicitly permitted the destination IP so DNS-rebinding of an
	//      allowed HostPattern CANNOT bypass BlockPrivateLAN or loopback blocks.
	matchedHostRule := false
	matchedIPRule := false
	var ruleDec Decision
	var ruleReason string

	for i := range j.netRules {
		r := &j.netRules[i]
		if !r.matchesOpProtoPort(req.Op, proto, req.Port) {
			continue
		}
		if r.hostPattern != "" {
			if !matchHostPattern(r.hostPattern, host, addr, hasAddr) {
				continue
			}
		}
		if r.hasPrefix {
			if !hasAddr || !r.prefix.Contains(addr) {
				continue
			}
		}
		if r.raw.Decision == DecisionDeny {
			reason := r.raw.Reason
			if reason == "" {
				reason = "denied by network rule"
			}
			return DecisionDeny, reason
		}
		isExplicitIPTarget := r.hasPrefix || isIPLiteralPattern(r.hostPattern)
		if isExplicitIPTarget {
			if !matchedIPRule || decisionSeverity(r.raw.Decision) > decisionSeverity(ruleDec) {
				matchedIPRule = true
				ruleDec = r.raw.Decision
				ruleReason = r.raw.Reason
			}
		} else if !matchedIPRule {
			if !matchedHostRule || decisionSeverity(r.raw.Decision) > decisionSeverity(ruleDec) {
				matchedHostRule = true
				ruleDec = r.raw.Decision
				ruleReason = r.raw.Reason
			}
		}
	}

	// 2. Enforce BlockPrivateLAN, unspecified-IP blocking, & DNS-rebinding
	//    loopback protection on the resolved IP unless an explicit IP/CIDR
	//    NetRule targeted that IP.
	isRemoteHostname := host != "" && host != "localhost" && !isIPLiteralPattern(host)
	if hasAddr && !matchedIPRule {
		if isUnspecifiedOrThisNetAddr(addr) {
			if req.Op == OpNetConnect {
				if j.netDefault == DecisionAllow && !j.blockPrivateLAN && j.allowLoopback && !matchedHostRule {
					return DecisionAllow, ""
				}
				return DecisionDeny, fmt.Sprintf("unspecified/local-network IP %s blocked by network policy", addr.String())
			}
			// OpNetListen: hostname rules must never rebind to wildcard 0.0.0.0 / ::.
			if matchedHostRule && host != "" && !isIPLiteralPattern(host) {
				return DecisionDeny, fmt.Sprintf("DNS rebinding to unspecified IP %s blocked", addr.String())
			}
		} else if isLoopbackAddr(addr) {
			if !j.allowLoopback {
				if isRemoteHostname || matchedHostRule {
					return DecisionDeny, fmt.Sprintf("DNS rebinding to loopback IP %s blocked", addr.String())
				}
				if j.blockPrivateLAN || (req.Op == OpNetConnect && j.netDefault == DecisionAllow) || (req.Op == OpNetListen && j.allowListen == DecisionAllow) {
					return DecisionDeny, fmt.Sprintf("loopback IP %s blocked by network policy", addr.String())
				}
			} else if req.Op == OpNetConnect {
				if matchedHostRule && isRemoteHostname {
					return DecisionDeny, fmt.Sprintf("DNS rebinding to loopback IP %s blocked", addr.String())
				}
				return DecisionAllow, ""
			}
		} else if j.blockPrivateLAN && isPrivateOrLinkLocalLAN(addr) {
			return DecisionDeny, fmt.Sprintf("private or link-local IP %s blocked by BlockPrivateLAN", addr.String())
		}
	}

	if matchedIPRule || matchedHostRule {
		return ruleDec, ruleReason
	}

	// 3. Built-in AllowLoopback check when IP is verified loopback (never 0.0.0.0 / ::).
	if j.allowLoopback && hasAddr && isLoopbackAddr(addr) {
		if req.Op == OpNetConnect {
			return DecisionAllow, ""
		}
		// Allow local loopback listeners in loopback mode unless AllowListen was
		// explicitly set to DecisionAsk with an active approver.
		if req.Op == OpNetListen && j.allowListen == DecisionDeny {
			return DecisionAllow, ""
		}
	}

	if req.Op == OpNetListen {
		return j.allowListen, "network listen disabled by jail"
	}
	return j.netDefault, "outbound network access disabled by jail"
}

// DialContext enforces network policy with DNS-rebinding & TOCTOU defense:
// hostnames are resolved upfront, both Host and every candidate ResolvedIP are
// checked against policy, and the underlying dialer connects directly to the
// verified IP:port so DNS cannot flip between check and connect.
func (j *Jail) DialContext(ctx context.Context, cmdName, network, address string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	baseProto := normalizeNetProto(network)
	if strings.HasPrefix(baseProto, "unix") {
		if err := j.CheckAccess(ctx, AccessRequest{
			Op:      OpWrite,
			Command: cmdName,
			Path:    address,
			RawPath: address,
		}); err != nil {
			return nil, err
		}
		if err := j.CheckAccess(ctx, AccessRequest{
			Op:      OpNetConnect,
			Command: cmdName,
			Proto:   baseProto,
			Host:    address,
		}); err != nil {
			return nil, err
		}
		return j.rawDial(ctx, network, address)
	}

	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := resolvePortNumber(ctx, baseProto, portStr)
	if err != nil {
		return nil, err
	}

	// Attach a per-dial approval memo so multi-IP/dual-stack hostnames only
	// prompt an interactive Approver once per DialContext call.
	ctx = withApprovalMemo(ctx)

	// Case 1: Address already specifies a literal IP.
	if addr, ok := parseNormalizedIP(host); ok {
		ipStr := addr.String()
		if err := j.CheckAccess(ctx, AccessRequest{
			Op:         OpNetConnect,
			Command:    cmdName,
			Proto:      baseProto,
			Host:       host,
			Port:       port,
			ResolvedIP: ipStr,
		}); err != nil {
			return nil, err
		}
		return j.rawDial(ctx, network, net.JoinHostPort(host, strconv.Itoa(port)))
	}

	// Case 2: Hostname. If static policy unconditionally denies with zero DNS
	// rules/approver/loopback, fail immediately without leaking a DNS query.
	if j.canShortCircuitHostDeny(baseProto, host, port) {
		err := j.CheckAccess(ctx, AccessRequest{
			Op:      OpNetConnect,
			Command: cmdName,
			Proto:   baseProto,
			Host:    host,
			Port:    port,
		})
		return nil, err
	}

	lookup := j.lookupIPAddr
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	ipAddrs, lookupErr := lookup(ctx, host)
	if lookupErr != nil || len(ipAddrs) == 0 {
		// Still run CheckAccess on Host so policy denials report consistently.
		if err := j.CheckAccess(ctx, AccessRequest{
			Op:      OpNetConnect,
			Command: cmdName,
			Proto:   baseProto,
			Host:    host,
			Port:    port,
		}); err != nil {
			return nil, err
		}
		if lookupErr != nil {
			return nil, lookupErr
		}
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}

	filtered := filterIPAddrsByNetwork(network, ipAddrs)
	if len(filtered) == 0 {
		filtered = ipAddrs
	}

	// Check every candidate IP; if any candidate hits a hard BlockPrivateLAN or
	// explicit Deny rule (e.g. round-robin DNS rebinding), abort immediately!
	var approvedTargets []string
	var firstAccessErr error
	for _, ipa := range filtered {
		ipStr := ipa.IP.String()
		if norm, ok := parseNormalizedIP(ipStr); ok {
			ipStr = norm.String()
		}
		req := AccessRequest{
			Op:         OpNetConnect,
			Command:    cmdName,
			Proto:      baseProto,
			Host:       host,
			Port:       port,
			ResolvedIP: ipStr,
		}
		staticDec, _ := j.evaluateNet(req)
		if staticDec == DecisionDeny && j.isHardIPBlock(req) {
			_ = j.CheckAccess(ctx, req) // record denial + audit
			return nil, &AccessDenialError{
				Op:     OpNetConnect,
				Target: net.JoinHostPort(host, strconv.Itoa(port)),
				Reason: fmt.Sprintf("resolved IP %s blocked by network policy", ipStr),
			}
		}
		if err := j.CheckAccess(ctx, req); err != nil {
			if firstAccessErr == nil {
				firstAccessErr = err
			}
			continue
		}
		dialHost := ipa.IP.String()
		if ipa.Zone != "" {
			dialHost += "%" + ipa.Zone
		}
		target := net.JoinHostPort(dialHost, strconv.Itoa(port))
		if !slices.Contains(approvedTargets, target) {
			approvedTargets = append(approvedTargets, target)
		}
	}

	if len(approvedTargets) == 0 {
		if firstAccessErr != nil {
			return nil, firstAccessErr
		}
		return nil, &AccessDenialError{
			Op:     OpNetConnect,
			Target: address,
			Reason: "no permitted IP addresses for host",
		}
	}

	var lastDialErr error
	for _, target := range approvedTargets {
		conn, err := j.rawDial(ctx, network, target)
		if err == nil {
			return conn, nil
		}
		lastDialErr = err
	}
	return nil, lastDialErr
}

func (j *Jail) isHardIPBlock(req AccessRequest) bool {
	addr, ok := parseNormalizedIP(req.ResolvedIP)
	if !ok {
		return false
	}
	if isUnspecifiedOrThisNetAddr(addr) {
		return true
	}
	if j.blockPrivateLAN && isPrivateOrLinkLocalLAN(addr) {
		return true
	}
	if !j.allowLoopback && isLoopbackAddr(addr) {
		return true
	}
	for i := range j.netRules {
		r := &j.netRules[i]
		if r.raw.Decision == DecisionDeny && r.hasPrefix && r.prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (j *Jail) canShortCircuitHostDeny(proto, host string, port int) bool {
	j.mu.Lock()
	hasApprover := j.approver != nil
	hasGrants := len(j.sessionGrants) > 0
	j.mu.Unlock()

	normHost := normalizeHostname(host)
	for i := range j.netRules {
		r := &j.netRules[i]
		if !r.matchesOpProtoPort(OpNetConnect, proto, port) {
			continue
		}
		if r.raw.Decision == DecisionDeny && !r.hasPrefix && r.hostPattern != "" {
			if matchHostPattern(r.hostPattern, normHost, netip.Addr{}, false) {
				return true
			}
		}
	}
	if hasApprover || hasGrants || j.allowLoopback || j.netDefault != DecisionDeny {
		return false
	}
	return len(j.netRules) == 0
}

func (j *Jail) rawDial(ctx context.Context, network, addr string) (net.Conn, error) {
	if j.transportDialer != nil {
		return j.transportDialer(ctx, network, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// Listen enforces OpNetListen policy (resolving non-IP hostnames upfront and
// binding the verified IP:port directly) before binding a stream listener.
func (j *Jail) Listen(ctx context.Context, cmdName, network, address string) (net.Listener, error) {
	targets, err := j.resolveAndCheckListenTargets(ctx, cmdName, network, address)
	if err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	var lastErr error
	for _, t := range targets {
		ln, lerr := lc.Listen(ctx, network, t)
		if lerr == nil {
			return ln, nil
		}
		lastErr = lerr
	}
	return nil, lastErr
}

// ListenPacket enforces OpNetListen policy (resolving non-IP hostnames upfront
// and binding the verified IP:port directly) before binding a packet listener.
func (j *Jail) ListenPacket(ctx context.Context, cmdName, network, address string) (net.PacketConn, error) {
	targets, err := j.resolveAndCheckListenTargets(ctx, cmdName, network, address)
	if err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	var lastErr error
	for _, t := range targets {
		pc, lerr := lc.ListenPacket(ctx, network, t)
		if lerr == nil {
			return pc, nil
		}
		lastErr = lerr
	}
	return nil, lastErr
}

func (j *Jail) resolveAndCheckListenTargets(ctx context.Context, cmdName, network, address string) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	baseProto := normalizeNetProto(network)
	if strings.HasPrefix(baseProto, "unix") {
		if err := j.CheckAccess(ctx, AccessRequest{
			Op:      OpWrite,
			Command: cmdName,
			Path:    address,
			RawPath: address,
		}); err != nil {
			return nil, err
		}
		if err := j.CheckAccess(ctx, AccessRequest{
			Op:      OpNetListen,
			Command: cmdName,
			Proto:   baseProto,
			Host:    address,
		}); err != nil {
			return nil, err
		}
		return []string{address}, nil
	}

	ctx = withApprovalMemo(ctx)

	host := ""
	port := 0
	if address != "" {
		h, portStr, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		host = h
		if portStr != "" {
			p, err := resolvePortNumber(ctx, baseProto, portStr)
			if err != nil {
				return nil, err
			}
			port = p
		}
	}

	// Wildcard listen (e.g. ":8080" or "").
	if host == "" {
		if err := j.CheckAccess(ctx, AccessRequest{
			Op:      OpNetListen,
			Command: cmdName,
			Proto:   baseProto,
			Host:    "",
			Port:    port,
		}); err != nil {
			return nil, err
		}
		if address == "" {
			return []string{""}, nil
		}
		return []string{net.JoinHostPort("", strconv.Itoa(port))}, nil
	}

	// Literal IP address (e.g. "127.0.0.1:8080" or "0.0.0.0:8080").
	if addr, ok := parseNormalizedIP(host); ok {
		if err := j.CheckAccess(ctx, AccessRequest{
			Op:         OpNetListen,
			Command:    cmdName,
			Proto:      baseProto,
			Host:       host,
			Port:       port,
			ResolvedIP: addr.String(),
		}); err != nil {
			return nil, err
		}
		return []string{net.JoinHostPort(host, strconv.Itoa(port))}, nil
	}

	// Hostname: resolve upfront so DNS cannot rebind to 0.0.0.0/:: or an
	// unpermitted interface between policy check and socket bind.
	lookup := j.lookupIPAddr
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	ipAddrs, lookupErr := lookup(ctx, host)
	if lookupErr != nil || len(ipAddrs) == 0 {
		if err := j.CheckAccess(ctx, AccessRequest{
			Op:      OpNetListen,
			Command: cmdName,
			Proto:   baseProto,
			Host:    host,
			Port:    port,
		}); err != nil {
			return nil, err
		}
		if lookupErr != nil {
			return nil, lookupErr
		}
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}

	filtered := filterIPAddrsByNetwork(network, ipAddrs)
	if len(filtered) == 0 {
		filtered = ipAddrs
	}

	var approvedTargets []string
	var firstAccessErr error
	for _, ipa := range filtered {
		ipStr := ipa.IP.String()
		if norm, ok := parseNormalizedIP(ipStr); ok {
			ipStr = norm.String()
		}
		req := AccessRequest{
			Op:         OpNetListen,
			Command:    cmdName,
			Proto:      baseProto,
			Host:       host,
			Port:       port,
			ResolvedIP: ipStr,
		}
		staticDec, _ := j.evaluateNet(req)
		if staticDec == DecisionDeny && j.isHardIPBlock(req) {
			_ = j.CheckAccess(ctx, req)
			return nil, &AccessDenialError{
				Op:     OpNetListen,
				Target: net.JoinHostPort(host, strconv.Itoa(port)),
				Reason: fmt.Sprintf("resolved IP %s blocked by network policy", ipStr),
			}
		}
		if err := j.CheckAccess(ctx, req); err != nil {
			if firstAccessErr == nil {
				firstAccessErr = err
			}
			continue
		}
		bindHost := ipa.IP.String()
		if ipa.Zone != "" {
			bindHost += "%" + ipa.Zone
		}
		target := net.JoinHostPort(bindHost, strconv.Itoa(port))
		if !slices.Contains(approvedTargets, target) {
			approvedTargets = append(approvedTargets, target)
		}
	}

	if len(approvedTargets) == 0 {
		if firstAccessErr != nil {
			return nil, firstAccessErr
		}
		return nil, &AccessDenialError{
			Op:     OpNetListen,
			Target: address,
			Reason: "no permitted listen IP addresses for host",
		}
	}
	return approvedTargets, nil
}

// --- Exec & Process Policy Evaluation ---

func (j *Jail) evaluateExec(req AccessRequest) (Decision, string) {
	realBin := req.Binary
	if realBin == "" {
		realBin = req.Path
	}
	realCanon, _ := canonicalizeForCheck(resolveHostPath("", realBin))

	var pathCands []string
	var baseCands []string
	var denyPathCands []string
	var denyBaseCands []string

	addDenyCand := func(p string) {
		if p == "" {
			return
		}
		np := normalizeMatchPath(p)
		if np != "" && !slices.Contains(denyPathCands, np) {
			denyPathCands = append(denyPathCands, np)
		}
		nb := normalizeBinBase(filepath.Base(p))
		if nb != "" && !slices.Contains(denyBaseCands, nb) {
			denyBaseCands = append(denyBaseCands, nb)
		}
	}

	addAllowCand := func(p string, allowBase bool) {
		if p == "" {
			return
		}
		np := normalizeMatchPath(p)
		if np != "" && !slices.Contains(pathCands, np) {
			pathCands = append(pathCands, np)
		}
		// Never credit bare basenames for executables located inside writable
		// jail roots/rules: otherwise writing +x /ws/git inside --jail /ws
		// would spoof AllowBinaries: ["git"]. Explicit full-path AllowBinaries
		// entries (bp.isPath == true) still match pathCands.
		if allowBase && np != "" && !j.isInsideWritableRoot(np) {
			nb := normalizeBinBase(filepath.Base(p))
			if nb != "" && !slices.Contains(baseCands, nb) {
				baseCands = append(baseCands, nb)
			}
		}
	}

	addDenyCand(realCanon)
	addAllowCand(realCanon, true)
	if req.Path != "" {
		pCanon, pLex := canonicalizeForCheck(resolveHostPath("", req.Path))
		addDenyCand(pCanon)
		addDenyCand(pLex)
		addAllowCand(pCanon, true)
		if npLex := normalizeMatchPath(pLex); npLex != "" && !j.isInsideWritableRoot(npLex) {
			addAllowCand(pLex, false)
		}
	}

	// DenyBinaries always check RawPath too so symlinking or invoking a denied
	// alias name is caught immediately.
	if req.RawPath != "" {
		rawParentCanon, _ := canonicalizeParentForCheck(resolveHostPath(req.WorkDir, req.RawPath))
		addDenyCand(rawParentCanon)
		np := normalizeMatchPath(rawParentCanon)
		// Only credit RawPath basename for AllowBinaries when the symlink itself
		// lives OUTSIDE writable jail Roots (e.g. OS /bin/sh -> dash or
		// /usr/bin/python3 -> python3.12), preventing an attacker from creating
		// `ln -s /usr/bin/curl /workspace/git` inside a writable jail root!
		if np != "" && !j.isInsideWritableRoot(np) {
			addAllowCand(rawParentCanon, true)
		}
	}

	for _, dp := range j.denyBinaries {
		if dp.matches(denyPathCands, denyBaseCands) {
			return DecisionDeny, fmt.Sprintf("binary %q denied by ExecPolicy", filepath.Base(realCanon))
		}
	}
	for _, ap := range j.allowBinaries {
		if ap.matches(pathCands, baseCands) {
			return DecisionAllow, ""
		}
	}
	return j.execDefault, "external binary execution disabled by jail"
}

func (j *Jail) isInsideWritableRoot(normPath string) bool {
	if normPath == "" {
		return false
	}
	for _, r := range j.roots {
		if isWithinNormalizedRoot(normPath, r) {
			return true
		}
	}
	for i := range j.rules {
		r := &j.rules[i]
		if (r.raw.Decision == DecisionAllow || r.raw.Decision == DecisionAllowSession) &&
			(len(r.ops) == 0 || r.ops[OpWrite]) &&
			r.matches(normPath) {
			return true
		}
	}
	return false
}

func (j *Jail) evaluateProc(_ AccessRequest) (Decision, string) {
	return j.procDefault, "host process access disabled by jail"
}

// --- Pattern Compilation & Matching Helpers ---

func expandTildePath(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func normalizeMatchPath(p string) string {
	if p == "" {
		return ""
	}
	s := normalizeWinVerbatim(p)
	s = strings.ReplaceAll(s, `\`, "/")
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	if len(s) > 1 && strings.HasSuffix(s, "/") && !(len(s) == 3 && s[1] == ':' && s[2] == '/') {
		s = strings.TrimSuffix(s, "/")
	}
	if runtime.GOOS == "windows" {
		s = strings.ToLower(s)
	}
	return s
}

func isWithinNormalizedRoot(cand, root string) bool {
	if cand == "" || root == "" {
		return false
	}
	if cand == root {
		return true
	}
	if root == "/" {
		return strings.HasPrefix(cand, "/")
	}
	if len(root) == 3 && root[1] == ':' && root[2] == '/' {
		return strings.HasPrefix(cand, root)
	}
	return strings.HasPrefix(cand, root+"/")
}

func hasGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

func compilePathRule(rule PathRule, baseRoots []string) (compiledPathRule, error) {
	pat := strings.TrimSpace(rule.Pattern)
	if pat == "" {
		return compiledPathRule{}, fmt.Errorf("empty PathRule pattern")
	}
	cr := compiledPathRule{raw: rule}
	if len(rule.Ops) > 0 {
		cr.ops = make(map[AccessOp]bool, len(rule.Ops))
		for _, op := range rule.Ops {
			cr.ops[op] = true
		}
	}

	expanded := expandTildePath(pat)
	slashExpanded := strings.ReplaceAll(expanded, `\`, "/")

	if !hasGlobMeta(slashExpanded) {
		cr.hasGlob = false
		addRoot := func(raw string) {
			realP, lexP := canonicalizeForCheck(resolveHostPath("", raw))
			if nr := normalizeMatchPath(realP); nr != "" && !slices.Contains(cr.exactRoots, nr) {
				cr.exactRoots = append(cr.exactRoots, nr)
			}
			if nl := normalizeMatchPath(lexP); nl != "" && !slices.Contains(cr.exactRoots, nl) {
				cr.exactRoots = append(cr.exactRoots, nl)
			}
		}
		if isRawAbsOrMSYS(expanded) {
			addRoot(expanded)
		} else if len(baseRoots) > 0 {
			for _, br := range baseRoots {
				addRoot(filepath.Join(br, expanded))
			}
		} else {
			addRoot(expanded)
		}
		return cr, nil
	}

	cr.hasGlob = true
	var globPatterns []string
	addGlob := func(g string) {
		g = strings.ReplaceAll(g, `\`, "/")
		for strings.Contains(g, "//") {
			g = strings.ReplaceAll(g, "//", "/")
		}
		if len(g) > 1 {
			g = strings.TrimSuffix(g, "/")
		}
		if runtime.GOOS == "windows" {
			g = strings.ToLower(g)
		}
		if g != "" && !slices.Contains(globPatterns, g) {
			globPatterns = append(globPatterns, g)
		}
	}

	if strings.HasPrefix(slashExpanded, "**/") || slashExpanded == "**" {
		addGlob(slashExpanded)
	} else if isRawAbsOrMSYS(expanded) {
		hostExpanded := resolveHostPath("", expanded)
		addGlob(hostExpanded)
		// Also canonicalize the static directory prefix before the first glob
		// metacharacter so OS symlinks (e.g. macOS /var -> /private/var) never
		// bypass absolute or ~/ glob rules!
		if canonVariant, ok := canonicalizeStaticGlobPrefix(hostExpanded); ok {
			addGlob(canonVariant)
		}
	} else if len(baseRoots) > 0 {
		for _, br := range baseRoots {
			joined := filepath.Join(br, expanded)
			addGlob(joined)
			if canonVariant, ok := canonicalizeStaticGlobPrefix(joined); ok {
				addGlob(canonVariant)
			}
		}
	} else {
		addGlob("**/" + slashExpanded)
	}

	for _, gp := range globPatterns {
		cr.globSegs = append(cr.globSegs, splitNormalizedSegments(gp))
	}
	return cr, nil
}

func canonicalizeStaticGlobPrefix(p string) (string, bool) {
	s := strings.ReplaceAll(p, `\`, "/")
	idx := strings.IndexAny(s, "*?[")
	if idx <= 0 {
		return "", false
	}
	slashIdx := strings.LastIndexByte(s[:idx], '/')
	if slashIdx <= 0 {
		return "", false
	}
	prefix := s[:slashIdx]
	suffix := s[slashIdx:]
	realPrefix, _ := canonicalizeForCheck(filepath.FromSlash(prefix))
	if realPrefix == "" {
		return "", false
	}
	return strings.ReplaceAll(realPrefix, `\`, "/") + suffix, true
}

func splitNormalizedSegments(norm string) []string {
	return strings.Split(norm, "/")
}

func (cr *compiledPathRule) matches(normPath string) bool {
	if !cr.hasGlob {
		for _, er := range cr.exactRoots {
			if isWithinNormalizedRoot(normPath, er) {
				return true
			}
		}
		return false
	}
	candSegs := splitNormalizedSegments(normPath)
	for _, patSegs := range cr.globSegs {
		if matchPathSegments(patSegs, candSegs) {
			return true
		}
	}
	return false
}

// matchPathSegments matches slash-split glob pattern segments (supporting `**`
// across zero or more path segments, and `*`, `?`, `[...]` within a segment)
// in O(P * S) time with zero backtracking blowup.
func matchPathSegments(pat, cand []string) bool {
	pLen, cLen := len(pat), len(cand)
	// prev[j] = pat[:i] matches cand[:j]
	prev := make([]bool, cLen+1)
	curr := make([]bool, cLen+1)
	prev[0] = true

	for i := 0; i < pLen; i++ {
		clear(curr)
		seg := pat[i]
		if seg == "**" {
			// `**` matches 0 segments (curr[j] = prev[j]) or 1+ segments (curr[j] = curr[j-1]).
			curr[0] = prev[0]
			for j := 1; j <= cLen; j++ {
				curr[j] = prev[j] || curr[j-1]
			}
		} else {
			for j := 1; j <= cLen; j++ {
				if prev[j-1] && matchSingleSegmentGlob(seg, cand[j-1]) {
					curr[j] = true
				}
			}
		}
		prev, curr = curr, prev
	}
	return prev[cLen]
}

// matchSingleSegmentGlob matches a single path segment (or env key) against a
// pattern supporting `*`, `?`, and `[...]` character classes.
func matchSingleSegmentGlob(pat, str string) bool {
	px, sx := 0, 0
	nextPx, nextSx := 0, 0
	for px < len(pat) || sx < len(str) {
		if px < len(pat) {
			switch pat[px] {
			case '*':
				nextPx = px
				nextSx = sx + 1
				px++
				continue
			case '?':
				if sx < len(str) {
					px++
					sx++
					continue
				}
			case '[':
				if sx < len(str) {
					if matched, newPx, valid := matchBracketClass(pat, px, str[sx]); valid {
						if matched {
							px = newPx
							sx++
							continue
						}
					} else if pat[px] == str[sx] {
						px++
						sx++
						continue
					}
				}
			case '\\':
				if runtime.GOOS != "windows" && px+1 < len(pat) {
					px++
				}
				if sx < len(str) && pat[px] == str[sx] {
					px++
					sx++
					continue
				}
			default:
				if sx < len(str) && pat[px] == str[sx] {
					px++
					sx++
					continue
				}
			}
		}
		if nextSx > 0 && nextSx <= len(str) {
			px = nextPx
			sx = nextSx
			continue
		}
		return false
	}
	return true
}

func matchBracketClass(pat string, start int, ch byte) (matched bool, nextIdx int, valid bool) {
	i := start + 1
	if i >= len(pat) {
		return false, 0, false
	}
	neg := false
	if pat[i] == '!' || pat[i] == '^' {
		neg = true
		i++
	}
	first := true
	hit := false
	for i < len(pat) {
		if pat[i] == ']' && !first {
			if neg {
				hit = !hit
			}
			return hit, i + 1, true
		}
		first = false
		lo := pat[i]
		if i+2 < len(pat) && pat[i+1] == '-' && pat[i+2] != ']' {
			hi := pat[i+2]
			if lo <= ch && ch <= hi {
				hit = true
			}
			i += 3
			continue
		}
		if ch == lo {
			hit = true
		}
		i++
	}
	return false, 0, false
}

// --- Network Compilation & IP Classification ---

func compileNetRule(nr NetRule) (compiledNetRule, error) {
	cnr := compiledNetRule{
		raw:         nr,
		proto:       normalizeNetProto(nr.Proto),
		hostPattern: normalizeHostname(nr.HostPattern),
		portMin:     nr.PortRange[0],
		portMax:     nr.PortRange[1],
	}
	if cnr.proto != "" && cnr.proto != "tcp" && cnr.proto != "udp" && !strings.HasPrefix(cnr.proto, "unix") {
		return compiledNetRule{}, fmt.Errorf("invalid NetRule proto %q", nr.Proto)
	}
	if nr.CIDR != "" {
		pfx, err := netip.ParsePrefix(strings.TrimSpace(nr.CIDR))
		if err != nil {
			return compiledNetRule{}, fmt.Errorf("invalid NetRule CIDR %q: %w", nr.CIDR, err)
		}
		cnr.hasPrefix = true
		cnr.prefix = pfx.Masked()
	}
	for _, p := range nr.Ports {
		if p < 0 || p > 65535 {
			return compiledNetRule{}, fmt.Errorf("invalid NetRule port %d", p)
		}
		if cnr.ports == nil {
			cnr.ports = make(map[int]bool, len(nr.Ports))
		}
		cnr.ports[p] = true
	}
	if cnr.portMin != 0 || cnr.portMax != 0 {
		if cnr.portMin < 0 || cnr.portMax > 65535 || cnr.portMin > cnr.portMax {
			return compiledNetRule{}, fmt.Errorf("invalid NetRule PortRange [%d, %d]", cnr.portMin, cnr.portMax)
		}
	}
	if len(nr.Ops) > 0 {
		cnr.ops = make(map[AccessOp]bool, len(nr.Ops))
		for _, op := range nr.Ops {
			cnr.ops[op] = true
		}
	}
	return cnr, nil
}

func (cnr *compiledNetRule) matchesOpProtoPort(op AccessOp, proto string, port int) bool {
	if len(cnr.ops) > 0 && !cnr.ops[op] {
		return false
	}
	if cnr.proto != "" && cnr.proto != proto {
		return false
	}
	hasPortConstraint := len(cnr.ports) > 0 || cnr.portMin > 0 || cnr.portMax > 0
	if !hasPortConstraint {
		return true
	}
	if cnr.ports[port] {
		return true
	}
	if (cnr.portMin > 0 || cnr.portMax > 0) && port >= cnr.portMin && port <= cnr.portMax {
		return true
	}
	return false
}

func normalizeNetProto(network string) string {
	n := strings.ToLower(strings.TrimSpace(network))
	switch n {
	case "tcp", "tcp4", "tcp6":
		return "tcp"
	case "udp", "udp4", "udp6":
		return "udp"
	default:
		return n
	}
}

func normalizeHostname(h string) string {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	h = strings.TrimSuffix(h, ".")
	return strings.ToLower(h)
}

func parseNormalizedIP(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func isIPLiteralPattern(pat string) bool {
	_, ok := parseNormalizedIP(pat)
	return ok
}

func matchHostPattern(pat, host string, addr netip.Addr, hasAddr bool) bool {
	if pat == "*" {
		return true
	}
	if patAddr, ok := parseNormalizedIP(pat); ok {
		if hasAddr && patAddr == addr {
			return true
		}
		if hostAddr, ok2 := parseNormalizedIP(host); ok2 && patAddr == hostAddr {
			return true
		}
		return false
	}
	if host == "" {
		return false
	}
	if strings.HasPrefix(pat, "*.") {
		suffix := pat[1:] // ".example.com"
		return len(host) > len(suffix) && strings.HasSuffix(host, suffix)
	}
	if hasGlobMeta(pat) {
		return matchSingleSegmentGlob(pat, host)
	}
	return host == pat
}

var (
	prefixThisNetV4   = netip.MustParsePrefix("0.0.0.0/8")
	prefixRFC1918_10  = netip.MustParsePrefix("10.0.0.0/8")
	prefixCGNAT       = netip.MustParsePrefix("100.64.0.0/10")
	prefixRFC1918_172 = netip.MustParsePrefix("172.16.0.0/12")
	prefixRFC1918_192 = netip.MustParsePrefix("192.168.0.0/16")
	prefixLinkLocalV4 = netip.MustParsePrefix("169.254.0.0/16")
	prefixIPv6ULA     = netip.MustParsePrefix("fc00::/7")
	prefixIPv6LinkLoc = netip.MustParsePrefix("fe80::/10")
	prefixNAT64       = netip.MustParsePrefix("64:ff9b::/96")
)

func isPrivateOrLinkLocalLAN(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	if addr.Is6() && prefixNAT64.Contains(addr) {
		return true
	}
	u := addr.Unmap()
	if u.Is4() {
		return prefixRFC1918_10.Contains(u) ||
			prefixCGNAT.Contains(u) ||
			prefixRFC1918_172.Contains(u) ||
			prefixRFC1918_192.Contains(u) ||
			prefixLinkLocalV4.Contains(u)
	}
	return prefixIPv6ULA.Contains(u) ||
		prefixIPv6LinkLoc.Contains(u) ||
		prefixNAT64.Contains(u)
}

func isLoopbackAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	return addr.Unmap().IsLoopback()
}

func isUnspecifiedOrThisNetAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	u := addr.Unmap()
	if u.IsUnspecified() {
		return true
	}
	return u.Is4() && prefixThisNetV4.Contains(u)
}

func isLoopbackOrUnspecifiedAddr(addr netip.Addr) bool {
	return isLoopbackAddr(addr) || isUnspecifiedOrThisNetAddr(addr)
}

func resolvePortNumber(ctx context.Context, proto, portStr string) (int, error) {
	if p, err := strconv.Atoi(portStr); err == nil {
		if p < 0 || p > 65535 {
			return 0, fmt.Errorf("invalid port %d", p)
		}
		return p, nil
	}
	if proto == "" {
		proto = "tcp"
	}
	return net.DefaultResolver.LookupPort(ctx, proto, portStr)
}

func filterIPAddrsByNetwork(network string, addrs []net.IPAddr) []net.IPAddr {
	n := strings.ToLower(network)
	wantV4 := strings.HasSuffix(n, "4")
	wantV6 := strings.HasSuffix(n, "6")
	if !wantV4 && !wantV6 {
		return addrs
	}
	var out []net.IPAddr
	for _, a := range addrs {
		is4 := a.IP.To4() != nil
		if wantV4 && is4 {
			out = append(out, a)
		} else if wantV6 && !is4 {
			out = append(out, a)
		}
	}
	return out
}

// --- Binary Matching Helpers ---

func normalizeBinBase(name string) string {
	name = strings.TrimSpace(name)
	if runtime.GOOS == "windows" {
		lower := strings.ToLower(name)
		for _, ext := range []string{".exe", ".com", ".bat", ".cmd"} {
			if strings.HasSuffix(lower, ext) && len(lower) > len(ext) {
				return lower[:len(lower)-len(ext)]
			}
		}
		return lower
	}
	return name
}

func compileBinPattern(raw string) (compiledBinPattern, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return compiledBinPattern{}, false
	}
	if strings.ContainsAny(s, `/\`) || filepath.IsAbs(s) || strings.HasPrefix(s, "~") {
		expanded := expandTildePath(s)
		realBin, _ := canonicalizeForCheck(resolveHostPath("", expanded))
		return compiledBinPattern{
			isPath:   true,
			fullPath: normalizeMatchPath(realBin),
		}, true
	}
	return compiledBinPattern{
		isPath:   false,
		baseName: normalizeBinBase(s),
	}, true
}

func (bp compiledBinPattern) matches(pathCands, baseCands []string) bool {
	if bp.isPath {
		return slices.Contains(pathCands, bp.fullPath)
	}
	return slices.Contains(baseCands, bp.baseName)
}
