package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

// idxEntry is what this device last synced for one path: the blob and the file's stat at that
// moment. A file whose stat still matches is untouched; one whose hash still equals Blob was
// only "touched". Anything else is a local edit. It is also the echo guard: after we write a
// file ourselves the index matches it, so the watcher event it causes is a no-op.
type idxEntry struct {
	Blob  string `json:"blob"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"` // UnixNano of the file on disk
}

type persisted struct {
	Index   map[string]idxEntry `json:"index"`
	Keep    map[string]bool     `json:"keep"`
	Noticed map[string]bool     `json:"noticed"`
}

func (a *App) statePath() string { return filepath.Join(a.data, "state.json") }

func (a *App) loadState() {
	b, err := os.ReadFile(a.statePath())
	if err != nil {
		return
	}
	var p persisted
	if json.Unmarshal(b, &p) != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if p.Index != nil {
		a.index = p.Index
	}
	if p.Keep != nil {
		a.keep = p.Keep
	}
	if p.Noticed != nil {
		a.noticed = p.Noticed
	}
}

func (a *App) saveState() {
	a.mu.RLock()
	b, err := json.Marshal(persisted{Index: a.index, Keep: a.keep, Noticed: a.noticed})
	a.mu.RUnlock()
	if err != nil || a.data == "" {
		return
	}
	if err := writeFileAtomic(a.statePath(), b, 0o600); err != nil {
		a.logf("files: save state: %v", err)
	}
}

