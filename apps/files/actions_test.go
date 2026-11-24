package files

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/apv022/sol/sdk/go/sol"
)

// snapshot captures everything an action could change: the folder, every feed of both people
// and Bob's view of what is shared with him.
func snapshot(t *testing.T, w *world, ns ...*node) string {
	t.Helper()
	var sb strings.Builder
	for _, n := range ns {
		for _, f := range n.files() {
			c, _ := n.read(f)
			fmt.Fprintf(&sb, "disk %s=%s\n", f, c)
		}
	}
	for _, p := range []string{w.alice.ID, w.bob.ID} {
		fs, _ := w.a1.Client(appID).Feeds(context.Background(), p)
		fs2, _ := w.b1.Client(appID).Feeds(context.Background(), p)
		fmt.Fprintf(&sb, "feeds %v %v\n", fs, fs2)
	}
	cs, _ := w.b1.Client(appID).People(context.Background())
	for _, c := range cs {
		fmt.Fprintf(&sb, "contact %s %v %v\n", c.Name, c.SharesWithMe, c.IShare)
	}
	fmt.Fprintf(&sb, "inbox %d\n", len(w.alice.Inbox()))
	return sb.String()
}

func TestEveryActionHonoursDryRun(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.b1, nil)
	a.write(t, "Docs/a.txt", "a")
	a.write(t, "Docs/sub/b.txt", "b")
	a.write(t, "Other/c.txt", "c")
	a.must(t, "files.share", map[string]any{"path": "Other", "people": []string{"bob"}})
	a.settle(t)
	// A conflict for resolve to look at.
	blob := mustBlob(t, a, "rival")
	if _, err := w.a2.Client(appID).Append(context.Background(), treeFeed, body{Op: opPut, Path: "Docs/a.txt", Blob: blob, Size: 5, Mtime: 1, Dev: "laptop2"}); err != nil {
		t.Fatal(err)
	}
	if err := a.app.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.settle(t)
	if len(a.app.getView().conflicts) != 1 {
		t.Fatalf("setup: want a conflict: %v", a.files())
	}
	conflictCopy := ""
	for _, f := range a.files() {
		if strings.Contains(f, "conflict from") {
			conflictCopy = f
		}
	}
	st := a.must(t, "files.stat", map[string]any{"path": "Docs/a.txt"})
	hash := st["blob"].(string)

	calls := []struct {
		name string
		args map[string]any
	}{
		{"files.write", map[string]any{"path": "Docs/new.txt", "text": "t"}},
		{"files.write", map[string]any{"path": "Docs/a.txt", "blob": hash}},
		{"files.mkdir", map[string]any{"path": "Docs/newdir"}},
		{"files.move", map[string]any{"from": "Docs/sub", "to": "Docs/moved"}},
		{"files.move", map[string]any{"from": "Docs/sub/b.txt", "to": "Other/b.txt"}},
		{"files.copy", map[string]any{"from": "Docs", "to": "Docs2"}},
		{"files.delete", map[string]any{"paths": []string{"Docs/sub", "Other/c.txt"}}},
		{"files.share", map[string]any{"path": "Docs", "people": []string{"bob"}, "mode": "write"}},
		{"files.unshare", map[string]any{"path": "Other", "people": []string{"bob"}}},
		{"files.keep", map[string]any{"path": "Docs", "keep": false}},
		{"files.resolve", map[string]any{"path": "Docs/a.txt", "keep": "that"}},
		{"files.resolve", map[string]any{"path": conflictCopy, "keep": "both"}},
		{"files.sync", map[string]any{}},
	}
	for _, c := range calls {
		before := snapshot(t, w, a, b)
		out, code := a.act(t, c.name, c.args, true)
		if code != http.StatusOK || out["dry_run"] != true {
			t.Errorf("%s dry run: %d %v", c.name, code, out)
			continue
		}
		if would, _ := out["would"].([]any); len(would) == 0 {
			t.Errorf("%s dry run describes nothing: %v", c.name, out)
		}
		a.settle(t)
		if after := snapshot(t, w, a, b); after != before {
			t.Errorf("%s --dry-run changed things:\n%s\n---\n%s", c.name, before, after)
		}
	}
	// Read actions are read only anyway and take dry-run without complaint.
	for _, name := range []string{"files.list", "files.stat", "files.read", "files.search", "files.shared", "files.conflicts", "files.status"} {
		args := map[string]any{"path": "Docs/a.txt", "query": "a"}
		if name == "files.list" {
			args = map[string]any{"path": "Docs"}
		}
		if _, code := a.act(t, name, args, true); code != http.StatusOK {
			t.Errorf("%s: %d", name, code)
		}
	}
	// Dry run still validates: a bad call fails the same way.
	if _, code := a.act(t, "files.write", map[string]any{"path": "CON", "text": "x"}, true); code != http.StatusBadRequest {
		t.Errorf("dry-run must validate: %d", code)
	}
	if _, code := a.act(t, "files.delete", map[string]any{"paths": []string{"nope"}}, true); code != http.StatusNotFound {
		t.Errorf("dry-run delete of nothing: %d", code)
	}
}

