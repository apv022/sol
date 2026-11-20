package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"

	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/session"
)

// peerBook remembers the newest bundle (person certificate, device list, endpoint record) of
// every device this one held a session with, in peers.json (0600, atomic). The identity cache
// and the session manager keep that only in memory; the book is what lets a restarted daemon
// verify and redial its peers. Bundles are signed objects, so a tampered file can at worst
// point at nothing: every one is verified again when it is loaded.
type peerBook struct {
	path string
	mu   sync.Mutex
	m    map[string][]byte // device ID -> bundle wire form
}

func openPeers(path string) (*peerBook, error) {
	p := &peerBook{path: path, m: map[string][]byte{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &p.m); err != nil {
		return nil, errors.New("daemon: peers.json is corrupt")
	}
	return p, nil
}

// bundles returns the stored bundles that still parse, oldest identity state first (by
// person-certificate and device-list seq), so folding them in order ends at the newest
// without an older one being refused as a rollback after it.
func (p *peerBook) bundles() []identity.Bundle {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []identity.Bundle
	for _, raw := range p.m {
		if b, err := identity.ParseBundle(raw); err == nil {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Person.Seq != b.Person.Seq {
			return a.Person.Seq < b.Person.Seq
		}
		return a.Devices.Seq < b.Devices.Seq
	})
	return out
}

// seed folds the contacts' bundles into the identity cache: what a restarted daemon knew of
// its contacts' devices and endpoints (pushed and relayed news included) survives.
func (p *peerBook) seed(kr *identity.Keyring, cache *identity.Cache) {
	me := kr.Person()
	for _, b := range p.bundles() {
		if me != nil && bytes.Equal(b.Person.Person, me) {
			continue
		}
		_, _ = cache.Accept(b, b.Endpoints.Device) // a stale or bad one is simply not used
	}
}

// keep makes the session manager redial every remembered device still mine or a contact's.
func (p *peerBook) keep(kr *identity.Keyring, cache *identity.Cache, contacts *feeds.Contacts, m *session.Manager) {
	me := kr.Person()
	mine, _ := kr.Bundle()
	for _, b := range p.bundles() {
		dev := b.Endpoints.Device
		switch {
		case me != nil && bytes.Equal(b.Person.Person, me):
			if _, ok := mine.Devices.Find(dev); !ok || bytes.Equal(dev, kr.DevicePublic()) {
				continue
			}
		case !contacts.IsContact(b.Person.Person):
			continue
		default:
			if _, dl, ok := cache.Latest(b.Person.Person); !ok {
				continue
			} else if _, in := dl.Find(dev); !in {
				continue // removed from its person's list since
			}
		}
		m.NoteEndpoints(b.Endpoints)
		m.Keep(b.Person.Person, dev)
	}
}

// put remembers a peer's bundle (session.Config.OnBundle: everything verified and new).
func (p *peerBook) put(b identity.Bundle) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := identity.ID(b.Endpoints.Device)
	raw := b.Marshal()
	if bytes.Equal(p.m[id], raw) {
		return
	}
	p.m[id] = raw
	out, _ := json.Marshal(p.m)
	_ = fsutil.WriteFileAtomic(p.path, out, 0o600)
}
