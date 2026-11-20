//go:build !linux && !windows

package daemon

import "net/netip"

// defaultSameUser is not supported on macOS and the rest yet: the browser uses a login code.
func defaultSameUser(remote, local netip.AddrPort) (bool, error) {
	return false, ErrSameUserUnsupported
}
