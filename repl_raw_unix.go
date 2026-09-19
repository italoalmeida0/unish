//go:build !windows

package main

// TTY raw mode + is-terminal helpers (unix): termios via
// golang.org/x/term (already an indirect dep).

import (
	"os"

	"golang.org/x/term"
)

func termIsTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

func termRawOn() (func(), bool) {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, false
	}
	return func() { _ = term.Restore(fd, old) }, true
}

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}
