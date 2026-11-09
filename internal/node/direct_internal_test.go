package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/identity"
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
