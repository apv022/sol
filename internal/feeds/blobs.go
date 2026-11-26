package feeds

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/apv022/sol/internal/identity"
)

// blobRefPrefix starts a blob reference in an entry body (API.md §4).
const blobRefPrefix = "sha256:"

// ReleaseField is the one top-level body field the daemon reads (API.md §4): an entry
// {"release": ["sha256:…", …], …} says the earlier entries of its feed no longer reference those
// blobs. The references inside it are releases, not references.
const ReleaseField = "release"

// BlobRefs indexes which feeds reference which blobs: every entry body this
// store holds, outside Sol's internal feeds, is scanned for "sha256:<64 hex>"
// strings, anywhere in the body. It is the whole blob permission rule of
// API.md §4: a person may fetch a blob from me iff an entry of a feed I share
// with them references it, and my own devices may fetch anything.
//
// Feeds are append-only, so an app that stops needing a blob says so with a
// release (ReleaseField): a feed references a blob only while its newest
// reference comes after its newest release, in the stamp order (unix_ms,
// device, seq) that orders a feed's device chains (PROTOCOL.md §Feeds on the
// wire). Append never stamps an entry before one the feed already holds, so
// an entry written after a release (re-uploading the same bytes) references
// the blob again. Within one entry a reference outside the release field
// wins over the release.
//
// The index lives in memory: it is rebuilt from the store when created and
// kept current by a synchronous hook on every stored entry (appended or
// accepted), so a blob referenced by an entry is fetchable the moment the
// entry is stored, and a released one stops being fetchable the moment the
// release is stored. MayFetch asks Shares on every call, so an unshare
// revokes at once. Safe for concurrent use.
type BlobRefs struct {
	s  *Store
	sh *Shares

	mu    sync.RWMutex
	ids   map[string]uint32 // feed address -> index into addrs
	addrs []string
	devs  map[string]string       // interned device IDs (one string per device, not per entry)
	refs  map[[32]byte][]feedRef  // blob digest -> its state in each feed that named it
	onRel []func(hashes []string) // OnRelease hooks
}

// feedRef is one blob's state in one feed: the stamps of its newest
// reference and newest release there.
type feedRef struct {
	feed     uint32
	ref, rel stamp
	hasRef   bool
	hasRel   bool
}

// live reports whether the feed references the blob now.
func (r feedRef) live() bool { return r.hasRef && (!r.hasRel || r.ref.after(r.rel)) }

// NewBlobRefs builds the index over s (scanning every stored entry) and
// keeps it current. sh answers who may read which of my feeds.
func NewBlobRefs(s *Store, sh *Shares) (*BlobRefs, error) {
	b := &BlobRefs{s: s, sh: sh, ids: map[string]uint32{}, devs: map[string]string{}, refs: map[[32]byte][]feedRef{}}
	// Hook first, then scan: an entry stored meanwhile is added twice,
	// which is harmless (adding keeps the newest stamps), instead of being
	// missed.
	s.onStored(b.stored)
	for _, fi := range s.Feeds() {
		if _, app, _, err := ParseAddress(fi.Feed); err != nil || app == InternalApp {
			continue
		}
		for from := uint64(1); ; {
			es, err := s.Read(fi.Feed, from, 5000)
			if err != nil {
				return nil, fmt.Errorf("feeds: indexing blob refs of %s: %w", fi.Feed, err)
			}
			if len(es) == 0 {
				break
			}
			for _, e := range es {
				b.add(e)
			}
			from = es[len(es)-1].Pos + 1
		}
	}
	return b, nil
}

// OnRelease registers fn, called (synchronously, without locks held) with
// the blobs (lowercase hex) that a stored entry of a feed of mine released.
// They may still be referenced elsewhere: the garbage collector checks.
func (b *BlobRefs) OnRelease(fn func(hashes []string)) {
	b.mu.Lock()
	b.onRel = append(b.onRel[:len(b.onRel):len(b.onRel)], fn)
	b.mu.Unlock()
}

func (b *BlobRefs) stored(e Entry) {
	rel := b.add(e)
	if len(rel) == 0 || !b.mineAddr(e.Feed) {
		return
	}
	b.mu.RLock()
	hooks := b.onRel
	b.mu.RUnlock()
	for _, fn := range hooks {
		fn(rel)
	}
}

