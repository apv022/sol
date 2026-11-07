// Package pairing issues one-time pairing secrets and encodes pairing links.
//
// Flow (spec.md 7.3):
//  1. The panel asks for a token. The Manager creates a 32-byte secret that
//     expires after DefaultTTL and, when Tor is in use, an x25519 keypair for
//     Tor client authorization.
//  2. The panel shows a Link (as text and QR). It carries the node ID, its
//     addresses, the secret, and the Tor client-auth private key.
//  3. The device connects and sends a protocol.OpPair handshake with the
//     secret. The node calls Consume, which validates and burns the token.
//
// Secrets are looked up by their SHA-256 hash, so lookups do not leak timing
// information about the secret.
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// DefaultTTL is how long a pairing secret stays valid.
const DefaultTTL = 10 * time.Minute

// LinkScheme is the URL scheme of pairing links.
const LinkScheme = "sol"

// ErrInvalidToken is returned for unknown, expired, or already-used secrets.
var ErrInvalidToken = errors.New("invalid or expired pairing token")

// Token is one pending pairing.
type Token struct {
	Secret  string // base64url, 32 random bytes
	Expires time.Time
	// TorAuth is the Tor client-auth keypair for the device, or nil.
	TorAuth *identity.X25519Key
}

// Manager holds pending tokens. Safe for concurrent use.
type Manager struct {
	ttl time.Duration
	now func() time.Time // replaced in tests

	mu       sync.Mutex
	tokens   map[[32]byte]*Token // keyed by sha256(secret)
	onChange []func()
}

// NewManager creates a Manager (ttl <= 0 means DefaultTTL).
func NewManager(ttl time.Duration) *Manager {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Manager{ttl: ttl, now: time.Now, tokens: map[[32]byte]*Token{}}
}

// OnChange registers fn to run when the set of pending tokens changes.
// The node uses it to add or remove pending Tor client-auth keys.
func (m *Manager) OnChange(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = append(m.onChange, fn)
}

func (m *Manager) fire() {
	m.mu.Lock()
	hooks := append([]func(){}, m.onChange...)
	m.mu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// NewToken creates a single-use token. If withTorAuth is true it also
// generates a Tor client-auth keypair for the device.
func (m *Manager) NewToken(withTorAuth bool) (*Token, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	t := &Token{
		Secret:  base64.RawURLEncoding.EncodeToString(raw),
		Expires: m.now().Add(m.ttl),
	}
	if withTorAuth {
		k, err := identity.NewX25519()
		if err != nil {
			return nil, err
		}
		t.TorAuth = &k
	}
	m.mu.Lock()
	m.purgeLocked()
	m.tokens[sha256.Sum256([]byte(t.Secret))] = t
	m.mu.Unlock()
	m.fire()
	return t, nil
}

// Consume validates secret and burns it. Each secret works exactly once.
func (m *Manager) Consume(secret string) (*Token, error) {
	key := sha256.Sum256([]byte(secret))
	m.mu.Lock()
	t, ok := m.tokens[key]
	if ok {
		delete(m.tokens, key)
	}
	m.mu.Unlock()
	if !ok || m.now().After(t.Expires) {
		return nil, ErrInvalidToken
	}
	m.fire()
	return t, nil
}

// Pending returns the unexpired tokens (copies).
func (m *Manager) Pending() []Token {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purgeLocked()
	out := make([]Token, 0, len(m.tokens))
	for _, t := range m.tokens {
		out = append(out, *t)
	}
	return out
}

func (m *Manager) purgeLocked() {
	now := m.now()
	for k, t := range m.tokens {
		if now.After(t.Expires) {
			delete(m.tokens, k)
		}
	}
}

// Link is everything a device needs to pair with a node.
type Link struct {
	NodeID      string // required
	Onion       string // "<56 chars>.onion" or ""
	I2P         string // "<52 chars>.b32.i2p" or "" (roadmap)
	LAN         string // clearnet "host:port" or ""
	Secret      string // required
	TorAuthPriv string // base64 x25519 private key or ""
}

// String encodes the link as sol://pair?v=1&node=...&s=...
func (l Link) String() string {
	q := url.Values{}
	q.Set("v", "1")
	q.Set("node", l.NodeID)
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("tor", l.Onion)
	set("i2p", l.I2P)
	set("lan", l.LAN)
	set("auth", l.TorAuthPriv)
	q.Set("s", l.Secret)
	return LinkScheme + "://pair?" + q.Encode()
}

// ParseLink decodes a pairing link.
func ParseLink(s string) (Link, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return Link{}, fmt.Errorf("parsing pairing link: %w", err)
	}
	if u.Scheme != LinkScheme || u.Host != "pair" {
		return Link{}, errors.New("not a sol://pair link")
	}
	q := u.Query()
	if v := q.Get("v"); v != "" && v != "1" {
		return Link{}, fmt.Errorf("unsupported pairing link version %q", v)
	}
	l := Link{
		NodeID:      q.Get("node"),
		Onion:       q.Get("tor"),
		I2P:         q.Get("i2p"),
		LAN:         q.Get("lan"),
		Secret:      q.Get("s"),
		TorAuthPriv: q.Get("auth"),
	}
	if _, err := identity.ParseNodeID(l.NodeID); err != nil {
		return Link{}, err
	}
	if l.Secret == "" {
		return Link{}, errors.New("pairing link has no secret")
	}
	if l.Onion == "" && l.I2P == "" && l.LAN == "" {
		return Link{}, errors.New("pairing link has no address")
	}
	return l, nil
}
