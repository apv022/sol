package memsol_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
	"github.com/apv022/sol/sdk/go/sol/memsol"
	"github.com/apv022/sol/sdk/go/sol/soltest"
)

const manifest = soltest.Manifest

// factory builds a soltest World on memsol. mode is "direct" (memsol clients) or "http" (the
// real sol.Client over Server).
func factory(mode string) soltest.Factory {
	return func(t *testing.T) *soltest.World {
		n := memsol.New()
		alice, bob, carol := n.AddPerson("Alice"), n.AddPerson("Bob"), n.AddPerson("Carol")
		alice2 := alice.AddDevice("phone", false)
		n.Pair(alice, bob)
		n.Pair(alice, carol)
		m := soltest.ParsedManifest()
		for _, p := range []*memsol.Person{alice, bob, carol} {
			p.Install(m, soltest.Actions())
			p.OnDial("docs", soltest.Dial)
		}
		w := &soltest.World{AliceID: alice.ID, BobID: bob.ID, CarolID: carol.ID, SDKHashes: true,
			SetOnline: func(id string, on bool) { n.Person(id).SetOnline(on) },
			Pinned:    func(id, h string) bool { return n.Person(id).Pinned(h) },
			Answer:    func(id, item string, ok bool) error { return n.Person(id).Answer(item, ok) },
			InboxKinds: func(id string) []string {
				var out []string
				for _, it := range n.Person(id).Inbox() {
					out = append(out, it.Kind)
				}
				return out
			}}
		cs := []*memsol.Client{alice.Client("docs"), alice2.Client("docs"), bob.Client("docs"), carol.Client("docs")}
		apis := make([]sol.API, len(cs))
		for i, c := range cs {
			apis[i] = c
		}
		if mode == "http" {
			ts := httptest.NewServer(memsol.NewServer(n))
			t.Cleanup(ts.Close)
			for i, c := range cs {
				cl, err := sol.New(ts.URL, c.Token())
				if err != nil {
					t.Fatal(err)
				}
				apis[i] = cl
			}
		}
		w.A, w.A2, w.B, w.C = apis[0], apis[1], apis[2], apis[3]
		return w
	}
}

func TestConformance(t *testing.T) {
	for _, mode := range []string{"direct", "http"} {
		t.Run(mode, func(t *testing.T) { soltest.Run(t, factory(mode)) })
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

var ctx = context.Background()

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

// TestReleaseFreesBlob: a release drops the releaser's copy unless it is pinned or another feed
// of theirs still references it; copies other people fetched stay theirs.
func TestReleaseFreesBlob(t *testing.T) {
	ctx := context.Background()
	n := memsol.New()
	alice, bob := n.AddPerson("Alice"), n.AddPerson("Bob")
	n.Pair(alice, bob)
	a, b := alice.Devices[0].Client("docs"), bob.Devices[0].Client("docs")
	put := func(s string) string {
		info, err := a.PutBlob(ctx, strings.NewReader(s), "")
		if err != nil {
			t.Fatal(err)
		}
		return info.Hash
	}
	gone, other, pinned := put("gone"), put("other"), put("pinned")
	for _, h := range []string{gone, other, pinned} {
		if _, err := a.Append(ctx, "f", map[string]string{"blob": h}); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = a.Append(ctx, "g", map[string]string{"blob": other})
	_ = a.Share(ctx, sol.ShareRequest{Feed: "f", People: []string{bob.ID}})
	rc, err := b.GetBlob(ctx, gone, sol.BlobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	_ = a.Pin(ctx, []string{pinned})
	if _, err := a.Append(ctx, "f", sol.Release{Release: []string{gone, other, pinned}}); err != nil {
		t.Fatal(err)
	}
	if n.HasBlob(alice.ID, gone) || !n.HasBlob(alice.ID, other) || !n.HasBlob(alice.ID, pinned) {
		t.Fatal("release freed the wrong blobs")
	}
	if !n.HasBlob(bob.ID, gone) {
		t.Fatal("Bob's fetched copy was dropped")
	}
}
