// Package identity holds the minimal key types the transports need: node IDs
// (base32 ed25519 public keys) and X25519 keypairs for Tor client auth.
//
// TODO(C3): this is a trimmed copy of the PoC identity package; the full node
// identity (signing, persistence, device IDs) is rebuilt in C3 and should
// replace or absorb this.
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

// b32 is lowercase-friendly unpadded base32 (RFC 4648 alphabet).
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NodeID is the lowercase base32 form of a node's ed25519 public key.
func NodeID(pub ed25519.PublicKey) string { return strings.ToLower(b32.EncodeToString(pub)) }

// ParseNodeID decodes a node ID back into a public key.
func ParseNodeID(id string) (ed25519.PublicKey, error) {
	b, err := b32.DecodeString(strings.ToUpper(id))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid node id %q", id)
	}
	return ed25519.PublicKey(b), nil
}

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
