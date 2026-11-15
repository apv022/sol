package node

// Liveness of peer connections (docs/ARCHITECTURE.md, "Peer liveness").
//
// Sol streams are plain byte pipes after the handshake, so a path that dies
// without a FIN or RST (a phone that changed networks or slept, a NAT that
// dropped its mapping, a WireGuard tunnel whose UDP stopped) would leave them
// hanging forever. The supervisor here notices that and recovers:
//
//   - Every path that carries a live outbound connection is probed every
//     Liveness.ProbeEvery (with jitter) by a "list" request over exactly that
//     address. Liveness.Misses failed probes in a row mean the path is dead:
//     its connections are closed (apps and tunnel clients reconnect, and a
//     tunnel redials on the next connection) and the peer is redialled at
//     once over the best remaining path.
//   - A peer that can't be reached is redialled with exponential backoff and
//     jitter (Liveness.RedialMin..RedialMax), only while something wants it
//     (a saved tunnel, or connections that were just cut), one probe per
//     peer at a time and at most maxProbes at once: no storm.
//   - A direct path (WireGuard, TLS) that failed is demoted behind Tor and
//     probed in the background on its backoff schedule. It is preferred
//     again only after Liveness.DirectStableProbes successful probes over at
//     least Liveness.DirectStableFor (hysteresis); any failure meanwhile
//     doubles the backoff. A flaky path therefore drifts towards being tried
//     rarely instead of flapping.
//   - Node.NetworkChanged, or a wall-clock jump that means the machine slept,
//     re-probes everything right away (debounced): in-use paths, direct
//     paths in backoff, and peers waiting for a redial.
//
// Invariants: probes are ordinary authenticated "list" requests (no new wire
// message); they never touch the peer's record on either side; nothing here
// runs unless Run is running.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"time"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/events"
	"github.com/apv022/sol/internal/protocol"
	"github.com/apv022/sol/internal/transport"
)

// Liveness tunes how the node checks and recovers its peer connections.
// The zero value of any field means its default (in brackets). Tests use
// short values; nothing else needs to change them.
type Liveness struct {
	// ProbeEvery: how often a path carrying a live outbound connection is
	// probed [30s]. Each interval gets ±20% jitter.
	ProbeEvery time.Duration
	// ProbeTimeout bounds one probe over a direct or LAN path [10s];
	// SlowProbeTimeout over Tor or I2P [60s].
	ProbeTimeout, SlowProbeTimeout time.Duration
	// Misses: failed probes in a row that mean the path is dead [2].
	Misses int
	// RedialMin and RedialMax bound the backoff for redialling a peer that
	// can't be reached [2s, 5m].
	RedialMin, RedialMax time.Duration
	// RedialFor: how long a peer whose connections were cut is redialled
	// when no saved tunnel wants it [30m].
	RedialFor time.Duration
	// DirectBackoff and DirectBackoffMax: how long a failed direct path
	// waits before its next background probe, doubling per failure [1m, 1h].
	DirectBackoff, DirectBackoffMax time.Duration
	// DirectStableFor and DirectStableProbes: a failed direct path is
	// preferred again once this many probes in a row succeeded over at
	// least this long [2m, 3].
	DirectStableFor    time.Duration
	DirectStableProbes int
	// NetChangeDebounce collapses a burst of network-change signals [2s].
	NetChangeDebounce time.Duration
	// SleepCheckEvery and SleepJump: the wall clock is compared with the
	// monotonic clock this often; running ahead by more than SleepJump
	// means the machine slept, which counts as a network change [5s, 20s].
	SleepCheckEvery, SleepJump time.Duration
	// Tick: how often the supervisor looks for due work [1s].
	Tick time.Duration
	// Wall reads the wall clock without its monotonic reading (a test hook)
	// [time.Now().Round(0)].
	Wall func() time.Time
}

