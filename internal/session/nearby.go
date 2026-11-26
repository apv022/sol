package session

// Nearby hints (design/PROTOCOL.md §Nearby): a beacon recognised on the LAN says where a kept
// device is right now. The address is unsigned, so it only joins the candidates of the
// device's newest signed endpoint record for a while (the record's keys stay, and the
// handshake authenticates whoever answers); the device's keeper is woken so it redials, or
// moves a session on a worse path (Tor) to the LAN, through the usual ranking and probation.
// Anonymous contacts never get here: they are not offered to the beacon (NearbyPeers), and a
// hint for one is ignored.

import (
	"crypto/ed25519"
	"maps"
	"slices"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/transport"
)

// NearbyTTL is how long an address learned from a beacon is used; beacons renew it.
var NearbyTTL = 3 * time.Minute

// nearbyHint is where a beacon said a device is: transport name -> host:port.
type nearbyHint struct {
	addrs map[string]string
	at    time.Time
}

// NearbyPeer is a device this device keeps a session with and that may recognise it on a LAN.
type NearbyPeer struct {
	Person, Device ed25519.PublicKey
	// Connected: a live session with it exists.
	Connected bool
}

// NearbyPeers returns the kept devices (my other devices, my buddies' devices) that nearby
// beacons are for; anonymous contacts' devices are left out, so they can neither recognise my
// beacons nor have theirs recognised.
func (m *Manager) NearbyPeers() []NearbyPeer {
	m.mu.Lock()
	var out []NearbyPeer
	for _, k := range m.keepers {
		s := m.sessions[string(k.device)]
		out = append(out, NearbyPeer{Person: k.person, Device: k.device, Connected: s != nil && s.Alive()})
	}
	m.mu.Unlock()
	return slices.DeleteFunc(out, func(p NearbyPeer) bool { return m.Anonymous(p.Person) })
}

// NoteNearby records that a beacon placed device at addrs (transport name -> host:port) and
// wakes its keeper. It reports false, and does nothing, for a device that isn't kept or is an
// anonymous contact's.
func (m *Manager) NoteNearby(device ed25519.PublicKey, addrs map[string]string) bool {
	if len(addrs) == 0 {
		return false
	}
	m.mu.Lock()
	k := m.keepers[string(device)]
	m.mu.Unlock()
	if k == nil || m.Anonymous(k.person) {
		return false
	}
	now := time.Now()
	m.mu.Lock()
	if m.nearby == nil {
		m.nearby = map[string]nearbyHint{}
	}
	old, had := m.nearby[string(device)]
	changed := !had || now.Sub(old.at) >= NearbyTTL || !maps.Equal(old.addrs, addrs)
	m.nearby[string(device)] = nearbyHint{addrs: maps.Clone(addrs), at: now}
	s := m.sessions[string(device)]
	m.mu.Unlock()
	if changed {
		m.log.Info("a kept device is on this network", "peer", short(device))
		m.goBG(func() { m.syncPeer(k.person, device) }) // WireGuard learns the new candidate
	}
	if changed || s == nil || !s.Alive() {
		k.poke()
	}
	return true
}

// withNearby returns rec with a fresh nearby hint for its device folded in: the hinted address
// first among the candidates of its WireGuard and TLS direct endpoints, and an extra clearnet
// endpoint before the record's own. Transports the record lacks are not added (their keys
// would be unknown). rec itself is not modified.
func (m *Manager) withNearby(rec identity.EndpointRecord) identity.EndpointRecord {
	m.mu.Lock()
	h, ok := m.nearby[string(rec.Device)]
	if ok && time.Since(h.at) >= NearbyTTL {
		delete(m.nearby, string(rec.Device))
		ok = false
	}
	m.mu.Unlock()
	if !ok {
		return rec
	}
	eps := make([]identity.Endpoint, 0, len(rec.Endpoints)+1)
	for _, ep := range rec.Endpoints {
		hp, hinted := h.addrs[ep.Transport]
		switch {
		case !hinted:
		case ep.Transport == "clearnet":
			if ep.Address != hp {
				eps = append(eps, identity.Endpoint{Transport: ep.Transport, Address: hp, Access: ep.Access})
			}
		default:
			if merged, ok := transport.AddCandidate(ep.Address, hp); ok {
				ep.Address = merged
			}
		}
		eps = append(eps, ep)
	}
	rec.Endpoints = eps
	return rec
}
