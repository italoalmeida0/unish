//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func killProc(proc *os.Process, sig string) error {
	s, ok := map[string]os.Signal{
		"HUP":  syscall.SIGHUP,
		"INT":  syscall.SIGINT,
		"QUIT": syscall.SIGQUIT,
		"KILL": syscall.SIGKILL,
		"TERM": syscall.SIGTERM,
		"USR1": syscall.SIGUSR1,
		"USR2": syscall.SIGUSR2,
	}[sig]
	if !ok {
		n := 0
		for _, c := range sig {
			if c < '0' || c > '9' {
				return syscall.EINVAL
			}
			n = n*10 + int(c-'0')
		}
		s = syscall.Signal(n)
	}
	return proc.Signal(s)
}

func signalExitCode(ee *exec.ExitError) int {
	if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
		return 128 + int(st.Signal())
	}
	return -1
}
