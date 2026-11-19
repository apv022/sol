package memsol_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
	"github.com/apv022/sol/sdk/go/sol/memsol"
)

const manifest = `{"id":"docs","version":"1","exec":"docs","scopes":["feeds","blobs","people","dial","actions"],
 "actions":[
  {"name":"docs.count","effect":"read","input":{"type":"object","properties":{"n":{"type":"integer"}}}},
  {"name":"docs.rename","effect":"write","input":{"type":"object","required":["title"],"properties":{"title":{"type":"string"}}}},
  {"name":"docs.wipe","effect":"destructive","approval":"agents"}]}`

type world struct {
	n                 *memsol.Network
	alice, bob, carol *memsol.Person
	alice2            *memsol.Device
	A, A2, B, C       sol.API // docs app of each, the way the test mode reaches it
	ac, bc, cc        *memsol.Client
	aliceID, bobID    string
}

// newWorld builds Alice (two devices), Bob and Carol. Alice is paired with both; Bob and Carol
// are not paired. mode is "direct" (memsol clients) or "http" (real sol.Client over Server).
func newWorld(t *testing.T, mode string) *world {
	t.Helper()
	n := memsol.New()
	w := &world{n: n}
	w.alice, w.bob, w.carol = n.AddPerson("Alice"), n.AddPerson("Bob"), n.AddPerson("Carol")
	w.alice2 = w.alice.AddDevice("phone", false)
	n.Pair(w.alice, w.bob)
	n.Pair(w.alice, w.carol)
	w.ac, w.bc, w.cc = w.alice.Client("docs"), w.bob.Client("docs"), w.carol.Client("docs")
	a2 := w.alice2.Client("docs")
	w.aliceID, w.bobID = w.alice.ID, w.bob.ID
	m, err := sol.ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*memsol.Person{w.alice, w.bob, w.carol} {
		acts := sol.NewActions(m)
		acts.Handle("docs.count", func(_ context.Context, c sol.Call) (any, error) {
			return map[string]any{"self": c.Caller.Self, "who": c.Caller.Person, "agent": c.Caller.Agent}, nil
		})
		acts.Handle("docs.rename", func(_ context.Context, c sol.Call) (any, error) {
			return map[string]any{"dry": c.DryRun}, nil
		})
		acts.Handle("docs.wipe", func(context.Context, sol.Call) (any, error) { return map[string]string{"wiped": "yes"}, nil })
		p.Install(m, acts)
		p.OnDial("docs", func(conn net.Conn, c sol.Caller, path string) {
			defer conn.Close()
			io.WriteString(conn, "hello "+c.Person[:4]+" "+path+"\n")
			_, _ = io.Copy(conn, conn) // echo
		})
	}
	switch mode {
	case "direct":
		w.A, w.A2, w.B, w.C = w.ac, a2, w.bc, w.cc
	case "http":
		ts := httptest.NewServer(memsol.NewServer(n))
		t.Cleanup(ts.Close)
		mk := func(c *memsol.Client) sol.API {
			cl, err := sol.New(ts.URL, c.Token())
			if err != nil {
				t.Fatal(err)
			}
			return cl
		}
		w.A, w.A2, w.B, w.C = mk(w.ac), mk(a2), mk(w.bc), mk(w.cc)
	}
	return w
}

func each(t *testing.T, f func(t *testing.T, w *world)) {
	for _, mode := range []string{"direct", "http"} {
		t.Run(mode, func(t *testing.T) { f(t, newWorld(t, mode)) })
	}
}

func wait(t *testing.T, ch <-chan sol.Event) sol.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	return sol.Event{}
}

func quiet(t *testing.T, ch <-chan sol.Event) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

var ctx = context.Background()

