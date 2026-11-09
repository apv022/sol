// Package config loads, validates, and atomically saves the node's
// configuration file (~/.sol/config.json).
//
// The Config value is the single source of truth for the node's name,
// services, and peers. All access goes through methods that hold an internal
// mutex, and every read accessor returns a copy so callers can never mutate
// shared state by accident.
//
// Invariants:
//   - The file is written atomically (temp file, fsync, rename) with mode 0600.
//   - A missing file is not an error: Load writes and returns Default().
//   - Fields added in later versions get defaults via applyDefaults, and the
//     Version field plus migrate() handle breaking changes.
//
// See docs/spec.md section 6.6 and docs/PEERS.md.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

// CurrentVersion is the config schema version written by this build.
// Bump it and extend migrate() when the on-disk format changes incompatibly.
//
// Version 2 replaced one-way "devices" with mutual "peers" (docs/PEERS.md).
const CurrentVersion = 2

// Visibility modes for a service (spec.md section 4).
const (
	VisibilityPrivate         = "private"          // peers it is shared with, via the node onion + handshake
	VisibilityPublic          = "public"           // anyone, own onion, no auth
	VisibilityAnonymousPublic = "anonymous-public" // anyone, own onion, leak protections enforced (roadmap)
)

// Service protocols.
const (
	ProtocolTCP  = "tcp"  // raw byte pipe (default; works for HTTP too)
	ProtocolHTTP = "http" // HTTP-aware proxy, enables header stripping for public services
)

// Known runtime providers, in the default try order.
var knownProviders = []string{"bundled", "native", "docker"}

var serviceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Errors returned by mutators. Callers can match them with errors.Is.
var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("already exists")
)

// Service is one local app the node exposes.
type Service struct {
	Name       string   `json:"name"`                 // unique, [a-z0-9-]{1,32}
	LocalPort  int      `json:"local_port"`           // app listens on 127.0.0.1:<LocalPort>
	Visibility string   `json:"visibility"`           // one of the Visibility* constants
	Protocol   string   `json:"protocol,omitempty"`   // ProtocolTCP (default) or ProtocolHTTP
	Transports []string `json:"transports,omitempty"` // empty means every enabled transport
}

// Tunnel is a peer's service that this node keeps reachable on
// 127.0.0.1:Port (docs/PEERS.md, "Tunnels"). Tunnels are saved so they come
// back on the same port after a restart, and bookmarks keep working.
type Tunnel struct {
	Peer    string `json:"peer"`    // peer ID
	Service string `json:"service"` // the peer's service name
	Port    int    `json:"port"`    // local port on 127.0.0.1
}

// RemoteService is one service a peer shares with us.
type RemoteService struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
}

// Peer is another Sol node we are paired with (docs/PEERS.md). Pairing is
// mutual: each side can reach the other, and each decides what it shares.
//
// Everything transport-specific is a map keyed by transport name ("tor",
// "clearnet", later "i2p"), so a new transport adds keys, not fields.
type Peer struct {
	ID     string `json:"id"`      // identity.DeviceID(pubkey): short fingerprint for the UI and API
	NodeID string `json:"node_id"` // identity.NodeID(pubkey)
	Name   string `json:"name"`    // the peer's own node name
	PubKey string `json:"pubkey"`  // base64 ed25519 public key

	// Addrs is how to reach the peer, per transport. Empty for a peer that
	// publishes nothing (it can still dial us).
	Addrs map[string]string `json:"addrs,omitempty"`
	// AccessIn is the credential we issued to the peer, per transport (for
	// Tor: the x25519 public key we authorize on our onion).
	AccessIn map[string]string `json:"access_in,omitempty"`
	// AccessOut is the credential the peer issued to us, per transport (for
	// Tor: the x25519 private key we install in our tor). It is a secret.
	AccessOut map[string]string `json:"access_out,omitempty"`

	// Shares lists the services we let this peer open (built-ins included).
	Shares []string `json:"shares"`
	// Services is what the peer shares with us, as of the last list.
	Services []RemoteService `json:"services"`

	Added    time.Time `json:"added"`
	LastSeen time.Time `json:"last_seen"`
}

// Clone returns a deep copy, so callers can't mutate shared maps or slices.
func (p Peer) Clone() Peer {
	p.Addrs = maps.Clone(p.Addrs)
	p.AccessIn = maps.Clone(p.AccessIn)
	p.AccessOut = maps.Clone(p.AccessOut)
	p.Shares = slices.Clone(p.Shares)
	p.Services = slices.Clone(p.Services)
	return p
}

