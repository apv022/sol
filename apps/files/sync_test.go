package files

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTwoDevicesConverge(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)

	a.write(t, "Notes/ideas.md", "first")
	a.write(t, "top.txt", "top")
	waitFor(t, "b gets files", func() bool { return b.has("Notes/ideas.md", "first") && b.has("top.txt", "top") })

	// Edit on b travels to a, with the mtime preserved.
	mt := time.Now().Add(-time.Hour).Truncate(time.Second)
	b.writeAt(t, "Notes/ideas.md", "second", mt)
	waitFor(t, "a gets edit", func() bool { return a.has("Notes/ideas.md", "second") })
	if st, _ := osStat(a.path("Notes/ideas.md")); !st.ModTime().Equal(mt) {
		t.Fatalf("mtime not carried: %v want %v", st.ModTime(), mt)
	}

	// A folder move on a (as a program would do it) arrives on b as a move. Settle first: this
	// checks a move made while nothing is in flight (a rename racing an incoming write is its own
	// case).
	a.settle(t)
	b.settle(t)
	if err := osRename(a.path("Notes"), a.path("Archive")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b sees the move", func() bool { return b.has("Archive/ideas.md", "second") && b.gone("Notes") })

	// Delete.
	if err := osRemove(b.path("top.txt")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a sees the delete", func() bool { return a.gone("top.txt") })

	a.settle(t)
	b.settle(t)
	if got, want := strings.Join(a.files(), ","), strings.Join(b.files(), ","); got != want || got != "Archive/ideas.md" {
		t.Fatalf("a=%s b=%s", got, want)
	}
}

func TestConcurrentEditsKeepBothAsConflictCopy(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.write(t, "ideas.md", "base")
	waitFor(t, "b has base", func() bool { return b.has("ideas.md", "base") })
	a.settle(t)
	b.settle(t)

	// b loses its connection, then both edit the same version.
	b.api.down.Store(true)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	a.writeAt(t, "ideas.md", "from main", base.Add(2*time.Minute)) // later: keeps the name
	b.writeAt(t, "ideas.md", "from laptop2", base.Add(time.Minute))
	waitFor(t, "a published", func() bool { return len(treeEntries(t, a.api.API, w.alice.ID)) >= 2 })
	b.api.down.Store(false)

	copyName := ""
	waitFor(t, "both versions on both devices", func() bool {
		for _, f := range a.files() {
			if strings.HasPrefix(f, "ideas (conflict from laptop2 ") {
				copyName = f
			}
		}
		return copyName != "" && a.has("ideas.md", "from main") && a.has(copyName, "from laptop2") &&
			b.has("ideas.md", "from main") && b.has(copyName, "from laptop2")
	})
	if !strings.HasSuffix(copyName, ").md") {
		t.Fatalf("copy keeps the extension: %q", copyName)
	}
	// One notice for the pair of devices, not one each.
	a.settle(t)
	b.settle(t)
	if n := len(notices(w.alice)); n != 1 {
		t.Fatalf("want one inbox notice, got %d: %+v", n, w.alice.Inbox())
	}

	// conflicts lists it; "keep both" turns the copy into a normal file and clears the conflict.
	c := b.must(t, "files.conflicts", map[string]any{})["conflicts"].([]any)
	if len(c) != 1 || c[0].(map[string]any)["path"] != "ideas.md" {
		t.Fatalf("conflicts: %v", c)
	}
	b.must(t, "files.resolve", map[string]any{"path": "ideas.md", "keep": "both"})
	waitFor(t, "resolved everywhere", func() bool {
		return len(a.app.getView().conflicts) == 0 && len(b.app.getView().conflicts) == 0 &&
			a.has(copyName, "from laptop2") && a.has("ideas.md", "from main")
	})
}

func TestResolveThisAndThat(t *testing.T) {
	for _, keep := range []string{"this", "that"} {
		t.Run(keep, func(t *testing.T) {
			w := newWorld()
			a := startNode(t, w.a1, nil)
			b := startNode(t, w.a2, nil)
			a.write(t, "n.txt", "base")
			waitFor(t, "synced", func() bool { return b.has("n.txt", "base") })
			a.settle(t)
			b.settle(t)
			b.api.down.Store(true)
			base := time.Now().Add(-time.Hour).Truncate(time.Second)
			a.writeAt(t, "n.txt", "A", base.Add(2*time.Minute))
			b.writeAt(t, "n.txt", "B", base.Add(time.Minute))
			waitFor(t, "a published", func() bool { return len(treeEntries(t, a.api.API, w.alice.ID)) >= 2 })
			b.api.down.Store(false)
			waitFor(t, "conflict on a", func() bool { return len(a.app.getView().conflicts) == 1 && len(a.files()) == 2 })
			a.must(t, "files.resolve", map[string]any{"path": "n.txt", "keep": keep})
			want := map[string]string{"this": "A", "that": "B"}[keep]
			waitFor(t, "single file, right content, both devices", func() bool {
				return len(a.files()) == 1 && len(b.files()) == 1 && a.has("n.txt", want) && b.has("n.txt", want)
			})
		})
	}
}

func TestOfflineEditsMergeAfterReconnect(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.write(t, "keep.txt", "k")
	a.write(t, "gone.txt", "g")
	a.write(t, "edit.txt", "e0")
	waitFor(t, "synced", func() bool { return b.has("gone.txt", "g") && b.has("edit.txt", "e0") && b.has("keep.txt", "k") })
	a.settle(t)
	b.settle(t)

	b.api.down.Store(true)
	b.write(t, "new-on-b.txt", "nb")
	b.write(t, "edit.txt", "e1 from b")
	if err := osRemove(b.path("gone.txt")); err != nil {
		t.Fatal(err)
	}
	a.write(t, "new-on-a.txt", "na") // meanwhile, a keeps going
	waitFor(t, "b has pending work", func() bool {
		b.app.mu.RLock()
		defer b.app.mu.RUnlock()
		return len(b.app.pending) >= 2
	})
	if b.has("new-on-a.txt", "na") {
		t.Fatal("b cannot have received while offline")
	}
	b.api.down.Store(false)

	waitFor(t, "merged", func() bool {
		return a.has("new-on-b.txt", "nb") && a.has("edit.txt", "e1 from b") && a.gone("gone.txt") && b.has("new-on-a.txt", "na")
	})
	a.settle(t)
	b.settle(t)
	if got, want := strings.Join(a.files(), ","), strings.Join(b.files(), ","); got != want {
		t.Fatalf("a=%s b=%s", got, want)
	}
	if len(a.app.getView().conflicts) != 0 || len(b.app.getView().conflicts) != 0 {
		t.Fatal("independent offline edits are not conflicts")
	}
}

func TestNoWatcherEchoLoop(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.write(t, "d/one.txt", "1")
	a.write(t, "d/two.txt", "2")
	waitFor(t, "b has files", func() bool { return b.has("d/one.txt", "1") && b.has("d/two.txt", "2") })
	a.settle(t)
	b.settle(t)
	// b only applied remote changes: it must not have published anything, and nothing may keep
	// growing afterwards on either side.
	if n := b.app.Stats().TreeAppends; n != 0 {
		t.Fatalf("b echoed %d entries back", n)
	}
	before := len(treeEntries(t, a.api.API, w.alice.ID))
	time.Sleep(20 * a.app.cfg.Debounce)
	if after := len(treeEntries(t, a.api.API, w.alice.ID)); after != before {
		t.Fatalf("tree feed kept growing: %d -> %d", before, after)
	}
	// A rescan and a restart are no-ops as well.
	if err := b.app.Rescan(t.Context()); err != nil {
		t.Fatal(err)
	}
	b.restart(t, w.a2)
	b.settle(t)
	if after := len(treeEntries(t, a.api.API, w.alice.ID)); after != before {
		t.Fatalf("rescan or restart published: %d -> %d", before, after)
	}
}

func TestChangesWhileStoppedArePublishedOnStart(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.write(t, "x.txt", "x0")
	waitFor(t, "b has x", func() bool { return b.has("x.txt", "x0") })
	b.settle(t)
	b.app.Stop()
	b.write(t, "x.txt", "x1 edited while the app was off")
	b.write(t, "y.txt", "new")
	b.restart(t, w.a2)
	waitFor(t, "a gets them", func() bool { return a.has("x.txt", "x1 edited while the app was off") && a.has("y.txt", "new") })
	if len(a.app.getView().conflicts) != 0 {
		t.Fatal("edit made while stopped started from the synced version: no conflict")
	}
}

func TestScanFallbackWithoutWatcher(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, func(c *Config) { c.NoWatch = true })
	b := startNode(t, w.a2, func(c *Config) { c.NoWatch = true })
	if a.app.watching {
		t.Fatal("watcher should be off")
	}
	a.write(t, "s/scan.txt", "found by scan")
	waitFor(t, "scan finds it", func() bool { return b.has("s/scan.txt", "found by scan") })
}

