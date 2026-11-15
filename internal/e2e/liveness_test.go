package e2e

import (
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apv022/sol/internal/faultnet"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/node"
	"github.com/apv022/sol/internal/transport"
)

// fastLiveness is the liveness supervisor on a test clock: a dead path is
// noticed in well under a second, and a healed direct path is preferred
// again after DirectStableFor.
func fastLiveness(wall func() time.Time) node.Liveness {
	return node.Liveness{
		ProbeEvery:         100 * time.Millisecond,
		ProbeTimeout:       300 * time.Millisecond,
		SlowProbeTimeout:   300 * time.Millisecond,
		Misses:             2,
		RedialMin:          50 * time.Millisecond,
		RedialMax:          200 * time.Millisecond,
		DirectBackoff:      100 * time.Millisecond,
		DirectBackoffMax:   200 * time.Millisecond,
		DirectStableFor:    600 * time.Millisecond,
		DirectStableProbes: 3,
		NetChangeDebounce:  20 * time.Millisecond,
		SleepCheckEvery:    20 * time.Millisecond,
		SleepJump:          time.Minute,
		Tick:               20 * time.Millisecond,
		Wall:               wall,
	}
}

// eventually polls cond for up to d.
func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

// closedWithin reports whether the node closed c (a read ends with an
// error that isn't our own deadline) within d.
func closedWithin(c net.Conn, d time.Duration) error {
	c.SetReadDeadline(time.Now().Add(d))
	_, err := io.Copy(io.Discard, c)
	if isTimeout(err) {
		return fmt.Errorf("still open after %s", d)
	}
	return nil
}

func linkOf(t *testing.T, n *testNode, id string) node.PeerLink {
	t.Helper()
	l, ok := n.PeerLink(id)
	if !ok {
		t.Fatalf("PeerLink(%s): no such peer", id)
	}
	return l
}

