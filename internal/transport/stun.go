package transport

// A minimal STUN client (RFC 5389 Binding requests only) that runs on
// WireGuard's own UDP socket, so the public address it learns is the very
// mapping WireGuard's packets use through the NAT. That is what makes hole
// punching possible: a peer sending to that address reaches this socket.
//
// STUN packets are told apart from WireGuard packets by their fixed magic
// cookie (bytes 4..8); WireGuard messages start with a type byte 1..4 and
// three zero bytes, so the two can never be confused.

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

const (
	stunMagic          = 0x2112A442
	stunBindingRequest = 0x0001
	stunBindingSuccess = 0x0101
	stunHeaderSize     = 20
	stunAttrMapped     = 0x0001
	stunAttrXORMapped  = 0x0020
)

type stunTxID [12]byte

// stunRequest builds a Binding request with a random transaction id.
func stunRequest() ([]byte, stunTxID) {
	var id stunTxID
	rand.Read(id[:])
	b := make([]byte, stunHeaderSize)
	binary.BigEndian.PutUint16(b[0:], stunBindingRequest)
	binary.BigEndian.PutUint16(b[2:], 0) // no attributes
	binary.BigEndian.PutUint32(b[4:], stunMagic)
	copy(b[8:], id[:])
	return b, id
}

// isSTUN reports whether a datagram is a STUN message.
func isSTUN(b []byte) bool {
	return len(b) >= stunHeaderSize && b[0]&0xC0 == 0 && binary.BigEndian.Uint32(b[4:]) == stunMagic
}

// parseSTUNResponse returns the transaction id and the mapped address of a
// Binding success response.
func parseSTUNResponse(b []byte) (stunTxID, netip.AddrPort, error) {
	var id stunTxID
	if !isSTUN(b) || binary.BigEndian.Uint16(b[0:]) != stunBindingSuccess {
		return id, netip.AddrPort{}, errors.New("not a STUN binding success")
	}
	n := int(binary.BigEndian.Uint16(b[2:]))
	if stunHeaderSize+n > len(b) {
		return id, netip.AddrPort{}, errors.New("truncated STUN message")
	}
	copy(id[:], b[8:20])
	var mapped netip.AddrPort
	attrs := b[stunHeaderSize : stunHeaderSize+n]
	for len(attrs) >= 4 {
		typ, l := binary.BigEndian.Uint16(attrs[0:]), int(binary.BigEndian.Uint16(attrs[2:]))
		if 4+l > len(attrs) {
			break
		}
		v := attrs[4 : 4+l]
		switch typ {
		case stunAttrXORMapped:
			if ap, ok := decodeSTUNAddr(v, true, b[4:20]); ok {
				return id, ap, nil // preferred: middleboxes don't rewrite it
			}
		case stunAttrMapped:
			if ap, ok := decodeSTUNAddr(v, false, nil); ok {
				mapped = ap
			}
		}
		attrs = attrs[4+(l+3)&^3:] // attributes are padded to 4 bytes
	}
	if mapped.IsValid() {
		return id, mapped, nil
	}
	return id, netip.AddrPort{}, errors.New("no mapped address in STUN response")
}