// Shared reports whether service is in p.Shares.
func (p Peer) Shared(service string) bool { return slices.Contains(p.Shares, service) }

// TransportsConfig toggles each transport.
type TransportsConfig struct {
	Tor      bool `json:"tor"`
	I2P      bool `json:"i2p"`
	Clearnet bool `json:"clearnet"`
	// ClearnetListen is the address the clearnet transport listens on when
	// enabled (LAN/testing only; clearnet is not encrypted or anonymous).
	ClearnetListen string `json:"clearnet_listen,omitempty"`
	// ClearnetAdvertise overrides the host:port put in pairing links.
	ClearnetAdvertise string `json:"clearnet_advertise,omitempty"`
	// Direct enables direct mode: peers connect straight to this machine's
	// IP addresses (TLS pinned to the node key) when they can, and over Tor
	// when they can't. Off by default because it shows peers your IP.
	Direct bool `json:"direct"`
	// DirectListen is the direct-mode listen address (default ":7701").
	DirectListen string `json:"direct_listen,omitempty"`
	// DirectAdvertise replaces the advertised IP:port list, e.g. with a
	// port forwarded on the router ("203.0.113.7:7701"). IP addresses only:
	// peers never dial names an address list gives them. Loopback
	// candidates are dialled only by a node whose own direct_listen is on
	// loopback (two nodes on one machine: put both on 127.0.0.1).
	DirectAdvertise []string `json:"direct_advertise,omitempty"`
}

// RuntimeConfig controls how tor/i2pd are obtained (spec.md 6.2.1).
type RuntimeConfig struct {
	ProviderOrder    []string `json:"provider_order"`    // subset of bundled, native, docker
	ContainerRuntime string   `json:"container_runtime"` // "docker" or "podman"
}

// Config is the whole config file. Use the methods; do not touch fields of a
// Config that other goroutines may be using.
type Config struct {
	Version    int              `json:"version"`
	Name       string           `json:"name"`       // this node's name, shown to peers
	PanelPort  int              `json:"panel_port"` // control panel, always bound to 127.0.0.1
	NodePort   int              `json:"node_port"`  // node listener that transports forward to (127.0.0.1)
	Transports TransportsConfig `json:"transports"`
	Runtime    RuntimeConfig    `json:"runtime"`
	Services   []Service        `json:"services"`
	Peers      []Peer           `json:"peers"`
	Tunnels    []Tunnel         `json:"tunnels"`

	mu   sync.Mutex
	path string // empty for in-memory configs (tests); Save is then a no-op
}

// Default returns a first-run configuration with no services or peers.
// Tor is the only transport enabled by default.
func Default() *Config {
	return &Config{
		Version:   CurrentVersion,
		Name:      defaultName(),
		PanelPort: 8080,
		NodePort:  8081,
		Transports: TransportsConfig{
			Tor:            true,
			ClearnetListen: "0.0.0.0:7700",
			DirectListen:   ":7701",
		},
		Runtime: RuntimeConfig{
			ProviderOrder:    slices.Clone(knownProviders),
			ContainerRuntime: "docker",
		},
		Services: []Service{},
		Peers:    []Peer{},
		Tunnels:  []Tunnel{},
	}
}

// defaultName is the machine's hostname if that is a valid name.
func defaultName() string {
	h, _ := os.Hostname()
	if ValidateName(h) != nil {
		return "sol"
	}
	return h
}

// ValidateName checks a human node name: 1-64 printable characters.
func ValidateName(name string) error {
	if name == "" || len(name) > 64 {
		return errors.New("name must be 1-64 characters")
	}
	if strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return errors.New("name contains unprintable characters")
	}
	return nil
}

