package files

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/apv022/sol/sdk/go/sol"
)

// commit appends one change (paths in the unified tree) to the right feed and folds it right
// away, so what an action returns is already visible. Changes under a shared folder I own are
// mirrored into that share's feed; changes inside a folder shared with me go to my own
// same-named feed. a.syncMu held.
func (a *App) commit(ctx context.Context, b body) error {
	b.Dev = a.devName
	if b.At == 0 {
		b.At = a.nowMs()
	}
	if b.Mtime == 0 {
		b.Mtime = b.At
	}
	v := a.getView()
	rs, relp, err := v.route(b.Path)
	if err != nil {
		return err
	}
	if rs != nil {
		nb := b
		nb.Path = relp
		nb.In = ""
		if b.In != "" {
			if rsIn, relIn, err := v.route(b.In); err == nil && rsIn == rs {
				nb.In = relIn // the owner keeps it in their Trash under the same deletion
			}
		}
		if b.To != "" {
			rs2, rel2, err := v.route(b.To)
			if err != nil {
				return err
			}
			if rs2 != rs {
				return sol.Errorf("bad_request", "a move cannot cross between shared folders")
			}
			nb.To = rel2
		}
		if err := a.shareBack(ctx, rs); err != nil {
			return err
		}
		if _, err := a.api.Append(ctx, sharePrefix+rs.ID, nb); err != nil {
			return err
		}
		a.mu.Lock()
		a.stats.ShareAppends++
		a.mu.Unlock()
		_, err := a.pull(ctx, a.addr(sharePrefix+rs.ID))
		return err
	}
	var pre []*vfile
	if b.Op == opMove {
		if rs2, _, err := v.route(b.To); err != nil || rs2 != nil {
			return sol.Errorf("bad_request", "a move cannot cross into a shared folder")
		}
		pre = v.filesUnder(b.Path)
	}
	if _, err := a.api.Append(ctx, treeFeed, b); err != nil {
		return err
	}
	a.mu.Lock()
	a.stats.TreeAppends++
	a.mu.Unlock()
	if _, err := a.pull(ctx, a.addr(treeFeed)); err != nil {
		return err
	}
	return a.mirror(ctx, b, pre)
}

// shareBack makes my co-author feed of a folder shared with me for writing readable by its
// owner (API.md §5): their write share alone grants them nothing of mine, so my edits reach
// them only once I share the feed back. Once per share and run; sharing is idempotent.
func (a *App) shareBack(ctx context.Context, rs *recvShare) error {
	a.mu.RLock()
	done := a.sharedBack[rs.ID]
	a.mu.RUnlock()
	if done || rs.Owner == "" {
		return nil
	}
	if err := a.api.Share(ctx, sol.ShareRequest{Feed: sharePrefix + rs.ID, People: []string{rs.Owner}, Mode: sol.ModeRead}); err != nil {
		return err
	}
	a.mu.Lock()
	a.sharedBack[rs.ID] = true
	a.mu.Unlock()
	return nil
}

// mirror copies a change under a folder I share into that share's feed (relative paths).
func (a *App) mirror(ctx context.Context, b body, pre []*vfile) error {
	a.mu.RLock()
	shs := make([]*shareRec, 0, len(a.shares))
	for _, s := range a.shares {
		shs = append(shs, s)
	}
	a.mu.RUnlock()
	sort.Slice(shs, func(i, j int) bool { return shs[i].ID < shs[j].ID })
	for _, sh := range shs {
		in := func(p string) bool { return p != sh.Path && under(p, sh.Path) }
		var out []body
		switch b.Op {
		case opPut, opDelete, opMkdir:
			if in(b.Path) {
				nb := b
				nb.Path = rel(b.Path, sh.Path)
				nb.In, nb.Trash = "", "" // Trash is mine: the deleted root may be outside their folder
				out = append(out, nb)
			}
		case opMove:
			switch {
			case in(b.Path) && in(b.To):
				nb := b
				nb.Path, nb.To = rel(b.Path, sh.Path), rel(b.To, sh.Path)
				out = append(out, nb)
			case in(b.Path):
				for _, f := range pre {
					out = append(out, body{Op: opDelete, Path: rel(f.Path, sh.Path), Base: f.Blob, Mtime: b.Mtime, Dev: b.Dev})
				}
			case in(b.To):
				for _, f := range a.getView().filesUnder(b.To) {
					out = append(out, body{Op: opPut, Path: rel(f.Path, sh.Path), Blob: f.Blob, Size: f.Size, Mtime: f.Mtime, Dev: b.Dev})
				}
			}
		}
		for _, nb := range out {
			if _, err := a.api.Append(ctx, sharePrefix+sh.ID, nb); err != nil {
				return err
			}
			a.mu.Lock()
			a.stats.ShareAppends++
			a.mu.Unlock()
		}
	}
	return nil
}

