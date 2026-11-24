package files

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

// Every action requires a caller from my own devices (or my agents): peers reach only what is
// shared with them, as feeds. Each action honours Call.DryRun by returning what it would do
// and changing nothing.

type planItem struct {
	Op     string   `json:"op"`
	Path   string   `json:"path,omitempty"`
	To     string   `json:"to,omitempty"`
	People []string `json:"people,omitempty"`
	Mode   string   `json:"mode,omitempty"`
	Note   string   `json:"note,omitempty"`
}

type dryRun struct {
	DryRun bool       `json:"dry_run"`
	Would  []planItem `json:"would"`
}

func plan(items ...planItem) dryRun {
	if items == nil {
		items = []planItem{}
	}
	return dryRun{DryRun: true, Would: items}
}

func bad(format string, a ...any) error { return sol.Errorf("bad_request", fmt.Sprintf(format, a...)) }

func (a *App) registerActions(act *sol.Actions) {
	h := func(name string, f func(ctx context.Context, c sol.Call) (any, error)) {
		act.Handle(name, func(ctx context.Context, c sol.Call) (any, error) {
			if !c.Caller.Self {
				return nil, sol.Errorf("forbidden", "Files actions are for my own devices and agents")
			}
			return f(ctx, c)
		})
	}
	h("files.list", a.actList)
	h("files.stat", a.actStat)
	h("files.read", a.actRead)
	h("files.write", a.actWrite)
	h("files.mkdir", a.actMkdir)
	h("files.move", a.actMove)
	h("files.copy", a.actCopy)
	h("files.delete", a.actDelete)
	h("files.search", a.actSearch)
	h("files.share", a.actShare)
	h("files.unshare", a.actUnshare)
	h("files.shared", a.actShared)
	h("files.keep", a.actKeep)
	h("files.conflicts", a.actConflicts)
	h("files.resolve", a.actResolve)
	h("files.send", a.actSend)
	h("files.sent", a.actSent)
	h("files.received", a.actReceived)
	h("files.receive", a.actReceive)
	h("files.status", a.actStatus)
	h("files.sync", a.actSync)
	h("files.trash_list", a.actTrashList)
	h("files.trash_restore", a.actTrashRestore)
	h("files.trash_purge", a.actTrashPurge)
	h("files.trash_empty", a.actTrashEmpty)
	h("files.trash_settings", a.actTrashSettings)
	h("files.versions", a.actVersions)
	h("files.version_restore", a.actVersionRestore)
	h("files.version_delete", a.actVersionDelete)
}

// actSync rescans the folder and folds every feed now: the "sync now" of the UI and of agents
// that just wrote into the folder with another program.
func (a *App) actSync(ctx context.Context, c sol.Call) (any, error) {
	if c.DryRun {
		return plan(planItem{Op: "sync", Note: "rescan the folder and fetch every device's changes"}), nil
	}
	if err := a.Rescan(ctx); err != nil {
		return nil, err
	}
	if err := a.Sync(ctx); err != nil {
		return nil, err
	}
	return a.actStatus(ctx, c)
}

// fileInfo is how the UI and agents see one file or folder.
type fileInfo struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Type     string `json:"type"` // "file" or "dir"
	Size     int64  `json:"size"`
	Mtime    int64  `json:"mtime"` // ms
	Blob     string `json:"blob,omitempty"`
	Kept     bool   `json:"kept"`     // contents are on this device's disk
	Local    bool   `json:"local"`    // this file is on disk right now
	Conflict bool   `json:"conflict"` // two edits of this file exist
	Copy     bool   `json:"copy"`     // this is the conflict copy of another file
	Shared   bool   `json:"shared"`   // I share this folder
	ReadOnly bool   `json:"read_only"`
	Virtual  bool   `json:"virtual"` // Shared/ and Shared/<person>: made up, not writable
	// Items is how many entries a folder holds (folders only), so a listing can say "3 items"
	// without one more call per folder.
	Items *int `json:"items,omitempty"`
}

