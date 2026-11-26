package files

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// Trash and versions (design/apps/FILES.md "Trash and versions").
//
// Both are folded from the tree feed, like the tree itself, so every device of mine (and the
// anchor) shows the same Trash without a feed of their own:
//   - a delete entry removes heads from a path; each removed head becomes a trash record (path,
//     blob, size, when, which device, which person). Entries of one deletion share a trash group,
//     so a deleted folder is one row that restores as a tree;
//   - a put that supersedes a head (an edit, a sync, a merge, "keep that") sets the old head aside
//     as a version of the path;
//   - "purge" entries forget records (restored, deleted forever, expired) and "trash-settings"
//     entries set the retention, last writer wins.
//
// Record ids are derived from the entry that made them (device, seq, path, blob), so every device
// computes the same ids. A purge is remembered even before the record it names arrives (another
// device's chain may come later), so the fold stays order independent.
//
// Expiry is a time window, applied when listing (so every device hides the same records at the same
// moment, whichever of them is online), and made durable by a purge entry that my first device
// appends from one timer set to the next expiry (App.expire); nothing polls.
//
// The blobs need nothing extra to stay: the entries that put and deleted them reference them, so
// the daemon keeps them and fetches them from whichever of my devices (or my anchor) holds them.
// A purge ends with a release of every blob nothing uses any more (release.go), so deleting
// forever, emptying Trash, expiry and the version cap free the bytes on every device.

// Retention defaults and bounds.
const (
	defaultTrashDays = 30
	defaultVersions  = 10
	maxTrashDays     = 3650
	maxVersions      = 100 // also how many versions per path the fold keeps at most
	dayMs            = 24 * 60 * 60 * 1000
)

// trashItem is one deleted file or empty folder.
type trashItem struct {
	ID    string
	Group string // the deletion it belongs to
	Root  string // the path the person deleted (a folder, or the file itself)
	Path  string
	Dir   bool // an explicit empty folder
	version
	At    int64  // deleted, Unix ms
	ByDev string // name of the device that deleted it
	By    string // person who deleted it ("" is me)
	Disk  bool   // noticed on disk rather than deleted in Files
}

// oldVersion is the content a path had before it was replaced.
type oldVersion struct {
	ID string
	version
	At    int64  // replaced, Unix ms
	ByDev string // name of the device whose change replaced it
	By    string // person whose change replaced it ("" is me)
}

// trashSettings is the retention every device agrees on.
type trashSettings struct {
	Days     int `json:"days"`     // trash records and versions are kept this long
	Versions int `json:"versions"` // at most this many versions per file
	at       int64
	device   string
}

// history is the Trash and versions part of my own tree.
type history struct {
	trash    map[string]*trashItem    // by record id
	vers     map[string][]*oldVersion // by path, newest first
	verPath  map[string]string        // version id -> path
	purged   map[string]bool
	settings trashSettings
}

func newHistory() *history {
	return &history{trash: map[string]*trashItem{}, vers: map[string][]*oldVersion{}, verPath: map[string]string{},
		purged: map[string]bool{}, settings: trashSettings{Days: defaultTrashDays, Versions: defaultVersions}}
}

// histID names a record after the entry that made it.
func histID(kind string, c change, p, blob string) string {
	h := sha256.Sum256([]byte(kind + "\x00" + c.device + "\x00" + c.from + "\x00" + strconv.FormatUint(c.seq, 10) + "\x00" + p + "\x00" + blob))
	return kind + hex.EncodeToString(h[:10])
}

// validRecID accepts the ids this file makes (and trash group ids, which are 16 hex).
func validRecID(id string) bool {
	if len(id) < 8 || len(id) > 40 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

func (h *history) item(c change, p string) *trashItem {
	root := p
	if in := c.b.In; in != "" && ValidatePath(in, false) == nil && under(p, in) {
		root = in
	}
	return &trashItem{Root: root, Path: p, At: c.at, ByDev: c.b.Dev, By: c.from, Disk: c.b.Why == whyDisk}
}

func (h *history) add(c change, it *trashItem) {
	it.Group = it.ID
	if g := c.b.Trash; g != "" && validRecID(g) {
		it.Group = g
		if c.from != "" {
			// A co-author names its own groups: it cannot add rows to one of mine.
			h := sha256.Sum256([]byte(c.from + "\x00" + g))
			it.Group = "g" + hex.EncodeToString(h[:10])
		}
	}
	if h.purged[it.ID] || h.purged[it.Group] {
		return
	}
	h.trash[it.ID] = it
}

// trashFile records a file head removed by a delete entry.
func (h *history) trashFile(c change, p string, v version) {
	if h == nil || c.b.Why == whyMove {
		return
	}
	it := h.item(c, p)
	it.ID = histID("t", c, p, v.Blob)
	it.version = v
	h.add(c, it)
}

// trashDir records an explicit folder removed by a delete entry.
func (h *history) trashDir(c change, p string) {
	if h == nil || c.b.Why == whyMove {
		return
	}
	it := h.item(c, p)
	it.ID = histID("t", c, p, "")
	it.Dir = true
	h.add(c, it)
}

// replaced keeps the head v of p, superseded by the change c, as a version.
func (h *history) replaced(c change, p string, v version) {
	if h == nil || c.b.Why == whyMove {
		return
	}
	id := histID("v", c, p, v.Blob)
	if h.purged[id] || h.verPath[id] != "" {
		return
	}
	ov := &oldVersion{ID: id, version: v, At: c.at, ByDev: c.b.Dev, By: c.from}
	vs := append(h.vers[p], ov)
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].newer(vs[j]) })
	for _, drop := range vs[min(len(vs), maxVersions):] {
		delete(h.verPath, drop.ID)
	}
	h.vers[p] = vs[:min(len(vs), maxVersions)]
	h.verPath[id] = p
}

