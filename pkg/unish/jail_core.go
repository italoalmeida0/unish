package unish

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// AccessOp classifies a capability-gated host operation.
type AccessOp string

const (
	OpRead       AccessOp = "read"
	OpWrite      AccessOp = "write"
	OpDelete     AccessOp = "delete"
	OpList       AccessOp = "list"
	OpStat       AccessOp = "stat"
	OpChdir      AccessOp = "chdir"
	OpLink       AccessOp = "link"
	OpNetConnect AccessOp = "net_connect"
	OpNetListen  AccessOp = "net_listen"
	OpExec       AccessOp = "exec"
	OpProc       AccessOp = "proc"
)

// Decision represents an access policy or interactive approval verdict.
type Decision int

const (
	DecisionAsk Decision = iota
	DecisionAllow
	DecisionAllowSession
	DecisionDeny
)

func (d Decision) String() string {
	switch d {
	case DecisionAsk:
		return "ask"
	case DecisionAllow:
		return "allow"
	case DecisionAllowSession:
		return "allow_session"
	case DecisionDeny:
		return "deny"
	default:
		return fmt.Sprintf("Decision(%d)", int(d))
	}
}

// AccessRequest describes an attempted filesystem, network, exec, or process
// operation passed to CapabilityHook.CheckAccess.
type AccessRequest struct {
	Op         AccessOp
	Command    string
	Path       string // canonicalized real path
	RawPath    string
	TargetPath string
	Proto      string
	Host       string
	Port       int
	ResolvedIP string
	Binary     string
	Argv       []string
	WorkDir    string
	PID        int
	Signal     string
}

// AccessDenialError is returned when a CapabilityHook denies an operation.
// Unwrap always returns os.ErrPermission (== fs.ErrPermission) so standard
// permission checks across builtins and callers work uniformly.
type AccessDenialError struct {
	Op     AccessOp
	Target string
	Reason string
}

func (e AccessDenialError) Error() string {
	var b strings.Builder
	b.WriteString("permission denied")
	if e.Op != "" {
		b.WriteString(" (")
		b.WriteString(string(e.Op))
		b.WriteByte(')')
	}
	if e.Target != "" {
		b.WriteString(": ")
		b.WriteString(e.Target)
	}
	if e.Reason != "" {
		b.WriteString(": ")
		b.WriteString(e.Reason)
	}
	return b.String()
}

func (e AccessDenialError) Unwrap() error {
	return os.ErrPermission
}

// CapabilityHook intercepts filesystem, network, process, and external exec
// operations within a shell execution context.
type CapabilityHook interface {
	CheckAccess(ctx context.Context, req AccessRequest) error
	DialContext(ctx context.Context, cmdName, network, address string) (net.Conn, error)
	Listen(ctx context.Context, cmdName, network, address string) (net.Listener, error)
	ListenPacket(ctx context.Context, cmdName, network, address string) (net.PacketConn, error)
}

type capabilityHookKey struct{}

// hasActiveCapabilityHooks stays false until WithCapabilityHook is called with
// a non-nil hook, guaranteeing a single atomic load (zero context-chain walk)
// on every operation in normal CLI mode.
var hasActiveCapabilityHooks atomic.Bool

// WithCapabilityHook attaches h to ctx (pass nil to clear an inherited hook).
func WithCapabilityHook(ctx context.Context, h CapabilityHook) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if h != nil {
		hasActiveCapabilityHooks.Store(true)
	}
	return context.WithValue(ctx, capabilityHookKey{}, h)
}

// CapabilityHookFromContext retrieves the active CapabilityHook from ctx, or
// nil when none is installed.
func CapabilityHookFromContext(ctx context.Context) CapabilityHook {
	if !hasActiveCapabilityHooks.Load() || ctx == nil {
		return nil
	}
	h, _ := ctx.Value(capabilityHookKey{}).(CapabilityHook)
	return h
}

func isNullDevPath(p string) bool {
	if p == "/dev/null" {
		return true
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(p, "nul") || strings.EqualFold(p, `\\.\nul`)
	}
	return false
}

func isRawAbsOrMSYS(p string) bool {
	if p == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		if p[0] == '/' || p[0] == '\\' {
			if _, ok := msysPath(p); ok {
				return true
			}
		}
	}
	return filepath.IsAbs(p)
}

