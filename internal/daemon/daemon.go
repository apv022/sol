// Package daemon assembles one Sol device: the keyring, the transports, the session manager,
// feeds with replication, blobs with their network service, pairing, the app API and the app
// host, over one data directory. `sol run` is Start, then waiting for a signal, then Close.
//
// Data directory layout (Init creates it; every directory 0700, every secret 0600, every file
// that matters replaced atomically):
//
//	<dir>/config.json      transports, UI address, synced folder (Config)
//	<dir>/keys/            the keyring (internal/identity: identity.key, 0600)
//	<dir>/feeds/           the feed store (internal/feeds)
//	<dir>/blobs/           the blob store (internal/blob)
//	<dir>/peers.json       the newest identity bundle of every device I held a session with,
//	                       so sessions come back after a restart (0600)
//	<dir>/inbox.json       the app inbox (internal/appapi, 0600)
//	<dir>/agents.json      the agent keys this device issued (agents.go, 0600)
//	<dir>/agents/          agent key files sol agent new writes by default (0700, files 0600)
//	<dir>/browser-sessions.json  browser logins to the Sol app, token hashes only (0600)
//	<dir>/services.json    local ports I share (port sharing, 0600)
//	<dir>/tunnels.json     contacts' services I opened on local ports (0600)
//	<dir>/petnames.json    the local names I gave contacts (0600)
//	<dir>/admin.token      the CLI's secret for the admin API, new on every start (0600)
//	<dir>/api.url          where the app API listens, in SOL_API form
//	<dir>/ui.url           the Sol app (the UI root)
//	<dir>/apps/<id>/       installed apps (internal/apphost)
//	<dir>/appdata/<id>/    each app's SOL_APP_DATA
//	<dir>/run/             api.sock and the apps' sockets; a short temporary directory instead
//	                       when <dir> is too deep for a unix socket path
//	<dir>/tor/, wg.key     transport state
//
// Invariants:
//   - A first run creates the identity (Init); Start never does, so a typo in --dir cannot
//     silently create a second person.
//   - Apps start only once this device belongs to a person (after Init, or after a join).
//   - Close stops everything Start started, in reverse order, and waits for it.
//   - Nothing here logs keys, tokens, pairing secrets or bodies.
package daemon

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/appapi"
	"github.com/apv022/sol/internal/apphost"
	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/blobnet"
	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/replicate"
	solruntime "github.com/apv022/sol/internal/runtime"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/internal/transport"
	"github.com/apv022/sol/sdk/go/sol"
)

// TransportConfig enables one transport.
type TransportConfig struct {
	// Name is "clearnet", "direct", "wg" or "tor".
	Name string `json:"name"`
	// Listen is the local address (clearnet, direct, wg), e.g. "0.0.0.0:7700".
	Listen string `json:"listen,omitempty"`
	// Advertise lists the addresses peers should use ("" means detect).
	Advertise []string `json:"advertise,omitempty"`
	// STUN servers (wg).
	STUN []string `json:"stun,omitempty"`
}

// Config is the persisted configuration (config.json).
type Config struct {
	Transports []TransportConfig `json:"transports"`
	// UI is the Sol app's and app UIs' listen address, e.g. "127.0.0.1:7777".
	UI string `json:"ui"`
	// Files is the synced folder (SOL_FILES).
	Files string `json:"files"`
	// DeviceName is the name a device made with --join gives itself when it joins (the join
	// request carries it, PROTOCOL.md "Settled in C3"). Unused once the device belongs to a
	// person: the device list holds its name.
	DeviceName string `json:"device_name,omitempty"`
	// TorBin is a tor the platform provides (`sol init --tor-bin`), run without a checksum
	// check; SOL_TOR_BIN overrides it (internal/runtime, provider "platform").
	TorBin string `json:"tor_bin,omitempty"`
}

// InitOptions are the choices of a first run.
type InitOptions struct {
	// Name and Device name the new person and this device. With Join, Name is ignored and
	// Device is the name the device asks for when it joins.
	Name, Device string
	// Join creates only a device key, for a device that will join an existing person
	// (sol pair <join link>).
	Join   bool
	Config Config
	// Builtins are installed as apps.
	Builtins map[string]apphost.Builtin
}

