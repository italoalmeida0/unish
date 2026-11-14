//go:build !windows

package unish

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	bsize := fsBlockSize(&st)
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
	if err := syscall.Lstat(path, &st); err != nil {
		return "0h/0d"
	}
	dev := uint64(st.Dev)
	return fmt.Sprintf("%xh/%dd", dev, dev)
}

func inodeOf(path string) int64 {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return 0
	}
	return int64(st.Ino)
}

func linksOf(path string, fi os.FileInfo) int {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err == nil {
		return int(st.Nlink)
	}
	if fi.IsDir() {
		return 2
	}
	return 1
}

func uidOf(path string) int {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err == nil {
		return int(st.Uid)
	}
	return os.Getuid()
}

func gidOf(path string) int {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err == nil {
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
	if m, ok := osMemInfo(); ok {
		return m
	}
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

func loadAvg() (float64, float64, float64) {
	if l1, l5, l15, ok := osLoadAvg(); ok {
		return l1, l5, l15
	}
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

// identity caches the user/group lookups (/etc/passwd+group); callers
// such as `ls -l` and `tar` use it per entry.
var (
	identityOnce sync.Once
	idUID        int
	idUser       string
	idGID        int
	idGroup      string
)

func identity() (uid int, user string, gid int, group string) {
	identityOnce.Do(func() {
		idUID = os.Getuid()
		idGID = os.Getgid()
		idUser = lookupUserName(idUID)
		idGroup = lookupGroupName(idGID)
	})
	return idUID, idUser, idGID, idGroup
}

func ttyNameFd(fd int) string {
	if dst, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd)); err == nil && strings.HasPrefix(dst, "/") {
		return dst
	}
	return ""
}

func ttyName() string {
	return ttyNameFd(int(os.Stdin.Fd()))
}

func syncDisks() {
	syscall.Sync()
}

func currentNice() int {
	p, err := syscall.Getpriority(syscall.PRIO_PROCESS, 0)
	if err != nil {
		return 0
	}
	if runtime.GOOS == "linux" {
		// Linux raw SYS_GETPRIORITY returns 20 - nice (1..40).
		return 20 - p
	}
	return p
}

func setProcNice(pid, n int) {
	if pid > 0 {
		_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, n)
	}
}

func selfPgid() int {
	return syscall.Getpgrp()
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
				fields := strings.Fields(line)
				if len(fields) < 10 {
					continue
				}
				state := tcpStates[fields[3]]
				if proto == "udp" {
					state = "UNCONN"
				} else if state == "" {
					state = fields[3]
				}
				local := parseProcNetAddr(fields[1])
				peer := parseProcNetAddr(fields[2])
				var txq, rxq int64
				if len(fields) > 4 {
					qq := strings.Split(fields[4], ":")
					if len(qq) == 2 {
						txq, _ = strconv.ParseInt(qq[0], 16, 64)
						rxq, _ = strconv.ParseInt(qq[1], 16, 64)
					}
				}
				inode := fields[9]
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
