package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/blobnet"
	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/replicate"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/internal/transport"
)

func TestMain(m *testing.M) {
	session.RedialMin = 20 * time.Millisecond
	session.RedialMax = 200 * time.Millisecond
	replicate.RetryMin = 20 * time.Millisecond
	replicate.RetryMax = 200 * time.Millisecond
	m.Run()
}

// daemon is one device: keyring, feed store with the contacts, shares,
// pins and blob-reference views, a blob store, a replicator, a blob service,
// and a session manager publishing over clearnet.
type daemon struct {
	t       *testing.T
	kr      *identity.Keyring
	dir     string // feed store, kept across restarts
	blobDir string // blob store, kept across restarts
	tokens  *pairing.Tokens
	cache   *identity.Cache

	store    *feeds.Store
	contacts *feeds.Contacts
	shares   *feeds.Shares
	pins     *feeds.Pins
	refs     *feeds.BlobRefs
	blobs    *blob.Store
	rep      *replicate.Replicator
	bn       *blobnet.Service
	cl       *transport.Clearnet
	m        *session.Manager
	addr     string // public clearnet address, stable across restarts

	// evil, when set, serves /p/sync instead of the replicator.
	evil atomic.Pointer[http.HandlerFunc]
	// blobGate, when set, runs before every /p/blobs/ request is served;
	// false drops the request unanswered.
	blobGate atomic.Pointer[func(*http.Request) bool]
}

func whoami(w http.ResponseWriter, r *http.Request) {
	p, _ := session.PeerFrom(r.Context())
	fmt.Fprintf(w, "%s %s", p.Relation, identity.ID(p.Device))
}