func TestActionsBasics(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	a.must(t, "files.mkdir", map[string]any{"path": "Empty"})
	a.must(t, "files.write", map[string]any{"path": "Docs/a.txt", "text": "alpha"})
	a.must(t, "files.write", map[string]any{"path": "Docs/b.txt", "text": "beta"})
	if got := strings.Join(a.list(t, ""), ","); got != "Docs/,Empty/" {
		t.Fatalf("root: %s", got)
	}
	if !a.has("Docs/a.txt", "alpha") {
		t.Fatalf("action write must reach the folder: %v", a.files())
	}
	if st, err := osStat(a.path("Empty")); err != nil || !st.IsDir() {
		t.Fatal("mkdir must create the folder")
	}
	st := a.must(t, "files.stat", map[string]any{"path": "Docs/a.txt"})
	if st["type"] != "file" || st["size"].(float64) != 5 || st["kept"] != true || st["local"] != true {
		t.Fatalf("stat: %v", st)
	}
	if _, code := a.act(t, "files.stat", map[string]any{"path": "nope"}, false); code != http.StatusNotFound {
		t.Fatalf("stat missing: %d", code)
	}
	if _, code := a.act(t, "files.list", map[string]any{"path": "Docs/a.txt"}, false); code != http.StatusBadRequest {
		t.Fatalf("list a file: %d", code)
	}
	// Write by blob (the UI's upload path), with and without base.
	blob := mustBlob(t, a, "uploaded bytes")
	a.must(t, "files.write", map[string]any{"path": "Docs/up.bin", "blob": blob})
	if !a.has("Docs/up.bin", "uploaded bytes") {
		t.Fatal("blob write")
	}
	if _, code := a.act(t, "files.write", map[string]any{"path": "x.bin", "blob": "sha256:" + strings.Repeat("0", 64)}, false); code != http.StatusNotFound {
		t.Fatalf("unknown blob: %d", code)
	}
	if _, code := a.act(t, "files.write", map[string]any{"path": "x", "blob": blob, "text": "x"}, false); code != http.StatusBadRequest {
		t.Fatalf("both blob and text: %d", code)
	}
	if _, code := a.act(t, "files.write", map[string]any{"path": "Docs", "text": "x"}, false); code != http.StatusConflict {
		t.Fatalf("write over a folder: %d", code)
	}
	if _, code := a.act(t, "files.write", map[string]any{"path": "Docs/a.txt/inside", "text": "x"}, false); code != http.StatusConflict {
		t.Fatalf("write inside a file: %d", code)
	}
	// A write whose base is stale makes a conflict instead of overwriting.
	a.must(t, "files.write", map[string]any{"path": "Docs/a.txt", "text": "alpha 2"})
	a.must(t, "files.write", map[string]any{"path": "Docs/a.txt", "text": "alpha stale", "base": st["blob"]})
	if c := a.must(t, "files.conflicts", map[string]any{})["conflicts"].([]any); len(c) != 1 {
		t.Fatalf("stale base must conflict: %v", c)
	}
	a.must(t, "files.resolve", map[string]any{"path": "Docs/a.txt", "keep": "this"})

	// move, copy, search, delete
	a.must(t, "files.move", map[string]any{"from": "Docs/b.txt", "to": "Docs/renamed.txt"})
	a.must(t, "files.copy", map[string]any{"from": "Docs", "to": "Copy"})
	if !a.has("Copy/renamed.txt", "beta") || !a.has("Docs/renamed.txt", "beta") || !a.gone("Docs/b.txt") {
		t.Fatalf("move/copy: %v", a.files())
	}
	if _, code := a.act(t, "files.move", map[string]any{"from": "Docs", "to": "Copy"}, false); code != http.StatusConflict {
		t.Fatalf("move onto existing: %d", code)
	}
	if _, code := a.act(t, "files.move", map[string]any{"from": "Docs", "to": "Docs/inside"}, false); code != http.StatusBadRequest {
		t.Fatalf("move into itself: %d", code)
	}
	res := a.must(t, "files.search", map[string]any{"query": "RENAMED"})["results"].([]any)
	if len(res) != 2 {
		t.Fatalf("search: %v", res)
	}
	a.must(t, "files.delete", map[string]any{"paths": []string{"Copy", "Empty"}})
	if !a.gone("Copy") || !a.gone("Empty") || !a.has("Docs/renamed.txt", "beta") {
		t.Fatalf("delete: %v", a.files())
	}
	if got := strings.Join(a.list(t, ""), ","); got != "Docs/" {
		t.Fatalf("root after delete: %s", got)
	}
	// The deletes above are still being published; wait for the sync to go idle.
	a.settle(t)
	status := a.must(t, "files.status", map[string]any{})
	devs := status["devices"].([]any)
	if len(devs) != 2 || status["state"] != "synced" || status["folder"] != a.root {
		t.Fatalf("status: %v", status)
	}
	if status["watcher"] != "inotify" && status["watcher"] != "scan" {
		t.Fatalf("watcher: %v", status["watcher"])
	}
}

