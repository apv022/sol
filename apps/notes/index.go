package notes

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
)

// doc is one indexed note. The index holds no note text: it is rebuilt from the files, which
// stay the only source of truth.
type doc struct {
	id     int
	path   string // relative to Notes/, "/"-separated
	title  string
	tags   []string
	links  []string
	snip   string
	mtime  int64 // UnixNano
	size   int64
	etag   string
	tokens map[string]uint16
}

// index is the in-memory view of all notes: titles, tags, links and an inverted index for
// full-text search with prefix matching. Safe for concurrent use.
type index struct {
	mu       sync.RWMutex
	docs     map[string]*doc
	byID     map[int]*doc
	nextID   int
	post     map[string]map[int]uint16 // token -> doc id -> term frequency
	sorted   []string                  // all tokens, sorted, for prefix ranges; nil when dirty
	byStem   map[string][]string       // lowercase stem -> paths
	byTitle  map[string][]string       // lowercase title -> paths
	dirs     map[string]bool           // every folder under Notes/ (relative), without the root
	rev      uint64
	bumped   chan struct{} // closed and replaced whenever rev changes (see changed)
	tagCount map[string]int
}

func newIndex() *index {
	return &index{docs: map[string]*doc{}, byID: map[int]*doc{}, post: map[string]map[int]uint16{},
		byStem: map[string][]string{}, byTitle: map[string][]string{}, dirs: map[string]bool{}, tagCount: map[string]int{}}
}

func etagOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func removeStr(s []string, v string) []string {
	for i, x := range s {
		if x == v {
			return append(s[:i:i], s[i+1:]...)
		}
	}
	return s
}

// buildDoc analyses text. It does not touch the index.
func buildDoc(path, text string, mtime, size int64, etag string) *doc {
	info := parseNote(path, text)
	d := &doc{path: path, title: info.Title, tags: info.Tags, links: info.Links, snip: info.Snip, mtime: mtime, size: size, etag: etag, tokens: map[string]uint16{}}
	add := func(s string, w uint16) {
		for _, t := range tokenize(s) {
			if d.tokens[t] < 60000 {
				d.tokens[t] += w
			}
		}
	}
	add(text, 1)
	add(info.Title, 3)
	for _, t := range info.Tags {
		add(t, 2)
	}
	return d
}

// put inserts or replaces a doc. It reports whether anything visible changed.
func (x *index) put(d *doc) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if old := x.docs[d.path]; old != nil {
		if old.etag == d.etag && old.mtime == d.mtime && old.size == d.size {
			return false
		}
		x.dropLocked(old)
	}
	x.nextID++
	d.id = x.nextID
	x.docs[d.path], x.byID[d.id] = d, d
	for t, n := range d.tokens {
		m := x.post[t]
		if m == nil {
			m = map[int]uint16{}
			x.post[t] = m
			x.sorted = nil
		}
		m[d.id] = n
	}
	sk, tk := strings.ToLower(stemOf(d.path)), strings.ToLower(d.title)
	x.byStem[sk] = append(x.byStem[sk], d.path)
	x.byTitle[tk] = append(x.byTitle[tk], d.path)
	for _, t := range d.tags {
		x.tagCount[t]++
	}
	for dir := dirOf(d.path); dir != ""; dir = dirOf(dir) {
		x.dirs[dir] = true
	}
	x.bumpLocked()
	return true
}

func (x *index) dropLocked(d *doc) {
	delete(x.docs, d.path)
	delete(x.byID, d.id)
	for t := range d.tokens {
		if m := x.post[t]; m != nil {
			delete(m, d.id)
			if len(m) == 0 {
				delete(x.post, t)
				x.sorted = nil
			}
		}
	}
	sk, tk := strings.ToLower(stemOf(d.path)), strings.ToLower(d.title)
	if v := removeStr(x.byStem[sk], d.path); len(v) > 0 {
		x.byStem[sk] = v
	} else {
		delete(x.byStem, sk)
	}
	if v := removeStr(x.byTitle[tk], d.path); len(v) > 0 {
		x.byTitle[tk] = v
	} else {
		delete(x.byTitle, tk)
	}
	for _, t := range d.tags {
		if x.tagCount[t]--; x.tagCount[t] <= 0 {
			delete(x.tagCount, t)
		}
	}
}

// remove drops the note at path, or every note under it when it is a folder.
func (x *index) remove(path string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	changed := false
	if d := x.docs[path]; d != nil {
		x.dropLocked(d)
		changed = true
	}
	prefix := path + "/"
	for p, d := range x.docs {
		if strings.HasPrefix(p, prefix) {
			x.dropLocked(d)
			changed = true
		}
	}
	for d := range x.dirs {
		if d == path || strings.HasPrefix(d, prefix) {
			delete(x.dirs, d)
			changed = true
		}
	}
	if changed {
		x.bumpLocked()
	}
	return changed
}

// addDir records an (empty or not) folder.
func (x *index) addDir(p string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if p == "" || x.dirs[p] {
		return false
	}
	for ; p != ""; p = dirOf(p) {
		x.dirs[p] = true
	}
	x.bumpLocked()
	return true
}

func (x *index) get(path string) *doc {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.docs[path]
}

// bumpLocked advances the revision and wakes everyone waiting in changed. x.mu held.
func (x *index) bumpLocked() {
	x.rev++
	if x.bumped != nil {
		close(x.bumped)
		x.bumped = nil
	}
}

