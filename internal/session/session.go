// Package session keeps this device's authenticated sessions with other
// devices (design/PROTOCOL.md §Sessions, §Liveness and healing): it dials
// peers over the configured transports, answers their handshakes, turns each
// connection into a mux session, and serves every mux stream as one HTTP/1.1
// request in both directions.
//
// Invariants:
//
//   - At most one live session per pair of devices. If both dial at once,
//     both sides keep the session whose caller has the lower device ID and
//     close the other, so they converge without talking about it.
//   - A peer is admitted only if its verified person is me (and its device is
//     in my current device list) or a contact (and its device is in the
//     newest list I know for them). Learning a newer list closes the
//     sessions of devices it no longer contains.
//   - Pair and join secrets are burned on first use (internal/pairing).
//   - A kept peer is redialed whenever its session dies, with exponential
//     backoff between RedialMin and RedialMax.
//   - Nothing here logs secrets or request bodies.
//
// Path choice is a simple ordered try (Config.Paths); transport ranking,
// probation and hedging are ticket C3b and plug in there.
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
	// Paths orders a device's endpoints for dialing. nil means the order of
	// the record. The seam for C3b's ranking and probation.
	Paths func(identity.EndpointRecord) []identity.Endpoint
	Log   *slog.Logger
}

// Manager runs the sessions of one device. Safe for concurrent use.
type Manager struct {
	cfg   Config
	kr    *identity.Keyring
	cache *identity.Cache
	log   *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	slots  chan struct{} // inbound handshakes in progress

	mu        sync.Mutex
	closed    bool
	sessions  map[string]*Session // by peer device key
	keepers   map[string]*keeper
	endpoints map[string]identity.EndpointRecord // newest seen per device
	dialing   map[string]*sync.Mutex
	listeners map[net.Listener]struct{}
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
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		cfg: cfg, kr: cfg.Keyring, cache: cfg.Cache,
		log:       cfg.Log.With("device", short(cfg.Keyring.DevicePublic())),
		ctx:       ctx,
		cancel:    cancel,
		slots:     make(chan struct{}, MaxPendingHandshakes),
		sessions:  map[string]*Session{},
		keepers:   map[string]*keeper{},
		endpoints: map[string]identity.EndpointRecord{},
		dialing:   map[string]*sync.Mutex{},
		listeners: map[net.Listener]struct{}{},
	}, nil
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
}

// Peer returns the verified other side.
func (s *Session) Peer() Peer { return s.peer }

// Caller returns the device key of the side that dialed this session.
func (s *Session) Caller() ed25519.PublicKey { return s.caller }

// Done is closed when the session dies.
func (s *Session) Done() <-chan struct{} { return s.mux.Done() }

// Alive reports whether the session is still up.
func (s *Session) Alive() bool { return s.mux.Err() == nil }

// Close closes the session.
func (s *Session) Close() error { return s.mux.Close() }

// Open opens a raw stream to the peer.
func (s *Session) Open(ctx context.Context) (net.Conn, error) { return s.mux.Open(ctx) }

// HTTPClient returns a client that sends each request over a new stream of
// this session. The URL's host is ignored; use e.g. "http://peer/p/bundle".
func (s *Session) HTTPClient() *http.Client { return s.client }

// Serve accepts connections on ln (the local target the transports publish)
// until ln is closed or the Manager is.
func (m *Manager) Serve(ln net.Listener) error {
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
			m.answer(conn)
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
}

