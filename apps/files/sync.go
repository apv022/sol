package files

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

// initialSync folds every feed, publishes what changed on disk while the app was not running,
// and makes the disk match the tree.
func (a *App) initialSync(ctx context.Context) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	_, err := a.pullAll(ctx)
	a.ign.reload(a.root)
	if serr := a.scan(ctx); serr != nil && err == nil {
		err = serr
	}
	if rerr := a.reconcile(ctx); rerr != nil && err == nil {
		err = rerr
	}
	a.afterPull(ctx)
	return err
}

func (a *App) startWatcher() {
	if a.cfg.NoWatch {
		return
	}
	w, err := newWatcher(a.root, func(p string) bool { a.ign.reload(a.root); return a.ign.ignored(p) })
	if err != nil {
		a.logf("files: no file watcher, scanning every %s: %v", a.cfg.ScanEvery, err)
		return
	}
	a.watch, a.watching = w, true
}

// loop is the one goroutine that reacts to API events, folder events and retries.
func (a *App) loop(evs <-chan sol.Event) {
	defer a.wg.Done()
	ctx := a.ctx
	timers := map[string]*time.Timer{}
	defer func() {
		for _, t := range timers {
			t.Stop()
		}
	}()
	var fsEvents <-chan string
	if a.watch != nil {
		fsEvents = a.watch.Events()
	}
	var scanTick <-chan time.Time
	if !a.watching {
		t := time.NewTicker(a.cfg.ScanEvery)
		defer t.Stop()
		scanTick = t.C
	}
	var retry <-chan time.Time
	// One timer to the next Trash or version expiry, set again whenever the tree changes.
	var expiry *time.Timer
	defer func() {
		if expiry != nil {
			expiry.Stop()
		}
	}()
	var expC <-chan time.Time
	expRev, expStale := uint64(0), true
	arm := func() {
		a.mu.RLock()
		need := len(a.pending) > 0 || a.pullErr
		rev := a.own.Rev()
		a.mu.RUnlock()
		if need && retry == nil {
			retry = after(a.cfg.RetryEvery)
		}
		if expStale || rev != expRev {
			expRev, expStale = rev, false
			if expiry != nil {
				expiry.Stop()
				expiry, expC = nil, nil
			}
			if next := a.nextExpiry(); next > 0 {
				expiry = time.NewTimer(time.Duration(max(next-a.nowMs(), 0)+1) * time.Millisecond)
				expC = expiry.C
			}
		}
	}
	arm()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-evs:
			if !ok {
				if ctx.Err() != nil {
					return
				}
				evs = a.resubscribe(ctx)
				continue
			}
			a.onEvent(ctx, ev)
		case p := <-fsEvents:
			a.debounce(ctx, timers, p)
		case p := <-a.fsq:
			delete(timers, p)
			a.onLocal(ctx, p)
		case <-scanTick:
			a.onLocal(ctx, "")
		case <-a.wake:
		case <-retry:
			retry = nil
			a.onRetry(ctx)
		case <-expC:
			expiry, expC, expStale = nil, nil, true
			a.expire(ctx)
		}
		arm()
	}
}

func (a *App) resubscribe(ctx context.Context) <-chan sol.Event {
	for ctx.Err() == nil {
		evs, err := a.api.Events(ctx, sol.EventOptions{People: true, Inbox: true})
		if err == nil {
			return evs
		}
		select {
		case <-ctx.Done():
		case <-after(a.cfg.RetryEvery):
		}
	}
	return nil
}

func (a *App) debounce(ctx context.Context, timers map[string]*time.Timer, p string) {
	if p == "" {
		a.fireLocal("")
		return
	}
	if t := timers[p]; t != nil {
		t.Reset(a.cfg.Debounce)
		return
	}
	timers[p] = time.AfterFunc(a.cfg.Debounce, func() { a.fireLocal(p) })
}

func (a *App) fireLocal(p string) {
	select {
	case a.fsq <- p:
	case <-a.ctx.Done():
	}
}

// onLocal handles a (debounced) folder event.
func (a *App) onLocal(ctx context.Context, p string) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	a.mu.Lock()
	a.busy++
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.busy--
		a.mu.Unlock()
	}()
	err := a.localChange(ctx, p)
	if err != nil {
		if p != "" && (transient(err) || errors.Is(err, errUnstable)) {
			a.mu.Lock()
			a.pending[p] = true
			a.mu.Unlock()
		}
		a.noteErr(err)
		return
	}
	a.mu.Lock()
	delete(a.pending, p)
	a.lastErr = ""
	a.mu.Unlock()
	_ = a.reconcile(ctx)
	a.afterPull(ctx)
}

