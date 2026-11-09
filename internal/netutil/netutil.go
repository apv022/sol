// Package netutil holds small networking helpers shared by the node, the
// connect client, and transports.
package netutil

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
)

// Pipe copies bytes in both directions until either side closes, then closes
// both. It returns when both copy loops have finished.
func Pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		// Half-close if possible so the peer sees EOF but can still reply;
		// otherwise close fully.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

// FreePorts asks the OS for n distinct free TCP ports on 127.0.0.1.
// There is an inherent race between closing the probe listener and the
// caller binding the port, which is acceptable for local child processes.
func FreePorts(n int) ([]int, error) {
	var ports []int
	var lns []net.Listener
	defer func() {
		for _, l := range lns {
			l.Close()
		}
	}()
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		lns = append(lns, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports, nil
}

// LANIP returns this machine's preferred outbound IPv4 address, used to
// advertise the clearnet transport in pairing links. No packets are sent.
func LANIP() (net.IP, error) {
	c, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1, never routed
	if err != nil {
		return nil, err
	}
	defer c.Close()
	ip := c.LocalAddr().(*net.UDPAddr).IP
	if ip.IsUnspecified() {
		return nil, errors.New("no LAN address")
	}
	return ip, nil
}

// DirectCandidate checks one direct-mode candidate (docs/PROTOCOL.md,
// "Direct mode transport"): an IP:port that could be another machine. No
// names, unspecified, multicast or link-local addresses, and loopback only
// if allowLoopback. Config validation and the dialer share it, so an entry
// the config accepts is one peers will dial.
func DirectCandidate(hostport string, allowLoopback bool) error {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return err
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("bad port %q", port)
	}
	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		return fmt.Errorf("%q is not an IP address (names aren't dialled)", host)
	case ip.IsUnspecified(), ip.IsMulticast(), ip.IsLinkLocalUnicast():
		return fmt.Errorf("%s can't reach another machine", ip)
	case ip.IsLoopback() && !allowLoopback:
		return fmt.Errorf("%s is this machine", ip)
	}
	return nil
}
