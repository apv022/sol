// Package node is the composition root for `sol`. It wires config, identity,
// peers, pairing, transports, the blob store, the panel, and the control
// socket together, and it owns the node listener that every transport
// forwards to.
//
// Every Sol install is a node, and nodes pair with each other as peers
// (docs/PEERS.md). A node both accepts connections from its peers and dials
// out to them.
//
// Request path for a private service (docs/ARCHITECTURE.md):
//
//	peer --Tor/I2P/clearnet--> transport --> node listener (127.0.0.1:node_port)
//	  --> protocol.ReadRequest (signed handshake)
//	  --> is it a peer? is the service shared with it?   (no: close, send nothing)
//	  --> net.Dial 127.0.0.1:<service port> --> byte pipe both ways
//
// Public and anonymous-public services do not go through the node listener:
// each gets its own publication (its own onion), so they cannot be linked to
// the node's identity.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/api"
	"github.com/apv022/sol/internal/autostart"
	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/control"
	"github.com/apv022/sol/internal/events"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/netutil"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/peers"
	"github.com/apv022/sol/internal/protocol"
	"github.com/apv022/sol/internal/proxy"
	"github.com/apv022/sol/internal/runtime"
	"github.com/apv022/sol/internal/transport"
	"github.com/apv022/sol/web"
)

const (
	// pubNode is the publication ID of the node listener.
	pubNode = "node"
	// serviceDialTimeout bounds connecting to a local service.
	serviceDialTimeout = 5 * time.Second

	// DefaultInboxMaxFile and DefaultInboxMaxTotal bound what peers can send.
	DefaultInboxMaxFile  = 4 << 30  // 4 GiB per file
	DefaultInboxMaxTotal = 20 << 30 // 20 GiB of received files in total
)

// Options configures a Node.
type Options struct {
	DataDir string // required, usually ~/.sol
	Version string // shown in the panel
	// PanelAddr overrides 127.0.0.1:<panel_port>. "off" disables the panel.
	PanelAddr string
	// NodeAddr overrides 127.0.0.1:<node_port>.
	NodeAddr string
	// ControlPath overrides <DataDir>/control.sock. "off" disables it.
	ControlPath string
	// Transports overrides the transports built from config (used by tests).
	Transports []transport.Transport

	// Inbox limits; zero means the defaults above.
	InboxMaxFile, InboxMaxTotal int64
	// Dial retry policy for reaching a peer whose onion may still be
	// publishing; zero means 6 attempts, 20 seconds apart.
	DialAttempts   int
	DialRetryDelay time.Duration
}

// Node is a Sol node.
type Node struct {
	opts    Options
	cfg     *config.Config
	id      *identity.Identity
	peers   *peers.Peers
	pairing *pairing.Manager
	reg     *transport.Registry
	log     *events.Log
	blobs   *blob.Store
	started time.Time
	ready   chan struct{}

	// Set in Run before ready is closed.
	ctx         context.Context
	nodeAddr    string
	panelAddr   string
	controlPath string

	mu       sync.Mutex
	conns    map[net.Conn]connInfo
	public   map[string]*publicService
	lastAuth map[string]string // transport -> client-auth key set last published for the node
	lastGood map[string]string // peer id -> address that last worked
	// peer id -> last refreshDirect (dial.go)
	lastDirectRefresh map[string]time.Time
	directFail        map[string]directFailure // peer id -> last failed direct address

	tmu     sync.Mutex
	tunnels map[int]*tunnel // by local port (tunnels.go)

	autostart autostart.Manager // for the panel's "starts at login"
}

// connInfo describes one live, handshake-verified connection with a peer.
type connInfo struct {
	peer, service string
	since         time.Time
	outbound      bool   // we dialed the peer (false: the peer dialed us)
	via           string // outbound only: the transport that carried it
}

type publicService struct {
	addrs map[string]string
	stop  func()
}