// Errors.
var (
	ErrExists = errors.New("daemon: this directory already holds a Sol identity")
	ErrNoInit = errors.New("daemon: no Sol identity here; run sol init first")
)

func keysDir(dir string) string { return filepath.Join(dir, "keys") }

// Init creates a data directory: the identity (a new person, or with Join a bare device key),
// config.json and the builtin apps.
func Init(dir string, o InitOptions) error {
	if _, err := identity.Open(keysDir(dir)); err == nil {
		return ErrExists
	}
	if o.Join && o.Config.DeviceName == "" {
		o.Config.DeviceName = o.Device
	}
	if err := prepareDir(dir, &o.Config, o.Builtins); err != nil {
		return err
	}
	now := time.Now()
	var err error
	if o.Join {
		_, err = identity.NewDevice(keysDir(dir), now)
	} else {
		if o.Name == "" || o.Device == "" {
			return errors.New("daemon: a name and a device name are required")
		}
		_, err = identity.Create(keysDir(dir), o.Name, o.Device, now)
	}
	return err
}

// prepareDir creates a data directory's config.json (filling in defaults) and installs the
// builtin apps' manifests.
func prepareDir(dir string, cfg *Config, builtins map[string]apphost.Builtin) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if cfg.UI == "" {
		cfg.UI = "127.0.0.1:7777"
	}
	if cfg.Files == "" {
		if h, err := os.UserHomeDir(); err == nil {
			cfg.Files = filepath.Join(h, "SolDrive")
		}
	}
	if len(cfg.Transports) == 0 {
		cfg.Transports = DefaultTransports()
	}
	if err := writeConfig(dir, *cfg); err != nil {
		return err
	}
	for name, bi := range builtins {
		m, err := sol.ParseManifest(bi.Manifest)
		if err != nil {
			return fmt.Errorf("daemon: builtin %s: %w", name, err)
		}
		ad := filepath.Join(dir, "apps", m.ID)
		if err := os.MkdirAll(ad, 0o700); err != nil {
			return err
		}
		if err := fsutil.WriteFileAtomic(filepath.Join(ad, "app.json"), bi.Manifest, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// writeConfig replaces config.json (0600, atomically).
func writeConfig(dir string, cfg Config) error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return fsutil.WriteFileAtomic(filepath.Join(dir, "config.json"), b, 0o600)
}

// freePort returns a TCP port free right now, so a default config gets a stable port.
func freePort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 7700
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// LoadConfig reads config.json.
func LoadConfig(dir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return c, ErrNoInit
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("daemon: config.json: %w", err)
	}
	return c, nil
}

// Options are the run-time choices of Start that are not persisted.
type Options struct {
	Dir      string
	Builtins map[string]apphost.Builtin
	// InProcessApps runs builtins as goroutines (tests) instead of `sol app run` children.
	InProcessApps bool
	// Exe is the sol binary for builtin children ("" means this executable).
	Exe string
	// APIOverTCP serves the app API on 127.0.0.1 instead of a unix socket (always on Windows).
	APIOverTCP bool
	Log        *slog.Logger
	// SameUser decides browser auto-login (see peercred.go); nil means the OS lookup. Tests inject.
	SameUser SameUserFunc
	// TorProvider runs the tor program for the "tor" transport (nil: the bundled or system
	// tor). Tests inject a fake tor.
	TorProvider solruntime.Provider
	// WrapTransport, if set, wraps every configured transport (tests: taking a device off the
	// network while its daemon keeps running).
	WrapTransport func(transport.Transport) transport.Transport
}

