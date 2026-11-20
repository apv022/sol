package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/replicate"
	"github.com/apv022/sol/internal/session"
)

// aliceAndBob sets up Alice's laptop (admin) and phone, and Bob, paired,
// with sessions between every pair of devices.
func aliceAndBob(t *testing.T) (a1, a2, b *daemon) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	ka1, _ := identity.Create(t.TempDir(), "Alice", "laptop", now)
	ka2, _ := identity.NewDevice(t.TempDir(), now)
	kb, _ := identity.Create(t.TempDir(), "Bob", "desktop", now)
	a1, a2, b = newDaemon(t, ka1), newDaemon(t, ka2), newDaemon(t, kb)
	jl, err := a1.m.Invite(pairing.KindJoin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a2.m.Join(ctx, jl); err != nil {
		t.Fatal(err)
	}
	pl, err := a1.m.Invite(pairing.KindPair)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.m.Pair(ctx, pl); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Bob in the phone's contacts", func() bool { return a2.contacts.IsContact(kb.Person()) })
	a2.m.NoteEndpoints(kb.Endpoints())
	if _, err := a2.m.Connect(ctx, kb.Person(), b.dev()); err != nil {
		t.Fatal(err)
	}
	a2.m.Keep(kb.Person(), b.dev())
	return a1, a2, b
}

func (d *daemon) person() string { return identity.ID(d.kr.Person()) }

