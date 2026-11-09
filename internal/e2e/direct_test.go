package e2e

import (
	"path/filepath"
	"testing"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/node"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/transport"
)

// directNode starts a node with direct mode (on loopback) plus the clearnet
// transport standing in for Tor as the fallback. advertise overrides the
// direct candidates (a dead address simulates a peer we can't reach directly).
func directNode(t *testing.T, name string, advertise ...string) (*testNode, *transport.Direct) {
	t.Helper()
	dir := t.TempDir()
	id, err := identity.LoadOrCreate(filepath.Join(dir, "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	d := transport.NewDirect("127.0.0.1:0", id.Signer(), advertise...)
	n := startNode(t, dir, name, func(o *node.Options) {
		o.Transports = []transport.Transport{d, transport.NewClearnet("127.0.0.1:0", "")}
	})
	return n, d
}

func viaOf(t *testing.T, n *testNode, peerID, service string) string {
	t.Helper()
	c, err := n.DialPeer(n.ctx, peerID, service)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, p := range n.Peers() {
		for _, cv := range p.Connections {
			if p.ID == peerID && cv.Outbound && cv.Service == service {
				return cv.Via
			}
		}
	}
	t.Fatal("connection not listed")
	return ""
}

func TestDirectModeIsPreferred(t *testing.T) {
	a, _ := directNode(t, "alpha")
	b, _ := directNode(t, "beta")
	webApp(t, a, "web", "alpha says")
	bOnA, aOnB := pair(t, a, b)
	a.SetShare(bOnA.ID, "web", true)
	if _, err := b.RefreshPeer(b.ctx, aOnB.ID); err != nil {
		t.Fatal(err)
	}
	if via := viaOf(t, b, aOnB.ID, "web"); via != "direct" {
		t.Fatalf("b -> a went via %q, want direct", via)
	}
	// Both ways: a reaches b's inbox directly too.
	if via := viaOf(t, a, bOnA.ID, "_inbox"); via != "direct" {
		t.Fatalf("a -> b went via %q, want direct", via)
	}
	// And the data is right (TLS + handshake + service pipe).
	tun, err := b.OpenTunnel(aOnB.ID, "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if body, err := get(tun.URL + "x"); err != nil || body != "alpha says /x" {
		t.Fatalf("GET via direct tunnel: %q %v", body, err)
	}
}

// A peer that moved networks: its old direct address is dead, so b falls
// back (here to clearnet, in real life Tor), asks it for its current
// address over that working path, and goes direct again.
func TestDirectModeFallsBackAndHeals(t *testing.T) {
	a, ad := directNode(t, "alpha", "127.0.0.1:1") // nothing listens on port 1
	b, _ := directNode(t, "beta")
	webApp(t, a, "web", "alpha says")
	bOnA, aOnB := pair(t, a, b)
	a.SetShare(bOnA.ID, "web", true)
	if _, err := b.RefreshPeer(b.ctx, aOnB.ID); err != nil {
		t.Fatal(err)
	}
	if via := viaOf(t, b, aOnB.ID, "web"); via != "clearnet" {
		t.Fatalf("dead direct path: went via %q, want the fallback", via)
	}

	// a's real address becomes reachable (as after a network change).
	ad.SetAdvertise(nil)
	if _, err := b.RefreshPeer(b.ctx, aOnB.ID); err != nil {
		t.Fatal(err)
	}
	if via := viaOf(t, b, aOnB.ID, "web"); via != "direct" {
		t.Fatalf("after a refresh: went via %q, want direct", via)
	}
}

// The heal also happens on its own: a failed direct attempt followed by a
// working fallback triggers a refresh in the background.
func TestDirectModeHealsByItself(t *testing.T) {
	a, ad := directNode(t, "alpha", "127.0.0.1:1")
	b, _ := directNode(t, "beta")
	webApp(t, a, "web", "alpha says")
	bOnA, aOnB := pair(t, a, b)
	a.SetShare(bOnA.ID, "web", true)
	b.RefreshPeer(b.ctx, aOnB.ID)
	ad.SetAdvertise(nil) // a moved; b still has the dead address
	if via := viaOf(t, b, aOnB.ID, "web"); via != "clearnet" {
		t.Fatalf("first dial: %q", via)
	}
	waitFor(t, "b to go direct after the background refresh", func() bool {
		c, err := b.DialPeer(b.ctx, aOnB.ID, "web")
		if err != nil {
			return false
		}
		defer c.Close()
		for _, p := range b.Peers() {
			for _, cv := range p.Connections {
				if cv.Outbound && cv.Via == "direct" {
					return true
				}
			}
		}
		return false
	})
}

// Direct mode carries only the node: a public service is published on the
// other transports and never gets a direct address.
func TestPublicServiceNeverDirect(t *testing.T) {
	a, _ := directNode(t, "alpha")
	webApp(t, a, "blog", "hi")
	if err := a.SetServiceVisibility("blog", config.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	for _, s := range a.Services() {
		if s.Name != "blog" {
			continue
		}
		if _, ok := s.Addresses["direct"]; ok {
			t.Fatalf("public service published on direct: %v", s.Addresses)
		}
		if _, ok := s.Addresses["clearnet"]; !ok {
			t.Fatalf("public service missing from the other transports: %v", s.Addresses)
		}
		return
	}
	t.Fatal("blog not listed")
}

// A pairing link may only carry a direct address for the inviter's own key.
func TestLinkWithForeignDirectAddressIsCleaned(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	inv, err := a.Invite()
	if err != nil {
		t.Fatal(err)
	}
	l, err := pairing.ParseLink(inv.Link)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := identity.Generate()
	l.Addrs["direct"] = "sol-direct:" + other.NodeID() + "@10.0.0.9:22"
	aOnB, err := b.Pair(b.ctx, l.String())
	if err != nil {
		t.Fatal(err)
	}
	if got := aOnB.Addrs["direct"]; got != "" {
		t.Fatalf("stored a direct address for another node: %q", got)
	}
}
