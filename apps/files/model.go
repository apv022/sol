package files

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/apv022/sol/sdk/go/sol"
)

const (
	treeFeed    = "tree"
	sharePrefix = "share."
	sharedDir   = "Shared"
)

// shareRec is one of my shares: a folder given to people in one mode. It lives in the tree
// feed (op "share"/"unshare") so all my devices know it.
type shareRec struct {
	ID     string
	Path   string
	Mode   string
	People map[string]bool
}

// recvShare is a folder somebody shared with me: their share feed plus my own same-named feed
// (my edits, when they granted write), folded into one tree.
type recvShare struct {
	ID      string
	Owner   string
	Name    string
	Mode    string
	Revoked bool
	Tree    *Tree
}

func (a *App) addr(feed string) string { return sol.Address(a.me, appID, feed) }

// applyEntry folds one entry into the model. a.mu held for writing.
func (a *App) applyEntry(addr string, e sol.Entry) {
	owner, _, feed, err := sol.ParseAddress(addr)
	if err != nil {
		return
	}
	if _, _, ok := isSendFeed(feed); ok {
		a.applySendEntry(owner, feed, e)
		return
	}
	var b body
	if json.Unmarshal(e.Body, &b) != nil {
		return
	}
	if owner == a.me && feed == treeFeed {
		st := a.devStats[e.Device]
		if st == nil {
			st = &devStat{}
			a.devStats[e.Device] = st
		}
		st.Entries++
		st.Last = e.Time
		switch b.Op {
		case opShare:
			a.applyShare(b)
		case opUnshare:
			a.applyUnshare(b)
		default:
			a.own.Apply(e.Device, "", e.Seq, b)
		}
		return
	}
	id, ok := strings.CutPrefix(feed, sharePrefix)
	if !ok || !validShareID(id) {
		return
	}
	if a.myShareID[id] {
		if owner == a.me {
			return // my own mirror of a subtree: derived from the tree, never folded back
		}
		// A person I gave write access edited my folder: fold their edits into my tree.
		sh := a.shares[id]
		if sh == nil || !sh.People[owner] || sh.Mode != sol.ModeWrite {
			return
		}
		switch b.Op {
		case opPut, opDelete, opMkdir:
			b.Path = join(sh.Path, b.Path)
		case opMove:
			b.Path, b.To = join(sh.Path, b.Path), join(sh.Path, b.To)
		default:
			return
		}
		if b.Path == sh.Path { // the shared folder itself is theirs to fill, not to delete
			return
		}
		a.own.Apply(e.Device, owner, e.Seq, b)
		return
	}
	rs := a.recv[id]
	if rs == nil {
		rs = &recvShare{ID: id, Tree: NewTree()}
		a.recv[id] = rs
	}
	if owner != a.me {
		rs.Owner = owner
		rs.Revoked = false
		if b.Op == opMeta {
			rs.Name, rs.Mode = b.Name, b.Mode
			return
		}
	}
	switch b.Op {
	case opPut, opDelete, opMkdir, opMove:
		rs.Tree.Apply(e.Device, owner, e.Seq, b)
	}
}

func validShareID(id string) bool {
	return len(id) >= 6 && len(id) <= 32 && sol.ValidFeedName(sharePrefix+id)
}

func (a *App) applyShare(b body) {
	if !validShareID(b.ID) || ValidatePath(b.Path, false) != nil {
		return
	}
	sh := a.shares[b.ID]
	if sh == nil {
		sh = &shareRec{ID: b.ID, Path: b.Path, Mode: b.Mode, People: map[string]bool{}}
		a.shares[b.ID] = sh
	}
	for _, p := range b.People {
		sh.People[p] = true
	}
	if !a.myShareID[b.ID] {
		a.myShareID[b.ID] = true
		// Entries of a co-author feed read before we knew this share went to a phantom tree;
		// read them again so they land in my tree.
		delete(a.recv, b.ID)
		for addr := range a.cursors {
			if _, _, f, err := sol.ParseAddress(addr); err == nil && f == sharePrefix+b.ID {
				a.cursors[addr] = 1
				a.dirty[addr] = true
			}
		}
	}
}

func (a *App) applyUnshare(b body) {
	sh := a.shares[b.ID]
	if sh == nil {
		return
	}
	if len(b.People) == 0 {
		sh.People = map[string]bool{}
	}
	for _, p := range b.People {
		delete(sh.People, p)
	}
	if len(sh.People) == 0 {
		delete(a.shares, b.ID)
	}
}

