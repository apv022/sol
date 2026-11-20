package daemon

import (
	"bufio"
	"encoding/hex"
	"errors"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// Same-user detection for browser auto-login (design/ADMIN.md "Who may use it").
//
// A browser that loads the UI root without a session is let in when the TCP connection it
// came over belongs to the same OS user as the daemon. The kernel's socket tables say who owns
// the client end of a loopback connection, and that cannot be forged by the peer.
//
// Invariants:
//   - The answer is computed per request from the live socket table; nothing is cached, so a
//     port reused by another user is never mistaken for the earlier owner.
//   - Only a loopback client talking to a loopback listener qualifies.
//   - Any doubt (no table, no matching row, unsupported OS) means "not the same user": the
//     browser falls back to the login screen.

// SameUserFunc reports whether the process that owns the client end of the connection
// remote -> local is run by the same OS user as this daemon. An error means "could not tell".
type SameUserFunc func(remote, local netip.AddrPort) (bool, error)

// ErrSameUserUnsupported means this OS cannot map a connection to its owner here.
var ErrSameUserUnsupported = errors.New("same-user detection is not supported on this system")

// loopbackPair reports that both ends are loopback addresses.
func loopbackPair(remote, local netip.AddrPort) bool {
	return remote.Addr().Unmap().IsLoopback() && local.Addr().Unmap().IsLoopback()
}

// parseProcNetUID reads a /proc/net/tcp or /proc/net/tcp6 table and returns the uid of the
// socket whose local address is client and whose remote address is server (the client end of
// the connection). Addresses are compared after Unmap, so an IPv4 client on a tcp6 row matches.
func parseProcNetUID(r io.Reader, client, server netip.AddrPort) (uint32, bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	first := true
	for sc.Scan() {
		if first { // header line
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 8 {
			continue
		}
		l, ok1 := parseProcAddr(f[1])
		rm, ok2 := parseProcAddr(f[2])
		if !ok1 || !ok2 || !sameAddrPort(l, client) || !sameAddrPort(rm, server) {
			continue
		}
		uid, err := strconv.ParseUint(f[7], 10, 32)
		if err != nil {
			return 0, false
		}
		return uint32(uid), true
	}
	return 0, false
}

func sameAddrPort(a, b netip.AddrPort) bool {
	return a.Port() == b.Port() && a.Addr().Unmap() == b.Addr().Unmap()
}

// parseProcAddr parses "0100007F:1F90" (IPv4) or a 32-hex-digit IPv6 form. The kernel prints
// each 32-bit word of the address in host (little-endian) byte order.
func parseProcAddr(s string) (netip.AddrPort, bool) {
	h, p, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, false
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	b, err := hex.DecodeString(h)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return netip.AddrPort{}, false
	}
	for i := 0; i+3 < len(b); i += 4 { // reverse each word
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	a, _ := netip.AddrFromSlice(b)
	return netip.AddrPortFrom(a, uint16(port)), true
}
