package feeds

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/internal/identity"
)

// Internal feed names (under InternalApp).
const (
	ContactsFeed = "contacts"
	SharesFeed   = "shares"
)

// Share modes (API.md §5).
const (
	ModeRead  = "read"
	ModeWrite = "write"
	// ModeCoauthor marks a feed readable to a person only because that
	// person write-shared their same-named feed with me.
	ModeCoauthor = "coauthor"
)

// stamp orders the writes of a last-writer-wins register across my
// devices: (unix_ms, device ID, seq), the order PROTOCOL.md gives for
// merging a person's device feeds.
type stamp struct {
	ms  int64
	dev string
	seq uint64
}

func (a stamp) after(b stamp) bool {
	if a.ms != b.ms {
		return a.ms > b.ms
	}
	if a.dev != b.dev {
		return a.dev > b.dev
	}
	return a.seq > b.seq
}

// lww folds one of my internal feeds into a map of last-writer-wins
// registers. It catches up lazily from the store on every read, so it
// needs no subscription and is current after replication stores entries.
type lww[V any] struct {
	s    *Store
	name string
	key  func(V) string

	mu    sync.Mutex
	owner string
	pos   uint64
	vals  map[string]V
	stamp map[string]stamp
}

func newLWW[V any](s *Store, name string, key func(V) string) *lww[V] {
	return &lww[V]{s: s, name: name, key: key}
}

// sync catches up with the store. l.mu is held. It returns false before
// this device has a person.
func (l *lww[V]) sync() bool {
	me := l.s.self.Person()
	if me == nil {
		return false
	}
	if id := identity.ID(me); id != l.owner {
		l.owner, l.pos, l.vals, l.stamp = id, 0, map[string]V{}, map[string]stamp{}
	}
	addr := Address(l.owner, InternalApp, l.name)
	for {
		es, err := l.s.Read(addr, l.pos+1, 1000)
		if err != nil || len(es) == 0 {
			return true
		}
		for _, e := range es {
			l.pos = e.Pos
			var v V
			if json.Unmarshal(e.Body, &v) != nil {
				continue
			}
			k := l.key(v)
			st := stamp{e.Time.UnixMilli(), e.Device, e.Seq}
			if old, ok := l.stamp[k]; !ok || st.after(old) {
				l.vals[k], l.stamp[k] = v, st
			}
		}
	}
}

// get returns the current value of key.
func (l *lww[V]) get(key string) (V, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var zero V
	if !l.sync() {
		return zero, false
	}
	v, ok := l.vals[key]
	return v, ok
}

// all returns a copy of every register.
func (l *lww[V]) all() map[string]V {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sync()
	return maps.Clone(l.vals)
}

// put appends a new value to my internal feed.
func (l *lww[V]) put(v V) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = l.s.Append(InternalApp, l.name, b)
	return err
}

// contactRecord is the body of an entry in my contacts feed.
type contactRecord struct {
	Person  string `json:"person"`
	Name    string `json:"name"`
	Removed bool   `json:"removed,omitempty"`
}

// Contacts is my contacts list, kept in my internal contacts feed so every
// device of mine shares it (PROTOCOL.md §Settled in C3). It implements
// session.Contacts.
type Contacts struct {
	l *lww[contactRecord]
}

// NewContacts returns the contacts view over s.
func NewContacts(s *Store) *Contacts {
	return &Contacts{l: newLWW(s, ContactsFeed, func(r contactRecord) string { return r.Person })}
}

// IsContact reports whether person is a (not removed) contact.
func (c *Contacts) IsContact(person ed25519.PublicKey) bool {
	r, ok := c.l.get(identity.ID(person))
	return ok && !r.Removed
}

// AddContact adds or renames person.
func (c *Contacts) AddContact(person ed25519.PublicKey, name string) error {
	if r, ok := c.l.get(identity.ID(person)); ok && !r.Removed && r.Name == name {
		return nil
	}
	return c.l.put(contactRecord{Person: identity.ID(person), Name: name})
}

