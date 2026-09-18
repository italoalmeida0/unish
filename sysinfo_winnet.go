//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	modIphlpapi             = syscall.NewLazyDLL("iphlpapi.dll")
	procGetTcpTable         = modIphlpapi.NewProc("GetTcpTable2")
	procGetExtendedUdpTable = modIphlpapi.NewProc("GetExtendedUdpTable")
	procGetUdpTable         = modIphlpapi.NewProc("GetUdpTable")
)

const (
	afInet            = 2
	udpTableOwnerPid  = 1
	mibTcpStateClosed = 1
	mibTcpStateListen = 2
	mibTcpStateSynSent = 3
	mibTcpStateSynRcvd = 4
	mibTcpStateEstab = 5
	mibTcpStateFinWait1 = 6
	mibTcpStateFinWait2 = 7
	mibTcpStateCloseWait = 8
	mibTcpStateClosing = 9
	mibTcpStateLastAck = 10
	mibTcpStateTimeWait = 11
)

var tcpStateNames = map[uint32]string{
	mibTcpStateClosed: "CLOSE", mibTcpStateListen: "LISTEN",
	mibTcpStateSynSent: "SYN-SENT", mibTcpStateSynRcvd: "SYN-RECV",
	mibTcpStateEstab: "ESTAB", mibTcpStateFinWait1: "FIN-WAIT-1",
	mibTcpStateFinWait2: "FIN-WAIT-2", mibTcpStateCloseWait: "CLOSE-WAIT",
	mibTcpStateClosing: "CLOSING", mibTcpStateLastAck: "LAST-ACK",
	mibTcpStateTimeWait: "TIME-WAIT",
}

type mibTcprow2 struct {
	state      uint32
	localAddr  uint32
	localPort  uint32
	remoteAddr uint32
	remotePort uint32
	owningPid  uint32
	offload    uint32
}

func tcpTable() []sockInfo {
	var size uint32
	procGetTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0)
	if size == 0 || size > 16*1024*1024 {
		return nil
	}
	buf := make([]byte, size)
	r, _, _ := procGetTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
	if r != 0 {
		return nil
	}
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	var out []sockInfo
	off := 4
	rowSize := int(unsafe.Sizeof(mibTcprow2{}))
	for i := uint32(0); i < n && off+rowSize <= len(buf); i++ {
		row := (*mibTcprow2)(unsafe.Pointer(&buf[off]))
		off += rowSize
		state := tcpStateNames[row.state]
		if state == "" {
			state = fmt.Sprintf("%d", row.state)
		}
		out = append(out, sockInfo{
			proto: "tcp", state: state,
			local: fmt.Sprintf("%s:%d", ipv4Str(row.localAddr), swapPort(row.localPort)),
			peer:  fmt.Sprintf("%s:%d", ipv4Str(row.remoteAddr), swapPort(row.remotePort)),
			pid:   int(row.owningPid),
		})
	}
	return out
}

type mibUdprowOwnerPid struct {
	localAddr uint32
	localPort uint32
	owningPid uint32
}

func udpTable() []sockInfo {
	var size uint32
	if procGetExtendedUdpTable.Find() == nil {
		procGetExtendedUdpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, uintptr(afInet), uintptr(udpTableOwnerPid), 0)
		if size > 0 && size <= 16*1024*1024 {
			buf := make([]byte, size)
			r, _, _ := procGetExtendedUdpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, uintptr(afInet), uintptr(udpTableOwnerPid), 0)
			if r == 0 {
				n := *(*uint32)(unsafe.Pointer(&buf[0]))
				var out []sockInfo
				off := 4
				rowSize := int(unsafe.Sizeof(mibUdprowOwnerPid{}))
				for i := uint32(0); i < n && off+rowSize <= len(buf); i++ {
					row := (*mibUdprowOwnerPid)(unsafe.Pointer(&buf[off]))
					off += rowSize
					out = append(out, sockInfo{
						proto: "udp", state: "UNCONN",
						local: fmt.Sprintf("%s:%d", ipv4Str(row.localAddr), swapPort(row.localPort)),
						peer:  "0.0.0.0:*",
						pid:   int(row.owningPid),
					})
				}
				return out
			}
		}
	}
	procGetUdpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0)
	if size == 0 || size > 16*1024*1024 {
		return nil
	}
	buf := make([]byte, size)
	r, _, _ := procGetUdpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
	if r != 0 {
		return nil
	}
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	var out []sockInfo
	off := 4
	rowSize := 8
	for i := uint32(0); i < n && off+rowSize <= len(buf); i++ {
		addr := *(*uint32)(unsafe.Pointer(&buf[off]))
		port := *(*uint32)(unsafe.Pointer(&buf[off+4]))
		off += rowSize
		out = append(out, sockInfo{
			proto: "udp", state: "UNCONN",
			local: fmt.Sprintf("%s:%d", ipv4Str(addr), swapPort(port)),
			peer:  "0.0.0.0:*",
		})
	}
	return out
}

func ipv4Str(addr uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", byte(addr), byte(addr>>8), byte(addr>>16), byte(addr>>24))
}

func swapPort(p uint32) int {
	return int((p&0xFF)<<8 | (p>>8)&0xFF)
}
