package identity

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

func key(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pubOf(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

// fixture is one person with R, P and two devices, signed by hand (no Keyring).
type fixture struct {
	r, p, d1, d2 ed25519.PrivateKey
	pc           PersonCert
	dl           DeviceList
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{r: key(t), p: key(t), d1: key(t), d2: key(t)}
	f.pc = PersonCert{Person: pubOf(f.r), Seq: 1, Key: pubOf(f.p), Issued: 1, Name: "Ada"}
	if err := f.pc.Sign(f.r); err != nil {
		t.Fatal(err)
	}
	f.dl = f.list(t, 1, f.d1, f.d2)
	return f
}

func (f *fixture) list(t *testing.T, seq uint64, devs ...ed25519.PrivateKey) DeviceList {
	t.Helper()
	l := DeviceList{Person: pubOf(f.r), Seq: seq}
	for i, d := range devs {
		l.Devices = append(l.Devices, Device{Key: pubOf(d), Name: "dev", Flags: uint64(i % 2), Added: 1})
	}
	if err := l.Sign(f.p); err != nil {
		t.Fatal(err)
	}
	return l
}

func endpoints(t *testing.T, d ed25519.PrivateKey, seq uint64) EndpointRecord {
	t.Helper()
	r := EndpointRecord{Device: pubOf(d), Seq: seq, Endpoints: []Endpoint{{"tor", "x.onion:1", "auth"}}}
	if err := r.Sign(d); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fixture) bundle(t *testing.T, d ed25519.PrivateKey) Bundle {
	return Bundle{Person: f.pc, Devices: f.dl, Endpoints: endpoints(t, d, 1)}
}

func TestVerifyBundleAndRoundTrip(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(t, f.d2)
	dev, err := VerifyBundle(b, pubOf(f.d2))
	if err != nil {
		t.Fatal(err)
	}
	if !dev.Admin() || dev.Anchor() {
		t.Fatalf("flags: %+v", dev)
	}
	b2, err := ParseBundle(b.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b2.Marshal(), b.Marshal()) {
		t.Fatal("round trip changed bytes")
	}
	if _, err := VerifyBundle(b2, pubOf(f.d2)); err != nil {
		t.Fatal(err)
	}
}

// Flipping any single bit of the wire form must make parsing or
// verification fail: every byte is a length, a signed field or a signature.
func TestBundleTamperEveryByte(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(t, f.d1)
	raw := b.Marshal()
	for i := range raw {
		for _, bit := range []byte{0x01, 0x80} {
			mut := append([]byte{}, raw...)
			mut[i] ^= bit
			pb, err := ParseBundle(mut)
			if err != nil {
				continue
			}
			if _, err := VerifyBundle(pb, pubOf(f.d1)); err == nil {
				t.Fatalf("byte %d bit %#x: tampered bundle verified", i, bit)
			}
		}
	}
}

func TestVerifyBundleRejects(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	stranger := key(t)
	cases := map[string]func() (Bundle, ed25519.PublicKey){
		"device not in list": func() (Bundle, ed25519.PublicKey) {
			b := f.bundle(t, stranger)
			return b, pubOf(stranger)
		},
		"list signed by other P": func() (Bundle, ed25519.PublicKey) {
			b := f.bundle(t, f.d1)
			b.Devices.Sig = ed25519.Sign(other.p, b.Devices.Body())
			return b, pubOf(f.d1)
		},
		"list of another person": func() (Bundle, ed25519.PublicKey) {
			b := f.bundle(t, f.d1)
			b.Devices = other.list(t, 1, f.d1) // signed by other's P, names other's R
			b.Person.Key = pubOf(other.p)      // and breaks the person cert sig
			return b, pubOf(f.d1)
		},
		"list of another person, valid sigs": func() (Bundle, ed25519.PublicKey) {
			// R1 certifies a P that other person also uses: the list names R2.
			b := f.bundle(t, f.d1)
			l := DeviceList{Person: pubOf(other.r), Seq: 1, Devices: []Device{{Key: pubOf(f.d1)}}}
			_ = l.Sign(f.p)
			b.Devices = l
			return b, pubOf(f.d1)
		},
		"person cert signed by other R": func() (Bundle, ed25519.PublicKey) {
			b := f.bundle(t, f.d1)
			b.Person.Sig = ed25519.Sign(other.r, b.Person.Body())
			return b, pubOf(f.d1)
		},
		"endpoint record from another device": func() (Bundle, ed25519.PublicKey) {
			b := f.bundle(t, f.d2)
			return b, pubOf(f.d1)
		},
		"endpoint record forged": func() (Bundle, ed25519.PublicKey) {
			b := f.bundle(t, f.d1)
			b.Endpoints.Endpoints[0].Address = "evil.onion:1"
			return b, pubOf(f.d1)
		},
		"missing signature": func() (Bundle, ed25519.PublicKey) {
			b := f.bundle(t, f.d1)
			b.Person.Sig = nil
			return b, pubOf(f.d1)
		},
	}
	for name, mk := range cases {
		b, dev := mk()
		if _, err := VerifyBundle(b, dev); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}

// A signature over one kind of object must not verify as another kind.
func TestWrongContext(t *testing.T) {
	f := newFixture(t)
	// A body identical to a person cert except for its context string.
	pc := f.pc
	var e enc
	e.str("sol person-cert v2")
	e.b = append(e.b, pc.Body()[4+len(ctxPersonCert):]...)
	pc.Sig = ed25519.Sign(f.r, e.b)
	if err := pc.Verify(); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("context not bound: %v", err)
	}
	// A device list's wire form never parses as a person cert or an
	// endpoint record, and vice versa.
	if _, err := ParsePersonCert(f.dl.Marshal()); err == nil {
		t.Error("device list parsed as person cert")
	}
	if _, err := ParseDeviceList(f.pc.Marshal()); err == nil {
		t.Error("person cert parsed as device list")
	}
	ep := endpoints(t, f.d1, 1)
	if _, err := ParseAgentCert(ep.Marshal()); err == nil {
		t.Error("endpoint record parsed as agent cert")
	}
	// Same-length contexts: swap the context bytes in place.
	raw := f.pc.Marshal()
	swapped := bytes.Replace(raw, []byte(ctxPersonCert), []byte(ctxDeviceList), 1)
	if _, err := ParseDeviceList(swapped); err == nil {
		t.Error("swapped context parsed")
	}
}

func TestSignRefusesWrongKey(t *testing.T) {
	f := newFixture(t)
	pc := f.pc
	if err := pc.Sign(f.p); err == nil {
		t.Error("person cert signed by a key other than R")
	}
	ep := EndpointRecord{Device: pubOf(f.d1), Seq: 1}
	if err := ep.Sign(f.d2); err == nil {
		t.Error("endpoint record signed by another device")
	}
	dl := DeviceList{Person: pubOf(f.r), Devices: []Device{{Key: pubOf(f.d1)}, {Key: pubOf(f.d1)}}}
	if err := dl.Sign(f.p); err == nil {
		t.Error("duplicate device accepted")
	}
}

func agentCert(t *testing.T, f *fixture, d ed25519.PrivateKey, issued, expires uint64) AgentCert {
	c := AgentCert{Person: pubOf(f.r), Device: pubOf(d), Agent: pubOf(key(t)), Name: "bot",
		Issued: issued, Expires: expires, Scopes: []string{"files.*", "person:" + ID(pubOf(f.r))}}
	// Signed by hand so invalid lifetimes can be built too.
	c.Sig = ed25519.Sign(d, c.Body())
	return c
}

func TestAgentCert(t *testing.T) {
	f := newFixture(t)
	b := f.bundle(t, f.d1)
	now := uint64(t0.Unix())
	day := uint64(24 * 3600)
	good := agentCert(t, f, f.d1, now, now+30*day)
	if err := VerifyAgent(good, b, t0); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseAgentCert(good.Marshal())
	if err != nil || VerifyAgent(parsed, b, t0) != nil {
		t.Fatalf("round trip: %v", err)
	}
	if err := VerifyAgent(good, b, t0.Add(31*24*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Errorf("expired accepted: %v", err)
	}
	if err := VerifyAgent(good, b, t0.Add(-time.Hour)); !errors.Is(err, ErrExpired) {
		t.Errorf("not-yet-valid accepted: %v", err)
	}
	long := agentCert(t, f, f.d1, now, now+91*day)
	if err := VerifyAgent(long, b, t0); !errors.Is(err, ErrInvalid) {
		t.Errorf("91-day cert accepted: %v", err)
	}
	if err := long.Sign(f.d1); err == nil {
		t.Error("Sign accepted a 91-day lifetime")
	}
	exact := agentCert(t, f, f.d1, now, now+90*day)
	if err := VerifyAgent(exact, b, t0); err != nil {
		t.Errorf("90-day cert refused: %v", err)
	}
	noExpiry := agentCert(t, f, f.d1, now, now)
	if err := VerifyAgent(noExpiry, b, t0); err == nil {
		t.Error("zero-lifetime cert accepted")
	}
	tampered := good
	tampered.Scopes = []string{"*"}
	if err := VerifyAgent(tampered, b, t0); !errors.Is(err, ErrBadSignature) {
		t.Errorf("widened scopes accepted: %v", err)
	}
	stranger := key(t)
	foreign := agentCert(t, f, stranger, now, now+day)
	if err := VerifyAgent(foreign, b, t0); !errors.Is(err, ErrNotInList) {
		t.Errorf("cert from a device not in the list accepted: %v", err)
	}
	otherPerson := newFixture(t)
	wrong := agentCert(t, otherPerson, f.d1, now, now+day)
	if err := VerifyAgent(wrong, b, t0); err == nil {
		t.Error("cert for another person accepted")
	}
	none := agentCert(t, f, f.d1, now, now+day)
	none.Scopes = nil
	none.Sig = ed25519.Sign(f.d1, none.Body())
	if err := VerifyAgent(none, b, t0); err == nil {
		t.Error("cert without scopes accepted")
	}
}

func TestCacheOrdering(t *testing.T) {
	f := newFixture(t)
	c := NewCache()
	b1 := f.bundle(t, f.d1)
	if _, err := c.Accept(b1, pubOf(f.d1)); err != nil {
		t.Fatal(err)
	}
	// The same bundle again is a normal handshake, but not an update.
	if _, err := c.Accept(b1, pubOf(f.d1)); err != nil {
		t.Fatalf("same bundle refused: %v", err)
	}
	if err := c.Update(b1, pubOf(f.d1)); !errors.Is(err, ErrStale) {
		t.Fatalf("equal seq update: %v", err)
	}

	// Remove d2 with list seq 2.
	old := f.bundle(t, f.d2)
	if _, err := c.Accept(old, pubOf(f.d2)); err != nil {
		t.Fatal(err)
	}
	f.dl = f.list(t, 2, f.d1)
	if err := c.Update(f.bundle(t, f.d1), pubOf(f.d1)); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Endpoints(pubOf(f.r), pubOf(f.d2)); ok {
		t.Error("removed device's endpoints still cached")
	}
	// The removed device replays the old list: rollback.
	if _, err := c.Accept(old, pubOf(f.d2)); !errors.Is(err, ErrRollback) {
		t.Fatalf("replayed old list: %v", err)
	}
	// With the new list it is simply not in it.
	if _, err := c.Accept(f.bundle(t, f.d2), pubOf(f.d2)); !errors.Is(err, ErrNotInList) {
		t.Fatalf("removed device: %v", err)
	}
	// Another list with the same seq: equivocation.
	same := f.bundle(t, f.d1)
	same.Devices = f.list(t, 2, f.d1, f.d2)
	if _, err := c.Accept(same, pubOf(f.d1)); !errors.Is(err, ErrConflict) {
		t.Fatalf("equal seq, different list: %v", err)
	}
	// Endpoint records: newer accepted, older refused.
	b := f.bundle(t, f.d1)
	b.Endpoints = endpoints(t, f.d1, 5)
	if err := c.Update(b, pubOf(f.d1)); err != nil {
		t.Fatal(err)
	}
	b.Endpoints = endpoints(t, f.d1, 4)
	if _, err := c.Accept(b, pubOf(f.d1)); !errors.Is(err, ErrRollback) {
		t.Fatalf("older endpoint record: %v", err)
	}
	// An older person cert is a rollback even with a high list seq.
	pc, dl, _ := c.Latest(pubOf(f.r))
	if pc.Seq != 1 || dl.Seq != 2 {
		t.Fatalf("latest: %d %d", pc.Seq, dl.Seq)
	}
	f.pc.Seq = 0
	_ = f.pc.Sign(f.r)
	f.dl = f.list(t, 99, f.d1)
	if _, err := c.Accept(f.bundle(t, f.d1), pubOf(f.d1)); !errors.Is(err, ErrRollback) {
		t.Fatalf("older person cert: %v", err)
	}
	c.Forget(pubOf(f.r))
	if _, _, ok := c.Latest(pubOf(f.r)); ok {
		t.Fatal("Forget kept state")
	}
}
