// Package session keeps this device's authenticated sessions with other
// devices (design/PROTOCOL.md §Sessions, §Liveness and healing): it dials
// peers over the configured transports, answers their handshakes, turns each
// connection into a mux session, and serves every mux stream as one HTTP/1.1
// request in both directions.
//
// Invariants:
//
//   - At most one current session per pair of devices. If both dial at once,
//     both sides keep the session whose caller has the lower device ID; of
//     two dialed by the same side, the newer. So they converge without
//     talking about it.
//   - A replaced session that is still alive drains instead of closing
//     (register, drain): new streams go over the current session, the open
//     ones finish, and it closes when idle or after DrainGrace. Moving a
//     session to a better path never cuts a stream.
//   - A peer is admitted only if its verified person is me (and its device is
//     in my current device list) or a contact (and its device is in the
//     newest list I know for them). Learning a newer list closes the
//     sessions of devices it no longer contains, draining ones included.
//   - Pair and join secrets are burned on first use (internal/pairing).
//   - A kept peer is redialed whenever its session dies, with exponential
//     backoff between RedialMin and RedialMax.
//   - Paths are ranked, dialed hedged, backed off and put on probation as in
//     paths.go; every session records the path it uses and why (Session.Path).
//   - Identity changes are pushed to every session at once, and contacts'
//     bundles are relayed to my other devices (push.go).
//   - Nothing here logs secrets or request bodies.
package session

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/mux"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/transport"
	"github.com/apv022/sol/internal/wire"
)

// Tunables, package variables so tests can shorten them. Set them before
// creating a Manager.
var (
	// RedialMin and RedialMax bound the backoff between redials of a kept
	// peer whose session died (the PoC's values).
	RedialMin = 2 * time.Second
	RedialMax = 5 * time.Minute
	// DialTimeout bounds opening one transport connection (not the
	// handshake, which has wire.HandshakeTimeout).
	DialTimeout = time.Minute
	// MaxPendingHandshakes bounds inbound handshakes in progress; more
	// connections are closed at once.
	MaxPendingHandshakes = 64
)

// maxBundle bounds a fetched bundle body.
const maxBundle = 1 << 20

// Errors.
var (
	ErrClosed     = errors.New("session: manager closed")
	ErrNoRoute    = errors.New("session: no known way to reach that device")
	ErrNotRelated = errors.New("session: not my device and not a contact")
	ErrWrongPeer  = errors.New("session: the device answered for another person")
)

// Config configures a Manager.
type Config struct {
	Keyring  *identity.Keyring
	Cache    *identity.Cache // contacts' identities; nil means a new one
	Contacts Contacts
	// Tokens redeems pair and join secrets; nil refuses both.
	Tokens *pairing.Tokens
	// Transports are tried for each endpoint whose transport name matches.
	Transports []transport.Transport
	// Handler serves peer requests other than the built-in ones (GET
	// /p/bundle). The verified peer is in the request context (PeerFrom).
	Handler http.Handler
	// OnBundle, if set, is called with every verified bundle that told this
	// device something new (in a handshake, a push or a relay), so the caller
	// can persist the identity cache. It must not block.
	OnBundle func(identity.Bundle)
	// OnIdentity, if set, is called whenever this device's own bundle may have changed (its
	// device list, names, endpoints), e.g. to recompute who may reach its onion. It must not
	// block.
	OnIdentity func()
	// Profiles stores my and my contacts' profiles and pictures (profile.go); nil keeps them in
	// memory only.
	Profiles *Profiles
	// OnProfile, if set, is called when the profile (or the picture) of a person, me included,
	// changed here. It must not block.
	OnProfile func(person ed25519.PublicKey)
	// OnSession, if set, is called (in its own goroutine) for every session
	// that comes up and wins the tie-break, e.g. to start feed replication
	// over it. It should return once the session is Retired: long-lived
	// streams then move to the session that replaced it (whose OnSession
	// call starts them again).
	OnSession func(*Session)
	Log       *slog.Logger
}

// Manager runs the sessions of one device. Safe for concurrent use.
type Manager struct {
	cfg      Config
	kr       *identity.Keyring
	cache    *identity.Cache
	profiles *Profiles
	profMu   sync.Mutex // serializes changes to the stored profiles (profile.go)
	log      *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	slots  chan struct{} // inbound handshakes in progress
	// announce wakes the announcer (BundleChanged); probes caps concurrent
	// path probes.
	announce chan struct{}
	probes   chan struct{}

	mu       sync.Mutex
	closed   bool
	sessions map[string]*Session // the current one, by peer device key
	draining map[*Session]bool   // replaced but still carrying streams (drain)
	// drainGrace and drainIdle are DrainGrace and DrainIdle when the manager was created.
	drainGrace, drainIdle time.Duration
	keepers               map[string]*keeper
	endpoints             map[string]identity.EndpointRecord // newest seen per device
	dialing               map[string]*sync.Mutex
	listeners             map[net.Listener]struct{}
	links                 map[string]*link      // path state per peer device (paths.go)
	auth                  clientAuth            // client-auth keys installed for peers' onions (toraccess.go)
	peerAddrs             map[string]string     // "<device>|<transport>" -> address given to a PeerSetter
	nearby                map[string]nearbyHint // LAN addresses from beacons, by device (nearby.go)
	bleHints              map[string]nearbyHint // Bluetooth LE links waiting, by device (nearby.go)

	trMu sync.RWMutex
	ts   []transport.Transport // Config.Transports, replaced by SetTransports
}

