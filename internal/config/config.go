// Package config loads, validates, and atomically saves the node's
// configuration file (~/.sol/config.json).
//
// The Config value is the single source of truth for services and paired
// devices. All access goes through methods that hold an internal mutex, and
// every read accessor returns a copy so callers can never mutate shared state
// by accident.
//
// Invariants:
//   - The file is written atomically (temp file, fsync, rename) with mode 0600.
//   - A missing file is not an error: Load writes and returns Default().
//   - Fields added in later versions get defaults via applyDefaults, and the
//     Version field plus migrate() handle breaking changes.
//
// See docs/spec.md section 6.6.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"
)

// CurrentVersion is the config schema version written by this build.
// Bump it and extend migrate() when the on-disk format changes incompatibly.
const CurrentVersion = 1

// Visibility modes for a service (spec.md section 4).
const (
	VisibilityPrivate         = "private"          // paired devices only, via the node onion + handshake
	VisibilityPublic          = "public"           // anyone, own onion, no auth
	VisibilityAnonymousPublic = "anonymous-public" // anyone, own onion, leak protections enforced (roadmap)
)

// Service protocols.
const (
	ProtocolTCP  = "tcp"  // raw byte pipe (default; works for HTTP too)
	ProtocolHTTP = "http" // HTTP-aware proxy, enables header stripping for public services
)

// Device kinds.
const (
	DeviceKindDevice         = "device"
	DeviceKindGatewaySession = "gateway-session" // v0.2: one per browser behind a gateway
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

// Device is one paired client allowed to reach private services.
type Device struct {
	ID         string    `json:"id"`                     // identity.DeviceID(pubkey)
	Name       string    `json:"name"`                   // human label chosen at pairing time
	PubKey     string    `json:"pubkey"`                 // base64 ed25519 public key
	TorAuthPub string    `json:"tor_auth_pub,omitempty"` // base32 x25519 key for Tor client auth
	Kind       string    `json:"kind"`                   // DeviceKind* constant
	Added      time.Time `json:"added"`
	LastSeen   time.Time `json:"last_seen"`
}

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
	PanelPort  int              `json:"panel_port"` // control panel, always bound to 127.0.0.1
	NodePort   int              `json:"node_port"`  // node listener that transports forward to (127.0.0.1)
	Transports TransportsConfig `json:"transports"`
	Runtime    RuntimeConfig    `json:"runtime"`
	Services   []Service        `json:"services"`
	Devices    []Device         `json:"devices"`

	mu   sync.Mutex
	path string // empty for in-memory configs (tests); Save is then a no-op
}

// Default returns a first-run configuration with no services or devices.
// Tor is the only transport enabled by default.
func Default() *Config {
	return &Config{
		Version:   CurrentVersion,
		PanelPort: 8080,
		NodePort:  8081,
		Transports: TransportsConfig{
			Tor:            true,
			ClearnetListen: "0.0.0.0:7700",
		},
		Runtime: RuntimeConfig{
			ProviderOrder:    slices.Clone(knownProviders),
			ContainerRuntime: "docker",
		},
		Services: []Service{},
		Devices:  []Device{},
	}
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
	if err := c.migrate(); err != nil {
		return nil, err
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", path, err)
	}
	return c, nil
}

// migrate upgrades older on-disk formats in place. Version 0 means a file
// written by the pre-release skeleton, which is compatible with version 1.
func (c *Config) migrate() error {
	switch {
	case c.Version > CurrentVersion:
		return fmt.Errorf("config version %d is newer than this build supports (%d); upgrade sol", c.Version, CurrentVersion)
	case c.Version < CurrentVersion:
		// Add per-version upgrade steps here, oldest first.
		c.Version = CurrentVersion
	}
	return nil
}

// applyDefaults fills zero-valued fields with defaults so that config files
// from older builds keep working when new fields are added.
func (c *Config) applyDefaults() {
	d := Default()
	if c.PanelPort == 0 {
		c.PanelPort = d.PanelPort
	}
	if c.NodePort == 0 {
		c.NodePort = d.NodePort
	}
	if c.Transports.ClearnetListen == "" {
		c.Transports.ClearnetListen = d.Transports.ClearnetListen
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
	if c.Devices == nil {
		c.Devices = []Device{}
	}
	for i := range c.Services {
		if c.Services[i].Protocol == "" {
			c.Services[i].Protocol = ProtocolTCP
		}
	}
	for i := range c.Devices {
		if c.Devices[i].Kind == "" {
			c.Devices[i].Kind = DeviceKindDevice
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
	for _, d := range c.Devices {
		if d.ID == "" || d.PubKey == "" {
			errs = append(errs, fmt.Errorf("device %q: missing id or pubkey", d.Name))
		}
		if ids[d.ID] {
			errs = append(errs, fmt.Errorf("device %q: %w", d.ID, ErrDuplicate))
		}
		ids[d.ID] = true
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
	services := slices.Clone(c.Services)
	devices := slices.Clone(c.Devices)
	rollback := func() { c.Services, c.Devices = services, devices }
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

// RemoveService deletes the named service and saves.
func (c *Config) RemoveService(name string) error {
	return c.mutate(func() error {
		i := slices.IndexFunc(c.Services, func(x Service) bool { return x.Name == name })
		if i < 0 {
			return fmt.Errorf("service %q: %w", name, ErrNotFound)
		}
		c.Services = slices.Delete(c.Services, i, i+1)
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

// AddDevice stores a new device record and saves.
func (c *Config) AddDevice(d Device) error {
	return c.mutate(func() error {
		if slices.ContainsFunc(c.Devices, func(x Device) bool { return x.ID == d.ID }) {
			return fmt.Errorf("device %q: %w", d.ID, ErrDuplicate)
		}
		c.Devices = append(c.Devices, d)
		return nil
	})
}

// DeleteDevice removes a device by ID, saves, and returns the removed record.
func (c *Config) DeleteDevice(id string) (Device, error) {
	var removed Device
	err := c.mutate(func() error {
		i := slices.IndexFunc(c.Devices, func(x Device) bool { return x.ID == id })
		if i < 0 {
			return fmt.Errorf("device %q: %w", id, ErrNotFound)
		}
		removed = c.Devices[i]
		c.Devices = slices.Delete(c.Devices, i, i+1)
		return nil
	})
	return removed, err
}

// Device returns one device by ID.
func (c *Config) Device(id string) (Device, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.Devices {
		if d.ID == id {
			return d, true
		}
	}
	return Device{}, false
}

// DeviceList returns a copy of all devices.
func (c *Config) DeviceList() []Device {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.Devices)
}

// SetDeviceLastSeen updates LastSeen in memory. If persist is true the config
// is also saved. Unknown IDs are ignored.
func (c *Config) SetDeviceLastSeen(id string, t time.Time, persist bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.Devices {
		if c.Devices[i].ID == id {
			c.Devices[i].LastSeen = t
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
		PanelPort:  c.PanelPort,
		NodePort:   c.NodePort,
		Transports: c.Transports,
		Runtime:    RuntimeConfig{ProviderOrder: slices.Clone(c.Runtime.ProviderOrder), ContainerRuntime: c.Runtime.ContainerRuntime},
		Services:   slices.Clone(c.Services),
		Devices:    slices.Clone(c.Devices),
	}
	return cp
}
