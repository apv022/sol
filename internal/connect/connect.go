// Package connect is the client side of Sol: pairing with a node and
// running local tunnels to its services (`sol pair`, `sol connect`).
//
// State lives in a client directory (default ~/.sol-client):
//
//	device.key   this device's ed25519 key (0600)
//	nodes.json   paired nodes: addresses, Tor client-auth key, services
//	tor/         the client's own tor data directory
//
// A Session-style API (Client.Dial) is exposed so other code, such as the
// v0.2 gateway, can open authenticated streams without the CLI.
package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/netutil"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/protocol"
	"github.com/apv022/sol/internal/runtime"
	"github.com/apv022/sol/internal/transport"
)

// NodeRecord is what the client remembers about one paired node.
type NodeRecord struct {
	NodeID      string                 `json:"node_id"`
	DeviceID    string                 `json:"device_id"`
	Onion       string                 `json:"onion,omitempty"`
	I2P         string                 `json:"i2p,omitempty"`
	LAN         string                 `json:"lan,omitempty"`
	TorAuthPriv string                 `json:"tor_auth_priv,omitempty"`
	Services    []protocol.ServiceInfo `json:"services"`
	Paired      time.Time              `json:"paired"`
}

// Addresses returns the node's addresses in preference order: anonymous
// transports first, LAN last.
func (r NodeRecord) Addresses() []string {
	var out []string
	for _, a := range []string{r.Onion, r.I2P, r.LAN} {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

// TransportFactory builds the transports a client needs to reach rec.
type TransportFactory func(dir string, rec NodeRecord) []transport.Transport

// DefaultTransports starts Tor if the node has an onion and clearnet if it
// has a LAN address.
func DefaultTransports(dir string, rec NodeRecord) []transport.Transport {
	var ts []transport.Transport
	if rec.Onion != "" {
		prov, err := runtime.Select([]string{"bundled", "native"}, "tor", runtime.Options{BinDir: filepath.Join(dir, "bin")})
		if err != nil {
			ts = append(ts, transport.NewUnavailable("tor", err))
		} else {
			ts = append(ts, transport.NewTor(filepath.Join(dir, "tor"), prov))
		}
	}
	if rec.LAN != "" {
		ts = append(ts, transport.NewClearnet("127.0.0.1:0", ""))
	}
	return ts
}

// Options configures a Client.
type Options struct {
	// Transports builds the transports; nil means DefaultTransports.
	Transports TransportFactory
	// Progress receives human-readable status lines (Tor bootstrap, retries).
	// nil discards them.
	Progress func(msg string)
}

// Retry policy for reaching a node whose onion may still be publishing.
var (
	dialAttempts   = 6
	dialRetryDelay = 20 * time.Second
)

// Client is a connection to one paired node.
type Client struct {
	Dir    string
	Device *identity.Identity
	Node   NodeRecord

	opts    Options
	reg     *transport.Registry
	started bool

	mu       sync.Mutex
	lastGood string // address that last worked
}

// DefaultDir returns ~/.sol-client.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".sol-client"), nil
}

func loadNodes(dir string) (map[string]NodeRecord, error) {
	data, err := os.ReadFile(filepath.Join(dir, "nodes.json"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]NodeRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	nodes := map[string]NodeRecord{}
	if err := json.Unmarshal(data, &nodes); err != nil {
		return nil, fmt.Errorf("parsing nodes.json: %w", err)
	}
	return nodes, nil
}

func saveNode(dir string, rec NodeRecord) error {
	nodes, err := loadNodes(dir)
	if err != nil {
		return err
	}
	nodes[rec.NodeID] = rec
	data, err := json.MarshalIndent(nodes, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(dir, "nodes.json"), append(data, '\n'), 0o600)
}

// Nodes lists the paired nodes, sorted by pairing time.
func Nodes(dir string) ([]NodeRecord, error) {
	m, err := loadNodes(dir)
	if err != nil {
		return nil, err
	}
	var out []NodeRecord
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Paired.Before(out[j].Paired) })
	return out, nil
}

func newClient(dir string, rec NodeRecord, opts Options) (*Client, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dev, err := identity.LoadOrCreate(filepath.Join(dir, "device.key"))
	if err != nil {
		return nil, err
	}
	if opts.Transports == nil {
		opts.Transports = DefaultTransports
	}
	return &Client{Dir: dir, Device: dev, Node: rec, opts: opts}, nil
}