// Daemon is one running device.
type Daemon struct {
	dir    string
	cfg    Config
	opt    Options
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	shutdown     chan struct{} // closed by POST /admin/v1/shutdown (sol stop)
	shutdownOnce sync.Once

	Keyring  *identity.Keyring
	Cache    *identity.Cache
	Store    *feeds.Store
	Contacts *feeds.Contacts
	Shares   *feeds.Shares
	Pins     *feeds.Pins
	Refs     *feeds.BlobRefs
	Blobs    *blob.Store
	Blobnet  *blobnet.Service
	Rep      *replicate.Replicator
	Sessions *session.Manager
	API      *appapi.Server
	Host     *apphost.Host

	trMu       sync.Mutex            // guards transports and cfg.Transports
	transports []transport.Transport // the running transports (transportsNow)
	nodeLn     net.Listener          // where the direct transports forward sessions
	anonLn     net.Listener          // where Tor and I2P forward them (anonymous contacts)
	apiSrv     *http.Server
	uiSrv      *http.Server
	apiURL     string
	uiPort     int
	runDir     string
	tmpRun     bool
	peers      *peerBook
	agents     *agents
	events     *adminEvents
	browser    *browserAuth
	pets       *petnames
	ports      *portShare
	admin      http.Handler
	wg         sync.WaitGroup // background goroutines (the theme watcher, the folder measurement)
	folder     folderSize     // cached size of the synced folder (foldersize.go)

	mu       sync.Mutex
	online   map[string]int       // person ID -> live sessions
	since    map[string]time.Time // device ID -> when its live session came up
	lastSeen map[string]time.Time // device or person ID -> when its last session ended
	settle   *time.Timer          // the second endpoint check after a network change

	netMu sync.Mutex // serializes refreshEndpoints with itself and Close

	privMu sync.Mutex      // serializes privacyChanged
	known  map[string]bool // contacts at the last privacy check
	anon   map[string]bool // of those, the anonymous ones

	tokens     *pairing.Tokens
	accessKick chan struct{} // wakes watchAccess (toraccess.go)
	accessMu   sync.Mutex    // serializes refreshAccess
	lastAccess string        // the client-auth set last published, joined
}

// appsRef lets the API server reach the host, which is built after it.
type appsRef struct{ h *apphost.Host }

// App implements appapi.Apps.
func (a *appsRef) App(id string) (appapi.App, bool) {
	if a.h == nil {
		return appapi.App{}, false
	}
	return a.h.App(id)
}

// List implements appapi.Apps.
func (a *appsRef) List() []appapi.App {
	if a.h == nil {
		return nil
	}
	return a.h.List()
}

