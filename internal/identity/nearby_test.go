package identity

import (
	"testing"
	"time"
)

// Both devices of a pair derive the same nearby key; another pair (sharing one device) gets a
// different one, and a device has none with itself.
func TestNearbyKey(t *testing.T) {
	mk := func() *Keyring {
		k, err := NewDevice(t.TempDir(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	a, b, c := mk(), mk(), mk()
	ab, err := a.NearbyKey(b.DevicePublic())
	if err != nil {
		t.Fatal(err)
	}
	ba, err := b.NearbyKey(a.DevicePublic())
	if err != nil {
		t.Fatal(err)
	}
	if ab != ba {
		t.Fatal("the two sides derived different keys")
	}
	ac, _ := a.NearbyKey(c.DevicePublic())
	cb, _ := c.NearbyKey(b.DevicePublic())
	if ac == ab || cb == ab || ac == cb {
		t.Fatal("different pairs share a key")
	}
	if _, err := a.NearbyKey(a.DevicePublic()); err == nil {
		t.Fatal("a key with itself")
	}
	if _, err := a.NearbyKey(make([]byte, 31)); err == nil {
		t.Fatal("a short key accepted")
	}
}