// BlobRefsIn returns the blob digests (lowercase hex) that body references
// as "sha256:<64 hex>", in order of first appearance, without repeats. A
// blob named only in the release field is not referenced.
func BlobRefsIn(body []byte) []string {
	refs, _ := scanBody(body)
	out := make([]string, 0, len(refs))
	for _, d := range refs {
		out = append(out, hex.EncodeToString(d[:]))
	}
	return out
}

// scanBody returns the blobs body references and the blobs it releases
// (named in its top-level release field and nowhere else in it), each in
// order of first appearance without repeats.
func scanBody(body []byte) (refs, rels [][32]byte) {
	count := map[[32]byte]int{}
	scanRefs(body, func(d [32]byte) {
		if count[d] == 0 {
			refs = append(refs, d)
		}
		count[d]++
	})
	if len(refs) == 0 || !bytes.Contains(body, []byte(`"`+ReleaseField+`"`)) {
		return refs, nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil || top[ReleaseField] == nil {
		return refs, nil
	}
	released := map[[32]byte]bool{}
	scanRefs(top[ReleaseField], func(d [32]byte) {
		count[d]--
		if !released[d] {
			released[d] = true
			rels = append(rels, d)
		}
	})
	refs = slices.DeleteFunc(refs, func(d [32]byte) bool { return count[d] <= 0 })
	rels = slices.DeleteFunc(rels, func(d [32]byte) bool { return count[d] > 0 })
	return refs, rels
}

// scanRefs calls fn for every "sha256:<64 lowercase hex>" in body. JSON
// never escapes those characters, so a byte scan of the canonical body
// finds exactly the references a JSON-aware walk would.
func scanRefs(body []byte, fn func([32]byte)) {
	for {
		i := bytes.Index(body, []byte(blobRefPrefix))
		if i < 0 {
			return
		}
		body = body[i+len(blobRefPrefix):]
		if len(body) < 64 || !lowerHex(body[:64]) {
			continue
		}
		var d [32]byte
		hex.Decode(d[:], body[:64])
		fn(d)
		body = body[64:]
	}
}

func lowerHex(b []byte) bool {
	for _, c := range b {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// digest parses a lowercase hex SHA-256 digest.
func digest(hash string) ([32]byte, bool) {
	var d [32]byte
	if len(hash) != 64 || !lowerHex([]byte(hash)) {
		return d, false
	}
	hex.Decode(d[:], []byte(hash))
	return d, true
}

// add records the references and releases in one entry and returns the
// blobs (lowercase hex) it released.
func (b *BlobRefs) add(e Entry) []string {
	if _, app, _, err := ParseAddress(e.Feed); err != nil || app == InternalApp {
		return nil
	}
	refs, rels := scanBody(e.Body)
	if len(refs) == 0 && len(rels) == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.ids[e.Feed]
	if !ok {
		id = uint32(len(b.addrs))
		b.ids[e.Feed] = id
		b.addrs = append(b.addrs, e.Feed)
	}
	dev, ok := b.devs[e.Device]
	if !ok {
		dev = e.Device
		b.devs[dev] = dev
	}
	st := stamp{e.Time.UnixMilli(), dev, e.Seq}
	at := func(d [32]byte) *feedRef {
		fs := b.refs[d]
		for i := range fs {
			if fs[i].feed == id {
				return &fs[i]
			}
		}
		b.refs[d] = append(fs, feedRef{feed: id})
		return &b.refs[d][len(b.refs[d])-1]
	}
	for _, d := range refs {
		if r := at(d); !r.hasRef || st.after(r.ref) {
			r.ref, r.hasRef = st, true
		}
	}
	var out []string
	for _, d := range rels {
		if r := at(d); !r.hasRel || st.after(r.rel) {
			r.rel, r.hasRel = st, true
		}
		out = append(out, hex.EncodeToString(d[:]))
	}
	return out
}

// Feeds returns the addresses of the feeds that reference the blob (hash:
// lowercase hex) now, mine and others', sorted. A feed that released it
// since its last reference is not among them.
func (b *BlobRefs) Feeds(hash string) []string {
	d, ok := digest(hash)
	if !ok {
		return nil
	}
	b.mu.RLock()
	var out []string
	for _, r := range b.refs[d] {
		if r.live() {
			out = append(out, b.addrs[r.feed])
		}
	}
	b.mu.RUnlock()
	sort.Strings(out)
	return out
}

// mineAddr reports whether the feed at addr is one of mine.
func (b *BlobRefs) mineAddr(addr string) bool {
	me := b.s.self.Person()
	if me == nil {
		return false
	}
	prefix := identity.ID(me) + "/"
	return len(addr) > len(prefix) && addr[:len(prefix)] == prefix
}

// Mine reports whether a feed of mine references the blob, which keeps it
// out of garbage collection.
func (b *BlobRefs) Mine(hash string) bool {
	for _, addr := range b.Feeds(hash) {
		if b.mineAddr(addr) {
			return true
		}
	}
	return false
}

// Released returns the blobs (lowercase hex, sorted) that a feed of mine
// released and no feed of mine references any more: what the garbage
// collector looks at when it starts.
func (b *BlobRefs) Released() []string {
	me := b.s.self.Person()
	if me == nil {
		return nil
	}
	prefix := identity.ID(me) + "/"
	b.mu.RLock()
	var out []string
	for d, fs := range b.refs {
		rel, live := false, false
		for _, r := range fs {
			if !strings.HasPrefix(b.addrs[r.feed], prefix) {
				continue
			}
			rel = rel || r.hasRel
			live = live || r.live()
		}
		if rel && !live {
			out = append(out, hex.EncodeToString(d[:]))
		}
	}
	b.mu.RUnlock()
	sort.Strings(out)
	return out
}

// MayFetch reports whether person (an ID) may fetch the blob from me: it is
// me (any of my devices), or an entry of a feed of mine that person may
// read (shared with them, or co-authored) references it and has not
// released it.
func (b *BlobRefs) MayFetch(person, hash string) bool {
	me := b.s.self.Person()
	if me == nil {
		return false
	}
	if person == identity.ID(me) {
		return true
	}
	for _, addr := range b.Feeds(hash) {
		if b.sh.ReadMode(person, addr) != "" {
			return true
		}
	}
	return false
}

// pinRecord is the body of an entry in my pins feed.
type pinRecord struct {
	Hash   string `json:"hash"` // lowercase hex, no prefix (so the index never sees it)
	Pinned bool   `json:"pinned"`
}

// Pins is the set of blobs I pinned, kept in my internal pins feed so every
// device of mine knows it: pinned blobs are kept from garbage collection
// everywhere and copied to my anchors (API.md §4).
type Pins struct {
	l *lww[pinRecord]
}

// NewPins returns the pins view over s.
func NewPins(s *Store) *Pins {
	return &Pins{l: newLWW(s, PinsFeed, func(r pinRecord) string { return r.Hash })}
}

// Address returns the address of my pins feed ("" before I have a person),
// so watchers can tell its entries from others on the bus.
func (p *Pins) Address() string {
	me := p.l.s.self.Person()
	if me == nil {
		return ""
	}
	return Address(identity.ID(me), InternalApp, PinsFeed)
}

// Pin pins blobs (lowercase hex digests).
func (p *Pins) Pin(hashes []string) error { return p.set(hashes, true) }

// Unpin unpins blobs (lowercase hex digests).
func (p *Pins) Unpin(hashes []string) error { return p.set(hashes, false) }

func (p *Pins) set(hashes []string, pinned bool) error {
	for _, h := range hashes {
		if _, ok := digest(h); !ok {
			return fmt.Errorf("%w: bad blob hash", ErrInvalid)
		}
	}
	for _, h := range hashes {
		if p.Pinned(h) == pinned {
			continue
		}
		if err := p.l.put(pinRecord{Hash: h, Pinned: pinned}); err != nil {
			return err
		}
	}
	return nil
}

// Pinned reports whether the blob is pinned.
func (p *Pins) Pinned(hash string) bool {
	r, _ := p.l.get(hash)
	return r.Pinned
}

// List returns the pinned blobs, sorted.
func (p *Pins) List() []string {
	var out []string
	for h, r := range p.l.all() {
		if r.Pinned {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}
