package files

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

// Actions on Trash and versions (trash.go has the model). Restoring writes ordinary put and
// mkdir entries and then purges the restored records, so every device sees the file come back
// and leave Trash together.

const (
	purgeChunk    = 400 // ids per purge entry, well under the 64 KiB entry limit
	maxGroupItems = 100 // items listed per Trash row (the row still counts them all)
)

type trashItemOut struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Dir  bool   `json:"dir,omitempty"`
	Size int64  `json:"size"`
	Blob string `json:"blob,omitempty"`
}

// trashRow is one deletion as the UI and agents see it.
type trashRow struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Path      string         `json:"path"`   // where it was
	Folder    string         `json:"folder"` // the folder it was in ("" is My files)
	Dir       bool           `json:"dir"`    // a folder was deleted
	DeletedAt int64          `json:"deleted_at"`
	ExpiresAt int64          `json:"expires_at"`
	Device    string         `json:"device"`       // the device that deleted it
	By        string         `json:"by,omitempty"` // a person I share the folder with, when not me
	Disk      bool           `json:"disk"`         // noticed on disk (rm, another program) rather than deleted in Files
	Size      int64          `json:"size"`
	Files     int            `json:"files"`
	Items     []trashItemOut `json:"items"`
	More      int            `json:"more,omitempty"` // items not listed
}

func (a *App) trashSnapshot(ctx context.Context) ([]trashRow, trashSettings, int64) {
	live := liveBlobs(a.freshView(ctx))
	now := a.nowMs()
	a.mu.RLock()
	h := a.own.hist
	gs := h.groups(now, live)
	s := h.settings
	_, next := h.expired(now)
	a.mu.RUnlock()
	rows := make([]trashRow, 0, len(gs))
	for _, g := range gs {
		r := trashRow{ID: g.ID, Name: baseOf(g.Root), Path: g.Root, Folder: parentOf(g.Root), DeletedAt: g.At,
			ExpiresAt: g.At + s.keepMs(), Device: g.ByDev, Size: g.Size, Files: g.Files, Items: []trashItemOut{}}
		if g.By != "" {
			r.By = a.personName(g.By)
		}
		r.Dir = len(g.Items) > 1 || g.Items[0].Dir || g.Items[0].Path != g.Root
		r.Disk = g.Disk
		for _, it := range g.Items {
			if len(r.Items) == maxGroupItems {
				r.More++
				continue
			}
			r.Items = append(r.Items, trashItemOut{ID: it.ID, Path: it.Path, Dir: it.Dir, Size: it.Size, Blob: it.Blob})
		}
		rows = append(rows, r)
	}
	return rows, s, next
}

func (a *App) actTrashList(ctx context.Context, c sol.Call) (any, error) {
	rows, s, next := a.trashSnapshot(ctx)
	var bytes int64
	for _, r := range rows {
		bytes += r.Size
	}
	out := map[string]any{"items": rows, "count": len(rows), "bytes": bytes, "days": s.Days, "versions": s.Versions}
	if next > 0 {
		out["next_expiry"] = next
	}
	return out, nil
}

// pick resolves trash ids (rows or single items) to sets of records, each restored or purged
// together. a.mu held.
type pickSet struct {
	ids   []string // what to purge once it is restored or deleted forever
	root  string
	items []*trashItem
}

func (a *App) pickTrash(ids []string, now int64, live map[string]bool) ([]pickSet, error) {
	h := a.own.hist
	rows := h.groups(now, live)
	var out []pickSet
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		ps := pickSet{}
	find:
		for _, r := range rows {
			if r.ID == id {
				ps = pickSet{ids: r.Members, root: r.Root, items: r.Items}
				break
			}
			for _, it := range r.Items {
				if it.ID == id {
					ps = pickSet{ids: []string{id}, root: it.Path, items: []*trashItem{it}}
					break find
				}
			}
		}
		if len(ps.items) == 0 {
			return nil, sol.Errorf("not_found", fmt.Sprintf("nothing in Trash has the id %q (see files.trash_list)", id))
		}
		out = append(out, ps)
	}
	return out, nil
}

