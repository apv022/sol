package identity

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Bundle is what a device presents in a handshake: its person's latest
// person certificate and device list, and its own endpoint record.
type Bundle struct {
	Person    PersonCert
	Devices   DeviceList
	Endpoints EndpointRecord
}

// Marshal returns the wire form: lp(person cert) | lp(device list) |
// lp(endpoint record), each in its own lp(body) | lp(sig) form.
func (b Bundle) Marshal() []byte {
	var e enc
	e.lp(b.Person.Marshal())
	e.lp(b.Devices.Marshal())
	e.lp(b.Endpoints.Marshal())
	return e.b
}

// ParseBundle decodes a wire form. It verifies nothing; use VerifyBundle.
func ParseBundle(raw []byte) (Bundle, error) {
	d := dec{b: raw}
	pc, dl, ep := d.lp(), d.lp(), d.lp()
	if err := d.done(); err != nil {
		return Bundle{}, err
	}
	var b Bundle
	var err error
	if b.Person, err = ParsePersonCert(pc); err != nil {
		return Bundle{}, err
	}
	if b.Devices, err = ParseDeviceList(dl); err != nil {
		return Bundle{}, err
	}
	if b.Endpoints, err = ParseEndpointRecord(ep); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// VerifyBundle checks the whole chain for a handshake from devicePub: R
// signed the person certificate, its P signed the device list for the same
// person, devicePub is in that list, and devicePub signed the endpoint record.
// It returns the device's list entry; the person ID is b.Person.Person.
//
// It cannot detect a replayed older bundle on its own; Cache.Accept does.
func VerifyBundle(b Bundle, devicePub ed25519.PublicKey) (Device, error) {
	if err := b.Person.Verify(); err != nil {
		return Device{}, fmt.Errorf("person certificate: %w", err)
	}
	if !bytes.Equal(b.Devices.Person, b.Person.Person) {
		return Device{}, fmt.Errorf("device list: %w: belongs to another person", ErrInvalid)
	}
	if err := b.Devices.Verify(b.Person.Key); err != nil {
		return Device{}, fmt.Errorf("device list: %w", err)
	}
	dev, ok := b.Devices.Find(devicePub)
	if !ok {
		return Device{}, ErrNotInList
	}
	if !bytes.Equal(b.Endpoints.Device, devicePub) {
		return Device{}, fmt.Errorf("endpoint record: %w: signed by another device", ErrInvalid)
	}
	if err := b.Endpoints.Verify(); err != nil {
		return Device{}, fmt.Errorf("endpoint record: %w", err)
	}
	return dev, nil
}

// VerifyAgent checks an agent certificate at now against a bundle already
// verified with VerifyBundle (or the cached state for that person): the
// certificate is valid, for the same person, and signed by a device in the
// device list.
func VerifyAgent(c AgentCert, b Bundle, now time.Time) error {
	if err := c.Verify(now); err != nil {
		return err
	}
	if !bytes.Equal(c.Person, b.Person.Person) {
		return fmt.Errorf("%w: agent certificate is for another person", ErrInvalid)
	}
	if _, ok := b.Devices.Find(c.Device); !ok {
		return ErrNotInList
	}
	return nil
}

// Errors from the Cache.
var (
	// ErrRollback means a bundle carries an object older than one already
	// seen for the same person or device.
	ErrRollback = errors.New("identity: older than what is already known")
	// ErrConflict means two different objects claim the same seq: the
	// signer equivocated (or a key leaked).
	ErrConflict = errors.New("identity: different object with the same seq")
	// ErrStale means Update was given nothing newer than the cache holds.
	ErrStale = errors.New("identity: nothing newer than what is already known")
)

// Cache keeps the newest verified identity state per person: the person
// certificate, the device list and the endpoint record of each listed device.
// It is safe for concurrent use. It is in memory only; callers persist the
// bundles they need.
//
// Ordering: a person's state is ordered by (person-cert seq, device-list
// seq). A newer person certificate starts a new device-list lineage under the
// new P, so a list issued from the words after every device was lost is
// accepted even if its seq is not above the old one; a list from a
// superseded P never is, because its person certificate is older.
type Cache struct {
	mu sync.Mutex
	m  map[string]*cached
}

type cached struct {
	pc  PersonCert
	dl  DeviceList
	eps map[string]EndpointRecord // by device key bytes
}

// NewCache returns an empty cache.
func NewCache() *Cache { return &Cache{m: make(map[string]*cached)} }

// Accept verifies b for a handshake from devicePub and folds it into the
// cache. A bundle equal to the cached state is fine (the normal case); any
// part older than the cache is ErrRollback, and a different object with an
// equal seq is ErrConflict. It returns the device's entry in the newest list.
func (c *Cache) Accept(b Bundle, devicePub ed25519.PublicKey) (Device, error) {
	dev, _, err := c.fold(b, devicePub)
	return dev, err
}

// Update is Accept for a bundle pushed as an update (GET /p/bundle, or a
// rotation): it refuses with ErrStale unless some part is newer.
func (c *Cache) Update(b Bundle, devicePub ed25519.PublicKey) error {
	_, changed, err := c.fold(b, devicePub)
	if err == nil && !changed {
		return ErrStale
	}
	return err
}

// Fold is Accept that also reports whether anything was newer than the
// cache held (a person certificate, a device list or the device's endpoint
// record), so a caller can pass news on exactly once.
func (c *Cache) Fold(b Bundle, devicePub ed25519.PublicKey) (Device, bool, error) {
	return c.fold(b, devicePub)
}

// Bundles exports the whole cache as bundles: one per device whose endpoint
// record is known, each with its person's newest certificate and list. A
// device relays them to its siblings (session.Manager), and the daemon
// persists them; every one verifies on its own.
func (c *Cache) Bundles() []Bundle {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Bundle
	for _, cur := range c.m {
		for _, ep := range cur.eps {
			out = append(out, Bundle{Person: cur.pc, Devices: cur.dl, Endpoints: ep})
		}
	}
	return out
}

func (c *Cache) fold(b Bundle, devicePub ed25519.PublicKey) (Device, bool, error) {
	dev, err := VerifyBundle(b, devicePub)
	if err != nil {
		return Device{}, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := string(b.Person.Person)
	cur := c.m[key]
	if cur == nil {
		c.m[key] = &cached{pc: b.Person, dl: b.Devices,
			eps: map[string]EndpointRecord{string(devicePub): b.Endpoints}}
		return dev, true, nil
	}
	newer, err := compareIdentity(cur, b)
	if err != nil {
		return Device{}, false, err
	}
	epNewer := true
	if old, ok := cur.eps[string(devicePub)]; ok {
		switch {
		case b.Endpoints.Seq < old.Seq:
			return Device{}, false, fmt.Errorf("endpoint record: %w", ErrRollback)
		case b.Endpoints.Seq == old.Seq:
			if !bytes.Equal(b.Endpoints.Marshal(), old.Marshal()) {
				return Device{}, false, fmt.Errorf("endpoint record: %w", ErrConflict)
			}
			epNewer = false
		}
	}
	if newer {
		cur.pc, cur.dl = b.Person, b.Devices
		for k := range cur.eps { // drop records of devices no longer listed
			if _, ok := cur.dl.Find(ed25519.PublicKey(k)); !ok {
				delete(cur.eps, k)
			}
		}
	}
	if epNewer {
		cur.eps[string(devicePub)] = b.Endpoints
	}
	return dev, newer || epNewer, nil
}

// compareIdentity orders b's person cert and device list against the cached
// ones: newer reports a strictly higher (pc seq, dl seq); equal requires
// identical objects.
func compareIdentity(cur *cached, b Bundle) (newer bool, err error) {
	switch {
	case b.Person.Seq < cur.pc.Seq:
		return false, fmt.Errorf("person certificate: %w", ErrRollback)
	case b.Person.Seq > cur.pc.Seq:
		return true, nil
	}
	if !bytes.Equal(b.Person.Marshal(), cur.pc.Marshal()) {
		return false, fmt.Errorf("person certificate: %w", ErrConflict)
	}
	switch {
	case b.Devices.Seq < cur.dl.Seq:
		return false, fmt.Errorf("device list: %w", ErrRollback)
	case b.Devices.Seq > cur.dl.Seq:
		return true, nil
	}
	if !bytes.Equal(b.Devices.Marshal(), cur.dl.Marshal()) {
		return false, fmt.Errorf("device list: %w", ErrConflict)
	}
	return false, nil
}

// Latest returns the cached person certificate and device list for a person.
func (c *Cache) Latest(person ed25519.PublicKey) (PersonCert, DeviceList, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.m[string(person)]
	if cur == nil {
		return PersonCert{}, DeviceList{}, false
	}
	dl := cur.dl
	dl.Devices = append([]Device(nil), dl.Devices...)
	return cur.pc, dl, true
}

// Endpoints returns the cached endpoint record of one of a person's devices.
func (c *Cache) Endpoints(person, device ed25519.PublicKey) (EndpointRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.m[string(person)]
	if cur == nil {
		return EndpointRecord{}, false
	}
	r, ok := cur.eps[string(device)]
	return r, ok
}

// Forget drops everything cached about a person (after unpairing).
func (c *Cache) Forget(person ed25519.PublicKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, string(person))
}