// New loads (or creates) the node's state in opts.DataDir. It does not open
// any listeners; call Run.
func New(opts Options) (*Node, error) {
	if opts.DataDir == "" {
		return nil, errors.New("node: DataDir is required")
	}
	if opts.InboxMaxFile <= 0 {
		opts.InboxMaxFile = DefaultInboxMaxFile
	}
	if opts.InboxMaxTotal <= 0 {
		opts.InboxMaxTotal = DefaultInboxMaxTotal
	}
	if opts.DialAttempts <= 0 {
		opts.DialAttempts = 6
	}
	if opts.DialRetryDelay <= 0 {
		opts.DialRetryDelay = 20 * time.Second
	}
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return nil, err
	}
	cfg, err := config.Load(filepath.Join(opts.DataDir, "config.json"))
	if err != nil {
		return nil, err
	}
	id, err := identity.LoadOrCreate(filepath.Join(opts.DataDir, "node.key"))
	if err != nil {
		return nil, err
	}
	blobs, err := blob.New(filepath.Join(opts.DataDir, "blobs"))
	if err != nil {
		return nil, err
	}
	n := &Node{
		opts:     opts,
		cfg:      cfg,
		id:       id,
		peers:    peers.New(cfg),
		pairing:  pairing.NewManager(pairing.DefaultTTL),
		reg:      transport.NewRegistry(),
		log:      events.New(500),
		blobs:    blobs,
		ready:    make(chan struct{}),
		conns:    map[net.Conn]connInfo{},
		public:   map[string]*publicService{},
		lastAuth: map[string]string{},
		lastGood: map[string]string{},
		tunnels:  map[int]*tunnel{},

		lastDirectRefresh: map[string]time.Time{},
		directFail:        map[string]directFailure{},
	}
	// A temporary binary (go run, go test) reports autostart as unsupported
	// rather than reading the real user's entry.
	if m, err := autostart.ForThisProcess(goruntime.GOOS, opts.DataDir); err == nil {
		n.autostart = m
	}
	ts := opts.Transports
	if ts == nil {
		ts = buildTransports(cfg, opts.DataDir, id)
	}
	for _, t := range ts {
		n.reg.Add(t)
	}

	// Removal: cut live connections in both directions now, forget the
	// peer's access key, and refresh our publication so the peer can no
	// longer even find our onion.
	n.peers.OnRemove(func(p config.Peer) {
		n.closeConns(func(ci connInfo) bool { return ci.peer == p.ID })
		n.stopTunnels(func(t config.Tunnel) bool { return t.Peer == p.ID })
		n.removeAccess(p)
		n.mu.Lock()
		delete(n.lastGood, p.ID)
		n.mu.Unlock()
		n.log.Add(events.KindRevoke, "peer removed: "+peers.Describe(p))
	})
	n.peers.OnChange(func() { n.refreshNodePublication(false) })
	return n, nil
}

// buildTransports creates the transports enabled in config.
func buildTransports(cfg *config.Config, dataDir string, id *identity.Identity) []transport.Transport {
	snap := cfg.Snapshot()
	ropts := runtime.Options{BinDir: filepath.Join(dataDir, "bin"), ContainerRuntime: snap.Runtime.ContainerRuntime}
	var ts []transport.Transport
	if snap.Transports.Tor {
		if prov, err := runtime.Select(snap.Runtime.ProviderOrder, "tor", ropts); err != nil {
			ts = append(ts, transport.NewUnavailable("tor", err))
		} else {
			ts = append(ts, transport.NewTor(filepath.Join(dataDir, "tor"), prov))
		}
	}
	if snap.Transports.I2P {
		prov, _ := runtime.Select(snap.Runtime.ProviderOrder, "i2pd", ropts)
		ts = append(ts, transport.NewI2P(filepath.Join(dataDir, "i2pd"), prov))
	}
	if snap.Transports.Direct {
		ts = append(ts, transport.NewDirect(snap.Transports.DirectListen, id.Signer(), snap.Transports.DirectAdvertise...))
	}
	if snap.Transports.Clearnet {
		ts = append(ts, transport.NewClearnet(snap.Transports.ClearnetListen, snap.Transports.ClearnetAdvertise))
	}
	return ts
}

