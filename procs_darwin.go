//go:build darwin

package main

import (
	"encoding/binary"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// listProcs on macOS/BSD: there is no /proc, but the kernel exposes the
// process table through sysctl (kern.proc.all) — the same source ps(1)
// itself reads. This makes pgrep/pkill/ps work on macOS instead of
// reporting "unsupported".
//
// Parsing goes through x/sys/unix's typed KinfoProc (SysctlKinfoProcSlice):
// the struct layout is maintained by the Go team, so it cannot drift the
// way a hand-rolled offset table can. (The previous hand-rolled table read
// e_ppid at offset 296 — the START of eproc — so every row reported ppid 0;
// ps(1)'s default columns hide ppid and pgrep/pkill never needed it, so the
// bug went unnoticed.)
func listProcs() ([]procInfo, error) {
	kinfos, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]procInfo, 0, len(kinfos))
	for i := range kinfos {
		kp := &kinfos[i]
		pid := int(kp.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		out = append(out, procInfo{
			pid:    pid,
			ppid:   int(kp.Eproc.Ppid),
			user:   cstr(kp.Eproc.Login[:]),
			tty:    "?",
			stat:   darwinStat(kp.Proc.P_stat),
			start:  "?",
			time:   "00:00:00",
			cmd:    cstr(kp.Proc.P_comm[:]),
			cpu:    -1,
			memPct: -1,
			vszKB:  -1,
			rssKB:  -1,
		})
	}
	return out, nil
}

// bootTime on Darwin: sysctl kern.boottime is a struct timeval; the first
// 8 bytes are the seconds since the epoch.
func bootTime() time.Time {
	raw, err := syscall.Sysctl("kern.boottime")
	if err != nil || len(raw) < 8 {
		return time.Now()
	}
	secs := int64(binary.LittleEndian.Uint64([]byte(raw)[:8]))
	if secs <= 0 {
		return time.Now()
	}
	return time.Unix(secs, 0)
}

// cstr reads a NUL-terminated string from a fixed-size field.
func cstr(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// darwinStat maps the BSD process state byte to the ps letter.
func darwinStat(s int8) string {
	switch s {
	case 1:
		return "I" // idle
	case 2:
		return "R" // running
	case 3:
		return "S" // sleeping
	case 4:
		return "T" // stopped
	case 5:
		return "Z" // zombie
	}
	return "?"
}

var _ = os.Getpid
