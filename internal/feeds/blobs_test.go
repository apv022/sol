package feeds

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
)

func hashOfString(s string) string {
	d := sha256.Sum256([]byte(s))
	return hex.EncodeToString(d[:])
}

// copyFeeds hands every entry of from's feeds to to, as replication would.
func copyFeeds(t *testing.T, from, to *Store) {
	t.Helper()
	for _, fi := range from.Feeds() {
		for _, e := range readAll(t, from, fi.Feed) {
			if _, err := to.Accept(e.Wire()); err != nil && !errors.Is(err, ErrDuplicate) {
				t.Fatal(err)
			}
		}
	}
}

func TestBlobRefsIn(t *testing.T) {
	a, b := hashOfString("a"), hashOfString("b")
	body := fmt.Sprintf(`{"x":"sha256:%s","y":["sha256:%s",{"z":"see sha256:%s"}],"bad":"sha256:%s","short":"sha256:abc"}`,
		a, b, a, "ABCDEF"+a[6:])
	got := BlobRefsIn([]byte(body))
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("BlobRefsIn = %v", got)
	}
}

func TestScanBodyRelease(t *testing.T) {
	a, b, c := hashOfString("a"), hashOfString("b"), hashOfString("c")
	hexes := func(ds [][32]byte) []string {
		var out []string
		for _, d := range ds {
			out = append(out, hex.EncodeToString(d[:]))
		}
		return out
	}
	for _, tc := range []struct {
		body       string
		refs, rels []string
	}{
		// Only the top-level release field releases; what it names is not a reference.
		{`{"op":"purge","release":["sha256:` + a + `","sha256:` + b + `"]}`, nil, []string{a, b}},
		// A reference elsewhere in the same entry wins over its release.
		{`{"blob":"sha256:` + a + `","release":["sha256:` + a + `","sha256:` + c + `"]}`, []string{a}, []string{c}},
		// A nested "release" is an ordinary field.
		{`{"x":{"release":["sha256:` + a + `"]}}`, []string{a}, nil},
		{`{"release":"sha256:` + b + `"}`, nil, []string{b}},
	} {
		refs, rels := scanBody([]byte(tc.body))
		if g, w := fmt.Sprint(hexes(refs)), fmt.Sprint(tc.refs); g != w {
			t.Errorf("%s: refs %v, want %v", tc.body, g, w)
		}
		if g, w := fmt.Sprint(hexes(rels)), fmt.Sprint(tc.rels); g != w {
			t.Errorf("%s: releases %v, want %v", tc.body, g, w)
		}
	}
	if got := BlobRefsIn([]byte(`{"blob":"sha256:` + a + `","release":["sha256:` + b + `"]}`)); len(got) != 1 || got[0] != a {
		t.Fatalf("BlobRefsIn = %v", got)
	}
}

