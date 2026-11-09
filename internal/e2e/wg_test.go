package e2e

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/apv022/sol/internal/node"
	"github.com/apv022/sol/internal/transport"
)

// wgNode starts a node with WireGuard direct mode (loopback) plus clearnet
// standing in for Tor as the fallback.
func wgNode(t *testing.T, name string, advertise ...string) *testNode {
	t.Helper()
	dir := t.TempDir()
	return startNode(t, dir, name, func(o *node.Options) {
		o.Transports = []transport.Transport{
			transport.NewWG("127.0.0.1:0", filepath.Join(dir, "wg.key"), advertise, nil),
			transport.NewClearnet("127.0.0.1:0", ""),
		}
	})
}

// Pairing exchanges WireGuard addresses both ways (the link carries the
// inviter's, the card the joiner's), and both sides connect over WireGuard.
func TestWireGuardAfterPairing(t *testing.T) {
	a, b := wgNode(t, "alpha"), wgNode(t, "beta")
	webApp(t, a, "web", "alpha says")
	bOnA, aOnB := pair(t, a, b)
	a.SetShare(bOnA.ID, "web", true)
	if _, err := b.RefreshPeer(b.ctx, aOnB.ID); err != nil {
		t.Fatal(err)
	}
	if via := viaOf(t, b, aOnB.ID, "web"); via != "wg" {
		t.Fatalf("b -> a went via %q, want wg", via)
	}
	if via := viaOf(t, a, bOnA.ID, "_inbox"); via != "wg" {
		t.Fatalf("a -> b went via %q, want wg", via)
	}
	tun, err := b.OpenTunnel(aOnB.ID, "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if body, err := get(tun.URL + "song.flac"); err != nil || body != "alpha says /song.flac" {
		t.Fatalf("GET through a WireGuard tunnel: %q %v", body, err)
	}
	waitFor(t, "the tunnel to report wg", func() bool {
		v := b.Tunnels()
		return len(v) == 1 && v[0].Via == "wg"
	})
}

// No WireGuard path (both sides advertise dead endpoints, so not even a
// punch gets through): Tor (here clearnet) carries the connection. With
// only one side unreachable, the punch from the other side heals it.
func TestWireGuardFallsBack(t *testing.T) {
	a := wgNode(t, "alpha", "127.0.0.1:1")
	b := wgNode(t, "beta", "127.0.0.1:2")
	webApp(t, a, "web", "alpha says")
	bOnA, aOnB := pair(t, a, b)
	a.SetShare(bOnA.ID, "web", true)
	b.RefreshPeer(b.ctx, aOnB.ID)
	if via := viaOf(t, b, aOnB.ID, "web"); via != "clearnet" {
		t.Fatalf("dead WireGuard path: went via %q, want the fallback", via)
	}
}

// Unpairing removes the peer from WireGuard on both sides.
func TestWireGuardUnpairCutsTheTunnel(t *testing.T) {
	a, b := wgNode(t, "alpha"), wgNode(t, "beta")
	bOnA, aOnB := pair(t, a, b)
	if via := viaOf(t, b, aOnB.ID, "_inbox"); via != "wg" {
		t.Fatalf("before: %q", via)
	}
	if _, _, err := a.Unpair(a.ctx, bOnA.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b to drop a", func() bool { return len(b.Peers()) == 0 })
	if _, err := b.DialPeer(b.ctx, aOnB.ID, "_inbox"); err == nil {
		t.Fatal("still reachable after unpairing")
	}
}

// A peer restarts (new UDP port, as after a reboot or network change): the
// tunnel heals by itself, through the fallback path's address refresh.
func TestWireGuardHealsAfterPeerRestart(t *testing.T) {
	node.SetRefreshDirectEvery(t, 0)
	clearnetB := fmt.Sprintf("127.0.0.1:%d", freePort(t)) // so a can still find b by the fallback
	a := wgNode(t, "alpha")
	bDir := t.TempDir()
	startB := func() *testNode {
		return startNode(t, bDir, "beta", func(o *node.Options) {
			o.Transports = []transport.Transport{
				transport.NewWG("127.0.0.1:0", filepath.Join(bDir, "wg.key"), nil, nil),
				transport.NewClearnet(clearnetB, ""),
			}
		})
	}
	b := startB()
	bOnA, _ := pair(t, a, b)
	if via := viaOf(t, a, bOnA.ID, "_inbox"); via != "wg" {
		t.Fatalf("before: %q", via)
	}
	b.stop()
	startB() // same data dir, new UDP port
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if viaOf(t, a, bOnA.ID, "_inbox") == "wg" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("a never reached the restarted b over WireGuard")
}
