package session

// Pushing identity changes (design/PROTOCOL.md §Pushing changes):
//
//   - Whenever this device's bundle changes (a device added, removed or renamed, a newer list
//     adopted from a sibling, new endpoint addresses), it is pushed to every open session
//     (POST /p/bundle), so a contact learns of a removed device at once and closes its
//     sessions with it, not at its next handshake.
//   - Whenever this device learns something newer about a contact (in a handshake or a push
//     from them), it relays that bundle to my other devices, and a sibling that comes up gets
//     every contact bundle this device holds. So my phone can reach a friend first met on my
//     laptop. Relays from a sibling are not relayed again, and only news is relayed, so
//     nothing loops.
//
// Body of POST /p/bundle: one or more `u32 len | bundle` records (each at most
// wire.MaxBundleFrame). Every bundle verifies on its own (VerifyBundle against its own
// endpoint record's device). A peer may send bundles of its own person; only my own devices
// may send my person's bundles or relay a contact's (or a person about to be one: my
// contacts list replicates separately); anything else is refused.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/wire"
)

// BundlePath is the session endpoint bundles are pushed to (POST) and fetched from (GET).
const BundlePath = "/p/bundle"

// Push limits.
const (
	maxPushBody    = 16 << 20 // bytes per POST /p/bundle
	maxPushBundles = 1024     // bundles per POST /p/bundle
)

// PushTimeout bounds one push of bundles to one peer.
var PushTimeout = 30 * time.Second

// BundleChanged tells the manager this device's bundle may have changed (call it after
// changing the keyring outside the manager: endpoints, device names or flags). If it did,
// the new bundle is pushed to every open session, and Config.OnIdentity is told. Cheap;
// bursts collapse.
func (m *Manager) BundleChanged() {
	if m.cfg.OnIdentity != nil {
		m.cfg.OnIdentity()
	}
	select {
	case m.announce <- struct{}{}:
	default:
	}
}

// announcer pushes this device's bundle to every live session whenever it changes.
func (m *Manager) announcer() {
	defer m.wg.Done()
	last, _ := m.myBundle()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.announce:
		}
		b, err := m.myBundle()
		if err != nil || bytes.Equal(b, last) {
			continue
		}
		last = b
		m.pushMine(m.Sessions())
	}
}

// pushMine pushes this device's bundle to every session in ss, each in the form its person
// may see (an anonymous contact gets the onion-only record), and waits for them.
func (m *Manager) pushMine(ss []*Session) {
	byForm := map[string][]*Session{}
	for _, s := range ss {
		b, err := m.bundleFor(s.peer.Person)
		if err != nil {
			continue
		}
		byForm[string(b)] = append(byForm[string(b)], s)
	}
	for b, group := range byForm {
		m.pushAll(group, [][]byte{[]byte(b)})
	}
}

// pushAll pushes bundles to every session in ss concurrently and waits for them.
func (m *Manager) pushAll(ss []*Session, bundles [][]byte) {
	done := make(chan struct{}, len(ss))
	for _, s := range ss {
		go func() {
			defer func() { done <- struct{}{} }()
			ctx, cancel := context.WithTimeout(m.ctx, PushTimeout)
			defer cancel()
			if err := PushBundles(ctx, s, bundles); err != nil && m.ctx.Err() == nil {
				m.log.Debug("pushing a bundle failed", "peer", short(s.peer.Device), "err", err)
			}
		}()
	}
	for range ss {
		<-done
	}
}

// relay passes a contact's bundle on to my other devices (in the background).
func (m *Manager) relay(b identity.Bundle) {
	var sibs []*Session
	for _, s := range m.Sessions() {
		if s.peer.Relation == Self {
			sibs = append(sibs, s)
		}
	}
	if len(sibs) == 0 {
		return
	}
	m.goBG(func() { m.pushAll(sibs, [][]byte{b.Marshal()}) })
}

// relayAll sends a sibling every contact bundle this device holds.
func (m *Manager) relayAll(s *Session) {
	var all [][]byte
	for _, b := range m.cache.Bundles() {
		if m.cfg.Contacts.IsContact(b.Person.Person) {
			all = append(all, b.Marshal())
		}
	}
	for len(all) > 0 {
		n := min(len(all), maxPushBundles)
		ctx, cancel := context.WithTimeout(m.ctx, PushTimeout)
		err := PushBundles(ctx, s, all[:n])
		cancel()
		if err != nil {
			if m.ctx.Err() == nil {
				m.log.Debug("relaying contacts' bundles failed", "peer", short(s.peer.Device), "err", err)
			}
			return
		}
		all = all[n:]
	}
}