func newDaemon(t *testing.T, kr *identity.Keyring) *daemon {
	d := &daemon{t: t, kr: kr, dir: t.TempDir(), blobDir: t.TempDir(), tokens: pairing.NewTokens()}
	d.start("127.0.0.1:0")
	if _, err := kr.SetEndpoints([]identity.Endpoint{{Transport: "clearnet", Address: d.addr}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.stop)
	return d
}

// start opens the store (if closed) and brings the network up.
func (d *daemon) start(listen string) {
	t := d.t
	var err error
	if d.store == nil {
		d.cache = identity.NewCache() // in memory: a restart forgets it
		d.store, err = feeds.Open(feeds.Config{Dir: d.dir, Self: d.kr, Lists: feeds.IdentityLists(d.kr, d.cache),
			Log: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		d.contacts = feeds.NewContacts(d.store)
		if d.shares, err = feeds.NewShares(d.store, d.contacts); err != nil {
			t.Fatal(err)
		}
		d.pins = feeds.NewPins(d.store)
		if d.refs, err = feeds.NewBlobRefs(d.store, d.shares); err != nil {
			t.Fatal(err)
		}
		if d.blobs, err = blob.New(d.blobDir); err != nil {
			t.Fatal(err)
		}
	}
	d.startNet(listen)
}

// startNet brings up the replicator, transport and session manager.
func (d *daemon) startNet(listen string) {
	t := d.t
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.cl = transport.NewClearnet(listen, "")
	d.cl.Start(context.Background())
	if d.addr, err = d.cl.Publish(context.Background(), transport.Publication{ID: "node", Target: ln.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	var m *session.Manager
	rep, err := replicate.New(replicate.Config{Store: d.store, Shares: d.shares, Log: slog.New(slog.DiscardHandler),
		Refresh: func(ctx context.Context, p ed25519.PublicKey) error { return m.Refresh(ctx, p) }})
	if err != nil {
		t.Fatal(err)
	}
	bn, err := blobnet.New(blobnet.Config{Blobs: d.blobs, Feeds: d.store, Refs: d.refs, Pins: d.pins,
		Sessions: func() []*session.Session { return m.Sessions() }, Anchor: d.anchor,
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	mx := http.NewServeMux()
	mx.HandleFunc("/whoami", whoami)
	mx.HandleFunc(blobnet.Path, func(w http.ResponseWriter, r *http.Request) {
		if g := d.blobGate.Load(); g != nil && !(*g)(r) {
			return
		}
		bn.ServeHTTP(w, r)
	})
	mx.HandleFunc(replicate.Path, func(w http.ResponseWriter, r *http.Request) {
		if h := d.evil.Load(); h != nil {
			(*h)(w, r)
			return
		}
		rep.ServeHTTP(w, r)
	})
	m, err = session.New(session.Config{Keyring: d.kr, Cache: d.cache, Contacts: d.contacts, Tokens: d.tokens,
		Transports: []transport.Transport{d.cl}, Handler: mx,
		OnSession: func(s *session.Session) { bn.SessionUp(s); rep.Attach(s) },
		Log:       slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	d.m, d.rep, d.bn = m, rep, bn
	go d.m.Serve(ln)
}

// anchor reports whether this device is an anchor in my device list.
func (d *daemon) anchor() bool {
	b, err := d.kr.Bundle()
	if err != nil {
		return false
	}
	dev, ok := b.Devices.Find(d.kr.DevicePublic())
	return ok && dev.Anchor()
}

// stopNet takes the device offline; its store stays open.
func (d *daemon) stopNet() {
	d.m.Close()
	d.rep.Close()
	d.bn.Close()
	d.cl.Stop()
}

func (d *daemon) stop() {
	if d.store == nil {
		return
	}
	d.stopNet()
	d.store.Close()
	d.store = nil
}

// restart kills the daemon (network and store) and brings it back on the
// same address, with the same keyring and data directory but a fresh
// in-memory state.
func (d *daemon) restart() {
	d.stop()
	d.start(d.addr)
}

func (d *daemon) dev() ed25519.PublicKey { return d.kr.DevicePublic() }

func (d *daemon) session(t *testing.T, other *daemon) *session.Session {
	t.Helper()
	var s *session.Session
	eventually(t, "a session", func() bool {
		var ok bool
		s, ok = d.m.Session(other.dev())
		return ok
	})
	return s
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func get(t *testing.T, s *session.Session, path string) string {
	t.Helper()
	resp, err := s.HTTPClient().Get("http://peer" + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// checkBundle fetches the peer's bundle over s and checks it is other's.
func checkBundle(t *testing.T, s *session.Session, other *daemon) {
	t.Helper()
	b, err := session.FetchBundle(context.Background(), s)
	if err != nil {
		t.Fatalf("GET /p/bundle: %v", err)
	}
	want, _ := other.kr.Bundle()
	if !bytes.Equal(b.Marshal(), want.Marshal()) {
		t.Fatal("GET /p/bundle returned a bundle other than the peer's current one")
	}
}

func TestTwoPeopleThreeDevices(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	ka1, err := identity.Create(t.TempDir(), "Alice", "laptop", now)
	if err != nil {
		t.Fatal(err)
	}
	ka2, err := identity.NewDevice(t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := identity.Create(t.TempDir(), "Bob", "desktop", now)
	if err != nil {
		t.Fatal(err)
	}
	a1, a2 := newDaemon(t, ka1), newDaemon(t, ka2)
	b := newDaemon(t, kb)

	// 1. Alice's second device joins.
	jl, err := a1.m.Invite(pairing.KindJoin)
	if err != nil {
		t.Fatal(err)
	}
	link, err := pairing.ParseLink(jl.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a2.m.Join(ctx, link); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ka2.Person(), ka1.Person()) {
		t.Fatal("the joined device is not Alice's")
	}
	s12, s21 := a1.session(t, a2), a2.session(t, a1)
	checkBundle(t, s12, a2)
	checkBundle(t, s21, a1)
	if got := get(t, s12, "/whoami"); got != "self "+identity.ID(a1.dev()) {
		t.Fatalf("a2 sees a1 as %q", got)
	}

	// 2. Alice pairs with Bob.
	pl, err := a1.m.Invite(pairing.KindPair)
	if err != nil {
		t.Fatal(err)
	}
	if link, err = pairing.ParseLink(pl.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.m.Pair(ctx, link); err != nil {
		t.Fatal(err)
	}
	sab, sba := a1.session(t, b), b.session(t, a1)
	checkBundle(t, sab, b)
	checkBundle(t, sba, a1)
	if got := get(t, sab, "/whoami"); got != "contact "+identity.ID(a1.dev()) {
		t.Fatalf("bob sees alice as %q", got)
	}
	if got := get(t, sba, "/whoami"); got != "contact "+identity.ID(b.dev()) {
		t.Fatalf("alice sees bob as %q", got)
	}

	// Alice's other device learns of Bob through her replicated contacts
	// feed, and reaches him: Bob knows it from Alice's list.
	eventually(t, "Bob in the phone's contacts", func() bool { return a2.contacts.IsContact(kb.Person()) })
	a2.m.NoteEndpoints(kb.Endpoints())
	s2b, err := a2.m.Connect(ctx, kb.Person(), b.dev())
	if err != nil {
		t.Fatal(err)
	}
	a2.m.Keep(kb.Person(), b.dev())
	checkBundle(t, s2b, b)
	checkBundle(t, b.session(t, a2), a2)

	// 3. Bob's daemon dies and comes back on the same address: Alice's
	// devices redial and the sessions heal.
	b.restart()
	eventually(t, "healed sessions", func() bool {
		s1, ok1 := a1.m.Session(b.dev())
		s2, ok2 := a2.m.Session(b.dev())
		return ok1 && ok2 && s1 != sab && s2 != s2b
	})
	checkBundle(t, a1.session(t, b), b)
	checkBundle(t, b.session(t, a1), a1)
	checkBundle(t, a2.session(t, b), b)

	// 4. Alice removes her second device: its session with her closes at
	// once and it can no longer connect.
	old := a1.session(t, a2)
	if _, err := a1.m.RemoveDevice(a2.dev()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("removed device's session stayed up")
	}
	eventually(t, "the removed device to notice", func() bool {
		_, ok := a2.m.Session(a1.dev())
		return !ok
	})
	if _, err := a2.m.Connect(ctx, ka1.Person(), a1.dev()); err == nil {
		t.Fatal("removed device connected to Alice again")
	}
	time.Sleep(5 * session.RedialMax) // let its keeper try a few times
	if _, ok := a1.m.Session(a2.dev()); ok {
		t.Fatal("removed device holds a session with Alice")
	}

	// Bob learns of the removal on his next contact with Alice (here: a
	// refresh over his session with a1), which closes a2's session and
	// refuses it from then on.
	toA2 := b.session(t, a2)
	if err := b.m.Refresh(ctx, ka1.Person()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-toA2.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("bob kept the removed device's session")
	}
	a2.m.Forget(b.dev()) // stop its keeper, then try by hand
	eventually(t, "the removed device to notice", func() bool {
		_, ok := a2.m.Session(b.dev())
		return !ok
	})
	if _, err := a2.m.Connect(ctx, kb.Person(), b.dev()); err == nil {
		t.Fatal("removed device connected to Bob")
	}
	// Alice and Bob are unaffected.
	checkBundle(t, a1.session(t, b), b)
	checkBundle(t, b.session(t, a1), a1)
}