// onEvent handles one API event.
func (a *App) onEvent(ctx context.Context, ev sol.Event) {
	switch ev.Type {
	case sol.EventEntry:
		a.syncFeed(ctx, ev.Entry.Feed)
	case sol.EventFeed:
		if ev.Feed.Readable {
			a.syncFeed(ctx, ev.Feed.Feed)
		} else {
			a.markRevoked(ev.Feed.Feed)
			a.syncFeed(ctx, "")
		}
	case sol.EventPerson:
		a.refreshPeople(ctx)
		a.syncFeed(ctx, "")
	case sol.EventInbox:
		if ev.Inbox != nil {
			a.onInbox(ctx, *ev.Inbox)
		}
	case sol.EventError:
		a.logf("files: event stream: %s", ev.Err)
	}
}

// syncFeed folds one feed (or none) and reconciles the disk.
func (a *App) syncFeed(ctx context.Context, addr string) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	a.mu.Lock()
	a.busy++
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.busy--
		a.mu.Unlock()
	}()
	if addr != "" {
		if _, err := a.pull(ctx, addr); err != nil {
			a.mu.Lock()
			a.pullErr = true
			a.mu.Unlock()
			a.noteErr(err)
			return
		}
	}
	if err := a.reconcile(ctx); err != nil && transient(err) {
		a.noteErr(err)
	}
	a.afterPull(ctx)
}

// onRetry tries again whatever failed because the daemon or a peer was unreachable.
func (a *App) onRetry(ctx context.Context) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	a.mu.Lock()
	a.busy++
	var ps []string
	for p := range a.pending {
		ps = append(ps, p)
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.busy--
		a.mu.Unlock()
	}()
	sort.Strings(ps)
	failed := false
	if _, err := a.pullAll(ctx); err != nil {
		failed = true
		a.noteErr(err)
	}
	for _, p := range ps {
		if err := a.localChange(ctx, p); err != nil {
			failed = true
			a.noteErr(err)
			continue
		}
		a.mu.Lock()
		delete(a.pending, p)
		a.mu.Unlock()
	}
	if err := a.reconcile(ctx); err != nil && transient(err) {
		failed = true
		a.noteErr(err)
	}
	a.mu.Lock()
	a.pullErr = failed
	if !failed {
		a.lastErr = ""
	}
	a.mu.Unlock()
	a.afterPull(ctx)
}

// afterPull posts an Inbox notice for each newly seen conflict. Only my first device (by id)
// posts, so one conflict makes one notice.
func (a *App) afterPull(ctx context.Context) {
	if a.tryMerges(ctx) {
		return // the merge's own pull runs afterPull again with the new state
	}
	a.notifySends(ctx)
	v := a.getView()
	a.mu.RLock()
	var fresh []conflictInfo
	var keys []string
	for _, c := range v.conflicts {
		k := conflictKey(c)
		if !a.noticed[k] {
			fresh = append(fresh, c)
			keys = append(keys, k)
		}
	}
	a.mu.RUnlock()
	if len(fresh) == 0 {
		return
	}
	if ds, err := a.api.Devices(ctx); err == nil {
		for _, d := range ds {
			if d.ID < a.devID {
				return // not my job: my first device posts it
			}
		}
	}
	for i, c := range fresh {
		heads := c.Heads
		names := make([]string, 0, len(heads))
		for _, h := range heads {
			names = append(names, h.Dev)
		}
		_, err := a.api.PostInbox(ctx, sol.InboxPost{Kind: "notice", To: "self",
			Title: "Edit conflict: " + baseOf(c.Path),
			Body:  fmt.Sprintf("%s was edited on %s at the same time. Both versions are kept; open Files > Conflicts to choose.", c.Path, strings.Join(names, " and "))})
		if err != nil {
			a.logf("files: inbox: %v", err)
			continue
		}
		a.mu.Lock()
		a.noticed[keys[i]] = true
		a.mu.Unlock()
		a.markDirty()
	}
}

func conflictKey(c conflictInfo) string {
	bs := make([]string, 0, len(c.Heads))
	for _, h := range c.Heads {
		bs = append(bs, h.Blob)
	}
	sort.Strings(bs)
	return c.Share + "|" + c.Path + "|" + strings.Join(bs, ",")
}

// Rescan walks the whole folder now (the on-demand scan of the spec) and syncs.
func (a *App) Rescan(ctx context.Context) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if err := a.scan(ctx); err != nil {
		return err
	}
	return a.reconcile(ctx)
}

// Sync folds every feed and reconciles the folder now.
func (a *App) Sync(ctx context.Context) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if _, err := a.pullAll(ctx); err != nil {
		return err
	}
	err := a.reconcile(ctx)
	a.afterPull(ctx)
	return err
}

func after(d time.Duration) <-chan time.Time { return time.After(d) }
