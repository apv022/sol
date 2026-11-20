package daemon

import (
	"net/netip"
	"os"
)

// defaultSameUser (Linux, Android) reads /proc/net/tcp and /proc/net/tcp6. On Android this
// works within the app's own uid; other apps have other uids and see the login screen.
func defaultSameUser(remote, local netip.AddrPort) (bool, error) {
	if !loopbackPair(remote, local) {
		return false, nil
	}
	var firstErr error
	for _, p := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(p)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		uid, ok := parseProcNetUID(f, remote, local)
		f.Close()
		if ok {
			return int(uid) == os.Getuid(), nil
		}
	}
	return false, firstErr
}
