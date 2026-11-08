package transport

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/apv022/sol/internal/netutil"
)

// Clearnet is a plain-TCP transport for LAN use and tests. It provides no
// encryption and no anonymity: traffic is visible to anyone on the path.
// The handshake still authenticates every peer.
//
// Publishing "node" listens on ListenAddr; every other publication listens
// on a random port on the same host.
type Clearnet struct {
	ListenAddr string // e.g. "0.0.0.0:7700" or "127.0.0.1:0" in tests
	Advertise  string // host:port to put in links; "" means auto-detect

	mu    sync.Mutex
	state State
	pubs  map[string]*clearnetPub
}

type clearnetPub struct {
	ln     net.Listener
	addr   string
	target string
}

// NewClearnet constructs the transport.
func NewClearnet(listenAddr, advertise string) *Clearnet {
	return &Clearnet{ListenAddr: listenAddr, Advertise: advertise, state: StateStopped, pubs: map[string]*clearnetPub{}}
}

func (c *Clearnet) Name() string { return "clearnet" }

// Start marks the transport ready; listeners are opened by Publish.
func (c *Clearnet) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateReady
	return nil
}

// Publish opens a listener that forwards every connection to p.Target.
// Re-publishing an ID only updates the target.
func (c *Clearnet) Publish(ctx context.Context, p Publication) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pub, ok := c.pubs[p.ID]; ok {
		pub.target = p.Target
		return pub.addr, nil
	}
	listen := c.ListenAddr
	if p.ID != "node" {
		host, _, err := net.SplitHostPort(c.ListenAddr)
		if err != nil {
			return "", err
		}
		listen = net.JoinHostPort(host, "0")
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return "", fmt.Errorf("clearnet listen %s: %w", listen, err)
	}
	pub := &clearnetPub{ln: ln, target: p.Target, addr: c.advertised(ln.Addr().(*net.TCPAddr), p.ID)}
	c.pubs[p.ID] = pub
	go c.serve(pub)
	return pub.addr, nil
}

func (c *Clearnet) advertised(la *net.TCPAddr, id string) string {
	port := fmt.Sprint(la.Port)
	if c.Advertise != "" && id == "node" {
		return c.Advertise
	}
	host := la.IP.String()
	if la.IP.IsUnspecified() {
		if ip, err := netutil.LANIP(); err == nil {
			host = ip.String()
		} else {
			host = "127.0.0.1"
		}
	}
	return net.JoinHostPort(host, port)
}

func (c *Clearnet) serve(pub *clearnetPub) {
	for {
		in, err := pub.ln.Accept()
		if err != nil {
			return // listener closed
		}
		c.mu.Lock()
		target := pub.target
		c.mu.Unlock()
		go func() {
			out, err := net.Dial("tcp", target)
			if err != nil {
				slog.Debug("clearnet: target unreachable", "target", target, "err", err)
				in.Close()
				return
			}
			netutil.Pipe(in, out)
		}()
	}
}

// Unpublish closes the listener for id.
func (c *Clearnet) Unpublish(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pub, ok := c.pubs[id]; ok {
		pub.ln.Close()
		delete(c.pubs, id)
	}
	return nil
}

// Handles accepts any host:port that is not an onion or I2P address.
func (c *Clearnet) Handles(addr string) bool {
	_, _, err := net.SplitHostPort(addr)
	return err == nil && !IsOnion(addr) && !IsI2P(addr)
}

// Dial opens a direct TCP connection.
func (c *Clearnet) Dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func (c *Clearnet) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Status{Name: c.Name(), State: c.state, Addresses: map[string]string{}}
	for id, p := range c.pubs {
		s.Addresses[id] = p.addr
	}
	if c.state == StateReady {
		s.Detail = "LAN only, unencrypted"
	}
	return s
}

// Stop closes every listener.
func (c *Clearnet) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, p := range c.pubs {
		p.ln.Close()
		delete(c.pubs, id)
	}
	c.state = StateStopped
	return nil
}
