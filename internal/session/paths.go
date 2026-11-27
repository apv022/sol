package session

// Path choice (design/PROTOCOL.md §Liveness and healing), ported from the PoC's dialer and
// liveness supervisor (~/solbak/internal/node/dial.go, liveness.go):
//
//   - A device's endpoints are ranked: WireGuard, TLS direct, clearnet (LAN, tests), then the
//     relayed and anonymous transports (Tor, I2P). Within that, the path that last worked and
//     transports that are ready right now go first. A path that failed is moved behind every
//     healthy one (it stays as a last resort).
//   - Dialing is hedged: the best path starts at once, the next one HedgeDelay later or as
//     soon as the one before it fails, and the first handshake to complete wins. Only one
//     attempt at a time may send its Request (wire.Call.Gate), so a loser is abandoned before
//     the callee could complete a second session.
//   - A path that failed (or lost a hedge after starting first: it is slow) backs off,
//     PathBackoff doubling up to PathBackoffMax. While the session runs on a worse path, the
//     keeper probes it (wire.OpProbe) when its backoff ends; it is preferred again only after
//     StableProbes probes in a row succeeded over at least StableFor (hysteresis), and then
//     the session moves back to it. Any failure meanwhile doubles the backoff, so a flaky
//     path drifts towards being tried rarely instead of flapping.
//   - Network changes make every backoff due at once (NetworkChanged).
//   - Bluetooth LE ("ble", Android) is ranked behind every IP path, healthy or failed: it carries
//     a session only when nothing else answers, and the keeper keeps probing the better paths.
//
// A session we dialed knows its path; one the peer dialed only knows it came in (the
// transports forward to one local listener). Only the side that dialed a session moves it,
// because only its new session is sure to win the tie-break (same caller, newer wins).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/transport"
	"github.com/apv022/sol/internal/wire"
)

// Path tunables (package variables so tests can shorten them; set them before creating a
// Manager). Defaults are the PoC's.
var (
	// HedgeDelay is how long a dial waits on one path before also starting the next.
	HedgeDelay = 2 * time.Second
	// PathBackoff and PathBackoffMax bound how long a failed path waits before its next
	// probe, doubling with each failure.
	PathBackoff    = time.Minute
	PathBackoffMax = time.Hour
	// StableFor and StableProbes: a failed path is preferred again once StableProbes probes
	// in a row succeeded over at least StableFor.
	StableFor    = 2 * time.Minute
	StableProbes = 3
	// ProbeTimeout bounds one probe (and the session check after a network change) over a
	// direct or LAN path; SlowProbeTimeout over Tor or I2P.
	ProbeTimeout     = 10 * time.Second
	SlowProbeTimeout = time.Minute
)

// maxProbes caps concurrent probes, so a network change with many peers doesn't open a
// burst of Tor circuits.
const maxProbes = 4

// errLost aborts a hedged attempt whose sibling already won.
var errLost = errors.New("session: another path answered first")

// PathInfo says how a session was reached, for the Sol app's Advanced view.
type PathInfo struct {
	// Transport carried the session ("wg", "direct", "tor", ...); empty when the peer
	// dialed this device.
	Transport string `json:"transport,omitempty"`
	// Inbound: the peer dialed this device, so only it knows the path.
	Inbound bool `json:"inbound,omitempty"`
	// Via is what carried an inbound session when this device can tell ("ble": the session layer
	// served it on the Bluetooth LE listener, ServeVia); empty otherwise.
	Via string `json:"via,omitempty"`
	// Reason says in a few words why this path: "best path", "wg failed: ...", "wg was
	// slower; tor answered first", "wg is stable again".
	Reason string `json:"reason"`
}

// PathState is what this device knows about one path to a peer device that has had trouble.
// Paths that are not listed are healthy.
type PathState struct {
	Transport string `json:"transport"`
	// State is "backoff" (failed: tried after the healthy paths until it proves itself
	// again) or "probation" (probes succeed again; preferred once stable).
	State     string    `json:"state"`
	Failures  int       `json:"failures"` // since it was last preferred
	LastError string    `json:"last_error,omitempty"`
	FailedAt  time.Time `json:"failed_at"`
	NextProbe time.Time `json:"next_probe"`
}

