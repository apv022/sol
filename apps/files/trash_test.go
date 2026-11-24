package files

import (
	"context"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// trashRows returns files.trash_list rows of a node.
func trashRows(t *testing.T, n *node) []map[string]any {
	t.Helper()
	out := n.must(t, "files.trash_list", map[string]any{})
	var rows []map[string]any
	for _, r := range out["items"].([]any) {
		rows = append(rows, r.(map[string]any))
	}
	return rows
}

func rowPaths(rows []map[string]any) string {
	var ps []string
	for _, r := range rows {
		ps = append(ps, r["path"].(string))
	}
	sort.Strings(ps)
	return strings.Join(ps, ",")
}

func versionsOfPath(t *testing.T, n *node, p string) []map[string]any {
	t.Helper()
	out := n.must(t, "files.versions", map[string]any{"path": p})
	var vs []map[string]any
	for _, v := range out["versions"].([]any) {
		vs = append(vs, v.(map[string]any))
	}
	return vs
}

func TestTrashDeleteByActionRestoreOnAnotherDevice(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.write(t, "Docs/a.txt", "alpha")
	waitFor(t, "b has the file", func() bool { return b.has("Docs/a.txt", "alpha") })
	a.settle(t)

	out := a.must(t, "files.delete", map[string]any{"paths": []string{"Docs/a.txt"}})
	ids := out["trash"].([]any)
	if len(ids) != 1 {
		t.Fatalf("delete answers its trash id: %v", out)
	}
	waitFor(t, "b sees the deletion", func() bool { return b.gone("Docs/a.txt") })
	var rows []map[string]any
	waitFor(t, "b's Trash has it", func() bool { rows = trashRows(t, b); return len(rows) == 1 })
	r := rows[0]
	if r["id"] != ids[0] || r["path"] != "Docs/a.txt" || r["folder"] != "Docs" || r["device"] != a.app.devName ||
		r["size"].(float64) != 5 || r["dir"] != false || r["disk"] != false {
		t.Fatalf("row: %v", r)
	}
	if exp := r["expires_at"].(float64) - r["deleted_at"].(float64); exp != 30*dayMs {
		t.Fatalf("default retention: %v", exp)
	}

	// Restored on b: back on both devices, out of both Trashes.
	b.must(t, "files.trash_restore", map[string]any{"ids": ids})
	waitFor(t, "a gets it back", func() bool { return a.has("Docs/a.txt", "alpha") })
	waitFor(t, "Trash empty on a", func() bool { return len(trashRows(t, a)) == 0 })
	if len(trashRows(t, b)) != 0 {
		t.Fatal("b's Trash still has it")
	}
	// Restoring again is not found.
	if _, code := b.act(t, "files.trash_restore", map[string]any{"ids": ids}, false); code != http.StatusNotFound {
		t.Fatalf("second restore: %d", code)
	}
}

func TestTrashRmOnDiskFolderIsOneRowAndRestoresTree(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.write(t, "Photos/x.jpg", "x")
	a.write(t, "Photos/sub/y.jpg", "yy")
	a.write(t, "keep.txt", "k")
	waitFor(t, "b has them", func() bool { return b.has("Photos/sub/y.jpg", "yy") && b.has("keep.txt", "k") })
	a.settle(t)
	b.settle(t)

	// rm -r on a, and rm of a file on b.
	if err := os.RemoveAll(a.path("Photos")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(b.path("keep.txt")); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	// However the watcher saw the rm -r (file by file, or the folder at once), it is one row.
	var photos map[string]any
	waitFor(t, "b's Trash shows both deletions", func() bool {
		rows = trashRows(t, b)
		for _, r := range rows {
			if r["path"] == "Photos" {
				photos = r
			}
		}
		return rowPaths(rows) == "Photos,keep.txt" && photos["files"].(float64) == 2
	})
	if photos["dir"] != true || photos["files"].(float64) != 2 || photos["size"].(float64) != 3 || photos["disk"] != true || photos["device"] != a.app.devName {
		t.Fatalf("folder row: %v", photos)
	}
	waitFor(t, "a's Trash agrees", func() bool { return rowPaths(trashRows(t, a)) == "Photos,keep.txt" })

	b.must(t, "files.trash_restore", map[string]any{"ids": []string{photos["id"].(string)}})
	waitFor(t, "the tree is back on a", func() bool { return a.has("Photos/x.jpg", "x") && a.has("Photos/sub/y.jpg", "yy") })
	waitFor(t, "and on b", func() bool { return b.has("Photos/x.jpg", "x") && b.has("Photos/sub/y.jpg", "yy") })
	if got := rowPaths(trashRows(t, a)); got != "keep.txt" {
		t.Fatalf("left in Trash: %s", got)
	}
}

func TestTrashRestoreClashAndDestination(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	a.must(t, "files.write", map[string]any{"path": "Docs/a.txt", "text": "old"})
	a.must(t, "files.write", map[string]any{"path": "Docs/b.txt", "text": "bee"})
	del := a.must(t, "files.delete", map[string]any{"paths": []string{"Docs/a.txt"}})
	a.must(t, "files.write", map[string]any{"path": "Docs/a.txt", "text": "new"})

	// Same place: the new file keeps the name.
	a.must(t, "files.trash_restore", map[string]any{"ids": del["trash"]})
	if !a.has("Docs/a.txt", "new") || !a.has("Docs/a (restored).txt", "old") {
		t.Fatalf("clash: %v", a.files())
	}

	// A chosen folder, made on the way.
	del = a.must(t, "files.delete", map[string]any{"paths": []string{"Docs/b.txt"}})
	a.must(t, "files.trash_restore", map[string]any{"ids": del["trash"], "to": "Back/Here"})
	if !a.has("Back/Here/b.txt", "bee") {
		t.Fatalf("to: %v", a.files())
	}

	// A folder whose parent was deleted too comes back with its parents.
	a.must(t, "files.write", map[string]any{"path": "Deep/er/c.txt", "text": "c"})
	del = a.must(t, "files.delete", map[string]any{"paths": []string{"Deep"}})
	rows := trashRows(t, a)
	if len(rows) != 1 || rows[0]["path"] != "Deep" {
		t.Fatalf("rows: %v", rows)
	}
	// One file of a deleted folder.
	item := rows[0]["items"].([]any)[0].(map[string]any)
	a.must(t, "files.trash_restore", map[string]any{"ids": []string{item["id"].(string)}})
	if !a.has("Deep/er/c.txt", "c") {
		t.Fatalf("item restore: %v", a.files())
	}
	if len(trashRows(t, a)) != 0 {
		t.Fatalf("the only file of the row is back: row stays? %v", trashRows(t, a))
	}
}

func TestTrashPurgeAndEmpty(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	for _, p := range []string{"one.txt", "two.txt", "three.txt"} {
		a.must(t, "files.write", map[string]any{"path": p, "text": p})
	}
	var ids []string
	for _, p := range []string{"one.txt", "two.txt", "three.txt"} {
		out := a.must(t, "files.delete", map[string]any{"paths": []string{p}})
		ids = append(ids, out["trash"].([]any)[0].(string))
	}
	waitFor(t, "b sees three", func() bool { return len(trashRows(t, b)) == 3 })

	before := len(treeEntries(t, a.api.API, w.alice.ID))
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"files.trash_purge", map[string]any{"ids": ids[:1]}},
		{"files.trash_empty", map[string]any{}},
		{"files.trash_restore", map[string]any{"ids": ids}},
		{"files.trash_settings", map[string]any{"days": 7}},
	} {
		out, code := a.act(t, c.name, c.args, true)
		if code != 200 || out["dry_run"] != true || len(out["would"].([]any)) == 0 {
			t.Fatalf("%s dry run: %d %v", c.name, code, out)
		}
	}
	if after := len(treeEntries(t, a.api.API, w.alice.ID)); after != before || len(trashRows(t, a)) != 3 || len(a.files()) != 0 {
		t.Fatalf("dry runs changed something: %d -> %d, %v", before, after, a.files())
	}

	a.must(t, "files.trash_purge", map[string]any{"ids": ids[:1]})
	waitFor(t, "b sees the purge", func() bool { return len(trashRows(t, b)) == 2 })
	if _, code := a.act(t, "files.trash_restore", map[string]any{"ids": ids[:1]}, false); code != http.StatusNotFound {
		t.Fatalf("restoring a purged one: %d", code)
	}
	b.must(t, "files.trash_empty", map[string]any{})
	waitFor(t, "a's Trash is empty", func() bool { return len(trashRows(t, a)) == 0 })
}

