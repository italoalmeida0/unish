//go:build windows

package main

// Windows UTF-8 fix: the classic agent papercut is
// "UnicodeEncodeError: 'charmap' codec can't encode character ..."
// when a child (python, node, ...) prints unicode under a cp1252/cp850
// console. Two layers:
//
//  1. Our own console: SetConsoleOutputCP(CP_UTF8) so unish itself
//     renders/accepts UTF-8 (echo 😊, cat utf8 files).
//  2. Our own env defaults: PYTHONIOENCODING=utf-8 + PYTHONUTF8=1 when
//     the user didn't set them, so children that inherit our env (and
//     grandchildren) default to UTF-8 stdio. Explicit user exports in
//     runExternalTracked/trackCmd/startTimeoutProc (shellExecEnv) always
//     win over these defaults.

import (
	"os"
	"syscall"
)

var (
	modKernel32UTF8   = syscall.NewLazyDLL("kernel32.dll")
	procSetOutputCP   = modKernel32UTF8.NewProc("SetConsoleOutputCP")
	procSetConsoleCP  = modKernel32UTF8.NewProc("SetConsoleCP")
	procGetConsoleCP  = modKernel32UTF8.NewProc("GetConsoleOutputCP")
)

const cpUTF8 = 65001

func init() {
	// Best effort: never fail startup because of this.
	_, _, _ = procSetOutputCP.Call(uintptr(cpUTF8))
	_, _, _ = procSetConsoleCP.Call(uintptr(cpUTF8))
	if os.Getenv("PYTHONIOENCODING") == "" {
		_ = os.Setenv("PYTHONIOENCODING", "utf-8")
	}
	if os.Getenv("PYTHONUTF8") == "" {
		_ = os.Setenv("PYTHONUTF8", "1")
	}
}
