package node

// Tunnels: a peer's service kept reachable on 127.0.0.1:<port> by the node
// itself, so the panel's Open button and `sol open` need no terminal left
// running (docs/PEERS.md, "Tunnels").
//
// Invariants:
//   - Tunnels listen on 127.0.0.1 only.
//   - A tunnel is saved in config and reopens on the same port when the node
//     starts, so bookmarks keep working. The listener stays up while the peer
//     is unreachable; every new connection dials the peer fresh (with the
//     usual retries), so reboots and network changes heal on their own.
//   - HTTP tunnels check the Host header (127.0.0.1:<port> or
//     localhost:<port> only). Without that, a web page could use DNS
//     rebinding to read a peer's app through this machine's loopback port.
//   - Unpairing a peer closes and forgets its tunnels. Closing a tunnel also
//     cuts every stream it opened to the peer (an SSH session through it
//     ends), so closing really removes access.
//   - A saved tunnel whose port is busy at start-up keeps retrying the port.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apv022/sol/internal/api"
	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/events"
	"github.com/apv022/sol/internal/netutil"
	"github.com/apv022/sol/internal/peers"
)

// firstTunnelPort is where automatic tunnel ports start: friendly numbers
// that are easy to bookmark.
const firstTunnelPort = 9000

// tunnel is one open (or failed-to-open) tunnel.
type tunnel struct {
	cfg      config.Tunnel
	protocol string

	ln  net.Listener // nil if the port couldn't be opened
	srv *http.Server // HTTP tunnels only

	active atomic.Int64

	mu      sync.Mutex
	lastOK  time.Time
	lastErr string
	via     string
	streams map[net.Conn]struct{} // open streams to the peer
	stopped bool
}

// retryPortEvery is how often a saved tunnel whose port was busy tries again.
var retryPortEvery = 10 * time.Second

func (t *tunnel) record(via string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.lastErr = err.Error()
		return
	}
	t.lastOK, t.lastErr = time.Now().UTC(), ""
	if via != "" {
		t.via = via
	}
}

// stop closes the listener and every stream the tunnel opened to the peer.
func (t *tunnel) stop() {
	t.mu.Lock()
	t.stopped = true
	streams := t.streams
	t.streams = nil
	ln, srv := t.ln, t.srv
	t.mu.Unlock()
	if srv != nil {
		srv.Close()
	} else if ln != nil {
		ln.Close()
	}
	for c := range streams {
		c.Close()
	}
}

// addStream registers c so stop can cut it; false means the tunnel is
// already stopped (the caller closes c).
func (t *tunnel) addStream(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return false
	}
	if t.streams == nil {
		t.streams = map[net.Conn]struct{}{}
	}
	t.streams[c] = struct{}{}
	return true
}

func (t *tunnel) dropStream(c net.Conn) {
	t.mu.Lock()
	delete(t.streams, c)
	t.mu.Unlock()
}

func (t *tunnel) listening() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ln != nil
}

// tunnelStream is a peer stream owned by a tunnel; closing it unregisters it.
type tunnelStream struct {
	net.Conn
	t    *tunnel
	once sync.Once
}

func (c *tunnelStream) Close() error {
	c.once.Do(func() { c.t.dropStream(c) })
	return c.Conn.Close()
}

// CloseWrite keeps half-close working through the wrapper.
func (c *tunnelStream) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

// Tunnels implements api.Backend.
func (n *Node) Tunnels() []api.TunnelView {
	n.tmu.Lock()
	list := make([]*tunnel, 0, len(n.tunnels))
	for _, t := range n.tunnels {
		list = append(list, t)
	}
	n.tmu.Unlock()
	slices.SortFunc(list, func(a, b *tunnel) int { return a.cfg.Port - b.cfg.Port })
	out := []api.TunnelView{}
	for _, t := range list {
		out = append(out, n.tunnelView(t))
	}
	return out
}

func (n *Node) tunnelView(t *tunnel) api.TunnelView {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(t.cfg.Port))
	v := api.TunnelView{
		Port: t.cfg.Port, Peer: t.cfg.Peer, Service: t.cfg.Service, Protocol: t.protocol,
		Addr: addr, Listening: t.listening(), Active: int(t.active.Load()),
	}
	if p, ok := n.peers.Get(t.cfg.Peer); ok {
		v.PeerName = p.Name
	}
	if t.protocol == config.ProtocolHTTP {
		v.URL = "http://" + addr + "/"
	}
	t.mu.Lock()
	v.LastOK, v.LastError, v.Via = t.lastOK, t.lastErr, t.via
	t.mu.Unlock()
	return v
}

