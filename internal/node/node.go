// Package node is the composition root for `sol` (node mode). It wires
// config, identity, devices, pairing, transports, the blob store, and the
// panel together, and it owns the node listener that every transport
// forwards to.
//
// Request path for a private service (docs/ARCHITECTURE.md):
//
//	client --Tor/I2P/clearnet--> transport --> node listener (127.0.0.1:node_port)
//	  --> protocol.ReadRequest (sol/1 handshake)
//	  --> device allowed? service exists?   (no: close, send nothing)
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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/api"
	"github.com/apv022/sol/internal/audit"
	"github.com/apv022/sol/internal/auth"
	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/events"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/netutil"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/protocol"
	"github.com/apv022/sol/internal/proxy"
	"github.com/apv022/sol/internal/runtime"
	"github.com/apv022/sol/internal/transport"
	"github.com/apv022/sol/web"
)

const (
	// pubNode is the publication ID of the node listener.
	pubNode = "node"
	// FilesService is the built-in service that serves the blob store to
	// paired devices. Its name cannot collide with user services, whose
	// names may not start with "_".
	FilesService = "_files"
	// serviceDialTimeout bounds connecting to a local service.
	serviceDialTimeout = 5 * time.Second
)

// Options configures a Node.
type Options struct {
	DataDir string // required, usually ~/.sol
	Version string // shown in the panel
	// PanelAddr overrides 127.0.0.1:<panel_port>. "off" disables the panel.
	PanelAddr string
	// NodeAddr overrides 127.0.0.1:<node_port>.
	NodeAddr string
	// Transports overrides the transports built from config (used by tests).
	Transports []transport.Transport
}

// Node is a Sol node.
type Node struct {
	opts    Options
	cfg     *config.Config
	id      *identity.Identity
	devices *auth.Devices
	pairing *pairing.Manager
	reg     *transport.Registry
	log     *events.Log
	blobs   *blob.Store
	started time.Time
	ready   chan struct{}

	// Set in Run before ready is closed.
	ctx       context.Context
	nodeAddr  string
	panelAddr string
	filesAddr string

	mu       sync.Mutex
	conns    map[net.Conn]connInfo
	public   map[string]*publicService
	lastAuth string // last client-auth key set published for the node
}

type connInfo struct {
	device, service string
	since           time.Time
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
		opts:    opts,
		cfg:     cfg,
		id:      id,
		devices: auth.NewDevices(cfg),
		pairing: pairing.NewManager(pairing.DefaultTTL),
		reg:     transport.NewRegistry(),
		log:     events.New(500),
		blobs:   blobs,
		ready:   make(chan struct{}),
		conns:   map[net.Conn]connInfo{},
		public:  map[string]*publicService{},
	}
	ts := opts.Transports
	if ts == nil {
		ts = buildTransports(cfg, opts.DataDir)
	}
	for _, t := range ts {
		n.reg.Add(t)
	}

	// Revocation: cut live connections now, and refresh Tor client auth so
	// the device can no longer even find the onion.
	n.devices.OnRemove(func(d config.Device) {
		n.closeConns(func(ci connInfo) bool { return ci.device == d.ID })
		n.log.Add(events.KindRevoke, "device removed: "+auth.Describe(d))
	})
	n.devices.OnChange(func() { n.refreshNodePublication(false) })
	return n, nil
}

// buildTransports creates the transports enabled in config.
func buildTransports(cfg *config.Config, dataDir string) []transport.Transport {
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
	if snap.Transports.Clearnet {
		ts = append(ts, transport.NewClearnet(snap.Transports.ClearnetListen, snap.Transports.ClearnetAdvertise))
	}
	return ts
}

// ID returns the node's identity.
func (n *Node) ID() *identity.Identity { return n.id }

// Ready is closed once Run has opened its listeners and started transports.
func (n *Node) Ready() <-chan struct{} { return n.ready }

