//go:build windows

package main

// Windows console raw mode. x/term.MakeRaw works on Windows consoles
// only when ENABLE_VIRTUAL_TERMINAL_INPUT is set; otherwise the
// console delivers cooked line input and raw mode fails — in that
// case we fall back to the plain (non-raw) loop.
//
// Strategy (same as shell.js ENABLE_VIRTUAL_TERMINAL_PROCESSING +
// raw stdin handling):
//  1. Enable virtual-terminal INPUT + OUTPUT processing so ANSI
//     sequences decode and render.
//  2. Clear ENABLE_LINE_INPUT | ENABLE_ECHO_INPUT | ENABLE_PROCESSED_INPUT
//     so every keystroke (arrows as ESC [ sequences, Ctrl keys, Alt)
//     arrives immediately.
//  3. x/term.MakeRaw as belt-and-braces (no-op if modes already raw).
// On any failure: report false and the caller uses plainLoop().

import (
	"os"
	"sync"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

func termIsTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

type rawSession struct {
	mu      sync.Mutex
	inH     windows.Handle
	inMode  uint32
	rawIn   uint32
	outH    windows.Handle
	haveOut bool
	outMode uint32
	rawOut  uint32
	oldTerm *term.State
	stdinFd int
	conFile *os.File
	active  bool
	closed  bool
}

func startRawSession(in *os.File) (*rawSession, bool) {
	if in == nil {
		in = os.Stdin
	}
	stdinFd := int(in.Fd())
	inH := windows.Handle(in.Fd())
	var inMode uint32
	own := false
	if err := windows.GetConsoleMode(inH, &inMode); err != nil {
		// Piped stdin but a real console exists (e.g. child with
		// redirected pipes): open CONIN$ directly so the REPL still
		// gets keystrokes instead of falling back to plainLoop.
		conH, err := openConIn()
		if err != nil {
			return nil, false
		}
		if err := windows.GetConsoleMode(conH, &inMode); err != nil {
			_ = windows.CloseHandle(conH)
			return nil, false
		}
		inH = conH
		own = true
	}
	return newWinRawSession(inH, inMode, stdinFd, own)
}

func termRawOn() (func(), bool) {
	s, ok := startRawSession(os.Stdin)
	if !ok {
		return nil, false
	}
	return s.close, true
}

func openConIn() (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString("CONIN$")
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
}

func newWinRawSession(inH windows.Handle, inMode uint32, stdinFd int, own bool) (*rawSession, bool) {
	outH := windows.Handle(os.Stdout.Fd())
	var outMode uint32
	haveOut := windows.GetConsoleMode(outH, &outMode) == nil

	const (
		ENABLE_PROCESSED_INPUT             = 0x0001
		ENABLE_LINE_INPUT                  = 0x0002
		ENABLE_ECHO_INPUT                  = 0x0004
		ENABLE_WINDOW_INPUT                = 0x0008
		ENABLE_MOUSE_INPUT                 = 0x0010
		ENABLE_VIRTUAL_TERMINAL_INPUT      = 0x0200
		ENABLE_PROCESSED_OUTPUT            = 0x0001
		ENABLE_VIRTUAL_TERMINAL_PROCESSING = 0x0004
	)
	rawIn := inMode
	rawIn &^= ENABLE_LINE_INPUT | ENABLE_ECHO_INPUT | ENABLE_PROCESSED_INPUT | ENABLE_MOUSE_INPUT
	rawIn |= ENABLE_WINDOW_INPUT | ENABLE_VIRTUAL_TERMINAL_INPUT
	if err := windows.SetConsoleMode(inH, rawIn); err != nil {
		if own {
			_ = windows.CloseHandle(inH)
		}
		return nil, false
	}
	rawOut := outMode
	if haveOut {
		rawOut = outMode | ENABLE_PROCESSED_OUTPUT | ENABLE_VIRTUAL_TERMINAL_PROCESSING
		_ = windows.SetConsoleMode(outH, rawOut)
	}
	var oldTerm *term.State
	if !own {
		oldTerm, _ = term.MakeRaw(stdinFd)
	}
	s := &rawSession{
		inH:     inH,
		inMode:  inMode,
		rawIn:   rawIn,
		outH:    outH,
		haveOut: haveOut,
		outMode: outMode,
		rawOut:  rawOut,
		oldTerm: oldTerm,
		stdinFd: stdinFd,
		active:  true,
	}
	if own {
		conFile := os.NewFile(uintptr(inH), "CONIN$")
		if conFile == nil {
			_ = windows.SetConsoleMode(inH, inMode)
			if haveOut {
				_ = windows.SetConsoleMode(outH, outMode)
			}
			_ = windows.CloseHandle(inH)
			return nil, false
		}
		s.conFile = conFile
		conInFile = conFile
	}
	return s, true
}

func (s *rawSession) suspend() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || s.closed {
		return
	}
	if s.oldTerm != nil {
		_ = term.Restore(s.stdinFd, s.oldTerm)
		s.oldTerm = nil
	}
	_ = windows.SetConsoleMode(s.inH, s.inMode)
	if s.haveOut {
		_ = windows.SetConsoleMode(s.outH, s.outMode)
	}
	s.active = false
}

func (s *rawSession) resume() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active || s.closed {
		return
	}
	if err := windows.SetConsoleMode(s.inH, s.rawIn); err != nil {
		return
	}
	if s.haveOut {
		_ = windows.SetConsoleMode(s.outH, s.rawOut)
	}
	if s.conFile == nil {
		s.oldTerm, _ = term.MakeRaw(s.stdinFd)
	}
	s.active = true
}

func (s *rawSession) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.active {
		if s.oldTerm != nil {
			_ = term.Restore(s.stdinFd, s.oldTerm)
			s.oldTerm = nil
		}
		_ = windows.SetConsoleMode(s.inH, s.inMode)
		if s.haveOut {
			_ = windows.SetConsoleMode(s.outH, s.outMode)
		}
		s.active = false
	}
	s.closed = true
	if s.conFile != nil {
		if conInFile == s.conFile {
			conInFile = nil
		}
		_ = s.conFile.Close()
		s.conFile = nil
	}
}

func (s *rawSession) inFile() *os.File {
	if s == nil {
		return nil
	}
	return s.conFile
}

// rawOnHandle puts an input console handle in raw VT mode. When own is
// true the handle came from openConIn: reads use it too (rp.inFile is
// swapped by the caller) and it is closed on restore.
func rawOnHandle(inH windows.Handle, inMode uint32, own bool) (func(), bool) {
	s, ok := newWinRawSession(inH, inMode, int(os.Stdin.Fd()), own)
	if !ok {
		return nil, false
	}
	return s.close, true
}

// conInFile is set by rawOnHandle when stdin was piped but CONIN$
// exists; runInteractive swaps rp.inFile to it.
var conInFile *os.File

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}
