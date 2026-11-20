// Package faultnet is a fault-injection toolkit for tests: it stands between
// two endpoints on loopback and misbehaves on command, so the node's
// recovery paths can be tested deterministically without root, network
// namespaces or a real bad network.
//
//	TCPProxy   forwards a TCP port and can add latency, throttle, blackhole
//	           (hold every byte, as a dead path does), reset or refuse.
//	SOCKS5     the same shaping behind a SOCKS5 server; real tor can be
//	           pointed at it ("Socks5Proxy") to see a slow network for real.
//	UDPProxy   forwards UDP datagrams (WireGuard) and can drop or delay them.
//	NAT        a home router for one host's UDP socket: only answers to what the host
//	           sent get in (hole punching needed), and Block cuts UDP altogether (nat.go).
//	FakeTor    a scripted stand-in for the tor program: it stalls at a chosen
//	           bootstrap percentage, exits, drops or freezes its control
//	           port, or never publishes a descriptor (faketor.go).
//
// Everything is standard library only and safe for concurrent use: the
// knobs may be turned while traffic flows, and take effect on connections
// that are already open.
//
// Invariant: nothing here is used by the node itself. Only tests (and the
// test binary re-executed as the fake tor) import this package.
package faultnet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// shape is the set of faults a proxy applies. The zero value forwards
// everything untouched.
type shape struct {
	mu        sync.Mutex
	latency   time.Duration // added to every chunk, each way
	rate      int           // bytes per second each way; 0 means unlimited
	blackhole bool          // hold every byte (nothing is lost; it flows again on heal)
	changed   chan struct{} // closed and replaced whenever a knob turns
}

func (s *shape) get() (latency time.Duration, rate int, blackhole bool, changed <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	return s.latency, s.rate, s.blackhole, s.changed
}