// Start brings a device up from its data directory.
func Start(o Options) (_ *Daemon, err error) {
	cfg, err := LoadConfig(o.Dir)
	if err != nil {
		return nil, err
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	kr, err := identity.Open(keysDir(o.Dir))
	if errors.Is(err, identity.ErrNoKeyring) {
		return nil, ErrNoInit
	}
	if err != nil {
		return nil, err
	}
	// The single-instance check: a second daemon on one directory would replace the first's
	// socket and tokens, so an answering one wins and this Start fails.
	if _, err := Running(o.Dir); err == nil {
		return nil, ErrRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Daemon{dir: o.Dir, cfg: cfg, opt: o, log: o.Log, ctx: ctx, cancel: cancel, Keyring: kr,
		Cache: identity.NewCache(), online: map[string]int{}, since: map[string]time.Time{},
		lastSeen: map[string]time.Time{}, events: newAdminEvents(), shutdown: make(chan struct{})}
	defer func() {
		if err != nil {
			d.Close()
		}
	}()
	if d.peers, err = openPeers(filepath.Join(o.Dir, "peers.json")); err != nil {
		return nil, err
	}
	d.peers.seed(kr, d.Cache)
	if d.agents, err = openAgents(filepath.Join(o.Dir, "agents.json")); err != nil {
		return nil, err
	}
	if d.browser, err = openBrowserAuth(filepath.Join(o.Dir, "browser-sessions.json")); err != nil {
		return nil, err
	}
	if d.pets, err = openPetnames(filepath.Join(o.Dir, "petnames.json")); err != nil {
		return nil, err
	}
	if d.Store, err = feeds.Open(feeds.Config{Dir: filepath.Join(o.Dir, "feeds"), Self: kr,
		Lists: feeds.IdentityLists(kr, d.Cache), Log: d.log}); err != nil {
		return nil, err
	}
	d.Contacts = feeds.NewContacts(d.Store)
	if d.Shares, err = feeds.NewShares(d.Store, d.Contacts); err != nil {
		return nil, err
	}
	d.Pins = feeds.NewPins(d.Store)
	if d.Refs, err = feeds.NewBlobRefs(d.Store, d.Shares); err != nil {
		return nil, err
	}
	if d.Blobs, err = blob.New(filepath.Join(o.Dir, "blobs")); err != nil {
		return nil, err
	}
	if d.Rep, err = replicate.New(replicate.Config{Store: d.Store, Shares: d.Shares, Log: d.log,
		Refresh: func(ctx context.Context, p ed25519.PublicKey) error { return d.Sessions.Refresh(ctx, p) }}); err != nil {
		return nil, err
	}
	if d.Blobnet, err = blobnet.New(blobnet.Config{Blobs: d.Blobs, Feeds: d.Store, Refs: d.Refs, Pins: d.Pins,
		Sessions: func() []*session.Session { return d.Sessions.Sessions() }, Anchor: d.anchor, Log: d.log}); err != nil {
		return nil, err
	}
	peerMux := http.NewServeMux()
	peerMux.Handle(replicate.Path, d.Rep)
	peerMux.Handle(blobnet.Path, d.Blobnet)
	peerMux.HandleFunc("GET "+ServicePath+"{name}", func(w http.ResponseWriter, r *http.Request) { d.ports.peerService(w, r) })
	peerMux.HandleFunc("POST "+ServicesPushPath, func(w http.ResponseWriter, r *http.Request) { d.ports.peerPush(w, r) })
	peerMux.HandleFunc("POST "+UnpairPath, d.peerUnpair)
	d.transports = d.newTransports(cfg.Transports)
	d.tokens, d.accessKick = pairing.NewTokens(), make(chan struct{}, 1)
	d.tokens.OnChange = d.kickAccess
	if d.Sessions, err = session.New(session.Config{Keyring: kr, Cache: d.Cache, Contacts: d.Contacts,
		Tokens: d.tokens, Transports: d.transports, Handler: peerMux, OnSession: d.onSession,
		OnBundle: func(b identity.Bundle) { d.peers.put(b); d.kickAccess() }, OnIdentity: d.kickAccess,
		Log: d.log}); err != nil {
		return nil, err
	}
	if d.ports, err = openPortShare(d, filepath.Join(o.Dir, "services.json"), filepath.Join(o.Dir, "tunnels.json")); err != nil {
		return nil, err
	}
	apps := &appsRef{}
	d.admin = d.adminHandler()
	if d.API, err = appapi.New(appapi.Config{Node: appapi.Node{Keyring: kr, Cache: d.Cache, Store: d.Store,
		Contacts: d.Contacts, Shares: d.Shares, Pins: d.Pins, Blobs: d.Blobs, Blobnet: d.Blobnet, Sessions: d.Sessions},
		Apps: apps, DataDir: o.Dir, Admin: d.admin, Agent: d.agentHandler(), Tap: d.tap, Petname: d.pets.get,
		Log: d.log}); err != nil {
		return nil, err
	}
	peerMux.Handle("/p/", d.API.PeerHandler())
	if err := d.startNet(); err != nil {
		return nil, err
	}
	d.peers.keep(kr, d.Cache, d.Contacts, d.Sessions)
	if err := d.startAPI(); err != nil {
		return nil, err
	}
	files := cfg.Files
	if files != "" {
		if err := os.MkdirAll(files, 0o755); err != nil {
			return nil, err
		}
	}
	if d.Host, err = apphost.New(apphost.Config{AppsDir: filepath.Join(o.Dir, "apps"),
		DataDir: filepath.Join(o.Dir, "appdata"), RunDir: filepath.Join(d.runDir, "apps"), Files: files,
		API: d.apiURL, Tokens: d.API, Builtins: o.Builtins, InProcess: o.InProcessApps, Exe: o.Exe,
		Bridge: d.API.Handler(), Me: d.me, Anchor: d.anchor, Root: d.rootHandler(), Peer: d.peerHost, Gate: d.appGate,
		OnChange: d.appChanged, Log: d.log}); err != nil {
		return nil, err
	}
	apps.h = d.Host
	if err := d.startUI(); err != nil {
		return nil, err
	}
	d.ports.start()
	if kr.Person() != nil {
		d.Host.Start(d.ctx)
	}
	d.notePrivacy()
	d.watchThemes()
	if d.cfg.Files != "" {
		d.folder.get(d.cfg.Files, d.ctx, &d.wg) // measure once now; status refreshes it lazily
	}
	d.watchNetwork()
	d.watchContacts()
	d.watchAccess()
	return d, nil
}

// me returns my person and device IDs.
func (d *Daemon) me() (string, string) {
	p := ""
	if pk := d.Keyring.Person(); pk != nil {
		p = identity.ID(pk)
	}
	return p, identity.ID(d.Keyring.DevicePublic())
}

// anchor reports whether this device is an anchor in my device list.
func (d *Daemon) anchor() bool {
	b, err := d.Keyring.Bundle()
	if err != nil {
		return false
	}
	dev, ok := b.Devices.Find(d.Keyring.DevicePublic())
	return ok && dev.Anchor()
}

// newTransports constructs transports from tcs, hooked to this daemon (and wrapped, in tests).
func (d *Daemon) newTransports(tcs []TransportConfig) []transport.Transport {
	ts := newTransports(d.dir, tcs, d.Keyring.Signer(), d.opt.TorProvider, d.cfg.TorBin, d.log)
	for i, t := range ts {
		d.hookTransport(t)
		if d.opt.WrapTransport != nil {
			ts[i] = d.opt.WrapTransport(t)
		}
	}
	return ts
}

// newTransports constructs transports from their configuration; signer is the device key the
// direct transport's TLS certificate is made from, torp runs tor (nil: the platform's, the
// bundled or the system's, solruntime.TorProviders), torBin is the config's tor_bin.
func newTransports(dir string, tcs []TransportConfig, signer crypto.Signer, torp solruntime.Provider, torBin string, log *slog.Logger) []transport.Transport {
	var out []transport.Transport
	for _, tc := range tcs {
		adv := ""
		if len(tc.Advertise) > 0 {
			adv = tc.Advertise[0]
		}
		switch tc.Name {
		case "clearnet":
			out = append(out, transport.NewClearnet(tc.Listen, adv))
		case "direct":
			out = append(out, transport.NewDirect(tc.Listen, signer, tc.Advertise...))
		case "wg":
			out = append(out, transport.NewWG(tc.Listen, filepath.Join(dir, "wg.key"), tc.Advertise, tc.STUN))
		case "tor":
			p, err := torp, error(nil)
			if p == nil {
				// The platform's tor (SOL_TOR_BIN, tor_bin) first, then the one embedded in
				// release builds (-tags bundled), else the system's.
				p, err = solruntime.Select(solruntime.TorProviders, "tor", solruntime.Options{BinDir: filepath.Join(dir, "bin"),
					Platform: solruntime.PlatformPaths(torBin), Log: log})
			}
			if err != nil {
				out = append(out, transport.NewUnavailable("tor", err))
				continue
			}
			out = append(out, transport.NewTor(filepath.Join(dir, "tor"), p))
		default:
			log.Warn("unknown transport in config.json", "name", tc.Name)
		}
	}
	return out
}

// startNet listens for sessions, publishes the listeners on every transport and signs the
// resulting endpoint record (refreshEndpoints keeps it current). Tor starts in the
// background: the record gains the onion once tor has registered it (transports.go).
func (d *Daemon) startNet() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	d.nodeLn = ln
	if d.anonLn, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		return err
	}
	auth, _ := d.accessSet()
	d.lastAccess = strings.Join(auth, ",")
	var eps []identity.Endpoint
	var slow []transport.Transport
	for _, t := range d.transportsNow() {
		if slowStart(t) {
			slow = append(slow, t)
			continue
		}
		if ep, ok := d.startTransport(t); ok {
			eps = append(eps, ep)
		}
	}
	// Keep a known onion until tor says otherwise: peers can still reach it once tor is up.
	for _, e := range d.Keyring.Endpoints().Endpoints {
		if identity.AnonymousTransport(e.Transport) && slices.ContainsFunc(slow, func(t transport.Transport) bool { return t.Name() == e.Transport }) {
			eps = append(eps, e)
		}
	}
	if !slices.Equal(eps, d.Keyring.Endpoints().Endpoints) {
		if _, err := d.Keyring.SetEndpoints(eps, time.Now()); err != nil {
			return err
		}
	}
	go func() { _ = d.Sessions.Serve(ln) }()
	go func() { _ = d.Sessions.ServeAnonymous(d.anonLn) }()
	d.startSlow(slow)
	return nil
}