func (m *Manager) endpointsFor(person, device ed25519.PublicKey) (identity.EndpointRecord, bool) {
	m.mu.Lock()
	rec, ok := m.endpoints[string(device)]
	m.mu.Unlock()
	if c, cok := m.cache.Endpoints(person, device); cok && (!ok || c.Seq > rec.Seq) {
		return c, true
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
// either direction) and folds its bundle into what we know.
func (m *Manager) admit(b identity.Bundle, dev ed25519.PublicKey) (Peer, error) {
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
		return Peer{Person: person, Device: dev, Relation: Self}, nil
	}
	if !m.cfg.Contacts.IsContact(person) {
		return Peer{}, ErrNotRelated
	}
	if err := m.foldContact(b, dev); err != nil {
		return Peer{}, err
	}
	m.NoteEndpoints(b.Endpoints)
	return Peer{Person: person, Device: dev, Relation: Contact}, nil
}

// foldContact checks a contact's bundle against the cache. A device that
// presents an older (but valid) list is still admitted while the newest
// list we know contains it: it just hasn't caught up.
func (m *Manager) foldContact(b identity.Bundle, dev ed25519.PublicKey) error {
	_, err := m.cache.Accept(b, dev)
	if err == nil {
		m.prune(b.Person.Person)
		return nil
	}
	if !errors.Is(err, identity.ErrRollback) {
		return err
	}
	_, dl, ok := m.cache.Latest(b.Person.Person)
	if !ok {
		return err
	}
	if _, in := dl.Find(dev); !in {
		return fmt.Errorf("contact device: %w", identity.ErrNotInList)
	}
	return nil
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
	return dl, nil
}

// answer runs the callee side of one inbound connection.
func (m *Manager) answer(conn net.Conn) {
	var peer Peer
	decide := func(r *wire.Request) ([]byte, error) {
		var err error
		peer, err = m.decide(r)
		if err != nil {
			return nil, err
		}
		return m.myBundle()
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
	s := m.start(conn, false, peer, req.Device)
	if s != nil {
		m.Keep(peer.Person, peer.Device)
	}
}

// decide is the callee's policy for each op (PROTOCOL.md §Callee decision).
func (m *Manager) decide(r *wire.Request) (Peer, error) {
	switch r.Op {
	case wire.OpSession:
		return m.admit(r.Bundle, r.Device)
	case wire.OpPair:
		person := r.Bundle.Person.Person
		if me := m.kr.Person(); me == nil || bytes.Equal(person, me) {
			return Peer{}, errors.New("pair from my own person or before I have one")
		}
		if m.cfg.Tokens == nil {
			return Peer{}, errors.New("pairing is off")
		}
		if err := m.cfg.Tokens.Consume(pairing.KindPair, r.Secret); err != nil {
			return Peer{}, err
		}
		if _, err := m.cache.Accept(r.Bundle, r.Device); err != nil {
			return Peer{}, err
		}
		if err := m.cfg.Contacts.AddContact(person, r.Bundle.Person.Name); err != nil {
			return Peer{}, err
		}
		m.NoteEndpoints(r.Bundle.Endpoints)
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
		// The v2 Request has no field for the new device's name, so it
		// gets a placeholder until it is renamed.
		if _, err := m.kr.AddDevice(r.Device, "device "+short(r.Device), 0, time.Now()); err != nil {
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
func (m *Manager) start(conn net.Conn, weCalled bool, peer Peer, caller ed25519.PublicKey) *Session {
	var ms *mux.Session
	if weCalled {
		ms = mux.Client(conn)
	} else {
		ms = mux.Server(conn)
	}
	s := &Session{m: m, mux: ms, peer: peer, caller: caller}
	s.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return ms.Open(ctx)
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
	winner := m.register(s)
	if winner != s {
		return winner
	}
	// register counted both goroutines in m.wg.
	go func() {
		defer m.wg.Done()
		s.srv.Serve(ms.Listener())
	}()
	go func() {
		defer m.wg.Done()
		<-ms.Done()
		s.srv.Close()
		s.client.CloseIdleConnections()
		m.unregister(s)
	}()
	m.log.Info("session up", "peer", short(peer.Device), "relation", peer.Relation, "caller", short(caller))
	return s
}

// register installs s as the session with its device, applying the
// tie-break: the session whose caller has the lower device ID wins; between
// two sessions dialed by the same side, the newer one (the older is likely
// dead). It returns the winner and closes the loser.
func (m *Manager) register(s *Session) *Session {
	key := string(s.peer.Device)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		s.Close()
		return nil
	}
	old := m.sessions[key]
	if old != nil && old.Alive() {
		oc, nc := identity.ID(old.caller), identity.ID(s.caller)
		if oc < nc {
			m.mu.Unlock()
			s.Close()
			return old
		}
	}
	m.sessions[key] = s
	m.wg.Add(2) // under m.mu, so Close's Wait sees them: start's two goroutines
	k := m.keepers[key]
	m.mu.Unlock()
	if old != nil {
		old.Close()
	}
	if k != nil {
		k.poke()
	}
	return s
}

func (m *Manager) unregister(s *Session) {
	key := string(s.peer.Device)
	m.mu.Lock()
	if m.sessions[key] == s {
		delete(m.sessions, key)
	}
	k := m.keepers[key]
	m.mu.Unlock()
	if k != nil {
		k.poke()
	}
}

func (m *Manager) handler() http.Handler {
	mx := http.NewServeMux()
	mx.HandleFunc("GET /p/bundle", func(w http.ResponseWriter, r *http.Request) {
		b, err := m.myBundle()
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
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	dm := m.dialing[string(device)]
	if dm == nil {
		dm = &sync.Mutex{}
		m.dialing[string(device)] = dm
	}
	m.mu.Unlock()
	dm.Lock()
	defer dm.Unlock()
	if s, ok := m.Session(device); ok {
		return s, nil
	}
	rec, ok := m.endpointsFor(person, device)
	if !ok || !bytes.Equal(rec.Device, device) {
		return nil, ErrNoRoute
	}
	conn, res, err := m.dial(ctx, rec, wire.OpSession, nil, person)
	if err != nil {
		return nil, err
	}
	peer, err := m.admit(res.Bundle, res.Device)
	if err != nil {
		conn.Close()
		return nil, err
	}
	s := m.start(conn, true, peer, m.kr.DevicePublic())
	if s == nil {
		return nil, ErrClosed
	}
	return s, nil
}

// transportFor returns the first configured transport that can dial ep.
func (m *Manager) transportFor(ep identity.Endpoint) transport.Transport {
	for _, t := range m.cfg.Transports {
		if t.Name() == ep.Transport && t.Handles(ep.Address) {
			return t
		}
	}
	return nil
}

// dial tries rec's endpoints in order and runs the handshake for op on the
// first connection that opens. The callee must be rec.Device and speak for
// wantPerson.
func (m *Manager) dial(ctx context.Context, rec identity.EndpointRecord, op string, secret []byte, wantPerson ed25519.PublicKey) (net.Conn, *wire.Result, error) {
	var bundle []byte
	var err error
	if op == wire.OpJoin {
		bundle = m.kr.Endpoints().Marshal()
	} else if bundle, err = m.myBundle(); err != nil {
		return nil, nil, err
	}
	paths := rec.Endpoints
	if m.cfg.Paths != nil {
		paths = m.cfg.Paths(rec)
	}
	var errs []error
	for _, ep := range paths {
		t := m.transportFor(ep)
		if t == nil {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, DialTimeout)
		conn, err := t.Dial(dctx, ep.Address)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ep.Transport, err))
			continue
		}
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		res, err := wire.Dial(conn, m.kr, wire.Call{Op: op, Want: rec.Device, Bundle: bundle, Secret: secret})
		stop()
		if err != nil {
			if errors.Is(err, wire.ErrWrongDevice) || errors.Is(err, wire.ErrTruncated) {
				errs = append(errs, fmt.Errorf("%s: %w", ep.Transport, err))
				continue // that path leads elsewhere, or broke: try the next
			}
			return nil, nil, err // the device itself answered no
		}
		if !bytes.Equal(res.Bundle.Person.Person, wantPerson) {
			conn.Close()
			return nil, nil, ErrWrongPeer
		}
		if ctx.Err() != nil {
			conn.Close()
			return nil, nil, ctx.Err()
		}
		return conn, res, nil
	}
	if len(errs) == 0 {
		return nil, nil, ErrNoRoute
	}
	return nil, nil, errors.Join(errs...)
}

// Pair redeems a sol://pair link: the inviter and this person become
// contacts, and a session comes up.
func (m *Manager) Pair(ctx context.Context, l pairing.Link) (*Session, error) {
	me := m.kr.Person()
	switch {
	case l.Kind != pairing.KindPair:
		return nil, errors.New("session: not a pair link")
	case me == nil:
		return nil, identity.ErrNoPerson
	case bytes.Equal(l.Person, me):
		return nil, errors.New("session: that link is from my own person; use a join link to add a device")
	}
	conn, res, err := m.dial(ctx, l.Endpoints, wire.OpPair, l.Secret, l.Person)
	if err != nil {
		return nil, err
	}
	if _, err := m.cache.Accept(res.Bundle, res.Device); err != nil {
		conn.Close()
		return nil, err
	}
	if err := m.cfg.Contacts.AddContact(l.Person, res.Bundle.Person.Name); err != nil {
		conn.Close()
		return nil, err
	}
	m.NoteEndpoints(res.Bundle.Endpoints)
	s := m.start(conn, true, Peer{Person: l.Person, Device: res.Device, Relation: Contact}, m.kr.DevicePublic())
	if s == nil {
		return nil, ErrClosed
	}
	m.Keep(l.Person, res.Device)
	return s, nil
}

// Join redeems a sol://join link on a device that has no person yet: the
// admin device adds it to the device list and sends the list back, which
// this device adopts.
func (m *Manager) Join(ctx context.Context, l pairing.Link) (*Session, error) {
	if l.Kind != pairing.KindJoin {
		return nil, errors.New("session: not a join link")
	}
	if m.kr.Person() != nil {
		return nil, errors.New("session: this device already belongs to a person")
	}
	conn, res, err := m.dial(ctx, l.Endpoints, wire.OpJoin, l.Secret, l.Person)
	if err != nil {
		return nil, err
	}
	if err := m.kr.Adopt(res.Bundle.Person, res.Bundle.Devices); err != nil {
		conn.Close()
		return nil, fmt.Errorf("session: adopting the device list: %w", err)
	}
	m.NoteEndpoints(res.Bundle.Endpoints)
	s := m.start(conn, true, Peer{Person: l.Person, Device: res.Device, Relation: Self}, m.kr.DevicePublic())
	if s == nil {
		return nil, ErrClosed
	}
	m.Keep(l.Person, res.Device)
	return s, nil
}

// Invite issues a one-time pair or join link for this device. Join links
// need an admin device.
func (m *Manager) Invite(kind pairing.Kind) (pairing.Link, error) {
	if m.cfg.Tokens == nil {
		return pairing.Link{}, errors.New("session: no token store")
	}
	b, err := m.kr.Bundle()
	if err != nil {
		return pairing.Link{}, err
	}
	if kind == pairing.KindJoin && !m.kr.IsAdmin() {
		return pairing.Link{}, identity.ErrNotAdmin
	}
	secret, _, err := m.cfg.Tokens.New(kind)
	if err != nil {
		return pairing.Link{}, err
	}
	l := pairing.Link{Kind: kind, Person: b.Person.Person, Device: m.kr.DevicePublic(),
		Secret: secret, Endpoints: b.Endpoints}
	if kind == pairing.KindPair {
		l.Name = b.Person.Name
	}
	return l, nil
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
}

// Forget stops keeping device and closes its session.
func (m *Manager) Forget(device ed25519.PublicKey) {
	m.mu.Lock()
	k := m.keepers[string(device)]
	delete(m.keepers, string(device))
	s := m.sessions[string(device)]
	m.mu.Unlock()
	if k != nil {
		k.cancel()
	}
	if s != nil {
		s.Close()
	}
}

// NetworkChanged makes every keeper waiting in backoff redial now.
func (m *Manager) NetworkChanged() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.keepers {
		k.poke()
	}
}

func (m *Manager) keep(k *keeper) {
	defer m.wg.Done()
	backoff := RedialMin
	for k.ctx.Err() == nil {
		if s, ok := m.Session(k.device); ok {
			backoff = RedialMin
			select {
			case <-s.Done():
			case <-k.wake:
			case <-k.ctx.Done():
				return
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