// pathFail is the state of one failed path, until it is stable again.
type pathFail struct {
	at        time.Time
	wait      time.Duration
	failures  int
	err       string
	oks       int
	okSince   time.Time
	nextProbe time.Time
}

// link is what the manager knows about the paths to one device. Guarded by Manager.mu.
type link struct {
	lastGood string               // pathKey of the last path that reached it
	fails    map[string]*pathFail // by pathKey
}

func pathKey(ep identity.Endpoint) string { return ep.Transport + "|" + ep.Address }

// preference orders transports: lower first.
func preference(name string) int {
	switch name {
	case "wg":
		return 0 // WireGuard: fastest, and punches NATs
	case "direct":
		return 1 // TLS over TCP, for networks that block UDP
	case "clearnet":
		return 2 // plain TCP: LAN and tests
	case "relay":
		return 3
	case "tor":
		return 5
	case "i2p":
		return 6
	case "ble":
		return 9 // Bluetooth LE (Android): tens of KB/s, only when no IP path answers
	}
	return 4
}

// slow reports whether name is a transport whose round trips may take seconds (the anonymous
// ones, and Bluetooth LE).
func slow(name string) bool { return name == "tor" || name == "i2p" || name == "ble" }

// jitter spreads d by ±20%, so peers don't probe in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d*8/10 + time.Duration(rand.Int64N(int64(d)*4/10+1))
}

// linkLocked returns (creating) device's link. Called with m.mu held.
func (m *Manager) linkLocked(device ed25519.PublicKey) *link {
	l := m.links[string(device)]
	if l == nil {
		l = &link{fails: map[string]*pathFail{}}
		m.links[string(device)] = l
	}
	return l
}

// usableTransport returns the transport for ep if it can dial now.
func (m *Manager) usableTransport(ep identity.Endpoint) transport.Transport {
	t := m.transportFor(ep)
	if t == nil || !transport.Usable(t) {
		return nil
	}
	return t
}

// rank orders rec's endpoints for dialing (see the file comment). Endpoints no usable local
// transport can dial are left out.
func (m *Manager) rank(rec identity.EndpointRecord) []identity.Endpoint {
	type cand struct {
		ep    identity.Endpoint
		pref  int
		ready bool
		last  bool
		bad   bool
	}
	var cs []cand
	for _, ep := range rec.Endpoints {
		if t := m.usableTransport(ep); t != nil {
			cs = append(cs, cand{ep: ep, pref: preference(ep.Transport), ready: t.Status().State == transport.StateReady})
		}
	}
	m.mu.Lock()
	if l := m.links[string(rec.Device)]; l != nil {
		for i := range cs {
			cs[i].last = l.lastGood == pathKey(cs[i].ep)
			cs[i].bad = l.fails[pathKey(cs[i].ep)] != nil
		}
	}
	m.mu.Unlock()
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		switch {
		case (a.ep.Transport == "ble") != (b.ep.Transport == "ble"): // behind every IP path, even a failed one
			return b.ep.Transport == "ble"
		case a.bad != b.bad:
			return !a.bad
		case a.bad: // both failed: by preference only
			return a.pref < b.pref
		case (a.pref <= 1) != (b.pref <= 1): // direct paths first while healthy
			return a.pref <= 1
		case a.last != b.last:
			return a.last
		case a.ready != b.ready:
			return a.ready
		}
		return a.pref < b.pref
	})
	out := make([]identity.Endpoint, len(cs))
	for i, c := range cs {
		out[i] = c.ep
	}
	return out
}

