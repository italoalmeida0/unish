package main

import (
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/interp"
)

const (
	memPipeBufSize = 64 * 1024
	memPipeMask    = memPipeBufSize - 1
)

var memPipeBufPool = sync.Pool{
	New: func() any {
		return new([memPipeBufSize]byte)
	},
}

func init() {
	interp.SetPipeFactory(func() (interp.StdinPipe, io.WriteCloser, error) {
		pr, pw := newMemPipe()
		return pr, pw, nil
	})
}

// memPipe is a race-free 64 KB ring-buffered in-memory pipe backed by sync.Pool.
// Readers drain all buffered bytes before observing io.EOF after the writer
// closes; writers observe io.ErrClosedPipe immediately when the reader closes
// early (SIGPIPE / `yes | head -n 1` semantics).
type memPipe struct {
	mu              sync.Mutex
	rCond           sync.Cond
	wCond           sync.Cond
	buf             *[memPipeBufSize]byte
	rpos            int
	count           int
	rClosed         bool
	wClosed         bool
	deadlineExpired bool
	deadlineSeq     uint64
	deadlineTimer   *time.Timer
}

type memPipeReader struct {
	p *memPipe
}

type memPipeWriter struct {
	p *memPipe
}

// newMemPipe returns a paired 64 KB buffered in-memory pipe reader and writer.
func newMemPipe() (*memPipeReader, *memPipeWriter) {
	p := &memPipe{
		buf: memPipeBufPool.Get().(*[memPipeBufSize]byte),
	}
	p.rCond.L = &p.mu
	p.wCond.L = &p.mu
	return &memPipeReader{p: p}, &memPipeWriter{p: p}
}

func (p *memPipe) maybeRecycleLocked() {
	if p.rClosed && p.wClosed && p.buf != nil {
		if p.deadlineTimer != nil {
			p.deadlineTimer.Stop()
			p.deadlineTimer = nil
		}
		memPipeBufPool.Put(p.buf)
		p.buf = nil
	}
}

