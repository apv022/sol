// Package pairing issues the one-time secrets behind `pair` (add a person)
// and `join` (add a device to my identity), and encodes the links that carry
// them (design/PROTOCOL.md §Pairing links):
//
//	sol://pair?v=2&person=<id>&device=<id>&s=<secret>&e=<endpoints>&n=<name>
//	sol://join?v=2&person=<id>&device=<id>&s=<secret>&e=<endpoints>
//
// Invariants:
//
//   - A secret is 32 random bytes, valid for TTL, and works exactly once:
//     Consume burns it whether or not it was used for the right kind.
//   - Secrets are stored and looked up by SHA-256, so a lookup's timing
//     says nothing about the secret.
//   - A link's endpoint record is the device's own signed record; ParseLink
//     verifies its signature and that it names the link's device, so a
//     tampered link can at worst point at nothing. Who answers is still
//     checked by the handshake against the link's device key.
//
// The handshakes that redeem tokens live in internal/session.
package pairing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// TTL is how long a secret stays valid.
const TTL = 10 * time.Minute

// SecretSize is the size of a secret in bytes.
const SecretSize = 32

// LinkVersion is the link format this build writes and reads.
const LinkVersion = 2

// Kind is what a token or link is for.
type Kind string

// Kinds.
const (
	KindPair Kind = "pair" // add a person: both become contacts
	KindJoin Kind = "join" // add a device to my identity
)

// Errors.
var (
	// ErrInvalidToken is returned for unknown, expired, used or
	// wrong-kind secrets. It never says which.
	ErrInvalidToken = errors.New("pairing: invalid or expired secret")
	// ErrLinkVersion means the link was made by a newer Sol.
	ErrLinkVersion = errors.New("pairing: this link was made by a newer Sol; update Sol here and try again")
	// ErrBadLink means the text is not a valid pair or join link.
	ErrBadLink = errors.New("pairing: not a valid sol://pair or sol://join link")
)

type token struct {
	kind    Kind
	expires time.Time
}

// Tokens holds pending secrets. Safe for concurrent use.
type Tokens struct {
	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[[32]byte]token
}

// NewTokens returns an empty store whose secrets live for TTL.
func NewTokens() *Tokens {
	return &Tokens{ttl: TTL, now: time.Now, m: map[[32]byte]token{}}
}

// New issues a secret of the given kind.
func (t *Tokens) New(kind Kind) ([]byte, time.Time, error) {
	if kind != KindPair && kind != KindJoin {
		return nil, time.Time{}, fmt.Errorf("pairing: unknown kind %q", kind)
	}
	secret := make([]byte, SecretSize)
	if _, err := rand.Read(secret); err != nil {
		return nil, time.Time{}, err
	}
	exp := t.now().Add(t.ttl)
	t.mu.Lock()
	t.purgeLocked()
	t.m[sha256.Sum256(secret)] = token{kind, exp}
	t.mu.Unlock()
	return secret, exp, nil
}

// Consume checks a secret and burns it. It succeeds only for an unexpired
// secret of the given kind; any presented secret that exists is burned.
func (t *Tokens) Consume(kind Kind, secret []byte) error {
	key := sha256.Sum256(secret)
	t.mu.Lock()
	tok, ok := t.m[key]
	delete(t.m, key)
	t.mu.Unlock()
	if !ok || tok.kind != kind || t.now().After(tok.expires) || len(secret) != SecretSize {
		return ErrInvalidToken
	}
	return nil
}

// Pending returns how many unexpired secrets are outstanding.
func (t *Tokens) Pending() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.purgeLocked()
	return len(t.m)
}

func (t *Tokens) purgeLocked() {
	now := t.now()
	for k, tok := range t.m {
		if now.After(tok.expires) {
			delete(t.m, k)
		}
	}
}

// Link is a pair or join invitation.
type Link struct {
	Kind      Kind
	Person    ed25519.PublicKey // the inviter's person ID (R_pub)
	Device    ed25519.PublicKey // the inviter's device: the handshake must reach exactly it
	Secret    []byte
	Endpoints identity.EndpointRecord // the inviter device's signed record
	Name      string                  // the inviter's display name (pair links)
}

var b64 = base64.RawURLEncoding

// String encodes the link.
func (l Link) String() string {
	q := url.Values{}
	q.Set("v", strconv.Itoa(LinkVersion))
	q.Set("person", identity.ID(l.Person))
	q.Set("device", identity.ID(l.Device))
	q.Set("s", b64.EncodeToString(l.Secret))
	q.Set("e", b64.EncodeToString(l.Endpoints.Marshal()))
	if l.Name != "" {
		q.Set("n", l.Name)
	}
	return "sol://" + string(l.Kind) + "?" + q.Encode()
}

// ParseLink decodes and checks a link. The version is checked first, so a
// link from a newer Sol says "update" whatever else it holds.
func ParseLink(s string) (Link, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme != "sol" || (u.Host != string(KindPair) && u.Host != string(KindJoin)) {
		return Link{}, ErrBadLink
	}
	q := u.Query()
	switch v := q.Get("v"); v {
	case strconv.Itoa(LinkVersion):
	default:
		if n, err := strconv.Atoi(v); err == nil && n > LinkVersion {
			return Link{}, fmt.Errorf("%w (link version %d, this Sol reads %d)", ErrLinkVersion, n, LinkVersion)
		}
		return Link{}, fmt.Errorf("%w: unsupported version %q", ErrBadLink, v)
	}
	l := Link{Kind: Kind(u.Host), Name: q.Get("n")}
	if l.Person, err = identity.ParseID(q.Get("person")); err != nil {
		return Link{}, fmt.Errorf("%w: person: %v", ErrBadLink, err)
	}
	if l.Device, err = identity.ParseID(q.Get("device")); err != nil {
		return Link{}, fmt.Errorf("%w: device: %v", ErrBadLink, err)
	}
	if l.Secret, err = b64.DecodeString(q.Get("s")); err != nil || len(l.Secret) != SecretSize {
		return Link{}, fmt.Errorf("%w: bad secret", ErrBadLink)
	}
	raw, err := b64.DecodeString(q.Get("e"))
	if err != nil {
		return Link{}, fmt.Errorf("%w: bad endpoints", ErrBadLink)
	}
	if l.Endpoints, err = identity.ParseEndpointRecord(raw); err != nil {
		return Link{}, fmt.Errorf("%w: endpoints: %v", ErrBadLink, err)
	}
	if err := l.Endpoints.Verify(); err != nil || !bytes.Equal(l.Endpoints.Device, l.Device) {
		return Link{}, fmt.Errorf("%w: endpoints are not signed by the link's device", ErrBadLink)
	}
	if len(l.Endpoints.Endpoints) == 0 {
		return Link{}, fmt.Errorf("%w: no way to reach the device", ErrBadLink)
	}
	return l, nil
}
