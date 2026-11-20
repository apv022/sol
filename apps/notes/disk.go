package notes

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxNote is the largest note Notes reads or writes. Bigger files are left alone (Files still
// syncs them) so one huge file cannot stall the index.
const maxNote = 4 << 20

func hidden(name string) bool { return strings.HasPrefix(name, ".") }

// ignorePath says whether a changed path is irrelevant: hidden files (editor and Files temp
// files) and anything inside an attachments folder.
func ignorePath(rel string) bool {
	for _, s := range strings.Split(rel, "/") {
		if hidden(s) || s == AttachDir {
			return true
		}
	}
	return false
}

// writeAtomic replaces the file at full: a temp file in the same folder, then a rename. The temp
// name starts with ".#", which Files ignores, so a half-written note is never synced.
func writeAtomic(full string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(full), ".#notes-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, full); err != nil {
		return err
	}
	ok = true
	return nil
}

// indexFileLocked (re)indexes one note from disk if its size or mtime changed. wmu is held.
func (a *App) indexFileLocked(rel string, fi fs.FileInfo) {
	if fi.Size() > maxNote {
		a.idx.remove(rel)
		return
	}
	if d := a.idx.get(rel); d != nil && d.mtime == fi.ModTime().UnixNano() && d.size == fi.Size() {
		return
	}
	full := filepath.Join(a.notes, filepath.FromSlash(rel))
	b, err := os.ReadFile(full)
	if err != nil {
		return // vanished; its delete event follows
	}
	a.idx.put(buildDoc(rel, string(b), fi.ModTime().UnixNano(), int64(len(b)), etagOf(b)))
}

// scanLocked indexes every note under rel ("" is Notes/) and, when prune is set, forgets notes
// and folders under it that are gone. wmu is held.
func (a *App) scanLocked(rel string, prune bool) {
	seen := map[string]bool{}
	dirs := map[string]bool{}
	base := filepath.Join(a.notes, filepath.FromSlash(rel))
	_ = filepath.WalkDir(base, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		r, e := filepath.Rel(a.notes, full)
		if e != nil {
			return nil
		}
		r = filepath.ToSlash(r)
		if r == "." {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || ignorePath(r) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			dirs[r] = true
			a.idx.addDir(r)
			return nil
		}
		if !isNoteName(r) || !d.Type().IsRegular() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			seen[r] = true
			a.indexFileLocked(r, fi)
		}
		return nil
	})
	if !prune {
		return
	}
	prefix := rel + "/"
	for _, p := range a.idx.paths() {
		if (rel == "" || strings.HasPrefix(p, prefix)) && !seen[p] {
			a.idx.remove(p)
		}
	}
	for _, dd := range a.idx.folders() {
		if (rel == "" || strings.HasPrefix(dd, prefix)) && !dirs[dd] {
			a.idx.remove(dd)
		}
	}
}

func (a *App) fullScan() error {
	if _, err := os.Stat(a.notes); err != nil {
		return err
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	a.scanLocked("", true)
	return nil
}

// Refresh re-reads one path (a note or a folder; "" is everything) from disk into the index. The
// watcher calls it; tests and callers that just changed a file can too.
func (a *App) Refresh(rel string) {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	a.refreshLocked(rel)
}

func (a *App) refreshLocked(rel string) {
	if rel == "" {
		a.scanLocked("", true)
		return
	}
	if ignorePath(rel) {
		return
	}
	full := filepath.Join(a.notes, filepath.FromSlash(rel))
	fi, err := os.Lstat(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		a.idx.remove(rel)
	case err != nil || fi.Mode()&fs.ModeSymlink != 0:
	case fi.IsDir():
		a.scanLocked(rel, true)
	case isNoteName(rel) && fi.Mode().IsRegular():
		a.indexFileLocked(rel, fi)
	}
}

func (a *App) startWatcher() {
	if a.cfg.NoWatch {
		return
	}
	w, err := newWatcher(a.notes, ignorePath)
	if err != nil {
		a.logf("notes: no watcher, scanning instead: %v", err)
		return
	}
	a.watch, a.watching = w, true
}

// loop applies watcher events (debounced) and does a periodic full scan as a safety net.
func (a *App) loop(ctx context.Context) {
	defer a.wg.Done()
	every := a.cfg.ScanEvery
	if every <= 0 {
		every = 10 * time.Second
		if a.watching {
			every = time.Minute
		}
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	var evs <-chan string
	if a.watch != nil {
		evs = a.watch.Events()
	}
	pending := map[string]bool{}
	var timer *time.Timer
	var fire <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-evs:
			pending[p] = true
			if fire == nil {
				timer = time.NewTimer(a.cfg.Debounce)
				fire = timer.C
			}
		case <-fire:
			fire, timer = nil, nil
			batch := pending
			pending = map[string]bool{}
			if batch[""] || len(batch) > 200 {
				a.Refresh("")
				continue
			}
			for p := range batch {
				a.Refresh(p)
			}
		case <-tick.C:
			a.Refresh("")
		}
	}
}

// ---------- pins ----------

func (a *App) pinsFile() string {
	if a.data == "" {
		return ""
	}
	return filepath.Join(a.data, "pins.json")
}

func (a *App) loadPins() {
	f := a.pinsFile()
	if f == "" {
		return
	}
	var list []string
	if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &list) == nil {
		a.pinMu.Lock()
		for _, p := range list {
			a.pins[p] = true
		}
		a.pinMu.Unlock()
	}
}

// savePinsLocked writes the pin list (pinMu held). Pins are a per-device convenience, so the
// file lives in app data, not in the synced folder.
func (a *App) savePinsLocked() {
	f := a.pinsFile()
	if f == "" {
		return
	}
	list := make([]string, 0, len(a.pins))
	for p := range a.pins {
		list = append(list, p)
	}
	b, _ := json.Marshal(list)
	if err := writeAtomic(f, b); err != nil {
		a.logf("notes: saving pins: %v", err)
		return
	}
	_ = os.Chmod(f, 0o600)
}

func (a *App) isPinned(p string) bool {
	a.pinMu.Lock()
	defer a.pinMu.Unlock()
	return a.pins[p]
}

func (a *App) setPin(p string, on bool) {
	a.pinMu.Lock()
	defer a.pinMu.Unlock()
	if a.pins[p] == on {
		return
	}
	if on {
		a.pins[p] = true
	} else {
		delete(a.pins, p)
	}
	a.savePinsLocked()
}

// movePins follows a rename (a note or every note under a folder).
func (a *App) movePins(from, to string) {
	a.pinMu.Lock()
	defer a.pinMu.Unlock()
	changed := false
	for p := range a.pins {
		switch {
		case p == from:
			delete(a.pins, p)
			a.pins[to] = true
			changed = true
		case strings.HasPrefix(p, from+"/"):
			delete(a.pins, p)
			a.pins[to+p[len(from):]] = true
			changed = true
		}
	}
	if changed {
		a.savePinsLocked()
	}
}

func (a *App) dropPins(from string) {
	a.pinMu.Lock()
	defer a.pinMu.Unlock()
	changed := false
	for p := range a.pins {
		if p == from || strings.HasPrefix(p, from+"/") {
			delete(a.pins, p)
			changed = true
		}
	}
	if changed {
		a.savePinsLocked()
	}
}