func (l Liveness) withDefaults() Liveness {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&l.ProbeEvery, 30*time.Second)
	def(&l.ProbeTimeout, 10*time.Second)
	def(&l.SlowProbeTimeout, time.Minute)
	def(&l.RedialMin, 2*time.Second)
	def(&l.RedialMax, 5*time.Minute)
	def(&l.RedialFor, 30*time.Minute)
	def(&l.DirectBackoff, directBackoff)
	def(&l.DirectBackoffMax, directBackoffMax)
	def(&l.DirectStableFor, 2*time.Minute)
	def(&l.NetChangeDebounce, 2*time.Second)
	def(&l.SleepCheckEvery, 5*time.Second)
	def(&l.SleepJump, 20*time.Second)
	def(&l.Tick, time.Second)
	if l.Misses <= 0 {
		l.Misses = 2
	}
	if l.DirectStableProbes <= 0 {
		l.DirectStableProbes = 3
	}
	if l.Wall == nil {
		l.Wall = func() time.Time { return time.Now().Round(0) }
	}
	return l
}

// lv is the node's liveness settings with defaults filled in (also for a
// bare Node in unit tests).
func (n *Node) lv() Liveness { return n.opts.Liveness.withDefaults() }

// maxProbes caps concurrent probes, so a network change with many peers
// doesn't open a burst of Tor circuits.
const maxProbes = 4

// jitter spreads d by ±20%, so peers and nodes don't probe in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d*8/10 + time.Duration(rand.Int64N(int64(d)*4/10+1))
}

// peerLink is the supervisor's state for one peer. Guarded by n.mu.
type peerLink struct {
	via, addr string    // the path that last worked
	lastOK    time.Time // last dial or probe that reached the peer
	misses    map[string]int
	nextCheck time.Time // next probe of the in-use paths

	down       bool // the last redial failed
	downSince  time.Time
	lastErr    string
	redialAt   time.Time
	redialWait time.Duration
	lostAt     time.Time // connections were cut by a dead path (zero: none pending)

	fallback   string // why the last connection didn't go direct
	fallbackAt time.Time

	// incompat: the last handshake found no common protocol version
	// (compat.go). Cleared when a handshake succeeds again.
	incompat *protocol.IncompatibleError

	busy bool // a probe for this peer is running
}

// linkLocked returns (creating) peer id's link. Called with n.mu held.
func (n *Node) linkLocked(id string) *peerLink {
	if n.links == nil {
		n.links = map[string]*peerLink{}
	}
	l := n.links[id]
	if l == nil {
		l = &peerLink{misses: map[string]int{}}
		n.links[id] = l
	}
	return l
}

// noteReached records that addr (over transport via) just reached peer id.
func (n *Node) noteReached(id, addr, via string) {
	if id == "" {
		return
	}
	n.mu.Lock()
	l := n.linkLocked(id)
	l.via, l.addr, l.lastOK = via, addr, time.Now()
	l.down, l.lastErr, l.redialWait = false, "", 0
	wasIncompat := l.incompat != nil
	l.incompat = nil
	if isFast(addr) {
		l.fallback, l.fallbackAt = "", time.Time{}
	}
	n.mu.Unlock()
	if wasIncompat {
		p, _ := n.peers.Get(id)
		n.log.Add(events.KindTransport, nameOr(p)+" now runs a compatible Sol; connections work again")
	}
}

// noteFallback records why peer id isn't being reached directly.
func (n *Node) noteFallback(id, why string) {
	if id == "" || why == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	l := n.linkLocked(id)
	l.fallback, l.fallbackAt = why, time.Now()
}

// NetworkChanged tells every transport that watches the network
// (transport.NetworkWatcher) that the machine's network may have changed,
// for example when a mobile OS announces it, and makes the peer supervisor
// re-check every peer path right away. Bursts are fine: both debounce.
func (n *Node) NetworkChanged(reason string) {
	for _, t := range n.reg.List() {
		if w, ok := t.(transport.NetworkWatcher); ok {
			w.NetworkChanged(reason)
		}
	}
	n.kickNetwork(reason)
}

