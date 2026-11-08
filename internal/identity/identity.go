// Package identity holds Sol's key material:
//
//   - ed25519 keys identify nodes and sign handshakes.
//   - x25519 keys are used for Tor v3 onion client authorization.
//
// A node's ID is the lowercase, unpadded base32 encoding of its ed25519 public
// key. DeviceID is a short fingerprint of a key, used to name peers in the UI.
// Private keys are stored as the 32-byte ed25519 seed in a 0600 file.
//
// Only the standard library is used (crypto/ed25519, crypto/ecdh).
package identity

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// b32 is lowercase-friendly unpadded base32 (RFC 4648 alphabet).
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Identity is an ed25519 keypair.
type Identity struct {
	priv ed25519.PrivateKey
}

// Generate creates a fresh in-memory identity.
func Generate() (*Identity, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating ed25519 key: %w", err)
	}
	return &Identity{priv: priv}, nil
}

// LoadOrCreate reads the key at path, or generates one and writes it (0600,
// parent dir 0700) if the file does not exist.
func LoadOrCreate(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(data) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s: want %d-byte seed, got %d bytes", path, ed25519.SeedSize, len(data))
		}
		return &Identity{priv: ed25519.NewKeyFromSeed(data)}, nil
	case errors.Is(err, os.ErrNotExist):
		id, err := Generate()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		// O_EXCL so two processes racing on first run cannot clobber each other.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("creating key file: %w", err)
		}
		if _, err := f.Write(id.priv.Seed()); err != nil {
			f.Close()
			return nil, err
		}
		return id, f.Close()
	default:
		return nil, fmt.Errorf("reading key: %w", err)
	}
}

// Public returns the ed25519 public key.
func (i *Identity) Public() ed25519.PublicKey { return i.priv.Public().(ed25519.PublicKey) }

// NodeID returns the node ID for this key (see package doc).
func (i *Identity) NodeID() string { return NodeID(i.Public()) }

// DeviceID returns the device ID for this key.
func (i *Identity) DeviceID() string { return DeviceID(i.Public()) }

// Sign signs msg.
func (i *Identity) Sign(msg []byte) []byte { return ed25519.Sign(i.priv, msg) }

// NodeID encodes a public key as a node ID (52 lowercase base32 chars).
func NodeID(pub ed25519.PublicKey) string { return strings.ToLower(b32.EncodeToString(pub)) }

// ParseNodeID decodes a node ID back into a public key.
func ParseNodeID(id string) (ed25519.PublicKey, error) {
	b, err := b32.DecodeString(strings.ToUpper(id))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid node id %q", id)
	}
	return ed25519.PublicKey(b), nil
}

// DeviceID is a 16-character fingerprint: base32(sha256(pub))[:16], lowercase.
// It is used as a stable, URL-safe handle in the panel and API.
func DeviceID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return strings.ToLower(b32.EncodeToString(sum[:]))[:16]
}

// Verify reports whether sig is a valid signature of msg by pub. It returns
// false (never panics) for malformed keys.
func Verify(pub ed25519.PublicKey, msg, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}

// EncodePublicKey returns the standard base64 form used in config and on the wire.
func EncodePublicKey(pub ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(pub) }

// DecodePublicKey parses EncodePublicKey's output.
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid ed25519 public key")
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