// markDirty schedules a state save (debounced).
func (a *App) markDirty() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.saveT != nil {
		a.saveT.Reset(500 * time.Millisecond)
		return
	}
	a.saveT = time.AfterFunc(500*time.Millisecond, a.saveState)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(perm); err != nil && !errors.Is(err, os.ErrPermission) {
		_ = err // chmod is not supported on every system; the directory is private anyway
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

func (a *App) disk(p string) string { return filepath.Join(a.root, filepath.FromSlash(p)) }

// isKept reports whether p's contents are written to disk on this device: the longest matching
// keep choice, else the default (kept on desktop, online only on Android and for shared
// folders).
func (a *App) isKept(p string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.keptLocked(p)
}

func (a *App) keptLocked(p string) bool {
	best, val := -1, false
	for k, v := range a.keep {
		if under(p, k) && len(k) > best {
			best, val = len(k), v
		}
	}
	if best >= 0 {
		return val
	}
	if under(p, sharedDir) {
		return false
	}
	return a.cfg.Role != RoleAndroid
}

func (a *App) skip(p, why string) {
	a.mu.Lock()
	a.skipped[p] = why
	a.mu.Unlock()
}

func (a *App) setIndex(p string, e idxEntry) {
	a.mu.Lock()
	a.index[p] = e
	delete(a.skipped, p)
	a.mu.Unlock()
	a.markDirty()
}

func (a *App) dropIndex(p string) {
	a.mu.Lock()
	delete(a.index, p)
	a.mu.Unlock()
	a.markDirty()
}

func (a *App) idx(p string) (idxEntry, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	e, ok := a.index[p]
	return e, ok
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

var errUnstable = errors.New("file is still changing")

// localChange brings one path of the folder (and everything below it) into the feed. It is the
// only way disk changes enter the tree. a.syncMu held.
func (a *App) localChange(ctx context.Context, p string) error {
	if p == "" {
		return a.scan(ctx)
	}
	a.ign.reload(a.root)
	if a.ign.ignored(p) {
		return nil
	}
	st, err := os.Lstat(a.disk(p))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return a.localGone(ctx, p)
	case err != nil:
		return err
	case st.Mode()&os.ModeSymlink != 0:
		a.skip(p, "symbolic links are not synced")
		return nil
	case st.IsDir():
		return a.localDir(ctx, p)
	case !st.Mode().IsRegular():
		a.skip(p, "not a regular file")
		return nil
	}
	if err := ValidatePath(p, false); err != nil {
		a.skip(p, err.Error())
		return nil
	}
	v := a.getView()
	rs, relp, err := v.route(p)
	if err != nil {
		a.skip(p, err.Error())
		return nil
	}
	_ = rs
	_ = relp
	if cur, ok := a.idx(p); ok && cur.Size == st.Size() && cur.Mtime == st.ModTime().UnixNano() {
		return nil
	}
	full := a.disk(p)
	hash, err := sha256File(full)
	if err != nil {
		return err
	}
	if st2, err := os.Lstat(full); err != nil || st2.Size() != st.Size() || !st2.ModTime().Equal(st.ModTime()) {
		return errUnstable
	}
	now := idxEntry{Blob: hash, Size: st.Size(), Mtime: st.ModTime().UnixNano()}
	cur, had := a.idx(p)
	if had && cur.Blob == hash {
		a.setIndex(p, now) // touched, not edited
		return nil
	}
	if f := v.files[p]; f != nil && f.Blob == hash {
		a.setIndex(p, now) // the feed already says this; nothing to publish
		return nil
	}
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	info, err := a.api.PutBlob(ctx, f, baseOf(p))
	f.Close()
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.stats.BlobUploads++
	a.mu.Unlock()
	if info.Hash != hash {
		return errUnstable
	}
	b := body{Op: opPut, Path: p, Blob: hash, Size: st.Size(), Mtime: st.ModTime().UnixMilli()}
	if had {
		b.Base = cur.Blob
	}
	if err := a.commit(ctx, b); err != nil {
		return err
	}
	a.setIndex(p, now)
	return nil
}

func (a *App) localDir(ctx context.Context, p string) error {
	if err := ValidatePath(p, false); err != nil {
		a.skip(p, err.Error())
		return nil
	}
	ents, err := os.ReadDir(a.disk(p))
	if err != nil {
		return err
	}
	var first error
	for _, e := range ents {
		if err := a.localChange(ctx, join(p, e.Name())); err != nil && first == nil {
			first = err
		}
	}
	if len(ents) == 0 {
		v := a.getView()
		if !v.dirs[p] {
			if _, _, err := v.route(p); err == nil {
				if err := a.commit(ctx, body{Op: opMkdir, Path: p}); err != nil && first == nil {
					first = err
				}
			}
		}
	}
	return first
}

// localGone publishes the deletion of a path (or a folder of paths) that vanished from disk.
func (a *App) localGone(ctx context.Context, p string) error {
	a.mu.RLock()
	var gone []string
	for k := range a.index {
		if under(k, p) {
			gone = append(gone, k)
		}
	}
	a.mu.RUnlock()
	sort.Strings(gone)
	v := a.getView()
	var first error
	for _, k := range gone {
		e, _ := a.idx(k)
		a.dropIndex(k)
		if v.files[k] == nil {
			continue
		}
		if _, _, err := v.route(k); err != nil {
			continue
		}
		if err := a.commit(ctx, body{Op: opDelete, Path: k, Base: e.Blob}); err != nil {
			a.setIndex(k, e) // not published: keep it so the next scan tries again
			if first == nil {
				first = err
			}
		}
	}
	if v.explicit[p] {
		if _, _, err := v.route(p); err == nil && !under(p, sharedDir) {
			if err := a.commit(ctx, body{Op: opDelete, Path: p}); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// scan compares the whole folder with the index: new or changed files are published, files
// that vanished are published as deleted. It hashes only files whose size or mtime changed.
func (a *App) scan(ctx context.Context) error {
	a.ign.reload(a.root)
	seen := map[string]bool{}
	var first error
	note := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	_ = filepath.WalkDir(a.root, func(full string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return nil
		}
		r, e := filepath.Rel(a.root, full)
		if e != nil || r == "." {
			return nil
		}
		p := filepath.ToSlash(r)
		if a.ign.ignored(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		seen[p] = true
		if d.IsDir() {
			if ents, e := os.ReadDir(full); e == nil && len(ents) == 0 {
				note(a.localChange(ctx, p))
			}
			return nil
		}
		if st, e := d.Info(); e == nil && st.Mode().IsRegular() {
			if cur, ok := a.idx(p); ok && cur.Size == st.Size() && cur.Mtime == st.ModTime().UnixNano() {
				return nil
			}
		}
		note(a.localChange(ctx, p))
		return nil
	})
	a.mu.RLock()
	var missing []string
	for k := range a.index {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	a.mu.RUnlock()
	sort.Strings(missing)
	for _, k := range missing {
		if _, err := os.Lstat(a.disk(k)); errors.Is(err, fs.ErrNotExist) {
			note(a.localGone(ctx, k))
		}
	}
	return first
}

// reconcile makes the disk match the tree for every kept path: it writes new and changed
// files, removes deleted ones and never touches a file with an unpublished local edit.
// a.syncMu held.
func (a *App) reconcile(ctx context.Context) error {
	var first error
	for pass := 0; pass < 4; pass++ {
		a.mu.RLock()
		start := a.rev
		a.mu.RUnlock()
		changed, err := a.reconcilePass(ctx)
		if err != nil && first == nil {
			first = err
		}
		a.mu.RLock()
		moved := a.rev != start
		a.mu.RUnlock()
		if !changed && !moved {
			break
		}
	}
	return first
}

func (a *App) reconcilePass(ctx context.Context) (bool, error) {
	v := a.getView()
	var first error
	note := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	paths := make([]string, 0, len(v.files))
	for p := range v.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	did := false
	for _, p := range paths {
		if ctx.Err() != nil {
			return did, ctx.Err()
		}
		f := v.files[p]
		if !a.isKept(p) || ValidatePath(p, false) != nil || a.ign.ignored(p) {
			continue
		}
		cur, had := a.idx(p)
		if had && cur.Blob == f.Blob {
			continue
		}
		full := a.disk(p)
		if st, err := os.Lstat(full); err == nil {
			if !st.Mode().IsRegular() {
				a.skip(p, "a folder or special file is in the way")
				continue
			}
			if !had || cur.Size != st.Size() || cur.Mtime != st.ModTime().UnixNano() {
				h, err := sha256File(full)
				if err != nil {
					note(err)
					continue
				}
				if h == f.Blob {
					a.setIndex(p, idxEntry{Blob: h, Size: st.Size(), Mtime: st.ModTime().UnixNano()})
					continue
				}
				if !had || h != cur.Blob {
					// An edit we have not published: publish it first (it becomes a conflict
					// copy if it really collides), never overwrite it.
					note(a.localChange(ctx, p))
					did = true
					continue
				}
			}
		}
		if err := a.materialise(ctx, p, f); err != nil {
			a.logf("files: write %s: %v", p, err)
			note(err)
			continue
		}
		did = true
	}
	// Folders.
	for d := range v.explicit {
		if a.isKept(d) && ValidatePath(d, false) == nil && !a.ign.ignored(d) {
			_ = os.MkdirAll(a.disk(d), 0o755)
		}
	}
	// Deletions: indexed files that left the tree.
	a.mu.RLock()
	var stale []string
	for k := range a.index {
		if v.files[k] == nil {
			stale = append(stale, k)
		}
	}
	a.mu.RUnlock()
	sort.Strings(stale)
	for _, k := range stale {
		e, _ := a.idx(k)
		if under(k, sharedDir) {
			if _, _, _, ok := v.sharedRoot(k); !ok {
				a.dropIndex(k) // the share is gone: what we copied stays, no longer tracked
				continue
			}
		}
		if !a.isKept(k) {
			a.dropIndex(k)
			continue
		}
		full := a.disk(k)
		h, err := sha256File(full)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			a.dropIndex(k)
		case err != nil:
			note(err)
		case h == e.Blob:
			a.dropIndex(k)
			if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
				note(err)
				continue
			}
			a.removeEmptyParents(k, v)
			did = true
		default:
			note(a.localChange(ctx, k)) // edited here while deleted elsewhere: the edit wins
			did = true
		}
	}
	return did, first
}

// removeEmptyParents deletes now-empty folders above p, except explicit ones and the root.
func (a *App) removeEmptyParents(p string, v *view) {
	for d := parentOf(p); d != ""; d = parentOf(d) {
		if v.explicit[d] || v.roots[d] != nil {
			return
		}
		if os.Remove(a.disk(d)) != nil {
			return
		}
	}
}

// materialise writes one tree file to disk: temp file in the same folder, mtime from the entry,
// atomic rename, then the index (so the watcher sees nothing new). a.syncMu held.
func (a *App) materialise(ctx context.Context, p string, f *vfile) error {
	full := a.disk(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	rc, err := a.api.GetBlob(ctx, f.Blob, sol.BlobOptions{From: f.From})
	if err != nil {
		return err
	}
	defer rc.Close()
	a.mu.Lock()
	a.stats.BlobFetches++
	a.mu.Unlock()
	tmp, err := os.CreateTemp(filepath.Dir(full), ".sol-*"+tempSuffix)
	if err != nil {
		return err
	}
	tname := tmp.Name()
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), rc)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && "sha256:"+hex.EncodeToString(h.Sum(nil)) != f.Blob {
		err = fmt.Errorf("blob content does not match its hash")
	}
	if err == nil {
		mt := time.UnixMilli(f.Mtime)
		if err = os.Chtimes(tname, mt, mt); err == nil {
			err = os.Rename(tname, full)
		}
	}
	if err != nil {
		os.Remove(tname)
		return err
	}
	st, err := os.Lstat(full)
	if err != nil {
		return err
	}
	a.setIndex(p, idxEntry{Blob: f.Blob, Size: st.Size(), Mtime: st.ModTime().UnixNano()})
	return nil
}

// evict removes the local copies under p after a "online only" choice: files whose content is
// what the tree says are deleted from disk without telling the feed; edited ones stay.
func (a *App) evict(ctx context.Context, p string) error {
	a.mu.RLock()
	var ks []string
	for k := range a.index {
		if under(k, p) && !a.keptLocked(k) {
			ks = append(ks, k)
		}
	}
	a.mu.RUnlock()
	sort.Strings(ks)
	v := a.getView()
	var first error
	for _, k := range ks {
		e, _ := a.idx(k)
		h, err := sha256File(a.disk(k))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			a.dropIndex(k)
		case err != nil:
			if first == nil {
				first = err
			}
		case h == e.Blob:
			a.dropIndex(k)
			_ = os.Remove(a.disk(k))
			a.removeEmptyParents(k, v)
		default:
			// Edited and not yet published: publish it, then leave it where it is.
			if err := a.localChange(ctx, k); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}
