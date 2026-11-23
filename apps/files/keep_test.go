package files

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOnlineOnlyStreamsByHashAndKeepToggles(t *testing.T) {
	w := newWorld()
	desk := startNode(t, w.a1, nil)
	phone := startNode(t, w.a2, func(c *Config) { c.Role = RoleAndroid })
	desk.write(t, "Movies/film.mp4", "0123456789")
	desk.write(t, "Music/song.mp3", "la la la")
	desk.write(t, "readme.txt", "hello")
	waitFor(t, "phone knows the tree", func() bool { return len(phone.list(t, "")) == 3 })

	// Android default: online only. The tree is known, nothing is on disk.
	if got := strings.Join(phone.list(t, "Movies"), ","); got != "film.mp4" {
		t.Fatalf("list: %s", got)
	}
	if len(phone.files()) != 0 {
		t.Fatalf("online-only device wrote files: %v", phone.files())
	}
	rd := phone.must(t, "files.read", map[string]any{"path": "Movies/film.mp4"})
	if rd["blob"] == "" || rd["local_path"] != "" || rd["size"].(float64) != 10 {
		t.Fatalf("read: %v", rd)
	}
	rec := rawGet(t, phone, "Movies/film.mp4", "bytes=3-", true)
	if b, _ := io.ReadAll(rec.Body); rec.Code != http.StatusPartialContent || string(b) != "3456789" {
		t.Fatalf("range to end: %d %q", rec.Code, b)
	}
	rec = rawGet(t, phone, "Movies/film.mp4", "bytes=-4", true)
	if b, _ := io.ReadAll(rec.Body); string(b) != "6789" {
		t.Fatalf("suffix range: %q", b)
	}
	if rec = rawGet(t, phone, "Movies/film.mp4", "bytes=99-", true); rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("bad range: %d", rec.Code)
	}
	if rec = rawGet(t, phone, "nope.txt", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rec.Code)
	}
	if len(phone.files()) != 0 {
		t.Fatal("streaming must not write to disk")
	}
	if phone.app.Stats().BlobFetches != 0 {
		t.Fatal("streaming goes through GetBlob ranges, not a materialised copy")
	}

	// Writing from an online-only device still reaches the others, without a local copy.
	phone.must(t, "files.write", map[string]any{"path": "from-phone.txt", "text": "pp"})
	waitFor(t, "desktop gets it", func() bool { return desk.has("from-phone.txt", "pp") })
	if len(phone.files()) != 0 {
		t.Fatal("online-only device kept a copy")
	}

	// Pin one folder: its files appear on disk, others do not.
	phone.must(t, "files.keep", map[string]any{"path": "Music", "keep": true})
	if !phone.has("Music/song.mp3", "la la la") || !phone.gone("Movies") {
		t.Fatalf("keep Music: %v", phone.files())
	}
	rd = phone.must(t, "files.read", map[string]any{"path": "Music/song.mp3"})
	if rd["local_path"] != phone.path("Music/song.mp3") {
		t.Fatalf("local_path: %v", rd["local_path"])
	}
	if !w.alice.Pinned(rd["blob"].(string)) {
		t.Fatal("kept files should be pinned in the daemon")
	}

	// Back to online only: the local copy goes, the other device keeps its file, and nothing is
	// published as a delete.
	before := len(treeEntries(t, phone.api.API, w.alice.ID))
	phone.must(t, "files.keep", map[string]any{"path": "Music", "keep": false})
	if !phone.gone("Music/song.mp3") {
		t.Fatal("evict left the file")
	}
	phone.settle(t)
	if !desk.has("Music/song.mp3", "la la la") || len(treeEntries(t, phone.api.API, w.alice.ID)) != before {
		t.Fatal("making a folder online only must not delete it elsewhere")
	}
	if got := strings.Join(phone.list(t, "Music"), ","); got != "song.mp3" {
		t.Fatalf("still listed: %s", got)
	}

	// An edit that was never published survives eviction (it is published, not thrown away).
	phone.must(t, "files.keep", map[string]any{"path": "Music", "keep": true})
	phone.write(t, "Music/song.mp3", "edited on the phone")
	phone.must(t, "files.keep", map[string]any{"path": "Music", "keep": false})
	waitFor(t, "edit reached desktop", func() bool { return desk.has("Music/song.mp3", "edited on the phone") })
}

func TestDesktopKeepsByDefaultAndCanGoOnlineOnly(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.write(t, "Big/f.bin", "payload")
	waitFor(t, "b has it", func() bool { return b.has("Big/f.bin", "payload") })
	b.must(t, "files.keep", map[string]any{"path": "Big", "keep": false})
	if !b.gone("Big/f.bin") {
		t.Fatal("not evicted")
	}
	a.write(t, "Big/g.bin", "more")
	a.settle(t)
	b.must(t, "files.status", map[string]any{})
	if !b.gone("Big/g.bin") {
		t.Fatal("online-only folder received a file")
	}
	if got := strings.Join(b.list(t, "Big"), ","); got != "f.bin,g.bin" {
		t.Fatalf("list: %s", got)
	}
}

// TestAndroidDefaults: on a phone every folder of mine is online only by default, except the
// folders another app reads from disk (Notes); shared folders are online only everywhere.
func TestAndroidDefaults(t *testing.T) {
	phone := &App{cfg: Config{Role: RoleAndroid}, keep: map[string]bool{}}
	desk := &App{cfg: Config{Role: RoleDesktop}, keep: map[string]bool{}}
	for _, c := range []struct {
		p           string
		phone, desk bool
	}{
		{"Photos/beach.jpg", false, true},
		{"Nextcloud/big.iso", false, true},
		{"Notes/todo.md", true, true},
		{"Notes", true, true},
		{"Notesy/x", false, true}, // a prefix of the name is not the folder
		{sharedDir + "/ben/Trip/a.jpg", false, false},
	} {
		if got := phone.isKept(c.p); got != c.phone {
			t.Errorf("phone %s: kept %v, want %v", c.p, got, c.phone)
		}
		if got := desk.isKept(c.p); got != c.desk {
			t.Errorf("desktop %s: kept %v, want %v", c.p, got, c.desk)
		}
	}
	// A choice the person made wins over the default.
	phone.keep["Photos"] = true
	if !phone.isKept("Photos/beach.jpg") {
		t.Error("a pinned folder is not kept on the phone")
	}
}
