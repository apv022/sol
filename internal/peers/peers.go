// Package peers manages the node's peers: the other Sol nodes it is paired
// with (docs/PEERS.md).
//
// Peers is a thin, concurrency-safe layer over config.Config that adds key
// lookups, rate-limited LastSeen persistence, and change hooks. The node uses
// the hooks to (a) close a removed peer's live connections and (b) refresh
// transport-level access (Tor client auth) so removal also applies at the
// network layer.
package peers

import (
	"crypto/ed25519"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/identity"
)

// lastSeenPersistEvery limits how often LastSeen is written to disk per peer.
const lastSeenPersistEvery = time.Minute

// Built-in service names. User service names can't start with "_".
const (
	// Inbox receives files from peers. Shared with every new peer.
	Inbox = "_inbox"
	// Files is the shared-files page of the files the owner added.
	Files = "_files"
)

// Builtins lists the built-in services in display order.
var Builtins = []string{Inbox, Files}

// DefaultShares is what a new peer may open: only the inbox, so pairing
// never exposes a service by surprise.
func DefaultShares() []string { return []string{Inbox} }

// Errors.
var (
	ErrAmbiguous = errors.New("more than one peer matches")
)

// Peers manages paired peers. Safe for concurrent use.
type Peers struct {
	cfg *config.Config

	mu          sync.Mutex
	lastPersist map[string]time.Time
	onRemove    []func(config.Peer)
	onChange    []func()
}

// New wraps cfg.
func New(cfg *config.Config) *Peers {
	return &Peers{cfg: cfg, lastPersist: map[string]time.Time{}}
}

// OnRemove registers fn to run after a peer is removed and saved.
func (p *Peers) OnRemove(fn func(config.Peer)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onRemove = append(p.onRemove, fn)
}

// OnChange registers fn to run after any peer is added, removed, or has its
// access changed.
func (p *Peers) OnChange(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onChange = append(p.onChange, fn)
}

func (p *Peers) fireChange() {
	p.mu.Lock()
	hooks := slices.Clone(p.onChange)
	p.mu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// Add stores a new peer. Its ID and NodeID are derived from pub; if a peer
// with the same key exists it is replaced (re-pairing).
func (p *Peers) Add(rec config.Peer, pub ed25519.PublicKey) (config.Peer, error) {
	if len(pub) != ed25519.PublicKeySize {
		return config.Peer{}, errors.New("invalid public key")
	}
	if err := config.ValidateName(rec.Name); err != nil {
		return config.Peer{}, err
	}
	now := time.Now().UTC()
	rec.ID = identity.DeviceID(pub)
	rec.NodeID = identity.NodeID(pub)
	rec.PubKey = identity.EncodePublicKey(pub)
	rec.Added, rec.LastSeen = now, now
	if rec.Shares == nil {
		rec.Shares = DefaultShares()
	}
	if rec.Services == nil {
		rec.Services = []config.RemoteService{}
	}
	if _, ok := p.cfg.Peer(rec.ID); ok {
		if err := p.Remove(rec.ID); err != nil {
			return config.Peer{}, err
		}
	}
	if err := p.cfg.AddPeer(rec); err != nil {
		return config.Peer{}, err
	}
	p.fireChange()
	return rec, nil
}

// Remove deletes a peer immediately and runs the OnRemove hooks.
func (p *Peers) Remove(id string) error {
	rec, err := p.cfg.DeletePeer(id)
	if err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.lastPersist, id)
	hooks := slices.Clone(p.onRemove)
	p.mu.Unlock()
	for _, fn := range hooks {
		fn(rec)
	}
	p.fireChange()
	return nil
}

// Update changes a stored peer (see config.UpdatePeer) and fires OnChange.
func (p *Peers) Update(id string, fn func(*config.Peer) error) (config.Peer, error) {
	rec, err := p.cfg.UpdatePeer(id, fn)
	if err == nil {
		p.fireChange()
	}
	return rec, err
}

// SetShared adds or removes service from what peer id may open.
func (p *Peers) SetShared(id, service string, shared bool) (config.Peer, error) {
	return p.Update(id, func(rec *config.Peer) error {
		rec.Shares = slices.DeleteFunc(rec.Shares, func(s string) bool { return s == service })
		if shared {
			rec.Shares = append(rec.Shares, service)
			sort.Strings(rec.Shares)
		}
		return nil
	})
}

// Lookup returns the peer that owns pub. This runs on every connection.
func (p *Peers) Lookup(pub ed25519.PublicKey) (config.Peer, bool) {
	if len(pub) != ed25519.PublicKeySize {
		return config.Peer{}, false
	}
	rec, ok := p.cfg.Peer(identity.DeviceID(pub))
	if !ok {
		return config.Peer{}, false
	}
	// The ID is a hash prefix, so confirm the full key matches.
	stored, err := identity.DecodePublicKey(rec.PubKey)
	if err != nil || subtle.ConstantTimeCompare(stored, pub) != 1 {
		return config.Peer{}, false
	}
	return rec, true
}

// Get returns a peer by ID.
func (p *Peers) Get(id string) (config.Peer, bool) { return p.cfg.Peer(id) }

// Find resolves what a user typed: an exact ID, a unique ID prefix, or a
// unique name (case-insensitive).
func (p *Peers) Find(query string) (config.Peer, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return config.Peer{}, fmt.Errorf("no peer given: %w", config.ErrNotFound)
	}
	var byName, byPrefix []config.Peer
	for _, rec := range p.List() {
		if rec.ID == q {
			return rec, nil
		}
		if strings.EqualFold(rec.Name, q) {
			byName = append(byName, rec)
		}
		if strings.HasPrefix(rec.ID, strings.ToLower(q)) {
			byPrefix = append(byPrefix, rec)
		}
	}
	for _, set := range [][]config.Peer{byName, byPrefix} {
		switch len(set) {
		case 0:
		case 1:
			return set[0], nil
		default:
			return config.Peer{}, fmt.Errorf("%q: %w; use the peer id", q, ErrAmbiguous)
		}
	}
	return config.Peer{}, fmt.Errorf("peer %q: %w", q, config.ErrNotFound)
}

// List returns all peers, sorted by name.
func (p *Peers) List() []config.Peer {
	list := p.cfg.PeerList()
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	return list
}

// Touch records that a peer just connected. It is persisted at most once per
// minute per peer; save errors are ignored (LastSeen is cosmetic).
func (p *Peers) Touch(id string) {
	now := time.Now().UTC()
	p.mu.Lock()
	persist := now.Sub(p.lastPersist[id]) >= lastSeenPersistEvery
	if persist {
		p.lastPersist[id] = now
	}
	p.mu.Unlock()
	_ = p.cfg.SetPeerLastSeen(id, now, persist)
}

// AccessKeys returns the credentials we issued to peers on one transport
// (for Tor: the client-auth public keys to put on our onion).
func (p *Peers) AccessKeys(transport string) []string {
	var keys []string
	for _, rec := range p.cfg.PeerList() {
		if k := rec.AccessIn[transport]; k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// Describe is used in log messages: "name (id)".
func Describe(rec config.Peer) string { return fmt.Sprintf("%s (%s)", rec.Name, rec.ID) }
