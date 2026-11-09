package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/protocol"
	"github.com/apv022/sol/internal/transport"
)

func nodeID(t *testing.T) string {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	return identity.NodeID(pub)
}

func TestSetDirectOnlyAcceptsThePeersOwnAddress(t *testing.T) {
	own, other := nodeID(t), nodeID(t)
	p := config.Peer{NodeID: own, Addrs: map[string]string{"tor": "x.onion:80"}}
	setDirect(&p, "sol-direct:"+own+"@10.0.0.2:7701")
	if p.Addrs["direct"] == "" {
		t.Fatal("own address dropped")
	}
	// Another node's key: dropped (and the old one with it, it's stale).
	setDirect(&p, "sol-direct:"+other+"@10.0.0.9:22")
	if p.Addrs["direct"] != "" {
		t.Fatalf("accepted another node's address: %q", p.Addrs["direct"])
	}
	setDirect(&p, "sol-direct:"+own+"@10.0.0.2:7701")
	setDirect(&p, "garbage")
	if p.Addrs["direct"] != "" || p.Addrs["tor"] == "" {
		t.Fatalf("malformed address handling: %v", p.Addrs)
	}
}

func TestDirectBackoffDoubles(t *testing.T) {
	n := &Node{directFail: map[string]directFailure{}}
	n.noteDirectFailure("", "a") // pairing dials aren't tracked
	if len(n.directFail) != 0 {
		t.Fatal("tracked a dial without a peer id")
	}
	waits := []time.Duration{}
	for range 8 {
		n.noteDirectFailure("p", "a")
		waits = append(waits, n.directFail["p"].wait)
	}
	if waits[0] != directBackoff || waits[1] != 2*directBackoff || waits[7] != directBackoffMax {
		t.Fatalf("backoff: %v", waits)
	}
	n.noteDirectFailure("p", "b") // a new address starts over
	if n.directFail["p"].wait != directBackoff {
		t.Fatal("new address kept the old backoff")
	}
}

// fakeTransport handles "<name>:" addresses; Dial blocks until ctx ends.
type fakeTransport struct{ name string }

func (f fakeTransport) Name() string                { return f.name }
func (f fakeTransport) Start(context.Context) error { return nil }
func (f fakeTransport) Unpublish(string) error      { return nil }
func (f fakeTransport) Stop() error                 { return nil }
func (f fakeTransport) Status() transport.Status {
	return transport.Status{Name: f.name, State: transport.StateReady}
}
func (f fakeTransport) Handles(a string) bool { return strings.HasPrefix(a, f.name+":") }
func (f fakeTransport) Publish(context.Context, transport.Publication) (string, error) {
	return "", transport.ErrNotImplemented
}
func (f fakeTransport) Dial(ctx context.Context, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func testNode() *Node {
	n := &Node{reg: transport.NewRegistry(), lastGood: map[string]string{}, directFail: map[string]directFailure{},
		lastDirectRefresh: map[string]time.Time{}}
	n.reg.Add(fakeTransport{"direct"})
	n.reg.Add(fakeTransport{"tor"})
	return n
}

func TestStaleDirectLastGoodIsSkipped(t *testing.T) {
	n := testNode()
	p := config.Peer{ID: "p", Addrs: map[string]string{"direct": "direct:new", "tor": "tor:x"}}
	n.lastGood["p"] = "direct:old" // worked before the peer moved
	got := n.addrsInOrder(p)
	if strings.Join(got, ",") != "direct:new,tor:x" {
		t.Fatalf("order %v", got)
	}
}

func TestCancelledDialStartsNoBackoff(t *testing.T) {
	n := testNode()
	p := config.Peer{ID: "p", Addrs: map[string]string{"direct": "direct:a"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.dialOnce(ctx, p, protocol.Request{Op: protocol.OpConnect})
	if len(n.directFail) != 0 {
		t.Fatal("our own cancel was blamed on the direct path")
	}
	// A deadline that runs out during the attempt does count (a black hole).
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer dcancel()
	n.dialOnce(dctx, p, protocol.Request{Op: protocol.OpConnect})
	if n.directFail["p"].addr != "direct:a" {
		t.Fatal("a timed-out direct attempt wasn't recorded")
	}
}