// pathFailed starts or doubles the backoff of a path to device and restarts its stability
// count.
func (m *Manager) pathFailed(device ed25519.PublicKey, ep identity.Endpoint, why string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.linkLocked(device)
	k := pathKey(ep)
	f := l.fails[k]
	wait, failures := PathBackoff, 1
	if f != nil {
		wait, failures = min(f.wait*2, PathBackoffMax), f.failures+1
	}
	now := time.Now()
	m.log.Debug("DBG pathFailed", "peer", short(device), "path", ep.Transport, "why", why)
	l.fails[k] = &pathFail{at: now, wait: wait, failures: failures, err: why, nextProbe: now.Add(jitter(wait))}
	if l.lastGood == k {
		l.lastGood = ""
	}
}

// pathWorked records that ep reached device in a dial (not a probe): it becomes the last
// good path. A failed path that reaches the peer in a dial counts one success towards its
// stability, like a probe; it is not preferred again at once.
func (m *Manager) pathWorked(device ed25519.PublicKey, ep identity.Endpoint) {
	m.mu.Lock()
	m.linkLocked(device).lastGood = pathKey(ep)
	m.mu.Unlock()
	m.pathOK(device, ep)
}

// pathOK counts a success of a failed path towards its hysteresis and reports whether that
// made it preferred again (true also for a path that was never in trouble).
func (m *Manager) pathOK(device ed25519.PublicKey, ep identity.Endpoint) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.linkLocked(device)
	f := l.fails[pathKey(ep)]
	if f == nil {
		return true
	}
	now := time.Now()
	if f.okSince.IsZero() {
		f.okSince = now
	}
	f.oks++
	if f.oks >= StableProbes && now.Sub(f.okSince) >= StableFor {
		delete(l.fails, pathKey(ep))
		return true
	}
	f.nextProbe = now.Add(jitter(StableFor / time.Duration(max(StableProbes-1, 1))))
	return false
}

// failState returns the trouble a path is in (nil: healthy).
func (m *Manager) failState(device ed25519.PublicKey, ep identity.Endpoint) *pathFail {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.links[string(device)]; l != nil {
		if f := l.fails[pathKey(ep)]; f != nil {
			cp := *f
			return &cp
		}
	}
	return nil
}

// Paths reports the paths to device that have had trouble, for the Sol app's Advanced view.
func (m *Manager) Paths(device ed25519.PublicKey) []PathState {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.links[string(device)]
	if l == nil {
		return nil
	}
	var out []PathState
	for k, f := range l.fails {
		name, _, _ := strings.Cut(k, "|")
		st := PathState{Transport: name, State: "backoff", Failures: f.failures, LastError: f.err,
			FailedAt: f.at, NextProbe: f.nextProbe}
		if f.oks > 0 {
			st.State = "probation"
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return preference(out[i].Transport) < preference(out[j].Transport) })
	return out
}

// attempt is the outcome of dialing one path.
type attempt struct {
	ep      identity.Endpoint
	started time.Time
	conn    net.Conn
	res     *wire.Result
	err     error
}

// definitive reports whether err came from the device itself, so no other path will do
// better. A rejection is not: before the Hello it is indistinguishable from a path that
// leads to something that hangs up; nor is garbage, which a middlebox can produce.
func definitive(err error) bool {
	switch {
	case errors.Is(err, wire.ErrWrongDevice), errors.Is(err, wire.ErrTruncated), errors.Is(err, wire.ErrRejected),
		errors.Is(err, wire.ErrMalformed), errors.Is(err, errLost), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	}
	var de *dialError
	return !errors.As(err, &de)
}

// dialError wraps a transport-level dial failure (never definitive).
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// blamable reports whether a failed attempt says something about the path: not when the
// device refused us, nor when our own caller gave up.
func blamable(ctx context.Context, err error) bool {
	return ctx.Err() == nil && !errors.Is(err, wire.ErrRejected) && !errors.Is(err, errLost) &&
		!errors.Is(err, ErrWrongPeer)
}

