package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/node"
	"github.com/apv022/sol/internal/runtime"
	"github.com/apv022/sol/internal/transport"
)

// TestOverRealTor runs two nodes over the real Tor network: each publishes
// its own onion with client authorization, they pair, and then each reaches
// the other's onion (a tunnelled request one way, a file the other way). It
// needs `tor` on PATH and network access, and takes a few minutes, so it
// only runs when SOL_TEST_TOR=1:
//
//	SOL_TEST_TOR=1 go test ./internal/e2e -run Tor -v -timeout 20m
func TestOverRealTor(t *testing.T) {
	if os.Getenv("SOL_TEST_TOR") != "1" {
		t.Skip("set SOL_TEST_TOR=1 to run against the real Tor network")
	}
	prov := &runtime.Native{}
	if !prov.Available("tor") {
		t.Fatal("tor is not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello over tor")
	}))
	defer app.Close()

	start := func(name string) (*node.Node, *transport.Tor) {
		dir := t.TempDir()
		tor := transport.NewTor(filepath.Join(dir, "tor"), prov)
		n, err := node.New(node.Options{DataDir: dir, PanelAddr: "off", NodeAddr: "127.0.0.1:0",
			ControlPath: "off", Transports: []transport.Transport{tor}})
		if err != nil {
			t.Fatal(err)
		}
		n.SetName(name)
		// Stop the node (and its tor) before the temp dirs are removed:
		// cleanups run last-registered first.
		nctx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- n.Run(nctx) }()
		t.Cleanup(func() { stop(); <-done })
		<-n.Ready()
		return n, tor
	}
	a, aTor := start("alpha")
	b, bTor := start("beta")
	port := app.Listener.Addr().(*net.TCPAddr).Port
	a.AddService(config.Service{Name: "web", LocalPort: port, Visibility: config.VisibilityPrivate})
	for _, tor := range []*transport.Tor{aTor, bTor} {
		if err := tor.WaitReady(ctx); err != nil {
			t.Fatal(err)
		}
	}

	inv, err := a.Invite()
	if err != nil {
		t.Fatal(err)
	}
	// Onion descriptors take a while to propagate; Pair retries internally,
	// and we retry with a fresh link if that is not enough.
	var aOnB config.Peer
	for attempt := 1; ; attempt++ {
		if aOnB, err = b.Pair(ctx, inv.Link); err == nil {
			break
		}
		if attempt == 3 || ctx.Err() != nil {
			t.Fatalf("pairing over tor: %v", err)
		}
		t.Logf("pair attempt %d: %v (retrying with a new link)", attempt, err)
		if inv, err = a.Invite(); err != nil {
			t.Fatal(err)
		}
	}
	var bOnA string
	for _, p := range a.Peers() {
		bOnA = p.ID
	}
	if _, err := a.SetShare(bOnA, "web", true); err != nil {
		t.Fatal(err)
	}

	// b -> a's onion: a tunnelled HTTP request to a shared service.
	conn, err := b.DialPeer(ctx, aOnB.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	if body, err := httpOver(conn, "/"); err != nil || body != "hello over tor" {
		t.Fatalf("GET over tor = %q, %v", body, err)
	}

	// a -> b's onion (published with the key b issued to a while pairing).
	if _, _, err := a.SendFile(ctx, bOnA, "over-tor.txt", strings.NewReader("a file over tor")); err != nil {
		t.Fatalf("send a -> b over tor: %v", err)
	}
	list, _ := b.Blobs().List()
	if len(list) != 1 || list[0].Name != "over-tor.txt" || list[0].From != aOnB.ID {
		t.Fatalf("b received %+v", list)
	}
}
