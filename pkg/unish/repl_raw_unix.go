//go:build !windows

package unish

// TTY raw mode + is-terminal helpers (unix): termios via
// golang.org/x/term (already an indirect dep).

import (
	"os"
	"sync"

	"golang.org/x/term"
)

func termIsTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

type rawSession struct {
	mu     sync.Mutex
	fd     int
	cooked *term.State
	active bool
}

func startRawSession(in *os.File) (*rawSession, bool) {
	if in == nil {
		in = os.Stdin
	}
	fd := int(in.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, false
	}
	return &rawSession{
		fd:     fd,
		cooked: old,
		active: true,
	}, true
}

func (s *rawSession) suspend() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	_ = term.Restore(s.fd, s.cooked)
	s.active = false
}

func (s *rawSession) resume() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return
	}
	if _, err := term.MakeRaw(s.fd); err == nil {
		s.active = true
	}
}

func (s *rawSession) close() {
	s.suspend()
}

func (s *rawSession) inFile() *os.File {
	return nil
}

func termRawOn() (func(), bool) {
	s, ok := startRawSession(os.Stdin)
	if !ok {
		return nil, false
	}
	return s.close, true
}

// conInFile is a Windows-only stash (CONIN$ when stdin is piped);
// nil everywhere else so repl_tty.go compiles on unix.
var conInFile *os.File

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}