// ID returns the node's identity.
func (n *Node) ID() *identity.Identity { return n.id }

// Name returns the node's name, as its peers see it.
func (n *Node) Name() string { return n.cfg.NodeName() }

// Ready is closed once Run has opened its listeners and started transports.
func (n *Node) Ready() <-chan struct{} { return n.ready }

// PanelAddr returns the panel's listen address (valid after Ready).
func (n *Node) PanelAddr() string { return n.panelAddr }

// ControlPath returns the control socket path ("" if disabled; valid after Ready).
func (n *Node) ControlPath() string { return n.controlPath }

// Run opens the listeners, starts transports, publishes the node and public
// services, and blocks until ctx is cancelled. It then shuts everything down.
func (n *Node) Run(ctx context.Context) error {
	n.ctx = ctx
	n.started = time.Now()
	snap := n.cfg.Snapshot()

	nodeAddr := n.opts.NodeAddr
	if nodeAddr == "" {
		nodeAddr = fmt.Sprintf("127.0.0.1:%d", snap.NodePort)
	}
	nodeLn, err := net.Listen("tcp", nodeAddr)
	if err != nil {
		return fmt.Errorf("node listener: %w", err)
	}
	defer nodeLn.Close()
	n.nodeAddr = nodeLn.Addr().String()

	// Tunnels first, so the panel and CLI see them from their first request.
	n.startSavedTunnels()
	defer n.stopTunnels(func(config.Tunnel) bool { return true })

	var panelSrv *http.Server
	if n.opts.PanelAddr != "off" {
		panelAddr := n.opts.PanelAddr
		if panelAddr == "" {
			panelAddr = fmt.Sprintf("127.0.0.1:%d", snap.PanelPort)
		}
		host, _, _ := net.SplitHostPort(panelAddr)
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("panel must bind a loopback address, got %q", panelAddr)
		}
		panelLn, err := net.Listen("tcp", panelAddr)
		if err != nil {
			return fmt.Errorf("panel listener: %w", err)
		}
		n.panelAddr = panelLn.Addr().String()
		panelSrv = &http.Server{Handler: api.Handler(n, web.FS), ReadHeaderTimeout: 30 * time.Second}
		go panelSrv.Serve(panelLn)
	}

	var ctlLn net.Listener
	if n.opts.ControlPath != "off" {
		path := n.opts.ControlPath
		if path == "" {
			path = control.Path(n.opts.DataDir)
		}
		if ctlLn, err = listenControl(path); err != nil {
			return err
		}
		defer ctlLn.Close()
		n.controlPath = path
		go n.serveControl(ctlLn)
	}

	go n.acceptLoop(nodeLn)

	if err := n.reg.StartAll(ctx); err != nil {
		n.log.Add(events.KindTransport, "transport start: "+err.Error())
	}
	for _, p := range n.peers.List() {
		n.installAccess(p)
	}
	n.refreshNodePublication(true)
	for _, svc := range n.cfg.ServiceList() {
		if svc.Visibility != config.VisibilityPrivate {
			n.publishService(svc)
		}
	}
	close(n.ready)

	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			n.shutdown(panelSrv)
			return nil
		case <-tick.C:
			// Drops the access keys of expired pairing tokens.
			n.refreshNodePublication(false)
		}
	}
}

func (n *Node) shutdown(panelSrv *http.Server) {
	if panelSrv != nil {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		panelSrv.Shutdown(sctx)
		cancel()
	}
	n.stopTunnels(func(config.Tunnel) bool { return true })
	n.closeConns(func(connInfo) bool { return true })
	n.mu.Lock()
	for _, p := range n.public {
		if p.stop != nil {
			p.stop()
		}
	}
	n.mu.Unlock()
	if err := n.reg.StopAll(); err != nil {
		slog.Warn("stopping transports", "err", err)
	}
}