// changed returns the revision and a channel closed at the next change, so the UI's change
// stream is pushed, never polled.
func (x *index) changed() (uint64, <-chan struct{}) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.bumped == nil {
		x.bumped = make(chan struct{})
	}
	return x.rev, x.bumped
}

func (x *index) revision() uint64 {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.rev
}

func (x *index) paths() []string {
	x.mu.RLock()
	defer x.mu.RUnlock()
	out := make([]string, 0, len(x.docs))
	for p := range x.docs {
		out = append(out, p)
	}
	return out
}

func (x *index) folders() []string {
	x.mu.RLock()
	defer x.mu.RUnlock()
	out := make([]string, 0, len(x.dirs))
	for d := range x.dirs {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// snapshot copies the docs for read-only use outside the lock.
func (x *index) snapshot() []*doc {
	x.mu.RLock()
	defer x.mu.RUnlock()
	out := make([]*doc, 0, len(x.docs))
	for _, d := range x.docs {
		out = append(out, d)
	}
	return out
}

// resolveLocked finds the note a wiki link from `from` names: a path (without .md), a file
// name, or a title; case-insensitive. Ties prefer the same folder, then the shortest path.
func (x *index) resolveLocked(from, target string) string {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return ""
	}
	var c []string
	if strings.Contains(t, "/") {
		t = strings.TrimSuffix(t, ".md")
		if d := x.docs[t+".md"]; d != nil {
			return d.path
		}
		for p := range x.docs { // case-insensitive path
			if strings.ToLower(strings.TrimSuffix(p, ".md")) == t {
				return p
			}
		}
		return ""
	}
	t = strings.TrimSuffix(t, ".md")
	if c = x.byStem[t]; len(c) == 0 {
		c = x.byTitle[t]
	}
	if len(c) == 0 {
		return ""
	}
	best := c[0]
	score := func(p string) int {
		s := len(p)
		if dirOf(p) == dirOf(from) {
			s -= 10000
		}
		return s
	}
	for _, p := range c[1:] {
		if score(p) < score(best) || (score(p) == score(best) && p < best) {
			best = p
		}
	}
	return best
}

// resolve is the locked form of resolveLocked.
func (x *index) resolve(from, target string) string {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.resolveLocked(from, target)
}

// backlinks lists the notes that link to path.
func (x *index) backlinks(path string) []*doc {
	x.mu.RLock()
	defer x.mu.RUnlock()
	var out []*doc
	for _, d := range x.docs {
		if d.path == path {
			continue
		}
		for _, l := range d.links {
			if x.resolveLocked(d.path, l) == path {
				out = append(out, d)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

type tagN struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

func (x *index) tags() []tagN {
	x.mu.RLock()
	defer x.mu.RUnlock()
	out := make([]tagN, 0, len(x.tagCount))
	for t, n := range x.tagCount {
		out = append(out, tagN{t, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Tag < out[j].Tag
	})
	return out
}

func hasTag(d *doc, tag string) bool {
	for _, t := range d.tags {
		if t == tag || strings.HasPrefix(t, tag+"/") {
			return true
		}
	}
	return false
}

// hit is one search result.
type hit struct {
	d     *doc
	score int
}

// search finds notes containing every word of the query (each matched as a prefix) and every
// #tag in it. Title and tag matches rank above body matches, then newer first.
func (x *index) search(query string, limit int) (hits []hit, terms []string) {
	var words, tags []string
	for _, f := range strings.Fields(strings.ToLower(query)) {
		if strings.HasPrefix(f, "#") && len(f) > 1 {
			tags = append(tags, strings.TrimLeft(f, "#"))
			continue
		}
		words = append(words, tokenize(f)...)
	}
	if len(words) == 0 && len(tags) == 0 {
		return nil, nil
	}
	x.mu.Lock()
	if x.sorted == nil {
		x.sorted = make([]string, 0, len(x.post))
		for t := range x.post {
			x.sorted = append(x.sorted, t)
		}
		sort.Strings(x.sorted)
	}
	x.mu.Unlock()
	x.mu.RLock()
	defer x.mu.RUnlock()

	var cand map[int]int
	for _, w := range words {
		got := map[int]int{}
		i := sort.SearchStrings(x.sorted, w)
		for ; i < len(x.sorted) && strings.HasPrefix(x.sorted[i], w); i++ {
			exact := 0
			if x.sorted[i] == w {
				exact = 1
			}
			for id, tf := range x.post[x.sorted[i]] {
				got[id] += int(tf) + exact
			}
		}
		// Intersect with the previous words.
		if cand != nil {
			for id := range got {
				if _, ok := cand[id]; !ok {
					delete(got, id)
				} else {
					got[id] += cand[id]
				}
			}
		}
		cand = got
		if len(cand) == 0 {
			return nil, words
		}
	}
	var out []hit
	consider := func(d *doc, score int) {
		for _, t := range tags {
			if !hasTag(d, t) {
				return
			}
		}
		tl := strings.ToLower(d.title)
		for _, w := range words {
			if strings.Contains(tl, w) {
				score += 20
			}
		}
		out = append(out, hit{d, score})
	}
	if cand != nil {
		for id, s := range cand {
			consider(x.byID[id], s)
		}
	} else {
		for _, d := range x.docs {
			consider(d, 0)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		if out[i].d.mtime != out[j].d.mtime {
			return out[i].d.mtime > out[j].d.mtime
		}
		return out[i].d.path < out[j].d.path
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, words
}
