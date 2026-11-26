package files

import (
	"context"
	"sort"
)

// Freeing the bytes of what was deleted forever (design/apps/FILES.md "Trash and versions").
//
// Feed entries are never removed, so the old put and delete entries of the tree feed name every
// content the folder ever held, and the daemon keeps a blob a feed of mine references. A purge
// therefore ends with a release entry (sol.ReleaseField): it lists every blob the tree feed
// references that nothing still uses, so every device of mine and the anchor delete those bytes.
// "Still uses" is decided on the fold, which is the same on every device: a head of a file (and,
// for a conflict, the base a merge needs), a Trash item, a kept version. Content is addressed by
// hash, so a blob is released only when none of them holds the same bytes.
//
// My share feeds (the mirrors a contact reads) reference the contents of the shared folder; they
// release what is no longer in the folder at the same moment, so a contact can no longer fetch a
// content that left the folder and was deleted forever (or was never more than a Trash item).
//
// Two devices may cross: one releases a blob while another, offline, puts the same bytes at
// some path. The release then sorts after that put and wins, although the fold needs the blob.
// Whichever device sees both writes a "keep" entry that names the blob again (keepNeeded), well
// within the daemon's grace before it deletes released bytes.

// Ops of the release machinery.
const (
	opRelease = "release" // tree or share feed: {"op":"release","release":[blobs]}
	opKeep    = "keep"    // tree or share feed: {"op":"keep","keep":[blobs]} names released blobs again
)

// releaseChunk bounds one release or keep entry (about 75 bytes a blob, well under 64 KiB).
const releaseChunk = 400

// neededBlobs is every blob my own tree still uses: the heads of every file, the base a conflict
// merge starts from, every Trash item and every version kept. a.mu held.
func (a *App) neededBlobs() map[string]bool {
	need := map[string]bool{}
	for _, heads := range a.own.files {
		for _, v := range heads {
			need[v.Blob] = true
			if len(heads) > 1 && v.Base != "" {
				need[v.Base] = true
			}
		}
	}
	if h := a.own.hist; h != nil {
		for _, it := range h.trash {
			if !it.Dir {
				need[it.Blob] = true
			}
		}
		for _, vs := range h.vers {
			for _, v := range vs {
				need[v.Blob] = true
			}
		}
	}
	delete(need, "")
	return need
}

// liveUnder is every blob of the files under dir (all heads): what a share of dir shows. a.mu held.
func (a *App) liveUnder(dir string) map[string]bool {
	live := map[string]bool{}
	for p, heads := range a.own.files {
		if p != dir && under(p, dir) {
			for _, v := range heads {
				live[v.Blob] = true
			}
		}
	}
	return live
}

// blobPlan is what one feed of mine should release or name again.
type blobPlan struct {
	feed string // feed name
	addr string
	ids  []string
}

// mirrorsLocked lists my share feeds with what each still shows (nil: the share is gone, it shows
// nothing). a.mu held.
func (a *App) mirrorsLocked() map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for id := range a.myShareID {
		var live map[string]bool
		if sh := a.shares[id]; sh != nil {
			live = a.liveUnder(sh.Path)
		}
		out[sharePrefix+id] = live
	}
	return out
}

// releaseUnused appends release entries for every blob my tree feed and my share feeds reference
// but no longer use. a.syncMu held.
func (a *App) releaseUnused(ctx context.Context) error {
	a.mu.RLock()
	need := a.neededBlobs()
	var plans []blobPlan
	add := func(feed string, keep map[string]bool) {
		addr := a.addr(feed)
		var ids []string
		for _, h := range a.blobs.Hashes(addr) {
			if !keep[h] {
				ids = append(ids, h)
			}
		}
		if len(ids) > 0 {
			plans = append(plans, blobPlan{feed: feed, addr: addr, ids: ids})
		}
	}
	add(treeFeed, need)
	for feed, live := range a.mirrorsLocked() {
		add(feed, live)
	}
	a.mu.RUnlock()
	return a.appendPlans(ctx, plans, func(ids []string) body { return body{Op: opRelease, Release: ids} })
}

// keepNeeded names again, in a feed of mine, every blob it released although the fold still uses
// it (a release that crossed a new use of the same bytes on another device). a.syncMu held.
func (a *App) keepNeeded(ctx context.Context) error {
	a.mu.RLock()
	var plans []blobPlan
	check := func(feed string, use map[string]bool) {
		addr := a.addr(feed)
		var ids []string
		for h := range use {
			if a.blobs.Released(addr, h) {
				ids = append(ids, h)
			}
		}
		if len(ids) > 0 {
			sort.Strings(ids)
			plans = append(plans, blobPlan{feed: feed, addr: addr, ids: ids})
		}
	}
	check(treeFeed, a.neededBlobs())
	for feed, live := range a.mirrorsLocked() {
		check(feed, live)
	}
	a.mu.RUnlock()
	if len(plans) > 0 {
		a.logf("files: naming %d released blobs again (a release crossed a new use)", len(plans))
	}
	return a.appendPlans(ctx, plans, func(ids []string) body { return body{Op: opKeep, Keep: ids} })
}

// appendPlans writes one entry per chunk of each plan and folds them, so the next look at the
// index sees them. a.syncMu held.
func (a *App) appendPlans(ctx context.Context, plans []blobPlan, mk func([]string) body) error {
	sort.Slice(plans, func(i, j int) bool { return plans[i].feed < plans[j].feed })
	for _, p := range plans {
		for ids := p.ids; len(ids) > 0; {
			n := min(len(ids), releaseChunk)
			b := mk(ids[:n])
			b.Dev, b.At = a.devName, a.nowMs()
			if _, err := a.api.Append(ctx, p.feed, b); err != nil {
				return err
			}
			a.mu.Lock()
			if p.feed == treeFeed {
				a.stats.TreeAppends++
			} else {
				a.stats.ShareAppends++
			}
			a.mu.Unlock()
			ids = ids[n:]
		}
		if _, err := a.pull(ctx, p.addr); err != nil {
			return err
		}
	}
	return nil
}

// trimVersions purges, on my first device, the versions beyond the per-file cap (hidden already
// on every device), so their bytes are released too. a.syncMu held.
func (a *App) trimVersions(ctx context.Context) error {
	a.mu.RLock()
	ids := a.own.hist.overCap(func(p string) map[string]bool {
		cur := map[string]bool{}
		for _, v := range a.own.files[p] {
			cur[v.Blob] = true
		}
		return cur
	}, a.nowMs())
	a.mu.RUnlock()
	if len(ids) == 0 || !a.firstDevice(ctx) {
		return nil
	}
	return a.purge(ctx, ids)
}