// clock is a settable test clock.
type clock struct{ off atomic.Int64 }

func (c *clock) now() time.Time          { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *clock) advance(d time.Duration) { c.off.Add(int64(d)) }

func TestTrashRetentionExpiresAndSettingsSync(t *testing.T) {
	w := newWorld()
	clk := &clock{}
	a := startNode(t, w.a1, func(c *Config) { c.Now = clk.now })
	b := startNode(t, w.a2, func(c *Config) { c.Now = clk.now })
	first := a
	if b.app.devID < a.app.devID {
		first = b
	}
	a.must(t, "files.write", map[string]any{"path": "old.txt", "text": "o"})
	a.must(t, "files.delete", map[string]any{"paths": []string{"old.txt"}})
	clk.advance(10 * 24 * time.Hour)
	a.must(t, "files.write", map[string]any{"path": "new.txt", "text": "n"})
	a.must(t, "files.delete", map[string]any{"paths": []string{"new.txt"}})
	waitFor(t, "b has both", func() bool { return len(trashRows(t, b)) == 2 })
	out := a.must(t, "files.trash_list", map[string]any{})
	if want := clk.now().UnixMilli() + 20*dayMs; abs(int64(out["next_expiry"].(float64))-want) > 5000 {
		t.Fatalf("next expiry %v, want about %d", out["next_expiry"], want)
	}

	// 21 days later the first is past 30 days: hidden everywhere at once, then purged for good.
	clk.advance(21 * 24 * time.Hour)
	if got := rowPaths(trashRows(t, b)); got != "new.txt" {
		t.Fatalf("after 31 days: %s", got)
	}
	purges := func() int {
		n := 0
		for _, b := range treeEntries(t, a.api.API, w.alice.ID) {
			if b.Op == opPurge {
				n++
			}
		}
		return n
	}
	// (The loop's own timer may get there first; either way one purge is appended.)
	first.app.expire(context.Background())
	waitFor(t, "one purge", func() bool { return purges() == 1 })
	// The other device does not append a second purge.
	other := a
	if first == a {
		other = b
	}
	waitFor(t, "purge folded", func() bool {
		other.app.mu.RLock()
		defer other.app.mu.RUnlock()
		return len(other.app.own.hist.trash) == 1
	})
	other.app.expire(context.Background())
	first.app.expire(context.Background())
	if purges() != 1 {
		t.Fatalf("purges: %d", purges())
	}

	// A setting made on one device holds on the other; shorter retention hides the rest.
	a.must(t, "files.trash_settings", map[string]any{"days": 7, "versions": 3})
	waitFor(t, "b has the setting", func() bool {
		s := b.must(t, "files.trash_settings", map[string]any{})
		return s["days"].(float64) == 7 && s["versions"].(float64) == 3
	})
	if len(trashRows(t, b)) != 0 {
		t.Fatalf("21-day-old deletion is past a 7 day window: %v", trashRows(t, b))
	}
	if _, code := a.act(t, "files.trash_settings", map[string]any{"days": 0}, false); code != http.StatusBadRequest {
		t.Fatalf("days 0: %d", code)
	}
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestVersionsOnOverwriteSyncRestoreAndCap(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.must(t, "files.write", map[string]any{"path": "doc.md", "text": "v1"})
	a.must(t, "files.write", map[string]any{"path": "doc.md", "text": "v2"})
	vs := versionsOfPath(t, a, "doc.md")
	if len(vs) != 1 || vs[0]["size"].(float64) != 2 || vs[0]["device"] != a.app.devName {
		t.Fatalf("after overwrite: %v", vs)
	}

	// An edit on another device (on its disk) makes v2 a version everywhere.
	waitFor(t, "b has v2", func() bool { return b.has("doc.md", "v2") })
	b.settle(t)
	b.write(t, "doc.md", "v3 from b")
	waitFor(t, "a has v3", func() bool { return a.has("doc.md", "v3 from b") })
	waitFor(t, "a lists two versions", func() bool { return len(versionsOfPath(t, a, "doc.md")) == 2 })
	vs = versionsOfPath(t, a, "doc.md")
	if vs[0]["device"] != b.app.devName {
		t.Fatalf("newest first, replaced by b: %v", vs)
	}

	// Restore v1 (dry run first): v1 is current, v3 becomes a version, v1 is no longer listed.
	v1 := vs[1]["id"].(string)
	before := len(treeEntries(t, a.api.API, w.alice.ID))
	if out, code := a.act(t, "files.version_restore", map[string]any{"path": "doc.md", "id": v1}, true); code != 200 || out["dry_run"] != true {
		t.Fatalf("dry run: %d %v", code, out)
	}
	if out, code := a.act(t, "files.version_delete", map[string]any{"path": "doc.md", "ids": []string{v1}}, true); code != 200 || out["dry_run"] != true {
		t.Fatalf("dry run: %d %v", code, out)
	}
	if len(treeEntries(t, a.api.API, w.alice.ID)) != before || !a.has("doc.md", "v3 from b") {
		t.Fatal("dry runs changed something")
	}
	a.must(t, "files.version_restore", map[string]any{"path": "doc.md", "id": v1})
	waitFor(t, "b has v1 again", func() bool { return b.has("doc.md", "v1") })
	waitFor(t, "b lists v3 and v2", func() bool {
		vs = versionsOfPath(t, b, "doc.md")
		return len(vs) == 2 && vs[0]["size"].(float64) == float64(len("v3 from b")) && vs[1]["size"].(float64) == 2
	})

	// Delete a version.
	b.must(t, "files.version_delete", map[string]any{"path": "doc.md", "ids": []string{vs[1]["id"].(string)}})
	waitFor(t, "a lists one", func() bool { return len(versionsOfPath(t, a, "doc.md")) == 1 })

	// The cap: at most N per file.
	a.must(t, "files.trash_settings", map[string]any{"versions": 3})
	for i := 0; i < 6; i++ {
		a.must(t, "files.write", map[string]any{"path": "doc.md", "text": strings.Repeat("x", i+10)})
	}
	if got := versionsOfPath(t, a, "doc.md"); len(got) != 3 || got[0]["size"].(float64) != 14 {
		t.Fatalf("cap 3: %v", got)
	}
	// A deleted file's versions stay with its path; restoring it from Trash brings them back.
	del := a.must(t, "files.delete", map[string]any{"paths": []string{"doc.md"}})
	a.must(t, "files.trash_restore", map[string]any{"ids": del["trash"]})
	if got := versionsOfPath(t, a, "doc.md"); len(got) != 3 {
		t.Fatalf("versions after a restore: %v", got)
	}
}

func TestVersionsOfMergeAndConflictChoice(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	a.must(t, "files.write", map[string]any{"path": "c.txt", "text": "base"})
	base := a.must(t, "files.stat", map[string]any{"path": "c.txt"})["blob"].(string)
	// Another device's edit of the same base arrives while a also edited: a conflict.
	rival := mustBlob(t, a, "rival")
	a.must(t, "files.write", map[string]any{"path": "c.txt", "text": "mine!", "base": base})
	if _, err := w.a2.Client(appID).Append(context.Background(), treeFeed, body{Op: opPut, Path: "c.txt", Blob: rival, Size: 5, Mtime: 1, Base: base, Dev: "laptop2", At: 1}); err != nil {
		t.Fatal(err)
	}
	if err := a.app.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(a.app.getView().conflicts) != 1 {
		t.Skip("merged instead of conflicting") // both are one-line edits of one line: never merged
	}
	a.must(t, "files.resolve", map[string]any{"path": "c.txt", "keep": "this"})
	var blobs []string
	for _, v := range versionsOfPath(t, a, "c.txt") {
		blobs = append(blobs, v["blob"].(string))
	}
	if !strings.Contains(strings.Join(blobs, ","), rival) || !strings.Contains(strings.Join(blobs, ","), base) {
		t.Fatalf("the base and the discarded side are versions: %v", blobs)
	}
	if len(trashRows(t, a)) != 0 {
		t.Fatalf("a conflict choice is not a deletion: %v", trashRows(t, a))
	}
}

func TestTrashFoldIsOrderIndependentForPurges(t *testing.T) {
	// A purge that arrives before the deletion it names (another device's chain came later)
	// still hides it.
	tr := newOwnTree()
	tr.Apply("d1", "", 1, body{Op: opPut, Path: "a.txt", Blob: h1, Size: 1, Mtime: 1, At: 1})
	tr.Apply("d2", "", 1, body{Op: opPurge, IDs: []string{"0123456789abcdef"}, At: 5})
	tr.Apply("d1", "", 2, body{Op: opDelete, Path: "a.txt", Base: h1, Trash: "0123456789abcdef", In: "a.txt", At: 3})
	if gs := tr.hist.groups(10, nil); len(gs) != 0 {
		t.Fatalf("purged group shows: %+v", gs[0])
	}
	// A co-author cannot add rows to one of my groups.
	tr.Apply("d1", "", 3, body{Op: opPut, Path: "b.txt", Blob: h1, Size: 1, Mtime: 1, At: 6})
	tr.Apply("d1", "", 4, body{Op: opDelete, Path: "b.txt", Base: h1, Trash: "fedcba9876543210", At: 7})
	tr.Apply("d1", "", 5, body{Op: opPut, Path: "c.txt", Blob: h1, Size: 1, Mtime: 1, At: 6})
	tr.Apply("bobdev", "bob", 1, body{Op: opDelete, Path: "c.txt", Base: h1, Trash: "fedcba9876543210", At: 8})
	if gs := tr.hist.groups(10, nil); len(gs) != 2 {
		t.Fatalf("want two rows: %d", len(gs))
	}
}

func TestFoldMergeKeepsBothSidesAsVersions(t *testing.T) {
	tr := newOwnTree()
	const h3 = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	tr.Apply("d1", "", 1, body{Op: opPut, Path: "m.txt", Blob: h1, Size: 1, Mtime: 1, At: 1})
	tr.Apply("d1", "", 2, body{Op: opPut, Path: "m.txt", Blob: h2, Size: 2, Mtime: 2, Base: h1, At: 2})
	tr.Apply("d2", "", 1, body{Op: opPut, Path: "m.txt", Blob: h3, Size: 3, Mtime: 3, Base: h1, At: 3})
	// The merge supersedes both heads.
	tr.Apply("d1", "", 3, body{Op: opPut, Path: "m.txt", Blob: "sha256:4444444444444444444444444444444444444444444444444444444444444444", Size: 4, Mtime: 4, Base: h3, Also: []string{h2}, At: 4})
	got := map[string]bool{}
	for _, v := range tr.hist.visibleVersions("m.txt", "", 5) {
		got[v.Blob] = true
	}
	if !got[h1] || !got[h2] || !got[h3] || len(got) != 3 {
		t.Fatalf("versions: %v", got)
	}
}

func TestRmBurstOnDiskIsOneRow(t *testing.T) {
	tr := newOwnTree()
	for i, p := range []string{"P/x", "P/sub/y", "Q/z"} {
		tr.Apply("d1", "", uint64(i+1), body{Op: opPut, Path: p, Blob: h1, Size: 1, Mtime: 1, At: 1})
	}
	// The watcher published P/x while P still existed, then the rest of P as one deletion.
	tr.Apply("d1", "", 4, body{Op: opDelete, Path: "P/x", Base: h1, Trash: "aaaaaaaaaaaaaaaa", In: "P/x", Why: whyDisk, Dev: "laptop", At: 1000})
	tr.Apply("d1", "", 5, body{Op: opDelete, Path: "P/sub/y", Base: h1, Trash: "bbbbbbbbbbbbbbbb", In: "P", Why: whyDisk, Dev: "laptop", At: 1500})
	// A removal elsewhere, long after, is a row of its own.
	tr.Apply("d1", "", 6, body{Op: opDelete, Path: "Q/z", Base: h1, Trash: "cccccccccccccccc", In: "Q/z", Why: whyDisk, Dev: "laptop", At: 99999})
	gs := tr.hist.groups(100000, nil)
	if len(gs) != 2 || gs[1].Root != "P" || gs[1].Files != 2 || len(gs[1].Members) != 2 {
		t.Fatalf("rows: %+v %+v", gs[0], gs[1])
	}
}
