// Package e2e runs a real node and a real client in one process over the
// clearnet transport, covering the demo's definition of done (spec.md 10.3)
// without needing Tor. If this passes, the handshake, pairing, routing,
// revocation, and panel guards all work together.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/connect"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/node"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/protocol"
	"github.com/apv022/sol/internal/transport"
)

func clearnetOnly(string, connect.NodeRecord) []transport.Transport {
	return []transport.Transport{transport.NewClearnet("127.0.0.1:0", "")}
}

type harness struct {
	t      *testing.T
	ctx    context.Context
	node   *node.Node
	appURL string
}

func start(t *testing.T) *harness {
	t.Helper()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s", r.URL.Path)
	}))
	t.Cleanup(app.Close)

	n, err := node.New(node.Options{
		DataDir:    t.TempDir(),
		Version:    "test",
		PanelAddr:  "127.0.0.1:0",
		NodeAddr:   "127.0.0.1:0",
		Transports: []transport.Transport{transport.NewClearnet("127.0.0.1:0", "")},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-n.Ready():
	case err := <-done:
		t.Fatalf("node exited: %v", err)
	}

	port := app.Listener.Addr().(*net.TCPAddr).Port
	if err := n.AddService(config.Service{Name: "web", LocalPort: port, Visibility: config.VisibilityPrivate}); err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, ctx: ctx, node: n, appURL: app.URL}
}

func (h *harness) link() string {
	h.t.Helper()
	p, err := h.node.NewPairing()
	if err != nil {
		h.t.Fatal(err)
	}
	return p.Link
}

// tunnel starts a local listener for service and returns its http base URL.
func tunnel(t *testing.T, ctx context.Context, c *connect.Client, service string) string {
	t.Helper()
	addrc := make(chan string, 1)
	go c.Listen(ctx, "127.0.0.1:0", service, func(a string) { addrc <- a })
	return "http://" + <-addrc
}

