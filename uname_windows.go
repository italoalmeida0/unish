//go:build windows

package main

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// windowsBuildNumber returns e.g. "10.0.26100" via RtlGetVersion.
func windowsBuildNumber() (string, error) {
	ntdll := syscall.NewLazyDLL("ntdll.dll")
	rtl := ntdll.NewProc("RtlGetVersion")
	type vs struct {
		size, major, minor, build uint32
		platform                  uint32
		buf                       [128]uint16
		spMajor, spMinor          uint16
		suite                     uint16
		ptype                     byte
		reserved                  byte
	}
	var v vs
	v.size = uint32(unsafe.Sizeof(v))
	r, _, _ := rtl.Call(uintptr(unsafe.Pointer(&v)))
	if r != 0 {
		return "", fmt.Errorf("RtlGetVersion: %d", r)
	}
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.build), nil
}

// kernelRelease reports a kernel release label on Windows. There is no
// uname(2); report the Windows build via a stable portable label that
// includes the OS version when available.
func kernelRelease() string {
	if v, err := windowsBuildNumber(); err == nil && v != "" {
		return v
	}
	return "unish"
}

// kernelVersion reports a kernel version label on Windows.
func kernelVersion() string {
	return fmt.Sprintf("%s windows", runtime.Version())
}

// machineProcessor reports the processor type (uname -p). On Windows GNU
// uname (MSYS) reports the MSYS architecture label; the closest honest
// answer is the real machine (x86_64 / aarch64), not "unknown".
func machineProcessor() string {
	return unameMachine()
}

// machineHardware reports the hardware platform (uname -i).
func machineHardware() string {
	return unameMachine()
}

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
