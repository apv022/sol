package node

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/api"
	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/peers"
	"github.com/apv022/sol/internal/transport"
)

// impactNode is a node with only what OptionImpact reads: a saved config
// with Tor on and two peers, one reachable only over Tor and one also by a
// direct address.
func impactNode(t *testing.T) *Node {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []config.Peer{
		{ID: "a", NodeID: "na", Name: "laptop", PubKey: "k", Addrs: map[string]string{"tor": "x.onion:80"}},
		{ID: "b", NodeID: "nb", Name: "server", PubKey: "k", Addrs: map[string]string{"tor": "y.onion:80", "wg": "sol-wg:nb@10.0.0.2:7701"}},
	} {
		if err := cfg.AddPeer(p); err != nil {
			t.Fatal(err)
		}
	}
	return &Node{cfg: cfg, peers: peers.New(cfg)}
}

func TestOptionImpactNamesPeersThatBecomeUnreachable(t *testing.T) {
	n := impactNode(t)
	if _, err := n.cfg.SetOption("direct", "on"); err != nil {
		t.Fatal(err)
	}
	im, err := n.OptionImpact("tor", "off")
	if err != nil {
		t.Fatal(err)
	}
	if !im.Changes || !im.Restart || !im.Confirm {
		t.Fatalf("turning Tor off: %+v", im)
	}
	if len(im.Unreachable) != 1 || im.Unreachable[0].Name != "laptop" {
		t.Fatalf("unreachable = %+v, want only laptop (server has a direct address)", im.Unreachable)
	}
	if !strings.Contains(strings.Join(im.Effects, " "), "laptop") {
		t.Fatalf("effects don't name the peer: %q", im.Effects)
	}
	// Nothing was changed.
	if o := option(n, "tor"); o.Value != "on" {
		t.Fatalf("impact changed the setting: tor=%s", o.Value)
	}
}

func TestOptionImpactNoChangeAndUnknownKey(t *testing.T) {
	n := impactNode(t)
	im, err := n.OptionImpact("tor", "on")
	if err != nil || im.Changes || len(im.Effects) != 0 {
		t.Fatalf("same value: %+v, %v", im, err)
	}
	if _, err := n.OptionImpact("nope", "on"); err == nil {
		t.Fatal("unknown key accepted")
	}
	// Turning direct mode on exposes the IP address: it asks first, and
	// cuts nobody off.
	im, err = n.OptionImpact("direct", "on")
	if err != nil || !im.Confirm || len(im.Unreachable) != 0 {
		t.Fatalf("direct on: %+v, %v", im, err)
	}
}

func option(n *Node, key string) config.Option {
	for _, o := range n.cfg.Options() {
		if o.Key == key {
			return o
		}
	}
	return config.Option{}
}

// pathNode is impactNode plus what peerPath reads: a direct transport, so
// that direct mode counts as running, and peer "b" with a WireGuard address.
func pathNode(t *testing.T) (*Node, config.Peer) {
	t.Helper()
	n := impactNode(t)
	n.reg = transport.NewRegistry()
	n.reg.Add(fakeTransport{"direct"})
	n.reg.Add(fakeTransport{"tor"})
	n.lastGood = map[string]string{}
	n.directFail = map[string]directFailure{}
	p, _ := n.peers.Get("b")
	return n, p
}

func TestPeerPathDirectPreferred(t *testing.T) {
	n, p := pathNode(t)
	v := n.peerPath(p)
	if v.Reason != "direct_next" || v.RetryIn != 0 || v.Down {
		t.Fatalf("%+v", v)
	}
}

func TestPeerPathBackoffUsesNextProbe(t *testing.T) {
	n, p := pathNode(t)
	a := p.Addrs["wg"]
	n.directFail[failKey(p.ID, a)] = directFailure{addr: a, at: time.Now(), wait: time.Hour,
		failures: 1, nextProbe: time.Now().Add(90 * time.Second)}
	v := n.peerPath(p)
	if v.Reason != "direct_backoff" || v.RetryIn < 85 || v.RetryIn > 90 {
		t.Fatalf("%+v", v)
	}
}

func TestPeerPathProbationUsesPreferredAt(t *testing.T) {
	n, p := pathNode(t)
	a := p.Addrs["wg"]
	// Stable for 1 minute of the default 2: preferred again in about one.
	n.directFail[failKey(p.ID, a)] = directFailure{addr: a, at: time.Now().Add(-5 * time.Minute), wait: time.Hour,
		failures: 1, oks: 2, okSince: time.Now().Add(-time.Minute), nextProbe: time.Now().Add(time.Hour)}
	v := n.peerPath(p)
	if v.Reason != "direct_probation" || v.RetryIn < 55 || v.RetryIn > 60 {
		t.Fatalf("%+v", v)
	}
}

func TestPeerPathLinkDownShowsRedial(t *testing.T) {
	n, p := pathNode(t)
	n.links = map[string]*peerLink{p.ID: {down: true, downSince: time.Now(), redialAt: time.Now().Add(30 * time.Second)}}
	v := n.peerPath(p)
	if !v.Down || v.RedialIn < 25 || v.RedialIn > 30 {
		t.Fatalf("%+v", v)
	}
	n.links[p.ID].down = false
	if v := n.peerPath(p); v.Down || v.RedialIn != 0 {
		t.Fatalf("up link reported down: %+v", v)
	}
}

func TestOptionImpactReportsRefusedChange(t *testing.T) {
	n := impactNode(t) // Tor is the only transport on
	im, err := n.OptionImpact("tor", "off")
	if err != nil {
		t.Fatal(err)
	}
	if im.Problem == "" || len(im.Effects) != 0 || im.Confirm {
		t.Fatalf("turning off the last transport: %+v", im)
	}
	if err := n.cfg.CheckOption("tor", "off"); err == nil || err.Error() != im.Problem {
		t.Fatalf("problem %q doesn't match CheckOption: %v", im.Problem, err)
	}
	if o := option(n, "tor"); o.Value != "on" {
		t.Fatal("impact changed the setting")
	}
}

func TestPeerStateTruthfulWithTunnel(t *testing.T) {
	n, p := pathNode(t)
	n.tunnels = map[int]*tunnel{}
	state := func() string { return n.peerState(p.ID, false, n.peerPath(p)) }
	if s := state(); s != "unknown" {
		t.Fatalf("nothing known: %s", s)
	}
	tn := &tunnel{cfg: config.Tunnel{Port: 9000, Peer: p.ID, Service: "web"}}
	n.tunnels[9000] = tn
	tn.record("tor", nil) // the tunnel to the peer works, but no stream is open
	if s := state(); s != "idle" {
		t.Fatalf("ready tunnel must be idle, got %s", s)
	}
	n.links = map[string]*peerLink{p.ID: {down: true, downSince: time.Now(), redialAt: time.Now().Add(time.Minute)}}
	if s := state(); s != "unreachable" {
		t.Fatalf("down link: %s", s)
	}
	if s := n.peerState(p.ID, true, api.PathView{}); s != "connected" {
		t.Fatalf("open connection: %s", s)
	}
}