// OpenTunnel implements api.Backend: it makes peer id's service reachable on
// 127.0.0.1:port (port 0 picks a free one from 9000 up) and saves the
// tunnel. Opening a tunnel that already exists returns it (asking for a
// different port is an error: close it first).
func (n *Node) OpenTunnel(id, service string, port int) (api.TunnelView, error) {
	if n.ctx == nil {
		return api.TunnelView{}, errors.New("the node isn't running yet")
	}
	p, ok := n.peers.Get(id)
	if !ok {
		return api.TunnelView{}, fmt.Errorf("peer %q: %w", id, config.ErrNotFound)
	}
	if service == peers.Inbox {
		return api.TunnelView{}, errors.New("the inbox is for sending files; use Send a file")
	}
	i := slices.IndexFunc(p.Services, func(s config.RemoteService) bool { return s.Name == service })
	if i < 0 {
		return api.TunnelView{}, fmt.Errorf("%s doesn't share %q with you (as of the last check; try Check again)", p.Name, service)
	}
	protocol := p.Services[i].Protocol

	n.tmu.Lock()
	defer n.tmu.Unlock()
	for _, t := range n.tunnels {
		if t.cfg.Peer == id && t.cfg.Service == service {
			if port != 0 && port != t.cfg.Port {
				return api.TunnelView{}, fmt.Errorf("%s is already open on port %d; close it first to move it", service, t.cfg.Port)
			}
			if !t.listening() {
				n.tryListen(t) // the port may be free by now
			}
			return n.tunnelView(t), nil
		}
	}
	if port < 0 || port > 65535 {
		return api.TunnelView{}, fmt.Errorf("port %d out of range", port)
	}
	ln, err := n.listenTunnel(port)
	if err != nil {
		return api.TunnelView{}, err
	}
	cfg := config.Tunnel{Peer: id, Service: service, Port: ln.Addr().(*net.TCPAddr).Port}
	if err := n.cfg.AddTunnel(cfg); err != nil {
		ln.Close()
		return api.TunnelView{}, err
	}
	t := n.serveTunnel(cfg, protocol, ln)
	n.tunnels[cfg.Port] = t
	n.log.Add(events.KindService, fmt.Sprintf("tunnel open: %s's %s on 127.0.0.1:%d", p.Name, service, cfg.Port))
	return n.tunnelView(t), nil
}

// listenTunnel opens 127.0.0.1:port, or the first free port from
// firstTunnelPort when port is 0. Called with tmu held.
func (n *Node) listenTunnel(port int) (net.Listener, error) {
	if port != 0 {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return nil, fmt.Errorf("port %d is taken; pick another or leave it empty", port)
		}
		return ln, nil
	}
	used := map[int]bool{}
	for _, t := range n.cfg.TunnelList() {
		used[t.Port] = true
	}
	for p := firstTunnelPort; p < firstTunnelPort+1000; p++ {
		if used[p] {
			continue
		}
		if ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p))); err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// CloseTunnel implements api.Backend: it stops the tunnel on port and
// forgets it.
func (n *Node) CloseTunnel(port int) error {
	n.tmu.Lock()
	t := n.tunnels[port]
	delete(n.tunnels, port)
	n.tmu.Unlock()
	if t == nil {
		return fmt.Errorf("tunnel on port %d: %w", port, config.ErrNotFound)
	}
	t.stop()
	if err := n.cfg.RemoveTunnel(t.cfg.Peer, t.cfg.Service); err != nil && !errors.Is(err, config.ErrNotFound) {
		return err
	}
	n.log.Add(events.KindService, fmt.Sprintf("tunnel closed: %s on 127.0.0.1:%d", t.cfg.Service, port))
	return nil
}

// startSavedTunnels reopens the tunnels saved in config (called from Run
// before the panel and control socket serve, so OpenTunnel always sees
// them). A port that can't be opened yet is retried in the background.
func (n *Node) startSavedTunnels() {
	n.tmu.Lock()
	defer n.tmu.Unlock()
	for _, cfg := range n.cfg.TunnelList() {
		protocol := config.ProtocolTCP
		if p, ok := n.peers.Get(cfg.Peer); ok {
			for _, s := range p.Services {
				if s.Name == cfg.Service {
					protocol = s.Protocol
				}
			}
		}
		t := &tunnel{cfg: cfg, protocol: protocol}
		n.tunnels[cfg.Port] = t
		if !n.tryListen(t) {
			n.log.Add(events.KindService, fmt.Sprintf("tunnel %s: port %d is taken; retrying", cfg.Service, cfg.Port))
			go n.retryTunnel(t)
		}
	}
}