// runDirFor picks the socket directory: <dir>/run, or a short temporary one when that path
// would be too long for a unix socket (about 104 bytes on macOS).
func (d *Daemon) runDirFor() (string, bool, error) {
	rd := filepath.Join(d.dir, "run")
	if abs, err := filepath.Abs(rd); err == nil {
		rd = abs
	}
	if len(rd) <= 70 {
		return rd, false, os.MkdirAll(rd, 0o700)
	}
	t, err := os.MkdirTemp("", "sol-")
	return t, true, err
}

// startAPI serves the app API and writes api.url and admin.token.
func (d *Daemon) startAPI() error {
	var err error
	if d.runDir, d.tmpRun, err = d.runDirFor(); err != nil {
		return err
	}
	var ln net.Listener
	if d.opt.APIOverTCP || runtime.GOOS == "windows" {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err
		}
		d.apiURL = "http://" + ln.Addr().String()
	} else {
		p := filepath.Join(d.runDir, "api.sock")
		_ = os.Remove(p)
		if ln, err = net.Listen("unix", p); err != nil {
			return err
		}
		_ = os.Chmod(p, 0o600)
		d.apiURL = "unix://" + p
	}
	d.apiSrv = &http.Server{Handler: d.API.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = d.apiSrv.Serve(ln) }()
	if err := fsutil.WriteFileAtomic(filepath.Join(d.dir, "api.url"), []byte(d.apiURL+"\n"), 0o600); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(d.dir, "admin.token"), []byte(d.API.IssueAdmin()+"\n"), 0o600)
}