// A direct path that blackholes mid-stream (no FIN, no RST: what a phone
// leaving the Wi-Fi looks like) is noticed by the probes: the stream is
// closed and new connections fall back (clearnet stands in for Tor). When
// the path heals, it isn't used again at once; only after it has been
// stable for DirectStableFor does direct carry new connections again.
func TestBlackholedDirectPathFallsBackAndReturns(t *testing.T) {
	proxy, err := faultnet.NewTCPProxy("127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	mk := func(name string, advertise ...string) (*testNode, *transport.Direct) {
		dir := t.TempDir()
		id, err := identity.LoadOrCreate(filepath.Join(dir, "node.key"))
		if err != nil {
			t.Fatal(err)
		}
		d := transport.NewDirect("127.0.0.1:0", id.Signer(), advertise...)
		return startNode(t, dir, name, func(o *node.Options) {
			o.Transports = []transport.Transport{d, transport.NewClearnet("127.0.0.1:0", "")}
			o.Liveness = fastLiveness(nil)
		}), d
	}
	a, ad := mk("alpha", proxy.Addr())
	b, _ := mk("beta")
	// The proxy forwards to a's real direct port; a advertises only the proxy.
	_, hosts, err := transport.ParseDirectAddr(ad.Status().Addresses["node"])
	if err != nil || len(hosts) != 1 || hosts[0] != proxy.Addr() {
		t.Fatalf("a's direct address: %v %v", hosts, err)
	}
	ad.SetAdvertise(nil)
	_, real, _ := transport.ParseDirectAddr(ad.Status().Addresses["node"])
	proxy.SetUpstream(real[0])
	ad.SetAdvertise([]string{proxy.Addr()})

	webApp(t, a, "web", "alpha says")
	bOnA, aOnB := pair(t, a, b)
	a.SetShare(bOnA.ID, "web", true)
	if _, err := b.RefreshPeer(b.ctx, aOnB.ID); err != nil {
		t.Fatal(err)
	}
	c, err := b.DialPeer(b.ctx, aOnB.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if l := linkOf(t, b, aOnB.ID); l.Via != "direct" || !l.Live {
		t.Fatalf("before: %+v", l)
	}

	proxy.Blackhole(true)
	if err := closedWithin(c, 5*time.Second); err != nil {
		t.Fatalf("blackholed stream: %v", err)
	}
	l := linkOf(t, b, aOnB.ID)
	if l.Fallback == "" || l.Direct["direct"].State != "backoff" {
		t.Fatalf("after the blackhole: %+v", l)
	}
	if via := viaOf(t, b, aOnB.ID, "web"); via != "clearnet" {
		t.Fatalf("while blackholed: went via %q, want the fallback", via)
	}

	healed := time.Now()
	proxy.Blackhole(false)
	// Hysteresis: one working probe isn't enough, so right after the heal
	// connections stay on the fallback.
	if via := viaOf(t, b, aOnB.ID, "web"); via != "clearnet" {
		t.Fatalf("just healed: went via %q; it should prove itself first", via)
	}
	eventually(t, 10*time.Second, "b to go direct again", func() bool {
		return viaOf(t, b, aOnB.ID, "web") == "direct"
	})
	if since := time.Since(healed); since < 600*time.Millisecond {
		t.Fatalf("direct preferred again after %s, before the hysteresis", since)
	}
	// And it stays direct: no flapping back.
	for range 5 {
		if via := viaOf(t, b, aOnB.ID, "web"); via != "direct" {
			t.Fatalf("after going back: %q", via)
		}
	}
	if l := linkOf(t, b, aOnB.ID); l.Fallback != "" || len(l.Direct) != 0 {
		t.Fatalf("after going back the link still reports a problem: %+v", l)
	}
}

// clearnetBehind starts a node whose only transport (clearnet) is reached
// through a fault-injecting proxy, so tests can kill and heal its path.
func clearnetBehind(t *testing.T, name string, lv node.Liveness) (*testNode, *faultnet.TCPProxy) {
	t.Helper()
	port := freePort(t)
	proxy, err := faultnet.NewTCPProxy(fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	n := newNode(t, name, func(o *node.Options) {
		o.Transports = []transport.Transport{transport.NewClearnet(fmt.Sprintf("127.0.0.1:%d", port), proxy.Addr())}
		o.Liveness = lv
	})
	return n, proxy
}

// downPeer pairs b with a (reached through proxy), opens a stream, then
// kills the path: the stream is cut and a is left "down" on b, with a
// redial backoff too long to retry by itself during the test.
func downPeer(t *testing.T, lv node.Liveness) (b *testNode, proxy *faultnet.TCPProxy, aID string) {
	t.Helper()
	lv.RedialMin, lv.RedialMax = time.Hour, time.Hour
	a, proxy := clearnetBehind(t, "alpha", lv)
	b = newNode(t, "beta", func(o *node.Options) { o.Liveness = lv })
	_, aOnB := pair(t, a, b)
	c, err := b.DialPeer(b.ctx, aOnB.ID, "_inbox")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	proxy.Blackhole(true)
	if err := closedWithin(c, 5*time.Second); err != nil {
		t.Fatalf("blackholed stream: %v", err)
	}
	eventually(t, 5*time.Second, "a to be down on b", func() bool { return linkOf(t, b, aOnB.ID).Down })
	proxy.Blackhole(false)
	// Nothing retries on its own for an hour.
	time.Sleep(300 * time.Millisecond)
	if !linkOf(t, b, aOnB.ID).Down {
		t.Fatal("redialled before the backoff ran out")
	}
	return b, proxy, aOnB.ID
}

// A network change (a mobile OS announcing it) redials a down peer at
// once instead of waiting out the backoff.
func TestNetworkChangedRedialsAtOnce(t *testing.T) {
	b, _, aID := downPeer(t, fastLiveness(nil))
	b.NetworkChanged("test: wifi to cellular")
	eventually(t, 3*time.Second, "the redial after NetworkChanged", func() bool { return !linkOf(t, b, aID).Down })
	if _, err := b.DialPeer(b.ctx, aID, "_inbox"); err != nil {
		t.Fatalf("after the redial: %v", err)
	}
}

// Waking from sleep shows as the wall clock running ahead of the monotonic
// one; the node treats it as a network change and recovers at once.
func TestWallClockJumpTriggersRecovery(t *testing.T) {
	var offset atomic.Int64
	wall := func() time.Time { return time.Now().Round(0).Add(time.Duration(offset.Load())) }
	b, _, aID := downPeer(t, fastLiveness(wall))
	offset.Add(int64(time.Hour)) // "slept" for an hour
	eventually(t, 3*time.Second, "recovery after the clock jump", func() bool { return !linkOf(t, b, aID).Down })
	found := false
	for _, e := range b.Events().Recent(500) {
		if strings.Contains(e.Message, "clock jumped") {
			found = true
		}
	}
	if !found {
		t.Fatal("the clock jump wasn't logged")
	}
}

// A WireGuard tunnel whose UDP path goes silent is declared dead by the
// transport itself (no keepalive heard for wgSilent): its streams are cut
// and new connections fall back.
func TestWireGuardSilentTunnelIsCut(t *testing.T) {
	const silent = 3 * time.Second
	transport.SetWGLiveness(t, 1, silent)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wgPort := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	udp, err := faultnet.NewUDPProxy(fmt.Sprintf("127.0.0.1:%d", wgPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(udp.Close)
	dir := t.TempDir()
	a := startNode(t, dir, "alpha", func(o *node.Options) {
		o.Transports = []transport.Transport{
			transport.NewWG(fmt.Sprintf("127.0.0.1:%d", wgPort), filepath.Join(dir, "wg.key"), []string{udp.Addr()}, nil),
			transport.NewClearnet("127.0.0.1:0", ""),
		}
	})
	b := wgNode(t, "beta", "127.0.0.1:2") // a can only answer b, never reach it first
	_, aOnB := pair(t, a, b)
	if via := viaOf(t, b, aOnB.ID, "_inbox"); via != "wg" {
		t.Fatalf("before: %q", via)
	}
	c, err := b.DialPeer(b.ctx, aOnB.ID, "_inbox")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	udp.Drop(true)
	// Cut once nothing is heard for silent, noticed on the transport's
	// next 500 ms tick; the rest is margin for a slow, raced runner. The
	// detection runs on the wall clock, so slowness adds ticks, not
	// multiples of silent.
	if err := closedWithin(c, 2*silent+2*time.Second); err != nil {
		t.Fatalf("silent tunnel: %v", err)
	}
	c.Close() // what the app does when its stream ends
	if via := viaOf(t, b, aOnB.ID, "_inbox"); via != "clearnet" {
		t.Fatalf("after the tunnel died: %q", via)
	}
}