// RemoveContact removes person from the list.
func (c *Contacts) RemoveContact(person ed25519.PublicKey) error {
	return c.l.put(contactRecord{Person: identity.ID(person), Removed: true})
}

// Name returns the stored name of a contact.
func (c *Contacts) Name(person ed25519.PublicKey) (string, bool) {
	r, ok := c.l.get(identity.ID(person))
	return r.Name, ok && !r.Removed
}

// List returns the IDs of every contact, sorted.
func (c *Contacts) List() []string {
	var out []string
	for id, r := range c.l.all() {
		if !r.Removed {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// shareRecord is the body of an entry in my shares feed. Mode "" unshares.
type shareRecord struct {
	Feed   string `json:"feed"` // <app>/<name>
	Person string `json:"person"`
	Mode   string `json:"mode"`
}

// Share is one row of a feed's sharing records: the feed name (within the
// app), a person ID and the mode. JSON shape of GET /v1/shares.
type Share struct {
	Feed   string `json:"feed"`
	Person string `json:"person"`
	Mode   string `json:"mode"`
}

// Shares is the permission source for replication (and later blobs):
// which of my feeds I share with whom, kept in my internal shares feed so
// all my devices agree, plus what contacts share with me, as they announced
// it over replication (persisted in incoming.json).
type Shares struct {
	s        *Store
	contacts *Contacts
	l        *lww[shareRecord]

	mu       sync.Mutex
	incoming map[string]map[string]string // person -> "<app>/<name>" -> mode
}

// NewShares returns the sharing view over s. contacts, if not nil, limits
// sharing to contacts.
func NewShares(s *Store, contacts *Contacts) (*Shares, error) {
	sh := &Shares{s: s, contacts: contacts, incoming: map[string]map[string]string{},
		l: newLWW(s, SharesFeed, func(r shareRecord) string { return r.Feed + "|" + r.Person })}
	raw, err := os.ReadFile(sh.incomingPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &sh.incoming); err != nil {
			return nil, fmt.Errorf("feeds: %s: %w", sh.incomingPath(), err)
		}
	}
	return sh, nil
}

func (sh *Shares) incomingPath() string { return filepath.Join(sh.s.Dir(), "incoming.json") }

// Share shares my feed app/name with people in mode (read or write).
func (sh *Shares) Share(app, name string, people []string, mode string) error {
	if mode != ModeRead && mode != ModeWrite {
		return fmt.Errorf("%w: mode must be read or write", ErrInvalid)
	}
	return sh.set(app, name, people, mode)
}

// Unshare stops sharing my feed app/name with people.
func (sh *Shares) Unshare(app, name string, people []string) error {
	return sh.set(app, name, people, "")
}

func (sh *Shares) set(app, name string, people []string, mode string) error {
	if !ValidApp(app) || app == InternalApp || !ValidName(name) {
		return fmt.Errorf("%w: bad app or feed name", ErrInvalid)
	}
	for _, p := range people {
		pub, err := identity.ParseID(p)
		if err != nil {
			return fmt.Errorf("%w: bad person", ErrInvalid)
		}
		if sh.contacts != nil && mode != "" && !sh.contacts.IsContact(pub) {
			return fmt.Errorf("%w: %s is not a contact", ErrInvalid, p)
		}
	}
	for _, p := range people {
		if sh.Mode(app, name, p) == mode {
			continue
		}
		if err := sh.l.put(shareRecord{Feed: app + "/" + name, Person: p, Mode: mode}); err != nil {
			return err
		}
	}
	return nil
}

// Mode returns how my feed app/name is shared with person ("" if not).
func (sh *Shares) Mode(app, name, person string) string {
	r, _ := sh.l.get(app + "/" + name + "|" + person)
	return r.Mode
}

// List returns the sharing records of my feed app/name, by person.
func (sh *Shares) List(app, name string) []Share {
	out := []Share{}
	for _, r := range sh.l.all() {
		if r.Feed == app+"/"+name && r.Mode != "" {
			out = append(out, Share{Feed: name, Person: r.Person, Mode: r.Mode})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Person < out[j].Person })
	return out
}

// CanRead reports whether reader (a person ID) may read my feed at addr:
// it is mine and reader is me, or I share it with reader, or reader
// write-shared their same-named feed with me (co-author). Internal feeds are
// never readable by anyone else. Feeds I don't own are never mine to serve.
func (sh *Shares) CanRead(reader, addr string) bool {
	return sh.ReadMode(reader, addr) != ""
}

// ReadMode is CanRead with the reason: "self", ModeRead, ModeWrite,
// ModeCoauthor, or "" when reader may not read addr.
func (sh *Shares) ReadMode(reader, addr string) string {
	owner, app, name, err := ParseAddress(addr)
	me := sh.s.self.Person()
	if err != nil || me == nil || owner != identity.ID(me) {
		return ""
	}
	if reader == owner {
		return "self"
	}
	if app == InternalApp {
		return ""
	}
	if m := sh.Mode(app, name, reader); m != "" {
		return m
	}
	if sh.Incoming(reader, app, name) == ModeWrite {
		return ModeCoauthor
	}
	return ""
}

// ReadableBy returns every feed of mine that person (a contact) may read,
// with the reason (ModeRead, ModeWrite or ModeCoauthor). It agrees with
// ReadMode and also names shared feeds that have no entries yet.
func (sh *Shares) ReadableBy(person string) map[string]string {
	out := map[string]string{}
	me := sh.s.self.Person()
	if me == nil || person == identity.ID(me) {
		return out
	}
	for _, r := range sh.l.all() {
		if r.Person == person && r.Mode != "" {
			out[identity.ID(me)+"/"+r.Feed] = r.Mode
		}
	}
	for k, mode := range sh.IncomingAll(person) {
		addr := identity.ID(me) + "/" + k
		if _, ok := out[addr]; !ok && mode == ModeWrite {
			if _, app, _, err := ParseAddress(addr); err == nil && app != InternalApp {
				out[addr] = ModeCoauthor
			}
		}
	}
	return out
}

// Incoming returns how person shares their feed app/name with me, as they
// last announced it ("" if not shared).
func (sh *Shares) Incoming(person, app, name string) string {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.incoming[person][app+"/"+name]
}

// IncomingAll returns everything person announced as readable to me:
// "<app>/<name>" -> mode.
func (sh *Shares) IncomingAll(person string) map[string]string {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return maps.Clone(sh.incoming[person])
}

// SetIncoming replaces what person shares with me ("<app>/<name>" -> mode,
// as announced by one of their devices), persists it, and publishes
// EventFeed for every feed that became readable or unreadable to me.
func (sh *Shares) SetIncoming(person string, m map[string]string) error {
	sh.mu.Lock()
	old := sh.incoming[person]
	if maps.Equal(old, m) {
		sh.mu.Unlock()
		return nil
	}
	if len(m) == 0 {
		delete(sh.incoming, person)
	} else {
		sh.incoming[person] = maps.Clone(m)
	}
	raw, err := json.Marshal(sh.incoming)
	if err == nil {
		// Under the lock, so concurrent updates reach the disk in order.
		err = fsutil.WriteFileAtomic(sh.incomingPath(), raw, 0o600)
	}
	sh.mu.Unlock()
	if err != nil {
		return err
	}
	for k := range old {
		if _, ok := m[k]; !ok {
			sh.s.bus.Publish(Event{Type: EventFeed, Feed: person + "/" + k, Readable: false})
		}
	}
	for k := range m {
		if _, ok := old[k]; !ok {
			sh.s.bus.Publish(Event{Type: EventFeed, Feed: person + "/" + k, Readable: true})
		}
	}
	return nil
}
