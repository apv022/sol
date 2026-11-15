package e2e

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/faultnet"
	"github.com/apv022/sol/internal/node"
	"github.com/apv022/sol/internal/protocol"
	"github.com/apv022/sol/internal/transport"
)

// countEvents counts b's log messages containing sub.
func countEvents(n *testNode, sub string) int {
	c := 0
	for _, e := range n.Events().Recent(1000) {
		if strings.Contains(e.Message, sub) {
			c++
		}
	}
	return c
}

// A peer that comes back speaking only an older protocol version shows as
// incompatible (saying which side to update), is logged once, and is
// redialled at the slowest liveness pace rather than in a hot loop. Once it
// speaks a common version again, it recovers by itself.
//
// Versions 2 stands in for "the current Sol" here: ranges above
// protocol.MaxVersion run the version 1 layout, so only negotiation differs.
func TestIncompatiblePeerShownAndNotHammered(t *testing.T) {
	v2 := protocol.Versions{Min: 2, Max: 2}
	lv := fastLiveness(nil)
	lv.RedialMin, lv.RedialMax = 20*time.Millisecond, 2*time.Second

	proxy, err := faultnet.NewTCPProxy("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	// alpha is reached only through the proxy, on a fixed port, so it can
	// restart as another "version" at the same address.
	startAlpha := func(dir, addr string, v protocol.Versions) (*testNode, bool) {
		cn := transport.NewClearnet(addr, proxy.Addr())
		start := startNode
		if dir == "" {
			start = func(t *testing.T, _ string, name string, opts ...opt) *testNode { return newNode(t, name, opts...) }
		}
		n := start(t, dir, "alpha", func(o *node.Options) {
			o.Transports = []transport.Transport{cn}
			o.Liveness = lv
			o.Protocol = v
		})
		return n, cn.Status().Addresses["node"] != ""
	}
	var a *testNode
	var addr string
	for range 5 {
		addr = fmt.Sprintf("127.0.0.1:%d", freePort(t))
		var ok bool
		if a, ok = startAlpha("", addr, v2); ok {
			break
		}
		a.stop()
		a = nil
	}
	if a == nil {
		t.Fatal("no free port for alpha")
	}
	proxy.SetUpstream(addr)

	b := newNode(t, "beta", func(o *node.Options) {
		o.Liveness = lv
		o.Protocol = v2
		o.DialAttempts = 5 // an incompatible peer must still get one attempt, not five
	})
	_, aOnB := pair(t, a, b)

	// Open a stream, then cut the path so beta wants to redial alpha.
	c, err := b.DialPeer(b.ctx, aOnB.ID, "_inbox")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	proxy.Blackhole(true)
	if err := closedWithin(c, 5*time.Second); err != nil {
		t.Fatalf("blackholed stream: %v", err)
	}
	eventually(t, 5*time.Second, "alpha to be down on beta", func() bool { return linkOf(t, b, aOnB.ID).Down })

	// alpha "downgrades" to a Sol that speaks only version 1. Refusing
	// (instead of blackholing) while it restarts lets it stop at once.
	if err := proxy.Refuse(true); err != nil {
		t.Fatal(err)
	}
	proxy.ResetAll()
	proxy.Blackhole(false)
	dir := a.dir
	a.stop()
	a, ok := startAlpha(dir, addr, protocol.Versions{Min: 1, Max: 1})
	if !ok {
		t.Fatal("alpha didn't get its port back")
	}
	if err := proxy.Refuse(false); err != nil {
		t.Fatal(err)
	}

	const older = "alpha runs an older Sol"
	eventually(t, 5*time.Second, "alpha to show as incompatible", func() bool {
		for _, p := range b.Peers() {
			if p.ID == aOnB.ID && p.State == "incompatible" && p.Incompatible != nil {
				return p.Incompatible.Update == "peer" && strings.Contains(p.Incompatible.Detail, "Update Sol on alpha")
			}
		}
		return false
	})

	// No hot loop: from now on alpha is redialled at RedialMax (2 s), so a
	// 1.5 s window sees at most one redial (normal backoff from RedialMin
	// would make five or more).
	before := proxy.Total()
	time.Sleep(1500 * time.Millisecond)
	if n := proxy.Total() - before; n > 1 {
		t.Fatalf("%d connections to the incompatible peer in 1.5 s", n)
	}

	// A user's dial fails at once with the reason, after one connection.
	before = proxy.Total()
	_, err = b.DialPeer(b.ctx, aOnB.ID, "_inbox")
	if !errors.Is(err, protocol.ErrIncompatible) || !strings.Contains(err.Error(), "Update Sol on alpha") {
		t.Fatalf("dial: want the incompatibility, got %v", err)
	}
	if n := proxy.Total() - before; n != 1 {
		t.Fatalf("dial made %d connections, want 1 (no retries)", n)
	}
	if n := countEvents(b, older); n != 1 {
		t.Fatalf("beta logged the incompatibility %d times, want once", n)
	}
	if n := countEvents(a, "a node running a newer Sol"); n != 1 {
		t.Fatalf("alpha logged the incompatible caller %d times, want once", n)
	}
	for _, cv := range b.Peers() {
		if cv.ID == aOnB.ID && cv.Online {
			t.Fatal("an incompatible peer shows as online")
		}
	}

	// alpha updates again: the next redial reaches it and the state clears.
	a.stop()
	if _, ok := startAlpha(dir, addr, v2); !ok {
		t.Fatal("alpha didn't get its port back")
	}
	eventually(t, 10*time.Second, "alpha to be compatible again", func() bool {
		for _, p := range b.Peers() {
			if p.ID == aOnB.ID {
				return p.Incompatible == nil && p.State != "incompatible"
			}
		}
		return false
	})
	if countEvents(b, "alpha now runs a compatible Sol") != 1 {
		t.Fatal("the recovery wasn't logged")
	}
}