func (n *Node) kickNetwork(reason string) {
	select {
	case n.netKick <- reason:
	default: // a burst: the debounce is already pending
	}
}

// supervise runs the liveness checks until ctx ends (started by Run).
func (n *Node) supervise(ctx context.Context) {
	lv := n.lv()
	tick := time.NewTicker(lv.Tick)
	defer tick.Stop()
	clock := time.NewTicker(lv.SleepCheckEvery)
	defer clock.Stop()
	lastMono, lastWall := time.Now(), lv.Wall()
	var debounce <-chan time.Time
	var reasons []string
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-n.netKick:
			if len(reasons) < 8 && !containsStr(reasons, r) {
				reasons = append(reasons, r)
			}
			if debounce == nil {
				debounce = time.After(lv.NetChangeDebounce)
			}
		case <-debounce:
			debounce = nil
			n.networkChanged(strings.Join(reasons, ", "))
			reasons = nil
		case <-clock.C:
			mono, wall := time.Now(), lv.Wall()
			// The monotonic clock stops while the machine sleeps (Linux,
			// Android, macOS); the wall clock doesn't.
			if skew := wall.Sub(lastWall) - mono.Sub(lastMono); skew > lv.SleepJump || skew < -lv.SleepJump {
				n.log.Add(events.KindTransport, fmt.Sprintf("the clock jumped by %s (the machine slept?); checking peer connections", skew.Round(time.Second)))
				n.kickNetwork("resumed from sleep")
			}
			lastMono, lastWall = mono, wall
		case <-tick.C:
			n.checkPeers()
		}
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// networkChanged makes everything due now: probes of in-use paths, direct
// paths in backoff, and redials. Outbound TCP connections whose local
// address is gone (the Wi-Fi they used is off) are closed at once.
func (n *Node) networkChanged(reason string) {
	if reason == "" {
		reason = "network changed"
	}
	n.log.Add(events.KindTransport, "network change ("+reason+"): re-checking peer connections")
	local := localIPs()
	var dead []net.Conn
	now := time.Now()
	n.mu.Lock()
	for c, ci := range n.conns {
		if !ci.outbound || (ci.via != "direct" && ci.via != "clearnet") || local == nil {
			continue
		}
		if ta, ok := c.LocalAddr().(*net.TCPAddr); ok && !ta.IP.IsLoopback() && !local[ta.IP.String()] {
			dead = append(dead, c)
			n.linkLocked(ci.peer).lostAt = now
		}
	}
	for _, l := range n.links {
		l.nextCheck = now
		l.misses = map[string]int{}
		if l.down {
			l.redialAt, l.redialWait = now, 0
		}
	}
	for k, df := range n.directFail {
		df.nextProbe = now
		n.directFail[k] = df
	}
	clear(n.lastDirectRefresh) // a peer reached over Tor is asked for its new address
	n.mu.Unlock()
	for _, c := range dead {
		c.Close()
	}
	n.checkPeers()
}

// localIPs lists this machine's addresses (nil if they can't be read, in
// which case nothing is closed for lack of one).
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