// New returns a Manager. Call Serve to accept connections.
func New(cfg Config) (*Manager, error) {
	if cfg.Keyring == nil || cfg.Contacts == nil {
		return nil, errors.New("session: Keyring and Contacts are required")
	}
	if cfg.Cache == nil {
		cfg.Cache = identity.NewCache()
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Profiles == nil {
		cfg.Profiles, _ = NewProfiles("")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg: cfg, kr: cfg.Keyring, cache: cfg.Cache, profiles: cfg.Profiles,
		log:        cfg.Log.With("device", short(cfg.Keyring.DevicePublic())),
		ctx:        ctx,
		cancel:     cancel,
		slots:      make(chan struct{}, MaxPendingHandshakes),
		sessions:   map[string]*Session{},
		draining:   map[*Session]bool{},
		drainGrace: DrainGrace, drainIdle: DrainIdle,
		keepers:   map[string]*keeper{},
		endpoints: map[string]identity.EndpointRecord{},
		dialing:   map[string]*sync.Mutex{},
		listeners: map[net.Listener]struct{}{},
		links:     map[string]*link{},
		peerAddrs: map[string]string{},
		ts:        cfg.Transports,
		announce:  make(chan struct{}, 1),
		probes:    make(chan struct{}, maxProbes),
	}
	m.wg.Add(1)
	go m.announcer()
	return m, nil
}

// goBG runs f in the background unless the manager is closed; Close waits for it.
func (m *Manager) goBG(f func()) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		f()
	}()
	return true
}

func short(k ed25519.PublicKey) string {
	if len(k) != ed25519.PublicKeySize {
		return "?"
	}
	return identity.ID(k)[:8]
}

// Session is one live, authenticated, multiplexed connection to a device.
type Session struct {
	m      *Manager
	mux    *mux.Session
	peer   Peer
	caller ed25519.PublicKey // the device that dialed
	client *http.Client
	srv    *http.Server

	path    PathInfo  // how it was reached
	pathKey string    // the endpoint it was dialed over ("" inbound)
	started time.Time // when it came up
	anon    bool      // it runs over an anonymous transport (Tor, I2P)

	retired    chan struct{} // closed when it stops being current (Retired)
	retireOnce sync.Once
}

// Path says how the session was reached and why that path.
func (s *Session) Path() PathInfo { return s.path }

// Peer returns the verified other side.
func (s *Session) Peer() Peer { return s.peer }

// Caller returns the device key of the side that dialed this session.
func (s *Session) Caller() ed25519.PublicKey { return s.caller }

// Done is closed when the session dies (its connection is gone, and every stream on it).
func (s *Session) Done() <-chan struct{} { return s.mux.Done() }

// Retired is closed when the session stops being the device's current one: a newer session
// replaced it (it lost the tie-break, or moved to a better path), or it died. A retired
// session that is still alive is draining (PROTOCOL.md §Sessions): the streams open on it
// carry on, new ones go over the current session. Owners of long-lived streams (feed
// replication) stop at Retired and start again on the new session.
func (s *Session) Retired() <-chan struct{} { return s.retired }

func (s *Session) retire() { s.retireOnce.Do(func() { close(s.retired) }) }

// Alive reports whether the session is still up.
func (s *Session) Alive() bool { return s.mux.Err() == nil }

// Close closes the session at once, streams and all. Closing a session that is not draining
// (the current one, or one just taken out of the manager to cut a device off) also closes
// the sessions to that device that are draining: whoever closes a device's session means to
// stop talking to it.
func (s *Session) Close() error {
	s.mux.Close()
	m := s.m
	m.mu.Lock()
	if m.draining[s] {
		m.mu.Unlock()
		return nil
	}
	old := m.drainingLocked(s.peer.Device)
	m.mu.Unlock()
	for _, o := range old {
		o.mux.Close()
	}
	return nil
}

// drainingLocked returns the draining sessions to device. Called with m.mu held.
func (m *Manager) drainingLocked(device ed25519.PublicKey) []*Session {
	var out []*Session
	for d := range m.draining {
		if bytes.Equal(d.peer.Device, device) {
			out = append(out, d)
		}
	}
	return out
}

// Open opens a raw stream to the peer: over this session while it is current, over the
// current one once it is retired (so a holder of an old handle never pins new work to a
// draining session). With no live current session it falls back on this one.
func (s *Session) Open(ctx context.Context) (net.Conn, error) {
	select {
	case <-s.retired:
		if cur, ok := s.m.Session(s.peer.Device); ok && cur != s {
			return cur.mux.Open(ctx)
		}
	default:
	}
	return s.mux.Open(ctx)
}

// HTTPClient returns a client that sends each request over a new stream of
// this session. The URL's host is ignored; use e.g. "http://peer/p/bundle".
func (s *Session) HTTPClient() *http.Client { return s.client }

// Serve accepts connections on ln (the local target the transports publish)
// until ln is closed or the Manager is.
func (m *Manager) Serve(ln net.Listener) error { return m.serve(ln, false, "") }

// ServeVia is Serve for a listener only one transport forwards to (the Bluetooth LE bridge):
// inbound sessions accepted here say so in PathInfo.Via.
func (m *Manager) ServeVia(ln net.Listener, via string) error { return m.serve(ln, false, via) }

// ServeAnonymous is Serve for the local target of the anonymous transports (the onion): only
// connections accepted here may carry a session with an anonymous contact.
func (m *Manager) ServeAnonymous(ln net.Listener) error { return m.serve(ln, true, "") }

func (m *Manager) serve(ln net.Listener, anon bool, via string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		ln.Close()
		return ErrClosed
	}
	m.listeners[ln] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.listeners, ln)
		m.mu.Unlock()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if m.ctx.Err() != nil {
				return ErrClosed
			}
			return err
		}
		select {
		case m.slots <- struct{}{}:
		default:
			conn.Close() // too many handshakes at once: say nothing
			continue
		}
		go func() {
			defer func() { <-m.slots }()
			m.answer(conn, anon, via)
		}()
	}
}

