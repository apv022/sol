package e2e

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/transport"
)

// gate puts a device on or off the network while its daemon keeps running: it wraps a
// transport so that, while off, dials fail, incoming connections are closed unanswered,
// and every connection it carried is cut (sessions on both sides end).
type gate struct {
	off   atomic.Bool
	mu    sync.Mutex
	conns map[net.Conn]struct{}
	lns   []net.Listener
}

func newGate() *gate { return &gate{conns: map[net.Conn]struct{}{}} }

// set turns the network on or off.
func (g *gate) set(on bool) {
	g.off.Store(!on)
	if on {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for c := range g.conns {
		c.Close()
	}
	clear(g.conns)
}

func (g *gate) track(c net.Conn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.off.Load() {
		return false
	}
	g.conns[c] = struct{}{}
	return true
}

func (g *gate) untrack(c net.Conn) {
	g.mu.Lock()
	delete(g.conns, c)
	g.mu.Unlock()
}

func (g *gate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, ln := range g.lns {
		ln.Close()
	}
}

// wrap is a sold.Options.WrapTransport.
func (g *gate) wrap(t transport.Transport) transport.Transport { return &gated{Transport: t, g: g} }

type gated struct {
	transport.Transport
	g *gate
}

var errOffline = errors.New("gate: this device is offline")

func (t *gated) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if t.g.off.Load() {
		return nil, errOffline
	}
	c, err := t.Transport.Dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	if !t.g.track(c) {
		c.Close()
		return nil, errOffline
	}
	return &gatedConn{Conn: c, g: t.g}, nil
}

// Publish puts a forwarder in front of p.Target that refuses while the gate is off.
func (t *gated) Publish(ctx context.Context, p transport.Publication) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	t.g.mu.Lock()
	t.g.lns = append(t.g.lns, ln)
	t.g.mu.Unlock()
	target := p.Target
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			if !t.g.track(in) {
				in.Close() // never answer while offline
				continue
			}
			go func() {
				defer t.g.untrack(in)
				defer in.Close()
				out, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer out.Close()
				go func() { _, _ = io.Copy(out, in); out.Close() }()
				_, _ = io.Copy(in, out)
			}()
		}
	}()
	p.Target = ln.Addr().String()
	return t.Transport.Publish(ctx, p)
}

type gatedConn struct {
	net.Conn
	g *gate
}

func (c *gatedConn) Close() error {
	c.g.untrack(c.Conn)
	return c.Conn.Close()
}

// TestGateTakesDeviceOffline: the gate the offline scenarios rely on really cuts a device
// off (no session, no replication) and lets it back.
func TestGateTakesDeviceOffline(t *testing.T) {
	g := newGate()
	alice := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	bob := startGated(t, sold.InitOptions{Name: "Bob", Device: "desktop"}, nil, g)
	pairNodes(t, alice, bob)
	waitSession(t, alice, bob)
	bd, _ := identity.ParseID(bob.DeviceID())
	g.set(false)
	eventually(t, "the session to end", func() bool { s, ok := alice.Sessions.Session(bd); return !ok || !s.Alive() })
	time.Sleep(300 * time.Millisecond) // several redial rounds (RedialMax is 200ms here)
	if s, ok := alice.Sessions.Session(bd); ok && s.Alive() {
		t.Fatal("a session came up with a device that is offline")
	}
	g.set(true)
	bob.NetworkChanged("back")
	waitSession(t, alice, bob)
}
