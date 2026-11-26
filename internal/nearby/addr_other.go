//go:build !linux

package nearby

import (
	"errors"
	"net"
	"net/netip"
)

// openAddr is Linux (and Android) only: elsewhere interfaces can be listed, so an explicit
// address (the Android shell's Wi-Fi Direct interface) is never needed.
func openAddr(netip.Addr) (*net.UDPConn, error) { return nil, errors.ErrUnsupported }