// PushBundles sends bundles (wire forms) to the peer of s (POST /p/bundle).
func PushBundles(ctx context.Context, s *Session, bundles [][]byte) error {
	var body []byte
	for _, b := range bundles {
		if len(b) == 0 || len(b) > wire.MaxBundleFrame {
			return fmt.Errorf("session: bundle of %d bytes", len(b))
		}
		body = binary.BigEndian.AppendUint32(body, uint32(len(b)))
		body = append(body, b...)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://peer"+BundlePath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("session: POST %s: %s", BundlePath, resp.Status)
	}
	return nil
}

// servePush handles POST /p/bundle.
func (m *Manager) servePush(w http.ResponseWriter, r *http.Request) {
	from, ok := PeerFrom(r.Context())
	if !ok {
		http.Error(w, "no peer", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPushBody+1))
	if err != nil {
		return
	}
	if len(body) > maxPushBody {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	var bundles []identity.Bundle
	for len(body) > 0 {
		if len(body) < 4 || len(bundles) >= maxPushBundles {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}
		n := binary.BigEndian.Uint32(body)
		if n == 0 || n > wire.MaxBundleFrame || uint64(n) > uint64(len(body)-4) {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}
		b, err := identity.ParseBundle(body[4 : 4+n])
		if err != nil {
			http.Error(w, "malformed bundle", http.StatusBadRequest)
			return
		}
		bundles = append(bundles, b)
		body = body[4+n:]
	}
	var errs []error
	for _, b := range bundles {
		if err := m.take(from, b); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		m.log.Debug("ignored pushed bundles", "peer", short(from.Device), "n", len(errs), "err", errors.Join(errs...))
	}
	w.WriteHeader(http.StatusNoContent)
}

// take folds one pushed bundle from a verified peer (see the file comment for who may send
// what).
func (m *Manager) take(from Peer, b identity.Bundle) error {
	person, dev := b.Person.Person, b.Endpoints.Device

	if _, err := identity.VerifyBundle(b, dev); err != nil {
		return err
	}
	me := m.kr.Person()
	switch {
	case me != nil && bytes.Equal(person, me):
		if from.Relation != Self {
			return ErrNotRelated
		}
		switch err := m.kr.Adopt(b.Person, b.Devices); {
		case err == nil:
			m.prune(me)
			m.BundleChanged()
		case errors.Is(err, identity.ErrNotInList):
			m.log.Warn("my device list no longer contains this device: it was removed")
			return err
		case !errors.Is(err, identity.ErrStale):
			return err
		}
		if !bytes.Equal(dev, m.kr.DevicePublic()) {
			m.NoteEndpoints(b.Endpoints)
			m.noteBundle(b)
		}
		return nil
	case bytes.Equal(person, from.Person) || from.Relation == Self:
		// A sibling may relay a person my contacts list hasn't caught up with yet (it
		// replicates separately): cache it, since the cache grants nothing by itself, and
		// KeepContacts reaches them once they are a contact here too.
		if from.Relation != Self && !m.cfg.Contacts.IsContact(person) {
			return ErrNotRelated
		}
		_, changed, err := m.cache.Fold(b, dev)
		if err != nil {
			return err
		}
		// Asked after the fold, so a contact added meanwhile is kept either here or by
		// its KeepContacts.
		contact := m.cfg.Contacts.IsContact(person)
		m.NoteEndpoints(b.Endpoints)
		if !changed {
			return nil
		}
		m.prune(person)
		m.noteBundle(b)
		if from.Relation != Self {
			m.relay(b)
		}
		if contact && m.listed(person, dev) {
			m.Keep(person, dev)
		}
		return nil
	}
	return ErrNotRelated
}

// KeepContacts keeps a session up with every listed device of every contact whose endpoints
// this device knows (call it when the contacts list changes, e.g. replicated from my other
// devices).
func (m *Manager) KeepContacts() {
	for _, b := range m.cache.Bundles() {
		person, dev := b.Person.Person, b.Endpoints.Device
		if m.cfg.Contacts.IsContact(person) && m.listed(person, dev) {
			m.Keep(person, dev)
		}
	}
}

// noteBundle hands a verified bundle that was news to Config.OnBundle (persistence).
func (m *Manager) noteBundle(b identity.Bundle) {
	if m.cfg.OnBundle != nil {
		m.cfg.OnBundle(b)
	}
}
