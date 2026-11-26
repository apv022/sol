package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
)

// nearbyKey derives the key two devices share for LAN beacons (design/PROTOCOL.md §Nearby):
// SHA-256("sol nearby v1" | lo | hi | X25519(x(D_me), x(D_peer))), lo and hi being the two
// device keys sorted bytewise, so both sides get the same key and nobody else can. It is
// never sent: like the Tor client-auth keys it is derived from the device keys alone, so a
// removed contact's key needs no revocation (its entry is simply no longer sent).
func nearbyKey(priv ed25519.PrivateKey, peer ed25519.PublicKey) ([32]byte, error) {
	var out [32]byte
	me := priv.Public().(ed25519.PublicKey)
	if bytes.Equal(me, peer) {
		return out, errors.New("identity: a nearby key needs another device")
	}
	mine, err := montgomeryPrivate(priv)
	if err != nil {
		return out, err
	}
	theirs, err := montgomeryPublic(peer)
	if err != nil {
		return out, err
	}
	shared, err := mine.ECDH(theirs) // refuses an all-zero result (a low-order key)
	if err != nil {
		return out, err
	}
	lo, hi := me, peer
	if bytes.Compare(lo, hi) > 0 {
		lo, hi = hi, lo
	}
	h := sha256.New()
	h.Write([]byte("sol nearby v1"))
	h.Write(lo)
	h.Write(hi)
	h.Write(shared)
	copy(out[:], h.Sum(nil))
	return out, nil
}

// NearbyKey returns the key this device and peer share for nearby beacons (internal/nearby).
// Both devices compute the same key from their own private key and the other's public key.
func (k *Keyring) NearbyKey(peer ed25519.PublicKey) ([32]byte, error) {
	k.mu.Lock()
	priv := k.st.device
	k.mu.Unlock()
	return nearbyKey(priv, peer)
}