// refreshNodePublication (re)publishes the node listener on every usable
// transport with that transport's access keys: the ones issued to peers plus
// the ones held by pending pairing tokens. A transport whose key set is
// unchanged is skipped, unless force is set.
func (n *Node) refreshNodePublication(force bool) {
	if n.nodeAddr == "" {
		return // not running yet
	}
	pending := n.pairing.Pending()
	for _, t := range n.reg.List() {
		if !transport.Usable(t) {
			continue
		}
		name := t.Name()
		keys := n.peers.AccessKeys(name)
		for _, tok := range pending {
			if a, ok := tok.Access[name]; ok && a.Authorize != "" {
				keys = append(keys, a.Authorize)
			}
		}
		sort.Strings(keys)
		keys = slices.Compact(keys)
		sig := strings.Join(keys, ",")

		n.mu.Lock()
		last, seen := n.lastAuth[name]
		if !force && seen && sig == last {
			n.mu.Unlock()
			continue
		}
		n.lastAuth[name] = sig
		n.mu.Unlock()

		if _, err := t.Publish(n.ctx, transport.Publication{ID: pubNode, Target: n.nodeAddr, ClientAuth: keys}); err != nil {
			n.log.Add(events.KindTransport, "publishing the node on "+name+": "+err.Error())
		}
	}
}

// ownAddrs returns the node's public addresses, per transport.
func (n *Node) ownAddrs() map[string]string {
	addrs := map[string]string{}
	for _, st := range n.reg.Statuses() {
		if a := st.Addresses[pubNode]; a != "" {
			addrs[st.Name] = a
		}
	}
	return addrs
}

// installAccess gives our transports the credentials a peer issued to us, so
// we can reach its access-restricted addresses.
func (n *Node) installAccess(p config.Peer) {
	for name, secret := range p.AccessOut {
		addr := p.Addrs[name]
		t, ok := n.reg.Get(name)
		if !ok || addr == "" || secret == "" {
			continue
		}
		if ca, ok := t.(transport.ClientAuthorizer); ok {
			if err := ca.AuthorizeClient(addr, secret); err != nil {
				slog.Debug("installing peer access", "peer", p.ID, "transport", name, "err", err)
			}
		}
	}
}

// removeAccess forgets the credentials installed by installAccess.
func (n *Node) removeAccess(p config.Peer) {
	for name := range p.AccessOut {
		t, ok := n.reg.Get(name)
		if !ok || p.Addrs[name] == "" {
			continue
		}
		if ca, ok := t.(transport.ClientAuthorizer); ok {
			ca.DeauthorizeClient(p.Addrs[name])
		}
	}
}

// issueAccess creates one fresh credential on every transport that can
// restrict access, for a new peer.
func (n *Node) issueAccess() (map[string]transport.Access, error) {
	out := map[string]transport.Access{}
	for _, t := range n.reg.List() {
		ai, ok := t.(transport.AccessIssuer)
		if !ok || !transport.Usable(t) {
			continue
		}
		a, err := ai.NewAccess()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Name(), err)
		}
		out[t.Name()] = a
	}
	return out, nil
}

func (n *Node) acceptLoop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go n.handleConn(c)
	}
}

// reject closes c without sending anything and records why.
func (n *Node) reject(c net.Conn, why string) {
	c.Close()
	n.log.Add(events.KindReject, why)
}

func (n *Node) handleConn(c net.Conn) {
	req, err := protocol.ReadRequest(c, n.id)
	if errors.Is(err, protocol.ErrNoData) {
		c.Close() // a reachability probe (e.g. Tor's self-check); not worth logging
		return
	}
	if err != nil {
		n.reject(c, "handshake failed: "+handshakeReason(err))
		return
	}
	if req.Op == protocol.OpPair {
		n.handlePair(c, req)
		return
	}
	p, ok := n.peers.Lookup(req.Key)
	if !ok {
		n.reject(c, req.Op+" from a key that isn't a peer")
		return
	}
	switch req.Op {
	case protocol.OpList:
		protocol.Accept(c, n.peerInfo(p))
		c.Close()
	case protocol.OpUnpair:
		protocol.Accept(c, nil)
		c.Close()
		if err := n.peers.Remove(p.ID); err == nil {
			n.log.Add(events.KindRevoke, p.Name+" unpaired from this node, so it was removed here too")
		}
	case protocol.OpConnect:
		n.handleConnect(c, req, p)
	}
}