func (d *daemon) append(t *testing.T, app, name, body string) feeds.Entry {
	t.Helper()
	e, err := d.store.Append(app, name, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (d *daemon) entries(t *testing.T, addr string) []feeds.Entry {
	t.Helper()
	es, err := d.store.Read(addr, 1, 5000)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// count waits until the feed at addr holds exactly n entries on d, with
// linear chains, and stays there for a moment (no duplicates trickle in).
func (d *daemon) count(t *testing.T, addr string, n int) []feeds.Entry {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for got := len(d.entries(t, addr)); got < n; got = len(d.entries(t, addr)) {
		if time.Now().After(deadline) {
			t.Fatalf("%s holds %d entries, want %d", addr, got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	es := d.entries(t, addr)
	if len(es) != n {
		t.Fatalf("%s holds %d entries, want %d", addr, len(es), n)
	}
	last := map[string]feeds.Entry{}
	for i, e := range es {
		p, ok := last[e.Device]
		if e.Pos != uint64(i+1) || (ok && (e.Seq != p.Seq+1 || e.Prev != p.Hash)) || (!ok && e.Seq != 1) {
			t.Fatalf("broken chain or pos in %s at pos %d", addr, i+1)
		}
		last[e.Device] = e
	}
	return es
}

// rawSync sends a /p/sync request over s and returns the messages up to and
// including "live".
func rawSync(t *testing.T, s *session.Session, req replicate.Request) []replicate.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(req)
	hr, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://peer"+replicate.Path, bytes.NewReader(body))
	resp, err := s.HTTPClient().Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []replicate.Message
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m replicate.Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
		if m.T == replicate.MsgLive {
			return out
		}
	}
	t.Fatalf("stream ended before live: %v", sc.Err())
	return nil
}

func TestFeedReplication(t *testing.T) {
	a1, a2, b := aliceAndBob(t)
	alice, bob := a1.person(), b.person()
	todo := feeds.Address(alice, "notes", "todo")

	// 1. Both of Alice's devices write while apart; they converge.
	a2.stopNet()
	for range 3 {
		a1.append(t, "notes", "todo", `{"from":"laptop"}`)
	}
	for range 2 {
		a2.append(t, "notes", "todo", `{"from":"phone"}`)
	}
	a2.startNet(a2.addr)
	a1.count(t, todo, 5)
	a2.count(t, todo, 5)
	if h1, h2 := a1.store.Heads(todo), a2.store.Heads(todo); h1[identity.ID(a1.dev())] != 3 || h2[identity.ID(a2.dev())] != 2 || len(h1) != 2 || len(h2) != 2 {
		t.Fatalf("heads differ: %v %v", h1, h2)
	}

	// 2. A secret feed is never sent to Bob, even when he asks for it
	// directly: the callee filters, not the client.
	secret := feeds.Address(alice, "notes", "secret")
	a1.append(t, "notes", "secret", `{"pin":1234}`)
	contacts := feeds.Address(alice, feeds.InternalApp, feeds.ContactsFeed)
	for _, all := range []bool{false, true} {
		msgs := rawSync(t, b.session(t, a1), replicate.Request{All: all,
			Feeds: map[string]map[string]uint64{secret: {}, todo: {}, contacts: {}}})
		for _, m := range msgs {
			if m.T == replicate.MsgEntry || m.Readable {
				t.Fatalf("Bob got %+v from an unshared feed", m)
			}
		}
		if !all && len(msgs) != 4 { // three refusals and live
			t.Fatalf("explicit request answered with %+v", msgs)
		}
	}
	if len(b.entries(t, todo)) != 0 || len(b.entries(t, secret)) != 0 {
		t.Fatal("Bob holds Alice's unshared feeds")
	}

	// 3. Alice shares todo with Bob (read): he gets the backlog, then live
	// entries from either of her devices.
	sub := b.store.Bus().Subscribe(4096)
	defer sub.Close()
	if err := a1.shares.Share("notes", "todo", []string{bob}, feeds.ModeRead); err != nil {
		t.Fatal(err)
	}
	b.count(t, todo, 5)
	waitEvent(t, sub, todo, true)
	if b.shares.Incoming(alice, "notes", "todo") != feeds.ModeRead {
		t.Fatal("Bob does not know the feed is shared with him")
	}
	eventually(t, "the share on the phone", func() bool { return a2.shares.Mode("notes", "todo", bob) == feeds.ModeRead })
	a2.append(t, "notes", "todo", `{"live":1}`)
	b.count(t, todo, 6)
	if _, err := b.store.Append("notes", "todo", []byte(`{}`)); err != nil { // Bob's own, separate feed
		t.Fatal(err)
	}

	// 4. Unsharing stops it.
	if err := a1.shares.Unshare("notes", "todo", []string{bob}); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, sub, todo, false)
	eventually(t, "the unshare on the phone", func() bool { return a2.shares.Mode("notes", "todo", bob) == "" })
	a1.append(t, "notes", "todo", `{"after":"unshare"}`)
	a2.append(t, "notes", "todo", `{"after":"unshare"}`)
	a2.count(t, todo, 8)
	time.Sleep(300 * time.Millisecond)
	if n := len(b.entries(t, todo)); n != 6 {
		t.Fatalf("Bob got %d entries after the unshare, want 6", n)
	}

	// 5. A write share: Bob gets Alice's doc and Alice pulls Bob's
	// same-named doc as its co-author.
	doc, bobDoc := feeds.Address(alice, "notes", "doc"), feeds.Address(bob, "notes", "doc")
	a1.append(t, "notes", "doc", `{"title":"plan"}`)
	b.append(t, "notes", "doc", `{"edit":"bob"}`)
	if err := a1.shares.Share("notes", "doc", []string{bob}, feeds.ModeWrite); err != nil {
		t.Fatal(err)
	}
	b.count(t, doc, 1)
	a1.count(t, bobDoc, 1)
	b.append(t, "notes", "doc", `{"edit":"bob again"}`)
	a1.count(t, bobDoc, 2)
	a2.count(t, bobDoc, 2) // my own devices replicate every feed I hold

	// 6. A tampered entry is rejected; the genuine one arrives later.
	tamper := feeds.Address(alice, "notes", "tamper")
	var forged atomic.Int32
	evil := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forged.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		for _, e := range a2.entries(t, tamper) {
			wire := e.Wire()
			wire.Body = json.RawMessage(`{"v":"forged"}`)
			enc.Encode(replicate.Message{T: replicate.MsgEntry, Entry: &wire})
		}
		enc.Encode(replicate.Message{T: replicate.MsgLive})
		<-r.Context().Done()
	})
	a2.evil.Store(&evil)
	a2.stopNet()
	a2.append(t, "notes", "tamper", `{"v":"real"}`)
	a2.startNet(a2.addr)
	eventually(t, "a pull from the forger", func() bool { return forged.Load() > 0 })
	time.Sleep(300 * time.Millisecond)
	if n := len(a1.entries(t, tamper)); n != 0 {
		t.Fatal("a forged entry was stored")
	}
	a2.evil.Store(nil)
	a2.stopNet()
	a2.startNet(a2.addr)
	if es := a1.count(t, tamper, 1); string(es[0].Body) != `{"v":"real"}` {
		t.Fatalf("got %s", es[0].Body)
	}

	// 7. Bob's daemon restarts: it resumes from disk, with no duplicates
	// and no gaps, and still knows what Alice shares with him.
	b.restart()
	if b.shares.Incoming(alice, "notes", "doc") != feeds.ModeWrite {
		t.Fatal("incoming shares lost in the restart")
	}
	b.count(t, doc, 1)
	b.count(t, todo, 6)
	a1.append(t, "notes", "doc", `{"title":"plan v2"}`)
	a2.append(t, "notes", "doc", `{"title":"plan v3"}`)
	b.count(t, doc, 3)
	b.count(t, todo, 6)
	b.append(t, "notes", "doc", `{"edit":"after restart"}`)
	a1.count(t, bobDoc, 3)

	// 8. Alice removes her phone: its new entries are refused by her laptop
	// and, once he learns of it, by Bob, while its old ones stay.
	if _, err := a1.m.RemoveDevice(a2.dev()); err != nil {
		t.Fatal(err)
	}
	if err := b.m.Refresh(context.Background(), a1.kr.Person()); err != nil {
		t.Fatal(err)
	}
	late := a2.append(t, "notes", "doc", `{"from":"removed phone"}`)
	if _, err := a1.store.Accept(late.Wire()); !errors.Is(err, feeds.ErrRemovedDevice) {
		t.Fatalf("laptop: %v", err)
	}
	if _, err := b.store.Accept(late.Wire()); !errors.Is(err, feeds.ErrRemovedDevice) {
		t.Fatalf("Bob: %v", err)
	}
	b.count(t, doc, 3)
}

// waitEvent waits for a feed readability event on sub.
func waitEvent(t *testing.T, sub interface {
	C() <-chan feeds.Event
}, addr string, readable bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-sub.C():
			if ev.Type == feeds.EventFeed && ev.Feed == addr && ev.Readable == readable {
				return
			}
		case <-deadline:
			t.Fatalf("no feed event for %s (readable %v)", addr, readable)
		}
	}
}

// TestSlowPeerDoesNotBlock: a contact that opens a pull and never reads it
// (its mux window fills up) does not stall replication to anyone else.
func TestSlowPeerDoesNotBlock(t *testing.T) {
	a1, a2, b := aliceAndBob(t)
	alice := a1.person()
	big := feeds.Address(alice, "notes", "big")
	pad := `"` + string(bytes.Repeat([]byte("x"), 4000)) + `"`
	for range 150 { // ~600 KiB, beyond a stream's 256 KiB window
		a1.append(t, "notes", "big", `{"pad":`+pad+`}`)
	}
	if err := a1.shares.Share("notes", "big", []string{b.person()}, feeds.ModeRead); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, _ := json.Marshal(replicate.Request{All: true})
	hr, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://peer"+replicate.Path, bytes.NewReader(body))
	resp, err := b.session(t, a1).HTTPClient().Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() // never read
	a2.count(t, big, 150)
	for i := range 20 {
		a1.append(t, "notes", "big", `{"i":`+string(rune('0'+i%10))+`}`)
	}
	a2.count(t, big, 170)
	b.count(t, big, 170) // Bob's own puller is a separate stream
}
