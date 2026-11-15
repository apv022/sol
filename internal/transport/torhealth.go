package transport

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Tor health: tor does not reliably notice when the machine's network
// changes (VPN on/off, Wi-Fi switch). It keeps reporting "bootstrapped 100%"
// while the circuits that announce our onions are dead, so the node looks
// online but nobody can reach it. Three loops fix that (the supervisor in
// tor.go does the restarting):
//
//   - watchNetwork notices when the set of local IP addresses changes and
//     reports it to NetworkChanged, as an OS that announces network changes
//     (Android) does directly, and tor's CLOCK_JUMPED (a wake from sleep).
//   - netLoop debounces those reports: one restart per burst, at most one
//     per netRestartGap, none while there is no network to use, none for a
//     tor that started after the change, and a cut-short backoff instead of
//     a restart while the supervisor is waiting to retry.
//   - selfCheck periodically dials our own onion through our own tor.
//     Repeated failures mark the node unreachable and then restart tor.
//
// "Reachable" starts when tor reports that a directory accepted our
// descriptor (noteDescEvent), or when a self-check succeeds, whichever comes
// first. The self-check alone is not a good first signal: it fetches the
// descriptor from one directory picked at random, tor asks only one at a
// time, and a slow one stalls the fetch for two minutes. A node that was
// published within seconds then reported "not reachable yet" for minutes.
// For the same reason one failed check never marks a reachable node
// unreachable; it takes failsBeforeUnreachable in a row.
//
// For onions with client authorization, the Tor transport adds its own
// "self" key to every publication so the self-check can get in.

var (
	netPollEvery = 5 * time.Second
	// netSettle: a network change is acted on once no further change has
	// been reported for this long (VPNs add interfaces in steps), but no
	// later than netSettleMax after the first one.
	netSettle    = 3 * time.Second
	netSettleMax = 30 * time.Second
	// netRestartGap is the least time between two restarts for network
	// changes, so a flapping network cannot keep tor from ever connecting.
	netRestartGap     = 30 * time.Second
	selfCheckFirst    = 20 * time.Second
	selfCheckRetry    = 30 * time.Second
	selfCheckEvery    = 3 * time.Minute
	selfCheckTimeout  = 60 * time.Second
	restartAfterFails = 4 // consecutive failed self-checks
	// failsBeforeUnreachable is how many self-checks in a row must fail
	// before a published node is reported unreachable. Two checks span more
	// than the two minutes one stalled directory request can take.
	failsBeforeUnreachable = 2
	minAutoRestartGap      = 10 * time.Minute
	ignoredIfacePrefix     = []string{"lo", "veth", "docker", "br-", "virbr", "cni", "flannel"}
)

// Restart replaces the running tor with a fresh one, re-registering every
// onion and client key, at once: a pending backoff is cut short. Safe to
// call at any time.
func (t *Tor) Restart(reason string) {
	slog.Info("tor: restarting", "reason", reason)
	t.abandon(reason, true)
}

// NetworkChanged tells the transport that the machine's network may have
// changed (reason says how, for the log). It never blocks. Bursts are
// debounced by netLoop, so calling it on every OS notification is fine.
func (t *Tor) NetworkChanged(reason string) {
	select {
	case t.netc <- netEvent{reason: reason, at: time.Now()}:
	default: // one is pending already; netLoop folds the burst into it
	}
}