// startUI serves the Sol app at the UI root and the app origins.
func (d *Daemon) startUI() error {
	addr := d.cfg.UI
	if addr == "" {
		addr = "127.0.0.1:7777"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("daemon: UI address %s: %w", addr, err)
	}
	d.uiPort = ln.Addr().(*net.TCPAddr).Port
	d.uiSrv = &http.Server{Handler: d.Host.UI(d.uiPort), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = d.uiSrv.Serve(ln) }()
	return fsutil.WriteFileAtomic(filepath.Join(d.dir, "ui.url"), []byte(d.UIURL()+"\n"), 0o600)
}

// onSession runs for every session that comes up: blobs and replication start over it, the
// peer's bundle is remembered for restarts, and contacts' online state is pushed to apps.
func (d *Daemon) onSession(s *session.Session) {
	d.Blobnet.SessionUp(s)
	p := s.Peer()
	pid, did := identity.ID(p.Person), identity.ID(p.Device)
	d.mu.Lock()
	d.since[did] = time.Now().UTC()
	d.mu.Unlock()
	up := true
	si := d.liveSessionInfo(s)
	si.Up = &up
	d.emit(EvSession, si)
	d.emitStatus()
	if p.Relation == session.Self {
		d.emit(EvDevice, d.device(did))
	}
	go d.ports.push(s)
	defer func() {
		now := time.Now().UTC()
		d.mu.Lock()
		delete(d.since, did)
		d.lastSeen[did], d.lastSeen[pid] = now, now
		d.mu.Unlock()
		if d.ctx.Err() != nil {
			return
		}
		down := false
		si.Up = &down
		d.emit(EvSession, si)
		d.emitStatus()
		if p.Relation == session.Self {
			d.emit(EvDevice, d.device(did))
		}
	}()
	if p.Relation == session.Contact {
		d.mu.Lock()
		d.online[pid]++
		first := d.online[pid] == 1
		d.mu.Unlock()
		if first {
			d.API.PersonChanged(pid)
		}
		defer func() {
			d.mu.Lock()
			d.online[pid]--
			last := d.online[pid] == 0
			d.mu.Unlock()
			if last && d.ctx.Err() == nil {
				d.API.PersonChanged(pid)
			}
		}()
	}
	d.Rep.Attach(s)
}

// APIURL is where the app API listens (SOL_API form).
func (d *Daemon) APIURL() string { return d.apiURL }

// UIPort is the port of the Sol app and app UIs.
func (d *Daemon) UIPort() int { return d.uiPort }

