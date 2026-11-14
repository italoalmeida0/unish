//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

const (
	winProcessQueryLimitedInformation = 0x1000
	winStillActive                    = 259
)

func validWinSigName(sig string) bool {
	switch sig {
	case "0", "HUP", "INT", "QUIT", "ILL", "TRAP", "ABRT", "IOT", "BUS", "FPE",
		"KILL", "USR1", "SEGV", "USR2", "PIPE", "ALRM", "TERM", "CHLD", "CLD",
		"CONT", "STOP", "TSTP", "TTIN", "TTOU", "URG", "XCPU", "XFSZ", "VTALRM",
		"PROF", "WINCH", "IO", "POLL", "SYS":
		return true
	}
	if sig == "" {
		return false
	}
	n := 0
	for _, c := range sig {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n >= 0 && n < 65
}

func killProc(proc *os.Process, sig string) error {
	if proc == nil {
		return os.ErrProcessDone
	}
	if !validWinSigName(sig) {
		return syscall.EINVAL
	}
	if sig == "0" {
		h, err := syscall.OpenProcess(winProcessQueryLimitedInformation, false, uint32(proc.Pid))
		if err != nil {
			return os.ErrProcessDone
		}
		defer syscall.CloseHandle(h)
		var exitCode uint32
		if err := syscall.GetExitCodeProcess(h, &exitCode); err != nil || exitCode != winStillActive {
			return os.ErrProcessDone
		}
		return nil
	}
	return proc.Kill()
}

func signalExitCode(ee *exec.ExitError) int {
	return -1
}
