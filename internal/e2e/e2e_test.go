// Package e2e runs real nodes in one process over the clearnet transport,
// without needing Tor. Two nodes pair as peers (docs/PEERS.md) and exercise
// everything between them: sharing, tunnels, sending files both ways,
// unpairing from either side, and the guards that keep strangers out. If
// this passes, the handshake, pairing, routing, removal, and panel guards all
// work together.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/control"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/node"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/peers"
	"github.com/apv022/sol/internal/protocol"
	"github.com/apv022/sol/internal/transport"
)

type testNode struct {
	*node.Node
	t   *testing.T
	ctx context.Context
	dir string
}

type opt func(*node.Options)

// newNode starts a node called name with only the clearnet transport.
func newNode(t *testing.T, name string, opts ...opt) *testNode {
	t.Helper()
	dir := t.TempDir()
	return startNode(t, dir, name, opts...)
}

func startNode(t *testing.T, dir, name string, opts ...opt) *testNode {
	t.Helper()
	o := node.Options{
		DataDir:        dir,
		Version:        "test",
		PanelAddr:      "127.0.0.1:0",
		NodeAddr:       "127.0.0.1:0",
		ControlPath:    filepath.Join(dir, "c.sock"),
		Transports:     []transport.Transport{transport.NewClearnet("127.0.0.1:0", "")},
		DialAttempts:   1,
		DialRetryDelay: 10 * time.Millisecond,
	}
	for _, f := range opts {
		f(&o)
	}
	n, err := node.New(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.SetName(name); err != nil {
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
	return &testNode{Node: n, t: t, ctx: ctx, dir: dir}
}

// pair makes a and b peers (b joins a's link) and returns each one's record
// of the other.
func pair(t *testing.T, a, b *testNode) (bOnA, aOnB config.Peer) {
	t.Helper()
	inv, err := a.Invite()
	if err != nil {
		t.Fatal(err)
	}
	aOnB, err = b.Pair(b.ctx, inv.Link)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	bOnA = findPeer(t, a, b.Name())
	return bOnA, aOnB
}

func findPeer(t *testing.T, n *testNode, name string) config.Peer {
	t.Helper()
	for _, p := range n.Peers() {
		if p.Name == name {
			return config.Peer{ID: p.ID, Name: p.Name}
		}
	}
	t.Fatalf("%s has no peer called %q", n.Name(), name)
	return config.Peer{}
}

// webApp starts a local HTTP app and registers it on n as service name.
func webApp(t *testing.T, n *testNode, name, reply string) {
	t.Helper()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s", reply, r.URL.Path)
	}))
	t.Cleanup(app.Close)
	port := app.Listener.Addr().(*net.TCPAddr).Port
	if err := n.AddService(config.Service{Name: name, LocalPort: port, Visibility: config.VisibilityPrivate, Protocol: config.ProtocolHTTP}); err != nil {
		t.Fatal(err)
	}
}

