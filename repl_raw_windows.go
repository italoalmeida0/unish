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

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

func termIsTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

func termRawOn() (func(), bool) {
	inH := windows.Handle(os.Stdin.Fd())
	var inMode uint32
	if err := windows.GetConsoleMode(inH, &inMode); err != nil {
		return nil, false
	}
	var outH = windows.Handle(os.Stdout.Fd())
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
	newIn := inMode
	newIn &^= ENABLE_LINE_INPUT | ENABLE_ECHO_INPUT | ENABLE_PROCESSED_INPUT | ENABLE_MOUSE_INPUT
	newIn |= ENABLE_WINDOW_INPUT | ENABLE_VIRTUAL_TERMINAL_INPUT
	if err := windows.SetConsoleMode(inH, newIn); err != nil {
		return nil, false
	}
	var prevOut uint32
	if haveOut {
		prevOut = outMode
		newOut := outMode | ENABLE_PROCESSED_OUTPUT | ENABLE_VIRTUAL_TERMINAL_PROCESSING
		_ = windows.SetConsoleMode(outH, newOut)
	}
	oldTerm, _ := term.MakeRaw(int(os.Stdin.Fd()))
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if oldTerm != nil {
			_ = term.Restore(int(os.Stdin.Fd()), oldTerm)
		}
		_ = windows.SetConsoleMode(inH, inMode)
		if haveOut {
			_ = windows.SetConsoleMode(outH, prevOut)
		}
	}
	return restore, true
}

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}