// Themes follow the person between devices: .sol/theme and .sol/themes/*.css sync, the rest of
// .sol (settings, scratch) does not, and a change on one device reaches the other's disk.
func TestThemeFilesSync(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.write(t, ".sol/themes/paper.css", "/* @theme Paper */")
	a.write(t, ".sol/theme", "paper\n")
	a.write(t, ".sol/theme-settings.json", "{}")
	a.write(t, ".sol/themes/notes.txt", "x")
	a.write(t, ".sol/themes/Bad Name.css", "x")
	waitFor(t, "b has the theme", func() bool {
		return b.has(".sol/themes/paper.css", "/* @theme Paper */") && b.has(".sol/theme", "paper\n")
	})
	a.write(t, ".sol/theme", "sol\n")
	waitFor(t, "b sees the change", func() bool { return b.has(".sol/theme", "sol\n") })
	a.settle(t)
	b.settle(t)
	got := strings.Join(b.files(), ",")
	if got != ".sol/theme,.sol/themes/paper.css" {
		t.Fatalf("synced files: %s", got)
	}
}

func TestIgnoredFilesAreNotSynced(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.write(t, ".solignore", "# comment\n*.log\nbuild/\nsecret/*.key\n")
	a.write(t, "real.txt", "r")
	a.write(t, "debug.log", "x")
	a.write(t, "build/out.bin", "x")
	a.write(t, "secret/a.key", "x")
	a.write(t, "secret/a.txt", "kept")
	a.write(t, "doc.txt.swp", "x")
	a.write(t, "~$doc.docx", "x")
	a.write(t, ".#lock", "x")
	a.write(t, ".sol/internal", "x")
	waitFor(t, "b has real files", func() bool {
		return b.has("real.txt", "r") && b.has("secret/a.txt", "kept") && b.has(".solignore", "# comment\n*.log\nbuild/\nsecret/*.key\n")
	})
	a.settle(t)
	b.settle(t)
	got := strings.Join(b.files(), ",")
	if got != ".solignore,real.txt,secret/a.txt" {
		t.Fatalf("synced files: %s", got)
	}
}

// A folder renamed in the instant a synced file lands in it stays renamed: the file is not
// written again at the old path, and the move reaches the other device.
func TestRenameRacingAnIncomingWrite(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.write(t, "Notes/keep.md", "k")
	waitFor(t, "b gets the folder", func() bool { return b.has("Notes/keep.md", "k") })
	b.settle(t)

	var once sync.Once
	afterMaterialise = func(full string) {
		if filepath.Base(full) == "ideas.md" && strings.HasPrefix(full, b.path("Notes")) {
			once.Do(func() { _ = osRename(b.path("Notes"), b.path("Archive")) })
		}
	}
	t.Cleanup(func() { afterMaterialise = nil })
	a.write(t, "Notes/ideas.md", "new")
	waitFor(t, "the move reaches a", func() bool { return a.has("Archive/ideas.md", "new") && a.gone("Notes") })
	a.settle(t)
	b.settle(t)
	stays(t, "Notes stays gone", func() bool { return a.gone("Notes") && b.gone("Notes") })
}
