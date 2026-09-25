//go:build darwin

package main

import (
	"encoding/binary"
	"os"
	"strings"
	"syscall"
	"time"
)

// listProcs on macOS/BSD: there is no /proc, but the kernel exposes the
// process table through sysctl (kern.proc.all) — the same source ps(1)
// itself reads. This makes pgrep/pkill/ps work on macOS instead of
// reporting "unsupported".
//
// kinfo_proc is a fixed 648-byte struct on 64-bit Darwin: an extern_proc
// (struct proc) followed by an eproc. Only a few offsets are needed and
// they are stable ABI (defined in <sys/sysctl.h> / <sys/proc.h>).
const kinfoProcSize = 648

// Offsets within one kinfo_proc record (64-bit Darwin).
const (
	offPFlag  = 32  // extern_proc.p_flag (int32)
	offPStat  = 36  // extern_proc.p_stat (char)
	offPPid   = 40  // extern_proc.p_pid (pid_t)
	offComm   = 243 // extern_proc.p_comm (char[17])
	offEproc  = 296 // start of eproc
	offEPpid  = 296 // eproc.e_ppid (pid_t)
	offELogin = 344 // eproc.e_login (char[12])
)

func listProcs() ([]procInfo, error) {
	raw, err := syscall.Sysctl("kern.proc.all")
	if err != nil {
		return nil, err
	}
	buf := []byte(raw)
	var out []procInfo
	for off := 0; off+kinfoProcSize <= len(buf); off += kinfoProcSize {
		kp := buf[off : off+kinfoProcSize]
		pid := int(int32(binary.LittleEndian.Uint32(kp[offPPid : offPPid+4])))
		if pid <= 0 {
			continue
		}
		ppid := int(int32(binary.LittleEndian.Uint32(kp[offEPpid : offEPpid+4])))
		stat := kp[offPStat]
		comm := cstr(kp[offComm : offComm+17])
		out = append(out, procInfo{
			pid:    pid,
			ppid:   ppid,
			user:   cstr(kp[offELogin : offELogin+12]),
			tty:    "?",
			stat:   darwinStat(stat),
			start:  "?",
			time:   "00:00:00",
			cmd:    comm,
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
func darwinStat(s byte) string {
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
