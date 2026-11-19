package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"testing"
)

func TestEncodingGolden(t *testing.T) {
	var e enc
	e.lp([]byte("ab"))
	e.u64(1)
	e.count(2)
	e.str("")
	e.str("é")
	want := "00000002" + "6162" + "0000000000000001" + "0000000000000002" + "00000000" + "00000002c3a9"
	if got := hex.EncodeToString(e.b); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	d := dec{b: e.b}
	if string(d.lp()) != "ab" || d.u64() != 1 || d.count(10) != 2 || d.str() != "" || d.str() != "é" || d.done() != nil {
		t.Fatal("round trip failed")
	}
}

// Expected bytes computed independently (Python struct.pack) from the
// IDENTITY.md grammar.
func TestSignedBodyGolden(t *testing.T) {
	pc := PersonCert{Person: bytes.Repeat([]byte{1}, 32), Seq: 1, Key: bytes.Repeat([]byte{2}, 32), Issued: 2, Name: "A"}
	const wantPC = "00000012736f6c20706572736f6e2d63657274207631000000200101010101010101010101010101010101010101010101010101010101010101000000000000000100000020020202020202020202020202020202020202020202020202020202020202020200000000000000020000000141"
	if got := hex.EncodeToString(pc.Body()); got != wantPC {
		t.Fatalf("person cert body\ngot  %s\nwant %s", got, wantPC)
	}
	dl := DeviceList{Person: bytes.Repeat([]byte{1}, 32), Seq: 7,
		Devices: []Device{{Key: bytes.Repeat([]byte{3}, 32), Name: "pc", Flags: FlagAdmin | FlagAnchor, Added: 9}}}
	const wantDL = "00000012736f6c206465766963652d6c6973742076310000002001010101010101010101010101010101010101010101010101010101010101010000000000000007000000000000000100000020030303030303030303030303030303030303030303030303030303030303030300000002706300000000000000030000000000000009"
	if got := hex.EncodeToString(dl.Body()); got != wantDL {
		t.Fatalf("device list body\ngot  %s\nwant %s", got, wantDL)
	}
}

func TestDecodeRejects(t *testing.T) {
	for name, b := range map[string][]byte{
		"short length":   {0, 0, 0},
		"length too big": {0, 0, 0, 9, 1},
		"over maxField":  {0x7f, 0, 0, 0},
	} {
		d := dec{b: b}
		d.lp()
		if !errors.Is(d.done(), errMalformed) {
			t.Errorf("%s: accepted", name)
		}
	}
	d := dec{b: []byte{0, 0, 0, 0, 0, 0, 0, 1, 0}}
	d.u64()
	if d.done() == nil {
		t.Error("trailing bytes accepted")
	}
	d = dec{b: []byte{0, 0, 0, 0, 0, 0, 0, 5}}
	d.count(4)
	if d.done() == nil {
		t.Error("count above max accepted")
	}
}

func TestIDs(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	id := ID(pub)
	if len(id) != IDLen || id != NodeID(pub) {
		t.Fatalf("bad id %q", id)
	}
	got, err := ParseID(id)
	if err != nil || !bytes.Equal(got, pub) {
		t.Fatalf("ParseID: %v", err)
	}
	if _, err := ParseNodeID(id); err != nil {
		t.Fatal(err)
	}
	upper := []byte(id)
	upper[0] -= 'a' - 'A'
	for _, bad := range []string{"", id[:51], id + "a", string(upper), id[:51] + "1"} {
		if _, err := ParseID(bad); err == nil {
			t.Errorf("ParseID(%q) accepted", bad)
		}
	}
	// Last character with non-zero padding bits decodes to the same key but
	// is not canonical.
	last := []rune(id)
	for _, c := range "abcdefghijklmnopqrstuvwxyz234567" {
		if c != last[51] {
			alt := string(last[:51]) + string(c)
			if k, err := ParseID(alt); err == nil && bytes.Equal(k, pub) {
				t.Errorf("non-canonical id %q accepted", alt)
			}
		}
	}
}