// Close stops every keeper, listener and session.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	var all []*Session
	for _, s := range m.sessions {
		all = append(all, s)
	}
	for s := range m.draining {
		all = append(all, s)
	}
	for ln := range m.listeners {
		ln.Close()
	}
	m.mu.Unlock()
	for _, s := range all {
		s.Close()
	}
	m.wg.Wait()
	return nil
}

// Session returns the live session with a device.
func (m *Manager) Session(device ed25519.PublicKey) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[string(device)]
	if !ok || !s.Alive() {
		return nil, false
	}
	return s, true
}

// Sessions returns every live session.
func (m *Manager) Sessions() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Session
	for _, s := range m.sessions {
		if s.Alive() {
			out = append(out, s)
		}
	}
	return out
}

// NoteEndpoints records a device's endpoint record (from a link, a bundle or
// a push) if it is valid and newer than the one known.
func (m *Manager) NoteEndpoints(rec identity.EndpointRecord) {
	if rec.Verify() != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.endpoints[string(rec.Device)]; ok && old.Seq >= rec.Seq {
		return
	}
	m.endpoints[string(rec.Device)] = rec
	var person ed25519.PublicKey
	if k := m.keepers[string(rec.Device)]; k != nil {
		person = k.person
	}
	if person != nil {
		m.goBGLocked(func() { m.syncPeer(person, rec.Device) })
	}
}

// goBGLocked is goBG with m.mu held.
func (m *Manager) goBGLocked(f func()) {
	if m.closed {
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		f()
	}()
}

// endpointsFor returns where device of person may be dialed: its newest endpoint record,
// reduced to the anonymous transports for an anonymous contact.
func (m *Manager) endpointsFor(person, device ed25519.PublicKey) (identity.EndpointRecord, bool) {
	m.mu.Lock()
	rec, ok := m.endpoints[string(device)]
	m.mu.Unlock()
	if c, cok := m.cache.Endpoints(person, device); cok && (!ok || c.Seq > rec.Seq) {
		rec, ok = c, true
	}
	if ok {
		rec = m.withNearby(rec)
	}
	if ok && m.Anonymous(person) {
		rec = onlyAnonymous(rec)
	}
	return rec, ok
}

// myBundle returns this device's current bundle in wire form.
func (m *Manager) myBundle() ([]byte, error) {
	b, err := m.kr.Bundle()
	if err != nil {
		return nil, err
	}
	return b.Marshal(), nil
}

// admit decides whether a verified device may hold a session with us (in
// either direction) and folds its bundle into what we know. anon says the connection runs
// over an anonymous transport; an anonymous contact is refused on any other.
func (m *Manager) admit(b identity.Bundle, dev ed25519.PublicKey, anon bool) (Peer, error) {
	person := b.Person.Person
	me := m.kr.Person()
	if me == nil {
		return Peer{}, identity.ErrNoPerson
	}
	if bytes.Equal(person, me) {
		// It may know a newer list than we do (another admin changed it).
		switch err := m.kr.Adopt(b.Person, b.Devices); {
		case err == nil:
			m.prune(me)
			m.BundleChanged()
		case errors.Is(err, identity.ErrNotInList):
			return Peer{}, fmt.Errorf("its device list no longer contains this device: %w", err)
		}
		mine, err := m.kr.Bundle()
		if err != nil {
			return Peer{}, err
		}
		if _, ok := mine.Devices.Find(dev); !ok {
			return Peer{}, fmt.Errorf("own person, %w", identity.ErrNotInList)
		}
		m.NoteEndpoints(b.Endpoints)
		m.noteBundle(b)
		return Peer{Person: person, Device: dev, Relation: Self}, nil
	}
	if !m.cfg.Contacts.IsContact(person) {
		return Peer{}, ErrNotRelated
	}
	if !anon && m.Anonymous(person) {
		return Peer{}, ErrNotAnonymousPath
	}
	changed, err := m.foldContact(b, dev)
	if err != nil {
		return Peer{}, err
	}
	m.NoteEndpoints(b.Endpoints)
	m.noteBundle(b)
	if changed {
		m.relay(b)
	}
	return Peer{Person: person, Device: dev, Relation: Contact}, nil
}

// foldContact checks a contact's bundle against the cache. A device that
// presents an older (but valid) list is still admitted while the newest
// list we know contains it: it just hasn't caught up.
// changed reports news (to relay to my other devices).
func (m *Manager) foldContact(b identity.Bundle, dev ed25519.PublicKey) (changed bool, err error) {
	_, changed, err = m.cache.Fold(b, dev)
	if err == nil {
		m.prune(b.Person.Person)
		return changed, nil
	}
	if !errors.Is(err, identity.ErrRollback) {
		return false, err
	}
	_, dl, ok := m.cache.Latest(b.Person.Person)
	if !ok {
		return false, err
	}
	if _, in := dl.Find(dev); !in {
		return false, fmt.Errorf("contact device: %w", identity.ErrNotInList)
	}
	return false, nil
}

// listed reports whether device is in the newest list known for person.
func (m *Manager) listed(person, device ed25519.PublicKey) bool {
	if me := m.kr.Person(); me != nil && bytes.Equal(person, me) {
		b, err := m.kr.Bundle()
		if err != nil {
			return false
		}
		_, ok := b.Devices.Find(device)
		return ok
	}
	_, dl, ok := m.cache.Latest(person)
	if !ok {
		return false
	}
	_, ok = dl.Find(device)
	return ok
}

