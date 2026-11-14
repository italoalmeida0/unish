package unish

// Process-substitution bypass for Windows.
//
// mvdan/sh implements `<(...)` / `>(...)` via FIFOs, which do not exist
// on Windows ("TODO: support process substitution on Windows"). This file
// provides a ProcSubstHandler that materializes the substitution as a
// temp file instead, with synchronization so ordering matches FIFOs:
//
//   - `<(cmd)` (CmdIn): the producer subshell writes the temp file; the
//     consumer's open (OpenConsumer) blocks until the producer finishes,
//     so `diff <(a) <(b)` sees complete output.
//   - `>(cmd)` (CmdOut): the consumer writes the temp file; the
//     producer's stdin open blocks until the consumer closes, so cmd
//     reads complete input.
//
// On POSIX the default handler is kept (pipes, no behavior change).

import (
	"context"
	"io"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// winProcSubstRegistry maps temp paths to their substitution state so
// that builtin commands (which open paths directly, bypassing
// OpenConsumer) can wait for the producer like the shell does.
var winProcSubstRegistry = struct {
	mu sync.Mutex
	m  map[string]*winProcSubst
}{m: map[string]*winProcSubst{}}

func winProcSubstRegister(st *winProcSubst) {
	winProcSubstRegistry.mu.Lock()
	winProcSubstRegistry.m[st.name] = st
	// Sweep stale entries (producer done, no open consumers, file
	// already deleted) so the registry doesn't grow without bound.
	// NOTE: entries are NOT removed in Cleanup: the consumer may open
	// the path after the producer finishes (registry must still resolve
	// it for waiting + refcounted delete).
	for name, e := range winProcSubstRegistry.m {
		select {
		case <-e.producerDone:
			e.mu.Lock()
			idle := e.openCount <= 0
			e.mu.Unlock()
			if idle {
				if _, err := os.Stat(name); os.IsNotExist(err) {
					delete(winProcSubstRegistry.m, name)
				}
			}
		default:
		}
	}
	winProcSubstRegistry.mu.Unlock()
}

type procSubstScopeKey struct{}

func procSubstScopeFromHC(hc interp.HandlerContext) uintptr {
	rv := reflect.ValueOf(hc)
	if !rv.IsValid() || rv.Kind() != reflect.Struct {
		return 0
	}
	rf := rv.FieldByName("runner")
	if !rf.IsValid() || rf.Kind() != reflect.Pointer || rf.IsNil() {
		return 0
	}
	pf := rf.Elem().FieldByName("procSubsts")
	if !pf.IsValid() || pf.Kind() != reflect.Pointer || pf.IsNil() {
		return 0
	}
	return pf.Pointer()
}

func procSubstRunnerScope(ctx context.Context) (scope uintptr) {
	if ctx == nil {
		return 0
	}
	if id, ok := ctx.Value(procSubstScopeKey{}).(uintptr); ok && id != 0 {
		return id
	}
	defer func() {
		if recover() != nil {
			scope = 0
		}
	}()
	return procSubstScopeFromHC(interp.HandlerCtx(ctx))
}

func procSubstEntryScope(ctx context.Context, h CapabilityHook) uintptr {
	if h == nil || reflect.TypeOf(h).Comparable() {
		return 0
	}
	return procSubstRunnerScope(ctx)
}

func withProcSubstScope(ctx context.Context, hc interp.HandlerContext) context.Context {
	h := CapabilityHookFromContext(ctx)
	if h == nil || reflect.TypeOf(h).Comparable() {
		return ctx
	}
	if scope := procSubstScopeFromHC(hc); scope != 0 {
		return context.WithValue(ctx, procSubstScopeKey{}, scope)
	}
	return ctx
}

// sameProcSubstOwner reports whether a process-substitution entry created under
// (entryHook, entryScope) may be consumed without checkFS by ctx.
// When reqHook is nil (unjailed consumer), any active procsubst in this process
// is permitted. When reqHook is non-nil (jailed consumer), entryHook must be
// non-nil and match reqHook's identity, safely handling non-comparable hook
// concrete types (including closures) without panicking or cross-session leaks.
func sameProcSubstOwner(entryHook CapabilityHook, entryScope uintptr, ctx context.Context) bool {
	reqHook := CapabilityHookFromContext(ctx)
	if reqHook == nil {
		return true
	}
	if entryHook == nil {
		return false
	}
	t1 := reflect.TypeOf(entryHook)
	t2 := reflect.TypeOf(reqHook)
	if t1 != t2 {
		return false
	}
	if t1.Comparable() {
		return entryHook == reqHook
	}
	reqScope := procSubstRunnerScope(ctx)
	if entryScope == 0 || reqScope == 0 || entryScope != reqScope {
		return false
	}
	return sameNonComparableValue(reflect.ValueOf(entryHook), reflect.ValueOf(reqHook))
}

func sameNonComparableValue(v1, v2 reflect.Value) bool {
	if !v1.IsValid() || !v2.IsValid() || v1.Type() != v2.Type() {
		return false
	}
	switch v1.Kind() {
	case reflect.Func, reflect.Map:
		if v1.IsNil() || v2.IsNil() {
			return v1.IsNil() && v2.IsNil()
		}
		return v1.Pointer() == v2.Pointer()
	case reflect.Slice:
		if v1.IsNil() || v2.IsNil() {
			return v1.IsNil() && v2.IsNil()
		}
		return v1.Len() == v2.Len() && v1.Pointer() == v2.Pointer()
	case reflect.Struct:
		for i := 0; i < v1.NumField(); i++ {
			f1, f2 := v1.Field(i), v2.Field(i)
			if f1.Type().Comparable() && f1.CanInterface() && f2.CanInterface() {
				if f1.Interface() != f2.Interface() {
					return false
				}
				continue
			}
			if !sameNonComparableValue(f1, f2) {
				return false
			}
		}
		return true
	case reflect.Array:
		for i := 0; i < v1.Len(); i++ {
			if !sameNonComparableValue(v1.Index(i), v2.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Interface:
		if v1.IsNil() || v2.IsNil() {
			return v1.IsNil() && v2.IsNil()
		}
		e1, e2 := v1.Elem(), v2.Elem()
		if e1.Type() != e2.Type() {
			return false
		}
		if e1.Type().Comparable() && e1.CanInterface() && e2.CanInterface() {
			return e1.Interface() == e2.Interface()
		}
		return sameNonComparableValue(e1, e2)
	default:
		return false
	}
}

func winProcSubstLookup(name string) *winProcSubst {
	return winProcSubstLookupCtx(context.Background(), name)
}

func winProcSubstLookupCtx(ctx context.Context, name string) *winProcSubst {
	winProcSubstRegistry.mu.Lock()
	st := winProcSubstRegistry.m[name]
	winProcSubstRegistry.mu.Unlock()
	if st == nil {
		return nil
	}
	if !sameProcSubstOwner(st.hook, st.scope, ctx) {
		return nil
	}
	return st
}

func winProcSubstRemove(name string) {
	winProcSubstRegistry.mu.Lock()
	delete(winProcSubstRegistry.m, name)
	winProcSubstRegistry.mu.Unlock()
}

type unixProcSubst struct {
	op           syntax.ProcOperator
	hook         CapabilityHook
	scope        uintptr
	openConsumer func(ctx context.Context, flag int) (io.ReadWriteCloser, error)
}

// unixProcSubstRegistry tracks active POSIX FIFO paths created by procSubstHandler
// so builtins using fsOpenShellFile/fsReadShellFile can open runtime IPC pipes
// belonging to their own CapabilityHook without tripping workspace-only jails.
var unixProcSubstRegistry = struct {
	mu sync.Mutex
	m  map[string]*unixProcSubst
}{m: map[string]*unixProcSubst{}}

func unixProcSubstRegister(path string, st *unixProcSubst) {
	unixProcSubstRegistry.mu.Lock()
	unixProcSubstRegistry.m[path] = st
	unixProcSubstRegistry.mu.Unlock()
}

func unixProcSubstLookup(ctx context.Context, path string) (*unixProcSubst, bool) {
	unixProcSubstRegistry.mu.Lock()
	st, ok := unixProcSubstRegistry.m[path]
	unixProcSubstRegistry.mu.Unlock()
	if !ok || st == nil {
		return nil, false
	}
	if !sameProcSubstOwner(st.hook, st.scope, ctx) {
		return nil, false
	}
	return st, true
}

func unixProcSubstRemove(path string) {
	unixProcSubstRegistry.mu.Lock()
	delete(unixProcSubstRegistry.m, path)
	unixProcSubstRegistry.mu.Unlock()
}

// isActiveProcSubstPath reports whether rawPath is a live process-substitution
// IPC path synthesized by procSubstHandler for ctx's CapabilityHook.
func isActiveProcSubstPath(ctx context.Context, rawPath string) bool {
	if rawPath == "" {
		return false
	}
	if winProcSubstLookupCtx(ctx, rawPath) != nil {
		return true
	}
	_, ok := unixProcSubstLookup(ctx, rawPath)
	return ok
}

// waitProcSubstProducer blocks until the producer of a `<(cmd)` process-
// substitution temp file finishes (no-op for ordinary paths or `>(cmd)`).
func waitProcSubstProducer(path string) {
	waitProcSubstProducerCtx(context.Background(), path)
}

func waitProcSubstProducerCtx(ctx context.Context, path string) {
	if !strings.Contains(path, "unish-procsub-") {
		return
	}
	if st := winProcSubstLookupCtx(ctx, path); st != nil && st.op == syntax.CmdIn {
		// Bounded: a dead producer must not hang the consumer
		// forever (30s backstop; producers are local commands).
		select {
		case <-st.producerDone:
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}

// winProcSubst carries the shared state for one substitution.
type winProcSubst struct {
	name         string
	op           syntax.ProcOperator
	hook         CapabilityHook
	scope        uintptr
	producerDone chan struct{}
	consumerDone chan struct{}
	prodOnce     sync.Once
	consOnce     sync.Once
	mu           sync.Mutex
	openCount    int
	stmtEnded    bool
	prodDoneFlag bool
	deleted      bool
}

// consumerOpen records a new consumer fd.
func (s *winProcSubst) consumerOpen() {
	s.mu.Lock()
	s.openCount++
	s.mu.Unlock()
}

// tryDeleteLocked removes the CmdIn temp file once the producer has finished,
// the enclosing statement has finished, and all opened consumer handles are closed.
// Caller must hold s.mu; unlocks s.mu before filesystem removal.
func (s *winProcSubst) tryDeleteLocked() {
	if s.op != syntax.CmdIn || !s.prodDoneFlag || !s.stmtEnded || s.openCount > 0 || s.deleted {
		s.mu.Unlock()
		return
	}
	s.deleted = true
	name := s.name
	s.mu.Unlock()
	_ = os.Remove(name)
	winProcSubstRemove(name)
}

// consumerClose records a closed CmdIn consumer fd and deletes the temp file
// once all conditions are satisfied.
func (s *winProcSubst) consumerClose() {
	s.mu.Lock()
	s.openCount--
	if s.openCount < 0 {
		s.openCount = 0
	}
	s.tryDeleteLocked()
}

// consumerWriterClose records a closed CmdOut consumer writer (`> >(cmd)` or
// `exec 3> >(cmd)` followed by `exec 3>&-`) and unblocks the reader subshell
// once no open consumer writers remain.
func (s *winProcSubst) consumerWriterClose() {
	s.mu.Lock()
	s.openCount--
	if s.openCount < 0 {
		s.openCount = 0
	}
	done := s.openCount == 0
	s.mu.Unlock()
	if done {
		s.finishConsumer()
	}
}

// noteStmtDone is invoked when the statement that expanded `<(...)` or `>(...)`
// finishes executing.
func (s *winProcSubst) noteStmtDone() {
	s.mu.Lock()
	s.stmtEnded = true
	if s.op == syntax.CmdOut {
		unblock := s.openCount == 0
		s.mu.Unlock()
		if unblock {
			s.finishConsumer()
		}
		return
	}
	s.tryDeleteLocked()
}

// refcountedFile is an *os.File that releases a substitution ref on Close.
type refcountedFile struct {
	*os.File
	st   *winProcSubst
	once sync.Once
}

func (f *refcountedFile) Close() error {
	err := f.File.Close()
	f.once.Do(func() { f.st.consumerClose() })
	return err
}

func (s *winProcSubst) finishProducer() {
	s.prodOnce.Do(func() { close(s.producerDone) })
}

func (s *winProcSubst) finishConsumer() {
	s.consOnce.Do(func() { close(s.consumerDone) })
}

// countedFile signals on Close.
type countedFile struct {
	*os.File
	onClose func()
	once    sync.Once
}

func (f *countedFile) Close() error {
	err := f.File.Close()
	f.once.Do(f.onClose)
	return err
}

// procSubstHandler dispatches to the temp-file implementation on Windows
// and to mvdan/sh's default (FIFO-based) implementation elsewhere.
func procSubstHandler(ctx context.Context, op syntax.ProcOperator) (*interp.ProcSubstFile, error) {
	entryHook := CapabilityHookFromContext(ctx)
	entryScope := procSubstEntryScope(ctx, entryHook)
	if runtime.GOOS != "windows" {
		psf, err := interp.DefaultProcSubstHandler()(ctx, op)
		if err != nil || psf == nil {
			return psf, err
		}
		fifoPath := psf.Path
		unixProcSubstRegister(fifoPath, &unixProcSubst{
			op:           op,
			hook:         entryHook,
			scope:        entryScope,
			openConsumer: psf.OpenConsumer,
		})
		origCleanup := psf.Cleanup
		psf.Cleanup = func() error {
			defer unixProcSubstRemove(fifoPath)
			if origCleanup != nil {
				return origCleanup()
			}
			return nil
		}
		return psf, nil
	}
	f, err := os.CreateTemp("", "unish-procsub-*")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	f.Close() // lifecycle via explicit opens below
	st := &winProcSubst{
		name:         name,
		op:           op,
		hook:         entryHook,
		scope:        entryScope,
		producerDone: make(chan struct{}),
		consumerDone: make(chan struct{}),
	}
	winProcSubstRegister(st)
	openSubshell := func(ctx context.Context) (io.ReadWriteCloser, error) {
		switch op {
		case syntax.CmdIn:
			// Producer writes; signal completion on close (or open failure).
			w, err := openFileRetry(name, os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				st.finishProducer()
				return nil, err
			}
			return &countedFile{File: w, onClose: st.finishProducer}, nil
		default:
			// Producer reads; wait for the consumer to finish first.
			select {
			case <-st.consumerDone:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			r, err := openRetry(name)
			if err != nil {
				return nil, err
			}
			return &countedFile{File: r, onClose: func() {}}, nil
		}
	}
	openConsumer := func(ctx context.Context, flag int) (io.ReadWriteCloser, error) {
		_ = flag
		switch op {
		case syntax.CmdIn:
			select {
			case <-st.producerDone:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			r, err := openRetry(name)
			if err != nil {
				return nil, err
			}
			st.consumerOpen()
			return &refcountedFile{File: r, st: st}, nil
		default:
			w, err := openFileRetry(name, os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				st.finishConsumer()
				return nil, err
			}
			st.consumerOpen()
			return &countedFile{File: w, onClose: st.consumerWriterClose}, nil
		}
	}
	cleanup := func() error {
		st.finishProducer()
		if op == syntax.CmdIn {
			st.mu.Lock()
			st.prodDoneFlag = true
			st.tryDeleteLocked()
			return nil
		}
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			return err
		}
		winProcSubstRemove(name)
		return nil
	}
	return &interp.ProcSubstFile{
		Path:         name,
		OpenSubshell: openSubshell,
		OpenConsumer: openConsumer,
		StmtDone:     st.noteStmtDone,
		Cleanup:      cleanup,
	}, nil
}

// openShellFile opens a command input file, waiting first if it is a
// process-substitution temp file whose producer hasn't finished (the
// shell's OpenConsumer does the same for external commands; builtins
// open paths directly and must wait explicitly). The returned file
// releases the substitution (deleting it when last) on Close; callers
// must Close it (all builtins defer-close their inputs).
func openShellFile(path string) (io.ReadWriteCloser, error) {
	// /dev/stdin (and friends) do not exist on Windows: callers pass
	// hc.Stdin through devStdinReader instead of openShellFile, so this
	// is only reached on unix (real /dev/stdin) or odd spellings —
	// keep the direct open so the true OS error surfaces.
	if st := winProcSubstLookup(path); st != nil && st.op == syntax.CmdIn {
		select {
		case <-st.producerDone:
		case <-time.After(30 * time.Second):
		}
		f, err := openRetry(path)
		if err != nil {
			return nil, err
		}
		st.consumerOpen()
		return &refcountedFileShim{File: f, st: st}, nil
	}
	return openRetry(path)
}

// refcountedFileShim adapts *os.File to io.Closer with ref release.
// (Named distinctly from refcountedFile used by OpenConsumer paths.)
type refcountedFileShim struct {
	*os.File
	st   *winProcSubst
	once sync.Once
}

func (f *refcountedFileShim) Close() error {
	err := f.File.Close()
	f.once.Do(func() { f.st.consumerClose() })
	return err
}

// readShellFile reads a whole command input file via openShellFile so
// substitution refs are released (temp file deleted when last).
func readShellFile(path string) ([]byte, error) {
	f, err := openShellFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// fsOpenShellFile resolves rawPath against dir, enforces CapabilityHook OpRead
// on ordinary files, and allows active `<(cmd)` runtime IPC paths created by
// procSubstHandler even when os.TempDir() is outside the workspace jail.
func fsOpenShellFile(ctx context.Context, dir, cmdName, rawPath string) (io.ReadWriteCloser, error) {
	if st := winProcSubstLookupCtx(ctx, rawPath); st != nil && st.op == syntax.CmdIn {
		select {
		case <-st.producerDone:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
		}
		f, err := openRetry(rawPath)
		if err != nil {
			return nil, err
		}
		st.consumerOpen()
		return &refcountedFileShim{File: f, st: st}, nil
	}
	if st, ok := unixProcSubstLookup(ctx, rawPath); ok && st.op == syntax.CmdIn {
		if st.openConsumer != nil {
			return st.openConsumer(ctx, os.O_RDONLY)
		}
		return os.Open(rawPath)
	}
	p, err := checkFS(ctx, dir, cmdName, rawPath, OpRead)
	if err != nil {
		return nil, err
	}
	return openRetry(p)
}

// fsReadShellFile reads an input file via fsOpenShellFile, enforcing OpRead
// on ordinary files while supporting `<(cmd)` IPC paths inside strict jails.
func fsReadShellFile(ctx context.Context, dir, cmdName, rawPath string) ([]byte, error) {
	f, err := fsOpenShellFile(ctx, dir, cmdName, rawPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

type writeOnlyRWC struct {
	io.WriteCloser
}

func (writeOnlyRWC) Read([]byte) (int, error) {
	return 0, os.ErrInvalid
}

// tryOpenActiveProcSubst opens rawPath as an active process-substitution consumer
// (`<(cmd)` or `>(cmd)`) when rawPath belongs to ctx's CapabilityHook.
// Returns (nil, false, nil) for ordinary non-procsubst paths.
func tryOpenActiveProcSubst(ctx context.Context, rawPath string, flag int) (io.ReadWriteCloser, bool, error) {
	if rawPath == "" {
		return nil, false, nil
	}
	accMode := flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR)
	wantsWrite := accMode == os.O_WRONLY || accMode == os.O_RDWR || flag&(os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
	if !wantsWrite {
		if st := winProcSubstLookupCtx(ctx, rawPath); st != nil && st.op == syntax.CmdIn {
			select {
			case <-st.producerDone:
			case <-ctx.Done():
				return nil, true, ctx.Err()
			case <-time.After(30 * time.Second):
			}
			f, err := openRetry(rawPath)
			if err != nil {
				return nil, true, err
			}
			st.consumerOpen()
			return &refcountedFileShim{File: f, st: st}, true, nil
		}
		if st, ok := unixProcSubstLookup(ctx, rawPath); ok && st.op == syntax.CmdIn {
			if st.openConsumer != nil {
				rwc, err := st.openConsumer(ctx, os.O_RDONLY)
				return rwc, true, err
			}
			f, err := os.Open(rawPath)
			return f, true, err
		}
		return nil, false, nil
	}
	if w, ok, err := openProcSubstOut(ctx, rawPath); ok {
		if err != nil {
			return nil, true, err
		}
		if rwc, ok2 := w.(io.ReadWriteCloser); ok2 {
			return rwc, true, nil
		}
		return writeOnlyRWC{WriteCloser: w}, true, nil
	}
	return nil, false, nil
}

// openProcSubstOut opens a `>(cmd)` process-substitution output sink when
// rawPath is an active CmdOut IPC path synthesized by procSubstHandler for ctx's hook.
func openProcSubstOut(ctx context.Context, rawPath string) (io.WriteCloser, bool, error) {
	if st := winProcSubstLookupCtx(ctx, rawPath); st != nil && st.op == syntax.CmdOut {
		w, err := openFileRetry(rawPath, os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			st.finishConsumer()
			return nil, true, err
		}
		st.consumerOpen()
		return &countedFile{File: w, onClose: st.consumerWriterClose}, true, nil
	}
	if st, ok := unixProcSubstLookup(ctx, rawPath); ok && st.op == syntax.CmdOut {
		if st.openConsumer != nil {
			w, err := st.openConsumer(ctx, os.O_WRONLY)
			return w, true, err
		}
		w, err := os.OpenFile(rawPath, os.O_WRONLY, 0o600)
		return w, true, err
	}
	return nil, false, nil
}

// openRetry opens a file, retrying transient Windows sharing violations
// (AV/indexer briefly locking fresh temp files) for up to ~2s.
func openRetry(path string) (*os.File, error) {
	var f *os.File
	var err error
	for i := 0; i < 40; i++ {
		f, err = os.Open(path)
		if err == nil {
			return f, nil
		}
		if !isLockError(err) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return f, err
}

// openFileRetry is openRetry with flags/perm (for producers).
func openFileRetry(path string, flag int, perm os.FileMode) (*os.File, error) {
	var f *os.File
	var err error
	for i := 0; i < 40; i++ {
		f, err = os.OpenFile(path, flag, perm)
		if err == nil {
			return f, nil
		}
		if !isLockError(err) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return f, err
}

// isLockError reports transient file-locking failures worth retrying.
func isLockError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range []string{
		"being used by another process",
		"access is denied",
		"Access is denied",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