func normalizeWinVerbatim(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	if strings.HasPrefix(p, `\\?\UNC\`) {
		return `\\` + p[len(`\\?\UNC\`):]
	}
	if len(p) >= 6 && strings.HasPrefix(p, `\\?\`) && isASCIILetter(p[4]) && p[5] == ':' {
		return p[4:]
	}
	return p
}

func sameCleanPath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// resolveHostPath translates MSYS/null-device paths and joins relative paths
// against dir (when dir != "") without consulting process-global os.Getwd().
func resolveHostPath(dir, p string) string {
	if p == "" || p == "-" || p == "/dev/stdin" || p == "/dev/stdout" || p == "/dev/stderr" {
		return p
	}
	if p == "/dev/null" {
		if runtime.GOOS == "windows" {
			return "NUL"
		}
		return "/dev/null"
	}
	if runtime.GOOS == "windows" {
		if strings.EqualFold(p, "nul") || strings.EqualFold(p, `\\.\nul`) {
			return "NUL"
		}
		if p[0] == '/' || p[0] == '\\' {
			if w, ok := msysPath(p); ok {
				return w
			}
		}
		if filepath.IsAbs(p) || dir == "" {
			return p
		}
		return filepath.Join(dir, p)
	}
	if filepath.IsAbs(p) || dir == "" {
		return p
	}
	return filepath.Join(dir, p)
}

// canonicalizeForCheck cleans absPath and resolves symlinks (including dangling
// symlinks and symlinks along ancestor directories of not-yet-existing paths)
// so symlink escapes are detected BEFORE opening or creating files.
func canonicalizeForCheck(absPath string) (realPath string, lexicalPath string) {
	if absPath == "" {
		return "", ""
	}
	if runtime.GOOS == "windows" && (strings.EqualFold(absPath, "nul") || strings.EqualFold(absPath, `\\.\nul`)) {
		return "NUL", "NUL"
	}
	if !filepath.IsAbs(absPath) {
		if abs, err := filepath.Abs(absPath); err == nil {
			absPath = abs
		}
	}
	lexicalPath = normalizeWinVerbatim(filepath.Clean(absPath))

	if eval, err := filepath.EvalSymlinks(lexicalPath); err == nil {
		return normalizeWinVerbatim(filepath.Clean(eval)), lexicalPath
	}

	const maxSymlinkHops = 40
	cur := lexicalPath
	var tail []string
	for hops := 0; hops < maxSymlinkHops; {
		if eval, err := filepath.EvalSymlinks(cur); err == nil {
			real := normalizeWinVerbatim(filepath.Clean(eval))
			for i := len(tail) - 1; i >= 0; i-- {
				real = filepath.Join(real, tail[i])
			}
			return filepath.Clean(real), lexicalPath
		}
		// If cur itself is a dangling symlink (or points into a non-existent
		// directory), expand its target manually so writes through dangling
		// symlinks cannot escape a jail.
		if fi, lerr := os.Lstat(cur); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			if target, rerr := os.Readlink(cur); rerr == nil && target != "" {
				hops++
				target = normalizeWinVerbatim(target)
				if runtime.GOOS == "windows" && (target[0] == '/' || target[0] == '\\') && !filepath.IsAbs(target) {
					if w, ok := msysPath(target); ok {
						target = w
					} else if vol := filepath.VolumeName(cur); vol != "" {
						target = vol + target
					}
				}
				var next string
				if filepath.IsAbs(target) {
					next = filepath.Clean(target)
				} else {
					next = filepath.Clean(filepath.Join(filepath.Dir(cur), target))
				}
				for i := len(tail) - 1; i >= 0; i-- {
					next = filepath.Join(next, tail[i])
				}
				cur = filepath.Clean(next)
				tail = tail[:0]
				continue
			}
		}
		parent := filepath.Dir(cur)
		base := filepath.Base(cur)
		if parent == cur {
			real := cur
			for i := len(tail) - 1; i >= 0; i-- {
				real = filepath.Join(real, tail[i])
			}
			return normalizeWinVerbatim(filepath.Clean(real)), lexicalPath
		}
		tail = append(tail, base)
		cur = parent
	}

	real := cur
	for i := len(tail) - 1; i >= 0; i-- {
		real = filepath.Join(real, tail[i])
	}
	return normalizeWinVerbatim(filepath.Clean(real)), lexicalPath
}

// canonicalizeParentForCheck resolves symlinks on the parent directory of
// absPath while preserving the final path element literally (for Lstat/unlink).
func canonicalizeParentForCheck(absPath string) (realPath string, lexicalPath string) {
	if absPath == "" {
		return "", ""
	}
	if runtime.GOOS == "windows" && (strings.EqualFold(absPath, "nul") || strings.EqualFold(absPath, `\\.\nul`)) {
		return "NUL", "NUL"
	}
	if !filepath.IsAbs(absPath) {
		if abs, err := filepath.Abs(absPath); err == nil {
			absPath = abs
		}
	}
	lexicalPath = normalizeWinVerbatim(filepath.Clean(absPath))
	parent := filepath.Dir(lexicalPath)
	if parent == lexicalPath {
		return canonicalizeForCheck(lexicalPath)
	}
	parentReal, _ := canonicalizeForCheck(parent)
	return filepath.Clean(filepath.Join(parentReal, filepath.Base(lexicalPath))), lexicalPath
}

// canonicalLexicalInDir normalizes OS-level symlinks/8.3 short names on dir
// itself (e.g. macOS /var -> /private/var, Windows RUNNER~1 -> runneradmin)
// while keeping all path components beneath dir lexical so deny rules on
// workspace-relative paths cannot be bypassed by intra-workspace symlinks.
func canonicalLexicalInDir(dir, rawPath, lexicalPath string) string {
	if dir != "" {
		hostDir := resolveHostPath("", dir)
		if !filepath.IsAbs(hostDir) {
			if abs, err := filepath.Abs(hostDir); err == nil {
				hostDir = abs
			}
		}
		cleanDir := normalizeWinVerbatim(filepath.Clean(hostDir))
		canonDir, _ := canonicalizeForCheck(cleanDir)
		if rawPath != "" && !isRawAbsOrMSYS(rawPath) {
			return normalizeWinVerbatim(filepath.Clean(filepath.Join(canonDir, rawPath)))
		}
		if rel, err := filepath.Rel(cleanDir, lexicalPath); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return normalizeWinVerbatim(filepath.Clean(filepath.Join(canonDir, rel)))
		}
	}
	noFollow, _ := canonicalizeParentForCheck(lexicalPath)
	return noFollow
}

// checkFS resolves rawPath against dir and enforces any active CapabilityHook.
// When no hook is active in ctx, it returns resolveHostPath(dir, rawPath) with
// zero filesystem syscalls.
func checkFS(ctx context.Context, dir, cmdName, rawPath string, op AccessOp) (string, error) {
	hook := CapabilityHookFromContext(ctx)
	if hook == nil {
		return resolveHostPath(dir, rawPath), nil
	}
	if (op == OpRead || op == OpStat) && isNullDevPath(rawPath) {
		return resolveHostPath(dir, rawPath), nil
	}
	hostPath := resolveHostPath(dir, rawPath)
	realPath, lexicalPath := canonicalizeForCheck(hostPath)
	if err := hook.CheckAccess(ctx, AccessRequest{
		Op:      op,
		Command: cmdName,
		Path:    realPath,
		RawPath: rawPath,
	}); err != nil {
		return "", err
	}
	checkLex := canonicalLexicalInDir(dir, rawPath, lexicalPath)
	if !sameCleanPath(checkLex, realPath) {
		if err := hook.CheckAccess(ctx, AccessRequest{
			Op:      op,
			Command: cmdName,
			Path:    checkLex,
			RawPath: rawPath,
		}); err != nil {
			return "", err
		}
	}
	return realPath, nil
}

// checkFSNoFollowLeaf resolves symlinks on parent directories without following
// a leaf symlink, suitable for Lstat and unlinking symlink entries.
func checkFSNoFollowLeaf(ctx context.Context, dir, cmdName, rawPath string, op AccessOp) (string, error) {
	hook := CapabilityHookFromContext(ctx)
	if hook == nil {
		return resolveHostPath(dir, rawPath), nil
	}
	if op == OpStat && isNullDevPath(rawPath) {
		return resolveHostPath(dir, rawPath), nil
	}
	hostPath := resolveHostPath(dir, rawPath)
	noFollowReal, lexicalPath := canonicalizeParentForCheck(hostPath)
	if err := hook.CheckAccess(ctx, AccessRequest{
		Op:      op,
		Command: cmdName,
		Path:    noFollowReal,
		RawPath: rawPath,
	}); err != nil {
		return "", err
	}
	checkLex := canonicalLexicalInDir(dir, rawPath, lexicalPath)
	if !sameCleanPath(checkLex, noFollowReal) {
		if err := hook.CheckAccess(ctx, AccessRequest{
			Op:      op,
			Command: cmdName,
			Path:    checkLex,
			RawPath: rawPath,
		}); err != nil {
			return "", err
		}
	}
	return noFollowReal, nil
}

func checkChdir(ctx context.Context, dir, cmdName, rawPath string) (string, error) {
	return checkFS(ctx, dir, cmdName, rawPath, OpChdir)
}

func fsOpen(ctx context.Context, dir, cmdName, rawPath string) (*os.File, error) {
	p, err := checkFS(ctx, dir, cmdName, rawPath, OpRead)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func fsOpenFile(ctx context.Context, dir, cmdName, rawPath string, flag int, perm os.FileMode) (*os.File, error) {
	if isNullDevPath(rawPath) {
		p := resolveHostPath(dir, rawPath)
		if runtime.GOOS == "windows" {
			flag &^= os.O_TRUNC
		}
		return os.OpenFile(p, flag, perm)
	}
	accMode := flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR)
	wantsWrite := accMode == os.O_WRONLY || accMode == os.O_RDWR || flag&(os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
	wantsRead := accMode == os.O_RDONLY || accMode == os.O_RDWR

	var p string
	var err error
	switch {
	case wantsRead && wantsWrite:
		if _, err = checkFS(ctx, dir, cmdName, rawPath, OpRead); err != nil {
			return nil, err
		}
		p, err = checkFS(ctx, dir, cmdName, rawPath, OpWrite)
	case wantsWrite:
		p, err = checkFS(ctx, dir, cmdName, rawPath, OpWrite)
	default:
		p, err = checkFS(ctx, dir, cmdName, rawPath, OpRead)
	}
	if err != nil {
		return nil, err
	}
	return os.OpenFile(p, flag, perm)
}

func fsCreate(ctx context.Context, dir, cmdName, rawPath string) (*os.File, error) {
	if isNullDevPath(rawPath) {
		p := resolveHostPath(dir, rawPath)
		flag := os.O_RDWR | os.O_CREATE | os.O_TRUNC
		if runtime.GOOS == "windows" {
			flag &^= os.O_TRUNC
		}
		return os.OpenFile(p, flag, 0o666)
	}
	p, err := checkFS(ctx, dir, cmdName, rawPath, OpWrite)
	if err != nil {
		return nil, err
	}
	return os.Create(p)
}

func fsStat(ctx context.Context, dir, cmdName, rawPath string) (os.FileInfo, error) {
	p, err := checkFS(ctx, dir, cmdName, rawPath, OpStat)
	if err != nil {
		return nil, err
	}
	return os.Stat(p)
}

func fsLstat(ctx context.Context, dir, cmdName, rawPath string) (os.FileInfo, error) {
	p, err := checkFSNoFollowLeaf(ctx, dir, cmdName, rawPath, OpStat)
	if err != nil {
		return nil, err
	}
	return os.Lstat(p)
}

func fsReadDir(ctx context.Context, dir, cmdName, rawPath string) ([]os.DirEntry, error) {
	p, err := checkFS(ctx, dir, cmdName, rawPath, OpList)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(p)
}

func fsReadFile(ctx context.Context, dir, cmdName, rawPath string) ([]byte, error) {
	p, err := checkFS(ctx, dir, cmdName, rawPath, OpRead)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func fsWriteFile(ctx context.Context, dir, cmdName, rawPath string, data []byte, perm os.FileMode) error {
	if isNullDevPath(rawPath) {
		return nil
	}
	p, err := checkFS(ctx, dir, cmdName, rawPath, OpWrite)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, perm)
}

func fsMkdir(ctx context.Context, dir, cmdName, rawPath string, perm os.FileMode) error {
	if CapabilityHookFromContext(ctx) == nil {
		return os.Mkdir(resolveHostPath(dir, rawPath), perm)
	}
	if _, err := checkFS(ctx, dir, cmdName, rawPath, OpWrite); err != nil {
		return err
	}
	noFollow, _ := canonicalizeParentForCheck(resolveHostPath(dir, rawPath))
	return os.Mkdir(noFollow, perm)
}

func fsMkdirAll(ctx context.Context, dir, cmdName, rawPath string, perm os.FileMode) error {
	p, err := checkFS(ctx, dir, cmdName, rawPath, OpWrite)
	if err != nil {
		return err
	}
	return os.MkdirAll(p, perm)
}

func fsRemove(ctx context.Context, dir, cmdName, rawPath string) error {
	p, err := checkFSNoFollowLeaf(ctx, dir, cmdName, rawPath, OpDelete)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

func fsRemoveAll(ctx context.Context, dir, cmdName, rawPath string) error {
	p, err := checkFSNoFollowLeaf(ctx, dir, cmdName, rawPath, OpDelete)
	if err != nil {
		return err
	}
	return os.RemoveAll(p)
}

func fsRename(ctx context.Context, dir, cmdName, srcRaw, dstRaw string) error {
	if CapabilityHookFromContext(ctx) == nil {
		return os.Rename(resolveHostPath(dir, srcRaw), resolveHostPath(dir, dstRaw))
	}
	if _, err := checkFS(ctx, dir, cmdName, srcRaw, OpDelete); err != nil {
		return err
	}
	if _, err := checkFS(ctx, dir, cmdName, dstRaw, OpWrite); err != nil {
		return err
	}
	srcNoFollow, _ := canonicalizeParentForCheck(resolveHostPath(dir, srcRaw))
	dstNoFollow, _ := canonicalizeParentForCheck(resolveHostPath(dir, dstRaw))
	return os.Rename(srcNoFollow, dstNoFollow)
}

func fsChmod(ctx context.Context, dir, cmdName, rawPath string, mode os.FileMode) error {
	p, err := checkFS(ctx, dir, cmdName, rawPath, OpWrite)
	if err != nil {
		return err
	}
	return os.Chmod(p, mode)
}

func fsChtimes(ctx context.Context, dir, cmdName, rawPath string, atime, mtime time.Time) error {
	p, err := checkFS(ctx, dir, cmdName, rawPath, OpWrite)
	if err != nil {
		return err
	}
	return os.Chtimes(p, atime, mtime)
}

func fsSymlink(ctx context.Context, dir, cmdName, oldname, newname string) error {
	hook := CapabilityHookFromContext(ctx)
	if hook == nil {
		return os.Symlink(oldname, resolveHostPath(dir, newname))
	}
	newPath, err := checkFSNoFollowLeaf(ctx, dir, cmdName, newname, OpWrite)
	if err != nil {
		return err
	}
	targetResolved := resolveHostPath(filepath.Dir(newPath), oldname)
	targetReal, targetLex := canonicalizeForCheck(targetResolved)
	if err := hook.CheckAccess(ctx, AccessRequest{
		Op:         OpLink,
		Command:    cmdName,
		Path:       targetReal,
		RawPath:    oldname,
		TargetPath: newPath,
	}); err != nil {
		return err
	}
	checkLex := canonicalLexicalInDir(filepath.Dir(newPath), oldname, targetLex)
	if !sameCleanPath(checkLex, targetReal) {
		if err := hook.CheckAccess(ctx, AccessRequest{
			Op:         OpLink,
			Command:    cmdName,
			Path:       checkLex,
			RawPath:    oldname,
			TargetPath: newPath,
		}); err != nil {
			return err
		}
	}
	return os.Symlink(oldname, newPath)
}

func fsLink(ctx context.Context, dir, cmdName, oldname, newname string) error {
	hook := CapabilityHookFromContext(ctx)
	if hook == nil {
		return os.Link(resolveHostPath(dir, oldname), resolveHostPath(dir, newname))
	}
	oldHost := resolveHostPath(dir, oldname)
	oldReal, oldLex := canonicalizeForCheck(oldHost)
	newPath, err := checkFSNoFollowLeaf(ctx, dir, cmdName, newname, OpWrite)
	if err != nil {
		return err
	}
	if err := hook.CheckAccess(ctx, AccessRequest{
		Op:         OpLink,
		Command:    cmdName,
		Path:       oldReal,
		RawPath:    oldname,
		TargetPath: newPath,
	}); err != nil {
		return err
	}
	checkLex := canonicalLexicalInDir(dir, oldname, oldLex)
	if !sameCleanPath(checkLex, oldReal) {
		if err := hook.CheckAccess(ctx, AccessRequest{
			Op:         OpLink,
			Command:    cmdName,
			Path:       checkLex,
			RawPath:    oldname,
			TargetPath: newPath,
		}); err != nil {
			return err
		}
	}
	return os.Link(oldReal, newPath)
}

func fsCreateTemp(ctx context.Context, dir, cmdName, tempDir, pattern string) (*os.File, error) {
	hook := CapabilityHookFromContext(ctx)
	if hook == nil {
		if tempDir != "" {
			tempDir = resolveHostPath(dir, tempDir)
		}
		return os.CreateTemp(tempDir, pattern)
	}
	rawDir := tempDir
	if rawDir == "" {
		rawDir = os.TempDir()
	}
	checkedDir, err := checkFS(ctx, dir, cmdName, rawDir, OpWrite)
	if err != nil {
		return nil, err
	}
	return os.CreateTemp(checkedDir, pattern)
}

func fsMkdirTemp(ctx context.Context, dir, cmdName, tempDir, pattern string) (string, error) {
	hook := CapabilityHookFromContext(ctx)
	if hook == nil {
		if tempDir != "" {
			tempDir = resolveHostPath(dir, tempDir)
		}
		return os.MkdirTemp(tempDir, pattern)
	}
	rawDir := tempDir
	if rawDir == "" {
		rawDir = os.TempDir()
	}
	checkedDir, err := checkFS(ctx, dir, cmdName, rawDir, OpWrite)
	if err != nil {
		return "", err
	}
	return os.MkdirTemp(checkedDir, pattern)
}

func netDial(ctx context.Context, cmdName, network, address string, timeout time.Duration) (net.Conn, error) {
	hook := CapabilityHookFromContext(ctx)
	if hook == nil {
		var d net.Dialer
		if timeout > 0 {
			d.Timeout = timeout
		}
		return d.DialContext(ctx, network, address)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return hook.DialContext(ctx, cmdName, network, address)
}

func netListen(ctx context.Context, cmdName, network, address string) (net.Listener, error) {
	hook := CapabilityHookFromContext(ctx)
	if hook == nil {
		var lc net.ListenConfig
		return lc.Listen(ctx, network, address)
	}
	return hook.Listen(ctx, cmdName, network, address)
}

func netListenPacket(ctx context.Context, cmdName, network, address string) (net.PacketConn, error) {
	hook := CapabilityHookFromContext(ctx)
	if hook == nil {
		var lc net.ListenConfig
		return lc.ListenPacket(ctx, network, address)
	}
	return hook.ListenPacket(ctx, cmdName, network, address)
}

func checkExec(ctx context.Context, cmdName, binaryPath string, argv []string, workDir string) (string, error) {
	hook := CapabilityHookFromContext(ctx)
	if hook == nil {
		return binaryPath, nil
	}
	resolvedBin := resolveHostPath(workDir, binaryPath)
	realBin, lexBin := canonicalizeForCheck(resolvedBin)
	var realWorkDir string
	if workDir != "" {
		realWorkDir, _ = canonicalizeForCheck(resolveHostPath("", workDir))
	}
	argvCopy := append([]string(nil), argv...)
	if err := hook.CheckAccess(ctx, AccessRequest{
		Op:      OpExec,
		Command: cmdName,
		Path:    realBin,
		RawPath: binaryPath,
		Binary:  realBin,
		Argv:    argvCopy,
		WorkDir: realWorkDir,
	}); err != nil {
		return "", err
	}
	checkLex := canonicalLexicalInDir(workDir, binaryPath, lexBin)
	if !sameCleanPath(checkLex, realBin) {
		if err := hook.CheckAccess(ctx, AccessRequest{
			Op:      OpExec,
			Command: cmdName,
			Path:    checkLex,
			RawPath: binaryPath,
			Binary:  realBin,
			Argv:    argvCopy,
			WorkDir: realWorkDir,
		}); err != nil {
			return "", err
		}
	}
	return realBin, nil
}

func checkProc(ctx context.Context, cmdName string, pid int, sig string) error {
	hook := CapabilityHookFromContext(ctx)
	if hook == nil {
		return nil
	}
	return hook.CheckAccess(ctx, AccessRequest{
		Op:      OpProc,
		Command: cmdName,
		PID:     pid,
		Signal:  sig,
	})
}