func TestIdentity(t *testing.T) {
	each(t, func(t *testing.T, w *world) {
		me, err := w.A.WhoAmI(ctx)
		if err != nil || me.Person.Name != "Alice" || me.App.ID != "docs" || len(me.Person.ID) != 52 || me.Device.ID == "" {
			t.Fatalf("%+v %v", me, err)
		}
		ppl, _ := w.A.People(ctx)
		if len(ppl) != 2 || ppl[0].Name != "Bob" || ppl[0].Petname != "bob" || !ppl[0].Online || len(ppl[0].Devices) != 1 {
			t.Fatalf("%+v", ppl)
		}
		ppl, _ = w.B.People(ctx)
		if len(ppl) != 1 || ppl[0].Name != "Alice" || len(ppl[0].Devices) != 2 {
			t.Fatalf("%+v", ppl)
		}
		devs, _ := w.A.Devices(ctx)
		if len(devs) != 2 {
			t.Fatal(devs)
		}
	})
}

func TestShareReadAndEvents(t *testing.T) {
	each(t, func(t *testing.T, w *world) {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		addr := sol.Address(w.aliceID, "docs", "tree")
		e1, err := w.A.Append(ctx, "tree", map[string]any{"n": 1})
		if err != nil || e1.Seq != 1 || e1.Pos != 1 || e1.Prev != "" || e1.Feed != addr {
			t.Fatalf("%+v %v", e1, err)
		}
		bobEv, err := w.B.Events(cctx, sol.EventOptions{People: true})
		if err != nil {
			t.Fatal(err)
		}
		// Not shared yet.
		if _, err := w.B.Read(ctx, addr, 0, 0); !errors.Is(err, sol.ErrForbidden) {
			t.Fatalf("want forbidden, got %v", err)
		}
		// A different app's feed is off limits even for the owner.
		if _, err := w.A.Read(ctx, sol.Address(w.aliceID, "files", "x"), 0, 0); !errors.Is(err, sol.ErrForbidden) {
			t.Fatalf("want forbidden, got %v", err)
		}
		if err := w.A.Share(ctx, sol.ShareRequest{Feed: "tree", People: []string{w.bobID}}); err != nil {
			t.Fatal(err)
		}
		if fe := wait(t, bobEv); fe.Type != sol.EventFeed || !fe.Feed.Readable || fe.Feed.Feed != addr {
			t.Fatalf("%+v", fe)
		}
		if pe := wait(t, bobEv); pe.Type != sol.EventPerson || len(pe.Person.SharesWithMe) != 1 || pe.Person.SharesWithMe[0] != "docs/tree" {
			t.Fatalf("%+v", pe.Person)
		}
		shares, _ := w.A.Shares(ctx, "tree")
		if len(shares) != 1 || shares[0].Person != w.bobID || shares[0].Mode != "read" {
			t.Fatalf("%+v", shares)
		}
		got, err := w.B.Read(ctx, addr, 0, 0)
		if err != nil || len(got) != 1 || string(got[0].Body) != `{"n":1}` {
			t.Fatalf("%+v %v", got, err)
		}
		fs, _ := w.B.Feeds(ctx, w.aliceID)
		if len(fs) != 1 || fs[0].Feed != addr || fs[0].Pos != 1 {
			t.Fatalf("%+v", fs)
		}
		if fs, _ := w.C.Feeds(ctx, ""); len(fs) != 0 {
			t.Fatalf("carol sees %+v", fs)
		}

		// Live entries; Alice's second device appends to the same feed with its own chain.
		live, err := w.B.Events(cctx, sol.EventOptions{Feeds: []string{addr}})
		if err != nil {
			t.Fatal(err)
		}
		e2, _ := w.A2.Append(ctx, "tree", "from phone")
		e3, _ := w.A.Append(ctx, "tree", map[string]any{"n": 3})
		if e2.Seq != 1 || e2.Pos != 2 || e2.Prev != "" || e3.Seq != 2 || e3.Pos != 3 || e3.Prev != e1.Hash || e2.Device == e1.Device {
			t.Fatalf("chains: %+v %+v %+v", e1, e2, e3)
		}
		first := wait(t, live)
		if first.Type != sol.EventEntry || first.Entry.Pos != 2 || first.Entry.Hash != e2.Hash {
			t.Fatalf("%+v", first)
		}
		second := wait(t, live)
		if second.Entry.Pos != 3 {
			t.Fatalf("%+v", second)
		}
		// Resume after the first live event: only the second comes back.
		resumed, err := w.B.Events(cctx, sol.EventOptions{Feeds: []string{addr}, LastEventID: first.ID})
		if err != nil {
			t.Fatal(err)
		}
		if ev := wait(t, resumed); ev.Entry == nil || ev.Entry.Pos != 3 {
			t.Fatalf("resume: %+v", ev)
		}
		// Read from a cursor, with a limit, and verify the per-device chains.
		page, _ := w.B.Read(ctx, addr, 2, 1)
		if len(page) != 1 || page[0].Pos != 2 {
			t.Fatalf("%+v", page)
		}
		all, _ := w.B.Read(ctx, addr, 1, 0)
		if err := sol.VerifyChain(all); err != nil || len(all) != 3 {
			t.Fatalf("%v %d", err, len(all))
		}
		// Carol sees nothing of this.
		if _, err := w.C.Read(ctx, addr, 0, 0); !errors.Is(err, sol.ErrForbidden) {
			t.Fatal(err)
		}

		// Unshare: Bob loses access and is told.
		if err := w.A.Unshare(ctx, sol.ShareRequest{Feed: "tree", People: []string{w.bobID}}); err != nil {
			t.Fatal(err)
		}
		fe := wait(t, bobEv)
		for fe.Type == sol.EventEntry { // entries Bob's wide subscription got while shared
			fe = wait(t, bobEv)
		}
		if fe.Type != sol.EventFeed || fe.Feed.Readable {
			t.Fatalf("%+v", fe)
		}
		if _, err := w.B.Read(ctx, addr, 0, 0); !errors.Is(err, sol.ErrForbidden) {
			t.Fatal(err)
		}
	})
}

