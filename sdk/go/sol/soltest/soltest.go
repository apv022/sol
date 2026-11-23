// Package soltest is the conformance suite of the Sol app API (design/API.md): the scenarios
// every implementation must pass, written once against sol.API. memsol runs it in memory and
// over HTTP; the real daemon runs it over a network of real daemons (internal/e2e).
//
// A Factory builds a fresh World for each scenario: Alice with two devices, Bob and Carol,
// Alice paired with both, Bob and Carol not paired, and every person running the "docs" app of
// Manifest (actions from Handler, dialed streams from Dial). The scenarios allow for real
// replication: they wait for what travels between devices instead of expecting it at once.
// Checks that need a control a World may not have (taking a person offline, looking into an
// inbox) are skipped when the hook is nil.
package soltest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

// Manifest is the app.json of the docs app every person of a World runs.
const Manifest = `{"id":"docs","version":"1","exec":"docs","scopes":["feeds","blobs","people","dial","actions","inbox"],
 "actions":[
  {"name":"docs.count","effect":"read","input":{"type":"object","properties":{"n":{"type":"integer"}}}},
  {"name":"docs.rename","effect":"write","input":{"type":"object","required":["title"],"properties":{"title":{"type":"string"}}}},
  {"name":"docs.wipe","effect":"destructive","approval":"agents"}]}`

// ParsedManifest returns Manifest parsed (it panics on a bad constant, a build error).
func ParsedManifest() *sol.Manifest {
	m, err := sol.ParseManifest([]byte(Manifest))
	if err != nil {
		panic(err)
	}
	return m
}

// Actions returns the action router of the docs app.
func Actions() *sol.Actions {
	acts := sol.NewActions(ParsedManifest())
	acts.Handle("docs.count", func(_ context.Context, c sol.Call) (any, error) {
		return map[string]any{"self": c.Caller.Self, "who": c.Caller.Person, "agent": c.Caller.Agent}, nil
	})
	acts.Handle("docs.rename", func(_ context.Context, c sol.Call) (any, error) {
		return map[string]any{"dry": c.DryRun}, nil
	})
	acts.Handle("docs.wipe", func(context.Context, sol.Call) (any, error) { return map[string]string{"wiped": "yes"}, nil })
	return acts
}

// Dial serves one dialed stream of the docs app: a greeting naming the caller and the path,
// then an echo.
func Dial(conn net.Conn, c sol.Caller, path string) {
	defer conn.Close()
	_, _ = io.WriteString(conn, "hello "+c.Person[:4]+" "+path+"\n")
	_, _ = io.Copy(conn, conn)
}

// Handler is the whole docs app as served on its socket: actions under /_sol/actions/ and
// dialed streams (upgrade requests) at any other path.
func Handler() http.Handler {
	acts := Actions()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sol.IsStream(r) {
			conn, c, err := sol.AcceptStream(w, r)
			if err == nil {
				Dial(conn, c, r.URL.Path)
			}
			return
		}
		acts.ServeHTTP(w, r)
	})
}

// World is one fresh network for a scenario.
type World struct {
	// A and A2 are Alice's two devices, B is Bob, C is Carol, each as its docs app sees the API.
	A, A2, B, C sol.API
	// AliceID, BobID, CarolID are the person IDs.
	AliceID, BobID, CarolID string

	// SDKHashes is true when entry hashes are sol.Entry.ComputeHash (memsol). A real daemon's
	// hashes also cover the device signature, so only the chain links are checked.
	SDKHashes bool
	// SetOnline takes a person offline or back online (nil: offline checks are skipped).
	SetOnline func(person string, on bool)
	// SetDeviceOnline takes one device (A, A2, B or C) off the network or back, while the
	// person's other devices stay on (nil: the per-device offline checks are skipped).
	SetDeviceOnline func(device sol.API, on bool)
	// Pinned reports whether person pinned a blob (nil: skipped).
	Pinned func(person, hash string) bool
	// InboxKinds lists the kinds of a person's inbox items (nil: skipped).
	InboxKinds func(person string) []string
	// Answer approves or denies an approval item as the person would (nil: skipped).
	Answer func(person, id string, approve bool) error
}

// Factory builds a fresh World.
type Factory func(t *testing.T) *World