func (s *shape) set(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

// SetLatency delays every chunk of data by d in each direction.
func (s *shape) SetLatency(d time.Duration) { s.set(func() { s.latency = d }) }

// SetRate throttles each direction of every connection to bytesPerSecond
// (0: unlimited).
func (s *shape) SetRate(bytesPerSecond int) { s.set(func() { s.rate = bytesPerSecond }) }

// Blackhole holds every byte while on, in both directions: connections stay
// open and nothing arrives, which is what a dead network path looks like to
// both ends. Turning it off lets the held bytes through.
func (s *shape) Blackhole(on bool) { s.set(func() { s.blackhole = on }) }

type chunk struct {
	at   time.Time
	data []byte
}

// relay copies src to dst through the shape until either side ends or done
// is closed.
func (s *shape) relay(dst, src net.Conn, done <-chan struct{}) {
	queue := make(chan chunk, 256)
	go func() {
		defer close(queue)
		buf := make([]byte, 16<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				latency, rate, _, _ := s.get()
				if rate > 0 {
					// Pace the reader: this is what pushes back on the sender.
					wait := time.Duration(float64(n) / float64(rate) * float64(time.Second))
					select {
					case <-time.After(wait):
					case <-done:
						return
					}
				}
				c := chunk{at: time.Now().Add(latency), data: append([]byte(nil), buf[:n]...)}
				select {
				case queue <- c:
				case <-done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for c := range queue {
		if d := time.Until(c.at); d > 0 {
			select {
			case <-time.After(d):
			case <-done:
				return
			}
		}
		for {
			_, _, hole, changed := s.get()
			if !hole {
				break
			}
			select {
			case <-changed:
			case <-done:
				return
			}
		}
		if _, err := dst.Write(c.data); err != nil {
			return
		}
	}
	// The source ended cleanly: pass the half-close on.
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// link is one proxied connection.
type link struct {
	a, b net.Conn
	done chan struct{}
	once sync.Once
}

func (l *link) close(reset bool) {
	l.once.Do(func() {
		close(l.done)
		for _, c := range []net.Conn{l.a, l.b} {
			if tc, ok := c.(*net.TCPConn); ok && reset {
				tc.SetLinger(0) // close with RST: "connection reset by peer"
			}
			c.Close()
		}
	})
}

// links tracks the open connections of a proxy.
type links struct {
	shape
	mu    sync.Mutex
	open  map[*link]struct{}
	total atomic.Int64
}

func (ls *links) run(a, b net.Conn) {
	l := &link{a: a, b: b, done: make(chan struct{})}
	ls.mu.Lock()
	if ls.open == nil {
		ls.open = map[*link]struct{}{}
	}
	ls.open[l] = struct{}{}
	ls.mu.Unlock()
	ls.total.Add(1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); ls.relay(b, a, l.done) }()
	go func() { defer wg.Done(); ls.relay(a, b, l.done) }()
	wg.Wait()
	l.close(false)
	ls.mu.Lock()
	delete(ls.open, l)
	ls.mu.Unlock()
}

// Open returns how many connections are being forwarded right now.
func (ls *links) Open() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return len(ls.open)
}

// Total returns how many connections have been forwarded so far.
func (ls *links) Total() int { return int(ls.total.Load()) }

// ResetAll cuts every open connection with a TCP reset. New connections are
// still accepted.
func (ls *links) ResetAll() {
	ls.mu.Lock()
	var all []*link
	for l := range ls.open {
		all = append(all, l)
	}
	ls.mu.Unlock()
	for _, l := range all {
		l.close(true)
	}
}

// TCPProxy forwards one loopback TCP port to an upstream address, applying
// whatever faults are switched on.
type TCPProxy struct {
	links
	addr string

	mu       sync.Mutex
	ln       net.Listener
	upstream string
	refuse   bool
	closed   bool
}

// NewTCPProxy listens on a free loopback port and forwards to upstream.
func NewTCPProxy(upstream string) (*TCPProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &TCPProxy{addr: ln.Addr().String(), ln: ln, upstream: upstream}
	go p.serve(ln)
	return p, nil
}

// Addr is the address to give to whoever should go through the proxy.
func (p *TCPProxy) Addr() string { return p.addr }

// SetUpstream changes where new connections are forwarded to.
func (p *TCPProxy) SetUpstream(addr string) {
	p.mu.Lock()
	p.upstream = addr
	p.mu.Unlock()
}

// Refuse closes the listening port while on, so new connections are refused
// (as when the far machine is up but the program is not). Open connections
// are left alone; use ResetAll to cut them too.
func (p *TCPProxy) Refuse(on bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || on == p.refuse {
		return nil
	}
	p.refuse = on
	if on {
		p.ln.Close()
		return nil
	}
	var ln net.Listener
	var err error
	for range 50 { // the port may take a moment to be free again
		if ln, err = net.Listen("tcp", p.addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	p.ln = ln
	go p.serve(ln)
	return nil
}

func (p *TCPProxy) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			p.mu.Lock()
			up := p.upstream
			p.mu.Unlock()
			out, err := net.DialTimeout("tcp", up, 5*time.Second)
			if err != nil {
				if tc, ok := c.(*net.TCPConn); ok {
					tc.SetLinger(0)
				}
				c.Close()
				return
			}
			p.run(c, out)
		}()
	}
}

// Close stops the proxy and cuts every connection.
func (p *TCPProxy) Close() {
	p.mu.Lock()
	p.closed = true
	p.ln.Close()
	p.mu.Unlock()
	p.ResetAll()
}

// SOCKS5 is a SOCKS5 server (no authentication, CONNECT only) whose
// connections go through the same faults as a TCPProxy. Real tor can use it
// as its way out ("Socks5Proxy 127.0.0.1:port" in torrc), which shows how
// the node behaves on a slow or broken network without touching the
// machine's real network.
type SOCKS5 struct {
	links
	ln net.Listener
}

// NewSOCKS5 listens on a free loopback port.
func NewSOCKS5() (*SOCKS5, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &SOCKS5{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	return s, nil
}

// Addr is the server's address.
func (s *SOCKS5) Addr() string { return s.ln.Addr().String() }

// Close stops the server and cuts every connection.
func (s *SOCKS5) Close() {
	s.ln.Close()
	s.ResetAll()
}

func (s *SOCKS5) handle(c net.Conn) {
	target, err := socksRequest(c)
	if err != nil {
		c.Close()
		return
	}
	// A blackholed network doesn't answer connection attempts either.
	for {
		_, _, hole, changed := s.get()
		if !hole {
			break
		}
		<-changed
	}
	out, err := net.DialTimeout("tcp", target, 20*time.Second)
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0}) // connection refused
		c.Close()
		return
	}
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		c.Close()
		out.Close()
		return
	}
	s.run(c, out)
}

