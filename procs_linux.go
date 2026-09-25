//go:build linux

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func listProcs() ([]procInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []procInfo
	clkTck := int64(100)
	var uptime float64
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		fmt.Sscanf(string(data), "%f", &uptime)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p := procInfo{pid: pid, user: "?", tty: "?", stat: "?", start: "?", time: "00:00:00", cpu: -1, memPct: -1, vszKB: -1, rssKB: -1}
		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			s := string(data)
			ri := strings.LastIndexByte(s, ')')
			if ri > 0 {
				rest := strings.Fields(s[ri+1:])
				if len(rest) > 20 {
					p.ppid, _ = strconv.Atoi(rest[1])
					p.stat = rest[0]
					utime, _ := strconv.ParseInt(rest[11], 10, 64)
					stime, _ := strconv.ParseInt(rest[12], 10, 64)
					startTicks, _ := strconv.ParseInt(rest[19], 10, 64)
					vsz, _ := strconv.ParseInt(rest[20], 10, 64)
					rss, _ := strconv.ParseInt(rest[21], 10, 64)
					p.vszKB = vsz / 1024
					p.rssKB = rss * 4096 / 1024
					totalTicks := utime + stime
					elapsed := uptime - float64(startTicks)/float64(clkTck)
					if elapsed > 0 {
						p.cpu = float64(totalTicks) / float64(clkTck) / elapsed * 100
					}
					mins := (utime + stime) / clkTck / 60
					secs := (utime + stime) / clkTck % 60
					p.time = fmt.Sprintf("%02d:%02d:%02d", mins/60, mins%60, secs)
				}
			}
		}
		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil && len(data) > 0 {
			parts := strings.Split(string(data), "\x00")
			var cmd []string
			for _, x := range parts {
				if x != "" {
					cmd = append(cmd, x)
				}
			}
			p.cmd = strings.Join(cmd, " ")
		} else if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
			p.cmd = "[" + strings.TrimSpace(string(data)) + "]"
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
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "btime ") {
			if v, err := strconv.ParseInt(strings.Fields(line)[1], 10, 64); err == nil {
				return time.Unix(v, 0)
			}
		}
	}
	return time.Now()
}
