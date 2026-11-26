package sol

import (
	"bytes"
	"encoding/json"
	"slices"
	"sort"
	"sync"
)

// ReleaseField is the one top-level body field the daemon reads (API.md §4). An entry
// {"release": ["sha256:…", …], …} says the earlier entries of its feed no longer reference those
// blobs: once no feed of mine references a blob and it is not pinned, every device of mine (and
// my anchors) may delete its bytes, and nobody can fetch it from me through that feed. A later
// entry that names the blob again references it again. A reference elsewhere in the same entry
// wins over its release.
const ReleaseField = "release"

// Release is a body to embed (or copy the field of) to release blobs: Release{Release: hashes}
// marshals as {"release": [...]}.
type Release struct {
	Release []string `json:"release,omitempty"`
}

// BlobRefs returns the blobs ("sha256:<hex>") that an entry body references and the ones it
// releases, in order of first appearance without repeats, by the rule of API.md §4: every
// "sha256:<64 lowercase hex>" anywhere in the body is a reference, except inside the top-level
// release field, where it is a release unless the body also references it elsewhere.
func BlobRefs(body []byte) (refs, released []string) {
	count := map[string]int{}
	scanBlobRefs(body, func(h string) {
		if count[h] == 0 {
			refs = append(refs, h)
		}
		count[h]++
	})
	if len(refs) == 0 || !bytes.Contains(body, []byte(`"`+ReleaseField+`"`)) {
		return refs, nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil || top[ReleaseField] == nil {
		return refs, nil
	}
	seen := map[string]bool{}
	scanBlobRefs(top[ReleaseField], func(h string) {
		count[h]--
		if !seen[h] {
			seen[h] = true
			released = append(released, h)
		}
	})
	refs = slices.DeleteFunc(refs, func(h string) bool { return count[h] <= 0 })
	released = slices.DeleteFunc(released, func(h string) bool { return count[h] > 0 })
	return refs, released
}

func scanBlobRefs(b []byte, fn func(string)) {
	const prefix = "sha256:"
	for {
		i := bytes.Index(b, []byte(prefix))
		if i < 0 {
			return
		}
		b = b[i+len(prefix):]
		if len(b) < 64 || !isLowerHex(b[:64]) {
			continue
		}
		fn(prefix + string(b[:64]))
		b = b[64:]
	}
}

func isLowerHex(b []byte) bool {
	for _, c := range b {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// BlobIndex follows the blob references and releases of the entries added to it, per feed, the
// way the daemon does (API.md §4): a feed references a blob while its newest reference comes
// after its newest release, in the order (time in ms, device, seq) that merges a feed's device
// chains. Apps use it to see what they released (and to reference again what they still need);
// memsol uses it for the permission and collection rules. Adding an entry twice is harmless.
// Safe for concurrent use.
type BlobIndex struct {
	mu sync.Mutex
	m  map[string]map[string]*blobState // feed -> blob -> state
}

type blobStamp struct {
	ms  int64
	dev string
	seq uint64
}

func (a blobStamp) after(b blobStamp) bool {
	if a.ms != b.ms {
		return a.ms > b.ms
	}
	if a.dev != b.dev {
		return a.dev > b.dev
	}
	return a.seq > b.seq
}

type blobState struct {
	ref, rel       blobStamp
	hasRef, hasRel bool
}

func (s *blobState) live() bool { return s.hasRef && (!s.hasRel || s.ref.after(s.rel)) }

// Add folds one entry in and returns the blobs it released.
func (x *BlobIndex) Add(e Entry) []string {
	refs, rels := BlobRefs(e.Body)
	if len(refs) == 0 && len(rels) == 0 {
		return nil
	}
	st := blobStamp{e.Time.UnixMilli(), e.Device, e.Seq}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.m == nil {
		x.m = map[string]map[string]*blobState{}
	}
	f := x.m[e.Feed]
	if f == nil {
		f = map[string]*blobState{}
		x.m[e.Feed] = f
	}
	at := func(h string) *blobState {
		s := f[h]
		if s == nil {
			s = &blobState{}
			f[h] = s
		}
		return s
	}
	for _, h := range refs {
		if s := at(h); !s.hasRef || st.after(s.ref) {
			s.ref, s.hasRef = st, true
		}
	}
	for _, h := range rels {
		if s := at(h); !s.hasRel || st.after(s.rel) {
			s.rel, s.hasRel = st, true
		}
	}
	return rels
}

// Referenced reports whether the feed (an address) references the blob now.
func (x *BlobIndex) Referenced(feed, hash string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	s := x.m[feed][hash]
	return s != nil && s.live()
}

// Released reports whether the feed released the blob after its last reference to it: a blob
// that is still needed then has to be referenced again.
func (x *BlobIndex) Released(feed, hash string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	s := x.m[feed][hash]
	return s != nil && s.hasRef && s.hasRel && !s.live()
}

// Hashes returns the blobs the feed references now, sorted.
func (x *BlobIndex) Hashes(feed string) []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []string
	for h, s := range x.m[feed] {
		if s.live() {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// Feeds returns the feeds that reference the blob now, sorted.
func (x *BlobIndex) Feeds(hash string) []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []string
	for f, m := range x.m {
		if s := m[hash]; s != nil && s.live() {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}