// prune closes sessions and stops keepers for devices of person that the
// newest known list no longer contains.
func (m *Manager) prune(person ed25519.PublicKey) {
	m.mu.Lock()
	var drop []*Session
	var stop []*keeper
	for key, s := range m.sessions {
		if bytes.Equal(s.peer.Person, person) && !m.listed(person, s.peer.Device) {
			drop = append(drop, s)
			delete(m.sessions, key)
		}
	}
	for s := range m.draining {
		if bytes.Equal(s.peer.Person, person) && !m.listed(person, s.peer.Device) {
			drop = append(drop, s)
		}
	}
	for key, k := range m.keepers {
		if bytes.Equal(k.person, person) && !m.listed(person, k.device) {
			stop = append(stop, k)
			delete(m.keepers, key)
		}
	}
	m.mu.Unlock()
	for _, s := range drop {
		m.log.Info("closing session: device removed from its person's list", "peer", short(s.peer.Device))
		s.Close()
	}
	for _, k := range stop {
		k.cancel()
		m.syncPeer(k.person, k.device)
	}
}

// RemoveDevice removes one of my devices (admin only), closes its sessions
// and stops redialing it. It returns the new device list.
func (m *Manager) RemoveDevice(device ed25519.PublicKey) (identity.DeviceList, error) {
	dl, err := m.kr.RemoveDevice(device, time.Now())
	if err != nil {
		return dl, err
	}
	m.prune(m.kr.Person())
	m.BundleChanged()
	return dl, nil
}

// answer runs the callee side of one inbound connection.
func (m *Manager) answer(conn net.Conn, anon bool, via string) {
	var peer Peer
	var recovered []byte
	decide := func(r *wire.Request) ([]byte, error) {
		if r.Op == wire.OpRecover {
			if !anon && m.Anonymous(r.Person) {
				return nil, ErrNotAnonymousPath
			}
			var err error
			if recovered, err = m.recoverable(r.Person); err != nil {
				return nil, err
			}
			return m.bundleFor(r.Person)
		}
		var err error
		peer, err = m.decide(r, anon)
		if err != nil {
			return nil, err
		}
		return m.bundleFor(peer.Person)
	}
	req, err := wire.Answer(conn, m.kr, decide)
	if err != nil {
		if errors.Is(err, wire.ErrNoData) {
			m.log.Debug("connection closed before a handshake")
		} else {
			m.log.Info("rejected a connection", "reason", err.Error())
		}
		return
	}
	if req.Op == wire.OpRecover {
		conn.SetWriteDeadline(time.Now().Add(wire.HandshakeTimeout))
		_, _ = conn.Write(recovered)
		conn.Close()
		m.log.Info("answered a recovery request", "person", short(req.Person))
		return
	}
	if req.Op == wire.OpProbe {
		conn.Close() // the path works; the live session stays as it is
		return
	}
	if req.Op == wire.OpJoin {
		m.BundleChanged() // the device list grew: tell everyone
	}
	s := m.start(conn, false, peer, req.Device, PathInfo{Inbound: true, Via: via, Reason: "the peer dialed this device"}, "", anon)
	if s != nil {
		m.Keep(peer.Person, peer.Device)
	}
}

// decide is the callee's policy for each op (PROTOCOL.md §Callee decision). anon says the
// connection came over an anonymous transport.
func (m *Manager) decide(r *wire.Request, anon bool) (Peer, error) {
	switch r.Op {
	case wire.OpSession, wire.OpProbe:
		return m.admit(r.Bundle, r.Device, anon)
	case wire.OpPair:
		person := r.Bundle.Person.Person
		if me := m.kr.Person(); me == nil || bytes.Equal(person, me) {
			return Peer{}, errors.New("pair from my own person or before I have one")
		}
		if m.cfg.Tokens == nil {
			return Peer{}, errors.New("pairing is off")
		}
		if !anon && m.cfg.Tokens.Anonymous(r.Secret) {
			return Peer{}, ErrNotAnonymousPath // not burned: the link holder may still use Tor
		}
		tok, err := m.cfg.Tokens.Redeem(pairing.KindPair, r.Secret)
		if err != nil {
			return Peer{}, err
		}
		if _, err := m.cache.Accept(r.Bundle, r.Device); err != nil {
			return Peer{}, err
		}
		if ac, ok := m.cfg.Contacts.(AnonymousContacts); ok && tok.Anonymous {
			err = ac.AddAnonymousContact(person, r.Bundle.Person.Name)
		} else if tok.Anonymous {
			err = errors.New("session: this contacts store cannot keep anonymous contacts")
		} else {
			err = m.cfg.Contacts.AddContact(person, r.Bundle.Person.Name)
		}
		if err != nil {
			return Peer{}, err
		}
		m.NoteEndpoints(r.Bundle.Endpoints)
		m.noteBundle(r.Bundle) // persisted, so a restart before any other handshake still redials
		m.relay(r.Bundle)      // my other devices reach the new contact too (once the contacts feed says so)
		m.log.Info("paired", "person", short(person))
		return Peer{Person: person, Device: r.Device, Relation: Contact}, nil
	case wire.OpJoin:
		if m.cfg.Tokens == nil {
			return Peer{}, errors.New("joining is off")
		}
		if err := m.cfg.Tokens.Consume(pairing.KindJoin, r.Secret); err != nil {
			return Peer{}, err
		}
		if !m.kr.IsAdmin() {
			return Peer{}, identity.ErrNotAdmin
		}
		// The joining device names itself (Request.name, covered by T);
		// an old or nameless joiner gets a placeholder until renamed.
		name := r.Name
		if name == "" {
			name = "device " + short(r.Device)
		}
		if _, err := m.kr.AddDevice(r.Device, name, 0, time.Now()); err != nil {
			return Peer{}, err
		}
		m.NoteEndpoints(r.Endpoints)
		m.log.Info("a new device joined", "peer", short(r.Device))
		return Peer{Person: m.kr.Person(), Device: r.Device, Relation: Self}, nil
	}
	return Peer{}, fmt.Errorf("unknown op %q", r.Op)
}

