package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"testing"

	"github.com/apv022/sol/sdk/go/sol"
)

func blobOf(s string) string {
	d := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(d[:])
}

// released lists every blob the release entries of Alice's tree feed name.
func released(t *testing.T, n *node, person string) []string {
	t.Helper()
	var out []string
	for _, b := range treeEntries(t, n.api.API, person) {
		if b.Op == opRelease {
			out = append(out, b.Release...)
		}
	}
	return out
}

// TestPurgeReleasesBlobs: "delete forever" and "empty trash" free the bytes on every device of
// mine, but never a content another file, Trash item or version still holds (same bytes, same
// blob).
func TestPurgeReleasesBlobs(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.must(t, "files.write", map[string]any{"path": "x.txt", "text": "shared bytes"})
	a.must(t, "files.write", map[string]any{"path": "y.txt", "text": "shared bytes"})
	a.must(t, "files.write", map[string]any{"path": "z.txt", "text": "only z"})
	a.must(t, "files.write", map[string]any{"path": "t.txt", "text": "in trash too"})
	a.must(t, "files.write", map[string]any{"path": "u.txt", "text": "in trash too"})
	same, onlyZ, twice := blobOf("shared bytes"), blobOf("only z"), blobOf("in trash too")
	var ids []string
	for _, p := range []string{"x.txt", "z.txt", "t.txt", "u.txt"} {
		out := a.must(t, "files.delete", map[string]any{"paths": []string{p}})
		ids = append(ids, out["trash"].([]any)[0].(string))
	}
	waitFor(t, "b sees the deletions", func() bool { return len(trashRows(t, b)) == 4 })
	if !w.net.HasBlob(w.alice.ID, onlyZ) {
		t.Fatal("a Trash item's bytes are gone before any purge")
	}

	// x (same bytes as the live y), z, and t (same bytes as u, still in Trash) forever.
	b.must(t, "files.trash_purge", map[string]any{"ids": ids[:3]})
	if w.net.HasBlob(w.alice.ID, onlyZ) {
		t.Fatal("the purged content is still held")
	}
	if !w.net.HasBlob(w.alice.ID, same) || !w.net.HasBlob(w.alice.ID, twice) {
		t.Fatal("a content still used elsewhere was freed")
	}
	if rel := released(t, a, w.alice.ID); !slices.Contains(rel, onlyZ) || slices.Contains(rel, same) || slices.Contains(rel, twice) {
		t.Fatalf("released %v", rel)
	}
	// y still opens on both devices.
	waitFor(t, "y on b", func() bool { return b.has("y.txt", "shared bytes") })

	// Emptying Trash frees what only Trash held.
	a.must(t, "files.trash_empty", map[string]any{})
	if w.net.HasBlob(w.alice.ID, twice) || !w.net.HasBlob(w.alice.ID, same) {
		t.Fatal("empty trash: wrong blobs freed")
	}

	// The same bytes written again are a new reference: kept.
	a.must(t, "files.write", map[string]any{"path": "z2.txt", "text": "only z"})
	if !w.net.HasBlob(w.alice.ID, onlyZ) {
		t.Fatal("re-uploaded content not held")
	}
	a.must(t, "files.delete", map[string]any{"paths": []string{"y.txt"}})
	a.must(t, "files.trash_empty", map[string]any{})
	if w.net.HasBlob(w.alice.ID, same) || !w.net.HasBlob(w.alice.ID, onlyZ) {
		t.Fatal("second empty: wrong blobs freed")
	}
	waitFor(t, "z2 on b", func() bool { return b.has("z2.txt", "only z") })
}

