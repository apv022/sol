package session

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/faultnet"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/transport"
)

// renamed is a clearnet transport under another name, so tests can give a device several
// paths ("wg", "tor") that each run through a faultnet proxy.
type renamed struct {
	transport.Transport
	name string
}

func (r renamed) Name() string { return r.name }

// pathNode is a device reachable over two paths, "wg" (preferred) and "tor", each a
// faultnet proxy in front of its listener.
type pathNode struct {
	*node
	wg, tor *faultnet.TCPProxy
}

func newPathNode(t *testing.T, name string) *pathNode {
	t.Helper()
	kr, err := identity.Create(t.TempDir(), name, name+"-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pn := &pathNode{}
	for _, p := range []**faultnet.TCPProxy{&pn.wg, &pn.tor} {
		if *p, err = faultnet.NewTCPProxy(ln.Addr().String()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup((*p).Close)
	}
	if _, err := kr.SetEndpoints([]identity.Endpoint{{Transport: "tor", Address: pn.tor.Addr()},
		{Transport: "direct", Address: pn.wg.Addr()}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	cl := transport.NewClearnet("127.0.0.1:0", "")
	cl.Start(context.Background())
	n := &node{kr: kr, contacts: NewMemContacts(), tokens: pairing.NewTokens()}
	n.m, err = New(Config{Keyring: kr, Contacts: n.contacts, Tokens: n.tokens,
		Transports: []transport.Transport{renamed{cl, "direct"}, renamed{cl, "tor"}},
		Handler:    http.Handler(testHandler()), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	go n.m.Serve(ln)
	t.Cleanup(func() { n.m.Close(); cl.Stop() })
	pn.node = n
	return pn
}

// dialedSession returns the session between a and b, from the side that dialed it.
func dialedSession(a, b *pathNode) (*Session, bool) {
	if s, ok := a.m.Session(b.kr.DevicePublic()); ok && bytes.Equal(s.Caller(), a.kr.DevicePublic()) {
		return s, true
	}
	if s, ok := b.m.Session(a.kr.DevicePublic()); ok && bytes.Equal(s.Caller(), b.kr.DevicePublic()) {
		return s, true
	}
	return nil, false
}

func pairPathNodes(t *testing.T, a, b *pathNode) {
	t.Helper()
	pairNodes(t, a.node, b.node)
}

// TestFallbackAndReturn: with the preferred path refused, the session comes up over the
// fallback and says why; once the preferred path works again, the session moves back, but
// only after the probation (StableProbes probes over StableFor).
func TestFallbackAndReturn(t *testing.T) {
	alice, bob := newPathNode(t, "alice"), newPathNode(t, "bob")
	pairPathNodes(t, alice, bob)
	s, ok := dialedSession(alice, bob)
	if !ok || s.Path().Transport != "direct" || s.Path().Reason != "best path" {
		t.Fatalf("first session: %v %+v", ok, s.Path())
	}

	for _, n := range []*pathNode{alice, bob} {
		n.wg.Refuse(true)
		n.wg.ResetAll()
	}
	var fb *Session
	eventually(t, "a session over tor", func() bool {
		fb, ok = dialedSession(alice, bob)
		return ok && fb.Path().Transport == "tor"
	})
	// "direct failed" when this dial tried direct first; "best path" when an earlier attempt
	// had already marked direct as failing, so tor ranked first (seen on slow Windows runners).
	if r := fb.Path().Reason; !strings.Contains(r, "direct failed") && r != "best path" {
		t.Fatalf("fallback reason: %q", r)
	}
	caller, callee := alice, bob
	if bytes.Equal(fb.Caller(), bob.kr.DevicePublic()) {
		caller, callee = bob, alice
	}
	if ps := caller.m.Paths(callee.kr.DevicePublic()); len(ps) != 1 || ps[0].Transport != "direct" || ps[0].State != "backoff" {
		t.Fatalf("path states: %+v", ps)
	}

	healed := time.Now()
	for _, n := range []*pathNode{alice, bob} {
		n.wg.Refuse(false)
	}
	var back *Session
	eventually(t, "the session back on wg", func() bool {
		back, ok = dialedSession(alice, bob)
		return ok && back.Path().Transport == "direct"
	})
	if d := time.Since(healed); d < StableFor {
		t.Fatalf("returned after %v, before the probation of %v", d, StableFor)
	}
	if back.Path().Reason != "direct is stable again" {
		t.Fatalf("return reason: %q", back.Path().Reason)
	}
	if ps := caller.m.Paths(callee.kr.DevicePublic()); len(ps) != 0 {
		t.Fatalf("path states after return: %+v", ps)
	}
	// Both sides agree on the one session, and it serves.
	eventually(t, "the old session closed", func() bool { return !fb.Alive() })
	if got := get(t, back, "/whoami"); got == "" {
		t.Fatal("moved session does not serve")
	}
}

// TestHedgeBeatsSlowPath: a preferred path that is slow loses to the next one, started
// HedgeDelay later, long before the slow one would have answered.
func TestHedgeBeatsSlowPath(t *testing.T) {
	alice, bob := newPathNode(t, "alice"), newPathNode(t, "bob")
	introduce(t, alice.node, bob.node)
	alice.wg.SetLatency(2 * time.Second)
	t0 := time.Now()
	s, err := bob.m.Connect(context.Background(), alice.kr.Person(), alice.kr.DevicePublic())
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("connecting took %v", d)
	}
	if p := s.Path(); p.Transport != "tor" || !strings.Contains(p.Reason, "direct was slower") {
		t.Fatalf("path: %+v", p)
	}
	if ps := bob.m.Paths(alice.kr.DevicePublic()); len(ps) != 1 || ps[0].Transport != "direct" {
		t.Fatalf("path states: %+v", ps)
	}
	// The abandoned attempt never became a second session on Alice's side.
	time.Sleep(3 * time.Second)
	if sa, ok := alice.m.Session(bob.kr.DevicePublic()); !ok || !s.Alive() || !sa.Alive() {
		t.Fatal("the hedged session did not survive the slow attempt")
	}
}

// TestNetworkChangeRedials: a path that went silent (no FIN, no RST) keeps its session
// until the mux misses pongs; a network change checks every session at once, closes the
// silent one, and the keeper redials over a path that works.
func TestNetworkChangeRedials(t *testing.T) {
	alice, bob := newPathNode(t, "alice"), newPathNode(t, "bob")
	pairPathNodes(t, alice, bob)
	s, ok := dialedSession(alice, bob)
	if !ok || s.Path().Transport != "direct" {
		t.Fatal("no session over wg")
	}
	alice.wg.Blackhole(true)
	bob.wg.Blackhole(true)
	caller := bob // bob redeemed the pair link
	t0 := time.Now()
	caller.m.NetworkChanged()
	var ns *Session
	eventually(t, "a new session over tor", func() bool {
		ns, ok = dialedSession(alice, bob)
		return ok && ns != s && ns.Path().Transport == "tor"
	})
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("redial took %v", d)
	}
	if s.Alive() {
		t.Fatal("the silent session is still up")
	}
}

// TestRemovalCutsEverywhere: removing a device closes the sessions every contact holds with
// it within a second, through the pushed device list, and my other devices learn my
// contacts' bundles (and so reach them) through the relay.
func TestRemovalCutsEverywhere(t *testing.T) {
	alice, bob := person(t, "alice"), person(t, "bob")
	pairNodes(t, alice, bob)
	k2, err := identity.NewDevice(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	phone := newNode(t, k2)
	phone.contacts.AddContact(bob.kr.Person(), "bob") // replicated from the laptop in a real daemon
	l, err := alice.m.Invite(pairing.KindJoin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := phone.m.Join(context.Background(), l, "phone"); err != nil {
		t.Fatal(err)
	}
	// Bob learns the new list by push, the phone learns Bob by relay and dials him.
	eventually(t, "bob's session with the phone", func() bool {
		_, ok := bob.m.Session(k2.DevicePublic())
		return ok
	})
	bs, _ := bob.m.Session(k2.DevicePublic())

	t0 := time.Now()
	if _, err := alice.m.RemoveDevice(k2.DevicePublic()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bs.Done():
	case <-time.After(time.Second):
		t.Fatal("bob's session with the removed device outlived the push by a second")
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("cut after %v", d)
	}
	_, dl, _ := bob.m.cache.Latest(alice.kr.Person())
	if _, in := dl.Find(k2.DevicePublic()); in {
		t.Fatal("bob still lists the removed device")
	}
	time.Sleep(5 * RedialMax)
	if _, ok := bob.m.Session(k2.DevicePublic()); ok {
		t.Fatal("the removed device got a session back")
	}
}

// TestContactEndpointsReachMyOtherDevice: a contact's new endpoint record, pushed to my
// laptop, reaches my phone through the relay, and the phone reaches the contact there.
func TestContactEndpointsReachMyOtherDevice(t *testing.T) {
	alice, bob := person(t, "alice"), person(t, "bob")
	pairNodes(t, alice, bob)
	k2, err := identity.NewDevice(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	phone := newNode(t, k2)
	phone.contacts.AddContact(bob.kr.Person(), "bob") // replicated from the laptop in a real daemon
	l, err := alice.m.Invite(pairing.KindJoin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := phone.m.Join(context.Background(), l, "phone"); err != nil {
		t.Fatal(err)
	}
	// The relay at join told the phone where Bob is.
	eventually(t, "the phone's session with bob", func() bool {
		_, ok := phone.m.Session(bob.kr.DevicePublic())
		return ok
	})
	// They part ways for now, so what the phone learns next can only come through the laptop.
	phone.m.Forget(bob.kr.DevicePublic())
	bob.m.Forget(k2.DevicePublic())

	// Bob moves: a new address (a proxy in front of his listener).
	old := mustBundle(t, bob.kr).Endpoints.Endpoints[0].Address
	px, err := faultnet.NewTCPProxy(old)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(px.Close)
	rec, err := bob.kr.SetEndpoints([]identity.Endpoint{{Transport: "clearnet", Address: px.Addr()}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	bob.m.BundleChanged()
	eventually(t, "bob's new record on the phone", func() bool {
		r, ok := phone.m.endpointsFor(bob.kr.Person(), bob.kr.DevicePublic())
		return ok && r.Seq == rec.Seq
	})
	eventually(t, "the phone's session with bob at his new address", func() bool {
		s, ok := phone.m.Session(bob.kr.DevicePublic())
		return ok && s.pathKey == "clearnet|"+px.Addr()
	})
}
