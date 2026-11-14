//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	modKernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceEx  = modKernel32.NewProc("GetDiskFreeSpaceExW")
	procGlobalMemoryStatus  = modKernel32.NewProc("GlobalMemoryStatusEx")
	procGetTickCount64      = modKernel32.NewProc("GetTickCount64")
	procK32GetProcessMemory = modKernel32.NewProc("K32GetProcessMemoryInfo")
)

func diskUsage(path string) (total, free, avail int64, fstype, mount string) {
	absPath := path
	if a, err := filepath.Abs(path); err == nil {
		absPath = a
	}
	dir := absPath
	if fi, err := os.Stat(absPath); err == nil && !fi.IsDir() {
		dir = filepath.Dir(absPath)
	}
	vol := filepath.VolumeName(dir)
	if vol == "" {
		vol = "C:"
	}
	root := vol + `\`
	queryDir := dir
	if queryDir == "" {
		queryDir = root
	}
	utf16Dir, err := syscall.UTF16PtrFromString(queryDir)
	if err != nil {
		return 0, 0, 0, "", root
	}
	var freeB, totalB, availB uint64
	r, _, _ := procGetDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(utf16Dir)),
		uintptr(unsafe.Pointer(&availB)),
		uintptr(unsafe.Pointer(&totalB)),
		uintptr(unsafe.Pointer(&freeB)),
	)
	runtime.KeepAlive(utf16Dir)
	runtime.KeepAlive(&availB)
	runtime.KeepAlive(&totalB)
	runtime.KeepAlive(&freeB)
	if r == 0 {
		return 0, 0, 0, "", root
	}
	return int64(totalB), int64(freeB), int64(availB), "ntfs", root
}

func blocksOf(fi os.FileInfo) int64 {
	return (fi.Size() + 511) / 512
}

func deviceOf(path string) string {
	return "0h/0d"
}

func inodeOf(path string) int64 {
	return 0
}

func linksOf(path string, fi os.FileInfo) int {
	return 1
}

func uidOf(path string) int {
	return 0
}

func gidOf(path string) int {
	return 0
}

func userOf(path string) string {
	if u, err := userCurrent(); err == nil && u != "" {
		if i := strings.LastIndexAny(u, `\/`); i >= 0 {
			u = u[i+1:]
		}
		return u
	}
	return "user"
}

func groupOf(path string) string {
	return "group"
}

func birthOf(path string, fi os.FileInfo) string {
	return fi.ModTime().Format("2006-01-02 15:04:05.000000000 -0700")
}

type memStat struct {
	total, free, cached, buffers, shared, avail int64
	swapTotal, swapFree                         int64
}

type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

func memInfo() memStat {
	var m memStat
	var st memoryStatusEx
	st.length = uint32(unsafe.Sizeof(st))
	r, _, _ := procGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&st)))
	runtime.KeepAlive(&st)
	if r == 0 {
		return m
	}
	m.total = int64(st.totalPhys)
	m.free = int64(st.availPhys)
	m.avail = int64(st.availPhys)
	m.swapTotal = int64(st.totalPageFile)
	m.swapFree = int64(st.availPageFile)
	return m
}

func bootTime() time.Time {
	r, _, _ := procGetTickCount64.Call()
	ms := int64(r)
	return time.Now().Add(-time.Duration(ms) * time.Millisecond)
}

func loadAvg() (float64, float64, float64) {
	return 0, 0, 0
}

// identity caches the user lookup; callers use it per entry.
var (
	identityOnce sync.Once
	idUser       string
)

func identity() (uid int, user string, gid int, group string) {
	identityOnce.Do(func() {
		u := "user"
		if name, err := userCurrent(); err == nil && name != "" {
			u = name
			if i := strings.LastIndexAny(u, `\/`); i >= 0 {
				u = u[i+1:]
			}
		}
		idUser = u
	})
	return 0, idUser, 0, "group"
}

func ttyNameFd(fd int) string {
	_ = fd
	return ""
}

func ttyName() string { return "" }

func syncDisks() {
}

func currentNice() int { return 0 }

func setProcNice(pid, n int) {
	_, _ = pid, n
}

const (
	th32csSnapProcess  = 0x00000002
	maxPath            = 260
	invalidHandleValue = ^uintptr(0)
)

type processEntry32 struct {
	size            uint32
	usage           uint32
	processID       uint32
	defaultHeapID   uintptr
	moduleID        uint32
	threads         uint32
	parentProcessID uint32
	priClassBase    int32
	flags           uint32
	exeFile         [maxPath]uint16
}

var (
	procCreateSnapshot   = modKernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32First   = modKernel32.NewProc("Process32FirstW")
	procProcess32Next    = modKernel32.NewProc("Process32NextW")
	procCloseHandle      = modKernel32.NewProc("CloseHandle")
	procOpenProcess      = modKernel32.NewProc("OpenProcess")
	procGetProcessMemory = modPsapi.NewProc("GetProcessMemoryInfo")
	modPsapi             = syscall.NewLazyDLL("psapi.dll")
)

func listProcs() ([]procInfo, error) {
	snap, _, _ := procCreateSnapshot.Call(uintptr(th32csSnapProcess), 0)
	if snap == invalidHandleValue || snap == 0 {
		return nil, fmt.Errorf("snapshot failed")
	}
	defer procCloseHandle.Call(snap)
	var out []procInfo
	var pe processEntry32
	pe.size = uint32(unsafe.Sizeof(pe))
	r, _, _ := procProcess32First.Call(snap, uintptr(unsafe.Pointer(&pe)))
	for r != 0 {
		name := syscall.UTF16ToString(pe.exeFile[:])
		pid := pe.processID
		ppid := pe.parentProcessID
		out = append(out, procInfo{
			pid: int(pid), ppid: int(ppid),
			user: "?", comm: name,
			cpu: -1, memPct: -1, vszKB: -1, rssKB: procRSS(pid),
			tty: "?", stat: "?", start: "?", time: "00:00:00", cmd: name,
		})
		r, _, _ = procProcess32Next.Call(snap, uintptr(unsafe.Pointer(&pe)))
	}
	runtime.KeepAlive(&pe)
	return out, nil
}

type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

func procRSS(pid uint32) int64 {
	if pid == 0 {
		return -1
	}
	const (
		processQueryInfo        = 0x0400
		processQueryLimitedInfo = 0x1000
		processVMRead           = 0x0010
	)
	h, _, _ := procOpenProcess.Call(uintptr(processQueryLimitedInfo|processVMRead), 0, uintptr(pid))
	if h == 0 {
		h, _, _ = procOpenProcess.Call(uintptr(processQueryLimitedInfo), 0, uintptr(pid))
	}
	if h == 0 {
		h, _, _ = procOpenProcess.Call(uintptr(processQueryInfo|processVMRead), 0, uintptr(pid))
	}
	if h == 0 {
		return -1
	}
	defer procCloseHandle.Call(h)
	var pmc processMemoryCounters
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	var r uintptr
	if procK32GetProcessMemory.Find() == nil {
		r, _, _ = procK32GetProcessMemory.Call(h, uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb))
	} else {
		r, _, _ = procGetProcessMemory.Call(h, uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb))
	}
	runtime.KeepAlive(&pmc)
	if r == 0 {
		return -1
	}
	return int64(pmc.workingSetSize) / 1024
}

func listSockets(protos []string) ([]sockInfo, error) {
	var out []sockInfo
	for _, p := range protos {
		if p == "tcp" {
			out = append(out, tcpTable()...)
		} else {
			out = append(out, udpTable()...)
		}
	}
	if procs, err := listProcs(); err == nil {
		pMap := make(map[int]string, len(procs))
		for _, pr := range procs {
			pMap[pr.pid] = pr.cmd
		}
		for i := range out {
			if out[i].pid > 0 {
				out[i].procName = pMap[out[i].pid]
			}
		}
	}
	return out, nil
}