func (r *memPipeReader) Read(b []byte) (int, error) {
	p := r.p
	p.mu.Lock()
	for {
		if p.rClosed {
			p.mu.Unlock()
			return 0, io.ErrClosedPipe
		}
		if p.deadlineExpired {
			p.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		if p.count > 0 {
			if len(b) == 0 {
				p.mu.Unlock()
				return 0, nil
			}
			n := min(len(b), p.count)
			first := min(n, memPipeBufSize-p.rpos)
			copy(b[:first], p.buf[p.rpos:p.rpos+first])
			if first < n {
				copy(b[first:n], p.buf[:n-first])
			}
			p.count -= n
			if p.count == 0 {
				p.rpos = 0
			} else {
				p.rpos = (p.rpos + n) & memPipeMask
			}
			p.wCond.Signal()
			p.mu.Unlock()
			return n, nil
		}
		if p.wClosed {
			p.mu.Unlock()
			return 0, io.EOF
		}
		if len(b) == 0 {
			p.mu.Unlock()
			return 0, nil
		}
		p.rCond.Wait()
	}
}

func (r *memPipeReader) Close() error {
	p := r.p
	p.mu.Lock()
	if !p.rClosed {
		p.rClosed = true
		p.count = 0
		p.rpos = 0
		p.rCond.Broadcast()
		p.wCond.Broadcast()
		p.maybeRecycleLocked()
	}
	p.mu.Unlock()
	return nil
}

func (r *memPipeReader) SetReadDeadline(t time.Time) error {
	p := r.p
	p.mu.Lock()
	p.deadlineSeq++
	seq := p.deadlineSeq
	if p.deadlineTimer != nil {
		p.deadlineTimer.Stop()
		p.deadlineTimer = nil
	}
	if t.IsZero() {
		p.deadlineExpired = false
		p.mu.Unlock()
		return nil
	}
	d := time.Until(t)
	if d <= 0 {
		p.deadlineExpired = true
		p.rCond.Broadcast()
		p.mu.Unlock()
		return nil
	}
	p.deadlineExpired = false
	p.deadlineTimer = time.AfterFunc(d, func() {
		p.mu.Lock()
		if p.deadlineSeq == seq {
			p.deadlineExpired = true
			p.rCond.Broadcast()
		}
		p.mu.Unlock()
	})
	p.mu.Unlock()
	return nil
}

func (w *memPipeWriter) Write(b []byte) (int, error) {
	p := w.p
	p.mu.Lock()
	if p.rClosed || p.wClosed {
		p.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if len(b) == 0 {
		p.mu.Unlock()
		return 0, nil
	}
	written := 0
	for len(b) > 0 {
		for p.count == memPipeBufSize && !p.rClosed && !p.wClosed {
			p.wCond.Wait()
		}
		if p.rClosed || p.wClosed {
			p.mu.Unlock()
			return written, io.ErrClosedPipe
		}
		avail := memPipeBufSize - p.count
		n := min(len(b), avail)
		wpos := (p.rpos + p.count) & memPipeMask
		first := min(n, memPipeBufSize-wpos)
		copy(p.buf[wpos:wpos+first], b[:first])
		if first < n {
			copy(p.buf[:n-first], b[first:n])
		}
		p.count += n
		written += n
		b = b[n:]
		p.rCond.Signal()
	}
	p.mu.Unlock()
	return written, nil
}

func (w *memPipeWriter) WriteString(s string) (int, error) {
	p := w.p
	p.mu.Lock()
	if p.rClosed || p.wClosed {
		p.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if len(s) == 0 {
		p.mu.Unlock()
		return 0, nil
	}
	written := 0
	for len(s) > 0 {
		for p.count == memPipeBufSize && !p.rClosed && !p.wClosed {
			p.wCond.Wait()
		}
		if p.rClosed || p.wClosed {
			p.mu.Unlock()
			return written, io.ErrClosedPipe
		}
		avail := memPipeBufSize - p.count
		n := min(len(s), avail)
		wpos := (p.rpos + p.count) & memPipeMask
		first := min(n, memPipeBufSize-wpos)
		copy(p.buf[wpos:wpos+first], s[:first])
		if first < n {
			copy(p.buf[:n-first], s[first:n])
		}
		p.count += n
		written += n
		s = s[n:]
		p.rCond.Signal()
	}
	p.mu.Unlock()
	return written, nil
}

func (w *memPipeWriter) Close() error {
	p := w.p
	p.mu.Lock()
	if !p.wClosed {
		p.wClosed = true
		p.rCond.Broadcast()
		p.wCond.Broadcast()
		p.maybeRecycleLocked()
	}
	p.mu.Unlock()
	return nil
}

// devNullFile is a stateless, zero-syscall, zero-allocation write-only handle
// for redirects to /dev/null (or NUL on Windows).
type devNullFile struct{}

var sharedDevNull io.ReadWriteCloser = &devNullFile{}

func (*devNullFile) Read([]byte) (int, error)          { return 0, syscall.EBADF }
func (*devNullFile) Write(p []byte) (int, error)       { return len(p), nil }
func (*devNullFile) WriteString(s string) (int, error) { return len(s), nil }
func (*devNullFile) Close() error                      { return nil }
func (*devNullFile) IsVirtualDevNull() bool            { return true }

// isWriteOnlyDevNull reports whether (path, flag) opens the null device in
// write-only mode (e.g. `> /dev/null`, `>> /dev/null`, `2> /dev/null`).
func isWriteOnlyDevNull(path string, flag int) bool {
	if flag&(os.O_RDONLY|os.O_WRONLY|os.O_RDWR) != os.O_WRONLY {
		return false
	}
	if path == "/dev/null" || path == os.DevNull {
		return true
	}
	if runtime.GOOS == "windows" && len(path) == 3 && strings.EqualFold(path, "NUL") {
		return true
	}
	return false
}