// checkPeers starts the probes and redials that are due.
func (n *Node) checkPeers() {
	if n.ctx == nil || n.ctx.Err() != nil {
		return
	}
	lv := n.lv()
	now := time.Now()
	wanted := map[string]bool{}
	for _, t := range n.cfg.TunnelList() {
		wanted[t.Peer] = true
	}
	direct := n.directUsable()
	type job struct {
		p    config.Peer
		kind int // 0 in-use paths, 1 redial, 2 direct probe
		args []string
	}
	var jobs []job
	n.mu.Lock()
	inUse := map[string][]string{}
	for _, ci := range n.conns {
		if ci.outbound && ci.addr != "" && !containsStr(inUse[ci.peer], ci.addr) {
			inUse[ci.peer] = append(inUse[ci.peer], ci.addr)
		}
	}
	for _, p := range n.peers.List() {
		l := n.linkLocked(p.ID)
		if l.busy {
			continue
		}
		if !l.lostAt.IsZero() && now.Sub(l.lostAt) > lv.RedialFor {
			l.lostAt = time.Time{} // gave up: nothing asked for it since
		}
		switch {
		case len(inUse[p.ID]) > 0 && !now.Before(l.nextCheck):
			l.nextCheck = now.Add(jitter(lv.ProbeEvery))
			jobs = append(jobs, job{p, 0, inUse[p.ID]})
		case l.down && len(inUse[p.ID]) == 0 && (wanted[p.ID] || !l.lostAt.IsZero()) && !now.Before(l.redialAt):
			jobs = append(jobs, job{p, 1, nil})
		case direct:
			for _, kind := range []string{"wg", "direct"} {
				a := p.Addrs[kind]
				df, ok := n.directFail[failKey(p.ID, a)]
				if a == "" || !ok || df.addr != a || now.Before(df.nextProbe) {
					continue
				}
				df.nextProbe = now.Add(lv.ProbeEvery) // until the probe reports
				n.directFail[failKey(p.ID, a)] = df
				jobs = append(jobs, job{p, 2, []string{a}})
				break
			}
		}
	}
	for _, j := range jobs {
		n.linkLocked(j.p.ID).busy = true
	}
	n.mu.Unlock()
	for _, j := range jobs {
		ok := n.goBG(func() {
			defer func() {
				n.mu.Lock()
				n.linkLocked(j.p.ID).busy = false
				n.mu.Unlock()
			}()
			select {
			case n.probeSem <- struct{}{}:
				defer func() { <-n.probeSem }()
			case <-n.ctx.Done():
				return
			}
			switch j.kind {
			case 0:
				n.checkPaths(j.p, j.args)
			case 1:
				n.redial(j.p)
			case 2:
				n.probeDirect(j.p, j.args[0])
			}
		})
		if !ok {
			n.mu.Lock()
			n.linkLocked(j.p.ID).busy = false
			n.mu.Unlock()
		}
	}
}

// probe sends one "list" request to p over exactly addr and reads the
// answer, within the probe timeout for that kind of path.
func (n *Node) probe(p config.Peer, addr string) error {
	t, err := n.reg.For(addr)
	if err != nil {
		return err
	}
	lv := n.lv()
	timeout := lv.ProbeTimeout
	if name := t.Name(); name == "tor" || name == "i2p" {
		timeout = lv.SlowProbeTimeout
	}
	ctx, cancel := context.WithTimeout(n.ctx, timeout)
	defer cancel()
	conn, err := t.Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	// The handshake sets deadlines of its own (protocol.HandshakeTimeout);
	// the probe's shorter one wins by closing the stream.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := n.opts.Protocol.ClientHandshake(conn, n.id, p.NodeID, protocol.Request{Op: protocol.OpList}); err != nil {
		if errors.Is(err, protocol.ErrIncompatible) {
			return n.noteIncompatible(p, err)
		}
		return orTimeout(ctx, err)
	}
	if _, err := protocol.ReadPeerInfo(conn); err != nil {
		return orTimeout(ctx, err)
	}
	return nil
}

func orTimeout(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("no answer within the probe timeout: %w", ctx.Err())
	}
	return err
}

