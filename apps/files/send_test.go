package files

import (
	"context"
	"strings"
	"testing"

	"github.com/apv022/sol/sdk/go/sol"
	"github.com/apv022/sol/sdk/go/sol/memsol"
)

// approvals are a person's open Files approvals about sends.
func approvals(p *memsol.Person) []memsol.InboxItem {
	var out []memsol.InboxItem
	for _, it := range p.Inbox() {
		if it.App == appID && it.Kind == "approval" && strings.Contains(it.Title, "sent you") {
			out = append(out, it)
		}
	}
	return out
}

func sentStatus(t *testing.T, n *node) string {
	t.Helper()
	sent := n.must(t, "files.sent", map[string]any{})["sent"].([]any)
	if len(sent) == 0 {
		return ""
	}
	return sent[0].(map[string]any)["status"].(string)
}

func TestSendAccept(t *testing.T) {
	w := newWorld()
	alice := startNode(t, w.a1, nil)
	alice2 := startNode(t, w.a2, nil)
	bob := startNode(t, w.b1, nil)
	w.bob.Install(bob.app.manifest, bob.app.Handler())
	alice.write(t, "Photos/a.jpg", "jpeg bytes")
	alice.write(t, "notes.txt", "hello bob")
	alice.settle(t)

	out := alice.must(t, "files.send", map[string]any{"paths": []string{"Photos/a.jpg", "notes.txt"}, "person": "bob", "note": "for the trip"})
	if out["files"].(float64) != 2 {
		t.Fatalf("send: %v", out)
	}
	waitFor(t, "bob has one approval", func() bool { return len(approvals(w.bob)) == 1 })
	it := approvals(w.bob)[0]
	if !strings.HasPrefix(it.Title, "alice sent you 2 files (") || !strings.Contains(it.Body, "a.jpg") || !strings.Contains(it.Body, "for the trip") {
		t.Fatalf("approval: %q / %q", it.Title, it.Body)
	}
	if got := sentStatus(t, alice); got != "sent" {
		t.Fatalf("sent status %q", got)
	}
	rec := bob.must(t, "files.received", map[string]any{})["received"].([]any)
	if len(rec) != 1 || rec[0].(map[string]any)["status"] != "pending" {
		t.Fatalf("received: %v", rec)
	}

	// Approving the Inbox item runs files.receive.
	if err := w.bob.Answer(it.ID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "files saved under Received/alice", func() bool {
		return bob.has("Received/alice/a.jpg", "jpeg bytes") && bob.has("Received/alice/notes.txt", "hello bob")
	})
	waitFor(t, "alice sees saved", func() bool { return sentStatus(t, alice) == "saved" && sentStatus(t, alice2) == "saved" })
	rec = bob.must(t, "files.received", map[string]any{})["received"].([]any)
	if m := rec[0].(map[string]any); m["status"] != "saved" || m["saved_to"] != "Received/alice" {
		t.Fatalf("received after: %v", m)
	}
	if got := bob.list(t, "Received/alice"); strings.Join(got, ",") != "a.jpg,notes.txt" {
		t.Fatalf("tree: %v", got)
	}
	// Answering again (the Inbox item after Files, or twice) changes nothing.
	again := bob.must(t, "files.receive", map[string]any{"send": rec[0].(map[string]any)["send"], "accept": false})
	if again["already"] != true || again["status"] != "saved" {
		t.Fatalf("second answer: %v", again)
	}
	if len(approvals(w.bob)) != 1 {
		t.Fatalf("bob has %d approvals", len(approvals(w.bob)))
	}
}