// tryListen opens t's saved port and starts serving it. Called with tmu held.
func (n *Node) tryListen(t *tunnel) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(t.cfg.Port)))
	if err != nil {
		t.record("", fmt.Errorf("port %d is taken by another program", t.cfg.Port))
		return false
	}
	n.serve(t, ln)
	t.mu.Lock()
	t.lastErr = ""
	t.mu.Unlock()
	return true
}

// retryTunnel keeps trying t's port until it opens, the tunnel is closed, or
// the node stops.
func (n *Node) retryTunnel(t *tunnel) {
	tick := time.NewTicker(retryPortEvery)
	defer tick.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-tick.C:
		}
		n.tmu.Lock()
		current := n.tunnels[t.cfg.Port] == t
		ok := current && (t.listening() || n.tryListen(t))
		n.tmu.Unlock()
		if !current || ok {
			return
		}
	}
}

// stopTunnels stops (without forgetting) every tunnel matching match.
func (n *Node) stopTunnels(match func(config.Tunnel) bool) {
	n.tmu.Lock()
	defer n.tmu.Unlock()
	for port, t := range n.tunnels {
		if match(t.cfg) {
			t.stop()
			delete(n.tunnels, port)
		}
	}
}

// serveTunnel starts serving ln for cfg.
func (n *Node) serveTunnel(cfg config.Tunnel, protocol string, ln net.Listener) *tunnel {
	t := &tunnel{cfg: cfg, protocol: protocol}
	n.serve(t, ln)
	return t
}

// serve starts serving ln for t.
func (n *Node) serve(t *tunnel, ln net.Listener) {
	t.mu.Lock()
	t.ln = ln
	if t.protocol == config.ProtocolHTTP {
		t.srv = &http.Server{Handler: n.httpTunnel(t), ReadHeaderTimeout: 30 * time.Second}
	}
	srv := t.srv
	t.mu.Unlock()
	if srv != nil {
		go srv.Serve(ln)
	} else {
		go n.tcpTunnel(t, ln)
	}
}

// dialTunnel opens a stream to the tunnel's peer service and records how it went.
// A dial cancelled by the client (a browser leaving the page) says nothing
// about the peer, so it isn't recorded as an error.
func (n *Node) dialTunnel(ctx context.Context, t *tunnel) (net.Conn, error) {
	c, err := n.DialPeer(ctx, t.cfg.Peer, t.cfg.Service)
	if err != nil {
		if ctx.Err() == nil {
			t.record("", err)
		}
		return nil, err
	}
	via := ""
	if tc, ok := c.(*trackedConn); ok {
		via = tc.via
	}
	t.record(via, nil)
	sc := &tunnelStream{Conn: c, t: t}
	if !t.addStream(sc) {
		c.Close()
		return nil, errors.New("tunnel closed")
	}
	return sc, nil
}

func (n *Node) tcpTunnel(t *tunnel, ln net.Listener) {
	for {
		local, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Temporary (e.g. out of file descriptors): back off, keep serving.
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func() {
			t.active.Add(1)
			defer t.active.Add(-1)
			remote, err := n.dialTunnel(n.ctx, t)
			if err != nil {
				local.Close()
				return
			}
			netutil.Pipe(local, remote)
		}()
	}
}

// httpTunnel proxies HTTP to the peer's service, one peer stream per
// upstream connection (kept alive and reused, like a browser would).
func (n *Node) httpTunnel(t *tunnel) http.Handler {
	port := strconv.Itoa(t.cfg.Port)
	allowed := []string{"127.0.0.1:" + port, "localhost:" + port}
	rp := &httputil.ReverseProxy{
		// Rewrite (not Director) adds no X-Forwarded-* headers, so the peer's
		// app learns nothing about this machine beyond what a browser sends.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = pr.In.Host
			pr.Out.Host = pr.In.Host
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return n.dialTunnel(ctx, t)
			},
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, "Sol couldn't reach this service on the peer right now.\n\n"+explain(err)+
				"\n\nThe tunnel stays open: reload when the peer is back online.\n")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(allowed, r.Host) {
			http.Error(w, "Sol tunnel: wrong Host header (open it as http://127.0.0.1:"+port+"/)", http.StatusForbidden)
			return
		}
		t.active.Add(1)
		defer t.active.Add(-1)
		rp.ServeHTTP(w, r)
	})
}

// SetRetryPortEvery shortens the busy-port retry interval for a test.
func SetRetryPortEvery(t interface{ Cleanup(func()) }, d time.Duration) {
	old := retryPortEvery
	retryPortEvery = d
	t.Cleanup(func() { retryPortEvery = old })
}
