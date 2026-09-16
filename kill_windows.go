//go:build windows

package main

import (
	"os"
	"os/exec"
)

func killProc(proc *os.Process, sig string) error {
	return proc.Kill()
}

func signalExitCode(ee *exec.ExitError) int {
	return -1
}
