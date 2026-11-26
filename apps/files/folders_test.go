package files

import (
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func isDir(n *node, p string) bool {
	st, err := os.Lstat(n.path(p))
	return err == nil && st.IsDir()
}

// stays checks for a while that cond keeps holding (a folder put back by a later pass would
// show up after the first check).
func stays(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 30; i++ {
		if !cond() {
			t.Fatalf("did not stay: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testEmptyFolderRenameDelete(t *testing.T, mut func(*Config)) {
	w := newWorld()
	a := startNode(t, w.a1, mut)
	b := startNode(t, w.a2, mut)
	if err := os.Mkdir(a.path("Empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b gets the empty folder", func() bool { return isDir(b, "Empty") })
	a.settle(t)
	if err := os.Rename(a.path("Empty"), a.path("Renamed")); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			t.Logf("tree: %+v", treeEntries(t, a.api.API, w.alice.ID))
		}
	}()
	waitFor(t, "rename reaches b", func() bool { return isDir(b, "Renamed") && b.gone("Empty") })
	a.settle(t)
	b.settle(t)
	stays(t, "old name stays gone on both", func() bool { return a.gone("Empty") && b.gone("Empty") && isDir(a, "Renamed") })

	if err := os.Remove(b.path("Renamed")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "delete reaches a", func() bool { return a.gone("Renamed") })
	a.settle(t)
	b.settle(t)
	stays(t, "deleted folder stays gone on both", func() bool { return a.gone("Renamed") && b.gone("Renamed") })
	if v := a.app.getView(); v.dirs["Empty"] || v.dirs["Renamed"] {
		t.Fatalf("tree still lists the folder: %v", v.explicit)
	}
}

// An empty folder renamed or removed on disk must not be put back (found on Android).
func TestEmptyFolderRenameAndDelete(t *testing.T) { testEmptyFolderRenameDelete(t, nil) }
func TestEmptyFolderRenameAndDeleteScan(t *testing.T) {
	testEmptyFolderRenameDelete(t, func(c *Config) { c.NoWatch = true })
}

func TestFullFolderRenameAndDelete(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.write(t, "Box/one.txt", "1")
	a.write(t, "Box/in/two.txt", "2")
	waitFor(t, "b gets the folder", func() bool { return b.has("Box/in/two.txt", "2") })
	a.settle(t)
	if err := os.Rename(a.path("Box"), a.path("Crate")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "rename reaches b", func() bool { return b.has("Crate/in/two.txt", "2") && b.gone("Box") })
	a.settle(t)
	b.settle(t)
	stays(t, "Box stays gone", func() bool { return a.gone("Box") && b.gone("Box") })
	// On Windows a file the scanner has open for a moment can't be deleted ("used by another
	// process"), as with any program reading it: retry, as a person would.
	var rmErr error
	waitFor(t, "the folder deleted on b", func() bool { rmErr = os.RemoveAll(b.path("Crate")); return rmErr == nil })
	waitFor(t, "delete reaches a", func() bool { return a.gone("Crate") })
	a.settle(t)
	b.settle(t)
	stays(t, "Crate stays gone", func() bool { return a.gone("Crate") && b.gone("Crate") })
}

// files.list says how many items each folder holds and describes the folder itself, so the UI
// needs one call per folder; files.sync syncs now and answers with the status; /raw with v=blob
// is cacheable for good, without it the browser must revalidate.
func TestListCountsSyncAndRawCaching(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	a.write(t, "Box/one.txt", "1")
	a.write(t, "Box/two.txt", "2")
	a.must(t, "files.mkdir", map[string]any{"path": "Box/In"})
	// Both files come in through the watcher, in either order: wait for the folder to hold all three.
	var out map[string]any
	waitFor(t, "published", func() bool {
		out = a.must(t, "files.list", map[string]any{"path": ""})
		es, _ := out["entries"].([]any)
		return len(es) == 1 && es[0].(map[string]any)["items"] == float64(3)
	})
	box := out["entries"].([]any)[0].(map[string]any)
	if box["name"] != "Box" || box["items"] != float64(3) {
		t.Fatalf("folder entry: %v", box)
	}
	if here := out["here"].(map[string]any); here["type"] != "dir" || here["path"] != "" {
		t.Fatalf("here of the root: %v", here)
	}
	in := a.must(t, "files.list", map[string]any{"path": "Box"})
	if here := in["here"].(map[string]any); here["name"] != "Box" || here["kept"] != true {
		t.Fatalf("here: %v", here)
	}
	for _, e := range in["entries"].([]any) {
		m := e.(map[string]any)
		if m["type"] == "file" && m["items"] != nil {
			t.Fatalf("a file has no item count: %v", m)
		}
	}

	// A file written behind the watcher's back (NoWatch would be the same) shows up after sync.
	st := a.must(t, "files.sync", map[string]any{})
	if st["state"] == nil || st["devices"] == nil {
		t.Fatalf("sync answers with the status: %v", st)
	}

	f := a.must(t, "files.stat", map[string]any{"path": "Box/one.txt"})
	get := func(q string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/raw?path=Box/one.txt"+q, nil)
		r.Header.Set("Sol-Self", "1")
		rec := httptest.NewRecorder()
		a.app.Handler().ServeHTTP(rec, r)
		return rec
	}
	if rec := get("&v=" + url.QueryEscape(f["blob"].(string))); rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || rec.Body.String() != "1" {
		t.Fatalf("raw with v: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := get("&v=sha256:stale"); rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("raw with a stale v must not be cached: %q", rec.Header().Get("Cache-Control"))
	}
	if rec := get("&download=1"); !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %q", rec.Header().Get("Content-Disposition"))
	}
}
