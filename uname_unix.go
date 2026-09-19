//go:build !windows

package main

import (
	"os"
	"strings"
)

// kernelRelease reports the real kernel release (uname -r), e.g.
// "6.18.33.2-microsoft-standard-WSL2" on WSL2.
func kernelRelease() string {
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s
		}
	}
	return "unish"
}

// kernelVersion reports the real kernel version (uname -v), e.g.
// "#1 SMP PREEMPT_DYNAMIC ...".
func kernelVersion() string {
	if data, err := os.ReadFile("/proc/sys/kernel/version"); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s
		}
	}
	return "1.0"
}
