//go:build !windows

package main

import (
	"os"
	"runtime"
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

// machineProcessor reports the processor type (uname -p). GNU uname on
// Linux reports the machine architecture here (e.g. x86_64), not
// "unknown"; the earlier hardcoded value was simply wrong.
func machineProcessor() string {
	return unameMachine()
}

// machineHardware reports the hardware platform (uname -i). Linux has no
// separate notion, so GNU reports the same machine string as -m/-p.
func machineHardware() string {
	return unameMachine()
}

// unameMachine maps the Go architecture to the kernel machine name, the
// same mapping `uname -m` uses (arm64 -> aarch64, amd64 -> x86_64).
func unameMachine() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i686"
	}
	return runtime.GOARCH
}
