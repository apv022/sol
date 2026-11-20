package identity

import (
	"bytes"
	"crypto/ed25519"
	"testing"
	"time"
)

// The X25519 form of an Ed25519 key pair is consistent: the converted private key's public
// key is the converted public key.
func TestMontgomeryConversion(t *testing.T) {
	for range 50 {
		pub, priv, _ := ed25519.GenerateKey(nil)
		x, err := montgomeryPrivate(priv)
		if err != nil {
			t.Fatal(err)
		}
		u, err := montgomeryPublic(pub)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(x.PublicKey().Bytes(), u.Bytes()) {
			t.Fatal("converted keys do not match")
		}
	}
	if _, err := montgomeryPublic(make([]byte, 31)); err == nil {
		t.Fatal("short key accepted")
	}
	one := make([]byte, 32)
	one[0] = 1 // y = 1: the identity point, no u
	if _, err := montgomeryPublic(one); err == nil {
		t.Fatal("y=1 accepted")
	}
}

// Both sides derive the same client-auth key, distinct per pair and per direction; a third
// device cannot derive it for others.
func TestTorAccess(t *testing.T) {
	mk := func() *Keyring {
		k, err := NewDevice(t.TempDir(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	a, b, c := mk(), mk(), mk()
	ab, err := a.TorAccess(a.DevicePublic(), b.DevicePublic())
	if err != nil {
		t.Fatal(err)
	}
	ba, err := b.TorAccess(a.DevicePublic(), b.DevicePublic())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ab.Private, ba.Private) || !bytes.Equal(ab.Public, ba.Public) {
		t.Fatal("the two sides derived different keys")
	}
	rev, _ := b.TorAccess(b.DevicePublic(), a.DevicePublic())
	ac, _ := a.TorAccess(a.DevicePublic(), c.DevicePublic())
	self, err := a.TorAccess(a.DevicePublic(), a.DevicePublic())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []X25519Key{rev, ac, self} {
		if bytes.Equal(k.Public, ab.Public) {
			t.Fatal("keys collide across pairs or directions")
		}
	}
	if _, err := c.TorAccess(a.DevicePublic(), b.DevicePublic()); err == nil {
		t.Fatal("a third device derived someone else's key")
	}
	if len(ab.TorPublic()) != 52 || len(ab.TorPrivate()) != 44 {
		t.Fatalf("tor formats: %q", ab.TorPublic())
	}
}
