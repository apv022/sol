package files

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

// Op names in the tree and share feeds.
const (
	opPut     = "put"
	opDelete  = "delete"
	opMove    = "move"
	opMkdir   = "mkdir"
	opShare   = "share"   // tree feed only: I shared a folder (see shareRec)
	opUnshare = "unshare" // tree feed only
	opMeta    = "meta"    // share feed only: first entry, names the folder
)

// body is one entry of a tree or share feed (design/apps/FILES.md "Model"). Unknown ops are
// ignored so a newer device can add ops without breaking an older one.
type body struct {
	Op    string `json:"op"`
	Path  string `json:"path,omitempty"`
	To    string `json:"to,omitempty"`
	Blob  string `json:"blob,omitempty"`
	Size  int64  `json:"size,omitempty"`
	Mtime int64  `json:"mtime,omitempty"` // Unix milliseconds
	Base  string `json:"base,omitempty"`  // the blob this edit started from, or empty
	Dev   string `json:"dev,omitempty"`   // device name, for conflict copy names

	// share ops
	ID     string   `json:"id,omitempty"`
	People []string `json:"people,omitempty"`
	Mode   string   `json:"mode,omitempty"`
	Name   string   `json:"name,omitempty"`
}

// version is one candidate content of a path.
type version struct {
	Blob   string
	Size   int64
	Mtime  int64
	Device string // device id of the author (tie-break)
	Dev    string // device name (conflict copy names)
	From   string // person to fetch the blob from ("" means my own devices)
	seq    uint64
}

// newer is the last-writer-wins order: (mtime, device, seq).
func (v version) newer(o version) bool {
	if v.Mtime != o.Mtime {
		return v.Mtime > o.Mtime
	}
	if v.Device != o.Device {
		return v.Device > o.Device
	}
	return v.seq > o.seq
}

// Tree is the fold of a set of entries. Each path holds its "heads": the versions that no
// other version has superseded. One head is the normal case; more than one means two edits
// started from the same base (a conflict) and all of them survive. Folding is order independent
// for entries that arrive in causal order, which a signed per-device chain guarantees.
type Tree struct {
	files map[string][]version
	dirs  map[string]int64
	rev   uint64

	cache    map[string]resolved
	cacheRev uint64
}

// NewTree returns an empty tree.
func NewTree() *Tree {
	return &Tree{files: map[string][]version{}, dirs: map[string]int64{}}
}

// resolved is one path of the visible tree.
type resolved struct {
	version
	// Orig is set on conflict copies: the path whose other version this is.
	Orig string
}

// Apply folds one entry into the tree. path prefix mapping is the caller's job; invalid paths
// are ignored (they cannot be written to disk on every platform).
func (t *Tree) Apply(device, from string, seq uint64, b body) {
	switch b.Op {
	case opPut:
		if ValidatePath(b.Path, false) != nil || !sol.ValidHash(b.Blob) {
			return
		}
		t.put(b.Path, version{Blob: b.Blob, Size: b.Size, Mtime: b.Mtime, Device: device, Dev: b.Dev, From: from, seq: seq}, b.Base)
	case opDelete:
		if ValidatePath(b.Path, false) != nil {
			return
		}
		t.del(b.Path, b.Base, b.Mtime)
	case opMkdir:
		if ValidatePath(b.Path, false) != nil {
			return
		}
		t.dirs[b.Path] = b.Mtime
		t.rev++
	case opMove:
		if ValidatePath(b.Path, false) != nil || ValidatePath(b.To, false) != nil || under(b.To, b.Path) {
			return
		}
		t.move(b.Path, b.To)
	}
}

func (t *Tree) put(p string, v version, base string) {
	var keep []version
	for _, h := range t.files[p] {
		if h.Blob == v.Blob || (base != "" && h.Blob == base) {
			continue
		}
		keep = append(keep, h)
	}
	t.files[p] = append(keep, v)
	t.rev++
}

func (t *Tree) del(p, base string, mtime int64) {
	if _, isDir := t.dirs[p]; isDir {
		delete(t.dirs, p)
		t.rev++
	}
	var keep []version
	for _, h := range t.files[p] {
		if (base != "" && h.Blob == base) || (base == "" && h.Mtime <= mtime) {
			continue
		}
		keep = append(keep, h)
	}
	if len(keep) == 0 {
		delete(t.files, p)
	} else {
		t.files[p] = keep
	}
	t.rev++
}

func (t *Tree) move(from, to string) {
	for p, heads := range t.files {
		if !under(p, from) {
			continue
		}
		np := to + strings.TrimPrefix(p, from)
		delete(t.files, p)
		merged := t.files[np]
	next:
		for _, h := range heads {
			for _, m := range merged {
				if m.Blob == h.Blob {
					continue next
				}
			}
			merged = append(merged, h)
		}
		t.files[np] = merged
	}
	for d, m := range t.dirs {
		if under(d, from) {
			delete(t.dirs, d)
			t.dirs[to+strings.TrimPrefix(d, from)] = m
		}
	}
	t.rev++
}

// Rev changes whenever the tree does.
func (t *Tree) Rev() uint64 { return t.rev }

// Heads returns the versions of a path, newest first.
func (t *Tree) Heads(p string) []version {
	h := append([]version(nil), t.files[p]...)
	sort.Slice(h, func(i, j int) bool { return h[i].newer(h[j]) })
	return h
}

// Dirs returns the explicit (mkdir) folders.
func (t *Tree) Dirs() []string {
	out := make([]string, 0, len(t.dirs))
	for d := range t.dirs {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// Resolve returns the visible tree: the winner of every path plus a conflict copy for every
// other head. Conflict copy names are derived from the loser's device and time, so every device
// computes the same names without extra entries.
func (t *Tree) Resolve() map[string]resolved {
	if t.cache != nil && t.cacheRev == t.rev {
		return t.cache
	}
	out := make(map[string]resolved, len(t.files))
	paths := make([]string, 0, len(t.files))
	for p := range t.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		out[p] = resolved{version: t.Heads(p)[0]}
	}
	for _, p := range paths {
		heads := t.Heads(p)
		for _, l := range heads[1:] {
			first := conflictName(p, l)
			name := first
			for n := 2; ; n++ {
				if _, taken := out[name]; !taken && t.files[name] == nil {
					break
				}
				name = strings.TrimSuffix(first, ext(first)) + fmt.Sprintf(" %d", n) + ext(first)
			}
			out[name] = resolved{version: l, Orig: p}
		}
	}
	t.cache, t.cacheRev = out, t.rev
	return out
}

func ext(p string) string {
	b := baseOf(p)
	if i := strings.LastIndexByte(b, '.'); i > 0 {
		return b[i:]
	}
	return ""
}

// conflictName is "ideas (conflict from laptop 2026-11-19 2103).md" for "ideas.md".
func conflictName(p string, loser version) string {
	e := ext(p)
	stem := strings.TrimSuffix(p, e)
	dev := loser.Dev
	if dev == "" {
		dev = "device"
	}
	dev = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '-'
		}
		return r
	}, dev)
	dev = strings.TrimRight(dev, ". ")
	ts := time.UnixMilli(loser.Mtime).UTC().Format("2006-01-02 1504")
	return fmt.Sprintf("%s (conflict from %s %s)%s", stem, dev, ts, e)
}

// conflicts lists paths with more than one head.
func (t *Tree) conflicts() []string {
	var out []string
	for p, h := range t.files {
		if len(h) > 1 {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
