// Package nearby finds this device's peers on the local network (design/PROTOCOL.md §Nearby):
// it sends a small multicast beacon that only my own devices and my buddies' devices can
// recognise, and tells the caller when it recognises one of theirs, with the address it came
// from and the ports it advertised, so the session layer can dial that address.
//
// Invariants:
//
//   - A beacon carries no public key, name or stable ID: per peer device, an 8-byte tag from a
//     key only the two devices can derive, changing every WindowLength, and its ports masked
//     under the same key. Observers can't link beacons across windows or tell who knows whom.
//   - Only the peers Config.Peers returns get an entry, and only theirs are recognised: the
//     caller leaves out anonymous contacts, so they never learn of my LAN presence and I never
//     dial them on a LAN.
//   - A match grants nothing: it is a candidate address for one device, which the handshake
//     still authenticates. Nothing is ever sent to a beacon's source.
//   - Off (SetEnabled(false)), or with no port reachable on the LAN, no socket is open: nothing
//     is sent or received.
//   - Beacons go out at start, on network changes (and once more after they settle), every
//     Interval (jittered), and at most every ReplyEvery when a peer without a session was
//     recognised. That timer is the protocol, not polling: no state is polled.
package nearby

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/netip"
	"sync"
	"time"
)

// Tunables (package variables so tests can shorten them; set them before New).
var (
	// Interval is how often a beacon goes out (±20%).
	Interval = 30 * time.Second
	// Settle is how long after a network change the beacon is sent once more, when DHCP and the
	// transports have their new addresses.
	Settle = 5 * time.Second
	// ReplyEvery bounds the extra beacons sent because a peer without a session was seen.
	ReplyEvery = 5 * time.Second
	// SeenFor is how long a recognised peer counts as nearby in Status.
	SeenFor = 3 * time.Minute
	// MaxPerSecond bounds the datagrams processed per second; the rest are dropped.
	MaxPerSecond = 50
)

// dupFor is how long a datagram's copies (from the other interfaces' sockets) are dropped.
const dupFor = 2 * time.Second

// Peer is a device this device keeps a session with and may be recognised by.
type Peer struct {
	Device ed25519.PublicKey
	// Connected: a live session with it exists (a recognised beacon then needs no reply).
	Connected bool
}

// Config configures a Service.
type Config struct {
	// Self is this device's key.
	Self ed25519.PublicKey
	// Key returns the key this device shares with a peer (identity.Keyring.NearbyKey).
	Key func(peer ed25519.PublicKey) ([32]byte, error)
	// Peers returns the devices to send entries for and to recognise: my other devices and my
	// buddies' devices, never an anonymous contact's. Called per beacon sent or received, so it
	// must be cheap and must not block.
	Peers func() []Peer
	// Ports returns what to advertise now. All zero means nothing on the LAN could reach this
	// device: the Service then keeps no socket open.
	Ports func() Ports
	// Found is told about every recognised beacon: the device, where it came from and its
	// ports. It must not block.
	Found func(device ed25519.PublicKey, src netip.Addr, p Ports)
	// Net opens the sockets; nil means UDP multicast.
	Net Network
	// AllowLoopback accepts beacons from loopback sources (tests on one machine).
	AllowLoopback bool
	Log           *slog.Logger
}

// Status says how nearby is doing, for `sol status` and the transports list.
type Status struct {
	Enabled bool `json:"enabled"`
	// State is "up" (sockets open), "off", "idle" (on, but nothing to do: Detail says why) or
	// "failed".
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// Interfaces is how many interfaces beacons go out of (0 with State "up": the default one).
	Interfaces int `json:"interfaces,omitempty"`
	// Seen counts the peer devices recognised in the last SeenFor.
	Seen int `json:"seen"`
}

// Service sends and receives beacons. Safe for concurrent use.
type Service struct {
	cfg    Config
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	kick   chan struct{}

	mu        sync.Mutex
	enabled   bool
	conn      Conn
	gen       int // bumps with every (re)open, so a stale reader stops
	idle      string
	failed    string
	ifaces    int
	keys      map[string][32]byte
	table     *Table
	seen      map[string]time.Time
	dups      map[[32]byte]time.Time
	lastReply time.Time
	budget    int
	budgetAt  time.Time
	settle    *time.Timer
}

