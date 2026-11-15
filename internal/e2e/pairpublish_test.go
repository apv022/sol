package e2e

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/apv022/sol/internal/node"
	"github.com/apv022/sol/internal/transport"
)

// keyedClearnet is clearnet that also issues access keys, like Tor, and
// logs what the node publishes and dials, in order.
type keyedClearnet struct {
	*transport.Clearnet
	mu  sync.Mutex
	n   int
	log []string // "publish <keys>" (node publication) and "dial"
}

func (k *keyedClearnet) NewAccess() (transport.Access, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.n++
	return transport.Access{Authorize: fmt.Sprintf("pub%d", k.n), Secret: fmt.Sprintf("sec%d", k.n)}, nil
}

func (k *keyedClearnet) Publish(ctx context.Context, p transport.Publication) (string, error) {
	if p.ID == "node" {
		k.mu.Lock()
		k.log = append(k.log, "publish "+strings.Join(p.ClientAuth, ","))
		k.mu.Unlock()
	}
	return k.Clearnet.Publish(ctx, p)
}

func (k *keyedClearnet) Dial(ctx context.Context, addr string) (net.Conn, error) {
	k.mu.Lock()
	k.log = append(k.log, "dial")
	k.mu.Unlock()
	return k.Clearnet.Dial(ctx, addr)
}

func (k *keyedClearnet) events() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.log)
}

// Changing an onion's client keys re-publishes it, and a fresh descriptor
// takes a while to reach every directory. The joiner therefore publishes
// the key it issues to the inviter before its pairing dial, not after
// pairing (when the inviter may dial it at once), and pairing then changes
// nothing more.
func TestJoinerPublishesIssuedKeyBeforePairing(t *testing.T) {
	kb := &keyedClearnet{Clearnet: transport.NewClearnet("127.0.0.1:0", "")}
	a := newNode(t, "alpha")
	b := newNode(t, "beta", func(o *node.Options) { o.Transports = []transport.Transport{kb} })
	inv, err := a.Invite()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Pair(b.ctx, inv.Link); err != nil {
		t.Fatalf("pair: %v", err)
	}
	ev := kb.events()
	dial := slices.Index(ev, "dial")
	keyed := slices.Index(ev, "publish pub1")
	if dial < 0 || keyed < 0 || keyed > dial {
		t.Fatalf("issued key published after the pairing dial (or never): %q", ev)
	}
	for _, e := range ev[keyed+1:] {
		if strings.HasPrefix(e, "publish") {
			t.Fatalf("pairing re-published the onion again: %q", ev)
		}
	}
}

// If pairing fails, the key issued for it is withdrawn again.
func TestFailedPairingWithdrawsIssuedKey(t *testing.T) {
	kb := &keyedClearnet{Clearnet: transport.NewClearnet("127.0.0.1:0", "")}
	a := newNode(t, "alpha")
	b := newNode(t, "beta", func(o *node.Options) { o.Transports = []transport.Transport{kb} })
	inv, err := a.Invite()
	if err != nil {
		t.Fatal(err)
	}
	a.stop() // the inviter is gone: the pairing dial fails
	if _, err := b.Pair(b.ctx, inv.Link); err == nil {
		t.Fatal("paired with a stopped node")
	}
	ev := kb.events()
	if len(ev) == 0 || ev[len(ev)-1] != "publish " {
		t.Fatalf("issued key not withdrawn after a failed pairing: %q", ev)
	}
}
