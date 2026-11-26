//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package nearby

import (
	"errors"
	"net"
)

// supported is false: beacons are not built for this OS, and UDP.Open says so
// (errors.ErrUnsupported); everything else in Sol works without them.
const supported = false

func setLoopback(*net.UDPConn) error { return errors.ErrUnsupported }