// New returns a Service, off until SetEnabled(true).
func New(cfg Config) *Service {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Net == nil {
		cfg.Net = UDP{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{cfg: cfg, log: cfg.Log, ctx: ctx, cancel: cancel, kick: make(chan struct{}, 1),
		keys: map[string][32]byte{}, seen: map[string]time.Time{}, dups: map[[32]byte]time.Time{}}
	s.wg.Add(1)
	go s.sender()
	return s
}

// SetEnabled turns nearby on or off. Off closes the sockets at once.
func (s *Service) SetEnabled(on bool) {
	s.mu.Lock()
	s.enabled = on
	s.mu.Unlock()
	s.Update()
}

// Update re-evaluates whether sockets should be open (on, and some port to advertise) and
// opens or closes them; call it when transports change.
func (s *Service) Update() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateLocked(false)
}

// NetworkChanged reopens the sockets on the interfaces there are now and sends a beacon at
// once, and again after Settle.
func (s *Service) NetworkChanged() {
	s.mu.Lock()
	s.updateLocked(true)
	if s.settle != nil {
		s.settle.Stop()
	}
	if s.ctx.Err() == nil {
		s.settle = time.AfterFunc(Settle, s.Announce)
	}
	s.mu.Unlock()
}

// Announce sends a beacon now (if nearby is up).
func (s *Service) Announce() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// updateLocked opens or closes the sockets as they should be; reopen forces a fresh open.
func (s *Service) updateLocked(reopen bool) {
	if s.ctx.Err() != nil {
		return
	}
	want := s.enabled
	s.idle = ""
	if want && s.cfg.Ports().IsZero() {
		want, s.idle = false, "no transport listens on the LAN"
	}
	if s.conn != nil && (!want || reopen) {
		s.conn.Close()
		s.conn, s.ifaces = nil, 0
		s.gen++
	}
	if !want || s.conn != nil {
		return
	}
	c, err := s.cfg.Net.Open()
	switch {
	case errors.Is(err, ErrNoInterface):
		s.failed, s.idle = "", "no network"
		return
	case errors.Is(err, errors.ErrUnsupported):
		s.failed = "not available on this system"
		return
	case err != nil:
		s.failed = err.Error()
		s.log.Info("nearby: could not open the beacon sockets", "err", err)
		return
	}
	s.failed = ""
	s.conn, s.ifaces = c, c.Interfaces()
	s.gen++
	gen := s.gen
	s.wg.Add(1)
	go s.receive(c, gen)
	s.Announce()
}

// Status reports how nearby is doing.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Enabled: s.enabled}
	now := time.Now()
	for k, at := range s.seen {
		if now.Sub(at) < SeenFor {
			st.Seen++
		} else {
			delete(s.seen, k)
		}
	}
	switch {
	case !s.enabled:
		st.State = "off"
	case s.conn != nil:
		st.State, st.Interfaces = "up", s.ifaces
		ifs := "the default interface"
		if s.ifaces == 1 {
			ifs = "1 interface"
		} else if s.ifaces > 1 {
			ifs = fmt.Sprintf("%d interfaces", s.ifaces)
		}
		st.Detail = fmt.Sprintf("beacons on %s; %d of my and my buddies' devices nearby", ifs, st.Seen)
	case s.failed != "":
		st.State, st.Detail = "failed", s.failed
	default:
		st.State, st.Detail = "idle", s.idle
	}
	return st
}

