package feeds

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"sync"

	"github.com/apv022/sol/internal/identity"
)

// blobRefPrefix starts a blob reference in an entry body (API.md §4).
const blobRefPrefix = "sha256:"

// BlobRefs indexes which feeds reference which blobs: every entry body this
// store holds, outside Sol's internal feeds, is scanned for "sha256:<64 hex>"
// strings, anywhere in the body. It is the whole blob permission rule of
// API.md §4: a person may fetch a blob from me iff an entry of a feed I share
// with them references it, and my own devices may fetch anything.
//
// The index lives in memory: it is rebuilt from the store when created and
// kept current by a synchronous hook on every stored entry (appended or
// accepted), so a blob referenced by an entry is fetchable the moment the
// entry is stored. Entries are never deleted, so the index only grows; what
// shrinks is the sharing, and MayFetch asks Shares on every call, so an
// unshare revokes at once. Safe for concurrent use.
type BlobRefs struct {
	s  *Store
	sh *Shares

	mu    sync.RWMutex
	ids   map[string]uint32 // feed address -> index into addrs
	addrs []string
	refs  map[[32]byte][]uint32 // blob digest -> feeds referencing it, a set
}

// NewBlobRefs builds the index over s (scanning every stored entry) and
// keeps it current. sh answers who may read which of my feeds.
func NewBlobRefs(s *Store, sh *Shares) (*BlobRefs, error) {
	b := &BlobRefs{s: s, sh: sh, ids: map[string]uint32{}, refs: map[[32]byte][]uint32{}}
	// Hook first, then scan: an entry stored meanwhile is added twice,
	// which the set makes harmless, instead of being missed.
	s.onStored(func(e Entry) { b.add(e.Feed, e.Body) })
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
				b.add(e.Feed, e.Body)
			}
			from = es[len(es)-1].Pos + 1
		}
	}
	return b, nil
}

// BlobRefsIn returns the blob digests (lowercase hex) that body references
// as "sha256:<64 hex>", in order of first appearance, without repeats.
func BlobRefsIn(body []byte) []string {
	var out []string
	scanRefs(body, func(d [32]byte) {
		if h := hex.EncodeToString(d[:]); !slices.Contains(out, h) {
			out = append(out, h)
		}
	})
	return out
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

// add records the references in one entry body of the feed at addr.
func (b *BlobRefs) add(addr string, body []byte) {
	if _, app, _, err := ParseAddress(addr); err != nil || app == InternalApp {
		return
	}
	var found [][32]byte
	scanRefs(body, func(d [32]byte) { found = append(found, d) })
	if len(found) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.ids[addr]
	if !ok {
		id = uint32(len(b.addrs))
		b.ids[addr] = id
		b.addrs = append(b.addrs, addr)
	}
	for _, d := range found {
		if fs := b.refs[d]; !slices.Contains(fs, id) {
			b.refs[d] = append(fs, id)
		}
	}
}

// Feeds returns the addresses of the feeds whose entries reference the blob
// (hash: lowercase hex), mine and others', sorted.
func (b *BlobRefs) Feeds(hash string) []string {
	d, ok := digest(hash)
	if !ok {
		return nil
	}
	b.mu.RLock()
	out := make([]string, 0, len(b.refs[d]))
	for _, id := range b.refs[d] {
		out = append(out, b.addrs[id])
	}
	b.mu.RUnlock()
	sort.Strings(out)
	return out
}

// Mine reports whether a feed of mine references the blob, which keeps it
// out of garbage collection.
func (b *BlobRefs) Mine(hash string) bool {
	me := b.s.self.Person()
	if me == nil {
		return false
	}
	prefix := identity.ID(me) + "/"
	for _, addr := range b.Feeds(hash) {
		if len(addr) > len(prefix) && addr[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// MayFetch reports whether person (an ID) may fetch the blob from me: it is
// me (any of my devices), or an entry of a feed of mine that person may
// read (shared with them, or co-authored) references it.
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