func TestSendTwoDevicesOneApproval(t *testing.T) {
	w := newWorld()
	alice := startNode(t, w.a1, nil)
	b1 := startNode(t, w.b1, nil)
	w.bob.Install(b1.app.manifest, b1.app.Handler())
	b2 := startNode(t, w.bob.AddDevice("tablet", false), nil)
	alice.write(t, "x.txt", "x")
	alice.settle(t)
	alice.must(t, "files.send", map[string]any{"paths": []string{"x.txt"}, "person": "bob"})
	waitFor(t, "both bob devices know the send", func() bool {
		return len(b1.must(t, "files.received", map[string]any{})["received"].([]any)) == 1 && len(b2.must(t, "files.received", map[string]any{})["received"].([]any)) == 1
	})
	b1.settle(t)
	b2.settle(t)
	if n := len(approvals(w.bob)); n != 1 {
		t.Fatalf("bob has %d approvals, want 1", n)
	}
	// Saving on the device that did not file the approval reaches the other.
	rec := b2.must(t, "files.received", map[string]any{})["received"].([]any)
	b2.must(t, "files.receive", map[string]any{"send": rec[0].(map[string]any)["send"], "accept": true})
	waitFor(t, "both devices have the file", func() bool { return b1.has("Received/alice/x.txt", "x") && b2.has("Received/alice/x.txt", "x") })
}

func TestSendDecline(t *testing.T) {
	w := newWorld()
	alice := startNode(t, w.a1, nil)
	bob := startNode(t, w.b1, nil)
	w.bob.Install(bob.app.manifest, bob.app.Handler())
	alice.write(t, "a.txt", "a")
	alice.settle(t)
	alice.must(t, "files.send", map[string]any{"paths": []string{"a.txt"}, "person": "Bob"})
	waitFor(t, "approval", func() bool { return len(approvals(w.bob)) == 1 })
	if t1 := approvals(w.bob)[0].Title; !strings.HasPrefix(t1, "alice sent you a.txt (") {
		t.Fatalf("title %q", t1)
	}
	if err := w.bob.Answer(approvals(w.bob)[0].ID, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice sees declined", func() bool { return sentStatus(t, alice) == "declined" })
	bob.settle(t)
	if len(bob.files()) != 0 {
		t.Fatalf("declined send wrote %v", bob.files())
	}
	rec := bob.must(t, "files.received", map[string]any{})["received"].([]any)
	if rec[0].(map[string]any)["status"] != "declined" {
		t.Fatalf("received: %v", rec)
	}
	// A restart does not file it again.
	bob.restart(t, w.b1)
	bob.settle(t)
	if len(approvals(w.bob)) != 1 {
		t.Fatalf("approvals after restart: %d", len(approvals(w.bob)))
	}
}

func TestSendNameClash(t *testing.T) {
	w := newWorld()
	alice := startNode(t, w.a1, nil)
	bob := startNode(t, w.b1, nil)
	w.bob.Install(bob.app.manifest, bob.app.Handler())
	alice.write(t, "a.txt", "from alice")
	alice.write(t, "b/a.txt", "second a")
	alice.settle(t)
	bob.write(t, "Received/alice/a.txt", "already here")
	bob.settle(t)
	alice.must(t, "files.send", map[string]any{"paths": []string{"a.txt", "b/a.txt"}, "person": "bob"})
	waitFor(t, "approval", func() bool { return len(approvals(w.bob)) == 1 })
	if err := w.bob.Answer(approvals(w.bob)[0].ID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "clashing names get numbers", func() bool {
		return bob.has("Received/alice/a (2).txt", "from alice") && bob.has("Received/alice/a (3).txt", "second a")
	})
	if !bob.has("Received/alice/a.txt", "already here") {
		t.Fatal("the existing file was replaced")
	}
}

func TestSendRefusals(t *testing.T) {
	w := newWorld()
	alice := startNode(t, w.a1, nil)
	alice.write(t, "Dir/a.txt", "a")
	alice.settle(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"empty", map[string]any{"paths": []string{}, "person": "bob"}},
		{"folder", map[string]any{"paths": []string{"Dir"}, "person": "bob"}},
		{"missing", map[string]any{"paths": []string{"nope.txt"}, "person": "bob"}},
		{"nobody", map[string]any{"paths": []string{"Dir/a.txt"}, "person": "zed"}},
		{"no person", map[string]any{"paths": []string{"Dir/a.txt"}}},
	}
	for _, c := range cases {
		if out, code := alice.act(t, "files.send", c.args, false); code == 200 {
			t.Fatalf("%s accepted: %v", c.name, out)
		}
	}
	out, _ := alice.act(t, "files.send", map[string]any{"paths": []string{"Dir"}, "person": "bob"}, false)
	if !strings.Contains(out["error"].(map[string]any)["message"].(string), "share the folder") {
		t.Fatalf("folder message: %v", out)
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = "f" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".txt"
	}
	if _, code := alice.act(t, "files.send", map[string]any{"paths": many, "person": "bob"}, false); code == 200 {
		t.Fatal("101 files accepted")
	}
	if got := alice.must(t, "files.sent", map[string]any{})["sent"].([]any); len(got) != 0 {
		t.Fatalf("refused sends were recorded: %v", got)
	}
	// Only my own devices may call it.
	if _, code := actOn(t, alice.app.Handler(), "files.send", map[string]any{"paths": []string{"Dir/a.txt"}, "person": "bob"}, false, false); code != 403 {
		t.Fatalf("peer call: %d", code)
	}
}