func TestAgentDeleteNeedsApprovalThroughMemsol(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	w.alice.Install(a.app.manifest, a.app.Handler())
	a.write(t, "important.txt", "keep me")
	a.settle(t)
	ctx := context.Background()
	me := w.a1.Client(appID)
	agent := me.AsAgent("agent-1")

	// An agent's delete waits for the person.
	args, _ := json.Marshal(map[string]any{"paths": []string{"important.txt"}})
	res, err := agent.CallAction(ctx, sol.ActionCall{Action: "files.delete", Args: args})
	if err != nil || res.Approval == "" {
		t.Fatalf("agent delete: %+v %v", res, err)
	}
	if !a.has("important.txt", "keep me") {
		t.Fatal("deleted before approval")
	}
	// The same call with dry run needs no approval and changes nothing.
	res, err = agent.CallAction(ctx, sol.ActionCall{Action: "files.delete", Args: args, DryRun: true})
	if err != nil || res.Approval != "" || !strings.Contains(string(res.Output), `"dry_run":true`) {
		t.Fatalf("agent dry run: %+v %v", res, err)
	}
	if err := w.alice.Answer(res0(t, agent, args), true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "approved delete runs", func() bool { return a.gone("important.txt") })

	// Reads and writes by an agent need no approval.
	out, err := agent.CallAction(ctx, sol.ActionCall{Action: "files.write", Args: json.RawMessage(`{"path":"agent.txt","text":"by agent"}`)})
	if err != nil || out.Approval != "" {
		t.Fatalf("agent write: %+v %v", out, err)
	}
	waitFor(t, "agent write on disk", func() bool { return a.has("agent.txt", "by agent") })
	// My own call over the memsol path, with --dry-run.
	out, err = me.CallAction(ctx, sol.ActionCall{Action: "files.move", Args: json.RawMessage(`{"from":"agent.txt","to":"moved.txt"}`), DryRun: true})
	if err != nil || !strings.Contains(string(out.Output), `"would"`) || !a.has("agent.txt", "by agent") {
		t.Fatalf("dry-run move: %+v %v", out, err)
	}
	// The action list the CLI and agents see matches the manifest.
	specs, _ := me.Actions(ctx, appID)
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	if len(names) != 29 {
		t.Fatalf("actions: %v", names)
	}
	for _, s := range specs {
		if s.Name == "files.send" && (s.Effect != sol.EffectWrite || s.Approval != sol.ApprovalAgents) {
			t.Fatalf("send policy: %+v", s)
		}
		if s.Name == "files.receive" && s.Effect != sol.EffectWrite {
			t.Fatalf("receive policy: %+v", s)
		}
		switch s.Name {
		case "files.delete", "files.trash_purge", "files.trash_empty", "files.version_delete":
			if s.Effect != sol.EffectDestructive || s.Approval != sol.ApprovalAgents {
				t.Fatalf("%s policy: %+v", s.Name, s)
			}
		}
	}
}

// res0 asks again (not dry run) and returns the approval id of the pending delete.
func res0(t *testing.T, agent sol.API, args json.RawMessage) string {
	t.Helper()
	r, err := agent.CallAction(context.Background(), sol.ActionCall{Action: "files.delete", Args: args})
	if err != nil || r.Approval == "" {
		t.Fatalf("second delete: %+v %v", r, err)
	}
	return r.Approval
}
