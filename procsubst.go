package main

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

func winProcSubstLookup(name string) *winProcSubst {
	winProcSubstRegistry.mu.Lock()
	defer winProcSubstRegistry.mu.Unlock()
	return winProcSubstRegistry.m[name]
}

func winProcSubstRemove(name string) {
	winProcSubstRegistry.mu.Lock()
	delete(winProcSubstRegistry.m, name)
	winProcSubstRegistry.mu.Unlock()
}

// waitProcSubstProducer blocks until the producer of a process-
// substitution temp file finishes (no-op for ordinary paths).
func waitProcSubstProducer(path string) {
	if !strings.Contains(path, "unish-procsub-") {
		return
	}
	if st := winProcSubstLookup(path); st != nil {
		<-st.producerDone
	}
}

// winProcSubst carries the shared state for one substitution.
type winProcSubst struct {
	name         string
	op           syntax.ProcOperator
	producerDone chan struct{}
	consumerDone chan struct{}
	prodOnce     sync.Once
	consOnce     sync.Once
	mu           sync.Mutex
	openCount    int
	deleted      bool
}

// consumerOpen records a new consumer fd.
func (s *winProcSubst) consumerOpen() {
	s.mu.Lock()
	s.openCount++
	s.mu.Unlock()
}

// consumerClose records a closed consumer fd and deletes the temp file
// once the producer is done and no consumer fds remain.
func (s *winProcSubst) consumerClose() {
	s.mu.Lock()
	s.openCount--
	done := s.openCount <= 0
	if done {
		s.openCount = 0
	}
	name, del := s.name, done && !s.deleted
	if del {
		s.deleted = true
	}
	s.mu.Unlock()
	select {
	case <-s.producerDone:
		if del {
			os.Remove(name)
			winProcSubstRemove(name)
		}
	default:
	}
}

// refcountedFile is an *os.File that releases a substitution ref on Close.
type refcountedFile struct {
	*os.File
	st *winProcSubst
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
	if runtime.GOOS != "windows" {
		return interp.DefaultProcSubstHandler()(ctx, op)
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
		producerDone: make(chan struct{}),
		consumerDone: make(chan struct{}),
	}
	winProcSubstRegister(st)
	openSubshell := func(ctx context.Context) (io.ReadWriteCloser, error) {
		_ = ctx
		switch op {
		case syntax.CmdIn:
			// Producer writes; signal completion on close.
			w, err := openFileRetry(name, os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return nil, err
			}
			return &countedFile{File: w, onClose: st.finishProducer}, nil
		default:
			// Producer reads; wait for the consumer to finish first.
			<-st.consumerDone
			r, err := os.Open(name)
			if err != nil {
				return nil, err
			}
			return &countedFile{File: r, onClose: func() {}}, nil
		}
	}
	openConsumer := func(ctx context.Context, flag int) (io.ReadWriteCloser, error) {
		_ = ctx
		_ = flag
		switch op {
		case syntax.CmdIn:
			// Consumer reads; wait for the producer to finish first.
			// The file is deleted when the consumer closes it (the
			// runner's Cleanup may run before the consumer is done,
			// so Cleanup must NOT delete for this direction).
			<-st.producerDone
			r, err := os.Open(name)
			if err != nil {
				return nil, err
			}
			return &countedFile{File: r, onClose: func() { os.Remove(name) }}, nil
		default:
			// Consumer writes; signal completion on close so the
			// producer (blocked above) can proceed. Deletion happens
			// in Cleanup (after the producer is done).
			w, err := openFileRetry(name, os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return nil, err
			}
			return &countedFile{File: w, onClose: st.finishConsumer}, nil
		}
	}
	cleanup := func() error {
		// Must not delete or deregister here: the consumer typically
		// opens the path AFTER the producer finishes (which is when
		// this runs). Deletion happens on consumer close (CmdIn) or
		// here for CmdOut (producer already consumed the input).
		if op != syntax.CmdIn {
			if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
				return err
			}
			winProcSubstRemove(name)
		}
		return nil
	}
	return &interp.ProcSubstFile{
		Path:         name,
		OpenSubshell: openSubshell,
		OpenConsumer: openConsumer,
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
	if st := winProcSubstLookup(path); st != nil {
		<-st.producerDone
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