// Load reads the config at path. If the file does not exist it creates the
// parent directory (0700), writes Default() (0600), and returns it.
func Load(path string) (*Config, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating config dir: %w", err)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c := Default()
		c.path = path
		if err := c.Save(); err != nil {
			return nil, err
		}
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	c := &Config{}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	c.path = path
	migrated, err := c.migrate()
	if err != nil {
		return nil, err
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", path, err)
	}
	if migrated {
		if err := c.Save(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// migrate upgrades older on-disk formats in place and reports whether it
// changed anything.
func (c *Config) migrate() (bool, error) {
	switch {
	case c.Version > CurrentVersion:
		return false, fmt.Errorf("config version %d is newer than this build supports (%d); upgrade sol", c.Version, CurrentVersion)
	case c.Version < CurrentVersion:
		// Add per-version upgrade steps here, oldest first.
		// 0/1 -> 2: one-way "devices" have no address to reach them at, so
		// they can't become peers. The field is no longer read; it is dropped
		// on the next save, and machines re-pair (docs/PEERS.md).
		c.Version = CurrentVersion
		return true, nil
	}
	return false, nil
}

// applyDefaults fills zero-valued fields with defaults so that config files
// from older builds keep working when new fields are added.
func (c *Config) applyDefaults() {
	d := Default()
	if c.Name == "" {
		c.Name = d.Name
	}
	if c.PanelPort == 0 {
		c.PanelPort = d.PanelPort
	}
	if c.NodePort == 0 {
		c.NodePort = d.NodePort
	}
	if c.Transports.ClearnetListen == "" {
		c.Transports.ClearnetListen = d.Transports.ClearnetListen
	}
	if c.Transports.DirectListen == "" {
		c.Transports.DirectListen = d.Transports.DirectListen
	}
	if len(c.Runtime.ProviderOrder) == 0 {
		c.Runtime.ProviderOrder = d.Runtime.ProviderOrder
	}
	if c.Runtime.ContainerRuntime == "" {
		c.Runtime.ContainerRuntime = d.Runtime.ContainerRuntime
	}
	if c.Services == nil {
		c.Services = []Service{}
	}
	if c.Peers == nil {
		c.Peers = []Peer{}
	}
	if c.Tunnels == nil {
		c.Tunnels = []Tunnel{}
	}
	for i := range c.Services {
		if c.Services[i].Protocol == "" {
			c.Services[i].Protocol = ProtocolTCP
		}
	}
	for i := range c.Peers {
		if c.Peers[i].Shares == nil {
			c.Peers[i].Shares = []string{}
		}
		if c.Peers[i].Services == nil {
			c.Peers[i].Services = []RemoteService{}
		}
	}
}

// Path returns the file this config is bound to ("" for in-memory configs).
func (c *Config) Path() string { return c.path }

// Save writes the config atomically.
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
}

func (c *Config) saveLocked() error {
	if c.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	return WriteFileAtomic(c.path, append(data, '\n'), 0o600)
}

// WriteFileAtomic writes data to path via a temp file, fsync, and rename, so
// readers never observe a partially written file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Validate checks every field. It is called by Load and by every mutator.
func (c *Config) Validate() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.validateLocked()
}

func (c *Config) validateLocked() error {
	var errs []error
	if err := ValidateName(c.Name); err != nil {
		errs = append(errs, fmt.Errorf("node %w", err))
	}
	for _, p := range []int{c.PanelPort, c.NodePort} {
		if p < 0 || p > 65535 {
			errs = append(errs, fmt.Errorf("port %d out of range", p))
		}
	}
	if c.PanelPort != 0 && c.PanelPort == c.NodePort {
		errs = append(errs, errors.New("panel_port and node_port must differ"))
	}
	names := map[string]bool{}
	for _, s := range c.Services {
		if err := validateService(s); err != nil {
			errs = append(errs, err)
		}
		if names[s.Name] {
			errs = append(errs, fmt.Errorf("service %q: %w", s.Name, ErrDuplicate))
		}
		names[s.Name] = true
	}
	ids := map[string]bool{}
	for _, p := range c.Peers {
		if p.ID == "" || p.PubKey == "" || p.NodeID == "" {
			errs = append(errs, fmt.Errorf("peer %q: missing id, node_id or pubkey", p.Name))
		}
		if err := ValidateName(p.Name); err != nil {
			errs = append(errs, fmt.Errorf("peer %q: %w", p.ID, err))
		}
		if ids[p.ID] {
			errs = append(errs, fmt.Errorf("peer %q: %w", p.ID, ErrDuplicate))
		}
		ids[p.ID] = true
	}
	for _, a := range c.Transports.DirectAdvertise {
		host, _, err := net.SplitHostPort(a)
		if err != nil || net.ParseIP(host) == nil {
			errs = append(errs, fmt.Errorf("direct_advertise %q: must be IP:port (peers dial only IP addresses, not names)", a))
		}
	}
	ports := map[int]bool{}
	for _, t := range c.Tunnels {
		if t.Port < 1 || t.Port > 65535 {
			errs = append(errs, fmt.Errorf("tunnel %s/%s: port %d out of range", t.Peer, t.Service, t.Port))
		}
		if ports[t.Port] {
			errs = append(errs, fmt.Errorf("tunnel port %d: %w", t.Port, ErrDuplicate))
		}
		ports[t.Port] = true
		if !ids[t.Peer] {
			errs = append(errs, fmt.Errorf("tunnel %s/%s: no such peer", t.Peer, t.Service))
		}
	}
	for _, p := range c.Runtime.ProviderOrder {
		if !slices.Contains(knownProviders, p) {
			errs = append(errs, fmt.Errorf("unknown runtime provider %q", p))
		}
	}
	return errors.Join(errs...)
}

func validateService(s Service) error {
	if !serviceNameRE.MatchString(s.Name) {
		return fmt.Errorf("service name %q must match [a-z0-9][a-z0-9-]{0,31}", s.Name)
	}
	if s.LocalPort < 1 || s.LocalPort > 65535 {
		return fmt.Errorf("service %q: local_port %d out of range", s.Name, s.LocalPort)
	}
	switch s.Visibility {
	case VisibilityPrivate, VisibilityPublic, VisibilityAnonymousPublic:
	default:
		return fmt.Errorf("service %q: unknown visibility %q", s.Name, s.Visibility)
	}
	switch s.Protocol {
	case ProtocolTCP, ProtocolHTTP:
	default:
		return fmt.Errorf("service %q: unknown protocol %q", s.Name, s.Protocol)
	}
	return nil
}

// mutate runs fn under the lock, validates the result, and saves. If
// validation or saving fails, the in-memory state is rolled back.
func (c *Config) mutate(fn func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := c.Name
	services := slices.Clone(c.Services)
	peers := make([]Peer, len(c.Peers))
	for i, p := range c.Peers {
		peers[i] = p.Clone()
	}
	tunnels := slices.Clone(c.Tunnels)
	rollback := func() { c.Name, c.Services, c.Peers, c.Tunnels = name, services, peers, tunnels }
	if err := fn(); err != nil {
		rollback()
		return err
	}
	if err := c.validateLocked(); err != nil {
		rollback()
		return err
	}
	if err := c.saveLocked(); err != nil {
		rollback()
		return err
	}
	return nil
}

// NodeName returns this node's name.
func (c *Config) NodeName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Name
}

