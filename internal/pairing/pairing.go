// Package pairing issues one-time pairing secrets and encodes pairing links.
//
// Flow (docs/PEERS.md):
//  1. The inviter asks for a token. The Manager creates a 32-byte secret that
//     expires after DefaultTTL, together with one access credential per
//     transport that can restrict who reaches the node (Tor client auth).
//  2. The inviter shows a Link (as text and QR). It carries the inviter's node
//     ID, name, addresses, the secret, and the joiner's half of each
//     credential.
//  3. The joiner connects and sends a protocol.OpPair handshake with the
//     secret and its own card. The inviter calls Consume, which validates and
//     burns the token.
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
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/transport"
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
	// Access holds one credential per transport for the joiner. The node
	// authorizes Access[t].Authorize while the token is pending and keeps it
	// as the peer's AccessIn once it is redeemed.
	Access map[string]transport.Access
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
// The node uses it to add or remove pending access keys.
func (m *Manager) OnChange(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = append(m.onChange, fn)
}

func (m *Manager) fire() {
	m.mu.Lock()
	hooks := slices.Clone(m.onChange)
	m.mu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// NewToken creates a single-use token carrying access (which may be empty).
func (m *Manager) NewToken(access map[string]transport.Access) (*Token, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	t := &Token{
		Secret:  base64.RawURLEncoding.EncodeToString(raw),
		Expires: m.now().Add(m.ttl),
		Access:  maps.Clone(access),
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
		cp := *t
		cp.Access = maps.Clone(t.Access)
		out = append(out, cp)
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

// Link is everything another node needs to pair with the inviter.
//
//	sol://pair?node=<id>&name=<name>&s=<secret>&tor=<onion>&clearnet=<host:port>&auth.tor=<key>
//
// Every parameter that isn't one of the fixed ones names a transport: a bare
// "<transport>=" is an address and "auth.<transport>=" a credential, so a new
// transport needs no change here.
type Link struct {
	NodeID string            // required
	Name   string            // the inviter's node name (display only)
	Secret string            // required
	Addrs  map[string]string // transport -> address; at least one
	Access map[string]string // transport -> credential for the joiner (Access.Secret)
}

const authPrefix = "auth."

// fixed are the link parameters that are not transport names.
var fixed = []string{"v", "node", "name", "s"}

// String encodes the link.
func (l Link) String() string {
	q := url.Values{}
	q.Set("node", l.NodeID)
	if l.Name != "" {
		q.Set("name", l.Name)
	}
	for t, a := range l.Addrs {
		if a != "" {
			q.Set(t, a)
		}
	}
	for t, k := range l.Access {
		if k != "" {
			q.Set(authPrefix+t, k)
		}
	}
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
	l := Link{
		NodeID: q.Get("node"),
		Name:   q.Get("name"),
		Secret: q.Get("s"),
		Addrs:  map[string]string{},
		Access: map[string]string{},
	}
	for k, vs := range q {
		if len(vs) == 0 || vs[0] == "" || slices.Contains(fixed, k) {
			continue
		}
		if t, ok := strings.CutPrefix(k, authPrefix); ok {
			l.Access[t] = vs[0]
		} else {
			l.Addrs[k] = vs[0]
		}
	}
	if _, err := identity.ParseNodeID(l.NodeID); err != nil {
		return Link{}, err
	}
	if l.Secret == "" {
		return Link{}, errors.New("pairing link has no secret")
	}
	if len(l.Addrs) == 0 {
		return Link{}, errors.New("pairing link has no address")
	}
	return l, nil
}
