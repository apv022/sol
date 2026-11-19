// Package identity implements Sol's layered identity (design/IDENTITY.md):
// the recovery key R (24 words, never stored once the backup is confirmed),
// the person key P (admin devices), device keys D, and scoped, expiring agent
// keys G, together with the signed objects that bind them: person
// certificates, device lists, agent certificates and endpoint records.
//
// Invariants:
//
//   - Every signed object is a canonical byte string of length-prefixed
//     fields (encoding.go), never JSON, and starts with its own context string,
//     so a signature for one kind can never be replayed as another. Decoding
//     and re-encoding is the identity, so objects carry no raw copy.
//   - A person ID is R's public key; a bundle is trusted only if R signed the
//     person certificate, that certificate's P signed the device list, and the
//     presenting device is in that list (VerifyBundle).
//   - Sequence numbers only move forward. A Cache refuses any person
//     certificate, device list or endpoint record older than one it has seen,
//     so a removed device cannot be resurrected by replaying an old list.
//   - Agent certificates always expire, at most MaxAgentTTL after issue.
//   - Key material lives only in a Keyring directory: files 0600, directory
//     0700, written atomically. The recovery entropy is kept only until the
//     person confirms the paper backup. Nothing in this package logs, prints
//     or formats a private key or the entropy.
//
// The package also keeps the small helpers the transports use: node IDs and
// X25519 keypairs for Tor client authorization.
package identity

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strings"
)

// b32 is unpadded base32 (RFC 4648 alphabet); IDs are its lowercase form.
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// IDLen is the length of an ID: 32 bytes of public key in unpadded base32.
const IDLen = 52

// ID returns the canonical ID of an ed25519 public key: lowercase, unpadded
// base32. Person IDs (R), device IDs (D) and agent IDs (G) all use it.
func ID(pub ed25519.PublicKey) string { return strings.ToLower(b32.EncodeToString(pub)) }

// ParseID decodes a canonical ID back into a public key. Only the lowercase
// form is accepted, so every key has exactly one ID.
func ParseID(id string) (ed25519.PublicKey, error) {
	if len(id) != IDLen || strings.ToLower(id) != id {
		return nil, fmt.Errorf("invalid id %q", id)
	}
	b, err := b32.DecodeString(strings.ToUpper(id))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid id %q", id)
	}
	// Reject non-canonical trailing bits (52 chars carry 260 bits for 256).
	if ID(b) != id {
		return nil, fmt.Errorf("invalid id %q", id)
	}
	return ed25519.PublicKey(b), nil
}

// NodeID is ID under the name the transports use for a device key.
func NodeID(pub ed25519.PublicKey) string { return ID(pub) }

// ParseNodeID is ParseID under the name the transports use.
func ParseNodeID(id string) (ed25519.PublicKey, error) { return ParseID(id) }

// X25519Key is a keypair for Tor v3 client authorization.
//
// The onion service is configured with the public half (base32, as Tor's
// ClientAuthV3 expects); the client gets the private half (base64, as Tor's
// ONION_CLIENT_AUTH_ADD expects).
type X25519Key struct {
	Public  []byte // 32 bytes
	Private []byte // 32 bytes
}

// NewX25519 generates a client-authorization keypair.
func NewX25519() (X25519Key, error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return X25519Key{}, fmt.Errorf("generating x25519 key: %w", err)
	}
	return X25519Key{Public: k.PublicKey().Bytes(), Private: k.Bytes()}, nil
}

// TorPublic returns the public key in Tor's ClientAuthV3 format (unpadded base32).
func (k X25519Key) TorPublic() string { return b32.EncodeToString(k.Public) }

// TorPrivate returns the private key in Tor's ONION_CLIENT_AUTH_ADD format (base64).
func (k X25519Key) TorPrivate() string { return base64.StdEncoding.EncodeToString(k.Private) }
