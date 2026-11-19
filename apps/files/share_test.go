package files

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apv022/sol/sdk/go/sol"
)

func rawGet(t *testing.T, n *node, path, rng string, self bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "/raw?path="+strings.ReplaceAll(path, " ", "%20"), nil)
	if self {
		r.Header.Set("Sol-Self", "1")
	}
	if rng != "" {
		r.Header.Set("Range", rng)
	}
	w := httptest.NewRecorder()
	n.app.Handler().ServeHTTP(w, r)
	return w
}

func TestShareReadOnlyToBob(t *testing.T) {
	w := newWorld()
	alice := startNode(t, w.a1, nil)
	bob := startNode(t, w.b1, nil)
	alice.write(t, "Photos/a.jpg", "0123456789abcdef")
	alice.write(t, "Photos/sub/b.txt", "bee")
	alice.write(t, "Notes/secret.md", "private")
	alice.settle(t)

	// Not shared yet: Bob sees nothing.
	if got := bob.list(t, ""); len(got) != 0 {
		t.Fatalf("bob sees %v before any share", got)
	}
	out := alice.must(t, "files.share", map[string]any{"path": "Photos", "people": []string{"bob"}, "mode": "read"})
	if out["id"] == "" {
		t.Fatal("no share id")
	}
	waitFor(t, "bob lists the shared folder", func() bool { return strings.Join(bob.list(t, "Shared/alice/Photos"), ",") == "sub/,a.jpg" })
	if got := bob.list(t, "Shared/alice"); strings.Join(got, ",") != "Photos/" {
		t.Fatalf("bob sees %v: only the shared subtree may appear", got)
	}
	if got := bob.list(t, ""); strings.Join(got, ",") != "Shared/" {
		t.Fatalf("bob root: %v", got)
	}

	// Only that subtree's blobs are fetchable.
	st := alice.must(t, "files.stat", map[string]any{"path": "Notes/secret.md"})
	if _, err := w.b1.Client(appID).GetBlob(context.Background(), st["blob"].(string), sol.BlobOptions{From: w.alice.ID}); err == nil {
		t.Fatal("bob fetched a blob that was not shared")
	}

	// Shared folders are online only by default: nothing on Bob's disk, but streaming works by
	// blob hash with Range, without writing anything.
	if len(bob.files()) != 0 {
		t.Fatalf("shared folder must not be on disk by default: %v", bob.files())
	}
	rec := rawGet(t, bob, "Shared/alice/Photos/a.jpg", "bytes=2-5", true)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusPartialContent || string(body) != "2345" || rec.Header().Get("Content-Range") != "bytes 2-5/16" {
		t.Fatalf("range: %d %q %v", rec.Code, body, rec.Header())
	}
	rec = rawGet(t, bob, "Shared/alice/Photos/a.jpg", "", true)
	if body, _ := io.ReadAll(rec.Body); rec.Code != 200 || string(body) != "0123456789abcdef" || rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("full: %d %q", rec.Code, body)
	}
	if len(bob.files()) != 0 {
		t.Fatal("streaming wrote to disk")
	}
	if rawGet(t, bob, "Shared/alice/Photos/a.jpg", "", false).Code != http.StatusForbidden {
		t.Fatal("raw must require a self caller")
	}

	// Read only: Bob cannot write into it.
	if _, code := bob.act(t, "files.write", map[string]any{"path": "Shared/alice/Photos/x.txt", "text": "hi"}, false); code != http.StatusForbidden {
		t.Fatalf("write into read-only share: %d", code)
	}
	if _, code := bob.act(t, "files.mkdir", map[string]any{"path": "Shared/alice/new"}, false); code != http.StatusBadRequest {
		t.Fatalf("mkdir in the virtual folder: %d", code)
	}
	if _, code := bob.act(t, "files.delete", map[string]any{"paths": []string{"Shared/alice/Photos/a.jpg"}}, false); code != http.StatusForbidden {
		t.Fatalf("delete in read-only share: %d", code)
	}

	// Keep it: files land on disk under Shared/alice/Photos.
	bob.must(t, "files.keep", map[string]any{"path": "Shared/alice/Photos", "keep": true})
	if !bob.has("Shared/alice/Photos/a.jpg", "0123456789abcdef") || !bob.has("Shared/alice/Photos/sub/b.txt", "bee") {
		t.Fatalf("keep did not write files: %v", bob.files())
	}
	// Alice's later edits flow to Bob (mirrored into the share feed).
	alice.write(t, "Photos/a.jpg", "UPDATED")
	alice.write(t, "Photos/new.txt", "n")
	if err := osRemove(alice.path("Photos/sub/b.txt")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob follows alice", func() bool {
		return bob.has("Shared/alice/Photos/a.jpg", "UPDATED") && bob.has("Shared/alice/Photos/new.txt", "n") && bob.gone("Shared/alice/Photos/sub/b.txt")
	})

	// An edit Bob makes on disk to a read-only share is not published and not welcomed back.
	before := len(treeEntries(t, alice.api.API, w.alice.ID))
	bob.write(t, "Shared/alice/Photos/a.jpg", "BOB WAS HERE")
	bob.settle(t)
	if !alice.has("Photos/a.jpg", "UPDATED") || len(treeEntries(t, alice.api.API, w.alice.ID)) != before {
		t.Fatal("a read-only share leaked an edit back")
	}
	status := bob.must(t, "files.status", map[string]any{})
	if sk := status["skipped"].([]any); len(sk) != 1 {
		t.Fatalf("status should report the skipped edit: %v", sk)
	}

	// files.shared on both sides.
	sh := bob.must(t, "files.shared", map[string]any{})
	wm := sh["with_me"].([]any)
	if len(wm) != 1 || wm[0].(map[string]any)["path"] != "Shared/alice/Photos" || wm[0].(map[string]any)["mode"] != "read" {
		t.Fatalf("with_me: %v", wm)
	}
	by := alice.must(t, "files.shared", map[string]any{})["by_me"].([]any)
	if len(by) != 1 || by[0].(map[string]any)["path"] != "Photos" {
		t.Fatalf("by_me: %v", by)
	}

	// Unshare: Bob's view goes, his copies stay.
	alice.must(t, "files.unshare", map[string]any{"path": "Photos", "people": []string{"bob"}})
	waitFor(t, "share gone for bob", func() bool { return len(bob.list(t, "")) == 0 })
	bob.settle(t)
	if !bob.has("Shared/alice/Photos/new.txt", "n") {
		t.Fatal("unsharing must not delete copies Bob already has")
	}
	alice.write(t, "Photos/after.txt", "later")
	alice.settle(t)
	if _, ok := bob.read("Shared/alice/Photos/after.txt"); ok {
		t.Fatal("updates continued after unshare")
	}
}