func (a *App) info(v *view, p string) (fileInfo, bool) {
	fi := fileInfo{Name: baseOf(p), Path: p, Kept: a.isKept(p)}
	if f := v.files[p]; f != nil {
		fi.Type, fi.Size, fi.Mtime, fi.Blob = "file", f.Size, f.Mtime, f.Blob
		fi.Copy = f.Orig != ""
		fi.Conflict = fi.Copy
		if !fi.Copy {
			fi.Conflict = a.hasConflict(v, p)
		}
		if e, ok := a.idx(p); ok && e.Blob == f.Blob {
			fi.Local = true
		}
	} else if v.dirs[p] {
		fi.Type = "dir"
		n := len(v.kids[p])
		fi.Items = &n
		for _, sh := range a.sharesSnapshot() {
			if sh.Path == p {
				fi.Shared = true
			}
		}
		fi.Virtual = p == sharedDir || (under(p, sharedDir) && strings.Count(p, "/") == 1)
	} else {
		return fi, false
	}
	if rs, _, _, ok := v.sharedRoot(p); ok && rs.Mode != sol.ModeWrite {
		fi.ReadOnly = true
	}
	if fi.Virtual {
		fi.ReadOnly = true
	}
	return fi, true
}

func (a *App) hasConflict(v *view, p string) bool {
	for _, c := range v.conflicts {
		if c.Path == p {
			return true
		}
	}
	return false
}

