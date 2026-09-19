//go:build !windows

package main

import (
	"os/user"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func diskUsage(path string) (total, free, avail int64, fstype, mount string) {
	var st syscall.Statfs_t
	p := path
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		p = filepath.Dir(path)
	}
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, 0, 0, "", path
	}
	bsize := int64(st.Bsize)
	return int64(st.Blocks) * bsize, int64(st.Bfree) * bsize, int64(st.Bavail) * bsize, fstypeName(int64(st.Type)), p
}

func fstypeName(t int64) string {
	switch t {
	case 0xEF53:
		return "ext4"
	case 0x01021994:
		return "tmpfs"
	case 0x5346544E:
		return "ntfs"
	case 0x6969:
		return "nfs"
	case 0x9FA0:
		return "proc"
	case 0x62656572:
		return "sysfs"
	}
	return ""
}

func blocksOf(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks
	}
	return (fi.Size() + 511) / 512
}

func deviceOf(path string) string {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return "0h/0d"
	}
	dev := uint64(st.Dev)
	return fmt.Sprintf("%xh/%dd", dev, dev)
}

func inodeOf(path string) int64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0
	}
	return int64(st.Ino)
}

func linksOf(path string, fi os.FileInfo) int {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err == nil {
		return int(st.Nlink)
	}
	if fi.IsDir() {
		return 2
	}
	return 1
}

func uidOf(path string) int {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err == nil {
		return int(st.Uid)
	}
	return os.Getuid()
}

func gidOf(path string) int {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err == nil {
		return int(st.Gid)
	}
	return os.Getgid()
}

func userOf(path string) string {
	return fmt.Sprintf("%d", uidOf(path))
}

func groupOf(path string) string {
	return fmt.Sprintf("%d", gidOf(path))
}

func birthOf(path string, fi os.FileInfo) string {
	return fi.ModTime().Format("2006-01-02 15:04:05.000000000 -0700")
}

type memStat struct {
	total, free, cached, buffers, shared, avail int64
	swapTotal, swapFree                         int64
}

func memInfo() memStat {
	var m memStat
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return m
	}
	vals := map[string]int64{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		mult := int64(1)
		if len(f) > 2 && f[2] == "kB" {
			mult = 1024
		}
		vals[strings.TrimSuffix(f[0], ":")] = v * mult
	}
	m.total = vals["MemTotal"]
	m.free = vals["MemFree"]
	m.cached = vals["Cached"]
	m.buffers = vals["Buffers"]
	m.shared = vals["Shmem"]
	m.avail = vals["MemAvailable"]
	if m.avail == 0 {
		m.avail = m.free + m.cached + m.buffers
	}
	m.swapTotal = vals["SwapTotal"]
	m.swapFree = vals["SwapFree"]
	return m
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

func loadAvg() (float64, float64, float64) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	f := strings.Fields(string(data))
	if len(f) < 3 {
		return 0, 0, 0
	}
	a, _ := strconv.ParseFloat(f[0], 64)
	b, _ := strconv.ParseFloat(f[1], 64)
	c, _ := strconv.ParseFloat(f[2], 64)
	return a, b, c
}

func identity() (uid int, user string, gid int, group string) {
	uid = os.Getuid()
	gid = os.Getgid()
	user = lookupUserName(uid)
	group = lookupGroupName(gid)
	return uid, user, gid, group
}

func ttyName() string { return "" }

func syncDisks() {
	syscall.Sync()
}

func currentNice() int { return 0 }

func setNice(n int) {
	_, _ = syscall.Getpriority(syscall.PRIO_PROCESS, 0)
	syscall.Setpriority(syscall.PRIO_PROCESS, 0, n)
}

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

var tcpStates = map[string]string{
	"01": "ESTAB", "02": "SYN-SENT", "03": "SYN-RECV", "04": "FIN-WAIT-1",
	"05": "FIN-WAIT-2", "06": "TIME-WAIT", "07": "CLOSE", "08": "CLOSE-WAIT",
	"09": "LAST-ACK", "0A": "LISTEN", "0B": "CLOSING",
}

func getSocketInodes() map[string]struct {
	pid  int
	name string
} {
	m := make(map[string]struct {
		pid  int
		name string
	})
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return m
	}
	for _, pe := range procs {
		pid, err := strconv.Atoi(pe.Name())
		if err != nil {
			continue
		}
		comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		pname := strings.TrimSpace(string(comm))
		fds, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		for _, f := range fds {
			link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, f.Name()))
			if err == nil && strings.HasPrefix(link, "socket:[") {
				inode := strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")
				m[inode] = struct {
					pid  int
					name string
				}{pid: pid, name: pname}
			}
		}
	}
	return m
}

func listSockets(protos []string) ([]sockInfo, error) {
	inodeMap := getSocketInodes()
	var out []sockInfo
	for _, proto := range protos {
		for _, f := range []string{"/proc/net/" + proto, "/proc/net/" + proto + "6"} {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			lines := strings.Split(string(data), "\n")
			for _, line := range lines[1:] {
				f := strings.Fields(line)
				if len(f) < 10 {
					continue
				}
				state := tcpStates[f[3]]
				if proto == "udp" {
					state = "UNCONN"
				} else if state == "" {
					state = f[3]
				}
				local := netAddr(f[1])
				peer := netAddr(f[2])
				var txq, rxq int64
				if len(f) > 4 {
					qq := strings.Split(f[4], ":")
					if len(qq) == 2 {
						txq, _ = strconv.ParseInt(qq[0], 16, 64)
						rxq, _ = strconv.ParseInt(qq[1], 16, 64)
					}
				}
				inode := f[9]
				var pid int
				var procName string
				if info, ok := inodeMap[inode]; ok {
					pid = info.pid
					procName = info.name
				}
				out = append(out, sockInfo{
					proto: proto, state: state, local: local, peer: peer,
					recvQ: rxq, sendQ: txq, pid: pid, procName: procName,
				})
			}
		}
	}
	return out, nil
}

func netAddr(hex string) string {
	parts := strings.Split(hex, ":")
	if len(parts) != 2 {
		return hex
	}
	port, _ := strconv.ParseInt(parts[1], 16, 64)
	ipHex := parts[0]
	if len(ipHex) == 8 {
		var b [4]byte
		for i := 0; i < 4; i++ {
			v, _ := strconv.ParseUint(ipHex[i*2:i*2+2], 16, 8)
			b[3-i] = byte(v)
		}
		return fmt.Sprintf("%d.%d.%d.%d:%d", b[0], b[1], b[2], b[3], port)
	}
	if ip, err := parseIPv6Hex(ipHex); err == nil {
		return fmt.Sprintf("[%s]:%d", ip, port)
	}
	return hex
}

func parseIPv6Hex(s string) (string, error) {
	if len(s) != 32 {
		return "", fmt.Errorf("bad")
	}
	var b [16]byte
	for i := 0; i < 16; i++ {
		v, err := strconv.ParseUint(s[i*2:i*2+2], 16, 8)
		if err != nil {
			return "", err
		}
		b[i] = byte(v)
	}
	return net.IP(b[:]).String(), nil
}

var _ = runtime.GOOS
var _ = filepath.Separator

func lookupUserName(uid int) string {
	if u, err := user.LookupId(fmt.Sprintf("%d", uid)); err == nil && u.Username != "" {
		return u.Username
	}
	return fmt.Sprintf("%d", uid)
}

func lookupGroupName(gid int) string {
	if g, err := user.LookupGroupId(fmt.Sprintf("%d", gid)); err == nil && g.Name != "" {
		return g.Name
	}
	return fmt.Sprintf("%d", gid)
}