// start turns a handshaken connection into a registered session. It returns
// the session that won the tie-break (nil if the manager is closed).
func (m *Manager) start(conn net.Conn, weCalled bool, peer Peer, caller ed25519.PublicKey, path PathInfo, pk string, anon bool) *Session {
	var ms *mux.Session
	if weCalled {
		ms = mux.Client(conn)
	} else {
		ms = mux.Server(conn)
	}
	s := &Session{m: m, mux: ms, peer: peer, caller: caller, path: path, pathKey: pk, started: time.Now(), anon: anon,
		retired: make(chan struct{})}
	s.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return s.Open(ctx) // the current session's, once this one is retired
		},
		DisableKeepAlives:  true, // one request per stream
		DisableCompression: true,
	}}
	s.srv = &http.Server{
		Handler:           m.handler(),
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return withPeer(m.ctx, peer) },
		ErrorLog:          slog.NewLogLogger(m.log.Handler(), slog.LevelDebug),
	}
	winner, kept := m.register(s)
	if !kept {
		return winner
	}
	// register counted these goroutines in m.wg. A session that lost the tie-break is served
	// too, while it drains: the peer may have opened streams on it before it learned of the
	// winner.
	go func() {
		defer m.wg.Done()
		s.srv.Serve(ms.Listener())
	}()
	go func() {
		defer m.wg.Done()
		<-ms.Done()
		s.retire()
		s.srv.Close()
		s.client.CloseIdleConnections()
		m.unregister(s)
	}()
	if winner != s {
		go m.drain(s)
		return winner
	}
	m.log.Info("session up", "peer", short(peer.Device), "relation", peer.Relation, "caller", short(caller),
		"path", path.Transport, "why", path.Reason)
	if m.cfg.OnSession != nil {
		go m.cfg.OnSession(s)
	}
	// Tell the peer this device's current bundle. Changes are pushed only to sessions open at the
	// time, and the tie-break for two simultaneous dials retires one session for another, so
	// without this a change made in that gap would never reach the peer (until the next change).
	m.goBG(func() { m.pushMine([]*Session{s}) })
	// Profiles (profile.go): fetched from the peer; a sibling also gets my contacts' bundles,
	// then their profiles, which it verifies against those bundles.
	m.goBG(func() {
		if peer.Relation == Self {
			m.relayAll(s)
		}
		m.syncProfiles(s)
	})
	return s
}

// register installs s as the session with its device, applying the
// tie-break: the session whose caller has the lower device ID wins; between
// two sessions dialed by the same side, the newer one (the older is likely
// dead, or the dialer moved it to a better path). The loser, if alive,
// drains (drain): it is retired, and closes once its streams are done.
// register returns the winner, and whether s was kept (current or draining;
// the caller then serves it). It counts start's goroutines for s in m.wg (3
// for a draining s: drain too).
func (m *Manager) register(s *Session) (winner *Session, kept bool) {
	key := string(s.peer.Device)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		s.mux.Close()
		return nil, false
	}
	old := m.sessions[key]
	if old != nil && old.Alive() {
		oc, nc := identity.ID(old.caller), identity.ID(s.caller)
		if oc < nc {
			m.draining[s] = true
			m.wg.Add(3)
			m.mu.Unlock()
			s.retire()
			m.log.Info("a second session lost the tie-break; draining it", "peer", short(s.peer.Device))
			return old, true
		}
	}
	m.sessions[key] = s
	m.wg.Add(2) // under m.mu, so Close's Wait sees them: start's two goroutines
	drainOld := old != nil && old.Alive()
	if drainOld {
		m.draining[old] = true
		m.wg.Add(1) // drain(old)
	}
	k := m.keepers[key]
	m.mu.Unlock()
	if old != nil {
		old.retire()
	}
	if drainOld {
		go m.drain(old)
	}
	if k != nil {
		k.poke()
	}
	return s, true
}

// Drain tunables (PROTOCOL.md §Sessions). Package variables so tests can shorten them; set
// them before creating a Manager.
var (
	// DrainGrace is the longest a replaced session keeps carrying streams.
	DrainGrace = 5 * time.Minute
	// DrainIdle is how long a replaced session lingers once it has no stream: about the
	// time the peer may take to learn of the new session (a stream it opens on the old one
	// meanwhile still works), and to flush the last close frames.
	DrainIdle = 2 * time.Second
)

// drain closes a retired session once it has had no stream for DrainIdle, or after
// DrainGrace, whichever comes first. register counted it in m.wg.
func (m *Manager) drain(s *Session) {
	defer m.wg.Done()
	grace := time.NewTimer(m.drainGrace)
	defer grace.Stop()
	idle := time.NewTimer(m.drainIdle)
	defer idle.Stop()
	if s.mux.NumStreams() > 0 {
		idle.Stop()
	}
	for {
		select {
		case <-s.mux.Done():
			return
		case <-s.mux.Idle():
			if s.mux.NumStreams() == 0 {
				idle.Reset(m.drainIdle)
			}
		case <-idle.C:
			if s.mux.CloseIfIdle() {
				m.log.Debug("a replaced session drained", "peer", short(s.peer.Device))
				return
			}
		case <-grace.C:
			m.log.Info("closing a replaced session after the grace", "peer", short(s.peer.Device),
				"streams", s.mux.NumStreams())
			s.mux.Close()
			return
		}
	}
}