// checkPaths probes each path p's live outbound connections use.
func (n *Node) checkPaths(p config.Peer, addrs []string) {
	lv := n.lv()
	for _, addr := range addrs {
		err := n.probe(p, addr)
		if n.ctx.Err() != nil {
			return
		}
		via := pathName(n, addr)
		n.mu.Lock()
		l := n.linkLocked(p.ID)
		if err == nil {
			delete(l.misses, addr)
			l.lastOK = time.Now()
			n.mu.Unlock()
			if isFast(addr) {
				n.noteDirectSuccess(p.ID, addr)
			}
			continue
		}
		if errors.Is(err, protocol.ErrRejected) || errors.Is(err, protocol.ErrWrongNode) || errors.Is(err, protocol.ErrIncompatible) {
			n.mu.Unlock()
			continue // the peer answered: the path works (it may have unpaired, or updated)
		}
		l.misses[addr]++
		dead := l.misses[addr] >= lv.Misses
		if dead {
			delete(l.misses, addr)
			l.lostAt = time.Now()
		}
		n.mu.Unlock()
		if !dead {
			continue
		}
		why := fmt.Sprintf("the %s path stopped answering (%s)", via, explain(err))
		n.log.Add(events.KindTransport, fmt.Sprintf("connections to %s over %s went silent; closing them and reconnecting", nameOr(p), via))
		if isFast(addr) { // first, so whoever reconnects on the close avoids it
			n.directFailed(p.ID, addr, why)
			n.noteFallback(p.ID, why)
		}
		n.closeConns(func(ci connInfo) bool { return ci.peer == p.ID && ci.outbound && ci.addr == addr })
		n.redial(p)
	}
}

func pathName(n *Node, addr string) string {
	if t, err := n.reg.For(addr); err == nil {
		return t.Name()
	}
	return "unknown"
}

// redial reaches p over the best path there is now (dialOnce's order), and
// schedules the next try with backoff if that fails.
func (n *Node) redial(p config.Peer) {
	lv := n.lv()
	ctx, cancel := context.WithTimeout(n.ctx, 2*lv.SlowProbeTimeout)
	defer cancel()
	conn, err := n.dialOnce(ctx, p, protocol.Request{Op: protocol.OpList})
	if err == nil {
		_, err = protocol.ReadPeerInfo(conn)
		conn.Close()
	}
	if n.ctx.Err() != nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	l := n.linkLocked(p.ID)
	if err == nil || errors.Is(err, protocol.ErrRejected) {
		l.down, l.redialWait, l.lostAt = false, 0, time.Time{}
		// Reached, but not directly: the peer may have moved networks, so
		// ask it for its current direct addresses (rate-limited per peer).
		if err == nil && !isFast(l.addr) && p.Addrs["wg"]+p.Addrs["direct"] != "" {
			n.goBG(func() { n.refreshDirect(p.ID) })
		}
		return
	}
	if !l.down {
		l.down, l.downSince = true, time.Now()
	}
	l.lastErr = explain(err)
	l.redialWait = min(max(l.redialWait*2, lv.RedialMin), lv.RedialMax)
	if errors.Is(err, protocol.ErrIncompatible) {
		// Only an update fixes this: check back at the slowest pace, which
		// still notices the update without a restart.
		l.lastErr, l.redialWait = err.Error(), lv.RedialMax
	}
	l.redialAt = time.Now().Add(jitter(l.redialWait))
}

// probeDirect checks a direct path that is in backoff.
func (n *Node) probeDirect(p config.Peer, addr string) {
	err := n.probe(p, addr)
	if n.ctx.Err() != nil {
		return
	}
	if errors.Is(err, protocol.ErrIncompatible) {
		return // the path works; the peer needs an update
	}
	if err != nil && !errors.Is(err, protocol.ErrRejected) {
		n.directFailed(p.ID, addr, explain(err))
		return
	}
	if n.noteDirectSuccess(p.ID, addr) {
		n.log.Add(events.KindTransport, fmt.Sprintf("the %s path to %s has worked for a while again; new connections use it", pathName(n, addr), nameOr(p)))
	}
}