// Pair redeems a pairing link, saves the node, and returns a started Client.
func Pair(ctx context.Context, dir, link, deviceName string, opts Options) (*Client, error) {
	l, err := pairing.ParseLink(link)
	if err != nil {
		return nil, err
	}
	rec := NodeRecord{NodeID: l.NodeID, Onion: l.Onion, I2P: l.I2P, LAN: l.LAN, TorAuthPriv: l.TorAuthPriv}
	c, err := newClient(dir, rec, opts)
	if err != nil {
		return nil, err
	}
	if err := c.Start(ctx); err != nil {
		return nil, err
	}
	if err := c.WaitReady(ctx); err != nil {
		c.Close()
		return nil, err
	}
	conn, err := c.dialRetry(ctx, protocol.Request{Op: protocol.OpPair, Secret: l.Secret, Name: deviceName})
	if err != nil {
		c.Close()
		if errors.Is(err, protocol.ErrRejected) {
			return nil, errors.New("the node refused the pairing link: it has expired (10 minutes), was already used, or belongs to another node. Create a new one in the panel")
		}
		return nil, err
	}
	defer conn.Close()
	sl, err := protocol.ReadServiceList(conn)
	if err != nil {
		c.Close()
		return nil, err
	}
	c.Node.DeviceID = sl.DeviceID
	c.Node.Services = sl.Services
	c.Node.Paired = time.Now().UTC()
	if err := saveNode(dir, c.Node); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Load returns a started Client for a paired node. nodeID may be "" if
// exactly one node is paired.
func Load(ctx context.Context, dir, nodeID string, opts Options) (*Client, error) {
	nodes, err := Nodes(dir)
	if err != nil {
		return nil, err
	}
	var rec *NodeRecord
	for i := range nodes {
		if nodeID == "" || nodes[i].NodeID == nodeID {
			if rec != nil {
				return nil, errors.New("several nodes are paired; choose one with --node")
			}
			rec = &nodes[i]
		}
	}
	if rec == nil {
		return nil, errors.New("not paired with that node; run `sol pair <link>` first")
	}
	c, err := newClient(dir, *rec, opts)
	if err != nil {
		return nil, err
	}
	return c, c.Start(ctx)
}

// Start launches the transports needed to reach the node and installs the
// Tor client-auth key.
func (c *Client) Start(ctx context.Context) error {
	if c.started {
		return nil
	}
	c.reg = transport.NewRegistry()
	for _, t := range c.opts.Transports(c.Dir, c.Node) {
		c.reg.Add(t)
	}
	if err := c.reg.StartAll(ctx); err != nil {
		slog.Warn("some transports failed to start", "err", err)
	}
	c.started = true
	if c.Node.Onion != "" && c.Node.TorAuthPriv != "" {
		if t, ok := c.reg.Get("tor"); ok {
			if ca, ok := t.(transport.ClientAuthorizer); ok {
				if err := ca.AuthorizeClient(c.Node.Onion, c.Node.TorAuthPriv); err != nil {
					slog.Warn("installing Tor client auth", "err", err)
				}
			}
		}
	}
	return nil
}

func (c *Client) say(format string, args ...any) {
	if c.opts.Progress != nil {
		c.opts.Progress(fmt.Sprintf(format, args...))
	}
}

// stallHint is shown when a transport makes no progress for this long.
const stallHint = 90 * time.Second

// WaitReady blocks until at least one transport that can reach the node is
// ready, reporting progress through Options.Progress. It fails if every such
// transport has failed.
func (c *Client) WaitReady(ctx context.Context) error {
	last, lastChange, hinted := "", time.Now(), false
	for {
		var failed []string
		relevant := 0
		for _, addr := range c.Node.Addresses() {
			t, err := c.reg.For(addr)
			if err != nil {
				continue
			}
			relevant++
			st := t.Status()
			switch st.State {
			case transport.StateReady:
				if last != "" {
					c.say("%s: ready", st.Name)
				}
				return nil
			case transport.StateFailed:
				failed = append(failed, st.Name+": "+st.Detail)
			default:
				if line := st.Name + ": " + st.Detail; line != last {
					c.say("%s", line)
					last, lastChange, hinted = line, time.Now(), false
				}
			}
		}
		if relevant == 0 || len(failed) == relevant {
			if len(failed) == 0 {
				return errors.New("no transport on this machine can reach that node (does the link have a tor= address, and is tor installed?)")
			}
			return fmt.Errorf("cannot reach the node: %s", strings.Join(failed, "; "))
		}
		if !hinted && time.Since(lastChange) > stallHint {
			c.say("still waiting. If this never finishes: check this machine's internet connection and that its clock is correct (Tor refuses to work with a wrong clock)%s", c.torLogHint())
			hinted = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (c *Client) torLogHint() string {
	if t, ok := c.reg.Get("tor"); ok {
		if lp, ok := t.(interface{ LogPath() string }); ok {
			return ". Tor log: " + lp.LogPath()
		}
	}
	return ""
}

// dialRetry is dialAny with retries for errors that usually clear up on
// their own, like an onion that was published a moment ago.
func (c *Client) dialRetry(ctx context.Context, req protocol.Request) (net.Conn, error) {
	var err error
	for attempt := 1; attempt <= dialAttempts; attempt++ {
		var conn net.Conn
		conn, err = c.dialAny(ctx, req)
		if err == nil {
			return conn, nil
		}
		if errors.Is(err, protocol.ErrRejected) || errors.Is(err, protocol.ErrWrongNode) || ctx.Err() != nil {
			return nil, err
		}
		if attempt < dialAttempts {
			c.say("can't reach the node yet (%s). Retrying in %s (attempt %d of %d)...", explain(err), dialRetryDelay, attempt, dialAttempts)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(dialRetryDelay):
			}
		}
	}
	return nil, fmt.Errorf("could not reach the node: %s.\nCheck that the node is running (`sol`) and online, that both machines' clocks are correct, and try a fresh pairing link%s", explain(err), c.torLogHint())
}

// explain turns a dial error into a short plain-English reason.
func explain(err error) string {
	msg := err.Error()
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout(), strings.Contains(msg, "i/o timeout"):
		return "Tor timed out finding the node"
	case strings.Contains(msg, "descriptor not found"), strings.Contains(msg, "host unreachable"):
		return "the node's .onion isn't visible yet; this is normal for a minute or two after the node starts or a link is created"
	case strings.Contains(msg, "client authorization"):
		return "Tor rejected this device's access key; create a new pairing link"
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	}
	return msg
}

// Close stops the client's transports.
func (c *Client) Close() error {
	if c.reg == nil {
		return nil
	}
	return c.reg.StopAll()
}

// dialAny opens a stream to the node and completes a handshake for req,
// trying the last working address first, then each address in order.
func (c *Client) dialAny(ctx context.Context, req protocol.Request) (net.Conn, error) {
	addrs := c.Node.Addresses()
	c.mu.Lock()
	if c.lastGood != "" {
		addrs = append([]string{c.lastGood}, addrs...)
	}
	c.mu.Unlock()
	// Prefer addresses whose transport is ready right now (e.g. LAN while
	// tor is still bootstrapping); keep the order otherwise.
	sort.SliceStable(addrs, func(i, j int) bool { return c.ready(addrs[i]) && !c.ready(addrs[j]) })
	var errs []error
	tried := map[string]bool{}
	for _, addr := range addrs {
		if tried[addr] {
			continue
		}
		tried[addr] = true
		t, err := c.reg.For(addr)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		conn, err := t.Dial(dctx, addr)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
			continue
		}
		if err := protocol.ClientHandshake(conn, c.Device, c.Node.NodeID, req); err != nil {
			conn.Close()
			if errors.Is(err, protocol.ErrRejected) || errors.Is(err, protocol.ErrWrongNode) {
				return nil, err // the node answered; another path will not help
			}
			errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
			continue
		}
		c.mu.Lock()
		c.lastGood = addr
		c.mu.Unlock()
		return conn, nil
	}
	if len(errs) == 0 {
		return nil, errors.New("node has no reachable address")
	}
	return nil, errors.Join(errs...)
}

func (c *Client) ready(addr string) bool {
	t, err := c.reg.For(addr)
	return err == nil && t.Status().State == transport.StateReady
}

// Dial opens an authenticated stream to service on the node.
func (c *Client) Dial(ctx context.Context, service string) (net.Conn, error) {
	return c.dialAny(ctx, protocol.Request{Op: protocol.OpConnect, Service: service})
}

// Services asks the node for the current service list and saves it.
func (c *Client) Services(ctx context.Context) ([]protocol.ServiceInfo, error) {
	conn, err := c.dialRetry(ctx, protocol.Request{Op: protocol.OpList})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	sl, err := protocol.ReadServiceList(conn)
	if err != nil {
		return nil, err
	}
	c.Node.Services = sl.Services
	_ = saveNode(c.Dir, c.Node)
	return sl.Services, nil
}

// Listen accepts local connections on localAddr and tunnels each one to
// service. It blocks until ctx is cancelled. ready, if non-nil, receives the
// bound address once listening.
func (c *Client) Listen(ctx context.Context, localAddr, service string, ready func(addr string)) error {
	ln, err := net.Listen("tcp", localAddr)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	if ready != nil {
		ready(ln.Addr().String())
	}
	for {
		local, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			remote, err := c.Dial(ctx, service)
			if err != nil {
				slog.Warn("tunnel: could not reach the node", "service", service, "err", err)
				local.Close()
				return
			}
			netutil.Pipe(local, remote)
		}()
	}
}