// newer orders versions newest first: by when they were replaced, then (two edits in the same
// millisecond are common) by the last-writer-wins order of the content itself, which every device
// computes the same.
func (v *oldVersion) newer(o *oldVersion) bool {
	if v.At != o.At {
		return v.At > o.At
	}
	return v.version.newer(o.version)
}

// move carries the versions of the files under from along to their new paths.
func (h *history) move(from, to string) {
	if h == nil {
		return
	}
	moved := map[string][]*oldVersion{}
	for p, vs := range h.vers {
		if under(p, from) {
			moved[to+strings.TrimPrefix(p, from)] = vs
			delete(h.vers, p)
		}
	}
	for p, vs := range moved {
		all := append(h.vers[p], vs...)
		sort.SliceStable(all, func(i, j int) bool { return all[i].newer(all[j]) })
		h.vers[p] = all
		for _, v := range all {
			h.verPath[v.ID] = p
		}
	}
}

// purge forgets trash groups, trash records and versions by id.
func (h *history) purge(ids []string) {
	for _, id := range ids {
		if !validRecID(id) {
			continue
		}
		h.purged[id] = true
		delete(h.trash, id)
		for k, it := range h.trash {
			if it.Group == id {
				delete(h.trash, k)
			}
		}
		if p := h.verPath[id]; p != "" {
			vs := h.vers[p]
			for i, v := range vs {
				if v.ID == id {
					h.vers[p] = append(vs[:i:i], vs[i+1:]...)
					break
				}
			}
			if len(h.vers[p]) == 0 {
				delete(h.vers, p)
			}
			delete(h.verPath, id)
		}
	}
}

// setSettings applies a trash-settings entry if it is the newest. It reports a change.
func (h *history) setSettings(c change) bool {
	b := c.b
	if c.at < h.settings.at || (c.at == h.settings.at && c.device <= h.settings.device) {
		return false
	}
	// The newest record wins as a whole (every writer sends both fields), so the result does not
	// depend on the order entries arrive in. Out-of-range values mean the default.
	s := trashSettings{Days: defaultTrashDays, Versions: defaultVersions, at: c.at, device: c.device}
	if b.Days > 0 && b.Days <= maxTrashDays {
		s.Days = b.Days
	}
	if b.Versions != nil && *b.Versions >= 0 && *b.Versions <= maxVersions {
		s.Versions = *b.Versions
	}
	h.settings = s
	return true
}

// keepMs is how long records are kept.
func (s trashSettings) keepMs() int64 { return int64(s.Days) * dayMs }

// visibleVersions returns the versions of p still kept at now, newest first: within the retention
// window and among the newest Versions. A content is listed once (two devices that merged the
// same pair each recorded it), and never when it is the current content (cur).
func (h *history) visibleVersions(p, cur string, now int64) []*oldVersion {
	var out []*oldVersion
	seen := map[string]bool{cur: true}
	for _, v := range h.vers[p] {
		if len(out) >= h.settings.Versions {
			break
		}
		if v.At+h.settings.keepMs() > now && !seen[v.Blob] {
			seen[v.Blob] = true
			out = append(out, v)
		}
	}
	return out
}

// sameContent lists the ids of every version record of p holding blob, so purging one content
// purges its duplicates too.
func (h *history) sameContent(p, blob string) []string {
	var ids []string
	for _, v := range h.vers[p] {
		if v.Blob == blob {
			ids = append(ids, v.ID)
		}
	}
	return ids
}

