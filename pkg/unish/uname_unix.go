//go:build !windows

package unish

import (
	"os"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// kernelRelease reports the real kernel release (uname -r), e.g.
// "6.18.33.2-microsoft-standard-WSL2" on Linux or "24.3.0" on Darwin.
func kernelRelease() string {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err == nil {
		if s := strings.TrimSpace(unix.ByteSliceToString(uts.Release[:])); s != "" {
			return s
		}
	}
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s
		}
	}
	return "unish"
}

// kernelVersion reports the real kernel version (uname -v), e.g.
// "#1 SMP PREEMPT_DYNAMIC ..." on Linux or "Darwin Kernel Version ..." on macOS.
func kernelVersion() string {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err == nil {
		if s := strings.TrimSpace(unix.ByteSliceToString(uts.Version[:])); s != "" {
			return s
		}
	}
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
