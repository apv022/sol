package identity

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"math/big"
	"slices"
)

// TorAccessV1 is the Access value of a Tor endpoint whose onion requires client authorization
// with keys derived as in TorAccess (design/PROTOCOL.md §Transports, "Tor client
// authorization"). Nothing secret travels in the record: both sides compute the key.
const TorAccessV1 = "x25519-dh1"

// p25519 is the field prime 2^255 - 19.
var p25519 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))

// montgomeryPublic converts an Ed25519 public key to its X25519 (Montgomery u) form,
// u = (1 + y) / (1 - y) mod p, as libsodium's crypto_sign_ed25519_pk_to_curve25519 does.
func montgomeryPublic(pub ed25519.PublicKey) (*ecdh.PublicKey, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("identity: bad ed25519 public key")
	}
	le := slices.Clone([]byte(pub))
	le[31] &= 0x7f // the sign of x is not needed for u
	slices.Reverse(le)
	y := new(big.Int).SetBytes(le)
	if y.Cmp(p25519) >= 0 {
		return nil, errors.New("identity: non-canonical ed25519 public key")
	}
	den := new(big.Int).Sub(big.NewInt(1), y)
	den.Mod(den, p25519)
	if den.Sign() == 0 {
		return nil, errors.New("identity: ed25519 public key has no x25519 form")
	}
	u := new(big.Int).Add(big.NewInt(1), y)
	u.Mul(u, den.ModInverse(den, p25519))
	u.Mod(u, p25519)
	out := u.FillBytes(make([]byte, 32))
	slices.Reverse(out)
	return ecdh.X25519().NewPublicKey(out)
}

// montgomeryPrivate is the X25519 private key of an Ed25519 key: the clamped scalar is the
// first half of SHA-512(seed), exactly the scalar Ed25519 signs with, so its public key is
// montgomeryPublic of the Ed25519 public key.
func montgomeryPrivate(k ed25519.PrivateKey) (*ecdh.PrivateKey, error) {
	h := sha512.Sum512(k.Seed())
	return ecdh.X25519().NewPrivateKey(h[:32]) // X25519 clamps the scalar itself
}

// torAccess derives the client-authorization keypair for client reaching owner's onion
// from the X25519 shared secret of the two device keys, mine being priv.
func torAccess(priv ed25519.PrivateKey, owner, client ed25519.PublicKey) (X25519Key, error) {
	other := owner
	if bytes.Equal(owner, priv.Public().(ed25519.PublicKey)) {
		other = client
	} else if !bytes.Equal(client, priv.Public().(ed25519.PublicKey)) {
		return X25519Key{}, errors.New("identity: tor access needs this device on one side")
	}
	mine, err := montgomeryPrivate(priv)
	if err != nil {
		return X25519Key{}, err
	}
	theirs, err := montgomeryPublic(other)
	if err != nil {
		return X25519Key{}, err
	}
	shared, err := mine.ECDH(theirs) // refuses an all-zero result (a low-order key)
	if err != nil {
		return X25519Key{}, err
	}
	h := sha256.New()
	h.Write([]byte("sol tor access v1"))
	h.Write(owner)
	h.Write(client)
	h.Write(shared)
	return X25519FromSeed(h.Sum(nil))
}

// X25519FromSeed turns 32 secret bytes into a client-authorization keypair.
func X25519FromSeed(seed []byte) (X25519Key, error) {
	k, err := ecdh.X25519().NewPrivateKey(seed)
	if err != nil {
		return X25519Key{}, err
	}
	return X25519Key{Public: k.PublicKey().Bytes(), Private: k.Bytes()}, nil
}

// TorAccess derives the Tor client-authorization keypair that lets device client reach the
// onion of device owner; this device must be one of the two. Each side computes the same key
// from its own private key and the other's public key, so the owner lists the public half on
// its onion and the client installs the private half, and the key never travels. Owner and
// client may be the same device (the key that keeps an onion with nobody else on it closed).
func (k *Keyring) TorAccess(owner, client ed25519.PublicKey) (X25519Key, error) {
	k.mu.Lock()
	priv := k.st.device
	k.mu.Unlock()
	return torAccess(priv, owner, client)
}