func (m *Manager) unregister(s *Session) {
	key := string(s.peer.Device)
	m.mu.Lock()
	if m.sessions[key] == s {
		delete(m.sessions, key)
	}
	delete(m.draining, s)
	k := m.keepers[key]
	m.mu.Unlock()
	if k != nil {
		k.poke()
	}
}

func (m *Manager) handler() http.Handler {
	mx := http.NewServeMux()
	mx.HandleFunc("POST "+BundlePath, m.servePush)
	mx.HandleFunc("GET "+ProfilePath, m.serveProfile)
	mx.HandleFunc("POST "+ProfilePath, m.servePushProfile)
	mx.HandleFunc("POST "+ProfileEditPath, m.serveEdit)
	mx.HandleFunc("GET "+AvatarPath+"{hash}", m.serveAvatar)
	mx.HandleFunc("GET "+BundlePath, func(w http.ResponseWriter, r *http.Request) {
		p, _ := PeerFrom(r.Context())
		b, err := m.bundleFor(p.Person)
		if err != nil {
			http.Error(w, "no identity", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(b)
	})
	if m.cfg.Handler != nil {
		mx.Handle("/", m.cfg.Handler)
	}
	return mx
}

// FetchBundle gets the peer's latest bundle over s (GET /p/bundle) and
// verifies it names the session's person and device.
func FetchBundle(ctx context.Context, s *Session) (identity.Bundle, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://peer/p/bundle", nil)
	if err != nil {
		return identity.Bundle{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return identity.Bundle{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return identity.Bundle{}, fmt.Errorf("session: GET /p/bundle: %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBundle))
	if err != nil {
		return identity.Bundle{}, err
	}
	b, err := identity.ParseBundle(raw)
	if err != nil {
		return b, err
	}
	if !bytes.Equal(b.Person.Person, s.peer.Person) {
		return b, ErrWrongPeer
	}
	if _, err := identity.VerifyBundle(b, s.peer.Device); err != nil {
		return b, err
	}
	return b, nil
}

// Refresh pulls the bundle of person from every live session with one of
// its devices and folds in whatever is newest (a device that hasn't caught
// up serves an older one): a newer device list closes the sessions of the
// devices it dropped. This is how a removal reaches contacts until rotations
// are pushed.
func (m *Manager) Refresh(ctx context.Context, person ed25519.PublicKey) error {
	var errs []error
	ok := false
	for _, s := range m.Sessions() {
		if !bytes.Equal(s.peer.Person, person) {
			continue
		}
		b, err := FetchBundle(ctx, s)
		if err == nil {
			err = m.fold(b, s.peer.Device)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ok = true
	}
	m.prune(person)
	switch {
	case ok:
		return nil
	case len(errs) == 0:
		return ErrNoRoute
	}
	return errors.Join(errs...)
}

// fold takes in a newer bundle fetched from device, for me or a contact.
func (m *Manager) fold(b identity.Bundle, device ed25519.PublicKey) error {
	m.NoteEndpoints(b.Endpoints)
	if me := m.kr.Person(); me != nil && bytes.Equal(b.Person.Person, me) {
		if err := m.kr.Adopt(b.Person, b.Devices); err != nil && !errors.Is(err, identity.ErrStale) {
			return err
		}
		return nil
	}
	if err := m.cache.Update(b, device); err != nil && !errors.Is(err, identity.ErrStale) {
		return err
	}
	return nil
}

// NewestBundle asks every reachable known device of person for its bundle
// and returns the newest by (person-cert seq, device-list seq). Use it
// before issuing a new person key or restoring from the words, so the new
// sequence numbers start above everything the world has seen
// (IDENTITY.md, Implementation notes).
func (m *Manager) NewestBundle(ctx context.Context, person ed25519.PublicKey) (identity.Bundle, error) {
	var devices []identity.Device
	if me := m.kr.Person(); me != nil && bytes.Equal(person, me) {
		b, err := m.kr.Bundle()
		if err != nil {
			return identity.Bundle{}, err
		}
		devices = b.Devices.Devices
	} else if _, dl, ok := m.cache.Latest(person); ok {
		devices = dl.Devices
	}
	var best identity.Bundle
	found := false
	var errs []error
	for _, d := range devices {
		if bytes.Equal(d.Key, m.kr.DevicePublic()) {
			continue
		}
		s, err := m.Connect(ctx, person, d.Key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		b, err := FetchBundle(ctx, s)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !found || b.Person.Seq > best.Person.Seq ||
			(b.Person.Seq == best.Person.Seq && b.Devices.Seq > best.Devices.Seq) {
			best, found = b, true
		}
	}
	if !found {
		return best, fmt.Errorf("session: no device of that person answered: %w", errors.Join(append(errs, ErrNoRoute)...))
	}
	return best, nil
}

// Connect returns the live session with device, dialing it if there is
// none. Dials to one device are serialized, so the same side never races
// itself into two sessions.
func (m *Manager) Connect(ctx context.Context, person, device ed25519.PublicKey) (*Session, error) {
	if s, ok := m.Session(device); ok {
		return s, nil
	}
	dm := m.dialMutex(device)
	if dm == nil {
		return nil, ErrClosed
	}
	dm.Lock()
	defer dm.Unlock()
	if s, ok := m.Session(device); ok {
		return s, nil
	}
	rec, ok := m.endpointsFor(person, device)
	if !ok || !bytes.Equal(rec.Device, device) {
		return nil, ErrNoRoute
	}
	conn, res, path, pk, err := m.dial(ctx, rec, wire.Call{Op: wire.OpSession}, person)
	if err != nil {
		return nil, err
	}
	anon := identity.AnonymousTransport(path.Transport)
	peer, err := m.admit(res.Bundle, res.Device, anon)
	if err != nil {
		conn.Close()
		return nil, err
	}
	s := m.start(conn, true, peer, m.kr.DevicePublic(), path, pk, anon)
	if s == nil {
		return nil, ErrClosed
	}
	return s, nil
}

// dialMutex returns the lock that serializes dials to device (nil once closed).
func (m *Manager) dialMutex(device ed25519.PublicKey) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	dm := m.dialing[string(device)]
	if dm == nil {
		dm = &sync.Mutex{}
		m.dialing[string(device)] = dm
	}
	return dm
}

// transportFor returns the first configured transport that can dial ep.
func (m *Manager) transportFor(ep identity.Endpoint) transport.Transport {
	for _, t := range m.transportsNow() {
		if t.Name() == ep.Transport && t.Handles(ep.Address) {
			return t
		}
	}
	return nil
}

// dial reaches rec.Device over its ranked paths, hedged (paths.go), and runs
// the handshake for c (Want and Bundle are filled in here). The callee must
// speak for wantPerson. It returns the connection, the handshake result, why
// that path, and the path's key.
func (m *Manager) dial(ctx context.Context, rec identity.EndpointRecord, c wire.Call, wantPerson ed25519.PublicKey) (net.Conn, *wire.Result, PathInfo, string, error) {
	if c.Op == wire.OpJoin {
		c.Bundle = m.kr.Endpoints().Marshal()
	} else if c.Bundle == nil {
		b, err := m.bundleFor(wantPerson)
		if err != nil {
			return nil, nil, PathInfo{}, "", err
		}
		c.Bundle = b
	}
	c.Want = rec.Device
	paths := m.rank(rec)
	if c.Op == wire.OpPair || c.Op == wire.OpJoin {
		paths = noWG(paths)
	}
	conn, res, path, ep, err := m.dialHedged(ctx, rec.Device, wantPerson, paths, c)
	if err != nil {
		return nil, nil, path, "", err
	}
	return conn, res, path, pathKey(ep), nil
}

// Pair redeems a sol://pair link: the inviter and this person become
// contacts, and a session comes up. An anonymous link makes the inviter an anonymous
// contact here too: it is dialed over Tor only, gets only the onion-only form of my
// bundle (already in this handshake), and is refused on any other transport.
func (m *Manager) Pair(ctx context.Context, l pairing.Link) (*Session, error) {
	me := m.kr.Person()
	ac, canAnon := m.cfg.Contacts.(AnonymousContacts)
	switch {
	case l.Kind != pairing.KindPair:
		return nil, errors.New("session: not a pair link")
	case me == nil:
		return nil, identity.ErrNoPerson
	case bytes.Equal(l.Person, me):
		return nil, errors.New("session: that link is from my own person; use a join link to add a device")
	case l.Anonymous && !canAnon:
		return nil, errors.New("session: this contacts store cannot keep anonymous contacts")
	}
	c := wire.Call{Op: wire.OpPair, Secret: l.Secret}
	rec := l.Endpoints
	if l.Anonymous {
		rec = onlyAnonymous(rec)
		b, err := m.kr.AnonymousBundle()
		if err != nil {
			return nil, err
		}
		c.Bundle = b.Marshal() // dial keeps it: the inviter is not a contact yet
	}
	conn, res, path, pk, err := m.dial(ctx, rec, c, l.Person)
	if err != nil {
		return nil, err
	}
	if l.Anonymous && !identity.AnonymousTransport(path.Transport) {
		conn.Close() // cannot happen (rec is onion-only), but never pair anonymously in the clear
		return nil, ErrNotAnonymousPath
	}
	if _, err := m.cache.Accept(res.Bundle, res.Device); err != nil {
		conn.Close()
		return nil, err
	}
	if l.Anonymous {
		err = ac.AddAnonymousContact(l.Person, res.Bundle.Person.Name)
	} else {
		err = m.cfg.Contacts.AddContact(l.Person, res.Bundle.Person.Name)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	m.NoteEndpoints(res.Bundle.Endpoints)
	m.noteBundle(res.Bundle) // persisted: a restart right after pairing must still redial the inviter
	m.relay(res.Bundle)
	s := m.start(conn, true, Peer{Person: l.Person, Device: res.Device, Relation: Contact}, m.kr.DevicePublic(), path, pk,
		identity.AnonymousTransport(path.Transport))
	if s == nil {
		return nil, ErrClosed
	}
	m.Keep(l.Person, res.Device)
	return s, nil
}

// Join redeems a sol://join link on a device that has no person yet: the
// admin device adds it to the device list under name and sends the list
// back, which this device adopts.
func (m *Manager) Join(ctx context.Context, l pairing.Link, name string) (*Session, error) {
	if l.Kind != pairing.KindJoin {
		return nil, errors.New("session: not a join link")
	}
	if m.kr.Person() != nil {
		return nil, errors.New("session: this device already belongs to a person")
	}
	conn, res, path, pk, err := m.dial(ctx, l.Endpoints, wire.Call{Op: wire.OpJoin, Secret: l.Secret, Name: name}, l.Person)
	if err != nil {
		return nil, err
	}
	if err := m.kr.Adopt(res.Bundle.Person, res.Bundle.Devices); err != nil {
		conn.Close()
		return nil, fmt.Errorf("session: adopting the device list: %w", err)
	}
	m.BundleChanged() // this device has a person now
	m.NoteEndpoints(res.Bundle.Endpoints)
	m.noteBundle(res.Bundle) // persisted: a restart right after joining must still redial the admin device
	s := m.start(conn, true, Peer{Person: l.Person, Device: res.Device, Relation: Self}, m.kr.DevicePublic(), path, pk,
		identity.AnonymousTransport(path.Transport))
	if s == nil {
		return nil, ErrClosed
	}
	m.Keep(l.Person, res.Device)
	return s, nil
}

// Invite issues a one-time pair or join link for this device. Join links
// need an admin device.
func (m *Manager) Invite(kind pairing.Kind) (pairing.Link, error) {
	return m.InviteToken(pairing.Token{Kind: kind})
}

// ErrNoOnion: an anonymous invite needs this device's onion, and there is none yet.
var ErrNoOnion = errors.New("session: an anonymous invite needs Tor, and this device has no onion yet (is tor on and started?)")

// InviteToken is Invite for any token: an anonymous one makes a link that carries only the
// onion (and redeeming it makes an anonymous contact).
func (m *Manager) InviteToken(tok pairing.Token) (pairing.Link, error) {
	if m.cfg.Tokens == nil {
		return pairing.Link{}, errors.New("session: no token store")
	}
	b, err := m.kr.Bundle()
	if err != nil {
		return pairing.Link{}, err
	}
	if tok.Kind == pairing.KindJoin && !m.kr.IsAdmin() {
		return pairing.Link{}, identity.ErrNotAdmin
	}
	eps := b.Endpoints
	if tok.Anonymous {
		if _, ok := m.cfg.Contacts.(AnonymousContacts); !ok {
			return pairing.Link{}, errors.New("session: this contacts store cannot keep anonymous contacts")
		}
		eps = onlyAnonymous(eps)
		if !slices.ContainsFunc(eps.Endpoints, func(e identity.Endpoint) bool { return e.Transport == "tor" }) {
			return pairing.Link{}, ErrNoOnion
		}
	}
	secret, _, err := m.cfg.Tokens.NewToken(tok)
	if err != nil {
		return pairing.Link{}, err
	}
	return pairing.Link{Kind: tok.Kind, Person: b.Person.Person, Device: m.kr.DevicePublic(),
		Secret: secret, Endpoints: eps, Anonymous: tok.Anonymous}, nil
}

// keeper keeps a session with one device up.
type keeper struct {
	person, device ed25519.PublicKey
	ctx            context.Context
	cancel         context.CancelFunc
	wake           chan struct{}
}

func (k *keeper) poke() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// Keep keeps a session with device up from now on: whenever it dies, it is
// redialed with backoff. Inbound sessions count.
func (m *Manager) Keep(person, device ed25519.PublicKey) {
	key := string(device)
	m.mu.Lock()
	if m.closed || m.keepers[key] != nil || bytes.Equal(device, m.kr.DevicePublic()) {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	k := &keeper{person: bytes.Clone(person), device: bytes.Clone(device), ctx: ctx, cancel: cancel,
		wake: make(chan struct{}, 1)}
	m.keepers[key] = k
	m.wg.Add(1)
	m.mu.Unlock()
	go m.keep(k)
	m.syncPeer(person, device)
}

// Forget stops keeping device and closes its session.
func (m *Manager) Forget(device ed25519.PublicKey) {
	m.mu.Lock()
	k := m.keepers[string(device)]
	delete(m.keepers, string(device))
	s := m.sessions[string(device)]
	old := m.drainingLocked(device)
	m.mu.Unlock()
	if k != nil {
		k.cancel()
		m.syncPeer(k.person, device)
	}
	if s != nil {
		s.Close()
	}
	for _, o := range old {
		o.mux.Close()
	}
	m.deauthorize(device)
}

// NetworkChanged reacts to a network change or a wake from sleep: every
// keeper waiting in backoff redials now, every failed path is due for a
// probe, and every session is checked (one whose local address is gone, or
// that doesn't answer within the probe timeout, is closed and redialed over
// the best path there is now).
func (m *Manager) NetworkChanged() {
	m.pathsDue()
	m.checkSessions()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.keepers {
		k.poke()
	}
}

func (m *Manager) keep(k *keeper) {
	defer m.wg.Done()
	backoff := RedialMin
	var notBefore time.Time // no upgrade attempt before this
	for k.ctx.Err() == nil {
		if s, ok := m.Session(k.device); ok {
			backoff = RedialMin
			var due <-chan time.Time
			var t *time.Timer
			ep, when, up := m.upgradeDue(s)
			if up {
				t = time.NewTimer(time.Until(maxTime(when, notBefore)))
				due = t.C
			}
			select {
			case <-s.Retired(): // replaced or dead: look again
			case <-k.wake:
			case <-due:
				m.tryUpgrade(k.ctx, s, ep)
				notBefore = time.Now().Add(RedialMin)
			case <-k.ctx.Done():
				if t != nil {
					t.Stop()
				}
				return
			}
			if t != nil {
				t.Stop()
			}
			continue
		}
		if _, err := m.Connect(k.ctx, k.person, k.device); err == nil {
			continue
		} else if k.ctx.Err() == nil {
			m.log.Debug("redial failed", "peer", short(k.device), "err", err, "retry_in", backoff)
		}
		wait := backoff/2 + rand.N(backoff/2+1) // jitter, so two sides don't redial in lockstep
		backoff = min(backoff*2, RedialMax)
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-k.wake:
			t.Stop()
		case <-k.ctx.Done():
			t.Stop()
			return
		}
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