func handshakeReason(err error) string {
	switch {
	case errors.Is(err, protocol.ErrBadSignature):
		return "bad signature"
	case errors.Is(err, protocol.ErrMalformed):
		return "malformed request"
	default:
		return "connection error"
	}
}

// handlePair is the inviter's side of pairing (docs/PEERS.md).
func (n *Node) handlePair(c net.Conn, req *protocol.Request) {
	defer c.Close()
	tok, err := n.pairing.Consume(req.Secret)
	if err != nil {
		n.reject(c, "pairing attempt with an invalid or expired link")
		return
	}
	if identity.NodeID(req.Key) == n.id.NodeID() {
		n.reject(c, "pairing attempt from this node's own key")
		return
	}
	name := req.Card.Name
	if config.ValidateName(name) != nil {
		name = "peer"
	}
	accessIn := map[string]string{}
	for t, a := range tok.Access {
		accessIn[t] = a.Authorize
	}
	p, err := n.peers.Add(config.Peer{Name: name, Addrs: req.Card.Addrs, AccessIn: accessIn, AccessOut: req.Card.Access}, req.Key)
	if err != nil {
		n.reject(c, "pairing failed: "+err.Error())
		return
	}
	n.installAccess(p)
	if err := protocol.Accept(c, n.peerInfo(p)); err != nil {
		return
	}
	n.log.Add(events.KindPair, "paired with "+peers.Describe(p))
	go n.learnShares(p.ID)
}

// learnShares asks a just-paired joiner what it shares with us, so the
// inviter's view isn't empty. The joiner saves us only after reading our
// reply, so the first attempt may be refused; it is retried a few times.
func (n *Node) learnShares(id string) {
	for _, wait := range []time.Duration{time.Second, 5 * time.Second, 30 * time.Second} {
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(wait):
		}
		ctx, cancel := context.WithTimeout(n.ctx, 2*time.Minute)
		_, err := n.RefreshPeer(ctx, id)
		cancel()
		if err == nil || errors.Is(err, config.ErrNotFound) {
			return
		}
	}
}

func (n *Node) handleConnect(c net.Conn, req *protocol.Request, p config.Peer) {
	if !p.Shared(req.Service) {
		n.reject(c, fmt.Sprintf("%s asked for %q, which isn't shared with it", peers.Describe(p), truncate(req.Service, 40)))
		return
	}
	var out net.Conn
	switch req.Service {
	case peers.Inbox, peers.Files:
	default:
		svc, ok := n.cfg.Service(req.Service)
		if !ok {
			n.reject(c, fmt.Sprintf("%s asked for %q, which no longer exists", peers.Describe(p), truncate(req.Service, 40)))
			return
		}
		target := fmt.Sprintf("127.0.0.1:%d", svc.LocalPort)
		var err error
		if out, err = net.DialTimeout("tcp", target, serviceDialTimeout); err != nil {
			n.reject(c, fmt.Sprintf("service %q is not reachable on %s", req.Service, target))
			return
		}
	}
	closeAll := func() {
		c.Close()
		if out != nil {
			out.Close()
		}
	}
	if err := protocol.Accept(c, nil); err != nil {
		closeAll()
		return
	}
	n.peers.Touch(p.ID)
	if n.track(c, connInfo{peer: p.ID, service: req.Service, since: time.Now().UTC()}) {
		n.log.Add(events.KindConnect, fmt.Sprintf("%s connected to %s (signature verified)", peers.Describe(p), req.Service))
	}
	defer n.untrack(c)
	// Close the race with a removal that happened after Lookup.
	if cur, ok := n.peers.Lookup(req.Key); !ok || !cur.Shared(req.Service) {
		closeAll()
		return
	}
	switch req.Service {
	case peers.Inbox:
		serveHTTPConn(c, n.inboxHandler(p))
	case peers.Files:
		serveHTTPConn(c, n.blobs.Browser())
	default:
		netutil.Pipe(c, out)
	}
}