func TestBodyLimitAndNames(t *testing.T) {
	each(t, func(t *testing.T, w *world) {
		big := strings.Repeat("x", sol.MaxBodySize)
		if _, err := w.A.Append(ctx, "f", big); !errors.Is(err, sol.ErrBadRequest) {
			t.Fatalf("big body: %v", err)
		}
		if _, err := w.A.Append(ctx, "Bad Name", 1); !errors.Is(err, sol.ErrBadRequest) {
			t.Fatalf("bad name: %v", err)
		}
		if err := w.A.Share(ctx, sol.ShareRequest{Feed: "f", People: []string{"nobody"}}); !errors.Is(err, sol.ErrNotFound) {
			t.Fatalf("unknown person: %v", err)
		}
	})
}

func TestBlobPermissions(t *testing.T) {
	each(t, func(t *testing.T, w *world) {
		data := []byte("0123456789abcdef")
		info, err := w.A.PutBlob(ctx, bytes.NewReader(data), "x.txt")
		if err != nil || info.Size != 16 || !sol.ValidHash(info.Hash) {
			t.Fatalf("%+v %v", info, err)
		}
		again, _ := w.A.PutBlob(ctx, bytes.NewReader(data), "")
		if again != info {
			t.Fatal("same bytes, same hash")
		}
		secret, _ := w.A.PutBlob(ctx, strings.NewReader("unreferenced"), "")
		// Entry in a feed that is not shared: no one else can fetch.
		_, _ = w.A.Append(ctx, "private", map[string]string{"file": info.Hash})
		if _, err := w.B.GetBlob(ctx, info.Hash, sol.BlobOptions{}); !errors.Is(err, sol.ErrNotFound) {
			t.Fatalf("bob before share: %v", err)
		}
		_, _ = w.A.Append(ctx, "pub", map[string]any{"refs": []string{info.Hash}})
		if err := w.A.Share(ctx, sol.ShareRequest{Feed: "pub", People: []string{w.bobID}}); err != nil {
			t.Fatal(err)
		}
		st, err := w.B.StatBlob(ctx, info.Hash, sol.BlobOptions{})
		if err != nil || st.Size != 16 || st.Local {
			t.Fatalf("%+v %v", st, err)
		}
		rc, err := w.B.GetBlob(ctx, info.Hash, sol.BlobOptions{Offset: 4, Length: 6})
		if err != nil {
			t.Fatal(err)
		}
		part, _ := io.ReadAll(rc)
		rc.Close()
		if string(part) != "456789" {
			t.Fatalf("range: %q", part)
		}
		if st, _ := w.B.StatBlob(ctx, info.Hash, sol.BlobOptions{}); !st.Local {
			t.Fatal("blob should be cached after fetch")
		}
		rc, _ = w.B.GetBlob(ctx, info.Hash, sol.BlobOptions{Offset: 10})
		tail, _ := io.ReadAll(rc)
		rc.Close()
		if string(tail) != "abcdef" {
			t.Fatalf("open range: %q", tail)
		}
		// Unreferenced blob and other people: no.
		if _, err := w.B.GetBlob(ctx, secret.Hash, sol.BlobOptions{}); !errors.Is(err, sol.ErrNotFound) {
			t.Fatalf("bob unreferenced: %v", err)
		}
		if _, err := w.C.GetBlob(ctx, info.Hash, sol.BlobOptions{}); !errors.Is(err, sol.ErrNotFound) {
			t.Fatalf("carol: %v", err)
		}
		if _, err := w.C.StatBlob(ctx, info.Hash, sol.BlobOptions{}); !errors.Is(err, sol.ErrNotFound) {
			t.Fatalf("carol stat: %v", err)
		}
		// Unsharing does not take back a cached copy but stops new people. Pin works on local only.
		if err := w.B.Pin(ctx, []string{info.Hash}); err != nil {
			t.Fatal(err)
		}
		if !w.bob.Pinned(info.Hash) {
			t.Fatal("not pinned")
		}
		if err := w.B.Unpin(ctx, []string{info.Hash}); err != nil || w.bob.Pinned(info.Hash) {
			t.Fatal("unpin")
		}
		if err := w.C.Pin(ctx, []string{info.Hash}); !errors.Is(err, sol.ErrNotFound) {
			t.Fatalf("carol pin: %v", err)
		}
		// Offline holder: unavailable, not a permission error.
		w.alice.SetOnline(false)
		rc, err = w.A.GetBlob(ctx, info.Hash, sol.BlobOptions{})
		if err != nil {
			t.Fatal("owner reads own blob while offline:", err)
		}
		rc.Close()
		w.alice.SetOnline(true)
	})
}