// netLoop turns NetworkChanged calls into at most one action per burst.
func (t *Tor) netLoop(runCtx context.Context) {
	var lastRestart time.Time
	for {
		var ev netEvent
		select {
		case <-runCtx.Done():
			return
		case ev = <-t.netc:
		}
		// Debounce: wait until the reports stop (or netSettleMax), and no
		// sooner than netRestartGap after the last restart for this reason.
		settle := time.NewTimer(netSettle)
		gap := time.NewTimer(max(time.Until(lastRestart.Add(netRestartGap)), 0))
		settled, gapped := false, false
		for !settled || !gapped {
			select {
			case <-runCtx.Done():
				settle.Stop()
				gap.Stop()
				return
			case more := <-t.netc:
				ev.reason = more.reason
				if !settled && time.Since(ev.at) < netSettleMax {
					settle.Reset(min(netSettle, max(netSettleMax-time.Since(ev.at), 0)))
				}
			case <-settle.C:
				settled = true
			case <-gap.C:
				gapped = true
			}
		}
		if t.onNetworkChanged(ev) {
			lastRestart = time.Now()
		}
	}
}

// onNetworkChanged acts on one debounced network change and reports whether
// it restarted tor.
func (t *Tor) onNetworkChanged(ev netEvent) (restarted bool) {
	t.mu.Lock()
	phase, launched, stopping := t.phase, t.launchedAt, t.stopping
	t.mu.Unlock()
	switch {
	case stopping:
	case phase == PhaseRetrying || phase == PhaseLaunching:
		// The supervisor is waiting out a backoff: try now, the new network
		// may be the fix.
		slog.Info("tor: "+ev.reason+"; trying again now", "phase", phase)
		select {
		case t.kick <- struct{}{}:
		default:
		}
	case !torOnline():
		// Nothing to connect over: a new tor would do no better. The change
		// that brings the network back restarts it.
		slog.Debug("tor: " + ev.reason + "; no network, waiting")
	case launched.After(ev.at):
		// This tor started after the change and already uses the new network.
	default:
		slog.Info("tor: restarting", "reason", ev.reason)
		t.abandon(ev.reason, true)
		return true
	}
	return false
}

