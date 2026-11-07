// Package auth manages the list of paired devices: the set of ed25519 keys
// allowed to reach private services.
//
// Devices is a thin, concurrency-safe layer over config.Config that adds
// validation, rate-limited LastSeen persistence, and change hooks. The node
// uses the hooks to (a) close a revoked device's live connections and (b)
// refresh Tor client authorization so revocation also applies at the network
// layer (spec.md 7.1).
package auth

import (
	"crypto/ed25519"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/identity"
)

// lastSeenPersistEvery limits how often LastSeen is written to disk per device.
const lastSeenPersistEvery = time.Minute

// Devices manages paired devices. Safe for concurrent use.
type Devices struct {
	cfg *config.Config

	mu          sync.Mutex
	lastPersist map[string]time.Time
	onRemove    []func(config.Device)
	onChange    []func()
}

// NewDevices wraps cfg.
func NewDevices(cfg *config.Config) *Devices {
	return &Devices{cfg: cfg, lastPersist: map[string]time.Time{}}
}

// OnRemove registers fn to run after a device is removed and saved.
func (d *Devices) OnRemove(fn func(config.Device)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onRemove = append(d.onRemove, fn)
}

// OnChange registers fn to run after any device is added or removed.
func (d *Devices) OnChange(fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onChange = append(d.onChange, fn)
}

// ValidateName checks a human device label: 1-64 printable characters.
func ValidateName(name string) error {
	if name == "" || len(name) > 64 {
		return errors.New("device name must be 1-64 characters")
	}
	if strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return errors.New("device name contains unprintable characters")
	}
	return nil
}

// Add authorizes a new device. torAuthPub is the base32 x25519 key used for
// Tor client authorization ("" if none).
func (d *Devices) Add(name string, pub ed25519.PublicKey, torAuthPub, kind string) (config.Device, error) {
	if err := ValidateName(name); err != nil {
		return config.Device{}, err
	}
	if len(pub) != ed25519.PublicKeySize {
		return config.Device{}, errors.New("invalid public key")
	}
	if kind == "" {
		kind = config.DeviceKindDevice
	}
	now := time.Now().UTC()
	dev := config.Device{
		ID:         identity.DeviceID(pub),
		Name:       name,
		PubKey:     identity.EncodePublicKey(pub),
		TorAuthPub: torAuthPub,
		Kind:       kind,
		Added:      now,
		LastSeen:   now,
	}
	if err := d.cfg.AddDevice(dev); err != nil {
		return config.Device{}, err
	}
	d.fireChange()
	return dev, nil
}

// Remove revokes a device immediately.
func (d *Devices) Remove(id string) error {
	dev, err := d.cfg.DeleteDevice(id)
	if err != nil {
		return err
	}
	d.mu.Lock()
	delete(d.lastPersist, id)
	hooks := append([]func(config.Device){}, d.onRemove...)
	d.mu.Unlock()
	for _, fn := range hooks {
		fn(dev)
	}
	d.fireChange()
	return nil
}

func (d *Devices) fireChange() {
	d.mu.Lock()
	hooks := append([]func(){}, d.onChange...)
	d.mu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// Lookup returns the device that owns pub, if it is paired. This runs on
// every incoming connection.
func (d *Devices) Lookup(pub ed25519.PublicKey) (config.Device, bool) {
	if len(pub) != ed25519.PublicKeySize {
		return config.Device{}, false
	}
	dev, ok := d.cfg.Device(identity.DeviceID(pub))
	if !ok {
		return config.Device{}, false
	}
	// The ID is a hash prefix, so confirm the full key matches.
	stored, err := identity.DecodePublicKey(dev.PubKey)
	if err != nil || subtle.ConstantTimeCompare(stored, pub) != 1 {
		return config.Device{}, false
	}
	return dev, true
}

// IsAllowed reports whether pub belongs to a paired device.
func (d *Devices) IsAllowed(pub ed25519.PublicKey) bool {
	_, ok := d.Lookup(pub)
	return ok
}

// List returns all devices.
func (d *Devices) List() []config.Device { return d.cfg.DeviceList() }

// Touch records that a device just connected. It is persisted at most once
// per minute per device; save errors are ignored (LastSeen is cosmetic).
func (d *Devices) Touch(id string) {
	now := time.Now().UTC()
	d.mu.Lock()
	persist := now.Sub(d.lastPersist[id]) >= lastSeenPersistEvery
	if persist {
		d.lastPersist[id] = now
	}
	d.mu.Unlock()
	_ = d.cfg.SetDeviceLastSeen(id, now, persist)
}

// TorAuthKeys returns the Tor client-auth public keys of all paired devices.
func (d *Devices) TorAuthKeys() []string {
	var keys []string
	for _, dev := range d.List() {
		if dev.TorAuthPub != "" {
			keys = append(keys, dev.TorAuthPub)
		}
	}
	return keys
}

// String is used in log messages: "name (id)".
func Describe(dev config.Device) string { return fmt.Sprintf("%s (%s)", dev.Name, dev.ID) }
