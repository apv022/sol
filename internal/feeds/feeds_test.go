package feeds

import (
	"crypto"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// person is one identity with two devices (laptop admin, phone), plus a
// contact cache, for building stores.
type fixture struct {
	t             *testing.T
	laptop, phone *identity.Keyring
	cache         *identity.Cache
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	now := time.Now()
	l, err := identity.Create(t.TempDir(), "Alice", "laptop", now)
	if err != nil {
		t.Fatal(err)
	}
	p, err := identity.NewDevice(t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := l.AddDevice(p.DevicePublic(), "phone", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := l.Bundle()
	if err := p.Adopt(b.Person, dl); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, laptop: l, phone: p, cache: identity.NewCache()}
}

func (fx *fixture) open(kr *identity.Keyring, dir string) *Store {
	fx.t.Helper()
	s, err := Open(Config{Dir: dir, Self: kr, Lists: IdentityLists(kr, fx.cache), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.t.Cleanup(func() { s.Close() })
	return s
}

func appendN(t *testing.T, s *Store, name string, n int) []Entry {
	t.Helper()
	var out []Entry
	for i := range n {
		e, err := s.Append("notes", name, []byte(`{"n": `+itoa(i)+`, "a": true}`))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func itoa(i int) string { return strconv.Itoa(i) }

func readAll(t *testing.T, s *Store, addr string) []Entry {
	t.Helper()
	es, err := s.Read(addr, 1, 5000)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// checkChains verifies every device chain in es is linear and pos runs 1..n.
func checkChains(t *testing.T, es []Entry) {
	t.Helper()
	last := map[string]Entry{}
	for i, e := range es {
		if e.Pos != uint64(i+1) {
			t.Fatalf("entry %d has pos %d", i, e.Pos)
		}
		p, ok := last[e.Device]
		if (ok && (e.Seq != p.Seq+1 || e.Prev != p.Hash)) || (!ok && (e.Seq != 1 || e.Prev != "")) {
			t.Fatalf("broken chain at %s seq %d", e.Device, e.Seq)
		}
		last[e.Device] = e
	}
}

func TestAppendReadReopen(t *testing.T) {
	fx := newFixture(t)
	dir := t.TempDir()
	s := fx.open(fx.laptop, dir)
	es := appendN(t, s, "todo", 3)
	addr := es[0].Feed
	if es[0].Body == nil || string(es[0].Body) != `{"a":true,"n":0}` {
		t.Fatalf("body not canonical: %s", es[0].Body)
	}
	got := readAll(t, s, addr)
	if len(got) != 3 || got[2].Hash != es[2].Hash || got[2].Pos != 3 {
		t.Fatalf("read back %+v", got)
	}
	checkChains(t, got)
	if fi := s.Feeds(); len(fi) != 1 || fi[0].Feed != addr || fi[0].Pos != 3 {
		t.Fatalf("Feeds = %+v", fi)
	}
	if es, _ := s.Read(addr, 3, 10); len(es) != 1 || es[0].Seq != 3 {
		t.Fatal("Read from pos 3")
	}
	s.Close()
	if _, err := s.Append("notes", "todo", []byte(`{}`)); !errors.Is(err, ErrClosed) {
		t.Fatalf("append after close: %v", err)
	}

	s2 := fx.open(fx.laptop, dir)
	got = readAll(t, s2, addr)
	if len(got) != 3 || got[2].Hash != es[2].Hash {
		t.Fatal("reopened store lost entries")
	}
	e4, err := s2.Append("notes", "todo", []byte(`{}`))
	if err != nil || e4.Seq != 4 || e4.Prev != es[2].Hash || e4.Pos != 4 {
		t.Fatalf("append after reopen: %+v %v", e4, err)
	}
	if _, err := s2.Append("notes", "Bad Name", []byte(`{}`)); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad feed name accepted")
	}
	if _, err := s2.Append("notes", "x", []byte(`{`)); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad JSON accepted")
	}
	big := make([]byte, MaxBodySize+10)
	for i := range big {
		big[i] = 'a'
	}
	big[0], big[len(big)-1] = '"', '"'
	if _, err := s2.Append("notes", "x", big); !errors.Is(err, ErrInvalid) {
		t.Fatal("oversized body accepted")
	}
}

func TestTornTailIsTruncated(t *testing.T) {
	fx := newFixture(t)
	dir := t.TempDir()
	s := fx.open(fx.laptop, dir)
	phone := fx.open(fx.phone, t.TempDir())
	es := appendN(t, s, "todo", 3)
	for _, e := range appendN(t, phone, "todo", 2) {
		if _, err := s.Accept(e.Wire()); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	addr := es[0].Feed
	fdir := filepath.Join(dir, filepath.FromSlash(addr))
	chainPath := filepath.Join(fdir, identity.ID(fx.laptop.DevicePublic())+chainExt)

	// A crash in the middle of writing the laptop's 3rd record.
	fi, _ := os.Stat(chainPath)
	if err := os.Truncate(chainPath, fi.Size()-7); err != nil {
		t.Fatal(err)
	}
	s = fx.open(fx.laptop, dir)
	got := readAll(t, s, addr)
	if len(got) != 4 {
		t.Fatalf("after a torn record: %d entries, want 4", len(got))
	}
	checkChains(t, got)
	if h := s.Heads(addr); h[identity.ID(fx.laptop.DevicePublic())] != 2 || h[identity.ID(fx.phone.DevicePublic())] != 2 {
		t.Fatalf("heads %v", h)
	}
	e, err := s.Append("notes", "todo", []byte(`{"again":1}`))
	if err != nil || e.Seq != 3 || e.Prev != es[1].Hash || e.Pos != 5 {
		t.Fatalf("append after repair: %+v %v", e, err)
	}
	s.Close()

	// Garbage after the last record, and an index that lost its tail.
	f, _ := os.OpenFile(chainPath, os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte{0, 0, 0, 40, 'x', 'y'})
	f.Close()
	idx := filepath.Join(fdir, indexFile)
	if err := os.Truncate(idx, idxRecSize*2+5); err != nil {
		t.Fatal(err)
	}
	s = fx.open(fx.laptop, dir)
	got = readAll(t, s, addr)
	if len(got) != 5 {
		t.Fatalf("after index repair: %d entries, want 5", len(got))
	}
	checkChains(t, got)
	if fi, _ := os.Stat(idx); fi.Size() != 5*idxRecSize {
		t.Fatalf("index is %d bytes after repair", fi.Size())
	}
	// A record with a bad checksum in the middle cuts the chain there.
	s.Close()
	raw, _ := os.ReadFile(chainPath)
	raw[10] ^= 0xff
	os.WriteFile(chainPath, raw, 0o600)
	s = fx.open(fx.laptop, dir)
	if h := s.Heads(addr); h[identity.ID(fx.laptop.DevicePublic())] != 0 {
		t.Fatalf("corrupt first record kept: heads %v", h)
	}
	checkChains(t, readAll(t, s, addr))
}

// sign signs w with kr's device key.
func sign(t *testing.T, kr *identity.Keyring, w Wire) Wire {
	t.Helper()
	e, err := w.signed()
	if err != nil {
		t.Fatal(err)
	}
	w.Sig, err = kr.Signer().Sign(rand.Reader, e, crypto.Hash(0))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestAcceptChecks(t *testing.T) {
	fx := newFixture(t)
	laptop := fx.open(fx.laptop, t.TempDir())
	phone := fx.open(fx.phone, t.TempDir())
	es := appendN(t, laptop, "todo", 3)
	w := func(i int) Wire { return es[i].Wire() }

	if _, err := phone.Accept(w(0)); err != nil {
		t.Fatal(err)
	}
	var gap *GapError
	if _, err := phone.Accept(w(2)); !errors.As(err, &gap) || gap.From != 2 || gap.To != 2 {
		t.Fatalf("gap: %v", err)
	}
	if _, err := phone.Accept(w(0)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	tampered := w(1)
	tampered.Body = []byte(`{"a":false,"n":1}`)
	if _, err := phone.Accept(tampered); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered body: %v", err)
	}
	tampered = w(1)
	tampered.Sig = append([]byte{}, tampered.Sig...)
	tampered.Sig[0] ^= 1
	if _, err := phone.Accept(tampered); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered sig: %v", err)
	}
	notCanon := w(1)
	notCanon.Body = []byte(`{"n":1, "a":true}`)
	if _, err := phone.Accept(notCanon); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-canonical body: %v", err)
	}
	// Validly signed, but not continuing the chain.
	badPrev := w(1)
	badPrev.Prev = es[1].Hash
	if _, err := phone.Accept(sign(t, fx.laptop, badPrev)); !errors.Is(err, ErrBadPrev) {
		t.Fatalf("bad prev: %v", err)
	}
	fork := w(0)
	fork.Body = []byte(`{"fork":1}`)
	if _, err := phone.Accept(sign(t, fx.laptop, fork)); !errors.Is(err, ErrFork) {
		t.Fatalf("fork: %v", err)
	}
	for i := 1; i < 3; i++ {
		if _, err := phone.Accept(w(i)); err != nil {
			t.Fatal(err)
		}
	}
	checkChains(t, readAll(t, phone, es[0].Feed))

	// A device not in Alice's list.
	stranger, _ := identity.NewDevice(t.TempDir(), time.Now())
	sw := Wire{Feed: es[0].Feed, Seq: 1, Device: identity.ID(stranger.DevicePublic()), Time: 1, Body: []byte(`{}`)}
	if _, err := laptop.Accept(sign(t, stranger, sw)); !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("unknown device: %v", err)
	}

	// The phone is removed: its entries stored before stay, new ones are refused.
	p1 := appendN(t, phone, "todo", 1)[0]
	if _, err := laptop.Accept(p1.Wire()); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.laptop.RemoveDevice(fx.phone.DevicePublic(), time.Now()); err != nil {
		t.Fatal(err)
	}
	p2, err := phone.Append("notes", "todo", []byte(`{"after":"removal"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.Accept(p2.Wire()); !errors.Is(err, ErrRemovedDevice) {
		t.Fatalf("removed device: %v", err)
	}
	if got := readAll(t, laptop, es[0].Feed); len(got) != 4 || got[3].Hash != p1.Hash {
		t.Fatal("entries from before the removal were lost")
	}

	// A contact's feed verifies against the cache.
	bob, _ := identity.Create(t.TempDir(), "Bob", "pc", time.Now())
	bs := fx.open(bob, t.TempDir())
	be, _ := bs.Append("notes", "todo", []byte(`{}`))
	if _, err := laptop.Accept(be.Wire()); !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("unknown contact: %v", err)
	}
	bb, _ := bob.Bundle()
	if _, err := fx.cache.Accept(bb, bob.DevicePublic()); err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.Accept(be.Wire()); err != nil {
		t.Fatalf("contact entry: %v", err)
	}
	if _, err := laptop.Append("notes", "", []byte(`{}`)); err == nil {
		t.Fatal("empty feed name accepted")
	}
}

// TestConcurrentDevices appends on two devices of one person at once while
// each forwards to the other: both converge, every chain stays linear.
func TestConcurrentDevices(t *testing.T) {
	fx := newFixture(t)
	laptop := fx.open(fx.laptop, t.TempDir())
	phone := fx.open(fx.phone, t.TempDir())
	const n = 60
	addr := Address(identity.ID(fx.laptop.Person()), "notes", "todo")
	var wg sync.WaitGroup
	for _, s := range []*Store{laptop, phone} {
		for range 3 { // several writers per device
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range n / 3 {
					if _, err := s.Append("notes", "todo", []byte(`{"i":`+itoa(i)+`}`)); err != nil {
						t.Error(err)
					}
				}
			}()
		}
	}
	writers := make(chan struct{})
	go func() { wg.Wait(); close(writers) }()
	var fw sync.WaitGroup
	forward := func(from, to *Store) {
		defer fw.Done()
		var pos uint64
		for {
			es, _ := from.Read(addr, pos+1, 100)
			for _, e := range es {
				pos = e.Pos
				if _, err := to.Accept(e.Wire()); err != nil && !errors.Is(err, ErrDuplicate) {
					t.Error(err)
				}
			}
			if len(es) > 0 {
				continue
			}
			select {
			case <-writers:
				if es, _ := from.Read(addr, pos+1, 1); len(es) == 0 {
					return
				}
			case <-time.After(time.Millisecond):
			}
		}
	}
	fw.Add(2)
	go forward(laptop, phone)
	go forward(phone, laptop)
	fw.Wait()
	for _, s := range []*Store{laptop, phone} {
		es := readAll(t, s, addr)
		if len(es) != 2*n {
			t.Fatalf("a device has %d entries, want %d", len(es), 2*n)
		}
		checkChains(t, es)
	}
	if hl, hp := laptop.Heads(addr), phone.Heads(addr); len(hl) != 2 || hl[identity.ID(fx.laptop.DevicePublic())] != n || hp[identity.ID(fx.phone.DevicePublic())] != n {
		t.Fatalf("heads %v %v", hl, hp)
	}
}

func TestViews(t *testing.T) {
	fx := newFixture(t)
	dir := t.TempDir()
	laptop := fx.open(fx.laptop, dir)
	phone := fx.open(fx.phone, t.TempDir())
	bob, _ := identity.Create(t.TempDir(), "Bob", "pc", time.Now())
	bobID := identity.ID(bob.Person())
	lc, pc := NewContacts(laptop), NewContacts(phone)
	if lc.IsContact(bob.Person()) {
		t.Fatal("contact before adding")
	}
	if err := lc.AddContact(bob.Person(), "Bob"); err != nil {
		t.Fatal(err)
	}
	sync := func() {
		for _, fi := range laptop.Feeds() {
			for _, e := range readAll(t, laptop, fi.Feed) {
				if _, err := phone.Accept(e.Wire()); err != nil && !errors.Is(err, ErrDuplicate) {
					t.Fatal(err)
				}
			}
		}
	}
	sync()
	if n, ok := pc.Name(bob.Person()); !ok || n != "Bob" {
		t.Fatal("contact did not reach the phone")
	}
	if l := pc.List(); len(l) != 1 || l[0] != bobID {
		t.Fatalf("List = %v", l)
	}

	ls, err := NewShares(laptop, lc)
	if err != nil {
		t.Fatal(err)
	}
	stranger, _ := identity.Create(t.TempDir(), "Eve", "pc", time.Now())
	if err := ls.Share("notes", "todo", []string{identity.ID(stranger.Person())}, ModeRead); err == nil {
		t.Fatal("shared with a non-contact")
	}
	if err := ls.Share(InternalApp, ContactsFeed, []string{bobID}, ModeRead); err == nil {
		t.Fatal("shared an internal feed")
	}
	if err := ls.Share("notes", "todo", []string{bobID}, ModeRead); err != nil {
		t.Fatal(err)
	}
	me := identity.ID(fx.laptop.Person())
	todo, doc := Address(me, "notes", "todo"), Address(me, "notes", "doc")
	if !ls.CanRead(bobID, todo) || ls.CanRead(bobID, doc) || !ls.CanRead(me, doc) || ls.CanRead(bobID, Address(me, InternalApp, ContactsFeed)) {
		t.Fatal("CanRead after share")
	}
	if l := ls.List("notes", "todo"); len(l) != 1 || l[0].Mode != ModeRead || l[0].Feed != "todo" {
		t.Fatalf("List = %+v", l)
	}
	// Bob write-shares his "doc": he may pull my "doc" as co-author.
	sub := laptop.Bus().Subscribe(10)
	if err := ls.SetIncoming(bobID, map[string]string{"notes/doc": ModeWrite}, 0); err != nil {
		t.Fatal(err)
	}
	ev := <-sub.C()
	for ev.Type == EventEntry { // the incoming feed's own entry
		ev = <-sub.C()
	}
	if ev.Type != EventFeed || ev.Feed != bobID+"/notes/doc" || !ev.Readable {
		t.Fatalf("event %+v", ev)
	}
	// Security review F1: his claim alone grants nothing, or any contact could read any feed
	// of mine by announcing a write share of a feed with the same name.
	if ls.CanRead(bobID, doc) || len(ls.ReadableBy(bobID)) != 1 {
		t.Fatalf("an announced write share granted my same-named feed: %v", ls.ReadableBy(bobID))
	}
	if err := ls.SetIncoming(bobID, map[string]string{"notes/doc": ModeWrite, "notes/todo": ModeWrite, "files/tree": ModeWrite}, 0); err != nil {
		t.Fatal(err)
	}
	if ls.CanRead(bobID, Address(me, "files", "tree")) || !ls.CanRead(bobID, todo) || len(ls.ReadableBy(bobID)) != 1 {
		t.Fatal("announced write shares changed what Bob may read")
	}
	if err := ls.SetIncoming(bobID, map[string]string{"notes/doc": ModeWrite}, 0); err != nil {
		t.Fatal(err)
	}
	// Versioned announcements: one from a device of Bob's that is behind (a lower version) never
	// takes back what a device that caught up announced; a newer or equal one applies.
	if err := ls.SetIncoming(bobID, map[string]string{"notes/doc": ModeWrite, "notes/x": ModeRead}, 7); err != nil {
		t.Fatal(err)
	}
	if err := ls.SetIncoming(bobID, map[string]string{"notes/doc": ModeWrite}, 5); err != nil {
		t.Fatal(err)
	}
	if ls.Incoming(bobID, "notes", "x") != ModeRead {
		t.Fatal("a stale announcement replaced a newer one")
	}
	if err := ls.SetIncoming(bobID, map[string]string{"notes/doc": ModeWrite}, 8); err != nil {
		t.Fatal(err)
	}
	if ls.Incoming(bobID, "notes", "x") != "" || ls.Incoming(bobID, "notes", "doc") != ModeWrite {
		t.Fatal("a newer announcement was ignored")
	}
	// My app consents by sharing its co-author feed back.
	if err := ls.Share("notes", "doc", []string{bobID}, ModeRead); err != nil {
		t.Fatal(err)
	}
	if ls.ReadMode(bobID, doc) != ModeCoauthor {
		t.Fatal("co-author cannot read")
	}
	if rb := ls.ReadableBy(bobID); len(rb) != 2 || rb[todo] != ModeRead || rb[doc] != ModeCoauthor {
		t.Fatalf("ReadableBy = %v", rb)
	}
	// Last writer wins across devices.
	sync()
	ps, _ := NewShares(phone, pc)
	if ps.Mode("notes", "todo", bobID) != ModeRead {
		t.Fatal("share did not reach the phone")
	}
	time.Sleep(2 * time.Millisecond)
	if err := ps.Share("notes", "todo", []string{bobID}, ModeWrite); err != nil {
		t.Fatal(err)
	}
	for _, e := range readAll(t, phone, Address(me, InternalApp, SharesFeed)) {
		laptop.Accept(e.Wire())
	}
	if ls.Mode("notes", "todo", bobID) != ModeWrite {
		t.Fatal("the newer share did not win")
	}
	if err := ls.Unshare("notes", "todo", []string{bobID}); err != nil {
		t.Fatal(err)
	}
	if ls.CanRead(bobID, todo) {
		t.Fatal("still readable after unshare")
	}
	// Incoming shares survive a restart.
	ls2, err := NewShares(laptop, lc)
	if err != nil || ls2.Incoming(bobID, "notes", "doc") != ModeWrite {
		t.Fatal("incoming shares not persisted")
	}
}