// UIURL is the Sol app (the UI root).
func (d *Daemon) UIURL() string { return "http://localhost:" + strconv.Itoa(d.uiPort) + "/" }

// Dir is the data directory.
func (d *Daemon) Dir() string { return d.dir }

// PersonID and DeviceID name this device ("" person before it joined one).
func (d *Daemon) PersonID() string { p, _ := d.me(); return p }

// DeviceID is this device's ID.
func (d *Daemon) DeviceID() string { _, dv := d.me(); return dv }

// Invite issues a one-time pair (or, with join, join) link.
func (d *Daemon) Invite(join bool) (string, error) {
	k := pairing.KindPair
	if join {
		k = pairing.KindJoin
	}
	return d.invite(pairing.Token{Kind: k})
}

// InviteAnonymous issues a one-time anonymous pair link: it carries only this device's onion,
// and whoever redeems it becomes an anonymous contact (Tor only, both ways).
func (d *Daemon) InviteAnonymous() (string, error) {
	return d.invite(pairing.Token{Kind: pairing.KindPair, Anonymous: true})
}

func (d *Daemon) invite(tok pairing.Token) (string, error) {
	l, err := d.Sessions.InviteToken(tok)
	if err != nil {
		return "", err
	}
	d.refreshAccess() // the secret opens the onion before anyone has the link
	return l.Encode()
}

// Pair redeems a pair link (adds a person) or a join link (this device joins the person
// that made it; apps start once it has).
func (d *Daemon) Pair(ctx context.Context, link string) (kind string, person string, err error) {
	l, err := pairing.ParseLink(link)
	if err != nil {
		return "", "", err
	}
	switch l.Kind {
	case pairing.KindJoin:
		if d.Keyring.Person() != nil {
			return "", "", errors.New("daemon: this device already belongs to a person")
		}
		if _, err := d.Sessions.Join(ctx, l, d.cfg.DeviceName); err != nil {
			return "", "", err
		}
		d.Host.Start(d.ctx)
	default:
		if d.Keyring.Person() == nil {
			return "", "", identity.ErrNoPerson
		}
		if _, err := d.Sessions.Pair(ctx, l); err != nil {
			return "", "", err
		}
		d.API.PersonChanged(identity.ID(l.Person))
	}
	d.emitStatus()
	return string(l.Kind), identity.ID(l.Person), nil
}

// tap follows the app API's person and inbox events onto the admin stream.
func (d *Daemon) tap(kind, id string) {
	switch kind {
	case sol.EventPerson:
		if p, ok := d.person(id); ok {
			d.emit(EvPerson, p)
		}
	case sol.EventInbox:
		if it, ok := d.API.InboxItem(id); ok {
			d.emit(EvInbox, it)
		}
	}
}

// Close stops everything, in reverse order, and waits for it.
func (d *Daemon) Close() error {
	d.cancel()
	d.wg.Wait()
	d.mu.Lock()
	if d.settle != nil {
		d.settle.Stop()
	}
	d.mu.Unlock()
	d.netMu.Lock() // a running endpoint refresh finishes first; later ones see ctx done
	d.netMu.Unlock()
	if d.events != nil {
		d.events.close()
	}
	if d.ports != nil {
		d.ports.close()
	}
	if d.uiSrv != nil {
		_ = d.uiSrv.Close()
	}
	if d.Host != nil {
		d.Host.Close()
	}
	if d.apiSrv != nil {
		_ = d.apiSrv.Close()
	}
	if d.Sessions != nil {
		_ = d.Sessions.Close()
	}
	if d.nodeLn != nil {
		_ = d.nodeLn.Close()
	}
	if d.anonLn != nil {
		_ = d.anonLn.Close()
	}
	if d.Rep != nil {
		d.Rep.Close()
	}
	if d.Blobnet != nil {
		d.Blobnet.Close()
	}
	for _, t := range d.transportsNow() {
		_ = t.Stop()
	}
	if d.API != nil {
		d.API.Close()
	}
	var err error
	if d.Store != nil {
		err = d.Store.Close()
	}
	if d.tmpRun {
		_ = os.RemoveAll(d.runDir)
	}
	return err
}