func newShareID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// resolvePeople maps ids, petnames or names to person ids.
func (a *App) resolvePeople(ctx context.Context, who []string) ([]sol.Contact, error) {
	ps, err := a.api.People(ctx)
	if err != nil {
		return nil, err
	}
	var out []sol.Contact
	seen := map[string]bool{}
	for _, w := range who {
		var hit *sol.Contact
		for i := range ps {
			p := &ps[i]
			if p.ID == w || strings.EqualFold(p.Petname, w) || strings.EqualFold(p.Name, w) {
				hit = p
				break
			}
		}
		if hit == nil {
			return nil, sol.Errorf("not_found", fmt.Sprintf("no contact %q", w))
		}
		if !seen[hit.ID] {
			seen[hit.ID] = true
			out = append(out, *hit)
		}
	}
	return out, nil
}

// shareFolder gives people access to one of my folders (a.syncMu held).
func (a *App) shareFolder(ctx context.Context, dir, mode string, people []sol.Contact) (string, error) {
	// Anyone already in the other mode moves over.
	a.mu.RLock()
	var existing *shareRec
	var others []*shareRec
	for _, s := range a.shares {
		if s.Path != dir {
			continue
		}
		if s.Mode == mode {
			existing = s
		} else {
			others = append(others, s)
		}
	}
	a.mu.RUnlock()
	ids := make([]string, 0, len(people))
	for _, p := range people {
		ids = append(ids, p.ID)
	}
	for _, o := range others {
		var move []string
		for _, id := range ids {
			if o.People[id] {
				move = append(move, id)
			}
		}
		if len(move) > 0 {
			if err := a.unshareFolder(ctx, o, move); err != nil {
				return "", err
			}
		}
	}
	id := ""
	if existing == nil {
		id = newShareID()
		seed, err := a.seedShareFeed(ctx, id, dir, mode)
		if err != nil {
			return "", err
		}
		_ = seed
	} else {
		id = existing.ID
	}
	if _, err := a.api.Append(ctx, treeFeed, body{Op: opShare, ID: id, Path: dir, Mode: mode, People: ids, Mtime: a.nowMs(), Dev: a.devName}); err != nil {
		return "", err
	}
	a.mu.Lock()
	a.stats.TreeAppends++
	a.mu.Unlock()
	if _, err := a.pull(ctx, a.addr(treeFeed)); err != nil {
		return "", err
	}
	if err := a.api.Share(ctx, sol.ShareRequest{Feed: sharePrefix + id, People: ids, Mode: mode}); err != nil {
		return "", err
	}
	return id, nil
}

// seedShareFeed writes the first entries of a new share feed: its name, then the folder's
// current contents. It runs before the feed is shared, so the first read is complete.
func (a *App) seedShareFeed(ctx context.Context, id, dir, mode string) (int, error) {
	feed := sharePrefix + id
	n := 0
	add := func(b body) error {
		b.Dev = a.devName
		_, err := a.api.Append(ctx, feed, b)
		if err == nil {
			n++
			a.mu.Lock()
			a.stats.ShareAppends++
			a.mu.Unlock()
		}
		return err
	}
	if err := add(body{Op: opMeta, Name: baseOf(dir), Mode: mode}); err != nil {
		return n, err
	}
	v := a.getView()
	for d := range v.explicit {
		if d != dir && under(d, dir) {
			if err := add(body{Op: opMkdir, Path: rel(d, dir), Mtime: a.nowMs()}); err != nil {
				return n, err
			}
		}
	}
	for _, f := range v.filesUnder(dir) {
		if err := add(body{Op: opPut, Path: rel(f.Path, dir), Blob: f.Blob, Size: f.Size, Mtime: f.Mtime}); err != nil {
			return n, err
		}
	}
	return n, nil
}

// unshareFolder removes people (all when empty) from one share.
func (a *App) unshareFolder(ctx context.Context, sh *shareRec, ids []string) error {
	if len(ids) == 0 {
		for id := range sh.People {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	if len(ids) == 0 {
		return nil
	}
	if err := a.api.Unshare(ctx, sol.ShareRequest{Feed: sharePrefix + sh.ID, People: ids, Mode: sh.Mode}); err != nil {
		return err
	}
	if _, err := a.api.Append(ctx, treeFeed, body{Op: opUnshare, ID: sh.ID, People: ids, Mtime: a.nowMs(), Dev: a.devName}); err != nil {
		return err
	}
	a.mu.Lock()
	a.stats.TreeAppends++
	a.mu.Unlock()
	_, err := a.pull(ctx, a.addr(treeFeed))
	return err
}