// pull reads every new entry of one feed and folds it, then reads whatever feeds the folded
// entries asked to be read again. It reports whether anything changed.
func (a *App) pull(ctx context.Context, addr string) (bool, error) {
	queue := []string{addr}
	changed := false
	var first error
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		c, err := a.pullOne(ctx, cur)
		changed = changed || c
		if err != nil && first == nil {
			first = err
		}
		a.mu.Lock()
		for d := range a.dirty {
			queue = append(queue, d)
			delete(a.dirty, d)
		}
		a.mu.Unlock()
	}
	return changed, first
}

func (a *App) pullOne(ctx context.Context, addr string) (bool, error) {
	a.pullMu.Lock()
	defer a.pullMu.Unlock()
	changed := false
	for {
		a.mu.RLock()
		from := a.cursors[addr]
		a.mu.RUnlock()
		if from == 0 {
			from = 1
		}
		es, err := a.api.Read(ctx, addr, from, 500)
		if err != nil {
			if errors.Is(err, sol.ErrForbidden) || errors.Is(err, sol.ErrNotFound) {
				a.markRevoked(addr)
				return true, nil
			}
			return changed, err
		}
		if len(es) > 0 {
			a.mu.Lock()
			for _, e := range es {
				a.applyEntry(addr, e)
				a.cursors[addr] = e.Pos + 1
			}
			a.rev++
			a.mu.Unlock()
			changed = true
		}
		if len(es) < 500 {
			return changed, nil
		}
	}
}