// dialHedged dials device over paths (ranked) with call, as described in the file comment,
// and returns the winning connection, its handshake result and why that path.
func (m *Manager) dialHedged(ctx context.Context, device, wantPerson ed25519.PublicKey, paths []identity.Endpoint, call wire.Call) (net.Conn, *wire.Result, PathInfo, identity.Endpoint, error) {
	var none identity.Endpoint
	if len(paths) == 0 {
		return nil, nil, PathInfo{}, none, ErrNoRoute
	}
	actx, cancelAll := context.WithCancel(ctx)
	results := make(chan attempt, len(paths))
	var mu sync.Mutex
	won := false
	sem := make(chan struct{}, 1)
	// gate lets one attempt at a time commit (send its Request). A holder that takes longer
	// than HedgeDelay stops blocking the others: it is probably on a dying path.
	gate := func() (func(), error) {
		mu.Lock()
		lost := won
		mu.Unlock()
		if lost {
			return nil, errLost
		}
		t := time.NewTimer(HedgeDelay)
		defer t.Stop()
		select {
		case sem <- struct{}{}:
		case <-t.C:
			return nil, nil
		case <-actx.Done():
			return nil, errLost
		}
		mu.Lock()
		lost = won
		mu.Unlock()
		if lost {
			<-sem
			return nil, errLost
		}
		return func() { <-sem }, nil
	}
	run := func(ep identity.Endpoint) {
		a := attempt{ep: ep, started: time.Now()}
		defer func() { results <- a }()
		t := m.usableTransport(ep)
		if t == nil {
			a.err = &dialError{fmt.Errorf("%s: transport not available", ep.Transport)}
			return
		}
		m.authorize(t, device, ep, call.Secret)
		dctx, cancel := context.WithTimeout(actx, DialTimeout)
		defer cancel()
		conn, err := t.Dial(dctx, ep.Address)
		if err != nil {
			a.err = &dialError{fmt.Errorf("%s: %w", ep.Transport, err)}
			return
		}
		stop := context.AfterFunc(actx, func() { conn.Close() })
		c := call
		c.Gate = gate
		res, err := wire.Dial(conn, m.kr, c)
		if !stop() && err == nil {
			conn.Close()
			err = errLost
		}
		if err != nil {
			a.err = fmt.Errorf("%s: %w", ep.Transport, err)
			return
		}
		if !bytes.Equal(res.Bundle.Person.Person, wantPerson) {
			conn.Close()
			a.err = ErrWrongPeer
			return
		}
		a.conn, a.res = conn, res
	}

	started := map[string]time.Time{}
	next, running := 0, 0
	var hedge <-chan time.Time
	launch := func() {
		ep := paths[next]
		next++
		running++
		started[pathKey(ep)] = time.Now()
		go run(ep)
		if next < len(paths) {
			hedge = time.After(HedgeDelay)
		} else {
			hedge = nil
		}
	}
	finish := func() {
		cancelAll()
		n := running
		go func() { // collect the losers; one may have slipped through the gate
			for range n {
				if a := <-results; a.conn != nil {
					a.conn.Close()
				}
			}
		}()
	}
	var errs []error
	var failed []string // why better paths did not carry the session
	launch()
	for running > 0 || next < len(paths) {
		if running == 0 {
			launch()
			continue
		}
		select {
		case <-hedge:
			launch()
		case a := <-results:
			m.log.Debug("DBG attempt", "peer", short(device), "path", a.ep.Transport, "err", a.err, "op", call.Op)
			running--
			delete(started, pathKey(a.ep))
			if a.err == nil {
				mu.Lock()
				won = true
				mu.Unlock()
				// Paths that started before the winner and still haven't answered are slow.
				for k, at := range started {
					if at.Before(a.started) {
						name, addr, _ := strings.Cut(k, "|")
						m.pathFailed(device, identity.Endpoint{Transport: name, Address: addr},
							fmt.Sprintf("slower than %s", a.ep.Transport))
						failed = append(failed, name+" was slower")
					}
				}
				finish()
				m.pathWorked(device, a.ep)
				return a.conn, a.res, PathInfo{Transport: a.ep.Transport, Reason: m.why(device, a.ep, paths, failed)}, a.ep, nil
			}
			if definitive(a.err) {
				finish()
				return nil, nil, PathInfo{}, none, a.err
			}
			if blamable(ctx, a.err) {
				m.pathFailed(device, a.ep, explain(a.err))
				failed = append(failed, a.ep.Transport+" failed: "+explain(a.err))
			}
			errs = append(errs, a.err)
			if next < len(paths) {
				launch()
			}
		case <-ctx.Done():
			finish()
			return nil, nil, PathInfo{}, none, ctx.Err()
		}
	}
	cancelAll()
	return nil, nil, PathInfo{}, none, errors.Join(errs...)
}