// noteDirectSuccess counts a success of peer id's direct address towards
// the hysteresis, and reports whether that made the path preferred again.
func (n *Node) noteDirectSuccess(id, addr string) bool {
	lv := n.lv()
	n.mu.Lock()
	defer n.mu.Unlock()
	k := failKey(id, addr)
	df, ok := n.directFail[k]
	if !ok {
		return false
	}
	if df.addr != addr {
		delete(n.directFail, k) // about an old address
		return false
	}
	now := time.Now()
	if df.okSince.IsZero() {
		df.okSince = now
	}
	df.oks++
	if df.oks >= lv.DirectStableProbes && now.Sub(df.okSince) >= lv.DirectStableFor {
		delete(n.directFail, k)
		return true
	}
	df.nextProbe = now.Add(jitter(lv.DirectStableFor / time.Duration(max(lv.DirectStableProbes-1, 1))))
	n.directFail[k] = df
	return false
}

// PeerLink is what this node knows about how it reaches one peer, for the
// panel and the CLI. Every field comes from what the dialer and the
// liveness supervisor observed; nothing is guessed.
type PeerLink struct {
	// Via is the transport of the newest live outbound connection, or else
	// of the last dial or probe that reached the peer ("" if none since the
	// node started).
	Via string
	// Live: an outbound connection over Via is open right now.
	Live bool
	// LastOK is when a dial or probe last reached the peer.
	LastOK time.Time
	// Down: the last redial failed; NextRedial is when the next one starts
	// and LastError says why the last one failed.
	Down       bool
	DownSince  time.Time
	NextRedial time.Time
	LastError  string
	// Fallback is why the last connection didn't go direct (a direct path
	// failed or went silent); empty once a connection goes direct again.
	Fallback   string
	FallbackAt time.Time
	// Direct describes the peer's direct paths ("wg", "direct") this node
	// has had trouble with; a path that isn't listed is preferred.
	Direct map[string]DirectPath
	// Incompatible is set when the last handshake with the peer found no
	// common protocol version: one side must update Sol.
	Incompatible *protocol.IncompatibleError
}

// DirectPath is the state of one direct path that failed.
type DirectPath struct {
	// State: "backoff" (failed; tried after Tor until it proves itself
	// again) or "probation" (probes succeed again; preferred once stable).
	State     string
	FailedAt  time.Time // last failure
	Failures  int       // failures since it last was preferred
	LastError string
	// NextProbe is when the background probe next checks it; PreferredAt,
	// in probation, the earliest time it becomes preferred again.
	NextProbe   time.Time
	PreferredAt time.Time
}

// PeerLink reports how this node reaches peer id (false: no such peer).
// It is read-only and cheap; the panel may call it for every peer.
func (n *Node) PeerLink(id string) (PeerLink, bool) {
	p, ok := n.peers.Get(id)
	if !ok {
		return PeerLink{}, false
	}
	lv := n.lv()
	n.mu.Lock()
	defer n.mu.Unlock()
	var out PeerLink
	var newest time.Time
	for _, ci := range n.conns {
		if ci.peer == id && ci.outbound && ci.via != "" && ci.since.After(newest) {
			out.Via, out.Live, newest = ci.via, true, ci.since
		}
	}
	if l := n.links[id]; l != nil {
		if out.Via == "" {
			out.Via = l.via
		}
		out.LastOK, out.Down, out.DownSince, out.LastError = l.lastOK, l.down, l.downSince, l.lastErr
		if l.down {
			out.NextRedial = l.redialAt
		}
		out.Fallback, out.FallbackAt = l.fallback, l.fallbackAt
		if l.incompat != nil {
			cp := *l.incompat
			out.Incompatible = &cp
		}
	}
	for _, kind := range []string{"wg", "direct"} {
		a := p.Addrs[kind]
		df, ok := n.directFail[failKey(id, a)]
		if a == "" || !ok || df.addr != a {
			continue
		}
		dp := DirectPath{State: "backoff", FailedAt: df.at, Failures: df.failures, LastError: df.err, NextProbe: df.nextProbe}
		if df.oks > 0 {
			dp.State = "probation"
			dp.PreferredAt = df.okSince.Add(lv.DirectStableFor)
		}
		if out.Direct == nil {
			out.Direct = map[string]DirectPath{}
		}
		out.Direct[kind] = dp
	}
	return out, true
}
