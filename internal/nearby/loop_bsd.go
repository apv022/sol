//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package nearby

import (
	"net"
	"syscall"
)

// supported: this OS can send and receive beacons.
const supported = true

// setLoopback turns multicast loopback on, so a beacon also reaches other Sol devices on this
// machine (Go's ListenMulticastUDP turns it off). The BSDs take a byte, as Go's net does.
func setLoopback(c *net.UDPConn) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = syscall.SetsockoptByte(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_LOOP, 1)
	}); err != nil {
		return err
	}
	return serr
}