func TestBlobRelease(t *testing.T) {
	fx := newFixture(t)
	laptop := fx.open(fx.laptop, t.TempDir())
	bob, _ := identity.Create(t.TempDir(), "Bob", "pc", time.Now())
	bobID := identity.ID(bob.Person())
	c := NewContacts(laptop)
	c.AddContact(bob.Person(), "Bob")
	sh, _ := NewShares(laptop, c)
	refs, err := NewBlobRefs(laptop, sh)
	if err != nil {
		t.Fatal(err)
	}
	var hooked []string
	refs.OnRelease(func(hs []string) { hooked = append(hooked, hs...) })
	pins := NewPins(laptop)
	put := func(feed, h string) {
		t.Helper()
		if _, err := laptop.Append("files", feed, []byte(`{"op":"put","blob":"sha256:`+h+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	release := func(feed string, hs ...string) {
		t.Helper()
		var rs []string
		for _, h := range hs {
			rs = append(rs, `"sha256:`+h+`"`)
		}
		if _, err := laptop.Append("files", feed, []byte(`{"op":"purge","release":[`+strings.Join(rs, ",")+`]}`)); err != nil {
			t.Fatal(err)
		}
	}
	gone, again, shared, pinned := hashOfString("gone"), hashOfString("again"), hashOfString("shared"), hashOfString("pinned")
	put("tree", gone)
	put("tree", again)
	put("tree", shared)
	put("tree", pinned)
	put("other", shared)
	if err := sh.Share("files", "tree", []string{bobID}, ModeRead); err != nil {
		t.Fatal(err)
	}
	if !refs.MayFetch(bobID, gone) {
		t.Fatal("Bob cannot fetch a referenced blob")
	}
	release("tree", gone, again, shared, pinned)
	if err := pins.Pin([]string{pinned}); err != nil {
		t.Fatal(err)
	}
	// Released: no longer mine, no longer fetchable through the feed, listed for the collector.
	if refs.Mine(gone) || refs.MayFetch(bobID, gone) || len(refs.Feeds(gone)) != 0 {
		t.Fatal("a released blob is still referenced")
	}
	// Released in one feed, still referenced by another.
	if !refs.Mine(shared) || fmt.Sprint(refs.Feeds(shared)) != "["+Address(identity.ID(fx.laptop.Person()), "files", "other")+"]" {
		t.Fatalf("Feeds(shared) = %v", refs.Feeds(shared))
	}
	// A pinned blob is unreferenced too: pins are the collector's business, not the index's.
	if refs.Mine(pinned) || !pins.Pinned(pinned) {
		t.Fatal("pinned")
	}
	if fmt.Sprint(hooked) != fmt.Sprint([]string{gone, again, shared, pinned}) {
		t.Fatalf("OnRelease saw %v", hooked)
	}
	if got := fmt.Sprint(refs.Released()); got != fmt.Sprint(sorted(gone, again, pinned)) {
		t.Fatalf("Released = %v", got)
	}
	// Re-uploading the same bytes after the release references them again.
	put("tree", again)
	if !refs.Mine(again) || !refs.MayFetch(bobID, again) {
		t.Fatal("a reference after the release does not count")
	}
	// The rebuilt index agrees.
	re, err := NewBlobRefs(laptop, sh)
	if err != nil {
		t.Fatal(err)
	}
	if re.Mine(gone) || !re.Mine(again) || !re.Mine(shared) || fmt.Sprint(re.Released()) != fmt.Sprint(sorted(gone, pinned)) {
		t.Fatalf("rebuilt index: released %v", re.Released())
	}
}

// TestReleaseOrderAcrossDevices: a device whose clock is behind still stamps
// what it writes after a release it has seen, so its re-upload counts.
func TestReleaseOrderAcrossDevices(t *testing.T) {
	fx := newFixture(t)
	laptop := fx.open(fx.laptop, t.TempDir())
	sh, _ := NewShares(laptop, nil)
	refs, err := NewBlobRefs(laptop, sh)
	if err != nil {
		t.Fatal(err)
	}
	h := hashOfString("x")
	if _, err := laptop.Append("files", "tree", []byte(`{"blob":"sha256:`+h+`"}`)); err != nil {
		t.Fatal(err)
	}
	// The phone, its clock five minutes ahead, releases it.
	addr := Address(identity.ID(fx.laptop.Person()), "files", "tree")
	ahead := time.Now().Add(5 * time.Minute).UnixMilli()
	w := sign(t, fx.phone, Wire{Feed: addr, Seq: 1, Device: identity.ID(fx.phone.DevicePublic()), Time: ahead,
		Body: []byte(`{"release":["sha256:` + h + `"]}`)})
	first, err := laptop.Accept(w)
	if err != nil {
		t.Fatal(err)
	}
	if refs.Mine(h) {
		t.Fatal("release from the phone ignored")
	}
	e, err := laptop.Append("files", "tree", []byte(`{"blob":"sha256:`+h+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.Time.UnixMilli() <= ahead || !refs.Mine(h) {
		t.Fatalf("re-upload stamped %v (release at %v), mine %v", e.Time.UnixMilli(), ahead, refs.Mine(h))
	}
	// A clock a year ahead does not drag this device's entries along: they stay within
	// maxClockLead of its own clock.
	far := time.Now().Add(365 * 24 * time.Hour).UnixMilli()
	w = sign(t, fx.phone, Wire{Feed: addr, Seq: 2, Prev: first.Hash, Device: identity.ID(fx.phone.DevicePublic()), Time: far,
		Body: []byte(`{"note":"from the future"}`)})
	if _, err := laptop.Accept(w); err != nil {
		t.Fatal(err)
	}
	e, err = laptop.Append("files", "tree", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if lim := time.Now().Add(maxClockLead + time.Second).UnixMilli(); e.Time.UnixMilli() > lim {
		t.Fatalf("stamped %v, more than %v ahead", e.Time, maxClockLead)
	}
}

func sorted(s ...string) []string {
	slices.Sort(s)
	return s
}

func TestBlobPermission(t *testing.T) {
	fx := newFixture(t)
	dir := t.TempDir()
	laptop := fx.open(fx.laptop, dir)
	bob, _ := identity.Create(t.TempDir(), "Bob", "pc", time.Now())
	eve, _ := identity.Create(t.TempDir(), "Eve", "pc", time.Now())
	bobID, eveID, me := identity.ID(bob.Person()), identity.ID(eve.Person()), identity.ID(fx.laptop.Person())
	c := NewContacts(laptop)
	c.AddContact(bob.Person(), "Bob")
	c.AddContact(eve.Person(), "Eve")
	sh, err := NewShares(laptop, c)
	if err != nil {
		t.Fatal(err)
	}
	// An entry stored before the index exists is found by the rebuild.
	shared, private, pinned := hashOfString("shared"), hashOfString("private"), hashOfString("pinned")
	if _, err := laptop.Append("files", "music", []byte(`{"song":"sha256:`+shared+`"}`)); err != nil {
		t.Fatal(err)
	}
	refs, err := NewBlobRefs(laptop, sh)
	if err != nil {
		t.Fatal(err)
	}
	// One stored after is found by the hook.
	if _, err := laptop.Append("files", "secret", []byte(`{"f":"sha256:`+private+`"}`)); err != nil {
		t.Fatal(err)
	}
	pins := NewPins(laptop)
	if err := pins.Pin([]string{pinned}); err != nil {
		t.Fatal(err)
	}
	if err := sh.Share("files", "music", []string{bobID}, ModeRead); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		who, hash string
		want      bool
	}{
		{bobID, shared, true}, {bobID, private, false}, {eveID, shared, false},
		{me, private, true}, {me, hashOfString("unknown"), true}, // my devices fetch anything
		{bobID, pinned, false}, // internal feeds are not references
	} {
		if got := refs.MayFetch(tc.who, tc.hash); got != tc.want {
			t.Errorf("MayFetch(%s, %s) = %v", tc.who[:6], tc.hash[:6], got)
		}
	}
	if !refs.Mine(shared) || refs.Mine(pinned) || !pins.Pinned(pinned) || len(pins.List()) != 1 {
		t.Fatal("Mine or Pins")
	}
	// Unshare revokes at once.
	if err := sh.Unshare("files", "music", []string{bobID}); err != nil {
		t.Fatal(err)
	}
	if refs.MayFetch(bobID, shared) {
		t.Fatal("still fetchable after unshare")
	}
	// A co-author (Bob write-shares his "music") may read mine, and so fetch.
	if err := sh.SetIncoming(bobID, map[string]string{"files/music": ModeWrite}, 0); err != nil {
		t.Fatal(err)
	}
	if refs.MayFetch(bobID, shared) {
		t.Fatal("an announced write share alone let Bob fetch (security review F1)")
	}
	if err := sh.Share("files", "music", []string{bobID}, ModeRead); err != nil {
		t.Fatal(err)
	}
	if !refs.MayFetch(bobID, shared) {
		t.Fatal("co-author cannot fetch")
	}
	// Bob's own feed (replicated to me) referencing a blob lists him as a
	// source, but grants nobody anything from me.
	bobs := fx.open(bob, t.TempDir())
	if _, err := bobs.Append("files", "mix", []byte(`{"s":"sha256:`+private+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.cache.Accept(mustBundle(t, bob), bob.DevicePublic()); err != nil {
		t.Fatal(err)
	}
	copyFeeds(t, bobs, laptop)
	if fs := refs.Feeds(private); len(fs) != 2 || refs.MayFetch(eveID, private) {
		t.Fatalf("Feeds = %v", fs)
	}
	if err := pins.Unpin([]string{pinned}); err != nil || pins.Pinned(pinned) {
		t.Fatal("unpin")
	}
	if pins.Pin([]string{"sha256:" + pinned}) == nil {
		t.Fatal("pinned a malformed hash")
	}
}

func mustBundle(t *testing.T, kr *identity.Keyring) identity.Bundle {
	t.Helper()
	b, err := kr.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestBlobRefsScale checks the index stays fast with 100k referencing
// entries (fed straight to the index: signing and storing them is the
// store's cost, measured elsewhere).
func TestBlobRefsScale(t *testing.T) {
	fx := newFixture(t)
	s := fx.open(fx.laptop, t.TempDir())
	sh, _ := NewShares(s, nil)
	refs, err := NewBlobRefs(s, sh)
	if err != nil {
		t.Fatal(err)
	}
	me := identity.ID(fx.laptop.Person())
	start := time.Now()
	const n = 100_000
	for i := range n {
		addr := Address(me, "files", "f"+itoa(i%100))
		refs.add(Entry{Feed: addr, Device: me, Seq: uint64(i + 1), Time: start,
			Body: []byte(`{"a":"sha256:` + hashOfString(itoa(i)) + `","b":"sha256:` + hashOfString("common") + `"}`)})
	}
	add := time.Since(start)
	start = time.Now()
	for i := range n {
		if len(refs.Feeds(hashOfString(itoa(i)))) != 1 {
			t.Fatal("lost a ref")
		}
	}
	look := time.Since(start)
	if fs := refs.Feeds(hashOfString("common")); len(fs) != 100 {
		t.Fatalf("common blob in %d feeds", len(fs))
	}
	t.Logf("100k entries: index %v, lookups %v", add, look)
	if add > 30*time.Second || look > 30*time.Second { // catches quadratic behaviour, not jitter (-race is slow)
		t.Fatal("blob ref index too slow")
	}
}

func TestIncomingReplicates(t *testing.T) {
	fx := newFixture(t)
	dir := t.TempDir()
	laptop, phone := fx.open(fx.laptop, dir), fx.open(fx.phone, t.TempDir())
	bob, _ := identity.Create(t.TempDir(), "Bob", "pc", time.Now())
	bobID := identity.ID(bob.Person())

	// An incoming.json from an older build is folded into the feed.
	old, _ := json.Marshal(map[string]map[string]string{bobID: {"notes/a": ModeRead}})
	if err := os.WriteFile(filepath.Join(dir, "incoming.json"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	ls, err := NewShares(laptop, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ls.Incoming(bobID, "notes", "a") != ModeRead {
		t.Fatal("incoming.json not migrated")
	}
	if _, err := os.Stat(filepath.Join(dir, "incoming.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("incoming.json left behind")
	}
	ps, err := NewShares(phone, nil)
	if err != nil {
		t.Fatal(err)
	}
	sub := phone.Bus().Subscribe(100)
	defer sub.Close()
	if err := ls.SetIncoming(bobID, map[string]string{"notes/b": ModeWrite}, 0); err != nil {
		t.Fatal(err)
	}
	copyFeeds(t, laptop, phone)
	if ps.Incoming(bobID, "notes", "b") != ModeWrite || ps.Incoming(bobID, "notes", "a") != "" {
		t.Fatalf("phone sees %v", ps.IncomingAll(bobID))
	}
	// The phone announces the change on its own bus.
	got := map[string]bool{}
	for !got[bobID+"/notes/btrue"] || !got[bobID+"/notes/afalse"] {
		select {
		case ev := <-sub.C():
			if ev.Type == EventFeed {
				got[ev.Feed+fmt.Sprint(ev.Readable)] = true
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("events %v", got)
		}
	}
	// Setting the same thing again writes nothing.
	before := len(readAll(t, phone, Address(identity.ID(fx.laptop.Person()), InternalApp, IncomingFeed)))
	if err := ps.SetIncoming(bobID, map[string]string{"notes/b": ModeWrite}, 0); err != nil {
		t.Fatal(err)
	}
	if after := len(readAll(t, phone, Address(identity.ID(fx.laptop.Person()), InternalApp, IncomingFeed))); after != before {
		t.Fatal("an unchanged SetIncoming appended")
	}
}
