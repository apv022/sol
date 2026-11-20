package daemon

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"syscall"
	"unsafe"
)

var (
	iphlpapi                    = syscall.NewLazyDLL("iphlpapi.dll")
	procGetTCPTable             = iphlpapi.NewProc("GetExtendedTcpTable")
	errTableNotFound            = errors.New("connection not found in the TCP table")
	tcpTableOwnerPIDAll uintptr = 5
)

const processQueryLimitedInformation = 0x1000

// defaultSameUser (Windows) finds the owning PID of the client end with GetExtendedTcpTable
// (TCP_TABLE_OWNER_PID_ALL) and compares that process token's user SID with this process's.
func defaultSameUser(remote, local netip.AddrPort) (bool, error) {
	if !loopbackPair(remote, local) {
		return false, nil
	}
	pid, err := tcpOwnerPID(remote, local)
	if err != nil {
		return false, err
	}
	mine, err := tokenSID(syscall.Handle(^uintptr(0))) // pseudo handle of this process
	if err != nil {
		return false, err
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return false, err // another user's process: access denied, which is the answer
	}
	defer syscall.CloseHandle(h)
	theirs, err := tokenSID(h)
	if err != nil {
		return false, err
	}
	return mine == theirs, nil
}

func tokenSID(proc syscall.Handle) (string, error) {
	var tok syscall.Token
	if err := syscall.OpenProcessToken(proc, syscall.TOKEN_QUERY, &tok); err != nil {
		return "", err
	}
	defer tok.Close()
	var n uint32
	_ = syscall.GetTokenInformation(tok, syscall.TokenUser, nil, 0, &n)
	if n == 0 {
		return "", errors.New("no token user")
	}
	buf := make([]byte, n)
	if err := syscall.GetTokenInformation(tok, syscall.TokenUser, &buf[0], n, &n); err != nil {
		return "", err
	}
	u := (*syscall.Tokenuser)(unsafe.Pointer(&buf[0]))
	return u.User.Sid.String()
}

// tcpOwnerPID looks the connection up in the IPv4 then the IPv6 table.
func tcpOwnerPID(client, server netip.AddrPort) (uint32, error) {
	for _, af := range []uintptr{2, 23} { // AF_INET, AF_INET6
		var size uint32
		var buf []byte
		for try := 0; try < 4; try++ {
			var p unsafe.Pointer
			if len(buf) > 0 {
				p = unsafe.Pointer(&buf[0])
			}
			r, _, _ := procGetTCPTable.Call(uintptr(p), uintptr(unsafe.Pointer(&size)), 0, af, tcpTableOwnerPIDAll, 0)
			if r == 0 {
				break
			}
			if r != uintptr(syscall.ERROR_INSUFFICIENT_BUFFER) {
				buf = nil
				break
			}
			buf = make([]byte, size+4096)
			size = uint32(len(buf))
		}
		if len(buf) == 0 {
			continue
		}
		if pid, ok := scanTCPTable(buf, af == 23, client, server); ok {
			return pid, nil
		}
	}
	return 0, errTableNotFound
}

// scanTCPTable walks MIB_TCPTABLE_OWNER_PID (rows of 6 DWORDs) or MIB_TCP6TABLE_OWNER_PID
// (rows of 56 bytes); ports are network byte order in the low 16 bits of a DWORD.
func scanTCPTable(b []byte, v6 bool, client, server netip.AddrPort) (uint32, bool) {
	if len(b) < 4 {
		return 0, false
	}
	n := int(binary.LittleEndian.Uint32(b))
	rowLen := 24
	if v6 {
		rowLen = 56
	}
	port := func(o int) uint16 { return binary.BigEndian.Uint16(b[o : o+2]) }
	for i := 0; i < n && 4+(i+1)*rowLen <= len(b); i++ {
		r := 4 + i*rowLen
		var la, ra netip.Addr
		var lp, rp uint16
		var pid uint32
		if v6 {
			la = netip.AddrFrom16([16]byte(b[r : r+16]))
			lp = port(r + 20)
			ra = netip.AddrFrom16([16]byte(b[r+24 : r+40]))
			rp = port(r + 44)
			pid = binary.LittleEndian.Uint32(b[r+52:])
		} else {
			la = netip.AddrFrom4([4]byte(b[r+4 : r+8]))
			lp = port(r + 8)
			ra = netip.AddrFrom4([4]byte(b[r+12 : r+16]))
			rp = port(r + 16)
			pid = binary.LittleEndian.Uint32(b[r+20:])
		}
		if sameAddrPort(netip.AddrPortFrom(la, lp), client) && sameAddrPort(netip.AddrPortFrom(ra, rp), server) {
			return pid, true
		}
	}
	return 0, false
}