// why explains the choice of ep among ranked paths.
func (m *Manager) why(device ed25519.PublicKey, ep identity.Endpoint, ranked []identity.Endpoint, failed []string) string {
	if len(failed) > 0 {
		return strings.Join(failed, "; ") + "; " + ep.Transport + " answered"
	}
	var reasons []string
	for _, r := range ranked {
		if r == ep {
			break
		}
		if preference(r.Transport) >= preference(ep.Transport) {
			continue
		}
		if f := m.failState(device, r); f != nil {
			reasons = append(reasons, r.Transport+" is in backoff ("+f.err+")")
		}
	}
	if len(reasons) == 0 {
		return "best path"
	}
	return strings.Join(reasons, "; ") + "; using " + ep.Transport
}

// explain shortens a dial error for PathInfo and PathState.
func explain(err error) string {
	msg := err.Error()
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout(), strings.Contains(msg, "i/o timeout"):
		return "timed out"
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	case errors.Is(err, wire.ErrTruncated):
		return "the connection broke during the handshake"
	}
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return msg
}

// upgradeDue returns the best path, ranked above the session's own, that should be probed
// now or later (when), for a session this device dialed. ok is false when there is none.
func (m *Manager) upgradeDue(s *Session) (ep identity.Endpoint, when time.Time, ok bool) {
	if s.path.Inbound || s.path.Transport == "" {
		return ep, when, false
	}
	rec, found := m.endpointsFor(s.peer.Person, s.peer.Device)
	if !found {
		return ep, when, false
	}
	cur := preference(s.path.Transport)
	for _, r := range rec.Endpoints {
		if preference(r.Transport) >= cur || pathKey(r) == s.pathKey || m.usableTransport(r) == nil {
			continue
		}
		at := time.Now()
		if f := m.failState(s.peer.Device, r); f != nil {
			at = f.nextProbe
		} else if !s.started.IsZero() && time.Since(s.started) < StableFor/2 {
			// A healthy better path that didn't carry this session lost a hedge or wasn't
			// known when it was dialed: look again a little later, not in a loop.
			at = s.started.Add(StableFor / 2)
		}
		if !ok || preference(r.Transport) < preference(ep.Transport) || at.Before(when) {
			ep, when, ok = r, at, true
		}
	}
	return ep, when, ok
}

// probePath runs one probe handshake (wire.OpProbe) over ep and closes it.
func (m *Manager) probePath(ctx context.Context, person, device ed25519.PublicKey, ep identity.Endpoint) error {
	t := m.usableTransport(ep)
	if t == nil {
		return ErrNoRoute
	}
	select {
	case m.probes <- struct{}{}:
		defer func() { <-m.probes }()
	case <-ctx.Done():
		return ctx.Err()
	}
	timeout := ProbeTimeout
	if slow(ep.Transport) {
		timeout = SlowProbeTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bundle, err := m.bundleFor(person)
	if err != nil {
		return err
	}
	m.authorize(t, device, ep, nil)
	conn, err := t.Dial(pctx, ep.Address)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(pctx, func() { conn.Close() })
	defer stop()
	res, err := wire.Dial(conn, m.kr, wire.Call{Op: wire.OpProbe, Want: device, Bundle: bundle})
	if err != nil {
		if pctx.Err() != nil {
			return fmt.Errorf("no answer within %s", timeout)
		}
		return err
	}
	if !bytes.Equal(res.Bundle.Person.Person, person) {
		return ErrWrongPeer
	}
	return nil
}

// tryUpgrade probes ep for the session s; when that makes ep preferred, the session moves
// to it: a new session over ep replaces s on both sides (same caller, newer wins).
func (m *Manager) tryUpgrade(ctx context.Context, s *Session, ep identity.Endpoint) {
	dev := s.peer.Device
	wasFailing := m.failState(dev, ep) != nil
	err := m.probePath(ctx, s.peer.Person, dev, ep)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		if blamable(ctx, err) {
			m.pathFailed(dev, ep, explain(err))
		}
		return
	}
	if !m.pathOK(dev, ep) || !s.Alive() {
		return
	}
	reason := ep.Transport + " is better"
	if wasFailing {
		reason = ep.Transport + " is stable again"
		m.log.Info("a path works again; moving the session to it", "peer", short(dev), "path", ep.Transport)
	}
	m.dialPath(ctx, s, ep, reason)
}