func TestShareWriteComesBack(t *testing.T) {
	w := newWorld()
	a1 := startNode(t, w.a1, nil)
	a2 := startNode(t, w.a2, nil)
	bob := startNode(t, w.b1, nil)
	a1.write(t, "Docs/a.txt", "alice doc")
	waitFor(t, "a2 has it", func() bool { return a2.has("Docs/a.txt", "alice doc") })
	a1.must(t, "files.share", map[string]any{"path": "Docs", "people": []string{"bob"}, "mode": "write"})
	bob.must(t, "files.keep", map[string]any{"path": "Shared/alice/Docs", "keep": true})
	waitFor(t, "bob has the doc", func() bool { return bob.has("Shared/alice/Docs/a.txt", "alice doc") })

	// Bob writes through the action, and through the disk.
	bob.must(t, "files.write", map[string]any{"path": "Shared/alice/Docs/hello.txt", "text": "hi from bob"})
	waitFor(t, "alice's both devices get bob's file", func() bool { return a1.has("Docs/hello.txt", "hi from bob") && a2.has("Docs/hello.txt", "hi from bob") })
	bob.write(t, "Shared/alice/Docs/a.txt", "bob edited alice's doc")
	waitFor(t, "alice gets bob's edit on disk", func() bool { return a1.has("Docs/a.txt", "bob edited alice's doc") && a2.has("Docs/a.txt", "bob edited alice's doc") })
	bob.write(t, "Shared/alice/Docs/deep/new.txt", "deep")
	waitFor(t, "nested file", func() bool { return a1.has("Docs/deep/new.txt", "deep") })

	// And Alice's edit goes back to Bob, with Bob's edits intact.
	a2.write(t, "Docs/hello.txt", "alice replies")
	waitFor(t, "bob gets alice's reply", func() bool { return bob.has("Shared/alice/Docs/hello.txt", "alice replies") })
	a1.settle(t)
	bob.settle(t)
	if len(a1.app.getView().conflicts)+len(bob.app.getView().conflicts) != 0 {
		t.Fatal("sequential edits are not conflicts")
	}
	// Bob can delete too, and it reaches Alice.
	bob.must(t, "files.delete", map[string]any{"paths": []string{"Shared/alice/Docs/hello.txt"}})
	waitFor(t, "delete reaches alice", func() bool { return a1.gone("Docs/hello.txt") && a2.gone("Docs/hello.txt") })
}

func TestShareOverlapAndPeople(t *testing.T) {
	w := newWorld()
	alice := startNode(t, w.a1, nil)
	alice.write(t, "A/x.txt", "x")
	alice.write(t, "A/B/y.txt", "y")
	alice.must(t, "files.share", map[string]any{"path": "A", "people": []string{"bob"}})
	if _, code := alice.act(t, "files.share", map[string]any{"path": "A/B", "people": []string{"bob"}}, false); code != http.StatusConflict {
		t.Fatalf("overlapping share: %d", code)
	}
	if _, code := alice.act(t, "files.share", map[string]any{"path": "A", "people": []string{"nobody"}}, false); code != http.StatusNotFound {
		t.Fatalf("unknown person: %d", code)
	}
	if _, code := alice.act(t, "files.share", map[string]any{"path": "A/x.txt", "people": []string{"bob"}}, false); code != http.StatusBadRequest {
		t.Fatalf("sharing a file: %d", code)
	}
	if _, code := alice.act(t, "files.unshare", map[string]any{"path": "A/B"}, false); code != http.StatusNotFound {
		t.Fatalf("unshare unshared: %d", code)
	}
}
