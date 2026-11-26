package transport

import (
	"net"
	"slices"
	"strings"
)

// LAN discovery support (design/PROTOCOL.md §Nearby): which local port a transport listens on,
// so a beacon can advertise it, and how an address learned from a beacon joins the candidates
// of a signed endpoint address.

// LocalPorter is implemented by transports that listen on a port of this machine's own
// addresses (WireGuard: UDP; TLS direct and clearnet: TCP).
type LocalPorter interface {
	// LocalPort is the port the transport listens on now, 0 while it isn't listening.
	LocalPort() int
}

// Unwrapper is implemented by transport wrappers (tests) so LocalPort can see through them.
type Unwrapper interface {
	Unwrap() Transport
}

// LocalPort returns t's listening port, looking through wrappers, or 0 if it has none.
func LocalPort(t Transport) int {
	for t != nil {
		if lp, ok := t.(LocalPorter); ok {
			return lp.LocalPort()
		}
		u, ok := t.(Unwrapper)
		if !ok {
			return 0
		}
		t = u.Unwrap()
	}
	return 0
}

// LocalPort implements LocalPorter.
func (w *WG) LocalPort() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped == nil {
		return 0
	}
	return int(w.port)
}

// LocalPort implements LocalPorter.
func (d *Direct) LocalPort() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ln == nil {
		return 0
	}
	return d.ln.Addr().(*net.TCPAddr).Port
}

// LocalPort implements LocalPorter: the port of the "node" publication.
func (c *Clearnet) LocalPort() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.pubs["node"]; p != nil && p.ln != nil {
		if ta, ok := p.ln.Addr().(*net.TCPAddr); ok {
			return ta.Port
		}
	}
	return 0
}

// AddCandidate puts hostport first among the candidates of a WireGuard or TLS direct address
// ("sol-wg:<key>@a,b", "sol-direct:<id>@a,b"), keeping its key and at most the usual number of
// candidates. ok is false for any other address (clearnet's plain host:port has no list).
// The candidate is unauthenticated: the transport still checks the key it dials.
func AddCandidate(addr, hostport string) (string, bool) {
	if !IsWG(addr) && !IsDirect(addr) {
		return "", false
	}
	head, list, ok := strings.Cut(addr, "@")
	if !ok {
		return "", false
	}
	cands := []string{hostport}
	for _, c := range strings.Split(list, ",") {
		if c != "" && c != hostport && !slices.Contains(cands, c) {
			cands = append(cands, c)
		}
	}
	if len(cands) > directMaxCands {
		cands = cands[:directMaxCands]
	}
	return head + "@" + strings.Join(cands, ","), true
}

// VirtualInterface reports whether an interface is a container or VM bridge on this machine,
// whose addresses no other machine can use.
func VirtualInterface(name string) bool { return skipIface(name) }