// dialPath dials a new session to s's device over exactly ep; on success it replaces s.
func (m *Manager) dialPath(ctx context.Context, s *Session, ep identity.Endpoint, reason string) {
	dm := m.dialMutex(s.peer.Device)
	if dm == nil {
		return
	}
	dm.Lock()
	defer dm.Unlock()
	if cur, ok := m.Session(s.peer.Device); !ok || cur != s {
		return // it changed meanwhile
	}
	bundle, err := m.bundleFor(s.peer.Person)
	if err != nil {
		return
	}
	conn, res, _, _, err := m.dialHedged(ctx, s.peer.Device, s.peer.Person, []identity.Endpoint{ep},
		wire.Call{Op: wire.OpSession, Want: s.peer.Device, Bundle: bundle})
	if err != nil {
		return
	}
	anon := identity.AnonymousTransport(ep.Transport)
	peer, err := m.admit(res.Bundle, res.Device, anon)
	if err != nil {
		conn.Close()
		return
	}
	m.start(conn, true, peer, m.kr.DevicePublic(), PathInfo{Transport: ep.Transport, Reason: reason}, pathKey(ep), anon)
}

// checkSessions makes sure every session still answers after a network change: one whose
// local address is gone is closed at once, and one that doesn't answer a request within
// the probe timeout is closed, so its keeper redials over the best path there is now.
func (m *Manager) checkSessions() {
	local := localIPs()
	for _, s := range m.Sessions() {
		if ta, ok := s.mux.LocalAddr().(*net.TCPAddr); ok && local != nil && !ta.IP.IsLoopback() && !local[ta.IP.String()] {
			m.log.Info("closing a session: its local address is gone", "peer", short(s.peer.Device))
			s.Close()
			continue
		}
		m.goBG(func() {
			timeout := ProbeTimeout
			if slow(s.path.Transport) || s.path.Inbound {
				timeout = SlowProbeTimeout
			}
			ctx, cancel := context.WithTimeout(m.ctx, timeout)
			defer cancel()
			if _, err := FetchBundle(ctx, s); err != nil && m.ctx.Err() == nil && s.Alive() {
				m.log.Info("closing a session that stopped answering after a network change", "peer", short(s.peer.Device))
				s.Close()
			}
		})
	}
}

// localIPs lists this machine's addresses (nil if they can't be read, in which case
// nothing is closed for lack of one).
func localIPs() map[string]bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			out[ipn.IP.String()] = true
		}
	}
	return out
}

// pathsDue makes every failed path's next probe due now (after a network change).
func (m *Manager) pathsDue() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for _, l := range m.links {
		for _, f := range l.fails {
			f.nextProbe = now
		}
	}
}

// noWG drops WireGuard paths: before pairing or joining the callee can't know this device's
// WireGuard key, so a WireGuard handshake could only fail (and hold its retry timer).
func noWG(paths []identity.Endpoint) []identity.Endpoint {
	return slices.DeleteFunc(slices.Clone(paths), func(ep identity.Endpoint) bool { return ep.Transport == "wg" })
}