// Run runs every scenario as a subtest, each in a fresh World.
func Run(t *testing.T, f Factory) {
	for _, s := range []struct {
		name string
		f    func(*testing.T, *World)
	}{
		{"Identity", Identity},
		{"ShareReadAndEvents", ShareReadAndEvents},
		{"BodyLimitAndNames", BodyLimitAndNames},
		{"BlobPermissions", BlobPermissions},
		{"WriteShareCoAuthors", WriteShareCoAuthors},
		{"Actions", ActionsScenario},
		{"InboxNotice", InboxNotice},
		{"InboxApproval", InboxApproval},
		{"Dial", DialScenario},
		{"OfflineConverge", OfflineConverge},
	} {
		t.Run(s.name, func(t *testing.T) { s.f(t, f(t)) })
	}
}

var ctx = context.Background()

// Timeout bounds every wait of the suite.
var Timeout = 15 * time.Second

func wait(t *testing.T, ch <-chan sol.Event) sol.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed")
		}
		return ev
	case <-time.After(Timeout):
		t.Fatal("timed out waiting for event")
	}
	return sol.Event{}
}

// eventually retries cond (a test may poll; the product never does).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(Timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readN(t *testing.T, api sol.API, addr string, n int) []sol.Entry {
	t.Helper()
	var got []sol.Entry
	eventually(t, "entries of "+addr, func() bool {
		var err error
		got, err = api.Read(ctx, addr, 0, 0)
		return err == nil && len(got) >= n
	})
	return got
}

// checkChain checks hashes (when computable) and the per-device seq/prev links.
func checkChain(w *World, es []sol.Entry) error {
	if w.SDKHashes {
		return sol.VerifyChain(es)
	}
	last := map[string]sol.Entry{}
	for _, e := range es {
		if !sol.ValidHash(e.Hash) {
			return errors.New("bad hash " + e.Hash)
		}
		p, seen := last[e.Device]
		if seen && (e.Seq != p.Seq+1 || e.Prev != p.Hash) || !seen && e.Seq == 1 && e.Prev != "" {
			return errors.New("broken chain")
		}
		last[e.Device] = e
	}
	return nil
}

// Identity: whoami, people (with devices) and my devices.
func Identity(t *testing.T, w *World) {
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
}

// ShareReadAndEvents: sharing a feed, reading it, live entries from two devices of the owner,
// Last-Event-ID resume, and unsharing.
func ShareReadAndEvents(t *testing.T, w *World) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	addr := sol.Address(w.AliceID, "docs", "tree")
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
	if _, err := w.A.Read(ctx, sol.Address(w.AliceID, "files", "x"), 0, 0); !errors.Is(err, sol.ErrForbidden) {
		t.Fatalf("want forbidden, got %v", err)
	}
	if err := w.A.Share(ctx, sol.ShareRequest{Feed: "tree", People: []string{w.BobID}}); err != nil {
		t.Fatal(err)
	}
	if fe := wait(t, bobEv); fe.Type != sol.EventFeed || !fe.Feed.Readable || fe.Feed.Feed != addr {
		t.Fatalf("%+v", fe)
	}
	if pe := wait(t, bobEv); pe.Type != sol.EventPerson || len(pe.Person.SharesWithMe) != 1 || pe.Person.SharesWithMe[0] != "docs/tree" {
		t.Fatalf("%+v %+v", pe, pe.Person)
	}
	shares, _ := w.A.Shares(ctx, "tree")
	if len(shares) != 1 || shares[0].Person != w.BobID || shares[0].Mode != "read" {
		t.Fatalf("%+v", shares)
	}
	got := readN(t, w.B, addr, 1)
	if len(got) != 1 || string(got[0].Body) != `{"n":1}` {
		t.Fatalf("%+v", got)
	}
	fs, _ := w.B.Feeds(ctx, w.AliceID)
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
	readN(t, w.A2, addr, 1)
	e2, _ := w.A2.Append(ctx, "tree", "from phone")
	readN(t, w.A, addr, 2)
	e3, _ := w.A.Append(ctx, "tree", map[string]any{"n": 3})
	if e2.Seq != 1 || e2.Pos != 2 || e2.Prev != "" || e3.Seq != 2 || e3.Pos != 3 || e3.Prev != e1.Hash || e2.Device == e1.Device {
		t.Fatalf("chains: %+v %+v %+v", e1, e2, e3)
	}
	first := wait(t, live)
	if first.Type != sol.EventEntry || first.Entry.Pos != 2 || first.Entry.Hash != e2.Hash {
		t.Fatalf("%+v", first)
	}
	second := wait(t, live)
	if second.Entry == nil || second.Entry.Pos != 3 {
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
	if err := checkChain(w, all); err != nil || len(all) != 3 {
		t.Fatalf("%v %d", err, len(all))
	}
	// Carol sees nothing of this.
	if _, err := w.C.Read(ctx, addr, 0, 0); !errors.Is(err, sol.ErrForbidden) {
		t.Fatal(err)
	}

	// Unshare: Bob loses access and is told.
	if err := w.A.Unshare(ctx, sol.ShareRequest{Feed: "tree", People: []string{w.BobID}}); err != nil {
		t.Fatal(err)
	}
	fe := wait(t, bobEv)
	// Skip entries Bob's wide subscription got while shared, and presence news (a contact
	// coming online can land in between on a slow machine): the feed event must follow.
	for fe.Type == sol.EventEntry || fe.Type == sol.EventPerson {
		fe = wait(t, bobEv)
	}
	if fe.Type != sol.EventFeed || fe.Feed.Readable {
		t.Fatalf("%+v", fe)
	}
	if _, err := w.B.Read(ctx, addr, 0, 0); !errors.Is(err, sol.ErrForbidden) {
		t.Fatal(err)
	}
}

