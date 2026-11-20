package session

// Anonymous contacts (design/PROTOCOL.md §Anonymous contacts): a contact marked anonymous is
// reached over Tor only, in both directions, and never learns an address of mine.
//
//   - Every bundle of mine an anonymous contact receives (in a handshake, a push or GET
//     /p/bundle) carries the onion-only form of my endpoint record (identity.AnonymousBundle).
//     Bundles of my other devices are never sent to contacts at all (relays go to my own
//     devices only), so each of my devices tells them only its own onion.
//   - This device dials them over anonymous transports only (endpointsFor filters their
//     record), never tells WireGuard their key (so it never punches towards them) and never
//     probes a better path.
//   - A session with them must arrive over an anonymous transport: the daemon serves the
//     onion's target on its own listener (ServeAnonymous), and admit refuses an anonymous
//     contact on any other, silently like every refusal.
//   - An anonymous invite (pairing.Token.Anonymous) carries only the onion, may only be
//     redeemed over Tor, and makes whoever redeems it an anonymous contact.

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"slices"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/transport"
)

// AnonymousContacts is implemented by a Contacts store that knows which contacts are
// anonymous. Without it nobody is.
type AnonymousContacts interface {
	// Anonymous reports whether person is a contact reached only over Tor.
	Anonymous(person ed25519.PublicKey) bool
	// AddAnonymousContact records a person who redeemed an anonymous invite.
	AddAnonymousContact(person ed25519.PublicKey, name string) error
}

// ErrNotAnonymousPath refuses an anonymous contact (or an anonymous invite) that arrived over
// a transport that reveals addresses.
var ErrNotAnonymousPath = errors.New("session: an anonymous contact must come over Tor")

// Anonymous reports whether person is an anonymous contact of mine.
func (m *Manager) Anonymous(person ed25519.PublicKey) bool {
	if me := m.kr.Person(); me != nil && bytes.Equal(person, me) {
		return false
	}
	ac, ok := m.cfg.Contacts.(AnonymousContacts)
	return ok && ac.Anonymous(person)
}

// bundleFor returns the wire form of this device's bundle as person may see it.
func (m *Manager) bundleFor(person ed25519.PublicKey) ([]byte, error) {
	if len(person) > 0 && m.Anonymous(person) {
		b, err := m.kr.AnonymousBundle()
		if err != nil {
			return nil, err
		}
		return b.Marshal(), nil
	}
	return m.myBundle()
}

// onlyAnonymous drops every endpoint of rec that is not on an anonymous transport.
func onlyAnonymous(rec identity.EndpointRecord) identity.EndpointRecord {
	rec.Endpoints = identity.OnlyAnonymous(rec.Endpoints)
	return rec
}

// ContactChanged tells the manager a contact's privacy may have changed (anonymous or buddy,
// e.g. replicated from my other devices). Sessions that no longer fit are closed (an anonymous
// contact over a direct path), WireGuard learns or forgets their devices, and the keepers
// redial over what is allowed now. The caller re-signs and pushes the endpoint record.
func (m *Manager) ContactChanged(person ed25519.PublicKey) {
	anon := m.Anonymous(person)
	var drop []*Session
	var devs [][]byte
	m.mu.Lock()
	for _, s := range m.sessions {
		if bytes.Equal(s.peer.Person, person) && anon && !s.anon {
			drop = append(drop, s)
		}
	}
	for _, k := range m.keepers {
		if bytes.Equal(k.person, person) {
			devs = append(devs, k.device)
		}
	}
	m.mu.Unlock()
	for _, s := range drop {
		m.log.Info("closing a direct session: the contact is anonymous now", "peer", short(s.peer.Device))
		s.Close()
	}
	for _, d := range devs {
		m.syncPeer(person, d)
	}
}

// syncPeer tells every transport that keeps per-peer state (WireGuard) about device: its
// address while the device is kept and allowed on that transport, nothing otherwise.
func (m *Manager) syncPeer(person, device ed25519.PublicKey) {
	m.mu.Lock()
	kept := m.keepers[string(device)] != nil && !m.closed
	m.mu.Unlock()
	var rec identity.EndpointRecord
	if kept {
		rec, _ = m.endpointsFor(person, device)
	}
	for _, t := range m.transportsNow() {
		ps, ok := t.(transport.PeerSetter)
		if !ok {
			continue
		}
		want := ""
		for _, ep := range rec.Endpoints {
			if ep.Transport == t.Name() && t.Handles(ep.Address) {
				want = ep.Address
				break
			}
		}
		key := string(device) + "|" + t.Name()
		m.mu.Lock()
		prev := m.peerAddrs[key]
		if want == "" {
			delete(m.peerAddrs, key)
		} else {
			m.peerAddrs[key] = want
		}
		m.mu.Unlock()
		if prev == want {
			continue
		}
		if prev != "" {
			ps.RemovePeer(prev)
		}
		if want != "" {
			if err := ps.SetPeer(want); err != nil {
				m.log.Debug("transport refused a peer", "transport", t.Name(), "peer", short(device), "err", err)
			}
		}
	}
}

// SetTransports replaces the transports sessions are dialed over (a transport was enabled or
// disabled). Per-peer transport state is rebuilt for every kept device.
func (m *Manager) SetTransports(ts []transport.Transport) {
	m.trMu.Lock()
	m.ts = slices.Clone(ts)
	m.trMu.Unlock()
	type kd struct{ p, d ed25519.PublicKey }
	var kept []kd
	m.mu.Lock()
	clear(m.peerAddrs)
	for _, k := range m.keepers {
		kept = append(kept, kd{k.person, k.device})
	}
	m.mu.Unlock()
	for _, k := range kept {
		m.syncPeer(k.p, k.d)
	}
}

// transportsNow returns the current transports.
func (m *Manager) transportsNow() []transport.Transport {
	m.trMu.RLock()
	defer m.trMu.RUnlock()
	return m.ts
}
