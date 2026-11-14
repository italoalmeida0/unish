//go:build windows

package main

import (
	"fmt"
	"net"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	modIphlpapi             = syscall.NewLazyDLL("iphlpapi.dll")
	procGetExtendedTcpTable = modIphlpapi.NewProc("GetExtendedTcpTable")
	procGetTcpTable         = modIphlpapi.NewProc("GetTcpTable2")
	procGetExtendedUdpTable = modIphlpapi.NewProc("GetExtendedUdpTable")
	procGetUdpTable         = modIphlpapi.NewProc("GetUdpTable")
)

const (
	afInet                      = 2
	afInet6                     = 23
	tcpTableOwnerPidListener    = 3
	tcpTableOwnerPidConnections = 4
	tcpTableOwnerPidAll         = 5
	udpTableOwnerPid            = 1
	errorInsufficientBuffer     = 122

	mibTcpStateClosed    = 1
	mibTcpStateListen    = 2
	mibTcpStateSynSent   = 3
	mibTcpStateSynRcvd   = 4
	mibTcpStateEstab     = 5
	mibTcpStateFinWait1  = 6
	mibTcpStateFinWait2  = 7
	mibTcpStateCloseWait = 8
	mibTcpStateClosing   = 9
	mibTcpStateLastAck   = 10
	mibTcpStateTimeWait  = 11
	mibTcpStateDeleteTcb = 12
)

var tcpStateNames = map[uint32]string{
	mibTcpStateClosed: "CLOSE", mibTcpStateListen: "LISTEN",
	mibTcpStateSynSent: "SYN-SENT", mibTcpStateSynRcvd: "SYN-RECV",
	mibTcpStateEstab: "ESTAB", mibTcpStateFinWait1: "FIN-WAIT-1",
	mibTcpStateFinWait2: "FIN-WAIT-2", mibTcpStateCloseWait: "CLOSE-WAIT",
	mibTcpStateClosing: "CLOSING", mibTcpStateLastAck: "LAST-ACK",
	mibTcpStateTimeWait: "TIME-WAIT", mibTcpStateDeleteTcb: "CLOSE",
}

func callSizeTable(call func(bufPtr uintptr, sizePtr *uint32) uintptr) []byte {
	var size uint32
	r := call(0, &size)
	runtime.KeepAlive(&size)
	for attempt := 0; attempt < 5; attempt++ {
		if size == 0 || size > 16*1024*1024 {
			return nil
		}
		buf := make([]byte, size)
		r = call(uintptr(unsafe.Pointer(&buf[0])), &size)
		runtime.KeepAlive(buf)
		runtime.KeepAlive(&size)
		if r == 0 {
			return buf
		}
		if r != errorInsufficientBuffer {
			return nil
		}
	}
	return nil
}