// PanelAddr returns the panel's listen address (valid after Ready).
func (n *Node) PanelAddr() string { return n.panelAddr }

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

	// The blob store is served to paired devices as the built-in _files
	// service, over its own loopback HTTP listener.
	filesLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	filesSrv := &http.Server{Handler: n.blobs.Handler("/blob/"), ReadHeaderTimeout: 30 * time.Second}
	go filesSrv.Serve(filesLn)
	defer filesSrv.Close()
	n.filesAddr = filesLn.Addr().String()

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

	go n.acceptLoop(nodeLn)

	if err := n.reg.StartAll(ctx); err != nil {
		n.log.Add(events.KindTransport, "transport start: "+err.Error())
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
			// Drops the client-auth keys of expired pairing tokens.
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

// refreshNodePublication (re)publishes the node listener with the current
// client-auth keys: every paired device plus every pending pairing token.
// It does nothing if the key set is unchanged, unless force is set.
func (n *Node) refreshNodePublication(force bool) {
	if n.nodeAddr == "" {
		return // not running yet
	}
	keys := n.devices.TorAuthKeys()
	for _, t := range n.pairing.Pending() {
		if t.TorAuth != nil {
			keys = append(keys, t.TorAuth.TorPublic())
		}
	}
	sort.Strings(keys)
	keys = slices.Compact(keys)
	sig := strings.Join(keys, ",")

	n.mu.Lock()
	if !force && sig == n.lastAuth {
		n.mu.Unlock()
		return
	}
	n.lastAuth = sig
	n.mu.Unlock()

	_, err := n.reg.PublishAll(n.ctx, transport.Publication{ID: pubNode, Target: n.nodeAddr, ClientAuth: keys}, nil)
	if err != nil {
		n.log.Add(events.KindTransport, "publishing node: "+err.Error())
	}
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
	if err != nil {
		n.reject(c, "handshake failed: "+handshakeReason(err))
		return
	}
	switch req.Op {
	case protocol.OpPair:
		n.handlePair(c, req)
	case protocol.OpList:
		dev, ok := n.devices.Lookup(req.Key)
		if !ok {
			n.reject(c, "list from unknown device key")
			return
		}
		protocol.Accept(c, n.serviceList(dev))
		c.Close()
	case protocol.OpConnect:
		n.handleConnect(c, req)
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

func (n *Node) handlePair(c net.Conn, req *protocol.Request) {
	defer c.Close()
	tok, err := n.pairing.Consume(req.Secret)
	if err != nil {
		n.reject(c, "pairing attempt with an invalid or expired token")
		return
	}
	name := req.Name
	if name == "" {
		name = "device"
	}
	torPub := ""
	if tok.TorAuth != nil {
		torPub = tok.TorAuth.TorPublic()
	}
	// Re-pairing an existing key replaces its record (new name, new Tor key).
	if old, ok := n.devices.Lookup(req.Key); ok {
		n.devices.Remove(old.ID)
	}
	dev, err := n.devices.Add(name, req.Key, torPub, config.DeviceKindDevice)
	if err != nil {
		n.reject(c, "pairing failed: "+err.Error())
		return
	}
	if err := protocol.Accept(c, n.serviceList(dev)); err != nil {
		return
	}
	n.log.Add(events.KindPair, "device paired: "+auth.Describe(dev))
}

func (n *Node) handleConnect(c net.Conn, req *protocol.Request) {
	dev, ok := n.devices.Lookup(req.Key)
	if !ok {
		n.reject(c, "connection from unknown device key")
		return
	}
	var target string
	if req.Service == FilesService {
		target = n.filesAddr
	} else if svc, ok := n.cfg.Service(req.Service); ok {
		target = fmt.Sprintf("127.0.0.1:%d", svc.LocalPort)
	} else {
		n.reject(c, fmt.Sprintf("device %s asked for unknown service %q", dev.ID, truncate(req.Service, 40)))
		return
	}
	out, err := net.DialTimeout("tcp", target, serviceDialTimeout)
	if err != nil {
		n.reject(c, fmt.Sprintf("service %q is not reachable on %s", req.Service, target))
		return
	}
	if err := protocol.Accept(c, nil); err != nil {
		c.Close()
		out.Close()
		return
	}
	n.devices.Touch(dev.ID)

	n.mu.Lock()
	first := true
	for _, ci := range n.conns {
		if ci.device == dev.ID && ci.service == req.Service {
			first = false
			break
		}
	}
	n.conns[c] = connInfo{device: dev.ID, service: req.Service, since: time.Now().UTC()}
	n.mu.Unlock()
	if first {
		n.log.Add(events.KindConnect, fmt.Sprintf("%s connected to %s (signature verified)", auth.Describe(dev), req.Service))
	}
	defer func() {
		n.mu.Lock()
		delete(n.conns, c)
		n.mu.Unlock()
	}()
	// Close the race with a revocation that happened after Lookup.
	if !n.devices.IsAllowed(req.Key) {
		c.Close()
		out.Close()
		return
	}
	netutil.Pipe(c, out)
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

func (n *Node) serviceList(dev config.Device) protocol.ServiceList {
	sl := protocol.ServiceList{NodeID: n.id.NodeID(), DeviceID: dev.ID}
	for _, s := range n.cfg.ServiceList() {
		sl.Services = append(sl.Services, protocol.ServiceInfo{Name: s.Name, Protocol: s.Protocol})
	}
	sl.Services = append(sl.Services, protocol.ServiceInfo{Name: FilesService, Protocol: config.ProtocolHTTP})
	return sl
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
	if svc.Visibility == config.VisibilityAnonymousPublic {
		// Never expose an anonymous service on clearnet.
		if len(only) == 0 {
			for _, t := range n.reg.List() {
				only = append(only, t.Name())
			}
		}
		only = slices.DeleteFunc(slices.Clone(only), func(s string) bool { return s == "clearnet" })
		if len(only) == 0 {
			n.log.Add(events.KindService, svc.Name+": no anonymous transport available, not published")
			if stop != nil {
				stop()
			}
			return
		}
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

// --- api.Backend ---------------------------------------------------------

var _ api.Backend = (*Node)(nil)

// Status implements api.Backend.
func (n *Node) Status() api.StatusInfo {
	return api.StatusInfo{
		NodeID:     n.id.NodeID(),
		Version:    n.opts.Version,
		StartedAt:  n.started,
		Uptime:     time.Since(n.started).Round(time.Second).String(),
		Transports: n.reg.Statuses(),
	}
}

// Services implements api.Backend.
func (n *Node) Services() []api.ServiceView {
	var out []api.ServiceView
	n.mu.Lock()
	for _, s := range n.cfg.ServiceList() {
		v := api.ServiceView{Service: s}
		if p := n.public[s.Name]; p != nil {
			v.Addresses = p.addrs
		}
		for _, ci := range n.conns {
			if ci.service == s.Name {
				v.ActiveStreams++
			}
		}
		out = append(out, v)
	}
	n.mu.Unlock()
	// Probe outside the lock: is something actually listening on the port?
	for i := range out {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", out[i].LocalPort), 300*time.Millisecond)
		if err == nil {
			c.Close()
			out[i].Running = true
		}
	}
	if out == nil {
		out = []api.ServiceView{}
	}
	return out
}

// AddService implements api.Backend.
func (n *Node) AddService(s config.Service) error {
	if s.Protocol == "" {
		s.Protocol = config.ProtocolTCP
	}
	if err := n.cfg.AddService(s); err != nil {
		return err
	}
	if s.Visibility != config.VisibilityPrivate && n.ctx != nil {
		n.publishService(s)
	}
	n.log.Add(events.KindService, fmt.Sprintf("service added: %s -> 127.0.0.1:%d (%s)", s.Name, s.LocalPort, s.Visibility))
	return nil
}

// RemoveService implements api.Backend. Live connections to it are closed.
func (n *Node) RemoveService(name string) error {
	if err := n.cfg.RemoveService(name); err != nil {
		return err
	}
	n.unpublishService(name)
	n.closeConns(func(ci connInfo) bool { return ci.service == name })
	n.log.Add(events.KindService, "service removed: "+name)
	return nil
}

// SetServiceVisibility implements api.Backend.
func (n *Node) SetServiceVisibility(name, visibility string) error {
	if err := n.cfg.SetServiceVisibility(name, visibility); err != nil {
		return err
	}
	n.unpublishService(name)
	if svc, ok := n.cfg.Service(name); ok && svc.Visibility != config.VisibilityPrivate && n.ctx != nil {
		n.publishService(svc)
	}
	n.log.Add(events.KindService, fmt.Sprintf("service %s is now %s", name, visibility))
	return nil
}

// Devices implements api.Backend. Each device carries its live,
// handshake-verified connections grouped by service.
func (n *Node) Devices() []api.DeviceView {
	type key struct{ dev, svc string }
	live := map[key]*api.ConnectionView{}
	n.mu.Lock()
	for _, ci := range n.conns {
		k := key{ci.device, ci.service}
		cv := live[k]
		if cv == nil {
			cv = &api.ConnectionView{Service: ci.service, Since: ci.since}
			live[k] = cv
		}
		cv.Streams++
		if ci.since.Before(cv.Since) {
			cv.Since = ci.since
		}
	}
	n.mu.Unlock()

	out := []api.DeviceView{}
	for _, d := range n.devices.List() {
		v := api.DeviceView{Device: d, Connections: []api.ConnectionView{}, TorAuth: d.TorAuthPub != "", KeyFingerprint: d.ID}
		for k, cv := range live {
			if k.dev == d.ID {
				v.Connections = append(v.Connections, *cv)
			}
		}
		sort.Slice(v.Connections, func(i, j int) bool { return v.Connections[i].Service < v.Connections[j].Service })
		v.Online = len(v.Connections) > 0
		out = append(out, v)
	}
	return out
}

// RemoveDevice implements api.Backend.
func (n *Node) RemoveDevice(id string) error { return n.devices.Remove(id) }

// NewPairing implements api.Backend: it creates a one-time token and returns
// the pairing link containing every address the node is reachable at.
func (n *Node) NewPairing() (api.PairingInfo, error) {
	tor, hasTor := n.reg.Get("tor")
	if _, unavailable := tor.(*transport.Unavailable); unavailable {
		hasTor = false
	}
	tok, err := n.pairing.NewToken(hasTor)
	if err != nil {
		return api.PairingInfo{}, err
	}
	n.refreshNodePublication(false) // authorize the token's Tor key
	link := pairing.Link{NodeID: n.id.NodeID(), Secret: tok.Secret}
	if tok.TorAuth != nil {
		link.TorAuthPriv = tok.TorAuth.TorPrivate()
	}
	for _, st := range n.reg.Statuses() {
		addr := st.Addresses[pubNode]
		switch st.Name {
		case "tor":
			link.Onion = addr
		case "i2p":
			link.I2P = addr
		case "clearnet":
			link.LAN = addr
		}
	}
	if link.Onion == "" && link.I2P == "" && link.LAN == "" {
		return api.PairingInfo{}, errors.New("no transport has published the node yet; check the transport status on the dashboard")
	}
	return api.PairingInfo{Link: link.String(), Expires: tok.Expires}, nil
}

// Blobs implements api.Backend.
func (n *Node) Blobs() *blob.Store { return n.blobs }

// Events implements api.Backend.
func (n *Node) Events() *events.Log { return n.log }

// Audit implements api.Backend.
func (n *Node) Audit(ctx context.Context) []audit.Result {
	return audit.Run(ctx, audit.Env{Config: n.cfg.Snapshot()}, "")
}