// httpOver sends one GET over an already-open stream.
func httpOver(conn net.Conn, path string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext:       func(context.Context, string, string) (net.Conn, error) { return conn, nil },
		DisableKeepAlives: true,
	}}
	resp, err := client.Get("http://peer" + path)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
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

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 100 {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPairShareOpenUnpair(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	webApp(t, a, "web", "alpha says")
	bOnA, aOnB := pair(t, a, b)

	// 1. Pairing is mutual and names travel both ways.
	if aOnB.Name != "alpha" || bOnA.Name != "beta" {
		t.Fatalf("names: b sees %q, a sees %q", aOnB.Name, bOnA.Name)
	}
	// A new peer gets the inbox and nothing else.
	if len(aOnB.Services) != 1 || aOnB.Services[0].Name != peers.Inbox {
		t.Fatalf("a shares with b by default: %+v", aOnB.Services)
	}
	// The inviter learns what the joiner shares on its own, shortly after.
	waitFor(t, "a to learn b's shares", func() bool {
		ps := a.Peers()
		return len(ps) == 1 && len(ps[0].Services) == 1 && ps[0].Services[0].Name == peers.Inbox
	})

	// 2. A service that isn't shared is refused.
	if _, err := b.DialPeer(b.ctx, aOnB.ID, "web"); err == nil {
		t.Fatal("unshared service was reachable")
	}

	// 3. Share it; b sees it after a refresh and reaches it.
	if _, err := a.SetShare(bOnA.ID, "web", true); err != nil {
		t.Fatal(err)
	}
	if p, err := b.RefreshPeer(b.ctx, aOnB.ID); err != nil || len(p.Services) != 2 {
		t.Fatalf("refresh: %+v, %v", p.Services, err)
	}
	conn, err := b.DialPeer(b.ctx, aOnB.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	if body, err := httpOver(conn, "/song.flac"); err != nil || body != "alpha says /song.flac" {
		t.Fatalf("GET over peer = %q, %v", body, err)
	}

	// 4. Either side can open things on the other: a reaches b's inbox.
	if c, err := a.DialPeer(a.ctx, bOnA.ID, peers.Inbox); err != nil {
		t.Fatalf("a -> b inbox: %v", err)
	} else {
		c.Close()
	}

	// 5. The panel shows the live, verified connection on both sides.
	live, err := b.DialPeer(b.ctx, aOnB.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a to show b online", func() bool {
		for _, p := range a.Peers() {
			if p.ID == bOnA.ID && p.Online && len(p.Connections) > 0 && p.Connections[0].Service == "web" {
				return true
			}
		}
		return false
	})

	// 6. a unpairs: b is told and removes a too, and the live stream dies.
	_, notified, err := a.Unpair(a.ctx, bOnA.ID)
	if err != nil || !notified {
		t.Fatalf("unpair: notified=%v err=%v", notified, err)
	}
	live.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := live.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("live connection survived unpairing: %v", err)
	}
	waitFor(t, "b to drop a", func() bool { return len(b.Peers()) == 0 })
	if len(a.Peers()) != 0 {
		t.Fatalf("a still lists b: %+v", a.Peers())
	}
}

func TestUnpairFromTheOtherSide(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	bOnA, aOnB := pair(t, a, b)
	if _, notified, err := b.Unpair(b.ctx, aOnB.ID); err != nil || !notified {
		t.Fatalf("unpair: %v %v", notified, err)
	}
	waitFor(t, "a to drop b", func() bool { return len(a.Peers()) == 0 })
	// Even if a still had a record, b no longer accepts it.
	if _, err := a.DialPeer(a.ctx, bOnA.ID, peers.Inbox); err == nil {
		t.Fatal("a still reached b after unpairing")
	}
}

func TestUnsharingClosesConnections(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	webApp(t, a, "web", "hi")
	bOnA, aOnB := pair(t, a, b)
	a.SetShare(bOnA.ID, "web", true)
	live, err := b.DialPeer(b.ctx, aOnB.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "connection", func() bool { return a.Peers()[0].Online })
	if _, err := a.SetShare(bOnA.ID, "web", false); err != nil {
		t.Fatal(err)
	}
	live.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := live.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("connection survived unsharing: %v", err)
	}
	if _, err := b.DialPeer(b.ctx, aOnB.ID, "web"); err == nil {
		t.Fatal("unshared service still reachable")
	}
}

func TestLinkRules(t *testing.T) {
	a, b, c := newNode(t, "alpha"), newNode(t, "beta"), newNode(t, "gamma")
	inv, err := a.Invite()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Pair(a.ctx, inv.Link); err == nil {
		t.Fatal("a node paired with its own link")
	}
	if _, err := b.Pair(b.ctx, inv.Link); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pair(c.ctx, inv.Link); err == nil {
		t.Fatal("a pairing link worked twice")
	}
	if len(a.Peers()) != 1 {
		t.Fatalf("a's peers: %+v", a.Peers())
	}
}

func TestSendFilesBothWays(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	bOnA, aOnB := pair(t, a, b)

	send := func(from *testNode, to config.Peer, name, content string) string {
		t.Helper()
		hash, size, err := from.SendFile(from.ctx, to.ID, name, strings.NewReader(content))
		if err != nil {
			t.Fatalf("%s -> %s: %v", from.Name(), to.Name, err)
		}
		sum := sha256.Sum256([]byte(content))
		if hash != hex.EncodeToString(sum[:]) || size != int64(len(content)) {
			t.Fatalf("hash/size = %s/%d", hash, size)
		}
		return hash
	}
	h1 := send(b, aOnB, "notes.txt", "from beta to alpha")
	h2 := send(a, bOnA, "kit.tar.gz", "from alpha to beta")

	// The receiver has the file, labelled with its name and sender.
	checkReceived := func(n *testNode, hash, name, fromID string) {
		t.Helper()
		list, _ := n.Blobs().List()
		for _, e := range list {
			if e.Hash == hash {
				if e.Name != name || e.From != fromID {
					t.Fatalf("%s stored %+v", n.Name(), e)
				}
				return
			}
		}
		t.Fatalf("%s didn't store %s", n.Name(), name)
	}
	checkReceived(a, h1, "notes.txt", bOnA.ID)
	checkReceived(b, h2, "kit.tar.gz", aOnB.ID)

	// Received files are private: not on a's shared-files page for b.
	a.SetShare(bOnA.ID, peers.Files, true)
	conn, err := b.DialPeer(b.ctx, aOnB.ID, peers.Files)
	if err != nil {
		t.Fatal(err)
	}
	if page, err := httpOver(conn, "/"); err != nil || strings.Contains(page, "notes.txt") {
		t.Fatalf("received file listed on _files: %v %q", err, page)
	}
}

