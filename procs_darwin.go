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

func fsBlockSize(st *syscall.Statfs_t) int64 {
	return int64(st.Bsize)
}

func osMemInfo() (memStat, bool) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil || total == 0 {
		return memStat{}, false
	}
	m := memStat{total: int64(total)}
	if pageSize := int64(os.Getpagesize()); pageSize > 0 {
		if freePages, err := unix.SysctlUint32("vm.page_free_count"); err == nil {
			m.free = int64(freePages) * pageSize
			m.avail = m.free
		}
	}
	if raw, err := unix.SysctlRaw("vm.swapusage"); err == nil && len(raw) >= 24 {
		m.swapTotal = int64(binary.LittleEndian.Uint64(raw[0:8]))
		m.swapFree = int64(binary.LittleEndian.Uint64(raw[8:16]))
	}
	return m, true
}

func osLoadAvg() (float64, float64, float64, bool) {
	// struct loadavg on 64-bit Darwin: uint32 ldavg[3], 4-byte pad, int64 fscale.
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 24 {
		return 0, 0, 0, false
	}
	fscale := int64(binary.LittleEndian.Uint64(raw[16:24]))
	if fscale <= 0 {
		return 0, 0, 0, false
	}
	s := float64(fscale)
	l1 := float64(binary.LittleEndian.Uint32(raw[0:4])) / s
	l5 := float64(binary.LittleEndian.Uint32(raw[4:8])) / s
	l15 := float64(binary.LittleEndian.Uint32(raw[8:12])) / s
	return l1, l5, l15, true
}

func listProcs() ([]procInfo, error) {
	kinfos, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	uidCache := make(map[uint32]string, 8)
	out := make([]procInfo, 0, len(kinfos))
	for i := range kinfos {
		kp := &kinfos[i]
		pid := int(kp.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		userName := cstr(kp.Eproc.Login[:])
		if userName == "" {
			uid := kp.Eproc.Ucred.Uid
			u, ok := uidCache[uid]
			if !ok {
				u = lookupUserName(int(uid))
				uidCache[uid] = u
			}
			userName = u
		}
		if userName == "" {
			userName = "?"
		}
		comm := cstr(kp.Proc.P_comm[:])
		cmdLine := comm
		if raw, err := unix.SysctlRaw("kern.procargs2", pid); err == nil {
			execBase, fullCmd := parseDarwinProcArgs2(raw)
			if fullCmd != "" {
				cmdLine = fullCmd
			}
			// P_comm is truncated at MAXCOMLEN (16 chars); restore full name if prefix matches.
			if execBase != "" && (comm == "" || (len(comm) >= 16 && strings.HasPrefix(execBase, comm))) {
				comm = execBase
			}
		}
		if cmdLine == "" {
			continue
		}
		out = append(out, procInfo{
			pid:    pid,
			ppid:   int(kp.Eproc.Ppid),
			user:   userName,
			comm:   comm,
			tty:    "?",
			stat:   darwinStat(kp.Proc.P_stat),
			start:  "?",
			time:   "00:00:00",
			cmd:    cmdLine,
			cpu:    -1,
			memPct: -1,
			vszKB:  -1,
			rssKB:  -1,
		})
	}
	return out, nil
}

func bootTime() time.Time {
	if tv, err := unix.SysctlTimeval("kern.boottime"); err == nil && tv != nil && tv.Sec > 0 {
		return time.Unix(int64(tv.Sec), int64(tv.Usec)*1000)
	}
	return time.Now()
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