func TestWriteShareCoAuthors(t *testing.T) {
	each(t, func(t *testing.T, w *world) {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		_, _ = w.A.Append(ctx, "tree", "alice-1")
		aliceEv, _ := w.A.Events(cctx, sol.EventOptions{})
		if err := w.A.Share(ctx, sol.ShareRequest{Feed: "tree", People: []string{w.bobID}, Mode: "write"}); err != nil {
			t.Fatal(err)
		}
		bobTree := sol.Address(w.bobID, "docs", "tree")
		if fe := wait(t, aliceEv); fe.Type != sol.EventFeed || fe.Feed.Feed != bobTree || !fe.Feed.Readable {
			t.Fatalf("co-author feed event: %+v", fe)
		}
		if _, err := w.B.Append(ctx, "tree", "bob-1"); err != nil {
			t.Fatal(err)
		}
		fs, _ := w.A.Feeds(ctx, w.bobID)
		if len(fs) != 1 || fs[0].Feed != bobTree {
			t.Fatalf("%+v", fs)
		}
		got, err := w.A.Read(ctx, bobTree, 0, 0)
		if err != nil || len(got) != 1 || string(got[0].Body) != `"bob-1"` {
			t.Fatalf("%+v %v", got, err)
		}
		// Bob reads Alice's and Alice's other feeds stay private.
		if got, _ := w.B.Read(ctx, sol.Address(w.aliceID, "docs", "tree"), 0, 0); len(got) != 1 {
			t.Fatal("bob should read alice's tree")
		}
		if _, err := w.C.Read(ctx, bobTree, 0, 0); !errors.Is(err, sol.ErrForbidden) {
			t.Fatal(err)
		}
	})
}

