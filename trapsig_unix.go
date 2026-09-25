//go:build !windows

package main

import (
	"os"
	"syscall"
)

// trapSignals are the signals a script can trap. Without Notify the default
// action kills the process before any trap runs.
func trapSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}
}