// liveBlobs names the blobs the tree holds now (see history.groups).
func liveBlobs(v *view) map[string]bool {
	live := make(map[string]bool, len(v.files))
	for _, f := range v.files {
		live[f.Blob] = true
	}
	return live
}

type idsArg struct {
	IDs []string `json:"ids"`
}

// restoreStep is one entry a restore writes.
type restoreStep struct {
	it   *trashItem
	to   string
	skip bool // the same content is already there
}

// restoreName is "a (restored).txt", then "a (restored 2).txt", for a path that is taken.
func restoreName(p string, taken func(string) bool) string {
	e := ext(p)
	if strings.Contains(baseOf(p), ".") && strings.HasPrefix(baseOf(p), ".") && strings.Count(baseOf(p), ".") == 1 {
		e = "" // ".bashrc" has no extension
	}
	stem := strings.TrimSuffix(p, e)
	for n := 1; ; n++ {
		mark := " (restored)"
		if n > 1 {
			mark = fmt.Sprintf(" (restored %d)", n)
		}
		if q := stem + mark + e; !taken(q) {
			return q
		}
	}
}

// restorePlan decides where each record goes back to. to, when set, is the folder to restore into
// instead of the original one.
func (a *App) restorePlan(v *view, sets []pickSet, to *string) ([]restoreStep, error) {
	planned := map[string]bool{}
	taken := func(p string) bool { return planned[p] || v.files[p] != nil || v.dirs[p] }
	var steps []restoreStep
	for _, ps := range sets {
		root := ps.root
		if to != nil {
			root = join(*to, baseOf(ps.root))
		}
		single := len(ps.items) == 1 && ps.items[0].Path == ps.root
		if !single && v.files[root] != nil {
			root = restoreName(root, taken) // a file took the folder's name: restore beside it
		}
		for _, it := range ps.items {
			dest := root
			if !single {
				dest = join(root, rel(it.Path, ps.root))
			}
			st := restoreStep{it: it, to: dest}
			switch {
			case it.Dir:
				if v.files[dest] != nil {
					st.to = restoreName(dest, taken)
				} else if v.dirs[dest] || planned[dest] {
					st.skip = true
				}
			case v.files[dest] != nil && v.files[dest].Blob == it.Blob:
				st.skip = true // already back (an earlier restore, or the same file written again)
			case taken(dest):
				st.to = restoreName(dest, taken)
			}
			if err := ValidatePath(st.to, false); err != nil {
				return nil, bad("cannot restore to %q: %v", st.to, err)
			}
			if err := a.writable(v, st.to); err != nil {
				return nil, err
			}
			if err := checkParents(v, st.to); err != nil {
				return nil, sol.Errorf("conflict", err.Error()+"; restore it somewhere else with to")
			}
			planned[st.to] = true
			steps = append(steps, st)
		}
	}
	return steps, nil
}