func TestActions(t *testing.T) {
	each(t, func(t *testing.T, w *world) {
		specs, err := w.A.Actions(ctx, "docs")
		if err != nil || len(specs) != 3 {
			t.Fatalf("%+v %v", specs, err)
		}
		res, err := w.A.CallAction(ctx, sol.ActionCall{Action: "docs.count", Args: json.RawMessage(`{"n":2}`)})
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Self  bool
			Who   string
			Agent string
		}
		_ = json.Unmarshal(res.Output, &out)
		if !out.Self || out.Who != w.aliceID {
			t.Fatalf("%s", res.Output)
		}
		// Bob runs it on Alice's app: not self.
		res, err = w.B.CallAction(ctx, sol.ActionCall{Action: "docs.count", Person: w.aliceID})
		_ = json.Unmarshal(res.Output, &out)
		if err != nil || out.Self || out.Who != w.bobID {
			t.Fatalf("%s %v", res.Output, err)
		}
		// Schema validation, dry run.
		if _, err := w.A.CallAction(ctx, sol.ActionCall{Action: "docs.rename", Args: json.RawMessage(`{}`)}); !errors.Is(err, sol.ErrBadRequest) {
			t.Fatalf("want bad_request, got %v", err)
		}
		res, _ = w.A.CallAction(ctx, sol.ActionCall{Action: "docs.rename", Args: json.RawMessage(`{"title":"x"}`), DryRun: true})
		if string(res.Output) != `{"dry":true}` {
			t.Fatalf("%s", res.Output)
		}
		if _, err := w.A.CallAction(ctx, sol.ActionCall{Action: "docs.nope"}); !errors.Is(err, sol.ErrNotFound) {
			t.Fatal(err)
		}
		if _, err := w.A.CallAction(ctx, sol.ActionCall{Action: "files.list"}); !errors.Is(err, sol.ErrNotFound) {
			t.Fatal(err)
		}
		// Unpaired people can't be targeted.
		if _, err := w.B.CallAction(ctx, sol.ActionCall{Action: "docs.count", Person: w.carol.ID}); !errors.Is(err, sol.ErrNotFound) {
			t.Fatal(err)
		}
		w.bob.SetOnline(false)
		if _, err := w.A.CallAction(ctx, sol.ActionCall{Action: "docs.count", Person: w.bobID}); !errors.Is(err, sol.ErrUnavailable) {
			t.Fatalf("offline: %v", err)
		}
		w.bob.SetOnline(true)
		// Activity: writes are logged (including the rejected one above), reads and dry runs are not.
		act, _ := w.A.Activity(ctx, 0)
		if len(act) != 1 || !strings.HasPrefix(act[0].Result, "error") {
			t.Fatalf("%+v", act)
		}
		if _, err := w.A.CallAction(ctx, sol.ActionCall{Action: "docs.rename", Args: json.RawMessage(`{"title":"x"}`)}); err != nil {
			t.Fatal(err)
		}
		act, _ = w.A.Activity(ctx, 0)
		if len(act) != 2 || act[1].Action != "docs.rename" || act[1].Result != "ok" || !strings.HasPrefix(act[1].ArgsDigest, "sha256:") {
			t.Fatalf("%+v", act)
		}
	})
}