func decodeSTUNAddr(v []byte, xor bool, key []byte) (netip.AddrPort, bool) {
	if len(v) < 4 {
		return netip.AddrPort{}, false
	}
	port := binary.BigEndian.Uint16(v[2:])
	var ip []byte
	switch v[1] {
	case 0x01:
		if len(v) < 8 {
			return netip.AddrPort{}, false
		}
		ip = append([]byte(nil), v[4:8]...)
	case 0x02:
		if len(v) < 20 {
			return netip.AddrPort{}, false
		}
		ip = append([]byte(nil), v[4:20]...)
	default:
		return netip.AddrPort{}, false
	}
	if xor {
		port ^= uint16(stunMagic >> 16)
		for i := range ip {
			ip[i] ^= key[i] // magic cookie, then the transaction id (IPv6)
		}
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(addr.Unmap(), port), true
}

// stunBind wraps WireGuard's UDP bind: it removes STUN responses from the
// packet stream (WireGuard never sees them) and can send STUN requests from
// the same socket.
type stunBind struct {
	conn.Bind

	mu      sync.Mutex
	pending map[stunTxID]chan netip.AddrPort
	onPunch func(from conn.Endpoint, pub [32]byte) // see wg.go, "punch packets"

	// natFilter (tests only) makes the socket behave like a NAT that drops
	// inbound packets from any address it hasn't sent to.
	natFilter bool
	sentTo    map[string]bool
}

// Send records destinations for natFilter, then sends.
func (b *stunBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	b.mu.Lock()
	if b.natFilter {
		b.sentTo[ep.DstToString()] = true
	}
	b.mu.Unlock()
	return b.Bind.Send(bufs, ep)
}

// Punch packets: "SOLP" + the sender's WireGuard public key. They open the
// sender's NAT towards the peer and tell the peer where the sender really
// appears from. They are unauthenticated hints only (see wg.go).
const punchMagic = "SOLP"

func punchPacket(pub [32]byte) []byte { return append([]byte(punchMagic), pub[:]...) }

func isPunch(b []byte) bool { return len(b) == len(punchMagic)+32 && string(b[:4]) == punchMagic }

func newSTUNBind(inner conn.Bind) *stunBind {
	return &stunBind{Bind: inner, pending: map[stunTxID]chan netip.AddrPort{}}
}

// Open wraps every receive function with the STUN filter.
func (b *stunBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	fns, actual, err := b.Bind.Open(port)
	if err != nil {
		return nil, 0, err
	}
	wrapped := make([]conn.ReceiveFunc, len(fns))
	for i, fn := range fns {
		wrapped[i] = b.filter(fn)
	}
	return wrapped, actual, nil
}

func (b *stunBind) filter(fn conn.ReceiveFunc) conn.ReceiveFunc {
	return func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		for {
			n, err := fn(packets, sizes, eps)
			if err != nil {
				return n, err
			}
			kept := 0
			for i := 0; i < n; i++ {
				p := packets[i][:sizes[i]]
				b.mu.Lock()
				blocked := b.natFilter && !b.sentTo[eps[i].DstToString()]
				b.mu.Unlock()
				if blocked {
					continue
				}
				if isSTUN(p) {
					b.deliver(p)
					continue
				}
				if isPunch(p) {
					b.mu.Lock()
					f := b.onPunch
					b.mu.Unlock()
					if f != nil {
						var pub [32]byte
						copy(pub[:], p[4:])
						f(eps[i], pub)
					}
					continue
				}
				if kept != i {
					// Copy the bytes: wireguard-go reads each slot's own
					// buffer, so swapping slice headers would hand it stale data.
					copy(packets[kept][:cap(packets[kept])], p)
					sizes[kept], eps[kept] = sizes[i], eps[i]
				}
				kept++
			}
			if kept > 0 || n == 0 {
				return kept, nil
			}
			// The whole batch was STUN: read again rather than hand
			// WireGuard an empty batch.
		}
	}
}

func (b *stunBind) deliver(p []byte) {
	id, ap, err := parseSTUNResponse(p)
	if err != nil {
		return
	}
	b.mu.Lock()
	ch := b.pending[id]
	delete(b.pending, id)
	b.mu.Unlock()
	if ch != nil {
		ch <- ap
	}
}

// sendRaw sends one datagram from the WireGuard socket.
func (b *stunBind) sendRaw(pkt []byte, to string) error {
	ep, err := b.Bind.ParseEndpoint(to)
	if err != nil {
		return err
	}
	return b.Send([][]byte{pkt}, ep)
}

// Query asks one STUN server for this socket's public address.
func (b *stunBind) Query(server netip.AddrPort, timeout time.Duration) (netip.AddrPort, error) {
	ep, err := b.Bind.ParseEndpoint(server.String())
	if err != nil {
		return netip.AddrPort{}, err
	}
	req, id := stunRequest()
	ch := make(chan netip.AddrPort, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	}()
	if err := b.Send([][]byte{req}, ep); err != nil {
		return netip.AddrPort{}, err
	}
	select {
	case ap := <-ch:
		return ap, nil
	case <-time.After(timeout):
		return netip.AddrPort{}, errors.New("STUN: no answer from " + server.String())
	}
}

// resolveSTUN turns "host:port" server names into addresses (our own
// config, so names are fine here, unlike peer-supplied candidates).
func resolveSTUN(servers []string) []netip.AddrPort {
	var out []netip.AddrPort
	for _, s := range servers {
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			continue
		}
		ips, err := net.LookupIP(host)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if a, ok := netip.AddrFromSlice(ip); ok {
				if ap, err := netip.ParseAddrPort(net.JoinHostPort(a.Unmap().String(), port)); err == nil {
					out = append(out, ap)
				}
			}
		}
	}
	return out
}