// SetName changes this node's name and saves.
func (c *Config) SetName(name string) error {
	return c.mutate(func() error { c.Name = name; return nil })
}

// AddService appends a service (defaulting Protocol to tcp) and saves.
func (c *Config) AddService(s Service) error {
	if s.Protocol == "" {
		s.Protocol = ProtocolTCP
	}
	return c.mutate(func() error {
		if slices.ContainsFunc(c.Services, func(x Service) bool { return x.Name == s.Name }) {
			return fmt.Errorf("service %q: %w", s.Name, ErrDuplicate)
		}
		c.Services = append(c.Services, s)
		return nil
	})
}

// RemoveService deletes the named service, drops it from every peer's
// shares, and saves.
func (c *Config) RemoveService(name string) error {
	return c.mutate(func() error {
		i := slices.IndexFunc(c.Services, func(x Service) bool { return x.Name == name })
		if i < 0 {
			return fmt.Errorf("service %q: %w", name, ErrNotFound)
		}
		c.Services = slices.Delete(c.Services, i, i+1)
		for j := range c.Peers {
			c.Peers[j].Shares = slices.DeleteFunc(c.Peers[j].Shares, func(s string) bool { return s == name })
		}
		return nil
	})
}

// SetServiceVisibility changes one service's visibility and saves.
func (c *Config) SetServiceVisibility(name, visibility string) error {
	return c.mutate(func() error {
		i := slices.IndexFunc(c.Services, func(x Service) bool { return x.Name == name })
		if i < 0 {
			return fmt.Errorf("service %q: %w", name, ErrNotFound)
		}
		c.Services[i].Visibility = visibility
		return nil
	})
}

// ServiceList returns a copy of all services.
func (c *Config) ServiceList() []Service {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Service, len(c.Services))
	for i, s := range c.Services {
		s.Transports = slices.Clone(s.Transports)
		out[i] = s
	}
	return out
}

