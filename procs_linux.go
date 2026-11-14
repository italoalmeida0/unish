//go:build linux

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func fsBlockSize(st *syscall.Statfs_t) int64 {
	if st.Frsize > 0 {
		return int64(st.Frsize)
	}
	return int64(st.Bsize)
}

func osMemInfo() (memStat, bool) {
	return memStat{}, false
}

func osLoadAvg() (float64, float64, float64, bool) {
	return 0, 0, 0, false
}

func listProcs() ([]procInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []procInfo
	var uptime float64
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		fmt.Sscanf(string(data), "%f", &uptime)
	}
	pageSize := os.Getpagesize()
	uidCache := make(map[uint32]string, 8)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		procDir := fmt.Sprintf("/proc/%d", pid)
		p := procInfo{
			pid: pid, user: "?", tty: "?", stat: "?",
			start: "?", time: "00:00:00",
			cpu: -1, memPct: -1, vszKB: -1, rssKB: -1,
		}
		var st syscall.Stat_t
		if err := syscall.Stat(procDir, &st); err == nil {
			u, ok := uidCache[st.Uid]
			if !ok {
				u = lookupUserName(int(st.Uid))
				uidCache[st.Uid] = u
			}
			if u != "" {
				p.user = u
			}
		}
		if data, err := os.ReadFile(procDir + "/stat"); err == nil {
			if comm, statCh, ppid, pgid, cpu, vszKB, rssKB, cpuTime, ok := parseProcStat(string(data), uptime, pageSize); ok {
				p.comm = comm
				p.stat = statCh
				p.ppid = ppid
				p.pgid = pgid
				p.cpu = cpu
				p.vszKB = vszKB
				p.rssKB = rssKB
				p.time = cpuTime
			}
		}
		if data, err := os.ReadFile(procDir + "/cmdline"); err == nil && len(data) > 0 {
			parts := strings.Split(string(data), "\x00")
			var cmd []string
			for _, x := range parts {
				if x != "" {
					cmd = append(cmd, x)
				}
			}
			if len(cmd) > 0 {
				p.cmd = strings.Join(cmd, " ")
				base0 := baseNameAnySlash(cmd[0])
				if p.comm == "" || (len(p.comm) == 15 && strings.HasPrefix(base0, p.comm)) {
					p.comm = base0
				}
			}
		}
		if p.comm == "" {
			if data, err := os.ReadFile(procDir + "/comm"); err == nil {
				p.comm = strings.TrimSpace(string(data))
			}
		}
		if p.cmd == "" && p.comm != "" {
			p.cmd = "[" + p.comm + "]"
		}
		if p.cmd == "" {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func bootTime() time.Time {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Now()
	}
	if v, ok := parseProcBtime(string(data)); ok {
		return time.Unix(v, 0)
	}
	return time.Now()
}