// Close stops the Service and closes its sockets.
func (s *Service) Close() {
	s.cancel()
	s.mu.Lock()
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
	if s.settle != nil {
		s.settle.Stop()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// peerKeys returns the peers with their keys (cached; computing one is an X25519).
func (s *Service) peerKeys() ([]PeerKey, map[string]bool) {
	peers := s.cfg.Peers()
	out := make([]PeerKey, 0, len(peers))
	connected := make(map[string]bool, len(peers))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range peers {
		if len(p.Device) != ed25519.PublicKeySize || bytes.Equal(p.Device, s.cfg.Self) {
			continue
		}
		k, ok := s.keys[string(p.Device)]
		if !ok {
			var err error
			if k, err = s.cfg.Key(p.Device); err != nil {
				continue
			}
			s.keys[string(p.Device)] = k
		}
		out = append(out, PeerKey{Device: p.Device, Key: k})
		connected[string(p.Device)] = p.Connected
	}
	return out, connected
}

// sender sends a beacon every Interval (jittered) and whenever kicked.
func (s *Service) sender() {
	defer s.wg.Done()
	for {
		t := time.NewTimer(Interval*8/10 + rand.N(Interval*4/10+1))
		select {
		case <-s.ctx.Done():
			t.Stop()
			return
		case <-s.kick:
			t.Stop()
		case <-t.C:
		}
		s.send()
	}
}

// send builds and sends the beacon for the peers there are now.
func (s *Service) send() {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		return
	}
	ports := s.cfg.Ports()
	if ports.IsZero() {
		return
	}
	peers, _ := s.peerKeys()
	for _, d := range Encode(peers, s.cfg.Self, ports, time.Now()) {
		if err := c.Send(d); err != nil {
			s.log.Debug("nearby: sending a beacon failed", "err", err)
			return
		}
	}
}

// receive reads datagrams from c until it is closed.
func (s *Service) receive(c Conn, gen int) {
	defer s.wg.Done()
	buf := make([]byte, MaxDatagram+1)
	for {
		n, src, err := c.Receive(buf)
		if err != nil {
			return
		}
		s.mu.Lock()
		stale := s.gen != gen
		s.mu.Unlock()
		if stale {
			return
		}
		if n > MaxDatagram {
			continue
		}
		s.handle(buf[:n], src)
	}
}

// okSource reports whether a beacon from src could be from a device on this link.
func (s *Service) okSource(src netip.Addr) bool {
	src = src.Unmap()
	switch {
	case !src.Is4(), src.IsUnspecified(), src.IsMulticast(), src.IsLinkLocalUnicast():
		return false
	case src.IsLoopback():
		return s.cfg.AllowLoopback
	}
	return true
}

// handle processes one datagram.
func (s *Service) handle(p []byte, src netip.Addr) {
	if !s.okSource(src) || len(p) < headerSize || string(p[:len(magic)]) != magic {
		return
	}
	now := time.Now()
	s.mu.Lock()
	if now.Sub(s.budgetAt) >= time.Second {
		s.budget, s.budgetAt = MaxPerSecond, now
	}
	if s.budget <= 0 {
		s.mu.Unlock()
		return
	}
	s.budget--
	h := sha256.New()
	h.Write(src.AsSlice())
	h.Write(p)
	var key [32]byte
	copy(key[:], h.Sum(nil))
	if at, ok := s.dups[key]; ok && now.Sub(at) < dupFor {
		s.mu.Unlock()
		return
	}
	s.dups[key] = now
	if len(s.dups) > 256 {
		for k, at := range s.dups {
			if now.Sub(at) >= dupFor {
				delete(s.dups, k)
			}
		}
	}
	s.mu.Unlock()

	peers, connected := s.peerKeys()
	s.mu.Lock()
	if !s.table.fresh(peers, now) {
		s.table = NewTable(peers, now)
	}
	t := s.table
	s.mu.Unlock()
	found := t.Match(p)
	reply := false
	for _, f := range found {
		s.mu.Lock()
		s.seen[string(f.Device)] = now
		s.mu.Unlock()
		if !connected[string(f.Device)] {
			reply = true
		}
		if s.cfg.Found != nil {
			s.cfg.Found(f.Device, src.Unmap(), f.Ports)
		}
	}
	if reply {
		s.mu.Lock()
		due := now.Sub(s.lastReply) >= ReplyEvery
		if due {
			s.lastReply = now
		}
		s.mu.Unlock()
		if due {
			s.Announce()
		}
	}
}