// socksRequest reads a SOCKS5 greeting and CONNECT request and returns the
// target host:port.
func socksRequest(c net.Conn) (string, error) {
	c.SetDeadline(time.Now().Add(30 * time.Second))
	defer c.SetDeadline(time.Time{})
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return "", err
	}
	if hdr[0] != 5 {
		return "", errors.New("not SOCKS5")
	}
	if _, err := io.ReadFull(c, make([]byte, hdr[1])); err != nil {
		return "", err
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return "", err
	}
	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return "", err
	}
	if req[1] != 1 {
		return "", errors.New("only CONNECT is supported")
	}
	var host string
	switch req[3] {
	case 1, 4:
		ip := make([]byte, map[byte]int{1: 4, 4: 16}[req[3]])
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	case 3:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return "", err
		}
		name := make([]byte, l[0])
		if _, err := io.ReadFull(c, name); err != nil {
			return "", err
		}
		host = string(name)
	default:
		return "", fmt.Errorf("bad address type %d", req[3])
	}
	var port [2]byte
	if _, err := io.ReadFull(c, port[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), nil
}

// UDPProxy forwards datagrams between whoever sends to it and one upstream
// address (a WireGuard port). Each sender gets its own upstream socket, so
// replies find their way back; the upstream sees the proxy, never the
// sender.
type UDPProxy struct {
	pc       net.PacketConn
	upstream *net.UDPAddr

	mu      sync.Mutex
	drop    bool
	latency time.Duration
	sess    map[string]*net.UDPConn
	closed  bool

	forwarded atomic.Int64
	dropped   atomic.Int64
}

// NewUDPProxy listens on a free loopback UDP port and forwards to upstream.
func NewUDPProxy(upstream string) (*UDPProxy, error) {
	ua, err := net.ResolveUDPAddr("udp", upstream)
	if err != nil {
		return nil, err
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &UDPProxy{pc: pc, upstream: ua, sess: map[string]*net.UDPConn{}}
	go p.serve()
	return p, nil
}

// Addr is the address to advertise in place of the upstream's.
func (p *UDPProxy) Addr() string { return p.pc.LocalAddr().String() }

// Drop discards every datagram while on, in both directions.
func (p *UDPProxy) Drop(on bool) {
	p.mu.Lock()
	p.drop = on
	p.mu.Unlock()
}

// SetLatency delays every datagram by d.
func (p *UDPProxy) SetLatency(d time.Duration) {
	p.mu.Lock()
	p.latency = d
	p.mu.Unlock()
}

// Forwarded and Dropped count datagrams (both directions).
func (p *UDPProxy) Forwarded() int { return int(p.forwarded.Load()) }

// Dropped counts the datagrams discarded while Drop was on.
func (p *UDPProxy) Dropped() int { return int(p.dropped.Load()) }

// send delivers one datagram according to the current faults.
func (p *UDPProxy) send(data []byte, write func([]byte)) {
	p.mu.Lock()
	drop, latency := p.drop, p.latency
	p.mu.Unlock()
	if drop {
		p.dropped.Add(1)
		return
	}
	p.forwarded.Add(1)
	if latency <= 0 {
		write(data)
		return
	}
	cp := append([]byte(nil), data...)
	time.AfterFunc(latency, func() { write(cp) })
}

func (p *UDPProxy) serve() {
	buf := make([]byte, 64<<10)
	for {
		n, from, err := p.pc.ReadFrom(buf)
		if err != nil {
			return
		}
		p.mu.Lock()
		up := p.sess[from.String()]
		if up == nil && !p.closed {
			if up, err = net.DialUDP("udp", nil, p.upstream); err == nil {
				p.sess[from.String()] = up
				go p.back(up, from)
			}
		}
		p.mu.Unlock()
		if up != nil {
			p.send(buf[:n], func(b []byte) { up.Write(b) })
		}
	}
}

// back carries the upstream's replies to the sender they belong to.
func (p *UDPProxy) back(up *net.UDPConn, to net.Addr) {
	buf := make([]byte, 64<<10)
	for {
		n, err := up.Read(buf)
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			// "Connection refused" while the upstream is down: not fatal.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		p.send(buf[:n], func(b []byte) { p.pc.WriteTo(b, to) })
	}
}

// Close stops the proxy.
func (p *UDPProxy) Close() {
	p.mu.Lock()
	p.closed = true
	for _, c := range p.sess {
		c.Close()
	}
	p.mu.Unlock()
	p.pc.Close()
}