// overCap lists the ids of the versions beyond the per-file cap at now: the ones
// visibleVersions no longer lists because Versions newer contents come first. Versions holding
// a current content (cur: the heads of the path) or a content kept anyway are left alone, and so
// are expired ones (expired purges those).
func (h *history) overCap(cur func(p string) map[string]bool, now int64) []string {
	var ids []string
	for p, vs := range h.vers {
		seen := cur(p)
		n := 0
		for _, v := range vs {
			if v.At+h.settings.keepMs() <= now || seen[v.Blob] {
				continue
			}
			if n < h.settings.Versions {
				seen[v.Blob] = true
				n++
				continue
			}
			ids = append(ids, v.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// expired lists the ids of records whose time is up at now, and the time the next one is due
// (0 when nothing is waiting).
func (h *history) expired(now int64) (ids []string, next int64) {
	keep := h.settings.keepMs()
	due := func(at int64) bool {
		end := at + keep
		if end <= now {
			return true
		}
		if next == 0 || end < next {
			next = end
		}
		return false
	}
	// A deletion whose items are all due is purged by its group id (one id for a whole folder).
	groupDue := map[string]bool{}
	for _, it := range h.trash {
		d := due(it.At)
		if prev, ok := groupDue[it.Group]; !ok || prev {
			groupDue[it.Group] = d
		}
	}
	for id, it := range h.trash {
		if !groupDue[it.Group] && it.At+keep <= now {
			ids = append(ids, id)
		}
	}
	for g, d := range groupDue {
		if d {
			ids = append(ids, g)
		}
	}
	for _, vs := range h.vers {
		for _, v := range vs {
			if due(v.At) {
				ids = append(ids, v.ID)
			}
		}
	}
	sort.Strings(ids)
	return ids, next
}

// trashGroup is one row of Trash: one deletion.
type trashGroup struct {
	ID      string
	Members []string // the trash groups shown as this row (see groups)
	Root    string
	Items   []*trashItem // sorted by path
	At      int64
	ByDev   string
	By      string
	Disk    bool
	Size    int64
	Files   int
}

// diskBurst is how close in time removals on disk by one device must be to be one row: a
// `rm -r` races the watcher, which may publish part of the tree before the folder itself is gone.
const diskBurst = 10_000

// groups returns the deletions in Trash at now, newest first. live names the blobs the tree
// holds right now: a file removed on disk whose content still lives at another path was moved
// or renamed by another program, and is not listed (nothing was lost). Removals on disk by one
// device within diskBurst whose paths nest are one row, so one `rm -r` is one row.
func (h *history) groups(now int64, live map[string]bool) []*trashGroup {
	keep := h.settings.keepMs()
	byID := map[string]*trashGroup{}
	moved := map[string]bool{} // groups with files that only moved
	for _, it := range h.trash {
		if it.At+keep <= now {
			continue
		}
		if it.Disk && !it.Dir && live[it.Blob] {
			moved[it.Group] = true
			continue
		}
		g := byID[it.Group]
		if g == nil {
			g = &trashGroup{ID: it.Group, Members: []string{it.Group}, Root: it.Root, Disk: true}
			byID[it.Group] = g
		}
		g.Items = append(g.Items, it)
		g.Disk = g.Disk && it.Disk
		if it.At >= g.At {
			g.At, g.ByDev, g.By = it.At, it.ByDev, it.By
		}
		if !it.Dir {
			g.Size += it.Size
			g.Files++
		}
		if len(it.Root) < len(g.Root) {
			g.Root = it.Root
		}
	}
	all := make([]*trashGroup, 0, len(byID))
	for _, g := range byID {
		if g.Files == 0 && moved[g.ID] {
			continue // a folder renamed on disk: its files live on
		}
		all = append(all, g)
	}
	// Shallow roots first, so a burst's top folder takes in what lies below it.
	sort.Slice(all, func(i, j int) bool {
		if len(all[i].Root) != len(all[j].Root) {
			return len(all[i].Root) < len(all[j].Root)
		}
		return all[i].ID < all[j].ID
	})
	var out []*trashGroup
next:
	for _, g := range all {
		if g.Disk {
			for _, r := range out {
				if r.Disk && r.ByDev == g.ByDev && r.By == g.By && under(g.Root, r.Root) && abs64(r.At-g.At) <= diskBurst {
					r.Members = append(r.Members, g.ID)
					r.Items = append(r.Items, g.Items...)
					r.Size += g.Size
					r.Files += g.Files
					if g.At > r.At {
						r.At = g.At
					}
					continue next
				}
			}
		}
		out = append(out, g)
	}
	for _, g := range out {
		sort.Slice(g.Items, func(i, j int) bool { return g.Items[i].Path < g.Items[j].Path })
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At > out[j].At
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
