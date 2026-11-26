package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"
)

func testRelease(t *testing.T, c ed25519.PrivateKey, seq uint64, version string) Release {
	t.Helper()
	sum := sha256.Sum256([]byte("bytes of " + version))
	r := Release{Seq: seq, App: "raypass", Version: version, Time: 1700000000, Notes: "Faster scans.",
		Artifacts: []Artifact{
			{Platform: "linux-amd64", Kind: KindSolApp, SHA256: sum[:], Size: 42, Mirrors: []string{"https://example.org/r.tar"}},
			{Platform: "android", Kind: KindAPK, SHA256: sum[:], Size: 43},
		}}
	if err := r.Sign(c); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReleaseSignVerifyParse(t *testing.T) {
	_, c, _ := ed25519.GenerateKey(nil)
	r := testRelease(t, c, 7, "0.2.0")
	if err := r.Verify(); err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyFrom(c.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if err := r.VerifyFrom(other); err == nil {
		t.Fatal("verified as another channel's")
	}
	raw := r.Marshal()
	got, err := ParseRelease(raw)
	if err != nil || !bytes.Equal(got.Marshal(), raw) || got.Artifacts[0].Mirrors[0] != "https://example.org/r.tar" {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	if got.Artifacts[1].Mirrors != nil || got.Artifacts[0].Hash() != r.Artifacts[0].Hash() {
		t.Fatalf("artifacts: %+v", got.Artifacts)
	}
	// Every byte matters.
	for i := range raw {
		b := bytes.Clone(raw)
		b[i] ^= 1
		if p, err := ParseRelease(b); err == nil && p.Verify() == nil {
			t.Fatalf("flipping byte %d still verifies", i)
		}
	}
	// Signed by another key while naming C: refused.
	_, mallory, _ := ed25519.GenerateKey(nil)
	forged := testRelease(t, mallory, 8, "0.3.0")
	forged.Channel = r.Channel
	if err := forged.Verify(); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("forged: %v", err)
	}
}

func TestReleaseRefusesBadRecords(t *testing.T) {
	_, c, _ := ed25519.GenerateKey(nil)
	sum := sha256.Sum256(nil)
	good := Artifact{Platform: "linux-amd64", Kind: KindSolApp, SHA256: sum[:], Size: 1}
	for name, r := range map[string]Release{
		"no seq":       {App: "raypass", Version: "1", Artifacts: []Artifact{good}},
		"bad app":      {Seq: 1, App: "Ray Pass", Version: "1", Artifacts: []Artifact{good}},
		"no version":   {Seq: 1, App: "raypass", Artifacts: []Artifact{good}},
		"no artifacts": {Seq: 1, App: "raypass", Version: "1"},
		"bad kind":     {Seq: 1, App: "raypass", Version: "1", Artifacts: []Artifact{{Platform: "linux-amd64", Kind: "exe", SHA256: sum[:], Size: 1}}},
		"short hash":   {Seq: 1, App: "raypass", Version: "1", Artifacts: []Artifact{{Platform: "linux-amd64", Kind: KindSolApp, SHA256: sum[:5], Size: 1}}},
		"http mirror":  {Seq: 1, App: "raypass", Version: "1", Artifacts: []Artifact{{Platform: "linux-amd64", Kind: KindSolApp, SHA256: sum[:], Size: 1, Mirrors: []string{"http://x.org/a"}}}},
		"twice":        {Seq: 1, App: "raypass", Version: "1", Artifacts: []Artifact{good, good}},
		"long notes":   {Seq: 1, App: "raypass", Version: "1", Notes: strings.Repeat("x", MaxReleaseNotes+1), Artifacts: []Artifact{good}},
	} {
		if err := r.Sign(c); err == nil {
			t.Errorf("%s: signed", name)
		}
	}
}

func TestReleaseOrdering(t *testing.T) {
	_, c, _ := ed25519.GenerateKey(nil)
	r1, r2 := testRelease(t, c, 1, "0.1.0"), testRelease(t, c, 2, "0.2.0")
	if n, err := CompareRelease(r1, r2); n != 1 || err != nil {
		t.Fatalf("newer: %d %v", n, err)
	}
	if _, err := CompareRelease(r2, r1); !errors.Is(err, ErrRollback) {
		t.Fatalf("older: %v", err)
	}
	if n, err := CompareRelease(r2, r2); n != 0 || err != nil {
		t.Fatalf("same: %d %v", n, err)
	}
	if _, err := CompareRelease(r2, testRelease(t, c, 2, "0.2.1")); !errors.Is(err, ErrConflict) {
		t.Fatalf("same seq, other record: %v", err)
	}
	if s := NextReleaseSeq(5, time.Unix(3, 0)); s != 6 {
		t.Fatalf("next after 5: %d", s)
	}
	if s := NextReleaseSeq(5, time.Unix(1700000000, 0)); s != 1700000000 {
		t.Fatalf("next by time: %d", s)
	}
}
