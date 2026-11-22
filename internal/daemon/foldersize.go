package daemon

import (
	"context"
	"io/fs"
	"path/filepath"
	"sync"
	"time"
)

// folderRefresh is how stale the folder size may get before a status request starts a new
// measurement.
var folderRefresh = time.Minute

// folderSize caches the size of the synced folder. Walking the tree costs, so nothing walks on
// a status call: the daemon measures once at start, and a status request that finds the value
// older than folderRefresh starts one more measurement in the background and answers with the
// cached number. No timers: it only ever runs because someone asked.
type folderSize struct {
	mu      sync.Mutex
	size    uint64
	at      time.Time // when the last measurement finished; zero until the first one
	running bool
}

// get returns the cached size and whether it has been measured, starting a background
// measurement when none has run or the value is stale.
func (f *folderSize) get(root string, ctx context.Context, wg *sync.WaitGroup) (uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running && ctx.Err() == nil && (f.at.IsZero() || time.Since(f.at) >= folderRefresh) {
		f.running = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, ok := dirSize(ctx, root)
			f.mu.Lock()
			defer f.mu.Unlock()
			f.running = false
			if ok {
				f.size, f.at = n, time.Now()
			}
		}()
	}
	return f.size, !f.at.IsZero()
}

// dirSize sums the sizes of the regular files under root. Unreadable entries are skipped;
// ok is false when the context ended first.
func dirSize(ctx context.Context, root string) (n uint64, ok bool) {
	_ = filepath.WalkDir(root, func(_ string, e fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || !e.Type().IsRegular() {
			return nil
		}
		if fi, err := e.Info(); err == nil {
			n += uint64(fi.Size())
		}
		return nil
	})
	return n, ctx.Err() == nil
}