// netSignature summarises the machine's usable network addresses. A change
// means routes probably changed too, so tor's connections may be stale.
func netSignature() string {
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var parts []string
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ignoredInterface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			parts = append(parts, ifc.Name+"="+ipn.IP.String())
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func ignoredInterface(name string) bool {
	for _, p := range ignoredIfacePrefix {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// watchNetwork reports a change of the machine's addresses to
// NetworkChanged, which debounces it.
func (t *Tor) watchNetwork(runCtx context.Context) {
	last := netSignature()
	for {
		select {
		case <-runCtx.Done():
			return
		case <-time.After(netPollEvery):
		}
		if cur := netSignature(); cur != last {
			last = cur
			t.NetworkChanged("the network changed")
		}
	}
}

// probeTarget picks the onion to self-check: the node's own if published,
// otherwise any. Returns "" when nothing is published.
func (t *Tor) probeTarget() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if o, ok := t.onions["node"]; ok {
		return o.address()
	}
	ids := make([]string, 0, len(t.onions))
	for id, o := range t.onions {
		if o.serviceID != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return ""
	}
	return t.onions[ids[0]].address()
}

// stillReachable decides whether a node stays "reachable" after fails
// self-checks in a row have failed.
func stillReachable(was bool, fails int) bool { return was && fails < failsBeforeUnreachable }

// noteEvent handles one asynchronous event line from tor's second control
// connection (launch subscribes to HS_DESC and STATUS_GENERAL).
func (t *Tor) noteEvent(line string) {
	f := strings.Fields(line)
	if len(f) < 4 {
		return
	}
	switch f[1] {
	case "HS_DESC":
		t.noteDescEvent(line)
	case "STATUS_GENERAL":
		t.noteStatusGeneral(f[3], line)
	}
}

// noteStatusGeneral handles "650 STATUS_GENERAL <severity> <action> ...".
//
//   - CLOCK_SKEW: this machine's clock is wrong, so tor cannot accept the
//     network's documents and bootstrap stalls. A restart would not help;
//     the user has to fix the clock, so Status says so.
//   - CLOCK_JUMPED: time jumped (the machine slept). Its network has likely
//     changed and tor's circuits are stale: treated as a network change.
func (t *Tor) noteStatusGeneral(action, line string) {
	switch action {
	case "CLOCK_SKEW":
		msg := "This machine's clock looks wrong; Tor cannot connect until it is set right"
		if sec, err := strconv.Atoi(fieldValue(line, "SKEW")); err == nil && sec != 0 {
			d := (time.Duration(max(sec, -sec)) * time.Second).Round(time.Minute)
			dir := "ahead"
			if sec < 0 {
				dir = "behind"
			}
			if d >= time.Minute {
				msg = fmt.Sprintf("This machine's clock is about %s %s; Tor cannot connect until it is set right", d, dir)
			}
		}
		t.mu.Lock()
		t.clockSkew = msg
		t.mu.Unlock()
	case "CLOCK_JUMPED":
		t.NetworkChanged("the machine woke up")
	}
}

// noteDescEvent handles one HS_DESC event line from tor:
//
//	650 HS_DESC UPLOADED <onion address without .onion> <auth> <directory> ...
//
// UPLOADED means a directory accepted the descriptor of one of our onions.
// Tor builds a descriptor only after the onion's introduction circuits are
// up, so from then on peers can find and reach it. Events about other
// onions (our own lookups of peers) and other actions are ignored.
func (t *Tor) noteDescEvent(line string) {
	f := strings.Fields(line)
	if len(f) < 4 || f[1] != "HS_DESC" || f[2] != "UPLOADED" {
		return
	}
	target := strings.TrimSuffix(t.probeTarget(), ".onion")
	if target == "" || f[3] != target {
		return
	}
	t.mu.Lock()
	if !t.reachable {
		t.reachable, t.reachDetail = true, ""
		slog.Debug("tor: descriptor accepted by a directory; reachable")
	}
	t.updateReadyLocked()
	t.mu.Unlock()
}

// selfCheck periodically verifies our onions are reachable from the Tor
// network and restarts tor if they stay unreachable.
func (t *Tor) selfCheck(runCtx context.Context) {
	wait := selfCheckFirst
	fails := 0
	for {
		select {
		case <-runCtx.Done():
			return
		case <-time.After(wait):
		}
		target := t.probeTarget()
		t.mu.Lock()
		bootstrapped := t.state == StateReady
		t.mu.Unlock()
		if target == "" || !bootstrapped {
			wait, fails = selfCheckFirst, 0
			continue
		}

		ctx, cancel := context.WithTimeout(runCtx, selfCheckTimeout)
		conn, err := t.Dial(ctx, target)
		cancel()
		if err == nil {
			conn.Close()
			t.mu.Lock()
			t.reachable, t.reachDetail = true, ""
			t.updateReadyLocked()
			t.mu.Unlock()
			wait, fails = selfCheckEvery, 0
			continue
		}
		if runCtx.Err() != nil {
			return
		}
		fails++
		slog.Debug("tor: self-check failed", "n", fails, "err", err)
		t.mu.Lock()
		was := t.reachable
		t.reachable = stillReachable(was, fails)
		switch {
		case t.reachable:
			// One slow directory; not evidence that we are offline.
		case was:
			t.reachDetail = "lost contact with the Tor network; checking again"
		default:
			t.reachDetail = fmt.Sprintf("connected to Tor, but your address isn't reachable yet (check %d)", fails)
		}
		canRestart := time.Since(t.lastAutoRestart) > minAutoRestartGap
		t.mu.Unlock()
		wait = selfCheckRetry
		if fails >= restartAfterFails {
			fails = 0
			if canRestart {
				t.mu.Lock()
				t.lastAutoRestart = time.Now()
				t.mu.Unlock()
				slog.Info("tor: restarting: your address stayed unreachable")
				t.abandon("your address stayed unreachable", false)
				wait = selfCheckFirst
			} else {
				t.mu.Lock()
				t.reachDetail = "your address is still unreachable. If a VPN or firewall is on, it may be blocking Tor"
				t.mu.Unlock()
			}
		}
	}
}
