package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/transport"
	"github.com/apv022/sol/internal/wire"
)

func TestMain(m *testing.M) {
	RedialMin = 20 * time.Millisecond
	RedialMax = 200 * time.Millisecond
	m.Run()
}

type node struct {
	kr       *identity.Keyring
	m        *Manager
	contacts *MemContacts
	tokens   *pairing.Tokens
}

func whoami(w http.ResponseWriter, r *http.Request) {
	p, ok := PeerFrom(r.Context())
	if !ok {
		http.Error(w, "no peer", 500)
		return
	}
	fmt.Fprintf(w, "%s %s", p.Relation, identity.ID(p.Device))
}

func newNode(t *testing.T, kr *identity.Keyring) *node {
	t.Helper()
	ctx := context.Background()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cl := transport.NewClearnet("127.0.0.1:0", "")
	cl.Start(ctx)
	addr, err := cl.Publish(ctx, transport.Publication{ID: "node", Target: ln.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kr.SetEndpoints([]identity.Endpoint{{Transport: "clearnet", Address: addr}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	n := &node{kr: kr, contacts: NewMemContacts(), tokens: pairing.NewTokens()}
	n.m, err = New(Config{Keyring: kr, Contacts: n.contacts, Tokens: n.tokens,
		Transports: []transport.Transport{cl}, Handler: http.HandlerFunc(whoami),
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	go n.m.Serve(ln)
	t.Cleanup(func() { n.m.Close(); cl.Stop() })
	return n
}

func person(t *testing.T, name string) *node {
	k, err := identity.Create(t.TempDir(), name, name+"-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return newNode(t, k)
}

func pairNodes(t *testing.T, a, b *node) {
	t.Helper()
	l, err := a.m.Invite(pairing.KindPair)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pairing.ParseLink(l.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.m.Pair(context.Background(), parsed); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, s *Session, path string) string {
	t.Helper()
	resp, err := s.HTTPClient().Get("http://peer" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPairAndRequestsBothWays(t *testing.T) {
	alice, bob := person(t, "alice"), person(t, "bob")
	pairNodes(t, alice, bob)
	if !alice.contacts.IsContact(bob.kr.Person()) || !bob.contacts.IsContact(alice.kr.Person()) {
		t.Fatal("pairing did not make both sides contacts")
	}
	eventually(t, "sessions", func() bool {
		_, a := alice.m.Session(bob.kr.DevicePublic())
		_, b := bob.m.Session(alice.kr.DevicePublic())
		return a && b
	})
	sa, _ := alice.m.Session(bob.kr.DevicePublic())
	sb, _ := bob.m.Session(alice.kr.DevicePublic())
	if got := get(t, sa, "/whoami"); got != "contact "+identity.ID(alice.kr.DevicePublic()) {
		t.Fatalf("bob saw alice as %q", got)
	}
	if got := get(t, sb, "/whoami"); got != "contact "+identity.ID(bob.kr.DevicePublic()) {
		t.Fatalf("alice saw bob as %q", got)
	}
	b, err := FetchBundle(context.Background(), sb)
	if err != nil || !bytes.Equal(b.Person.Person, alice.kr.Person()) {
		t.Fatalf("bundle from alice: %v", err)
	}

	// Many concurrent requests in both directions over the one session.
	var wg sync.WaitGroup
	for range 20 {
		for _, s := range []*Session{sa, sb} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := FetchBundle(context.Background(), s); err != nil {
					t.Error(err)
				}
			}()
		}
	}
	wg.Wait()
}

func TestPairLinkWorksOnce(t *testing.T) {
	alice, bob, carol := person(t, "alice"), person(t, "bob"), person(t, "carol")
	l, _ := alice.m.Invite(pairing.KindPair)
	if _, err := bob.m.Pair(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	if _, err := carol.m.Pair(context.Background(), l); !errors.Is(err, wire.ErrRejected) {
		t.Fatalf("second use of a pair link: %v", err)
	}
	if alice.contacts.IsContact(carol.kr.Person()) {
		t.Fatal("a burned link added a contact")
	}
}

func TestStrangerIsRejected(t *testing.T) {
	alice, carol := person(t, "alice"), person(t, "carol")
	// Carol learns Alice's endpoints but never paired: Alice says nothing.
	rec := alice.kr.Endpoints()
	carol.m.NoteEndpoints(rec)
	carol.contacts.AddContact(alice.kr.Person(), "alice") // one-sided
	carol.m.cache.Accept(mustBundle(t, alice.kr), alice.kr.DevicePublic())
	if _, err := carol.m.Connect(context.Background(), alice.kr.Person(), alice.kr.DevicePublic()); !errors.Is(err, wire.ErrRejected) {
		t.Fatalf("stranger connect: %v", err)
	}
}

func mustBundle(t *testing.T, k *identity.Keyring) identity.Bundle {
	b, err := k.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// introduce makes a and b contacts without the pair handshake (and so
// without keepers), as if they had paired earlier.
func introduce(t *testing.T, a, b *node) {
	for _, x := range [][2]*node{{a, b}, {b, a}} {
		x[0].contacts.AddContact(x[1].kr.Person(), "")
		if _, err := x[0].m.cache.Accept(mustBundle(t, x[1].kr), x[1].kr.DevicePublic()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTieBreakLowerCallerWins(t *testing.T) {
	for round := range 5 {
		alice, bob := person(t, "alice"), person(t, "bob")
		introduce(t, alice, bob)
		ad, bd := alice.kr.DevicePublic(), bob.kr.DevicePublic()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); alice.m.Connect(context.Background(), bob.kr.Person(), bd) }()
		go func() { defer wg.Done(); bob.m.Connect(context.Background(), alice.kr.Person(), ad) }()
		wg.Wait()
		low := ad
		if identity.ID(bd) < identity.ID(ad) {
			low = bd
		}
		eventually(t, fmt.Sprintf("one session, round %d", round), func() bool {
			sa, ok1 := alice.m.Session(bd)
			sb, ok2 := bob.m.Session(ad)
			return ok1 && ok2 && bytes.Equal(sa.Caller(), low) && bytes.Equal(sb.Caller(), low)
		})
		sa, _ := alice.m.Session(bd)
		if got := get(t, sa, "/whoami"); got == "" {
			t.Fatal("winning session does not serve")
		}
		// And it stays that way: nobody keeps redialing.
		time.Sleep(5 * RedialMax / 2)
		sa2, ok := alice.m.Session(bd)
		if !ok || sa2 != sa {
			t.Fatalf("round %d: session churned after the tie-break", round)
		}
	}
}

func TestJoinAndRemove(t *testing.T) {
	alice := person(t, "alice")
	k2, err := identity.NewDevice(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	phone := newNode(t, k2)
	l, err := alice.m.Invite(pairing.KindJoin)
	if err != nil {
		t.Fatal(err)
	}
	s, err := phone.m.Join(context.Background(), l, "phone")
	if err != nil {
		t.Fatal(err)
	}
	if b := mustBundle(t, alice.kr); len(b.Devices.Devices) != 2 || b.Devices.Devices[1].Name != "phone" {
		t.Fatalf("joined device's name: %+v", b.Devices.Devices)
	}
	if !bytes.Equal(k2.Person(), alice.kr.Person()) {
		t.Fatal("joined device did not adopt the person")
	}
	if got := get(t, s, "/whoami"); got != "self "+identity.ID(k2.DevicePublic()) {
		t.Fatalf("admin saw the new device as %q", got)
	}
	// The join link is burned.
	other, _ := identity.NewDevice(t.TempDir(), time.Now())
	if _, err := newNode(t, other).m.Join(context.Background(), l, ""); !errors.Is(err, wire.ErrRejected) {
		t.Fatalf("second join: %v", err)
	}

	if _, err := alice.m.RemoveDevice(k2.DevicePublic()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("removed device's session stayed up")
	}
	if _, err := phone.m.Connect(context.Background(), alice.kr.Person(), alice.kr.DevicePublic()); err == nil {
		t.Fatal("removed device connected again")
	}
}

func TestNewestBundle(t *testing.T) {
	alice, bob := person(t, "alice"), person(t, "bob")
	pairNodes(t, alice, bob)
	// Alice's identity moves on after pairing; Bob's cache is behind.
	time.Sleep(1100 * time.Millisecond) // seqs are floored at unix seconds
	if _, err := alice.kr.AddDevice(bytes.Repeat([]byte{1}, 32), "x", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, err := bob.m.NewestBundle(context.Background(), alice.kr.Person())
	if err != nil {
		t.Fatal(err)
	}
	want := mustBundle(t, alice.kr)
	if b.Devices.Seq != want.Devices.Seq {
		t.Fatalf("got list seq %d, want %d", b.Devices.Seq, want.Devices.Seq)
	}
}

// TestRecover: a contact (and only for a person it knows) answers a recovery request with the
// newest identity state it holds; a stranger's request is refused.
func TestRecover(t *testing.T) {
	alice, bob, carol := person(t, "alice"), person(t, "bob"), person(t, "carol")
	pairNodes(t, alice, bob)
	words, ok := alice.kr.PendingWords()
	if !ok {
		t.Fatal("no words")
	}
	entropy, err := identity.WordsToEntropy(words)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := identity.RecoveryKey(entropy)
	cl := transport.NewClearnet("127.0.0.1:0", "")
	ctx := context.Background()
	cl.Start(ctx)
	defer cl.Stop()
	ts := []transport.Transport{cl}
	pc, dl, err := Recover(ctx, ts, bob.kr.Endpoints(), r)
	want := mustBundle(t, alice.kr)
	if err != nil || pc.Seq != want.Person.Seq || dl.Seq != want.Devices.Seq {
		t.Fatalf("from a contact: %v (%d %d)", err, pc.Seq, dl.Seq)
	}
	// Alice's own device answers too.
	if pc, _, err = Recover(ctx, ts, alice.kr.Endpoints(), r); err != nil || pc.Seq != want.Person.Seq {
		t.Fatalf("from my own device: %v", err)
	}
	// Carol never paired with Alice: nothing, and nothing sent.
	if _, _, err := Recover(ctx, ts, carol.kr.Endpoints(), r); !errors.Is(err, ErrNothingKnown) {
		t.Fatalf("stranger: %v", err)
	}
}