func (a *App) sharesSnapshot() []*shareRec {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*shareRec, 0, len(a.shares))
	for _, s := range a.shares {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// view for read actions: fold anything new first, so a UI that reacts to an event sees it.
func (a *App) freshView(ctx context.Context) *view {
	if _, err := a.pullAll(ctx); err != nil {
		a.logf("files: refresh: %v", err)
	}
	return a.getView()
}

type pathArg struct {
	Path string `json:"path"`
}

func cleanArg(p string, root bool) (string, error) {
	c, err := CleanPath(p, root)
	if err != nil {
		return "", bad("%v", err)
	}
	return c, nil
}

func (a *App) actList(ctx context.Context, c sol.Call) (any, error) {
	var in pathArg
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	p, err := cleanArg(in.Path, true)
	if err != nil {
		return nil, err
	}
	v := a.freshView(ctx)
	// Shared always exists, empty until someone shares with me: it is also where files people
	// send me wait for an answer.
	if p != "" && p != sharedDir && !v.dirs[p] {
		if v.files[p] != nil {
			return nil, bad("%q is a file, not a folder", p)
		}
		return nil, sol.Errorf("not_found", "no such folder")
	}
	out := []fileInfo{}
	for _, k := range v.kids[p] {
		if fi, ok := a.info(v, k); ok {
			out = append(out, fi)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Type == "dir") != (out[j].Type == "dir") {
			return out[i].Type == "dir"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	here := fileInfo{Name: "", Path: "", Type: "dir", Kept: a.isKept("")}
	if p == sharedDir && !v.dirs[p] {
		here = fileInfo{Name: sharedDir, Path: sharedDir, Type: "dir", Virtual: true, ReadOnly: true}
	} else if p != "" {
		here, _ = a.info(v, p)
	}
	// here is the folder itself (what files.stat says), so a UI needs one call per folder.
	return map[string]any{"path": p, "here": here, "entries": out}, nil
}

func (a *App) actStat(ctx context.Context, c sol.Call) (any, error) {
	var in pathArg
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	p, err := cleanArg(in.Path, true)
	if err != nil {
		return nil, err
	}
	v := a.freshView(ctx)
	if p == "" {
		return fileInfo{Name: "", Path: "", Type: "dir", Kept: a.isKept("")}, nil
	}
	fi, ok := a.info(v, p)
	if !ok {
		return nil, sol.Errorf("not_found", "no such file or folder")
	}
	return fi, nil
}

func (a *App) actRead(ctx context.Context, c sol.Call) (any, error) {
	var in pathArg
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	p, err := cleanArg(in.Path, false)
	if err != nil {
		return nil, err
	}
	v := a.freshView(ctx)
	f := v.files[p]
	if f == nil {
		if v.dirs[p] {
			return nil, bad("%q is a folder", p)
		}
		return nil, sol.Errorf("not_found", "no such file")
	}
	out := map[string]any{"path": p, "blob": f.Blob, "size": f.Size, "mtime": f.Mtime, "from": f.From, "local_path": ""}
	if e, ok := a.idx(p); ok && e.Blob == f.Blob && a.isKept(p) {
		if _, err := os.Lstat(a.disk(p)); err == nil {
			out["local_path"] = a.disk(p)
		}
	}
	if st, err := a.api.StatBlob(ctx, f.Blob, sol.BlobOptions{From: f.From}); err == nil {
		out["available"] = true
		out["cached"] = st.Local
	} else {
		out["available"] = false
		out["cached"] = false
	}
	return out, nil
}

type writeArg struct {
	Path string  `json:"path"`
	Blob string  `json:"blob"`
	Text *string `json:"text"`
	Base *string `json:"base"`
}

// checkParents refuses writes whose folder path runs through a file.
func checkParents(v *view, p string) error {
	for d := parentOf(p); d != ""; d = parentOf(d) {
		if v.files[d] != nil {
			return sol.Errorf("conflict", fmt.Sprintf("%q is a file, so %q cannot be inside it", d, p))
		}
	}
	return nil
}

func (a *App) writable(v *view, p string) error {
	_, _, err := v.route(p)
	return err
}

func (a *App) actWrite(ctx context.Context, c sol.Call) (any, error) {
	var in writeArg
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	p, err := cleanArg(in.Path, false)
	if err != nil {
		return nil, err
	}
	if (in.Blob == "") == (in.Text == nil) {
		return nil, bad("give exactly one of blob or text")
	}
	if in.Blob != "" && !sol.ValidHash(in.Blob) {
		return nil, bad("blob must look like sha256:<64 hex>")
	}
	v := a.freshView(ctx)
	if err := a.writable(v, p); err != nil {
		return nil, err
	}
	if v.dirs[p] {
		return nil, sol.Errorf("conflict", "a folder with that name exists")
	}
	if err := checkParents(v, p); err != nil {
		return nil, err
	}
	if c.DryRun {
		note := ""
		if f := v.files[p]; f != nil {
			note = "replaces the current file"
		}
		return plan(planItem{Op: "write", Path: p, Note: note}), nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	hash, size := in.Blob, int64(0)
	if in.Text != nil {
		info, err := a.api.PutBlob(ctx, strings.NewReader(*in.Text), baseOf(p))
		if err != nil {
			return nil, err
		}
		hash, size = info.Hash, info.Size
	} else {
		st, err := a.api.StatBlob(ctx, hash, sol.BlobOptions{})
		if err != nil {
			return nil, err
		}
		size = st.Size
	}
	v = a.getView()
	base := ""
	if in.Base != nil {
		base = *in.Base
	} else if f := v.files[p]; f != nil && f.Orig == "" {
		base = f.Blob
	}
	target := p
	if f := v.files[p]; f != nil && f.Orig != "" { // editing a conflict copy edits that version
		target, base = f.Orig, f.Blob
	}
	if err := a.commit(ctx, body{Op: opPut, Path: target, Blob: hash, Size: size, Base: base}); err != nil {
		return nil, err
	}
	if err := a.reconcile(ctx); err != nil && transient(err) {
		a.noteErr(err)
	}
	a.afterPull(ctx)
	return map[string]any{"path": p, "blob": hash, "size": size}, nil
}

func (a *App) actMkdir(ctx context.Context, c sol.Call) (any, error) {
	var in pathArg
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	p, err := cleanArg(in.Path, false)
	if err != nil {
		return nil, err
	}
	v := a.freshView(ctx)
	if err := a.writable(v, p); err != nil {
		return nil, err
	}
	if v.files[p] != nil {
		return nil, sol.Errorf("conflict", "a file with that name exists")
	}
	if err := checkParents(v, p); err != nil {
		return nil, err
	}
	if c.DryRun {
		return plan(planItem{Op: "mkdir", Path: p}), nil
	}
	if v.dirs[p] {
		return map[string]any{"path": p, "created": false}, nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if err := a.commit(ctx, body{Op: opMkdir, Path: p}); err != nil {
		return nil, err
	}
	_ = a.reconcile(ctx)
	return map[string]any{"path": p, "created": true}, nil
}

type fromTo struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (a *App) fromTo(ctx context.Context, c sol.Call) (string, string, *view, error) {
	var in fromTo
	if err := c.Decode(&in); err != nil {
		return "", "", nil, bad("%v", err)
	}
	from, err := cleanArg(in.From, false)
	if err != nil {
		return "", "", nil, err
	}
	to, err := cleanArg(in.To, false)
	if err != nil {
		return "", "", nil, err
	}
	v := a.freshView(ctx)
	if v.files[from] == nil && !v.dirs[from] {
		return "", "", nil, sol.Errorf("not_found", "nothing at "+from)
	}
	if under(to, from) {
		return "", "", nil, bad("cannot put %q inside itself", from)
	}
	if v.files[to] != nil || v.dirs[to] {
		return "", "", nil, sol.Errorf("conflict", fmt.Sprintf("%q already exists", to))
	}
	if err := a.writable(v, to); err != nil {
		return "", "", nil, err
	}
	if err := checkParents(v, to); err != nil {
		return "", "", nil, err
	}
	return from, to, v, nil
}

func (a *App) actMove(ctx context.Context, c sol.Call) (any, error) {
	from, to, v, err := a.fromTo(ctx, c)
	if err != nil {
		return nil, err
	}
	if err := a.writable(v, from); err != nil {
		return nil, err
	}
	if c.DryRun {
		return plan(planItem{Op: "move", Path: from, To: to}), nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	fromRS, _, _ := v.route(from)
	toRS, _, _ := v.route(to)
	simple := fromRS == nil && toRS == nil || (fromRS != nil && fromRS == toRS)
	if simple && v.files[from] != nil && v.files[from].Orig != "" {
		simple = false // a conflict copy is one version of another path: copy then delete
	}
	if simple {
		a.renameOnDisk(from, to)
		if err := a.commit(ctx, body{Op: opMove, Path: from, To: to}); err != nil {
			a.renameOnDisk(to, from)
			return nil, err
		}
	} else {
		if err := a.copyTree(ctx, v, from, to); err != nil {
			return nil, err
		}
		if _, err := a.deleteTree(ctx, a.getView(), from, whyMove); err != nil {
			return nil, err
		}
	}
	if err := a.reconcile(ctx); err != nil && transient(err) {
		a.noteErr(err)
	}
	return map[string]any{"from": from, "to": to}, nil
}

// renameOnDisk moves local copies (and their index entries) so a move does not download
// anything again. Best effort: reconcile fixes whatever is left.
func (a *App) renameOnDisk(from, to string) {
	if !a.isKept(from) || !a.isKept(to) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(a.disk(to)), 0o755); err != nil {
		return
	}
	if err := os.Rename(a.disk(from), a.disk(to)); err != nil {
		return
	}
	a.mu.Lock()
	moved := map[string]idxEntry{}
	for k, e := range a.index {
		if under(k, from) {
			moved[k] = e
		}
	}
	for k, e := range moved {
		delete(a.index, k)
		a.index[to+strings.TrimPrefix(k, from)] = e
	}
	var dirs []string
	for d := range a.dirsMade {
		if under(d, from) {
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		delete(a.dirsMade, d)
		a.dirsMade[to+strings.TrimPrefix(d, from)] = true
	}
	a.mu.Unlock()
	a.markDirty()
}

func (a *App) copyTree(ctx context.Context, v *view, from, to string) error {
	fs := v.filesUnder(from)
	for _, f := range fs {
		dst := to + strings.TrimPrefix(f.Path, from)
		if err := a.commit(ctx, body{Op: opPut, Path: dst, Blob: f.Blob, Size: f.Size, Mtime: a.nowMs()}); err != nil {
			return err
		}
	}
	for d := range v.explicit {
		if under(d, from) {
			if err := a.commit(ctx, body{Op: opMkdir, Path: to + strings.TrimPrefix(d, from)}); err != nil {
				return err
			}
		}
	}
	if len(fs) == 0 && !v.explicit[from] {
		return a.commit(ctx, body{Op: opMkdir, Path: to})
	}
	return nil
}

func (a *App) actCopy(ctx context.Context, c sol.Call) (any, error) {
	from, to, v, err := a.fromTo(ctx, c)
	if err != nil {
		return nil, err
	}
	if c.DryRun {
		return plan(planItem{Op: "copy", Path: from, To: to}), nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if err := a.copyTree(ctx, v, from, to); err != nil {
		return nil, err
	}
	if err := a.reconcile(ctx); err != nil && transient(err) {
		a.noteErr(err)
	}
	return map[string]any{"from": from, "to": to}, nil
}

// headsOf returns the versions that make up one visible file (a conflict copy is one version
// of its original path).
func (a *App) headsOf(f *vfile) (string, []version) {
	if f.Orig != "" {
		p := f.Orig
		return p, []version{f.version}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if f.Share == "" {
		return f.Path, a.own.Heads(f.Path)
	}
	if rs := a.recv[f.Share]; rs != nil {
		return f.Path, rs.Tree.Heads(f.Rel)
	}
	return f.Path, nil
}

// deleteTree deletes the file or folder p from the tree (all versions of every file). Unless why
// is whyMove the entries share one new trash group, which it returns: the deletion is one row
// of Trash.
func (a *App) deleteTree(ctx context.Context, v *view, p, why string) (string, error) {
	group := ""
	if why != whyMove {
		group = newShareID()
	}
	del := func(b body) error {
		b.Op, b.Trash, b.In, b.Why = opDelete, group, p, why
		return a.commit(ctx, b)
	}
	fs := v.filesUnder(p)
	for _, f := range fs {
		path, heads := a.headsOf(f)
		if f.Orig == "" {
			path = f.Path
		} else if f.Share != "" {
			path = join(rootOf(v, f), f.Orig)
		}
		for _, h := range heads {
			if err := del(body{Path: path, Base: h.Blob}); err != nil {
				return group, err
			}
		}
	}
	var dirs []string
	for d := range v.explicit {
		if under(d, p) && !(under(d, sharedDir) && v.roots[d] != nil) {
			dirs = append(dirs, d)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		if err := del(body{Path: d}); err != nil {
			return group, err
		}
	}
	return group, nil
}

// rootOf returns the unified prefix of the share a file belongs to.
func rootOf(v *view, f *vfile) string {
	for prefix, rs := range v.roots {
		if rs.ID == f.Share {
			return prefix
		}
	}
	return ""
}

func (a *App) actDelete(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		Paths []string `json:"paths"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if len(in.Paths) == 0 {
		return nil, bad("no paths")
	}
	v := a.freshView(ctx)
	var paths []string
	var items []planItem
	for _, raw := range in.Paths {
		p, err := cleanArg(raw, false)
		if err != nil {
			return nil, err
		}
		if v.files[p] == nil && !v.dirs[p] {
			return nil, sol.Errorf("not_found", "nothing at "+p)
		}
		if err := a.writable(v, p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
		n := len(v.filesUnder(p))
		note := ""
		if n > 1 || v.dirs[p] {
			note = fmt.Sprintf("%d files", n)
		}
		items = append(items, planItem{Op: "delete", Path: p, Note: note})
	}
	if c.DryRun {
		return plan(items...), nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	// Deletions of my own files go to my Trash (trash: their ids, for files.trash_restore);
	// ones inside a folder shared with me go to its owner's Trash.
	trash := []string{}
	for _, p := range paths {
		v := a.getView()
		rs, _, _ := v.route(p)
		g, err := a.deleteTree(ctx, v, p, "")
		if err != nil {
			return nil, err
		}
		if rs == nil {
			trash = append(trash, g)
		}
	}
	if err := a.reconcile(ctx); err != nil && transient(err) {
		a.noteErr(err)
	}
	return map[string]any{"deleted": paths, "trash": trash}, nil
}

func (a *App) actSearch(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	q := strings.ToLower(strings.TrimSpace(in.Query))
	if q == "" {
		return nil, bad("empty query")
	}
	if in.Limit <= 0 || in.Limit > 500 {
		in.Limit = 100
	}
	v := a.freshView(ctx)
	var hits []string
	for p := range v.files {
		if strings.Contains(strings.ToLower(baseOf(p)), q) {
			hits = append(hits, p)
		}
	}
	for d := range v.dirs {
		if strings.Contains(strings.ToLower(baseOf(d)), q) {
			hits = append(hits, d)
		}
	}
	sort.Strings(hits)
	out := []fileInfo{}
	for _, p := range hits {
		if len(out) >= in.Limit {
			break
		}
		if fi, ok := a.info(v, p); ok {
			out = append(out, fi)
		}
	}
	return map[string]any{"query": in.Query, "results": out}, nil
}

func (a *App) personName(id string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if n := a.names[id]; n != "" {
		return n
	}
	return id
}

func (a *App) actShare(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		Path   string   `json:"path"`
		People []string `json:"people"`
		Mode   string   `json:"mode"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	p, err := cleanArg(in.Path, false)
	if err != nil {
		return nil, err
	}
	if in.Mode == "" {
		in.Mode = sol.ModeRead
	}
	if in.Mode != sol.ModeRead && in.Mode != sol.ModeWrite {
		return nil, bad("mode must be read or write")
	}
	if len(in.People) == 0 {
		return nil, bad("no people")
	}
	if !c.DryRun && !under(p, sharedDir) {
		// Publish what was just dropped into the folder, so the share includes it.
		a.syncMu.Lock()
		err := a.localChange(ctx, p)
		a.syncMu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	v := a.freshView(ctx)
	if under(p, sharedDir) {
		return nil, bad("only folders in my own files can be shared")
	}
	if !v.dirs[p] {
		if v.files[p] != nil {
			return nil, bad("only folders can be shared; put the file in a folder")
		}
		return nil, sol.Errorf("not_found", "no such folder")
	}
	for _, sh := range a.sharesSnapshot() {
		if sh.Path != p && (under(p, sh.Path) || under(sh.Path, p)) {
			return nil, sol.Errorf("conflict", fmt.Sprintf("%q overlaps the shared folder %q", p, sh.Path))
		}
	}
	people, err := a.resolvePeople(ctx, in.People)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(people))
	for i, pp := range people {
		names[i] = pp.Petname
		if names[i] == "" {
			names[i] = pp.Name
		}
	}
	if c.DryRun {
		return plan(planItem{Op: "share", Path: p, People: names, Mode: in.Mode}), nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	id, err := a.shareFolder(ctx, p, in.Mode, people)
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": p, "id": id, "mode": in.Mode, "people": names}, nil
}

func (a *App) actUnshare(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		Path   string   `json:"path"`
		People []string `json:"people"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	p, err := cleanArg(in.Path, false)
	if err != nil {
		return nil, err
	}
	a.freshView(ctx)
	var recs []*shareRec
	for _, s := range a.sharesSnapshot() {
		if s.Path == p {
			recs = append(recs, s)
		}
	}
	if len(recs) == 0 {
		return nil, sol.Errorf("not_found", "that folder is not shared")
	}
	var ids []string
	if len(in.People) > 0 {
		people, err := a.resolvePeople(ctx, in.People)
		if err != nil {
			return nil, err
		}
		for _, pp := range people {
			ids = append(ids, pp.ID)
		}
	}
	type job struct {
		sh  *shareRec
		ids []string
	}
	var jobs []job
	var items []planItem
	for _, s := range recs {
		var these []string
		if len(ids) == 0 {
			for id := range s.People {
				these = append(these, id)
			}
		} else {
			for _, id := range ids {
				if s.People[id] {
					these = append(these, id)
				}
			}
		}
		sort.Strings(these)
		if len(these) == 0 {
			continue
		}
		jobs = append(jobs, job{s, these})
		names := make([]string, len(these))
		for i, id := range these {
			names[i] = a.personName(id)
		}
		items = append(items, planItem{Op: "unshare", Path: p, People: names, Mode: s.Mode})
	}
	if c.DryRun {
		return plan(items...), nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	for _, j := range jobs {
		if err := a.unshareFolder(ctx, j.sh, j.ids); err != nil {
			return nil, err
		}
	}
	return map[string]any{"path": p, "unshared": len(jobs) > 0}, nil
}

func (a *App) actShared(ctx context.Context, c sol.Call) (any, error) {
	v := a.freshView(ctx)
	type withMe struct {
		ID        string `json:"id"`
		Owner     string `json:"owner"`
		OwnerName string `json:"owner_name"`
		Name      string `json:"name"`
		Path      string `json:"path"`
		Mode      string `json:"mode"`
	}
	type personRef struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	type byMe struct {
		ID     string      `json:"id"`
		Path   string      `json:"path"`
		Mode   string      `json:"mode"`
		People []personRef `json:"people"`
	}
	with := []withMe{}
	prefixes := make([]string, 0, len(v.roots))
	for p := range v.roots {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	for _, prefix := range prefixes {
		rs := v.roots[prefix]
		with = append(with, withMe{ID: rs.ID, Owner: rs.Owner, OwnerName: a.personName(rs.Owner), Name: rs.Name, Path: prefix, Mode: rs.Mode})
	}
	mine := []byMe{}
	for _, s := range a.sharesSnapshot() {
		m := byMe{ID: s.ID, Path: s.Path, Mode: s.Mode, People: []personRef{}}
		ids := make([]string, 0, len(s.People))
		for id := range s.People {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			m.People = append(m.People, personRef{ID: id, Name: a.personName(id)})
		}
		mine = append(mine, m)
	}
	return map[string]any{"with_me": with, "by_me": mine}, nil
}

func (a *App) actKeep(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		Path string `json:"path"`
		Keep bool   `json:"keep"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	p, err := cleanArg(in.Path, true)
	if err != nil {
		return nil, err
	}
	v := a.freshView(ctx)
	if p != "" && !v.dirs[p] {
		if v.files[p] == nil {
			return nil, sol.Errorf("not_found", "no such file or folder")
		}
	}
	fs := v.filesUnder(p)
	if p == "" {
		fs = nil
		for _, f := range v.files {
			fs = append(fs, f)
		}
	}
	var bytes int64
	for _, f := range fs {
		bytes += f.Size
	}
	if c.DryRun {
		op := "keep"
		if !in.Keep {
			op = "online-only"
		}
		return plan(planItem{Op: op, Path: p, Note: fmt.Sprintf("%d files, %d bytes", len(fs), bytes)}), nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	a.mu.Lock()
	for k := range a.keep {
		if k != p && under(k, p) {
			delete(a.keep, k)
		}
	}
	a.keep[p] = in.Keep
	a.mu.Unlock()
	a.markDirty()
	if in.Keep {
		if err := a.reconcile(ctx); err != nil {
			return nil, err
		}
		a.pinKept(ctx, fs, true)
	} else {
		if err := a.evict(ctx, p); err != nil {
			return nil, err
		}
		a.pinKept(ctx, fs, false)
	}
	return map[string]any{"path": p, "keep": in.Keep, "files": len(fs), "bytes": bytes}, nil
}

// pinKept asks the daemon to keep (or stop keeping) the blobs of files I keep, best effort.
func (a *App) pinKept(ctx context.Context, fs []*vfile, pin bool) {
	var hs []string
	seen := map[string]bool{}
	for _, f := range fs {
		if f.Orig == "" && !seen[f.Blob] {
			seen[f.Blob] = true
			hs = append(hs, f.Blob)
		}
	}
	for len(hs) > 0 {
		n := min(len(hs), 200)
		var err error
		if pin {
			err = a.api.Pin(ctx, hs[:n])
		} else {
			err = a.api.Unpin(ctx, hs[:n])
		}
		if err != nil {
			return
		}
		hs = hs[n:]
	}
}

type conflictVersion struct {
	Blob     string `json:"blob"`
	Size     int64  `json:"size"`
	Mtime    int64  `json:"mtime"`
	Device   string `json:"device"`
	Winner   bool   `json:"winner"`
	CopyPath string `json:"copy_path,omitempty"`
}

type conflictOut struct {
	Path     string            `json:"path"`
	Share    string            `json:"share,omitempty"`
	Versions []conflictVersion `json:"versions"`
}

func (a *App) conflictList(v *view) []conflictOut {
	out := []conflictOut{}
	for _, c := range v.conflicts {
		co := conflictOut{Path: c.Path, Share: c.Share}
		for i, h := range c.Heads {
			cv := conflictVersion{Blob: h.Blob, Size: h.Size, Mtime: h.Mtime, Device: h.Dev, Winner: i == 0}
			if i > 0 {
				for q, f := range v.files {
					if f.Orig == c.Rel && f.Blob == h.Blob && f.Share == c.Share {
						cv.CopyPath = q
					}
				}
			}
			co.Versions = append(co.Versions, cv)
		}
		out = append(out, co)
	}
	return out
}

func (a *App) actConflicts(ctx context.Context, c sol.Call) (any, error) {
	v := a.freshView(ctx)
	return map[string]any{"conflicts": a.conflictList(v)}, nil
}

func (a *App) actResolve(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		Path string `json:"path"`
		Keep string `json:"keep"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	p, err := cleanArg(in.Path, false)
	if err != nil {
		return nil, err
	}
	if in.Keep != "this" && in.Keep != "that" && in.Keep != "both" {
		return nil, bad("keep must be this, that or both")
	}
	v := a.freshView(ctx)
	var hit *conflictOut
	for _, co := range a.conflictList(v) {
		co := co
		if co.Path == p {
			hit = &co
		}
		for _, ver := range co.Versions {
			if ver.CopyPath == p {
				hit = &co
			}
		}
	}
	if hit == nil {
		return nil, sol.Errorf("not_found", "no conflict at that path")
	}
	if err := a.writable(v, hit.Path); err != nil {
		return nil, err
	}
	winner, losers := hit.Versions[0], hit.Versions[1:]
	var items []planItem
	switch in.Keep {
	case "this":
		for _, l := range losers {
			items = append(items, planItem{Op: "discard", Path: l.CopyPath})
		}
	case "that":
		items = append(items, planItem{Op: "replace", Path: hit.Path, Note: "with the other version"})
	case "both":
		for _, l := range losers {
			items = append(items, planItem{Op: "keep-as-file", Path: l.CopyPath})
		}
	}
	if c.DryRun {
		return plan(items...), nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	switch in.Keep {
	case "this":
		for _, l := range losers {
			if err := a.commit(ctx, body{Op: opDelete, Path: hit.Path, Base: l.Blob}); err != nil {
				return nil, err
			}
		}
	case "both":
		for _, l := range losers {
			if err := a.commit(ctx, body{Op: opPut, Path: l.CopyPath, Blob: l.Blob, Size: l.Size}); err != nil {
				return nil, err
			}
			// It lives on under the copy's name: neither Trash nor a version.
			if err := a.commit(ctx, body{Op: opDelete, Path: hit.Path, Base: l.Blob, Why: whyMove}); err != nil {
				return nil, err
			}
		}
	case "that":
		first := losers[0]
		if err := a.commit(ctx, body{Op: opPut, Path: hit.Path, Blob: first.Blob, Size: first.Size, Base: winner.Blob}); err != nil {
			return nil, err
		}
		for _, l := range losers[1:] {
			if err := a.commit(ctx, body{Op: opDelete, Path: hit.Path, Base: l.Blob}); err != nil {
				return nil, err
			}
		}
	}
	if err := a.reconcile(ctx); err != nil && transient(err) {
		a.noteErr(err)
	}
	return map[string]any{"path": hit.Path, "kept": in.Keep}, nil
}

type deviceStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Self      bool   `json:"self"`
	Online    bool   `json:"online"`
	Anchor    bool   `json:"anchor"`
	Entries   int    `json:"entries"`
	LastEntry string `json:"last_entry,omitempty"`
}

func (a *App) actStatus(ctx context.Context, c sol.Call) (any, error) {
	v := a.freshView(ctx)
	devs := []deviceStatus{}
	if ds, err := a.api.Devices(ctx); err == nil {
		a.mu.RLock()
		for _, d := range ds {
			s := deviceStatus{ID: d.ID, Name: d.Name, Self: d.ID == a.devID, Online: d.Online || d.ID == a.devID, Anchor: d.Anchor}
			if st := a.devStats[d.ID]; st != nil {
				s.Entries = st.Entries
				s.LastEntry = st.Last.UTC().Format(time.RFC3339)
			}
			devs = append(devs, s)
		}
		a.mu.RUnlock()
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	state := "synced"
	switch {
	case a.lastErr != "" || a.pullErr:
		state = "offline"
	case a.busy > 0 || len(a.pending) > 0:
		state = "syncing"
	}
	skipped := []map[string]string{}
	keys := make([]string, 0, len(a.skipped))
	for k := range a.skipped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		skipped = append(skipped, map[string]string{"path": k, "reason": a.skipped[k]})
	}
	watcher := "scan"
	if a.watching {
		watcher = "inotify"
	}
	return map[string]any{
		"device": map[string]string{"id": a.devID, "name": a.devName, "role": a.cfg.Role},
		"state":  state, "pending": len(a.pending), "last_error": a.lastErr,
		"devices": devs, "conflicts": len(v.conflicts), "skipped": skipped,
		"files": len(v.files), "folder": a.root, "watcher": watcher,
	}, nil
}
