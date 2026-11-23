package e2e

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/sdk/go/sol"
)

// echoServer listens on 127.0.0.1 and echoes every line back.
func echoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func admin(t *testing.T, d *sold.Daemon) func(method, path string, in, out any) error {
	t.Helper()
	c, err := sold.Connect(d.Dir())
	if err != nil {
		t.Fatal(err)
	}
	return func(method, path string, in, out any) error { return c.Do(context.Background(), method, path, in, out) }
}

// offered reports whether d's admin API lists person sharing service with it.
func offered(t *testing.T, d *sold.Daemon, person, service string) bool {
	var people []sold.PersonInfo
	if err := admin(t, d)("GET", "/admin/v1/people", nil, &people); err != nil {
		t.Fatal(err)
	}
	for _, p := range people {
		if p.ID != person {
			continue
		}
		for _, s := range p.SharesWithMe {
			if s.Kind == "service" && s.Name == service {
				return true
			}
		}
	}
	return false
}

// roundTrip writes a line to conn and reads it back.
func roundTrip(conn net.Conn, br *bufio.Reader, line string) (string, error) {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		return "", err
	}
	return br.ReadString('\n')
}

// TestPortSharing: Alice shares a local TCP echo server with Bob; Bob opens a tunnel and
// round-trips bytes through it; unsharing cuts the open stream and refuses new ones; an HTTP
// service opens at http://<service>.<petname>.localhost:<port>/ through Bob's UI router.
func TestPortSharing(t *testing.T) {
	alice := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	bob := startNode(t, sold.InitOptions{Name: "Bob", Device: "desktop"}, nil)
	pairNodes(t, alice, bob)
	waitSession(t, bob, alice)
	A, B := admin(t, alice), admin(t, bob)

	// Not shared yet: Bob cannot open a tunnel.
	if err := B("POST", "/admin/v1/tunnels", map[string]any{"person": alice.PersonID(), "service": "echo"}, nil); !errors.Is(err, sol.ErrNotFound) {
		t.Fatalf("tunnel before sharing: %v", err)
	}
	port := echoServer(t)
	if err := A("POST", "/admin/v1/services", sold.Service{Name: "echo", Port: port, People: []string{bob.PersonID()}}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Bob learns of the service", func() bool { return offered(t, bob, alice.PersonID(), "echo") })

	var tun sold.TunnelInfo
	if err := B("POST", "/admin/v1/tunnels", map[string]any{"person": "alice", "service": "echo"}, &tun); err != nil {
		t.Fatal(err)
	}
	if tun.State != "open" || tun.Port == 0 || !strings.HasPrefix(tun.Addr, "127.0.0.1:") {
		t.Fatalf("%+v", tun)
	}
	conn, err := net.Dial("tcp", tun.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	for _, msg := range []string{"hello alice", strings.Repeat("x", 100000)} {
		if got, err := roundTrip(conn, br, msg); err != nil || got != msg+"\n" {
			t.Fatalf("round trip: %q %v", got[:min(len(got), 40)], err)
		}
	}
	var tuns []sold.TunnelInfo
	if err := B("GET", "/admin/v1/tunnels", nil, &tuns); err != nil || len(tuns) != 1 || tuns[0].Active != 1 || tuns[0].Device != alice.DeviceID() {
		t.Fatalf("%+v %v", tuns, err)
	}

	// Unsharing cuts the open stream, and new connections get nowhere.
	if err := A("DELETE", "/admin/v1/services/echo", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := roundTrip(conn, br, "still there?"); err == nil {
		t.Fatal("the stream survived unsharing")
	}
	eventually(t, "the tunnel shows it is no longer shared", func() bool {
		_ = B("GET", "/admin/v1/tunnels", nil, &tuns)
		return len(tuns) == 1 && tuns[0].State == "not_shared"
	})
	c2, err := net.Dial("tcp", tun.Addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := roundTrip(c2, bufio.NewReader(c2), "anyone?"); err == nil {
		t.Fatal("a new stream got through after unsharing")
	}
	c2.Close()
	if err := B("DELETE", "/admin/v1/tunnels/"+tun.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if c3, err := net.Dial("tcp", tun.Addr); err == nil {
		c3.Close()
		t.Fatal("the closed tunnel still listens")
	}

	// An HTTP service through Bob's UI router.
	web := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "web says hi to "+r.URL.Path)
	})}
	wl, _ := net.Listen("tcp", "127.0.0.1:0")
	go web.Serve(wl)
	defer web.Close()
	if err := A("POST", "/admin/v1/services", sold.Service{Name: "web", Port: wl.Addr().(*net.TCPAddr).Port, HTTP: true,
		People: []string{bob.PersonID()}}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Bob learns of the web service", func() bool { return offered(t, bob, alice.PersonID(), "web") })
	if code, body := ui(t, bob, "web.alice", "GET", "/page", "", "", nil); code != 200 || body != "web says hi to /page" {
		t.Fatalf("petname router: %d %q", code, body)
	}
	// A petname nobody has: nothing there.
	if code, _ := ui(t, bob, "web.nobody", "GET", "/", "", "", nil); code != 404 {
		t.Fatalf("unknown petname: %d", code)
	}
}

// TestPeerAppUI: http://<app>.<petname>.localhost:<port>/ proxies a contact's app UI once they
// share a feed of that app with me, with me as the caller.
func TestPeerAppUI(t *testing.T) {
	alice := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	bob := startNode(t, sold.InitOptions{Name: "Bob", Device: "desktop"}, nil)
	pairNodes(t, alice, bob)
	waitSession(t, bob, alice)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.URL.Path+" for "+r.Header.Get("Sol-Person")[:4]+" self="+r.Header.Get("Sol-Self"))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	m, err := sol.ParseManifest([]byte(`{"id":"pui","version":"1","exec":"x","ui":"/","scopes":["feeds"]}`))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := alice.Host.Attach(m, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	api, _ := sol.New(alice.APIURL(), tok)
	ctx := context.Background()
	if _, err := api.Append(ctx, "f", 1); err != nil {
		t.Fatal(err)
	}
	// Right after pairing both sides may dial; a request riding the duplicate session that the
	// tie-break closes fails with 502 once (C11). Wait that out, then insist on Alice's 403.
	var code int
	var body string
	eventually(t, "a settled session", func() bool {
		code, body = ui(t, bob, "pui.alice", "GET", "/x", "", "", map[string]string{"Sol-Person": "forged"})
		return code != http.StatusBadGateway
	})
	if code != http.StatusForbidden {
		t.Fatalf("before sharing: %d %q", code, body)
	}
	if err := api.Share(ctx, sol.ShareRequest{Feed: "f", People: []string{bob.PersonID()}}); err != nil {
		t.Fatal(err)
	}
	want := "/x for " + bob.PersonID()[:4] + " self="
	eventually(t, "the shared app's UI", func() bool {
		code, body := ui(t, bob, "pui.alice", "GET", "/x", "", "", map[string]string{"Sol-Person": "forged"})
		return code == 200 && body == want
	})
	// The kit is served locally on a peer origin; the API bridge is not offered.
	if code, _ := ui(t, bob, "pui.alice", "GET", "/kit/kit.css", "", "", nil); code != 200 {
		t.Fatal(code)
	}
	if code, _ := ui(t, bob, "pui.alice", "GET", "/_sol/v1/whoami", "", "", nil); code != http.StatusNotImplemented {
		t.Fatal(code)
	}
}

// TestLastingTunnelAndUnpair: a petname is local and routable, a tunnel survives a restart on
// the same port, and unpairing drops the contact on both sides along with the tunnel and the
// share.
func TestLastingTunnelAndUnpair(t *testing.T) {
	alice := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	bob := startNode(t, sold.InitOptions{Name: "Bob", Device: "desktop"}, nil)
	pairNodes(t, alice, bob)
	waitSession(t, bob, alice)
	A, B := admin(t, alice), admin(t, bob)
	var p sold.PersonInfo
	if err := B("PATCH", "/admin/v1/people/"+alice.PersonID(), map[string]string{"petname": "ally"}, &p); err != nil || p.Petname != "ally" {
		t.Fatalf("%+v %v", p, err)
	}
	if err := B("PATCH", "/admin/v1/people/"+alice.PersonID(), map[string]string{"petname": "Not A Label"}, nil); !errors.Is(err, sol.ErrBadRequest) {
		t.Fatal(err)
	}
	port := echoServer(t)
	if err := A("POST", "/admin/v1/services", sold.Service{Name: "echo", Port: port, People: []string{bob.PersonID()}}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Bob learns of the service", func() bool { return offered(t, bob, alice.PersonID(), "echo") })
	var tun sold.TunnelInfo
	if err := B("POST", "/admin/v1/tunnels", map[string]any{"person": "ally", "service": "echo"}, &tun); err != nil {
		t.Fatal(err)
	}

	// Bob restarts: the tunnel is back on the same port and works.
	dir := bob.Dir()
	bob.Close()
	bob2, err := sold.Start(sold.Options{Dir: dir, InProcessApps: true, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer bob2.Close()
	waitSession(t, bob2, alice)
	B = admin(t, bob2)
	var tuns []sold.TunnelInfo
	if err := B("GET", "/admin/v1/tunnels", nil, &tuns); err != nil || len(tuns) != 1 || tuns[0].Port != tun.Port {
		t.Fatalf("%+v %v", tuns, err)
	}
	eventually(t, "a round trip after the restart", func() bool {
		c, err := net.Dial("tcp", tun.Addr)
		if err != nil {
			return false
		}
		defer c.Close()
		got, err := roundTrip(c, bufio.NewReader(c), "back")
		return err == nil && got == "back\n"
	})

	// Bob unpairs Alice: both sides drop the contact, the tunnel goes, Alice's share forgets Bob.
	if err := B("DELETE", "/admin/v1/people/"+alice.PersonID(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := B("GET", "/admin/v1/tunnels", nil, &tuns); err != nil || len(tuns) != 0 {
		t.Fatalf("%+v %v", tuns, err)
	}
	eventually(t, "Alice drops Bob", func() bool {
		var people []sold.PersonInfo
		_ = A("GET", "/admin/v1/people", nil, &people)
		var svcs []sold.Service
		_ = A("GET", "/admin/v1/services", nil, &svcs)
		return len(people) == 0 && len(svcs) == 1 && len(svcs[0].People) == 0
	})
}