// TestVersionDeleteAndCapRelease: deleting a version and the per-file cap free its bytes.
func TestVersionDeleteAndCapRelease(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	startNode(t, w.a2, nil) // either may be my first device, which trims to the cap
	for _, s := range []string{"v1", "v2", "v3"} {
		a.must(t, "files.write", map[string]any{"path": "doc.md", "text": s})
	}
	vs := versionsOfPath(t, a, "doc.md")
	if len(vs) != 2 || vs[1]["blob"] != blobOf("v1") {
		t.Fatalf("versions: %v", vs)
	}
	a.must(t, "files.version_delete", map[string]any{"path": "doc.md", "ids": []string{vs[1]["id"].(string)}})
	if w.net.HasBlob(w.alice.ID, blobOf("v1")) || !w.net.HasBlob(w.alice.ID, blobOf("v2")) {
		t.Fatal("version delete freed the wrong bytes")
	}
	// Cap 1: v2 is beyond it once v3 is replaced; my only device purges and frees it.
	a.must(t, "files.trash_settings", map[string]any{"versions": 1})
	a.must(t, "files.write", map[string]any{"path": "doc.md", "text": "v4"})
	waitFor(t, "v2 freed by the cap", func() bool { return !w.net.HasBlob(w.alice.ID, blobOf("v2")) })
	if !w.net.HasBlob(w.alice.ID, blobOf("v3")) || !w.net.HasBlob(w.alice.ID, blobOf("v4")) {
		t.Fatal("the cap freed a kept version or the current content")
	}
	if got := versionsOfPath(t, a, "doc.md"); len(got) != 1 || got[0]["blob"] != blobOf("v3") {
		t.Fatalf("versions after the cap: %v", got)
	}
}

// TestReleaseCrossingNewUseIsUndone: a release that crossed a new use of the same bytes (another
// device put them while it was offline) is answered with a keep entry naming them again.
func TestReleaseCrossingNewUseIsUndone(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	a.must(t, "files.write", map[string]any{"path": "keep.txt", "text": "still here"})
	h := blobOf("still here")
	tree := sol.Address(w.alice.ID, appID, treeFeed)
	// What a crossing release looks like from here: written by another device of mine.
	if _, err := w.a2.Client(appID).Append(context.Background(), treeFeed, body{Op: opRelease, Release: []string{h}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a keep entry", func() bool {
		for _, b := range treeEntries(t, a.api.API, w.alice.ID) {
			if b.Op == opKeep && slices.Contains(b.Keep, h) {
				return true
			}
		}
		return false
	})
	waitFor(t, "a names it again", func() bool { return a.app.blobs.Referenced(tree, h) })
}

// TestPurgeReleasesFromShare: a content that left a shared folder and was deleted forever can no
// longer be fetched by the person the folder is shared with.
func TestPurgeReleasesFromShare(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	bob := w.b1.Client(appID) // Bob's API only: his Files app would cache the bytes
	a.must(t, "files.mkdir", map[string]any{"path": "Photos"})
	a.must(t, "files.write", map[string]any{"path": "Photos/cat.jpg", "text": "meow"})
	a.must(t, "files.write", map[string]any{"path": "Photos/dog.jpg", "text": "woof"})
	a.must(t, "files.share", map[string]any{"path": "Photos", "people": []string{w.bob.ID}, "mode": "read"})
	ctx := context.Background()
	for _, s := range []string{"meow", "woof"} {
		if _, err := bob.StatBlob(ctx, blobOf(s), sol.BlobOptions{}); err != nil {
			t.Fatalf("bob before: %v", err)
		}
	}
	out := a.must(t, "files.delete", map[string]any{"paths": []string{"Photos/cat.jpg"}})
	a.must(t, "files.trash_purge", map[string]any{"ids": out["trash"]})
	if _, err := bob.StatBlob(ctx, blobOf("meow"), sol.BlobOptions{}); !errors.Is(err, sol.ErrNotFound) {
		t.Fatalf("bob after the purge: %v", err)
	}
	if _, err := bob.StatBlob(ctx, blobOf("woof"), sol.BlobOptions{}); err != nil {
		t.Fatalf("a file still in the folder: %v", err)
	}
}
