package nearby

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"

	"github.com/apv022/sol/internal/transport"
)

// UDP is the real Network: IPv4 multicast to Group:Port on every interface that is up,
// multicast-capable, not loopback, not a container or VM bridge, and has a routable IPv4
// address. Each interface gets a socket of its own (net.ListenMulticastUDP: it joins the group
// there and sends out of it), with multicast loopback on, so two Sol devices on one machine
// see each other. Where interfaces can't be listed (Android 11+ refuses the netlink socket),
// one socket on the system's default interface.
//
// The sockets bind the wildcard address, so a datagram may arrive on several of them; the
// Service drops the copies.
type UDP struct {
	// Interfaces overrides which interfaces are used (tests); nil means every usable one.
	Interfaces func() ([]net.Interface, error)
}

// ifacesForbidden remembers that the OS refused to list interfaces (Android), so it isn't
// asked again on every network change.
var ifacesForbidden atomic.Bool

// usable reports whether beacons can go out of ifc.
func usable(ifc net.Interface) bool {
	if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 || ifc.Flags&net.FlagLoopback != 0 ||
		transport.VirtualInterface(ifc.Name) {
		return false
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip4 := ipn.IP.To4(); ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() {
				return true
			}
		}
	}
	return false
}

// Open implements Network.
func (u UDP) Open() (Conn, error) {
	if !supported {
		return nil, errors.ErrUnsupported
	}
	list := u.Interfaces
	if list == nil {
		list = net.Interfaces
	}
	var ifaces []net.Interface
	fallback := ifacesForbidden.Load()
	if !fallback {
		all, err := list()
		if errors.Is(err, os.ErrPermission) {
			ifacesForbidden.Store(true)
			fallback = true
		} else if err != nil {
			return nil, err
		}
		for _, ifc := range all {
			if usable(ifc) {
				ifaces = append(ifaces, ifc)
			}
		}
	}
	group := &net.UDPAddr{IP: net.ParseIP(Group), Port: Port}
	c := &udpConn{ch: make(chan udpPacket, 64), done: make(chan struct{})}
	open := func(ifc *net.Interface) error {
		s, err := net.ListenMulticastUDP("udp4", ifc, group)
		if err != nil {
			return err
		}
		if err := setLoopback(s); err != nil {
			s.Close()
			return err
		}
		c.socks = append(c.socks, s)
		return nil
	}
	var errs []error
	if fallback {
		if err := open(nil); err != nil {
			return nil, err
		}
	} else {
		for i := range ifaces {
			if err := open(&ifaces[i]); err != nil {
				errs = append(errs, err)
			}
		}
		if len(c.socks) == 0 {
			if len(errs) > 0 {
				return nil, errors.Join(errs...)
			}
			return nil, ErrNoInterface
		}
		c.n = len(c.socks)
	}
	for _, s := range c.socks {
		c.wg.Add(1)
		go c.read(s)
	}
	return c, nil
}

type udpPacket struct {
	p   []byte
	src netip.Addr
}

type udpConn struct {
	socks []*net.UDPConn
	n     int
	ch    chan udpPacket
	done  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup
}

func (c *udpConn) read(s *net.UDPConn) {
	defer c.wg.Done()
	buf := make([]byte, MaxDatagram+1)
	for {
		n, ap, err := s.ReadFromUDPAddrPort(buf)
		if err != nil {
			select {
			case <-c.done:
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return // the socket broke (its interface went away): the next network change reopens
		}
		if n > MaxDatagram {
			continue
		}
		select {
		case c.ch <- udpPacket{append([]byte(nil), buf[:n]...), ap.Addr().Unmap()}:
		default: // the Service is behind: drop, as the kernel would
		}
	}
}

// group is where Send writes.
var groupAddr = netip.AddrPortFrom(netip.MustParseAddr(Group), Port)

func (c *udpConn) Send(p []byte) error {
	var errs []error
	ok := false
	for _, s := range c.socks {
		if _, err := s.WriteToUDPAddrPort(p, groupAddr); err != nil {
			errs = append(errs, err)
		} else {
			ok = true
		}
	}
	if ok {
		return nil
	}
	return errors.Join(errs...)
}

func (c *udpConn) Receive(buf []byte) (int, netip.Addr, error) {
	select {
	case pk := <-c.ch:
		return copy(buf, pk.p), pk.src, nil
	case <-c.done:
		return 0, netip.Addr{}, errClosed
	}
}

func (c *udpConn) Interfaces() int { return c.n }

func (c *udpConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		for _, s := range c.socks {
			s.Close()
		}
	})
	c.wg.Wait()
	return nil
}