// track records a live connection and reports whether it is the first one
// between that peer and service (worth logging).
func (n *Node) track(c net.Conn, ci connInfo) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	first := true
	for _, o := range n.conns {
		if o.peer == ci.peer && o.service == ci.service && o.outbound == ci.outbound {
			first = false
			break
		}
	}
	n.conns[c] = ci
	return first
}

func (n *Node) untrack(c net.Conn) {
	n.mu.Lock()
	delete(n.conns, c)
	n.mu.Unlock()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// closeConns closes every tracked connection matching match.
func (n *Node) closeConns(match func(connInfo) bool) {
	n.mu.Lock()
	var victims []net.Conn
	for c, ci := range n.conns {
		if match(ci) {
			victims = append(victims, c)
		}
	}
	n.mu.Unlock()
	for _, c := range victims {
		c.Close()
	}
}

// peerInfo is what we tell peer p about ourselves: our name and the
// services we share with it.
func (n *Node) peerInfo(p config.Peer) protocol.PeerInfo {
	pi := protocol.PeerInfo{NodeID: n.id.NodeID(), Name: n.cfg.NodeName(), PeerID: p.ID, Services: []protocol.ServiceInfo{},
		Direct: n.ownAddrs()["direct"]}
	for _, name := range p.Shares {
		switch name {
		case peers.Inbox, peers.Files:
			pi.Services = append(pi.Services, protocol.ServiceInfo{Name: name, Protocol: config.ProtocolHTTP})
		default:
			if s, ok := n.cfg.Service(name); ok {
				pi.Services = append(pi.Services, protocol.ServiceInfo{Name: s.Name, Protocol: s.Protocol})
			}
		}
	}
	return pi
}

// publishService gives a public or anonymous-public service its own
// publication on the allowed transports.
func (n *Node) publishService(svc config.Service) {
	target := fmt.Sprintf("127.0.0.1:%d", svc.LocalPort)
	var stop func()
	if svc.Protocol == config.ProtocolHTTP {
		// Put the HTTP proxy in front so headers can be stripped.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			n.log.Add(events.KindService, "publishing "+svc.Name+": "+err.Error())
			return
		}
		strip := svc.Visibility == config.VisibilityAnonymousPublic
		srv := &http.Server{Handler: proxy.NewServiceProxy(svc.LocalPort, strip), ReadHeaderTimeout: 30 * time.Second}
		go srv.Serve(ln)
		target, stop = ln.Addr().String(), func() { srv.Close() }
	}
	only := svc.Transports
	if len(only) == 0 {
		for _, t := range n.reg.List() {
			only = append(only, t.Name())
		}
	}
	// Direct mode carries only the node itself (transport/direct.go), and an
	// anonymous service is never exposed on clearnet.
	only = slices.DeleteFunc(slices.Clone(only), func(s string) bool {
		return s == "direct" || (s == "clearnet" && svc.Visibility == config.VisibilityAnonymousPublic)
	})
	if len(only) == 0 { // PublishAll would read an empty list as "all"
		n.log.Add(events.KindService, svc.Name+": no suitable transport for a public service, not published")
		if stop != nil {
			stop()
		}
		return
	}
	addrs, err := n.reg.PublishAll(n.ctx, transport.Publication{ID: "svc-" + svc.Name, Target: target}, only)
	if err != nil {
		n.log.Add(events.KindService, "publishing "+svc.Name+": "+err.Error())
	}
	n.mu.Lock()
	n.public[svc.Name] = &publicService{addrs: addrs, stop: stop}
	n.mu.Unlock()
}

func (n *Node) unpublishService(name string) {
	n.mu.Lock()
	p := n.public[name]
	delete(n.public, name)
	n.mu.Unlock()
	if p == nil {
		return
	}
	n.reg.UnpublishAll("svc-" + name)
	if p.stop != nil {
		p.stop()
	}
}