func TestInboxLimits(t *testing.T) {
	a := newNode(t, "alpha", func(o *node.Options) { o.InboxMaxFile = 10; o.InboxMaxTotal = 15 })
	b := newNode(t, "beta")
	_, aOnB := pair(t, a, b)
	if _, _, err := b.SendFile(b.ctx, aOnB.ID, "big.bin", strings.NewReader("0123456789x")); err == nil {
		t.Fatal("file over the per-file limit was accepted")
	}
	if _, _, err := b.SendFile(b.ctx, aOnB.ID, "one.bin", strings.NewReader("0123456789")); err != nil {
		t.Fatalf("file at the limit: %v", err)
	}
	if _, _, err := b.SendFile(b.ctx, aOnB.ID, "two.bin", strings.NewReader("abcdefgh")); err == nil {
		t.Fatal("file over the total limit was accepted")
	}
	if list, _ := a.Blobs().List(); len(list) != 1 {
		t.Fatalf("a stored %d files, want 1", len(list))
	}
}

func TestStrangerGetsNothing(t *testing.T) {
	a := newNode(t, "alpha")
	webApp(t, a, "web", "secret")
	inv, _ := a.Invite()
	l, err := pairing.ParseLink(inv.Link)
	if err != nil {
		t.Fatal(err)
	}
	stranger, _ := identity.Generate()
	addr := l.Addrs["clearnet"]

	// Raw TCP to the node: a key that isn't a peer is closed without a reply.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	err = protocol.ClientHandshake(conn, stranger, l.NodeID, protocol.Request{Op: protocol.OpConnect, Service: "web"})
	if !errors.Is(err, protocol.ErrRejected) {
		t.Fatalf("stranger: want ErrRejected, got %v", err)
	}
	// Plain HTTP to the node gets no HTTP response at all.
	if body, err := get("http://" + addr + "/"); err == nil {
		t.Fatalf("plain HTTP got a response: %q", body)
	}
}

