package session

import (
	"context"
	"crypto/ed25519"
	"sync"
)

// Relation is how a verified peer relates to this device's person.
type Relation string

// Relations.
const (
	Self    Relation = "self"    // another device of my own person
	Contact Relation = "contact" // a person I paired with
)

// Peer is the verified other side of a session. Every handler serving a
// peer request can read it with PeerFrom.
type Peer struct {
	Person   ed25519.PublicKey // R_pub
	Device   ed25519.PublicKey // D_pub, proven in the handshake
	Relation Relation
}

type peerKey struct{}

func withPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

// PeerFrom returns the verified peer of a request served over a session.
func PeerFrom(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(peerKey{}).(Peer)
	return p, ok
}

// Contacts is the minimal view of the people this device has paired with.
// The full store (names, sharing, replication to my other devices) is a
// later ticket; the session layer only needs to ask and to add.
type Contacts interface {
	IsContact(person ed25519.PublicKey) bool
	// AddContact records a person after a successful pair handshake. name is
	// the name in their (verified) person certificate.
	AddContact(person ed25519.PublicKey, name string) error
}

// MemContacts is an in-memory Contacts, for tests and until the real store
// exists. Safe for concurrent use.
type MemContacts struct {
	mu sync.Mutex
	m  map[string]string
}

// NewMemContacts returns an empty store.
func NewMemContacts() *MemContacts { return &MemContacts{m: map[string]string{}} }

// IsContact reports whether person was added.
func (c *MemContacts) IsContact(person ed25519.PublicKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.m[string(person)]
	return ok
}

// AddContact adds or renames person.
func (c *MemContacts) AddContact(person ed25519.PublicKey, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[string(person)] = name
	return nil
}

// Name returns the stored name of person.
func (c *MemContacts) Name(person ed25519.PublicKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.m[string(person)]
	return n, ok
}
