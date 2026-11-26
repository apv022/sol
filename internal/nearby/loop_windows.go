package nearby

import (
	"net"
	"syscall"
)

// supported: this OS can send and receive beacons.
const supported = true

// setLoopback turns multicast loopback on (Go's ListenMulticastUDP turns it off). On Windows
// the option applies to the receiving socket, so without it this device would not hear other
// Sol devices on the same machine.
func setLoopback(c *net.UDPConn) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_LOOP, 1)
	}); err != nil {
		return err
	}
	return serr
}