func get(url string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func TestPairConnectRevoke(t *testing.T) {
	h := start(t)
	link := h.link()

	c, err := connect.Pair(h.ctx, t.TempDir(), link, "laptop", clearnetOnly)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	defer c.Close()
	if len(c.Node.Services) != 2 { // web + _files
		t.Fatalf("services after pairing: %+v", c.Node.Services)
	}

	// 1. A paired device reaches the private service through the tunnel.
	base := tunnel(t, h.ctx, c, "web")
	body, err := get(base + "/song.flac")
	if err != nil || body != "hello from /song.flac" {
		t.Fatalf("tunnel GET = %q, %v", body, err)
	}

	// 2. A pairing link works only once.
	if _, err := connect.Pair(h.ctx, t.TempDir(), link, "thief", clearnetOnly); err == nil {
		t.Fatal("pairing link was accepted twice")
	}

	// 3. A live connection is cut the moment the device is revoked.
	live, err := c.Dial(h.ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	devs := h.node.Devices()
	if len(devs) != 1 {
		t.Fatalf("devices: %+v", devs)
	}
	if err := h.node.RemoveDevice(devs[0].ID); err != nil {
		t.Fatal(err)
	}
	live.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := live.Read(make([]byte, 1)); err == nil || errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		t.Fatalf("live connection survived revocation: %v", err)
	}

	// 4. New connections after revocation get nothing.
	if _, err := get(base + "/"); err == nil {
		t.Fatal("revoked device still reached the service")
	}
	if _, err := c.Dial(h.ctx, "web"); !errors.Is(err, protocol.ErrRejected) {
		t.Fatalf("revoked dial: want ErrRejected, got %v", err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func TestStrangerGetsNothing(t *testing.T) {
	h := start(t)
	l, err := pairing.ParseLink(h.link())
	if err != nil {
		t.Fatal(err)
	}
	stranger, _ := identity.Generate()

	// Raw TCP to the node address: the node speaks first with its hello,
	// but an unpaired key is closed without any reply.
	conn, err := net.Dial("tcp", l.LAN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	err = protocol.ClientHandshake(conn, stranger, l.NodeID, protocol.Request{Op: protocol.OpConnect, Service: "web"})
	if !errors.Is(err, protocol.ErrRejected) {
		t.Fatalf("stranger: want ErrRejected, got %v", err)
	}

	// Plain HTTP to the node address gets no HTTP response at all.
	if body, err := get("http://" + l.LAN + "/"); err == nil {
		t.Fatalf("plain HTTP got a response: %q", body)
	}
}

func TestUnknownServiceAndFiles(t *testing.T) {
	h := start(t)
	c, err := connect.Pair(h.ctx, t.TempDir(), h.link(), "laptop", clearnetOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Dial(h.ctx, "ssh"); !errors.Is(err, protocol.ErrRejected) {
		t.Fatalf("unknown service: want ErrRejected, got %v", err)
	}

	hash, err := h.node.Blobs().Put(strings.NewReader("same bytes everywhere"))
	if err != nil {
		t.Fatal(err)
	}
	base := tunnel(t, h.ctx, c, node.FilesService)
	body, err := get(base + "/blob/" + hash)
	if err != nil || body != "same bytes everywhere" {
		t.Fatalf("files GET = %q, %v", body, err)
	}
}

func TestReloadedClientReconnects(t *testing.T) {
	h := start(t)
	dir := t.TempDir()
	c, err := connect.Pair(h.ctx, dir, h.link(), "laptop", clearnetOnly)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	c2, err := connect.Load(h.ctx, dir, "", clearnetOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	svcs, err := c2.Services(h.ctx)
	if err != nil || len(svcs) != 2 {
		t.Fatalf("Services = %+v, %v", svcs, err)
	}
}

func TestPublicServiceNeedsNoPairing(t *testing.T) {
	h := start(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "nginx/1.2.3")
		io.WriteString(w, "public page")
	}))
	defer app.Close()
	port := app.Listener.Addr().(*net.TCPAddr).Port
	if err := h.node.AddService(config.Service{Name: "blog", LocalPort: port, Visibility: config.VisibilityPublic, Protocol: config.ProtocolHTTP}); err != nil {
		t.Fatal(err)
	}
	var addr string
	for _, s := range h.node.Services() {
		if s.Name == "blog" {
			addr = s.Addresses["clearnet"]
		}
	}
	if addr == "" {
		t.Fatal("public service has no clearnet address")
	}
	body, err := get("http://" + addr + "/")
	if err != nil || body != "public page" {
		t.Fatalf("public GET = %q, %v", body, err)
	}

	// Removing the service takes it offline.
	if err := h.node.RemoveService("blog"); err != nil {
		t.Fatal(err)
	}
	if _, err := get("http://" + addr + "/"); err == nil {
		t.Fatal("removed public service still reachable")
	}
}

func TestAnonymousServiceNeverOnClearnet(t *testing.T) {
	h := start(t)
	if err := h.node.AddService(config.Service{Name: "hidden", LocalPort: 9, Visibility: config.VisibilityAnonymousPublic}); err != nil {
		t.Fatal(err)
	}
	for _, s := range h.node.Services() {
		if s.Name == "hidden" && s.Addresses["clearnet"] != "" {
			t.Fatal("anonymous-public service was published on clearnet")
		}
	}
}

func TestPanelGuards(t *testing.T) {
	h := start(t)
	base := "http://" + h.node.PanelAddr()
	do := func(method, path, host, csrf string) int {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(`{}`))
		if host != "" {
			req.Host = host
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := do("GET", "/api/status", "", ""); code != 200 {
		t.Fatalf("status: %d", code)
	}
	if code := do("GET", "/", "", ""); code != 200 {
		t.Fatalf("index: %d", code)
	}
	if code := do("GET", "/api/status", "evil.example:8080", ""); code != 403 {
		t.Fatalf("DNS-rebinding host allowed: %d", code)
	}
	if code := do("POST", "/api/devices/pair", "", ""); code != 403 {
		t.Fatalf("POST without CSRF allowed: %d", code)
	}
	if code := do("POST", "/api/devices/pair", "", "wrong"); code != 403 {
		t.Fatalf("POST with wrong CSRF allowed: %d", code)
	}
}
