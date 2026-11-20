package faultnet

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"
)

// NAT is a home router for one host, on loopback: the host's UDP socket (Listen, given to the
// WireGuard transport as WG.PacketConn) sends everything out of the router's one public port
// (Addr), and the router lets a datagram in only from an address the host has sent to
// before: endpoint-independent mapping with address-and-port-dependent filtering, the common
// home NAT. Two hosts behind such routers reach each other only by both punching (each
// sends first, which opens its router for the other), which is what the WireGuard
// transport's hole punching does. Nothing unsolicited gets in.
//
// Block cuts UDP both ways (a network that blocks UDP, or a router that lost its state for
// good), so the node must fall back to another transport.
type NAT struct {
	pub *net.UDPConn

	mu      sync.Mutex
	allowed map[netip.AddrPort]bool // where the host has sent to
	blocked bool
	host    *natConn
	dropped int // unsolicited (or blocked) inbound datagrams
	closed  bool
}

// NewNAT starts a router with a fresh public port on 127.0.0.1.
func NewNAT() (*NAT, error) {
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	n := &NAT{pub: pc, allowed: map[netip.AddrPort]bool{}}
	go n.inbound()
	return n, nil
}

// Addr is the router's public address: what the outside sees the host as.
func (n *NAT) Addr() string { return n.pub.LocalAddr().String() }

// Listen returns the host's socket behind the router, replacing any earlier one (which is
// closed). Its local address is the public one, as a host learns from STUN.
func (n *NAT) Listen() (net.PacketConn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, net.ErrClosed
	}
	if n.host != nil {
		n.host.closeLocked()
	}
	n.host = &natConn{n: n, in: make(chan natPacket, 512), done: make(chan struct{})}
	return n.host, nil
}

// Block drops every datagram in both directions while on.
func (n *NAT) Block(on bool) {
	n.mu.Lock()
	n.blocked = on
	n.mu.Unlock()
}

// Dropped counts inbound datagrams the router refused (unsolicited, or blocked).
func (n *NAT) Dropped() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dropped
}

// Close stops the router.
func (n *NAT) Close() {
	n.mu.Lock()
	n.closed = true
	if n.host != nil {
		n.host.closeLocked()
	}
	n.mu.Unlock()
	n.pub.Close()
}

// inbound filters what arrives at the public port.
func (n *NAT) inbound() {
	buf := make([]byte, 65536)
	for {
		sz, from, err := n.pub.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		n.mu.Lock()
		h := n.host
		ok := !n.blocked && n.allowed[from] && h != nil
		if !ok {
			n.dropped++
		}
		n.mu.Unlock()
		if !ok {
			continue
		}
		p := natPacket{b: append([]byte(nil), buf[:sz]...), from: from}
		select {
		case h.in <- p:
		case <-h.done:
		default: // a full queue drops, as a router does
		}
	}
}

type natPacket struct {
	b    []byte
	from netip.AddrPort
}

// natConn is the host's side of a NAT: a net.PacketConn.
type natConn struct {
	n    *NAT
	in   chan natPacket
	done chan struct{}
	once sync.Once

	dmu      sync.Mutex
	deadline time.Time
}

func (c *natConn) closeLocked() { c.once.Do(func() { close(c.done) }) }

// ReadFrom implements net.PacketConn.
func (c *natConn) ReadFrom(b []byte) (int, net.Addr, error) {
	c.dmu.Lock()
	dl := c.deadline
	c.dmu.Unlock()
	var timeout <-chan time.Time
	if !dl.IsZero() {
		t := time.NewTimer(time.Until(dl))
		defer t.Stop()
		timeout = t.C
	}
	select {
	case p := <-c.in:
		return copy(b, p.b), net.UDPAddrFromAddrPort(p.from), nil
	case <-c.done:
		return 0, nil, net.ErrClosed
	case <-timeout:
		return 0, nil, os.ErrDeadlineExceeded
	}
}

// WriteTo implements net.PacketConn: the datagram leaves from the router's public port, and
// the router lets answers from that address in from now on.
func (c *natConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	ua, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, errors.New("faultnet: not a UDP address")
	}
	ap := ua.AddrPort()
	ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	c.n.mu.Lock()
	blocked := c.n.blocked
	if !blocked {
		c.n.allowed[ap] = true
	}
	c.n.mu.Unlock()
	if blocked {
		return len(b), nil // gone, as on a network that drops UDP
	}
	return c.n.pub.WriteToUDPAddrPort(b, ap)
}

// Close implements net.PacketConn.
func (c *natConn) Close() error {
	c.n.mu.Lock()
	c.closeLocked()
	if c.n.host == c {
		c.n.host = nil
	}
	c.n.mu.Unlock()
	return nil
}

// LocalAddr implements net.PacketConn: the router's public address.
func (c *natConn) LocalAddr() net.Addr { return c.n.pub.LocalAddr() }

// SetDeadline implements net.PacketConn (reads only; writes never block).
func (c *natConn) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }

// SetReadDeadline implements net.PacketConn.
func (c *natConn) SetReadDeadline(t time.Time) error {
	c.dmu.Lock()
	c.deadline = t
	c.dmu.Unlock()
	return nil
}

// SetWriteDeadline implements net.PacketConn.
func (c *natConn) SetWriteDeadline(time.Time) error { return nil }
