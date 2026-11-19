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
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/internal/transport"
)

func TestMain(m *testing.M) {
	session.RedialMin = 20 * time.Millisecond
	session.RedialMax = 200 * time.Millisecond
	m.Run()
}

// daemon is one device: keyring, contacts, tokens, a clearnet transport
// publishing the session manager's listener.
type daemon struct {
	t        *testing.T
	kr       *identity.Keyring
	contacts session.Contacts
	tokens   *pairing.Tokens
	cl       *transport.Clearnet
	m        *session.Manager
	addr     string // public clearnet address, stable across restarts
}

func whoami(w http.ResponseWriter, r *http.Request) {
	p, _ := session.PeerFrom(r.Context())
	fmt.Fprintf(w, "%s %s", p.Relation, identity.ID(p.Device))
}

func newDaemon(t *testing.T, kr *identity.Keyring, contacts session.Contacts) *daemon {
	d := &daemon{t: t, kr: kr, contacts: contacts, tokens: pairing.NewTokens()}
	d.start("127.0.0.1:0")
	if _, err := kr.SetEndpoints([]identity.Endpoint{{Transport: "clearnet", Address: d.addr}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.stop)
	return d
}

func (d *daemon) start(listen string) {
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
	d.m, err = session.New(session.Config{Keyring: d.kr, Contacts: d.contacts, Tokens: d.tokens,
		Transports: []transport.Transport{d.cl}, Handler: http.HandlerFunc(whoami),
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	go d.m.Serve(ln)
}

func (d *daemon) stop() {
	d.m.Close()
	d.cl.Stop()
}

// restart kills the daemon's listener and sessions and brings it back on
// the same address, with the same keyring and contacts but a fresh
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
	// Alice's devices share one contacts store: it stands in for the
	// contacts replication between own devices (a later ticket).
	aliceContacts := session.NewMemContacts()
	a1, a2 := newDaemon(t, ka1, aliceContacts), newDaemon(t, ka2, aliceContacts)
	b := newDaemon(t, kb, session.NewMemContacts())

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

	// Alice's other device reaches Bob too: Bob knows it from Alice's list.
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