func TestAgentApproval(t *testing.T) {
	n := memsol.New()
	p := n.AddPerson("Alice")
	m, _ := sol.ParseManifest([]byte(manifest))
	acts := sol.NewActions(m)
	acts.Handle("docs.wipe", func(context.Context, sol.Call) (any, error) { return map[string]string{"wiped": "yes"}, nil })
	p.Install(m, acts)
	c := p.Client("docs")
	// A human runs it straight away.
	if res, err := c.CallAction(ctx, sol.ActionCall{Action: "docs.wipe"}); err != nil || res.Approval != "" {
		t.Fatalf("%+v %v", res, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ev, _ := c.Events(cctx, sol.EventOptions{Inbox: true})
	agent := c.AsAgent("agent-1")
	res, err := agent.CallAction(ctx, sol.ActionCall{Action: "docs.wipe"})
	if err != nil || res.Approval == "" || res.Output != nil {
		t.Fatalf("%+v %v", res, err)
	}
	if e := wait(t, ev); e.Type != sol.EventInbox || e.Inbox.ID != res.Approval || e.Inbox.Answer != "" {
		t.Fatalf("%+v", e)
	}
	act, _ := c.Activity(ctx, 0)
	if len(act) != 1 { // only the human run so far
		t.Fatalf("%+v", act)
	}
	if err := p.Answer(res.Approval, true); err != nil {
		t.Fatal(err)
	}
	if e := wait(t, ev); e.Inbox.Answer != "approved" {
		t.Fatalf("%+v", e)
	}
	act, _ = c.Activity(ctx, 0)
	if len(act) != 2 || act[1].Agent != "agent-1" {
		t.Fatalf("%+v", act)
	}
	if err := p.Answer(res.Approval, true); !errors.Is(err, sol.ErrNotFound) {
		t.Fatalf("double answer: %v", err)
	}
}

func TestInboxNotice(t *testing.T) {
	each(t, func(t *testing.T, w *world) {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		ev, _ := w.B.Events(cctx, sol.EventOptions{Inbox: true})
		r, err := w.A.PostInbox(ctx, sol.InboxPost{Kind: "notice", Title: "Album added", To: w.bobID})
		if err != nil || r.ID == "" {
			t.Fatal(r, err)
		}
		if e := wait(t, ev); e.Inbox == nil || e.Inbox.Title != "Album added" {
			t.Fatalf("%+v", e)
		}
		if _, err := w.A.PostInbox(ctx, sol.InboxPost{Kind: "approval", Title: "x"}); !errors.Is(err, sol.ErrBadRequest) {
			t.Fatal(err)
		}
		if got := w.bob.Inbox(); len(got) != 1 || got[0].Kind != "notice" {
			t.Fatalf("%+v", got)
		}
	})
}

func TestDial(t *testing.T) {
	each(t, func(t *testing.T, w *world) {
		conn, err := w.B.Dial(ctx, sol.DialRequest{Person: w.aliceID, App: "docs", Path: "/stream"})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		buf := make([]byte, len("hello "+w.bobID[:4]+" /stream\n"))
		if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello "+w.bobID[:4]+" /stream\n" {
			t.Fatalf("%q %v", buf, err)
		}
		_, _ = conn.Write([]byte("ping"))
		echo := make([]byte, 4)
		if _, err := io.ReadFull(conn, echo); err != nil || string(echo) != "ping" {
			t.Fatalf("%q %v", echo, err)
		}
		if _, err := w.B.Dial(ctx, sol.DialRequest{Person: w.aliceID, App: "nope", Path: "/"}); !errors.Is(err, sol.ErrNotFound) {
			t.Fatal(err)
		}
		w.alice.SetOnline(false)
		if _, err := w.B.Dial(ctx, sol.DialRequest{Person: w.aliceID, App: "docs", Path: "/"}); !errors.Is(err, sol.ErrUnavailable) {
			t.Fatal(err)
		}
	})
}

func TestServerAuth(t *testing.T) {
	n := memsol.New()
	ts := httptest.NewServer(memsol.NewServer(n))
	defer ts.Close()
	c, _ := sol.New(ts.URL, "wrong")
	if _, err := c.WhoAmI(ctx); !errors.Is(err, sol.ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := c.Events(ctx, sol.EventOptions{}); !errors.Is(err, sol.ErrUnauthorized) {
		t.Fatal(err)
	}
}
