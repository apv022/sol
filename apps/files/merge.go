package files

import (
	"bytes"
	"context"
	"io"
	"unicode/utf8"

	"github.com/apv022/sol/apps/files/merge"
	"github.com/apv022/sol/sdk/go/sol"
)

// maxMergeSize bounds each of the three versions a text merge reads.
const maxMergeSize = 4 << 20

// tryMerges looks for paths of my own tree with exactly two concurrent heads that started from
// the same blob and, for text files, replaces them with their three-way merge. It reports
// whether it appended anything.
//
// Determinism: every device sees the same two heads, orders them by the tree's last-writer
// order ("mine" is the older, "theirs" the newer) and stamps the result with the newer mtime,
// so all devices compute and append the same blob; a second append of the same blob just
// replaces the first. No loop: the merged put supersedes both heads, so the path has one head
// afterwards, and a failed attempt is remembered per pair of blobs and never retried.
func (a *App) tryMerges(ctx context.Context) bool {
	v := a.getView()
	did := false
	for _, c := range v.conflicts {
		if c.Share != "" || len(c.Heads) != 2 {
			continue
		}
		newer, older := c.Heads[0], c.Heads[1] // Heads is newest first
		if older.Base == "" || older.Base != newer.Base || !sol.ValidHash(older.Base) {
			continue // no common base (both created it): a real conflict
		}
		key := conflictKey(c)
		a.mu.Lock()
		tried := a.mergeTried[key]
		a.mergeTried[key] = true
		a.mu.Unlock()
		if tried {
			continue
		}
		if older.Size > maxMergeSize || newer.Size > maxMergeSize {
			continue
		}
		base, ok1 := a.textBlob(ctx, older.Base, older, newer)
		mine, ok2 := a.textBlob(ctx, older.Blob, older)
		theirs, ok3 := a.textBlob(ctx, newer.Blob, newer)
		if !ok1 || !ok2 || !ok3 {
			continue
		}
		out, ok := merge.Merge(base, mine, theirs)
		if !ok || !isText(out) {
			continue
		}
		info, err := a.api.PutBlob(ctx, bytes.NewReader(out), baseOf(c.Path))
		if err != nil {
			a.logf("files: merge %s: %v", c.Path, err)
			a.mu.Lock()
			delete(a.mergeTried, key) // transient: try again on the next pull
			a.mu.Unlock()
			continue
		}
		mt := newer.Mtime
		if older.Mtime > mt {
			mt = older.Mtime
		}
		b := body{Op: opPut, Path: c.Path, Blob: info.Hash, Size: int64(len(out)), Mtime: mt,
			Base: newer.Blob, Also: []string{older.Blob}}
		if err := a.commit(ctx, b); err != nil {
			a.logf("files: merge %s: %v", c.Path, err)
			a.mu.Lock()
			delete(a.mergeTried, key)
			a.mu.Unlock()
			continue
		}
		did = true
	}
	return did
}

// textBlob reads a blob (trying each version's source in turn) and reports whether it is
// small valid UTF-8 without NUL bytes.
func (a *App) textBlob(ctx context.Context, hash string, from ...version) ([]byte, bool) {
	for _, f := range from {
		rc, err := a.api.GetBlob(ctx, hash, sol.BlobOptions{From: f.From})
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxMergeSize+1))
		rc.Close()
		if err != nil || len(b) > maxMergeSize {
			continue
		}
		return b, isText(b)
	}
	return nil, false
}

func isText(b []byte) bool { return utf8.Valid(b) && bytes.IndexByte(b, 0) < 0 }
