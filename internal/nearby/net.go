package nearby

import (
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
)

// Conn is an open beacon socket: it sends to the group on every interface it covers and
// receives what arrives from the group.
type Conn interface {
	// Send writes one datagram to the group on every interface.
	Send(b []byte) error
	// Receive blocks for the next datagram, returning its length and source address. It
	// fails once the Conn is closed.
	Receive(buf []byte) (int, netip.Addr, error)
	// Interfaces is how many interfaces the Conn covers (0: the system's default one).
	Interfaces() int
	Close() error
}

// Network opens beacon sockets. The real one is UDP multicast (UDP); tests use a Bus.
type Network interface {
	Open() (Conn, error)
}

// ErrNoInterface is returned by Open when no interface can carry beacons right now (offline).
var ErrNoInterface = errors.New("nearby: no network interface for beacons")

var errClosed = errors.New("nearby: closed")

// Bus is an in-memory multicast group for tests: every datagram one of its Conns sends
// reaches every open Conn (the sender's too, as with multicast loopback), from the sender's
// address.
type Bus struct {
	mu    sync.Mutex
	conns map[*busConn]struct{}
}

// NewBus returns an empty bus.
func NewBus() *Bus { return &Bus{conns: map[*busConn]struct{}{}} }

// Network returns a Network on the bus whose Conns send from src.
func (b *Bus) Network(src netip.Addr) *BusNetwork { return &BusNetwork{b: b, src: src} }

// Inject delivers a datagram to every open Conn as if src had sent it (tests: replays,
// strangers).
func (b *Bus) Inject(p []byte, src netip.Addr) { b.deliver(p, src) }

func (b *Bus) deliver(p []byte, src netip.Addr) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.conns {
		select {
		case c.ch <- busPacket{append([]byte(nil), p...), src}:
		default: // a full queue drops, as a busy socket would
		}
	}
}

// BusNetwork is one device's attachment to a Bus.
type BusNetwork struct {
	b    *Bus
	src  netip.Addr
	sent atomic.Int64
}

// Sent counts the datagrams sent through this network.
func (n *BusNetwork) Sent() int { return int(n.sent.Load()) }

// Open implements Network.
func (n *BusNetwork) Open() (Conn, error) {
	c := &busConn{n: n, ch: make(chan busPacket, 64), done: make(chan struct{})}
	n.b.mu.Lock()
	n.b.conns[c] = struct{}{}
	n.b.mu.Unlock()
	return c, nil
}

type busPacket struct {
	p   []byte
	src netip.Addr
}

type busConn struct {
	n    *BusNetwork
	ch   chan busPacket
	done chan struct{}
	once sync.Once
}

func (c *busConn) Send(p []byte) error {
	select {
	case <-c.done:
		return errClosed
	default:
	}
	c.n.sent.Add(1)
	c.n.b.deliver(p, c.n.src)
	return nil
}

func (c *busConn) Receive(buf []byte) (int, netip.Addr, error) {
	select {
	case pk := <-c.ch:
		return copy(buf, pk.p), pk.src, nil
	case <-c.done:
		return 0, netip.Addr{}, errClosed
	}
}

func (c *busConn) Interfaces() int { return 1 }

func (c *busConn) Close() error {
	c.once.Do(func() {
		c.n.b.mu.Lock()
		delete(c.n.b.conns, c)
		c.n.b.mu.Unlock()
		close(c.done)
	})
	return nil
}
