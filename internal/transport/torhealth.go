package transport

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"
)

// Tor health: tor does not reliably notice when the machine's network
// changes (VPN on/off, Wi-Fi switch). It keeps reporting "bootstrapped 100%"
// while the circuits that announce our onions are dead, so the node looks
// online but nobody can reach it. Two loops fix that:
//
//   - watchNetwork restarts tor when the set of local IP addresses changes.
//   - selfCheck periodically dials our own onion through our own tor. Only a
//     successful dial counts as "reachable"; Status reports StateStarting
//     with an explanation until then. Repeated failures restart tor.
//
// For onions with client authorization, the Tor transport adds its own
// "self" key to every publication so the self-check can get in.

var (
	netPollEvery       = 5 * time.Second
	netSettle          = 3 * time.Second
	selfCheckFirst     = 20 * time.Second
	selfCheckRetry     = 30 * time.Second
	selfCheckEvery     = 3 * time.Minute
	selfCheckTimeout   = 60 * time.Second
	restartAfterFails  = 4 // consecutive failed self-checks (~2 minutes)
	minAutoRestartGap  = 10 * time.Minute
	ignoredIfacePrefix = []string{"lo", "veth", "docker", "br-", "virbr", "cni", "flannel"}
)

// Restart stops the running tor; the supervisor starts a fresh one and
// re-registers every onion and client key. Safe to call at any time.
func (t *Tor) Restart(reason string) {
	t.mu.Lock()
	proc, ctrl := t.proc, t.ctrl
	t.restartReason = reason
	t.reachable = false
	t.lastRestart = time.Now()
	t.mu.Unlock()
	slog.Info("tor: restarting", "reason", reason)
	if ctrl != nil {
		ctrl.close() // TAKEOWNERSHIP: tor exits when this connection closes
	}
	if proc != nil {
		proc.Stop()
	}
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

// watchNetwork restarts tor after the network changes and settles.
func (t *Tor) watchNetwork(runCtx context.Context) {
	last := netSignature()
	for {
		select {
		case <-runCtx.Done():
			return
		case <-time.After(netPollEvery):
		}
		cur := netSignature()
		if cur == last {
			continue
		}
		// Wait for the change to settle (VPNs add interfaces in steps).
		select {
		case <-runCtx.Done():
			return
		case <-time.After(netSettle):
		}
		cur = netSignature()
		if cur == last {
			continue
		}
		last = cur
		t.Restart("the network changed")
	}
}

// probeTarget picks the onion to self-check: the node's own if published,
// otherwise any. Returns "" when nothing is published.
func (t *Tor) probeTarget() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if o, ok := t.onions["node"]; ok {
		return o.serviceID + ".onion"
	}
	ids := make([]string, 0, len(t.onions))
	for id := range t.onions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return ""
	}
	return t.onions[ids[0]].serviceID + ".onion"
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
		t.reachable = false
		if was {
			t.reachDetail = "lost contact with the Tor network; checking again"
		} else {
			t.reachDetail = fmt.Sprintf("connected to Tor, but your address isn't reachable yet (check %d)", fails)
		}
		canRestart := time.Since(t.lastRestart) > minAutoRestartGap
		t.mu.Unlock()
		wait = selfCheckRetry
		if fails >= restartAfterFails {
			fails = 0
			if canRestart {
				t.Restart("your address stayed unreachable")
				wait = selfCheckFirst
			} else {
				t.mu.Lock()
				t.reachDetail = "your address is still unreachable. If a VPN or firewall is on, it may be blocking Tor"
				t.mu.Unlock()
			}
		}
	}
}