// Two nodes on one data directory would fight over its keys; the second is
// refused while the first runs.
func TestOneNodePerDataDir(t *testing.T) {
	first := newNode(t, "beta")
	second, err := node.New(node.Options{DataDir: first.dir, PanelAddr: "off", NodeAddr: "127.0.0.1:0",
		ControlPath: first.ControlPath(), Transports: []transport.Transport{transport.NewClearnet("127.0.0.1:0", "")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second node on the same data dir: %v", err)
	}
}

func TestReloadedPeerReconnects(t *testing.T) {
	a := newNode(t, "alpha")
	webApp(t, a, "web", "still here")
	bDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	b1, err := node.New(node.Options{DataDir: bDir, PanelAddr: "off", NodeAddr: "127.0.0.1:0", ControlPath: "off",
		Transports: []transport.Transport{transport.NewClearnet("127.0.0.1:0", "")}, DialAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- b1.Run(ctx) }()
	<-b1.Ready()
	inv, _ := a.Invite()
	aOnB, err := b1.Pair(ctx, inv.Link)
	if err != nil {
		t.Fatal(err)
	}
	a.SetShare(findPeer(t, a, b1.Name()).ID, "web", true)
	cancel()
	<-done

	b2 := startNode(t, bDir, "beta")
	conn, err := b2.DialPeer(b2.ctx, aOnB.ID, "web")
	if err != nil {
		t.Fatalf("after restart: %v", err)
	}
	if body, err := httpOver(conn, "/"); err != nil || body != "still here /" {
		t.Fatalf("after restart GET = %q, %v", body, err)
	}
}

// The CLI path: everything through the control socket, as `sol` commands do.
func TestControlSocket(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	webApp(t, a, "web", "via socket")
	ctx := context.Background()

	var inv control.Invite
	if err := control.Call(ctx, a.ControlPath(), control.Request{Op: control.OpInvite}, &inv); err != nil {
		t.Fatal(err)
	}
	var aOnB control.PeerView
	if err := control.Call(ctx, b.ControlPath(), control.Request{Op: control.OpPair, Link: inv.Link}, &aOnB); err != nil {
		t.Fatal(err)
	}
	var bOnA control.PeerView
	if err := control.Call(ctx, a.ControlPath(), control.Request{Op: control.OpShare, Peer: "beta", Service: "web", Shared: true}, &bOnA); err != nil {
		t.Fatal(err)
	}

	// sol peers -r alpha
	var list []control.PeerView
	if err := control.Call(ctx, b.ControlPath(), control.Request{Op: control.OpPeers, Peer: "alpha", Refresh: true}, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || len(list[0].Services) != 2 {
		t.Fatalf("peers: %+v", list)
	}

	// sol open: a dial stream through the socket.
	c, err := control.Dial(ctx, b.ControlPath())
	if err != nil {
		t.Fatal(err)
	}
	c.Send(control.Request{Op: control.OpDial, Peer: "alpha", Service: "web"})
	if err := c.Recv(nil); err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(c.Conn, "GET /x HTTP/1.0\r\nHost: peer\r\n\r\n")
	resp, _ := io.ReadAll(c)
	c.Close()
	if !bytes.HasSuffix(resp, []byte("via socket /x")) {
		t.Fatalf("dial through socket: %q", resp)
	}

	// sol send
	s, err := control.Dial(ctx, b.ControlPath())
	if err != nil {
		t.Fatal(err)
	}
	s.Send(control.Request{Op: control.OpSend, Peer: "alpha", Name: "hello.txt"})
	if err := s.Recv(nil); err != nil {
		t.Fatal(err)
	}
	io.WriteString(s.Conn, "sent over the socket")
	s.CloseWrite()
	var sent control.Sent
	if err := s.Recv(&sent); err != nil || sent.Size != int64(len("sent over the socket")) {
		t.Fatalf("send: %+v %v", sent, err)
	}
	s.Close()

	// sol unpair
	var u control.Unpaired
	if err := control.Call(ctx, b.ControlPath(), control.Request{Op: control.OpUnpair, Peer: "alpha"}, &u); err != nil || !u.Notified {
		t.Fatalf("unpair: %+v %v", u, err)
	}

	// Nothing answers once the node is gone.
	if err := control.Call(ctx, filepath.Join(t.TempDir(), "none.sock"), control.Request{Op: control.OpStatus}, nil); !errors.Is(err, control.ErrNotRunning) {
		t.Fatalf("no node: %v", err)
	}
}

func TestPublicServiceNeedsNoPairing(t *testing.T) {
	a := newNode(t, "alpha")
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "nginx/1.2.3")
		io.WriteString(w, "public page")
	}))
	defer app.Close()
	port := app.Listener.Addr().(*net.TCPAddr).Port
	if err := a.AddService(config.Service{Name: "blog", LocalPort: port, Visibility: config.VisibilityPublic, Protocol: config.ProtocolHTTP}); err != nil {
		t.Fatal(err)
	}
	var addr string
	for _, s := range a.Services() {
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
	if err := a.RemoveService("blog"); err != nil {
		t.Fatal(err)
	}
	if _, err := get("http://" + addr + "/"); err == nil {
		t.Fatal("removed public service still reachable")
	}
}

func TestAnonymousServiceNeverOnClearnet(t *testing.T) {
	a := newNode(t, "alpha")
	if err := a.AddService(config.Service{Name: "hidden", LocalPort: 9, Visibility: config.VisibilityAnonymousPublic}); err != nil {
		t.Fatal(err)
	}
	for _, s := range a.Services() {
		if s.Name == "hidden" && s.Addresses["clearnet"] != "" {
			t.Fatal("anonymous-public service was published on clearnet")
		}
	}
}

func TestPanelGuards(t *testing.T) {
	a := newNode(t, "alpha")
	base := "http://" + a.PanelAddr()
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
	if code := do("POST", "/api/peers/invite", "", ""); code != 403 {
		t.Fatalf("POST without CSRF allowed: %d", code)
	}
	if code := do("POST", "/api/peers/invite", "", "wrong"); code != 403 {
		t.Fatalf("POST with wrong CSRF allowed: %d", code)
	}
}