// Service returns one service by name.
func (c *Config) Service(name string) (Service, bool) {
	for _, s := range c.ServiceList() {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// AddPeer stores a new peer record and saves.
func (c *Config) AddPeer(p Peer) error {
	return c.mutate(func() error {
		if slices.ContainsFunc(c.Peers, func(x Peer) bool { return x.ID == p.ID }) {
			return fmt.Errorf("peer %q: %w", p.ID, ErrDuplicate)
		}
		c.Peers = append(c.Peers, p.Clone())
		return nil
	})
}

// DeletePeer removes a peer by ID, saves, and returns the removed record.
func (c *Config) DeletePeer(id string) (Peer, error) {
	var removed Peer
	err := c.mutate(func() error {
		i := slices.IndexFunc(c.Peers, func(x Peer) bool { return x.ID == id })
		if i < 0 {
			return fmt.Errorf("peer %q: %w", id, ErrNotFound)
		}
		removed = c.Peers[i].Clone()
		c.Peers = slices.Delete(c.Peers, i, i+1)
		c.Tunnels = slices.DeleteFunc(c.Tunnels, func(t Tunnel) bool { return t.Peer == id })
		return nil
	})
	return removed, err
}

// UpdatePeer applies fn to a copy of the stored peer and saves the result.
// The change is kept only if fn, validation, and saving all succeed.
func (c *Config) UpdatePeer(id string, fn func(*Peer) error) (Peer, error) {
	var out Peer
	err := c.mutate(func() error {
		i := slices.IndexFunc(c.Peers, func(x Peer) bool { return x.ID == id })
		if i < 0 {
			return fmt.Errorf("peer %q: %w", id, ErrNotFound)
		}
		p := c.Peers[i].Clone()
		if err := fn(&p); err != nil {
			return err
		}
		c.Peers[i] = p
		out = p.Clone()
		return nil
	})
	return out, err
}

// Peer returns one peer by ID.
func (c *Config) Peer(id string) (Peer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.Peers {
		if p.ID == id {
			return p.Clone(), true
		}
	}
	return Peer{}, false
}

// PeerList returns a copy of all peers.
func (c *Config) PeerList() []Peer {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Peer, len(c.Peers))
	for i, p := range c.Peers {
		out[i] = p.Clone()
	}
	return out
}

// SetPeerLastSeen updates LastSeen in memory. If persist is true the config
// is also saved. Unknown IDs are ignored.
func (c *Config) SetPeerLastSeen(id string, t time.Time, persist bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.Peers {
		if c.Peers[i].ID == id {
			c.Peers[i].LastSeen = t
			if persist {
				return c.saveLocked()
			}
			return nil
		}
	}
	return nil
}

// Snapshot returns a deep copy of the exported settings (for read-only use
// such as the audit package). The copy is not bound to a file.
func (c *Config) Snapshot() *Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := &Config{
		Version:    c.Version,
		Name:       c.Name,
		PanelPort:  c.PanelPort,
		NodePort:   c.NodePort,
		Transports: c.Transports,
		Runtime:    RuntimeConfig{ProviderOrder: slices.Clone(c.Runtime.ProviderOrder), ContainerRuntime: c.Runtime.ContainerRuntime},
		Services:   slices.Clone(c.Services),
		Peers:      make([]Peer, 0, len(c.Peers)),
		Tunnels:    slices.Clone(c.Tunnels),
	}
	for _, p := range c.Peers {
		cp.Peers = append(cp.Peers, p.Clone())
	}
	return cp
}

// TunnelList returns a copy of the saved tunnels.
func (c *Config) TunnelList() []Tunnel {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.Tunnels)
}

// AddTunnel saves a tunnel. A tunnel for the same peer and service, or one
// on the same port, is a duplicate.
func (c *Config) AddTunnel(t Tunnel) error {
	return c.mutate(func() error {
		if slices.ContainsFunc(c.Tunnels, func(x Tunnel) bool {
			return x.Peer == t.Peer && x.Service == t.Service || x.Port == t.Port
		}) {
			return fmt.Errorf("tunnel %s/%s: %w", t.Peer, t.Service, ErrDuplicate)
		}
		c.Tunnels = append(c.Tunnels, t)
		return nil
	})
}

// RemoveTunnel forgets the tunnel to peer's service.
func (c *Config) RemoveTunnel(peer, service string) error {
	return c.mutate(func() error {
		i := slices.IndexFunc(c.Tunnels, func(x Tunnel) bool { return x.Peer == peer && x.Service == service })
		if i < 0 {
			return fmt.Errorf("tunnel %s/%s: %w", peer, service, ErrNotFound)
		}
		c.Tunnels = slices.Delete(c.Tunnels, i, i+1)
		return nil
	})
}
