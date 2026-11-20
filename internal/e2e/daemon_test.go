package e2e

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"testing"

	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/sdk/go/sol"
)

// TestInitAndAdmin: a directory holds one identity, and the CLI's admin API works.
func TestInitAndAdmin(t *testing.T) {
	d := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	if err := sold.Init(d.Dir(), sold.InitOptions{Name: "Eve", Device: "x"}); !errors.Is(err, sold.ErrExists) {
		t.Fatalf("second init: %v", err)
	}
	if _, err := sold.Start(sold.Options{Dir: t.TempDir()}); !errors.Is(err, sold.ErrNoInit) {
		t.Fatalf("start without init: %v", err)
	}
	c, err := sold.Connect(d.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var st sold.Status
	if err := c.Do(context.Background(), "GET", "/admin/v1/status", nil, &st); err != nil || st.Me.Person.Name != "Alice" || st.Me.Person.ID != d.PersonID() || len(st.Endpoints) != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	var inv struct{ Link string }
	if err := c.Do(context.Background(), "POST", "/admin/v1/invite", map[string]bool{"join": true}, &inv); err != nil || inv.Link == "" {
		t.Fatal(err)
	}
	bad, _ := sol.New(d.APIURL(), "nope")
	if _, err := bad.WhoAmI(context.Background()); !errors.Is(err, sol.ErrUnauthorized) {
		t.Fatalf("bad token: %v", err)
	}
}

// TestScopesAndApproval: the API enforces manifest scopes, and an "always" action waits for
// the person's approval, then runs.
func TestScopesAndApproval(t *testing.T) {
	d := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{}, 1)
	acts := sol.NewActions(nil)
	acts.Handle("tiny.go", func(context.Context, sol.Call) (any, error) {
		ran <- struct{}{}
		return map[string]bool{"ok": true}, nil
	})
	srv := &http.Server{Handler: acts}
	go srv.Serve(ln)
	defer srv.Close()
	m, err := sol.ParseManifest([]byte(`{"id":"tiny","version":"1","exec":"x","scopes":["actions","inbox"],
		"actions":[{"name":"tiny.go","effect":"write","approval":"always"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := d.Host.Attach(m, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	api, _ := sol.New(d.APIURL(), tok)
	ctx := context.Background()
	if _, err := api.Append(ctx, "x", 1); !errors.Is(err, sol.ErrForbidden) {
		t.Fatalf("feeds without scope: %v", err)
	}
	if _, err := api.People(ctx); !errors.Is(err, sol.ErrForbidden) {
		t.Fatalf("people without scope: %v", err)
	}
	if _, err := api.Events(ctx, sol.EventOptions{People: true}); !errors.Is(err, sol.ErrForbidden) {
		t.Fatalf("people events without scope: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	evs, err := api.Events(cctx, sol.EventOptions{Inbox: true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := api.CallAction(ctx, sol.ActionCall{Action: "tiny.go"})
	if err != nil || res.Approval == "" {
		t.Fatalf("%+v %v", res, err)
	}
	if ev := <-evs; ev.Inbox == nil || ev.Inbox.ID != res.Approval {
		t.Fatalf("%+v", ev)
	}
	select {
	case <-ran:
		t.Fatal("ran before approval")
	default:
	}
	if _, err := d.API.Answer(ctx, res.Approval, true); err != nil {
		t.Fatal(err)
	}
	<-ran
	if ev := <-evs; ev.Inbox == nil || ev.Inbox.Answer != "approved" {
		t.Fatalf("%+v", ev)
	}
	if _, err := d.API.Answer(ctx, res.Approval, true); !errors.Is(err, sol.ErrNotFound) {
		t.Fatalf("double answer: %v", err)
	}
	items := d.API.InboxItems()
	if len(items) != 1 || items[0].Answer != "approved" || string(items[0].Result) != `{"ok":true}` {
		t.Fatalf("%+v", items)
	}
}

// TestRestartRedials: a daemon that restarts reconnects to its peers from peers.json, keeps
// its feeds, and an app's event stream resumes.
func TestRestartRedials(t *testing.T) {
	alice := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	bob := startNode(t, sold.InitOptions{Name: "Bob", Device: "desktop"}, nil)
	pairNodes(t, alice, bob)
	waitSession(t, bob, alice)
	A, B := attachDocs(t, alice), attachDocs(t, bob)
	ctx := context.Background()
	if _, err := A.Append(ctx, "tree", "one"); err != nil {
		t.Fatal(err)
	}
	if err := A.Share(ctx, sol.ShareRequest{Feed: "tree", People: []string{bob.PersonID()}}); err != nil {
		t.Fatal(err)
	}
	addr := sol.Address(alice.PersonID(), "docs", "tree")
	eventually(t, "replication", func() bool { es, _ := B.Read(ctx, addr, 0, 0); return len(es) == 1 })

	// Bob restarts on the same directory; clearnet ports change, so Alice's record of him is
	// stale, but Bob still knows Alice's address and redials.
	dir := bob.Dir()
	bob.Close()
	bob2, err := sold.Start(sold.Options{Dir: dir, InProcessApps: true, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer bob2.Close()
	waitSession(t, bob2, alice)
	B2 := attachDocs(t, bob2)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	evs, err := B2.Events(cctx, sol.EventOptions{Feeds: []string{addr}})
	if err != nil {
		t.Fatal(err)
	}
	if es, err := B2.Read(ctx, addr, 0, 0); err != nil || len(es) != 1 {
		t.Fatalf("after restart: %v %v", es, err)
	}
	if _, err := A.Append(ctx, "tree", "two"); err != nil {
		t.Fatal(err)
	}
	if ev := <-evs; ev.Entry == nil || string(ev.Entry.Body) != `"two"` {
		t.Fatalf("%+v", ev)
	}
}
