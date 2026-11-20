package transport

import (
	"errors"
	"net"
	"net/netip"
	"sync"

	"golang.zx2c4.com/wireguard/conn"
)

// packetBind is a WireGuard bind over a net.PacketConn from a function, for WG.PacketConn:
// a test puts the transport behind a simulated NAT (internal/faultnet.NAT) this way, without
// root or network namespaces. One packet per call; the device opens it again after a Close.
type packetBind struct {
	open func() (net.PacketConn, error)

	mu sync.Mutex
	pc net.PacketConn
}

// Open implements conn.Bind. The port is the PacketConn's own.
func (b *packetBind) Open(uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pc != nil {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	pc, err := b.open()
	if err != nil {
		return nil, 0, err
	}
	ua, ok := pc.LocalAddr().(*net.UDPAddr)
	if !ok {
		pc.Close()
		return nil, 0, errors.New("wireguard: the packet conn is not UDP")
	}
	b.pc = pc
	recv := func(bufs [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		n, from, err := pc.ReadFrom(bufs[0])
		if err != nil {
			return 0, err
		}
		fa, ok := from.(*net.UDPAddr)
		if !ok {
			return 0, nil
		}
		ap := fa.AddrPort()
		sizes[0], eps[0] = n, &conn.StdNetEndpoint{AddrPort: netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())}
		return 1, nil
	}
	return []conn.ReceiveFunc{recv}, uint16(ua.Port), nil
}

// Close implements conn.Bind.
func (b *packetBind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pc == nil {
		return nil
	}
	err := b.pc.Close()
	b.pc = nil
	return err
}

// SetMark implements conn.Bind (no marks on a test socket).
func (b *packetBind) SetMark(uint32) error { return nil }

// Send implements conn.Bind.
func (b *packetBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	b.mu.Lock()
	pc := b.pc
	b.mu.Unlock()
	if pc == nil {
		return net.ErrClosed
	}
	ap, err := netip.ParseAddrPort(ep.DstToString())
	if err != nil {
		return err
	}
	to := net.UDPAddrFromAddrPort(ap)
	for _, buf := range bufs {
		if _, err := pc.WriteTo(buf, to); err != nil {
			return err
		}
	}
	return nil
}

// ParseEndpoint implements conn.Bind.
func (b *packetBind) ParseEndpoint(s string) (conn.Endpoint, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return nil, err
	}
	return &conn.StdNetEndpoint{AddrPort: ap}, nil
}

// BatchSize implements conn.Bind.
func (b *packetBind) BatchSize() int { return 1 }