func TestSendDryRun(t *testing.T) {
	w := newWorld()
	alice := startNode(t, w.a1, nil)
	bob := startNode(t, w.b1, nil)
	w.bob.Install(bob.app.manifest, bob.app.Handler())
	alice.write(t, "a.txt", "a")
	alice.settle(t)
	out, code := alice.act(t, "files.send", map[string]any{"paths": []string{"a.txt"}, "person": "bob", "note": "hi"}, true)
	if code != 200 || out["dry_run"] != true {
		t.Fatalf("dry run: %d %v", code, out)
	}
	if es, err := w.a1.Client(appID).Read(context.Background(), sol.Address(w.alice.ID, appID, "send."+w.bob.ID), 1, 10); err == nil && len(es) != 0 {
		t.Fatalf("dry run wrote %d entries", len(es))
	}
	bob.settle(t)
	if len(approvals(w.bob)) != 0 || len(alice.must(t, "files.sent", map[string]any{})["sent"].([]any)) != 0 {
		t.Fatal("dry run sent something")
	}
	// Receive dry runs change nothing either.
	alice.must(t, "files.send", map[string]any{"paths": []string{"a.txt"}, "person": "bob"})
	waitFor(t, "approval", func() bool { return len(approvals(w.bob)) == 1 })
	rec := bob.must(t, "files.received", map[string]any{})["received"].([]any)
	if out, _ := bob.act(t, "files.receive", map[string]any{"send": rec[0].(map[string]any)["send"], "accept": true}, true); out["dry_run"] != true {
		t.Fatalf("receive dry run: %v", out)
	}
	if len(bob.files()) != 0 {
		t.Fatal("receive dry run wrote files")
	}
}

func TestSendFeedHiddenFromOthers(t *testing.T) {
	w := newWorld()
	carol := w.net.AddPerson("Carol")
	w.net.Pair(w.alice, carol)
	alice := startNode(t, w.a1, nil)
	cn := startNode(t, carol.Devices[0], nil)
	alice.write(t, "secret.txt", "for bob only")
	alice.settle(t)
	out := alice.must(t, "files.send", map[string]any{"paths": []string{"secret.txt"}, "person": "bob"})
	addr := sol.Address(w.alice.ID, appID, "send."+w.bob.ID)
	if _, err := carol.Devices[0].Client(appID).Read(context.Background(), addr, 1, 10); err == nil {
		t.Fatal("carol can read bob's send feed")
	}
	st := alice.must(t, "files.stat", map[string]any{"path": "secret.txt"})
	if _, err := carol.Devices[0].Client(appID).GetBlob(context.Background(), st["blob"].(string), sol.BlobOptions{From: w.alice.ID}); err == nil {
		t.Fatal("carol fetched bob's blob")
	}
	cn.settle(t)
	if n := len(approvals(carol)); n != 0 || len(cn.must(t, "files.received", map[string]any{})["received"].([]any)) != 0 {
		t.Fatalf("carol saw a send meant for bob (%v)", out)
	}
}