type mibTcpRowOwnerPid struct {
	state      uint32
	localAddr  uint32
	localPort  uint32
	remoteAddr uint32
	remotePort uint32
	owningPid  uint32
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

type mibTcp6RowOwnerPid struct {
	localAddr     [16]byte
	localScopeId  uint32
	localPort     uint32
	remoteAddr    [16]byte
	remoteScopeId uint32
	remotePort    uint32
	state         uint32
	owningPid     uint32
}

func tcpTable() []sockInfo {
	out := tcpTableV4()
	out = append(out, tcpTableV6()...)
	return out
}

func queryExtendedTcpV4(tableClass int, forceListen bool) ([]sockInfo, bool) {
	buf := callSizeTable(func(bufPtr uintptr, sizePtr *uint32) uintptr {
		r, _, _ := procGetExtendedTcpTable.Call(
			bufPtr,
			uintptr(unsafe.Pointer(sizePtr)),
			0,
			uintptr(afInet),
			uintptr(tableClass),
			0,
		)
		return r
	})
	if len(buf) < 4 {
		return nil, false
	}
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	var out []sockInfo
	off := 4
	rowSize := int(unsafe.Sizeof(mibTcpRowOwnerPid{}))
	for i := uint32(0); i < n && off+rowSize <= len(buf); i++ {
		row := (*mibTcpRowOwnerPid)(unsafe.Pointer(&buf[off]))
		off += rowSize
		stateVal := row.state
		if forceListen {
			stateVal = mibTcpStateListen
		}
		state := tcpStateNames[stateVal]
		if state == "" {
			state = fmt.Sprintf("%d", stateVal)
		}
		peer := "0.0.0.0:0"
		if stateVal != mibTcpStateListen {
			peer = fmt.Sprintf("%s:%d", ipv4Str(row.remoteAddr), swapPort(row.remotePort))
		}
		out = append(out, sockInfo{
			proto: "tcp", state: state,
			local: fmt.Sprintf("%s:%d", ipv4Str(row.localAddr), swapPort(row.localPort)),
			peer:  peer,
			pid:   int(row.owningPid),
		})
	}
	runtime.KeepAlive(buf)
	return out, true
}

func tcpTableV4() []sockInfo {
	if procGetExtendedTcpTable.Find() == nil {
		// Query LISTENER and CONNECTIONS partitions separately: on Windows 11 ARM64,
		// iphlpapi's TCP_TABLE_OWNER_PID_ALL leaves listener dwState uninitialized.
		listeners, okL := queryExtendedTcpV4(tcpTableOwnerPidListener, true)
		conns, okC := queryExtendedTcpV4(tcpTableOwnerPidConnections, false)
		if okL || okC {
			return append(listeners, conns...)
		}
		if all, ok := queryExtendedTcpV4(tcpTableOwnerPidAll, false); ok {
			return all
		}
	}

	buf := callSizeTable(func(bufPtr uintptr, sizePtr *uint32) uintptr {
		r, _, _ := procGetTcpTable.Call(bufPtr, uintptr(unsafe.Pointer(sizePtr)), 0)
		return r
	})
	if len(buf) < 4 {
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
		peer := "0.0.0.0:0"
		if row.state != mibTcpStateListen {
			peer = fmt.Sprintf("%s:%d", ipv4Str(row.remoteAddr), swapPort(row.remotePort))
		}
		out = append(out, sockInfo{
			proto: "tcp", state: state,
			local: fmt.Sprintf("%s:%d", ipv4Str(row.localAddr), swapPort(row.localPort)),
			peer:  peer,
			pid:   int(row.owningPid),
		})
	}
	runtime.KeepAlive(buf)
	return out
}

func queryExtendedTcpV6(tableClass int, forceListen bool) ([]sockInfo, bool) {
	buf := callSizeTable(func(bufPtr uintptr, sizePtr *uint32) uintptr {
		r, _, _ := procGetExtendedTcpTable.Call(
			bufPtr,
			uintptr(unsafe.Pointer(sizePtr)),
			0,
			uintptr(afInet6),
			uintptr(tableClass),
			0,
		)
		return r
	})
	if len(buf) < 4 {
		return nil, false
	}
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	var out []sockInfo
	off := 4
	rowSize := int(unsafe.Sizeof(mibTcp6RowOwnerPid{}))
	for i := uint32(0); i < n && off+rowSize <= len(buf); i++ {
		row := (*mibTcp6RowOwnerPid)(unsafe.Pointer(&buf[off]))
		off += rowSize
		stateVal := row.state
		if forceListen {
			stateVal = mibTcpStateListen
		}
		state := tcpStateNames[stateVal]
		if state == "" {
			state = fmt.Sprintf("%d", stateVal)
		}
		peer := "[::]:0"
		if stateVal != mibTcpStateListen {
			peer = fmt.Sprintf("%s:%d", ipv6BracketStr(row.remoteAddr), swapPort(row.remotePort))
		}
		out = append(out, sockInfo{
			proto: "tcp", state: state,
			local: fmt.Sprintf("%s:%d", ipv6BracketStr(row.localAddr), swapPort(row.localPort)),
			peer:  peer,
			pid:   int(row.owningPid),
		})
	}
	runtime.KeepAlive(buf)
	return out, true
}

func tcpTableV6() []sockInfo {
	if procGetExtendedTcpTable.Find() != nil {
		return nil
	}
	listeners, okL := queryExtendedTcpV6(tcpTableOwnerPidListener, true)
	conns, okC := queryExtendedTcpV6(tcpTableOwnerPidConnections, false)
	if okL || okC {
		return append(listeners, conns...)
	}
	all, _ := queryExtendedTcpV6(tcpTableOwnerPidAll, false)
	return all
}

type mibUdprowOwnerPid struct {
	localAddr uint32
	localPort uint32
	owningPid uint32
}

type mibUdp6RowOwnerPid struct {
	localAddr    [16]byte
	localScopeId uint32
	localPort    uint32
	owningPid    uint32
}

func udpTable() []sockInfo {
	out := udpTableV4()
	out = append(out, udpTableV6()...)
	return out
}

func udpTableV4() []sockInfo {
	if procGetExtendedUdpTable.Find() == nil {
		buf := callSizeTable(func(bufPtr uintptr, sizePtr *uint32) uintptr {
			r, _, _ := procGetExtendedUdpTable.Call(
				bufPtr,
				uintptr(unsafe.Pointer(sizePtr)),
				0,
				uintptr(afInet),
				uintptr(udpTableOwnerPid),
				0,
			)
			return r
		})
		if len(buf) >= 4 {
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
			runtime.KeepAlive(buf)
			return out
		}
	}

	buf := callSizeTable(func(bufPtr uintptr, sizePtr *uint32) uintptr {
		r, _, _ := procGetUdpTable.Call(bufPtr, uintptr(unsafe.Pointer(sizePtr)), 0)
		return r
	})
	if len(buf) < 4 {
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
	runtime.KeepAlive(buf)
	return out
}

func udpTableV6() []sockInfo {
	if procGetExtendedUdpTable.Find() != nil {
		return nil
	}
	buf := callSizeTable(func(bufPtr uintptr, sizePtr *uint32) uintptr {
		r, _, _ := procGetExtendedUdpTable.Call(
			bufPtr,
			uintptr(unsafe.Pointer(sizePtr)),
			0,
			uintptr(afInet6),
			uintptr(udpTableOwnerPid),
			0,
		)
		return r
	})
	if len(buf) < 4 {
		return nil
	}
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	var out []sockInfo
	off := 4
	rowSize := int(unsafe.Sizeof(mibUdp6RowOwnerPid{}))
	for i := uint32(0); i < n && off+rowSize <= len(buf); i++ {
		row := (*mibUdp6RowOwnerPid)(unsafe.Pointer(&buf[off]))
		off += rowSize
		out = append(out, sockInfo{
			proto: "udp", state: "UNCONN",
			local: fmt.Sprintf("%s:%d", ipv6BracketStr(row.localAddr), swapPort(row.localPort)),
			peer:  "[::]:*",
			pid:   int(row.owningPid),
		})
	}
	runtime.KeepAlive(buf)
	return out
}

func ipv4Str(addr uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", byte(addr), byte(addr>>8), byte(addr>>16), byte(addr>>24))
}

func ipv6BracketStr(addr [16]byte) string {
	isV4Mapped := true
	for i := 0; i < 10; i++ {
		if addr[i] != 0 {
			isV4Mapped = false
			break
		}
	}
	if isV4Mapped && addr[10] == 0xff && addr[11] == 0xff {
		return fmt.Sprintf("[::ffff:%d.%d.%d.%d]", addr[12], addr[13], addr[14], addr[15])
	}
	return fmt.Sprintf("[%s]", net.IP(addr[:]).String())
}

func swapPort(p uint32) int {
	return int((p&0xFF)<<8 | (p>>8)&0xFF)
}