func (a *App) actTrashRestore(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		IDs []string `json:"ids"`
		To  *string  `json:"to"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if len(in.IDs) == 0 {
		return nil, bad("give the ids to restore (from files.trash_list)")
	}
	if in.To != nil {
		t, err := cleanArg(*in.To, true)
		if err != nil {
			return nil, err
		}
		in.To = &t
	}
	prepare := func(v *view) ([]pickSet, []restoreStep, error) {
		a.mu.RLock()
		sets, err := a.pickTrash(in.IDs, a.nowMs(), liveBlobs(v))
		a.mu.RUnlock()
		if err != nil {
			return nil, nil, err
		}
		if in.To != nil && *in.To != "" {
			if v.files[*in.To] != nil {
				return nil, nil, bad("%q is a file, not a folder", *in.To)
			}
		}
		steps, err := a.restorePlan(v, sets, in.To)
		return sets, steps, err
	}
	_, steps, err := prepare(a.freshView(ctx))
	if err != nil {
		return nil, err
	}
	if c.DryRun {
		items := make([]planItem, 0, len(steps))
		for _, st := range steps {
			pi := planItem{Op: "restore", Path: st.it.Path, To: st.to}
			if st.skip {
				pi.Note = "already there"
			}
			items = append(items, pi)
		}
		return plan(items...), nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	sets, steps, err := prepare(a.getView())
	if err != nil {
		return nil, err
	}
	type moved struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	done := []moved{}
	for _, st := range steps {
		if st.skip {
			continue
		}
		b := body{Op: opPut, Path: st.to, Blob: st.it.Blob, Size: st.it.Size, Mtime: st.it.Mtime}
		if st.it.Dir {
			b = body{Op: opMkdir, Path: st.to}
		}
		if err := a.commit(ctx, b); err != nil {
			return nil, err
		}
		done = append(done, moved{From: st.it.Path, To: st.to})
	}
	var ids []string
	for _, ps := range sets {
		ids = append(ids, ps.ids...)
	}
	if err := a.purge(ctx, ids); err != nil {
		return nil, err
	}
	if err := a.reconcile(ctx); err != nil && transient(err) {
		a.noteErr(err)
	}
	return map[string]any{"restored": done}, nil
}

// purge appends purge entries for ids. a.syncMu held.
func (a *App) purge(ctx context.Context, ids []string) error {
	for len(ids) > 0 {
		n := min(len(ids), purgeChunk)
		if err := a.commit(ctx, body{Op: opPurge, IDs: ids[:n]}); err != nil {
			return err
		}
		ids = ids[n:]
	}
	return nil
}

func (a *App) actTrashPurge(ctx context.Context, c sol.Call) (any, error) {
	var in idsArg
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if len(in.IDs) == 0 {
		return nil, bad("give the ids to delete forever (from files.trash_list)")
	}
	live := liveBlobs(a.freshView(ctx))
	a.mu.RLock()
	sets, err := a.pickTrash(in.IDs, a.nowMs(), live)
	a.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if c.DryRun {
		items := make([]planItem, 0, len(sets))
		for _, ps := range sets {
			items = append(items, planItem{Op: "delete-forever", Path: ps.root, Note: plural(len(ps.items), "item")})
		}
		return plan(items...), nil
	}
	var ids []string
	for _, ps := range sets {
		ids = append(ids, ps.ids...)
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if err := a.purge(ctx, ids); err != nil {
		return nil, err
	}
	return map[string]any{"purged": in.IDs}, nil
}

func (a *App) actTrashEmpty(ctx context.Context, c sol.Call) (any, error) {
	a.freshView(ctx)
	a.mu.RLock()
	seen := map[string]bool{}
	var ids []string
	for _, it := range a.own.hist.trash {
		if !seen[it.Group] {
			seen[it.Group] = true
			ids = append(ids, it.Group)
		}
	}
	a.mu.RUnlock()
	sort.Strings(ids)
	if c.DryRun {
		return plan(planItem{Op: "empty-trash", Note: plural(len(ids), "deletion") + " deleted forever"}), nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if err := a.purge(ctx, ids); err != nil {
		return nil, err
	}
	return map[string]any{"purged": len(ids)}, nil
}

func plural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return fmt.Sprintf("%d %ss", n, w)
}

// versionOut is one earlier content of a file.
type versionOut struct {
	ID         string `json:"id"`
	Blob       string `json:"blob"`
	Size       int64  `json:"size"`
	Mtime      int64  `json:"mtime"`       // the file's time when it had this content
	ReplacedAt int64  `json:"replaced_at"` // when another content replaced it
	Device     string `json:"device"`      // the device whose change replaced it
	By         string `json:"by,omitempty"`
	ExpiresAt  int64  `json:"expires_at"`
}

// versionsOf returns the path's visible versions and its current content (nil when none).
func (a *App) versionsOf(v *view, p string) ([]versionOut, *vfile, error) {
	if rs, _, _ := v.route(p); rs != nil || p == sharedDir || strings.HasPrefix(p, sharedDir+"/") {
		return nil, nil, sol.Errorf("bad_request", "versions of a shared folder's files are kept by its owner")
	}
	cur := v.files[p]
	if cur != nil && cur.Orig != "" {
		return nil, nil, bad("%q is a conflict copy: settle the conflict (files.resolve) first", p)
	}
	curBlob := ""
	if cur != nil {
		curBlob = cur.Blob
	}
	now := a.nowMs()
	a.mu.RLock()
	h := a.own.hist
	vs := h.visibleVersions(p, curBlob, now)
	keep := h.settings.keepMs()
	out := make([]versionOut, 0, len(vs))
	for _, ov := range vs {
		vo := versionOut{ID: ov.ID, Blob: ov.Blob, Size: ov.Size, Mtime: ov.Mtime, ReplacedAt: ov.At, Device: ov.ByDev, ExpiresAt: ov.At + keep}
		if ov.By != "" {
			vo.By = ov.By
		}
		out = append(out, vo)
	}
	a.mu.RUnlock()
	for i := range out {
		if out[i].By != "" {
			out[i].By = a.personName(out[i].By)
		}
	}
	return out, cur, nil
}

func (a *App) actVersions(ctx context.Context, c sol.Call) (any, error) {
	var in pathArg
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	p, err := cleanArg(in.Path, false)
	if err != nil {
		return nil, err
	}
	v := a.freshView(ctx)
	if v.dirs[p] {
		return nil, bad("%q is a folder: versions are kept per file", p)
	}
	vs, cur, err := a.versionsOf(v, p)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"path": p, "versions": vs}
	if cur != nil {
		out["current"] = map[string]any{"blob": cur.Blob, "size": cur.Size, "mtime": cur.Mtime}
	}
	return out, nil
}

// findVersion looks an id up among the path's visible versions.
func findVersion(vs []versionOut, id string) *versionOut {
	for i := range vs {
		if vs[i].ID == id {
			return &vs[i]
		}
	}
	return nil
}

func (a *App) actVersionRestore(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		Path string `json:"path"`
		ID   string `json:"id"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	p, err := cleanArg(in.Path, false)
	if err != nil {
		return nil, err
	}
	prepare := func(v *view) (*versionOut, *vfile, error) {
		if v.dirs[p] {
			return nil, nil, sol.Errorf("conflict", "a folder has that name now")
		}
		vs, cur, err := a.versionsOf(v, p)
		if err != nil {
			return nil, nil, err
		}
		ver := findVersion(vs, in.ID)
		if ver == nil {
			return nil, nil, sol.Errorf("not_found", "that file has no version with this id (see files.versions)")
		}
		if err := checkParents(v, p); err != nil {
			return nil, nil, err
		}
		return ver, cur, nil
	}
	ver, cur, err := prepare(a.freshView(ctx))
	if err != nil {
		return nil, err
	}
	if c.DryRun {
		note := "the current content becomes a version"
		if cur == nil {
			note = "the file is created again"
		}
		return plan(planItem{Op: "restore-version", Path: p, Note: note + " (" + time.UnixMilli(ver.Mtime).UTC().Format("2006-01-02 15:04") + " UTC)"}), nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if ver, cur, err = prepare(a.getView()); err != nil {
		return nil, err
	}
	b := body{Op: opPut, Path: p, Blob: ver.Blob, Size: ver.Size}
	if cur != nil {
		b.Base = cur.Blob
		// A conflict has more heads: restoring replaces all of them.
		for _, h := range a.headsAt(p) {
			if h.Blob != cur.Blob {
				b.Also = append(b.Also, h.Blob)
			}
		}
	}
	if err := a.commit(ctx, b); err != nil {
		return nil, err
	}
	a.mu.RLock()
	ids := a.own.hist.sameContent(p, ver.Blob)
	a.mu.RUnlock()
	if err := a.purge(ctx, ids); err != nil {
		return nil, err
	}
	if err := a.reconcile(ctx); err != nil && transient(err) {
		a.noteErr(err)
	}
	return map[string]any{"path": p, "blob": ver.Blob, "size": ver.Size}, nil
}

// headsAt returns the heads of a path of my own tree.
func (a *App) headsAt(p string) []version {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.own.Heads(p)
}

func (a *App) actVersionDelete(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		Path string   `json:"path"`
		IDs  []string `json:"ids"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	p, err := cleanArg(in.Path, false)
	if err != nil {
		return nil, err
	}
	if len(in.IDs) == 0 {
		return nil, bad("give the version ids to delete (from files.versions)")
	}
	vs, _, err := a.versionsOf(a.freshView(ctx), p)
	if err != nil {
		return nil, err
	}
	var blobs []string
	for _, id := range in.IDs {
		ver := findVersion(vs, id)
		if ver == nil {
			return nil, sol.Errorf("not_found", fmt.Sprintf("%s has no version %q (see files.versions)", p, id))
		}
		blobs = append(blobs, ver.Blob)
	}
	if c.DryRun {
		items := make([]planItem, 0, len(in.IDs))
		for _, id := range in.IDs {
			items = append(items, planItem{Op: "delete-version", Path: p, Note: id})
		}
		return plan(items...), nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	var ids []string
	a.mu.RLock()
	for _, b := range blobs {
		ids = append(ids, a.own.hist.sameContent(p, b)...)
	}
	a.mu.RUnlock()
	if err := a.purge(ctx, ids); err != nil {
		return nil, err
	}
	return map[string]any{"path": p, "deleted": in.IDs}, nil
}

func (a *App) actTrashSettings(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		Days     *int `json:"days"`
		Versions *int `json:"versions"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	a.freshView(ctx)
	a.mu.RLock()
	s := a.own.hist.settings
	a.mu.RUnlock()
	cur := map[string]any{"days": s.Days, "versions": s.Versions}
	if in.Days == nil && in.Versions == nil {
		return cur, nil
	}
	days, vers := s.Days, s.Versions
	if in.Days != nil {
		if *in.Days < 1 || *in.Days > maxTrashDays {
			return nil, bad("days must be between 1 and %d", maxTrashDays)
		}
		days = *in.Days
	}
	if in.Versions != nil {
		if *in.Versions < 0 || *in.Versions > maxVersions {
			return nil, bad("versions must be between 0 and %d", maxVersions)
		}
		vers = *in.Versions
	}
	if c.DryRun {
		return plan(planItem{Op: "trash-settings", Note: fmt.Sprintf("keep deleted files and versions %d days, at most %d versions per file", days, vers)}), nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if err := a.commit(ctx, body{Op: opTrashSettings, Days: days, Versions: &vers}); err != nil {
		return nil, err
	}
	return map[string]any{"days": days, "versions": vers}, nil
}

// nextExpiry is when the next record leaves Trash or versions (0: nothing waiting).
func (a *App) nextExpiry() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, next := a.own.hist.expired(a.nowMs())
	return next
}

// expire makes expiry durable: my first device appends one purge for every record whose time is
// up (the others already hide them, by time). The loop calls it from a timer set to the next
// expiry; nothing polls.
func (a *App) expire(ctx context.Context) {
	a.mu.RLock()
	ids, _ := a.own.hist.expired(a.nowMs())
	a.mu.RUnlock()
	if len(ids) == 0 || !a.firstDevice(ctx) {
		return
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if err := a.purge(ctx, ids); err != nil {
		a.logf("files: expiring Trash: %v", err)
		a.noteErr(err)
	}
}

// firstDevice reports whether this is my device with the lowest id, the one that does the
// once-per-person chores (conflict notices, expiry).
func (a *App) firstDevice(ctx context.Context) bool {
	ds, err := a.api.Devices(ctx)
	if err != nil {
		return false
	}
	for _, d := range ds {
		if d.ID < a.devID {
			return false
		}
	}
	return true
}

// histVersion finds blob among the versions and Trash records of p.
func (a *App) histVersion(p, blob string) (version, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	h := a.own.hist
	for _, v := range h.vers[p] {
		if v.Blob == blob {
			return v.version, true
		}
	}
	for _, it := range h.trash {
		if it.Path == p && it.Blob == blob && !it.Dir {
			return it.version, true
		}
	}
	return version{}, false
}
