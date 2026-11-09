package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/control"
	"github.com/apv022/sol/internal/node"
	"github.com/apv022/sol/internal/transport"
)

// shareWeb makes a share an HTTP app "web" with b and returns a's id on b.
func shareWeb(t *testing.T, a, b *testNode, reply string) string {
	t.Helper()
	webApp(t, a, "web", reply)
	bOnA, aOnB := pair(t, a, b)
	if _, err := a.SetShare(bOnA.ID, "web", true); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RefreshPeer(b.ctx, aOnB.ID); err != nil {
		t.Fatal(err)
	}
	return aOnB.ID
}

func getHost(url, host string) (int, string, error) {
	req, _ := http.NewRequest("GET", url, nil)
	if host != "" {
		req.Host = host
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func TestTunnelOpensHTTPService(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	aID := shareWeb(t, a, b, "alpha says")

	tun, err := b.OpenTunnel(aID, "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if tun.URL == "" || !strings.HasPrefix(tun.Addr, "127.0.0.1:") || tun.PeerName != "alpha" {
		t.Fatalf("tunnel view: %+v", tun)
	}
	if body, err := get(tun.URL + "song.flac"); err != nil || body != "alpha says /song.flac" {
		t.Fatalf("GET through tunnel = %q, %v", body, err)
	}
	// localhost works too; anything else is a DNS-rebinding attempt.
	if code, _, err := getHost(tun.URL, fmt.Sprintf("localhost:%d", tun.Port)); err != nil || code != 200 {
		t.Fatalf("localhost Host: %d %v", code, err)
	}
	if code, _, err := getHost(tun.URL, fmt.Sprintf("evil.example:%d", tun.Port)); err != nil || code != http.StatusForbidden {
		t.Fatalf("foreign Host got %d (%v), want 403", code, err)
	}

	// Opening it again returns the same tunnel.
	again, err := b.OpenTunnel(aID, "web", 0)
	if err != nil || again.Port != tun.Port {
		t.Fatalf("reopen: %+v %v", again, err)
	}
	if got := b.Tunnels(); len(got) != 1 || got[0].Via != "clearnet" || got[0].LastOK.IsZero() {
		t.Fatalf("tunnel status: %+v", got)
	}

	// Services the peer doesn't share can't be tunneled.
	if _, err := b.OpenTunnel(aID, "nope", 0); err == nil {
		t.Fatal("tunnel to an unshared service")
	}

	// Closing frees the port and forgets the tunnel.
	if err := b.CloseTunnel(tun.Port); err != nil {
		t.Fatal(err)
	}
	if _, err := get(tun.URL); err == nil {
		t.Fatal("closed tunnel still answers")
	}
	if len(b.Tunnels()) != 0 {
		t.Fatal("closed tunnel still listed")
	}
}

func TestTunnelTCPService(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	if err := a.AddService(config.Service{Name: "echo", LocalPort: echo.Addr().(*net.TCPAddr).Port, Visibility: config.VisibilityPrivate}); err != nil {
		t.Fatal(err)
	}
	bOnA, aOnB := pair(t, a, b)
	a.SetShare(bOnA.ID, "echo", true)
	b.RefreshPeer(b.ctx, aOnB.ID)

	tun, err := b.OpenTunnel(aOnB.ID, "echo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if tun.URL != "" || tun.Protocol != config.ProtocolTCP {
		t.Fatalf("tcp tunnel view: %+v", tun)
	}
	c, err := net.Dial("tcp", tun.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintln(c, "ping")
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := bufio.NewReader(c).ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("echo through tunnel = %q, %v", line, err)
	}
}

func TestUnpairClosesTunnels(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	aID := shareWeb(t, a, b, "hi")
	tun, err := b.OpenTunnel(aID, "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Unpair(b.ctx, aID); err != nil {
		t.Fatal(err)
	}
	if _, err := get(tun.URL); err == nil {
		t.Fatal("tunnel survived unpairing")
	}
	if len(b.Tunnels()) != 0 {
		t.Fatal("unpaired peer's tunnel still listed")
	}
}

// freePort returns a port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// The point of saved tunnels: the same port works after either side
// restarts, with nothing to do by hand.
func TestTunnelSurvivesRestarts(t *testing.T) {
	// a needs a fixed clearnet port so b can find it again after a restarts.
	aDir, aAddr := t.TempDir(), fmt.Sprintf("127.0.0.1:%d", freePort(t))
	fixed := func(o *node.Options) { o.Transports = []transport.Transport{transport.NewClearnet(aAddr, "")} }
	a := startNode(t, aDir, "alpha", fixed)
	bDir := t.TempDir()
	b := startNode(t, bDir, "beta")
	aID := shareWeb(t, a, b, "alpha says")
	tun, err := b.OpenTunnel(aID, "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if body, err := get(tun.URL); err != nil || body != "alpha says /" {
		t.Fatalf("before restarts: %q %v", body, err)
	}

	// 1. The peer goes away: the tunnel stays up and explains, instead of vanishing.
	a.stop()
	if code, body, err := getHost(tun.URL, ""); err != nil || code != http.StatusBadGateway || !strings.Contains(body, "reload") {
		t.Fatalf("peer down: %d %q %v", code, body, err)
	}
	if v := b.Tunnels(); len(v) != 1 || v[0].LastError == "" {
		t.Fatalf("peer down should be reported: %+v", v)
	}

	// 2. The peer comes back (its web app too): the same URL works again by itself.
	a = startNode(t, aDir, "alpha", fixed)
	webAppOnPort(t, a, "web")
	if body, err := get(tun.URL); err != nil || body != "alpha says /" {
		t.Fatalf("peer back: %q %v", body, err)
	}

	// 3. This node restarts: the tunnel reopens on the same port.
	b.stop()
	b = startNode(t, bDir, "beta")
	if v := b.Tunnels(); len(v) != 1 || v[0].Port != tun.Port || !v[0].Listening {
		t.Fatalf("after restart: %+v", v)
	}
	if body, err := get(tun.URL); err != nil || body != "alpha says /" {
		t.Fatalf("after restart: %q %v", body, err)
	}
}

// webAppOnPort restarts an app on the port service name already has in n's
// config (the app from webApp was a test server that is still running, so
// this only checks the service survived the node's restart).
func webAppOnPort(t *testing.T, n *testNode, name string) {
	t.Helper()
	for _, s := range n.Services() {
		if s.Name == name {
			return
		}
	}
	t.Fatalf("%s lost service %q across a restart", n.Name(), name)
}

// The CLI path: sol open / tunnels / close through the control socket.
func TestTunnelsOverControlSocket(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	shareWeb(t, a, b, "alpha says")
	path := b.ControlPath()
	ctx := context.Background()

	var tun control.Tunnel
	if err := control.Call(ctx, path, control.Request{Op: control.OpTunnelOpen, Peer: "alpha", Service: "web"}, &tun); err != nil {
		t.Fatal(err)
	}
	if body, err := get(tun.URL); err != nil || body != "alpha says /" {
		t.Fatalf("GET = %q %v", body, err)
	}
	var list []control.Tunnel
	if err := control.Call(ctx, path, control.Request{Op: control.OpTunnels}, &list); err != nil || len(list) != 1 || list[0].Peer != "alpha" {
		t.Fatalf("tunnels: %+v %v", list, err)
	}
	if err := control.Call(ctx, path, control.Request{Op: control.OpTunnelClose, Peer: "alpha"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := control.Call(ctx, path, control.Request{Op: control.OpTunnels}, &list); err != nil || len(list) != 0 {
		t.Fatalf("after close: %+v %v", list, err)
	}
}

// Review fixes: closing cuts live sessions, a busy port is retried, and
// asking for a different port than the open tunnel uses is an error.
func TestTunnelCloseCutsSessionsAndRetriesBusyPort(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	a.AddService(config.Service{Name: "ssh", LocalPort: echo.Addr().(*net.TCPAddr).Port, Visibility: config.VisibilityPrivate})
	bOnA, aOnB := pair(t, a, b)
	a.SetShare(bOnA.ID, "ssh", true)
	b.RefreshPeer(b.ctx, aOnB.ID)

	tun, err := b.OpenTunnel(aOnB.ID, "ssh", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.OpenTunnel(aOnB.ID, "ssh", tun.Port+1); err == nil {
		t.Fatal("a different port for an open tunnel should be refused")
	}

	// A live session ends when the tunnel closes.
	c, err := net.Dial("tcp", tun.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintln(c, "hi")
	r := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err := b.CloseTunnel(tun.Port); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := r.ReadString('\n'); err == nil || isTimeout(err) {
		t.Fatalf("session survived closing the tunnel: %v", err)
	}

	// A saved tunnel whose port is busy at start-up comes up once it's free.
	tun, err = b.OpenTunnel(aOnB.ID, "ssh", 0)
	if err != nil {
		t.Fatal(err)
	}
	b.stop()
	squatter, err := net.Listen("tcp", tun.Addr)
	if err != nil {
		t.Fatal(err)
	}
	node.SetRetryPortEvery(t, 20*time.Millisecond)
	b = startNode(t, b.dir, "beta")
	if v := b.Tunnels(); len(v) != 1 || v[0].Listening {
		t.Fatalf("busy port should be reported: %+v", v)
	}
	squatter.Close()
	waitFor(t, "the tunnel to take its port back", func() bool {
		v := b.Tunnels()
		return len(v) == 1 && v[0].Listening
	})
	c2, err := net.Dial("tcp", tun.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	fmt.Fprintln(c2, "back")
	c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := bufio.NewReader(c2).ReadString('\n'); err != nil || line != "back\n" {
		t.Fatalf("after retry: %q %v", line, err)
	}
}
