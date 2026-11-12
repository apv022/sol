// Package transport defines the Transport interface (a pipe that can carry
// connections to and from the node) and a Registry of enabled transports.
//
// Implementations:
//
//	tor.go       Tor onion services (control port) and SOCKS5 dialing.
//	wg.go        Direct mode over WireGuard, with NAT hole punching (stun.go).
//	direct.go    Direct mode over TLS, for networks that block UDP.
//	clearnet.go  Plain TCP for LAN and testing. Not encrypted or anonymous.
//	i2p.go       I2P via i2pd. Registered stub, see docs/EXTENDING.md.
//
// A transport "publishes" a local target address (host:port on 127.0.0.1)
// under a stable ID and returns the public address that reaches it. The node
// publishes its own listener as ID "node" and each public service under its
// own ID, so public services never share an address with the node.
//
// To add a transport, implement Transport, register it in node.buildTransports,
// and add a config toggle. If it can restrict who reaches a publication, also
// implement AccessIssuer and ClientAuthorizer. Nothing else needs to change:
// peers' addresses and credentials are maps keyed by transport name
// (docs/PEERS.md).
package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"sync"
)

// ErrNotImplemented is returned by transports and features that are
// registered but not built yet (I2P in the MVP).
var ErrNotImplemented = errors.New("not implemented yet")

// State is a transport's lifecycle state.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting" // process launching or bootstrapping
	StateReady    State = "ready"    // published addresses are reachable
	StateFailed   State = "failed"
)

// Status is a snapshot for the panel.
type Status struct {
	Name      string            `json:"name"`
	State     State             `json:"state"`
	Detail    string            `json:"detail,omitempty"`    // e.g. "bootstrapping 80%"
	Addresses map[string]string `json:"addresses,omitempty"` // publication ID -> public address
}

// Publication describes one thing to expose through a transport.
type Publication struct {
	// ID is a stable name ("node", "svc-blog"). Transports that derive
	// addresses from keys (Tor, I2P) persist one key per ID, so the public
	// address survives restarts.
	ID string
	// Target is the local host:port that inbound connections are forwarded to.
	Target string
	// ClientAuth lists the Access.Authorize values this transport issued
	// (for Tor: client-auth public keys, base32 x25519). If non-empty, only
	// holders of a matching Access.Secret can even find the publication.
	// Transports without such a feature ignore it.
	ClientAuth []string
}

// Transport is one pipe.
type Transport interface {
	// Name returns the transport's name: "tor", "wg", "direct", "i2p", or "clearnet".
	Name() string
	// Start launches the transport and returns once it can accept Publish
	// calls. It may keep bootstrapping in the background (see Status).
	Start(ctx context.Context) error
	// Publish exposes p.Target and returns its public address. Publishing an
	// ID again replaces the earlier publication (same address).
	Publish(ctx context.Context, p Publication) (string, error)
	// Unpublish removes a publication. Unknown IDs are not an error.
	Unpublish(id string) error
	// Handles reports whether addr is an address this transport can dial.
	Handles(addr string) bool
	// Dial opens a connection to a public address.
	Dial(ctx context.Context, addr string) (net.Conn, error)
	Status() Status
	Stop() error
}

// Access is one per-peer credential for a transport that can restrict who
// reaches a publication. The issuer keeps Authorize and lists it in
// Publication.ClientAuth; the peer gets Secret and installs it with
// ClientAuthorizer. For Tor these are an x25519 client-auth keypair.
type Access struct {
	Authorize string // kept by the issuer (Tor: x25519 public key, base32)
	Secret    string // handed to the peer (Tor: x25519 private key, base64)
}

// AccessIssuer is implemented by transports that can lock a publication to
// specific peers (Tor client authorization; I2P encrypted leasesets later).
type AccessIssuer interface {
	NewAccess() (Access, error)
}

// ClientAuthorizer is implemented by transports whose dialing side must
// install a credential before it can reach an access-restricted address.
type ClientAuthorizer interface {
	AuthorizeClient(addr, secret string) error
	// DeauthorizeClient forgets the credential for addr. Unknown addresses
	// are not an error.
	DeauthorizeClient(addr string) error
}

// Registry holds the enabled transports. Safe for concurrent use.
type Registry struct {
	mu sync.RWMutex
	ts map[string]Transport
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry { return &Registry{ts: map[string]Transport{}} }

// Add registers t under t.Name(), replacing any previous one.
func (r *Registry) Add(t Transport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ts[t.Name()] = t
}

// Get returns a transport by name.
func (r *Registry) Get(name string) (Transport, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.ts[name]
	return t, ok
}

// List returns the transports sorted by name.
func (r *Registry) List() []Transport {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Transport, 0, len(r.ts))
	for _, t := range r.ts {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// StartAll starts every transport. A failing transport does not stop the
// others; all errors are joined.
func (r *Registry) StartAll(ctx context.Context) error {
	var errs []error
	for _, t := range r.List() {
		if err := t.Start(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// PublishAll publishes p on every transport whose name is in only (all if
// only is empty) and returns name -> address for the ones that succeeded.
func (r *Registry) PublishAll(ctx context.Context, p Publication, only []string) (map[string]string, error) {
	addrs := map[string]string{}
	var errs []error
	for _, t := range r.List() {
		if len(only) > 0 && !slices.Contains(only, t.Name()) {
			continue
		}
		if t.Status().State == StateFailed {
			continue
		}
		a, err := t.Publish(ctx, p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
			continue
		}
		addrs[t.Name()] = a
	}
	return addrs, errors.Join(errs...)
}

// UnpublishAll removes publication id from every transport.
func (r *Registry) UnpublishAll(id string) error {
	var errs []error
	for _, t := range r.List() {
		if err := t.Unpublish(id); err != nil && !errors.Is(err, ErrNotImplemented) {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// Statuses returns a snapshot of every transport, sorted by name.
func (r *Registry) Statuses() []Status {
	var out []Status
	for _, t := range r.List() {
		out = append(out, t.Status())
	}
	return out
}

// For returns the transport that can dial addr. (A stub still matches its
// addresses, so dialing one gives its honest ErrNotImplemented.)
func (r *Registry) For(addr string) (Transport, error) {
	for _, t := range r.List() {
		if t.Handles(addr) {
			return t, nil
		}
	}
	return nil, fmt.Errorf("no enabled transport can reach %q", addr)
}

// Usable reports whether t is a real transport that has not failed (an
// Unavailable placeholder or a failed transport is not).
func Usable(t Transport) bool {
	if _, ok := t.(*Unavailable); ok {
		return false
	}
	return t.Status().State != StateFailed
}

// StopAll stops every transport and joins the errors.
func (r *Registry) StopAll() error {
	var errs []error
	for _, t := range r.List() {
		if err := t.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// IsOnion reports whether addr (host or host:port) is a .onion address.
func IsOnion(addr string) bool { return hasSuffix(addr, ".onion") }

// IsI2P reports whether addr (host or host:port) is a .i2p address.
func IsI2P(addr string) bool { return hasSuffix(addr, ".i2p") }

func hasSuffix(addr, suffix string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	return len(host) > len(suffix) && host[len(host)-len(suffix):] == suffix
}
