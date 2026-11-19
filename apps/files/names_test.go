package files

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/apv022/sol/sdk/go/sol"
)

func TestInvalidNamesAreRefused(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	before := len(treeEntries(t, a.api.API, w.alice.ID))
	for _, p := range []string{"CON", "dir/aux.txt", "a:b.txt", "trail.", "trail /x", "q?.txt", "x|y", `x\y`, "../up", "a/../b", "nul", "COM1.log", "Shared"} {
		for _, action := range []string{"files.write", "files.mkdir"} {
			args := map[string]any{"path": p}
			if action == "files.write" {
				args["text"] = "x"
			}
			if out, code := a.act(t, action, args, false); code == http.StatusOK {
				t.Errorf("%s %q accepted: %v", action, p, out)
			}
		}
	}
	a.must(t, "files.write", map[string]any{"path": "ok.txt", "text": "x"})
	a.write(t, "fine/x", "1")
	if _, code := a.act(t, "files.move", map[string]any{"from": "ok.txt", "to": "bad:name"}, false); code != http.StatusBadRequest {
		t.Fatalf("move to invalid name: %d", code)
	}
	if _, code := a.act(t, "files.copy", map[string]any{"from": "ok.txt", "to": "bad."}, false); code != http.StatusBadRequest {
		t.Fatalf("copy to invalid name: %d", code)
	}
	if got := len(treeEntries(t, a.api.API, w.alice.ID)) - before; got != 1 {
		t.Fatalf("only the one good write may reach the feed, got %d entries", got)
	}
}

func TestInvalidNamesFromDiskAreSkippedAndFromFeedIgnored(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.write(t, "good.txt", "g")
	a.write(t, "has:colon.txt", "bad on windows")
	a.write(t, "ends.with.dot.", "bad")
	waitFor(t, "good file travels", func() bool { return b.has("good.txt", "g") })
	a.settle(t)
	if b.has("has:colon.txt", "bad on windows") || len(b.files()) != 1 {
		t.Fatalf("invalid names reached the other device: %v", b.files())
	}
	st := a.must(t, "files.status", map[string]any{})
	if sk := st["skipped"].([]any); len(sk) != 2 || !strings.Contains(sk[0].(map[string]any)["reason"].(string), "Windows") {
		t.Fatalf("status.skipped: %v", sk)
	}
	// A hostile or buggy device appends an invalid path: every fold ignores it.
	inj := w.a1.Client(appID)
	for _, p := range []string{"CON", "../escape", "/abs", "a/../../b", "x:y"} {
		if _, err := inj.Append(context.Background(), treeFeed, body{Op: opPut, Path: p, Blob: h1, Size: 1, Mtime: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := inj.Append(context.Background(), treeFeed, body{Op: opPut, Path: "after.txt", Blob: mustBlob(t, a, "after"), Size: 5, Mtime: 5}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "valid entry after the bad ones applies", func() bool { return b.has("after.txt", "after") })
	if len(b.files()) != 2 {
		t.Fatalf("invalid paths were written: %v", b.files())
	}
}

func mustBlob(t *testing.T, n *node, text string) string {
	t.Helper()
	info, err := n.api.PutBlob(context.Background(), strings.NewReader(text), "")
	if err != nil {
		t.Fatal(err)
	}
	return info.Hash
}

func TestNFCNamesAreNormalised(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	a.must(t, "files.write", map[string]any{"path": "café.txt", "text": "x"})
	if got := a.list(t, ""); len(got) != 1 || got[0] != "café.txt" {
		t.Fatalf("names: %q", got)
	}
	if !a.has("café.txt", "x") {
		t.Fatalf("on disk: %v", a.files())
	}
	// Peers cannot make Files treat a decomposed name as valid.
	if _, err := w.a1.Client(appID).Append(context.Background(), treeFeed, body{Op: opPut, Path: "caféb.txt", Blob: h1, Size: 1, Mtime: 1}); err != nil {
		t.Fatal(err)
	}
	if err := a.app.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.list(t, ""); len(got) != 1 {
		t.Fatalf("non-NFC wire path accepted: %q", got)
	}
}

func TestPeersCannotCallActions(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	a.write(t, "x.txt", "x")
	a.settle(t)
	for _, name := range []string{"files.list", "files.read", "files.status", "files.shared", "files.conflicts"} {
		if _, code := actOn(t, a.app.Handler(), name, map[string]any{"path": "x.txt"}, false, false); code != http.StatusForbidden {
			t.Errorf("%s from a peer: %d", name, code)
		}
	}
	if _, code := actOn(t, a.app.Handler(), "files.delete", map[string]any{"paths": []string{"x.txt"}}, false, false); code != http.StatusForbidden {
		t.Errorf("delete from a peer: %d", code)
	}
	if !a.has("x.txt", "x") {
		t.Fatal("peer deleted a file")
	}
	// Schema validation still applies to my own calls.
	if _, code := a.act(t, "files.write", map[string]any{}, false); code != http.StatusBadRequest {
		t.Fatalf("missing path: %d", code)
	}
	var _ sol.API = a.api
}
