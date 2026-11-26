package nearby

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"syscall"
)

// openAddr opens a beacon socket for the interface that has the IPv4 address a, without
// listing interfaces (Android 11+ refuses that to apps, so the shell passes the address of a
// new Wi-Fi Direct interface): it binds the group's port on the wildcard address with
// SO_REUSEADDR like the other sockets, joins the group on a, and sends out of a.
func openAddr(a netip.Addr) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		if err := rc.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:"+strconv.Itoa(Port))
	if err != nil {
		return nil, err
	}
	c := pc.(*net.UDPConn)
	rc, err := c.SyscallConn()
	if err != nil {
		c.Close()
		return nil, err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		mreq := &syscall.IPMreq{Multiaddr: groupAddr.Addr().As4(), Interface: a.As4()}
		if serr = syscall.SetsockoptIPMreq(int(fd), syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, mreq); serr != nil {
			return
		}
		if serr = syscall.SetsockoptInet4Addr(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF, a.As4()); serr != nil {
			return
		}
		if serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_TTL, 1); serr != nil {
			return
		}
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_LOOP, 1)
	}); err != nil {
		c.Close()
		return nil, err
	}
	if serr != nil {
		c.Close()
		return nil, serr
	}
	return c, nil
}
