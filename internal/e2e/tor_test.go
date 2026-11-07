package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/connect"
	"github.com/apv022/sol/internal/node"
	"github.com/apv022/sol/internal/runtime"
	"github.com/apv022/sol/internal/transport"
)

// TestOverRealTor runs the full flow over the real Tor network: onion
// publication, client authorization, pairing, and a tunnelled request. It
// needs `tor` on PATH and network access, and takes a few minutes, so it
// only runs when SOL_TEST_TOR=1:
//
//	SOL_TEST_TOR=1 go test ./internal/e2e -run Tor -v -timeout 15m
func TestOverRealTor(t *testing.T) {
	if os.Getenv("SOL_TEST_TOR") != "1" {
		t.Skip("set SOL_TEST_TOR=1 to run against the real Tor network")
	}
	prov := &runtime.Native{}
	if !prov.Available("tor") {
		t.Fatal("tor is not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello over tor")
	}))
	defer app.Close()

	nodeDir := t.TempDir()
	nodeTor := transport.NewTor(filepath.Join(nodeDir, "tor"), prov)
	n, err := node.New(node.Options{DataDir: nodeDir, PanelAddr: "off", NodeAddr: "127.0.0.1:0", Transports: []transport.Transport{nodeTor}})
	if err != nil {
		t.Fatal(err)
	}
	go n.Run(ctx)
	<-n.Ready()
	port := app.Listener.Addr().(*net.TCPAddr).Port
	n.AddService(config.Service{Name: "web", LocalPort: port, Visibility: config.VisibilityPrivate})
	if err := nodeTor.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := n.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pairing link: %s", p.Link)

	torOnly := func(dir string, rec connect.NodeRecord) []transport.Transport {
		return []transport.Transport{transport.NewTor(filepath.Join(dir, "tor"), prov)}
	}
	// Onion descriptors take a while to propagate; retry pairing.
	var c *connect.Client
	for attempt := 1; ; attempt++ {
		c, err = connect.Pair(ctx, t.TempDir(), p.Link, "tor-test", connect.Options{Transports: torOnly, Progress: func(m string) { t.Log(m) }})
		if err == nil {
			break
		}
		if attempt == 5 || ctx.Err() != nil {
			t.Fatalf("pairing over tor: %v", err)
		}
		t.Logf("pair attempt %d: %v (retrying)", attempt, err)
		time.Sleep(20 * time.Second)
	}
	defer c.Close()

	base := tunnel(t, ctx, c, "web")
	body, err := get(base + "/")
	if err != nil || body != "hello over tor" {
		t.Fatalf("GET over tor = %q, %v", body, err)
	}
}