func (a *App) markRevoked(addr string) {
	owner, _, feed, err := sol.ParseAddress(addr)
	id, ok := strings.CutPrefix(feed, sharePrefix)
	if err != nil || !ok || owner == a.me {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if rs := a.recv[id]; rs != nil && !rs.Revoked {
		rs.Revoked = true
		a.rev++
	}
}

// pullAll reads every feed this app can see, the tree first.
func (a *App) pullAll(ctx context.Context) (bool, error) {
	fs, err := a.api.Feeds(ctx, "")
	if err != nil {
		return false, err
	}
	sort.Slice(fs, func(i, j int) bool {
		ti, tj := strings.HasSuffix(fs[i].Feed, "/"+treeFeed), strings.HasSuffix(fs[j].Feed, "/"+treeFeed)
		if ti != tj {
			return ti
		}
		return fs[i].Feed < fs[j].Feed
	})
	changed := false
	var first error
	for _, f := range fs {
		c, err := a.pull(ctx, f.Feed)
		changed = changed || c
		if err != nil && first == nil {
			first = err
		}
	}
	return changed, first
}

func (a *App) refreshPeople(ctx context.Context) {
	ps, err := a.api.People(ctx)
	if err != nil {
		return
	}
	a.mu.Lock()
	for _, p := range ps {
		n := p.Petname
		if n == "" {
			n = p.Name
		}
		a.petnames[p.ID] = n
		a.names[p.ID] = p.DisplayName()
	}
	a.rev++
	a.mu.Unlock()
}

// vfile is one visible path of the unified tree (my folder plus Shared/...).
type vfile struct {
	resolved
	Path  string
	Share string // "" for my own folder, else the received share id
	Rel   string // path inside the share
}

// view is the unified, resolved tree. It is rebuilt when the model changes.
type view struct {
	rev       uint64
	files     map[string]*vfile
	dirs      map[string]bool // every folder, implied ones included
	explicit  map[string]bool // folders that exist without files (mkdir)
	kids      map[string][]string
	roots     map[string]*recvShare // visible received shares by "Shared/pet/name"
	conflicts []conflictInfo
}

type conflictInfo struct {
	Path  string // unified path of the winner
	Share string
	Heads []version
	Rel   string
}

// getView returns the current unified tree.
func (a *App) getView() *view {
	a.mu.RLock()
	if a.view != nil && a.view.rev == a.rev {
		v := a.view
		a.mu.RUnlock()
		return v
	}
	a.mu.RUnlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.view == nil || a.view.rev != a.rev {
		a.view = a.buildView()
	}
	return a.view
}

// buildView with a.mu held for writing (Tree.Resolve caches).
func (a *App) buildView() *view {
	v := &view{rev: a.rev, files: map[string]*vfile{}, dirs: map[string]bool{}, explicit: map[string]bool{},
		kids: map[string][]string{}, roots: map[string]*recvShare{}}
	for p, r := range a.own.Resolve() {
		v.files[p] = &vfile{resolved: r, Path: p}
	}
	for _, d := range a.own.Dirs() {
		v.explicit[d] = true
	}
	for _, p := range a.own.conflicts() {
		v.conflicts = append(v.conflicts, conflictInfo{Path: p, Rel: p, Heads: a.own.Heads(p)})
	}
	ids := make([]string, 0, len(a.recv))
	for id := range a.recv {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	used := map[string]bool{}
	for _, id := range ids {
		rs := a.recv[id]
		if rs.Revoked || rs.Owner == "" || rs.Name == "" || ValidatePath(rs.Name, false) != nil {
			continue
		}
		pet := a.petnames[rs.Owner]
		if pet == "" {
			pet = rs.Owner[:min(8, len(rs.Owner))]
		}
		if validElement(pet) != nil {
			pet = rs.Owner[:min(8, len(rs.Owner))]
		}
		prefix := join(join(sharedDir, pet), rs.Name)
		if used[prefix] {
			prefix += " (" + id[:min(4, len(id))] + ")"
		}
		used[prefix] = true
		v.roots[prefix] = rs
		v.explicit[prefix] = true
		for p, r := range rs.Tree.Resolve() {
			v.files[join(prefix, p)] = &vfile{resolved: r, Path: join(prefix, p), Share: id, Rel: p}
		}
		for _, d := range rs.Tree.Dirs() {
			v.explicit[join(prefix, d)] = true
		}
		for _, p := range rs.Tree.conflicts() {
			v.conflicts = append(v.conflicts, conflictInfo{Path: join(prefix, p), Share: id, Rel: p, Heads: rs.Tree.Heads(p)})
		}
	}
	addDirs := func(p string) {
		for d := parentOf(p); d != ""; d = parentOf(d) {
			if v.dirs[d] {
				break
			}
			v.dirs[d] = true
		}
	}
	for p := range v.files {
		addDirs(p)
	}
	for d := range v.explicit {
		v.dirs[d] = true
		addDirs(d)
	}
	for p := range v.files {
		par := parentOf(p)
		v.kids[par] = append(v.kids[par], p)
	}
	for d := range v.dirs {
		par := parentOf(d)
		v.kids[par] = append(v.kids[par], d)
	}
	for _, k := range v.kids {
		sort.Strings(k)
	}
	sort.Slice(v.conflicts, func(i, j int) bool { return v.conflicts[i].Path < v.conflicts[j].Path })
	return v
}

// filesUnder lists the files at or below p, sorted by path.
func (v *view) filesUnder(p string) []*vfile {
	var out []*vfile
	if f := v.files[p]; f != nil {
		return []*vfile{f}
	}
	for q, f := range v.files {
		if under(q, p) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// sharedRoot returns the received share that contains p (and the path inside it).
func (v *view) sharedRoot(p string) (*recvShare, string, string, bool) {
	best := ""
	for prefix := range v.roots {
		if under(p, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return nil, "", "", false
	}
	return v.roots[best], best, rel(p, best), true
}

var (
	errReadOnly = sol.Errorf("forbidden", "this folder is read only")
	errVirtual  = sol.Errorf("bad_request", "Shared is a view of what people shared with you; add files inside one of their folders")
)

// route decides where a write to p goes: my own tree (rs nil) or a received share I may write.
func (v *view) route(p string) (*recvShare, string, error) {
	if p == sharedDir || strings.HasPrefix(p, sharedDir+"/") {
		rs, prefix, r, ok := v.sharedRoot(p)
		if !ok || p == prefix {
			return nil, "", errVirtual
		}
		if rs.Mode != sol.ModeWrite {
			return nil, "", errReadOnly
		}
		return rs, r, nil
	}
	return nil, p, nil
}

func (a *App) nowMs() int64 { return a.cfg.Now().UnixMilli() }

// noteErr remembers the last transient failure for the status page and schedules a retry.
func (a *App) noteErr(err error) {
	a.mu.Lock()
	a.lastErr = err.Error()
	a.mu.Unlock()
	a.kick()
}

func (a *App) kick() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func transient(err error) bool {
	if err == nil {
		return false
	}
	return !(errors.Is(err, sol.ErrBadRequest) || errors.Is(err, sol.ErrForbidden))
}