// BodyLimitAndNames: the 64 KiB body limit, feed names, unknown people.
func BodyLimitAndNames(t *testing.T, w *World) {
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
}

// BlobPermissions: the API.md §4 rule, ranges, caching and pins.
func BlobPermissions(t *testing.T, w *World) {
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
	if err := w.A.Share(ctx, sol.ShareRequest{Feed: "pub", People: []string{w.BobID}}); err != nil {
		t.Fatal(err)
	}
	var st sol.BlobStat
	eventually(t, "the blob to be fetchable", func() bool {
		st, err = w.B.StatBlob(ctx, info.Hash, sol.BlobOptions{})
		return err == nil
	})
	if st.Size != 16 || st.Local {
		t.Fatalf("%+v", st)
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
	eventually(t, "the blob to be cached after a fetch", func() bool {
		st, _ := w.B.StatBlob(ctx, info.Hash, sol.BlobOptions{})
		return st.Local
	})
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
	// Pin works on local blobs only.
	if err := w.B.Pin(ctx, []string{info.Hash}); err != nil {
		t.Fatal(err)
	}
	if w.Pinned != nil && !w.Pinned(w.BobID, info.Hash) {
		t.Fatal("not pinned")
	}
	if err := w.B.Unpin(ctx, []string{info.Hash}); err != nil || (w.Pinned != nil && w.Pinned(w.BobID, info.Hash)) {
		t.Fatal("unpin")
	}
	if err := w.C.Pin(ctx, []string{info.Hash}); !errors.Is(err, sol.ErrNotFound) {
		t.Fatalf("carol pin: %v", err)
	}
	if w.SetOnline != nil {
		// The owner reads its own blob while offline.
		w.SetOnline(w.AliceID, false)
		rc, err = w.A.GetBlob(ctx, info.Hash, sol.BlobOptions{})
		if err != nil {
			t.Fatal("owner reads own blob while offline:", err)
		}
		rc.Close()
		w.SetOnline(w.AliceID, true)
	}
}

// WriteShareCoAuthors: after a write share, the other person's same-named feed becomes a
// co-author feed once their app shares it back; the write share alone grants nothing.
func WriteShareCoAuthors(t *testing.T, w *World) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, _ = w.A.Append(ctx, "tree", "alice-1")
	if err := w.A.Share(ctx, sol.ShareRequest{Feed: "tree", People: []string{w.BobID}, Mode: "write"}); err != nil {
		t.Fatal(err)
	}
	bobTree := sol.Address(w.BobID, "docs", "tree")
	if _, err := w.B.Append(ctx, "tree", "bob-private"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // replication is pushed; give a leak time to show
	if es, err := w.A.Read(ctx, bobTree, 0, 0); err == nil && len(es) > 0 {
		t.Fatalf("a write share alone exposed the recipient's same-named feed: %+v", es)
	}
	aliceEv, _ := w.A.Events(cctx, sol.EventOptions{})
	if err := w.B.Share(ctx, sol.ShareRequest{Feed: "tree", People: []string{w.AliceID}}); err != nil {
		t.Fatal(err)
	}
	if fe := wait(t, aliceEv); fe.Type != sol.EventFeed || fe.Feed.Feed != bobTree || !fe.Feed.Readable {
		t.Fatalf("co-author feed event: %+v", fe)
	}
	if _, err := w.B.Append(ctx, "tree", "bob-1"); err != nil {
		t.Fatal(err)
	}
	got := readN(t, w.A, bobTree, 2)
	if len(got) != 2 || string(got[1].Body) != `"bob-1"` {
		t.Fatalf("%+v", got)
	}
	fs, _ := w.A.Feeds(ctx, w.BobID)
	if len(fs) != 1 || fs[0].Feed != bobTree {
		t.Fatalf("%+v", fs)
	}
	// Bob reads Alice's and Alice's other feeds stay private.
	readN(t, w.B, sol.Address(w.AliceID, "docs", "tree"), 1)
	if _, err := w.C.Read(ctx, bobTree, 0, 0); !errors.Is(err, sol.ErrForbidden) {
		t.Fatal(err)
	}
}

// ActionsScenario: listing, calling mine and a peer's, validation, dry runs, the activity log.
func ActionsScenario(t *testing.T, w *World) {
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
	if !out.Self || out.Who != w.AliceID {
		t.Fatalf("%s", res.Output)
	}
	// Bob runs it on Alice's app: not self.
	res, err = w.B.CallAction(ctx, sol.ActionCall{Action: "docs.count", Person: w.AliceID})
	_ = json.Unmarshal(res.Output, &out)
	if err != nil || out.Self || out.Who != w.BobID {
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
	if _, err := w.B.CallAction(ctx, sol.ActionCall{Action: "docs.count", Person: w.CarolID}); !errors.Is(err, sol.ErrNotFound) {
		t.Fatal(err)
	}
	if w.SetOnline != nil {
		w.SetOnline(w.BobID, false)
		if _, err := w.A.CallAction(ctx, sol.ActionCall{Action: "docs.count", Person: w.BobID}); !errors.Is(err, sol.ErrUnavailable) {
			t.Fatalf("offline: %v", err)
		}
		w.SetOnline(w.BobID, true)
	}
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
}

// InboxNotice: a notice to another person reaches their app's inbox events.
func InboxNotice(t *testing.T, w *World) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ev, _ := w.B.Events(cctx, sol.EventOptions{Inbox: true})
	r, err := w.A.PostInbox(ctx, sol.InboxPost{Kind: "notice", Title: "Album added", To: w.BobID})
	if err != nil || r.ID == "" {
		t.Fatal(r, err)
	}
	if e := wait(t, ev); e.Inbox == nil || e.Inbox.Title != "Album added" {
		t.Fatalf("%+v", e)
	}
	if _, err := w.A.PostInbox(ctx, sol.InboxPost{Kind: "approval", Title: "x"}); !errors.Is(err, sol.ErrBadRequest) {
		t.Fatal(err)
	}
	if w.InboxKinds != nil {
		if got := w.InboxKinds(w.BobID); len(got) != 1 || got[0] != "notice" {
			t.Fatalf("%+v", got)
		}
	}
}

// InboxApproval: an app files an approval; Inbox lists it open; once the person approves, the
// stored action runs as the app and the item carries the answer and the result, with an inbox
// event for the app.
func InboxApproval(t *testing.T, w *World) {
	items, err := w.A.Inbox(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("fresh inbox: %+v %v", items, err)
	}
	r, err := w.A.PostInbox(ctx, sol.InboxPost{Kind: "approval", Title: "Rename?",
		Action: &sol.ActionCall{Action: "docs.rename", Args: json.RawMessage(`{"title":"y"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	items, err = w.A.Inbox(ctx)
	if err != nil || len(items) != 1 || items[0].ID != r.ID || items[0].Kind != "approval" || items[0].Answer != "" ||
		items[0].Action == nil || items[0].Action.Action != "docs.rename" || items[0].App != "docs" {
		t.Fatalf("open approval: %+v %v", items, err)
	}
	if other, _ := w.B.Inbox(ctx); len(other) != 0 {
		t.Fatalf("another person sees my inbox: %+v", other)
	}
	if w.Answer == nil {
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ev, err := w.A.Events(cctx, sol.EventOptions{Inbox: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Answer(w.AliceID, r.ID, true); err != nil {
		t.Fatal(err)
	}
	if e := wait(t, ev); e.Inbox == nil || e.Inbox.ID != r.ID || e.Inbox.Answer != "approved" {
		t.Fatalf("%+v", e)
	}
	items, _ = w.A.Inbox(ctx)
	if len(items) != 1 || items[0].Answer != "approved" || string(items[0].Result) != `{"dry":false}` || items[0].Error != nil {
		t.Fatalf("answered: %+v", items)
	}
	if err := w.Answer(w.AliceID, r.ID, false); !errors.Is(err, sol.ErrNotFound) {
		t.Fatalf("answering twice: %v", err)
	}
}

// DialScenario: a live byte stream to a peer's app.
func DialScenario(t *testing.T, w *World) {
	conn, err := w.B.Dial(ctx, sol.DialRequest{Person: w.AliceID, App: "docs", Path: "/stream"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	want := "hello " + w.BobID[:4] + " /stream\n"
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != want {
		t.Fatalf("%q %v", buf, err)
	}
	_, _ = conn.Write([]byte("ping"))
	echo := make([]byte, 4)
	if _, err := io.ReadFull(conn, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("%q %v", echo, err)
	}
	if _, err := w.B.Dial(ctx, sol.DialRequest{Person: w.AliceID, App: "nope", Path: "/"}); !errors.Is(err, sol.ErrNotFound) {
		t.Fatal(err)
	}
	if w.SetOnline != nil {
		w.SetOnline(w.AliceID, false)
		if _, err := w.B.Dial(ctx, sol.DialRequest{Person: w.AliceID, App: "docs", Path: "/"}); !errors.Is(err, sol.ErrUnavailable) {
			t.Fatal(err)
		}
	}
}

// hashes is the sorted set of entry hashes in es.
func hashes(es []sol.Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Hash)
	}
	slices.Sort(out)
	return out
}

// OfflineConverge: devices that were offline catch up when they come back. A contact who was
// offline gets what was written meanwhile; a device of mine that wrote while offline, as my
// other device did, ends with the same entries as that device and the contact, each device's
// chain intact.
func OfflineConverge(t *testing.T, w *World) {
	if w.SetOnline == nil && w.SetDeviceOnline == nil {
		t.Skip("this World cannot take anyone offline")
	}
	addr := sol.Address(w.AliceID, "docs", "log")
	if _, err := w.A.Append(ctx, "log", "a-1"); err != nil {
		t.Fatal(err)
	}
	if err := w.A.Share(ctx, sol.ShareRequest{Feed: "log", People: []string{w.BobID}}); err != nil {
		t.Fatal(err)
	}
	readN(t, w.B, addr, 1)
	readN(t, w.A2, addr, 1)
	n := 1

	if w.SetOnline != nil {
		// Bob is away while Alice writes; what he has stays readable.
		w.SetOnline(w.BobID, false)
		for _, b := range []string{"a-2", "a-3"} {
			if _, err := w.A.Append(ctx, "log", b); err != nil {
				t.Fatal(err)
			}
		}
		n += 2
		if _, err := w.B.Read(ctx, addr, 0, 0); err != nil {
			t.Fatal("offline read of what was replicated:", err)
		}
		w.SetOnline(w.BobID, true)
		readN(t, w.B, addr, n)
	}

	if w.SetDeviceOnline != nil {
		// Alice's phone is offline: both her devices write; then it comes back.
		w.SetDeviceOnline(w.A2, false)
		if _, err := w.A.Append(ctx, "log", "laptop while phone away"); err != nil {
			t.Fatal(err)
		}
		if _, err := w.A2.Append(ctx, "log", "phone offline 1"); err != nil {
			t.Fatal(err)
		}
		if _, err := w.A2.Append(ctx, "log", "phone offline 2"); err != nil {
			t.Fatal(err)
		}
		n += 3
		w.SetDeviceOnline(w.A2, true)
	}

	a, a2, b := readN(t, w.A, addr, n), readN(t, w.A2, addr, n), readN(t, w.B, addr, n)
	if !slices.Equal(hashes(a), hashes(a2)) || !slices.Equal(hashes(a), hashes(b)) || len(a) != n {
		t.Fatalf("not converged: %d %d %d entries, want %d", len(a), len(a2), len(b), n)
	}
	for _, es := range [][]sol.Entry{a, a2, b} {
		if err := checkChain(w, es); err != nil {
			t.Fatal(err)
		}
	}
}
