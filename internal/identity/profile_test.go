package identity

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestProfileSignVerifyParse(t *testing.T) {
	k, err := Create(t.TempDir(), "Ada", "laptop", t0)
	if err != nil {
		t.Fatal(err)
	}
	pc := mustBundle(t, k).Person
	pic := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3}
	full, anon, err := k.SignProfile(0, "Ada L.", AvatarHash(pic), t0)
	if err != nil {
		t.Fatal(err)
	}
	if full.Seq != anon.Seq || anon.Avatar != "" || full.Avatar != AvatarHash(pic) || !anon.Anonymous() {
		t.Fatalf("forms: %+v / %+v", full, anon)
	}
	for _, p := range []Profile{full, anon} {
		if err := p.Verify(pc); err != nil {
			t.Fatal(err)
		}
		got, err := ParseProfile(p.Marshal())
		if err != nil || !bytes.Equal(got.Marshal(), p.Marshal()) {
			t.Fatalf("round trip: %v", err)
		}
	}
	// Every byte matters.
	raw := full.Marshal()
	for i := range raw {
		b := bytes.Clone(raw)
		b[i] ^= 1
		if p, err := ParseProfile(b); err == nil && p.Verify(pc) == nil {
			t.Fatalf("flipping byte %d still verifies", i)
		}
	}
	// Without a picture both forms are the same bytes (ed25519 is deterministic).
	f2, a2, _ := k.SignProfile(full.Seq, "Ada", "", t0)
	if !bytes.Equal(f2.Marshal(), a2.Marshal()) || f2.Seq <= full.Seq {
		t.Fatal("no-picture forms differ or seq did not move")
	}
	// Another person's certificate does not verify it, nor does a profile of another context.
	other, _ := Create(t.TempDir(), "Bob", "x", t0)
	if err := full.Verify(mustBundle(t, other).Person); err == nil {
		t.Fatal("verified under another person")
	}
	if _, err := ParseProfile(pc.Marshal()); err == nil {
		t.Fatal("a person certificate parsed as a profile")
	}
	if err := (&Profile{Person: pc.Person, Avatar: "md5:x"}).Sign(nil); err == nil {
		t.Fatal("bad avatar hash signed")
	}
	// A non-admin device cannot sign.
	nd, _ := NewDevice(t.TempDir(), t0)
	dl, _ := k.AddDevice(nd.DevicePublic(), "phone", 0, t0.Add(time.Second))
	if err := nd.Adopt(pc, dl); err != nil {
		t.Fatal(err)
	}
	if _, _, err := nd.SignProfile(0, "x", "", t0); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("non-admin signed: %v", err)
	}
}

func TestProfileOrdering(t *testing.T) {
	k, _ := Create(t.TempDir(), "Ada", "laptop", t0)
	f1, a1, _ := k.SignProfile(0, "Ada", AvatarHash([]byte("x")), t0)
	f2, _, _ := k.SignProfile(f1.Seq, "Ada 2", "", t0)
	cases := []struct {
		cur, next Profile
		want      int
		err       error
	}{
		{f1, f2, 1, nil},
		{f2, f1, 0, ErrRollback},
		{f1, f1, 0, nil},
		{a1, f1, 1, nil}, // the full form replaces the anonymous one of its seq
		{f1, a1, 0, nil}, // the anonymous form adds nothing
	}
	for i, c := range cases {
		got, err := CompareProfile(c.cur, c.next)
		if got != c.want || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
			t.Fatalf("case %d: %d, %v", i, got, err)
		}
	}
	// A different name at the same seq is equivocation.
	p := Profile{Person: f1.Person, Seq: f1.Seq, Name: "Eve"}
	if _, err := CompareProfile(f1, p); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
}

func TestProfileOldPRejected(t *testing.T) {
	k, _ := Create(t.TempDir(), "Ada", "laptop", t0)
	words, _ := k.PendingWords()
	old, _, _ := k.SignProfile(0, "Ada", "", t0)
	if err := old.Verify(mustBundle(t, k).Person); err != nil {
		t.Fatal(err)
	}
	if err := k.RotatePerson(words, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	newPC := mustBundle(t, k).Person
	if err := old.Verify(newPC); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a profile signed by the old P verified under the new certificate: %v", err)
	}
	fresh, _, _ := k.SignProfile(old.Seq, "Ada", "", t0.Add(time.Hour))
	if err := fresh.Verify(newPC); err != nil {
		t.Fatal(err)
	}
}

func TestAvatarType(t *testing.T) {
	for b, want := range map[string]string{
		"\xff\xd8\xff\xe0abc":             "image/jpeg",
		"\x89PNG\r\n\x1a\nabc":            "image/png",
		"RIFF\x00\x00\x00\x00WEBPVP8 abc": "image/webp",
	} {
		if got, err := AvatarType([]byte(b)); err != nil || got != want {
			t.Fatalf("%q: %s %v", b, got, err)
		}
	}
	for _, b := range [][]byte{nil, []byte("GIF89a"), []byte("<svg"), append([]byte{0xff, 0xd8, 0xff}, make([]byte, MaxAvatarSize)...)} {
		if _, err := AvatarType(b); err == nil {
			t.Fatalf("accepted %d bytes starting %q", len(b), b[:min(len(b), 6)])
		}
	}
	h := AvatarHash([]byte("x"))
	if !ValidAvatarHash(h) || ValidAvatarHash("sha256:../../etc") || ValidAvatarHash(h[:20]) {
		t.Fatal("ValidAvatarHash")
	}
}
