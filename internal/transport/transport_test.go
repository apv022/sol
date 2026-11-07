package transport

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- SOCKS5 -----------------------------------------------------------------

// fakeSOCKS accepts one CONNECT, records the requested host:port, replies
// success, then echoes bytes.
func fakeSOCKS(t *testing.T) (addr string, got <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 3)
		io.ReadFull(c, buf) // 5 1 0
		c.Write([]byte{5, 0})
		hdr := make([]byte, 5)
		io.ReadFull(c, hdr) // 5 1 0 3 len
		host := make([]byte, hdr[4])
		io.ReadFull(c, host)
		var port [2]byte
		io.ReadFull(c, port[:])
		ch <- net.JoinHostPort(string(host), strconv.Itoa(int(binary.BigEndian.Uint16(port[:]))))
		c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		io.Copy(c, c)
	}()
	return ln.Addr().String(), ch
}

func TestSOCKS5SendsHostname(t *testing.T) {
	proxyAddr, got := fakeSOCKS(t)
	target := strings.Repeat("a", 56) + ".onion:80"
	conn, err := dialSOCKS5(context.Background(), proxyAddr, target)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if g := <-got; g != target {
		t.Fatalf("proxy saw %q, want %q", g, target)
	}
	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	io.ReadFull(conn, buf)
	if string(buf) != "ping" {
		t.Fatalf("echo = %q", buf)
	}
}

// --- Tor control port ---------------------------------------------------------

// fakeTorControl answers a scripted set of commands and records them.
type fakeTorControl struct {
	mu   sync.Mutex
	cmds []string
}

func (f *fakeTorControl) serve(t *testing.T, c net.Conn) {
	defer c.Close()
	tp := textproto.NewConn(c)
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.cmds = append(f.cmds, line)
		f.mu.Unlock()
		// Mirror real tor: client auth keys without Flags=V3Auth are refused.
		if strings.HasPrefix(line, "ADD_ONION") && strings.Contains(line, "ClientAuthV3=") && !strings.Contains(line, "Flags=V3Auth") {
			tp.PrintfLine("512 No auth type specified")
			continue
		}
		if strings.Contains(line, "ClientAuthV3=REJECTME") {
			tp.PrintfLine("512 Failed to decode v3 client authorization")
			continue
		}
		switch {
		case strings.HasPrefix(line, "AUTHENTICATE"), line == "TAKEOWNERSHIP",
			strings.HasPrefix(line, "DEL_ONION"), strings.HasPrefix(line, "ONION_CLIENT_AUTH_ADD"):
			tp.PrintfLine("250 OK")
		case line == "GETINFO status/bootstrap-phase":
			tp.PrintfLine(`250-status/bootstrap-phase=NOTICE BOOTSTRAP PROGRESS=100 TAG=done SUMMARY="Done"`)
			tp.PrintfLine("250 OK")
		case line == "GETINFO multi":
			tp.PrintfLine("250+multi=")
			tp.PrintfLine("line one")
			tp.PrintfLine("line two")
			tp.PrintfLine(".")
			tp.PrintfLine("250 OK")
		case strings.HasPrefix(line, "ADD_ONION NEW:"):
			tp.PrintfLine("250-ServiceID=%s", strings.Repeat("x", 56))
			tp.PrintfLine("250-PrivateKey=ED25519-V3:secretkeyblob")
			tp.PrintfLine("250 OK")
		case strings.HasPrefix(line, "ADD_ONION ED25519-V3:"):
			tp.PrintfLine("250-ServiceID=%s", strings.Repeat("x", 56))
			tp.PrintfLine("250 OK")
		default:
			tp.PrintfLine("510 Unrecognized command")
		}
	}
}

func startFakeControl(t *testing.T) (*torControl, *fakeTorControl) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeTorControl{}
	go func() {
		c, err := ln.Accept()
		if err == nil {
			f.serve(t, c)
		}
	}()
	cookie := filepath.Join(t.TempDir(), "cookie")
	os.WriteFile(cookie, []byte{0xde, 0xad, 0xbe, 0xef}, 0o600)
	ctrl, err := dialTorControl(context.Background(), ln.Addr().String(), cookie)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctrl.close() })
	return ctrl, f
}

func TestTorControlReplies(t *testing.T) {
	ctrl, f := startFakeControl(t)
	if f.cmds[0] != "AUTHENTICATE deadbeef" {
		t.Fatalf("auth command = %q", f.cmds[0])
	}
	v, err := ctrl.getInfo("multi")
	if err != nil || v != "line one\nline two" {
		t.Fatalf("multi-line GETINFO = %q, %v", v, err)
	}
	if _, err := ctrl.do("BOGUS"); err == nil || !strings.Contains(err.Error(), "510") {
		t.Fatalf("error reply not surfaced: %v", err)
	}
}

func TestTorPublishPersistsKeyAndClientAuth(t *testing.T) {
	ctrl, f := startFakeControl(t)
	tor := NewTor(t.TempDir(), nil)
	tor.ctrl = ctrl

	addr, err := tor.Publish(context.Background(), Publication{ID: "node", Target: "127.0.0.1:8081", ClientAuth: []string{"KEYA", "KEYB"}})
	if err != nil {
		t.Fatal(err)
	}
	if addr != strings.Repeat("x", 56)+".onion" {
		t.Fatalf("addr = %q", addr)
	}
	key, err := os.ReadFile(tor.keyPath("node"))
	if err != nil || strings.TrimSpace(string(key)) != "ED25519-V3:secretkeyblob" {
		t.Fatalf("key not persisted: %q %v", key, err)
	}
	last := lastCmd(f, "ADD_ONION")
	if strings.Count(last, "ClientAuthV3=") != 3 {
		t.Fatalf("self-check key not added: %q", last)
	}
	if !strings.Contains(last, "Flags=V3Auth") || !strings.Contains(last, "Port=80,127.0.0.1:8081") || !strings.Contains(last, "ClientAuthV3=KEYA") || !strings.Contains(last, "ClientAuthV3=KEYB") {
		t.Fatalf("ADD_ONION = %q", last)
	}

	// Re-publishing reuses the stored key and removes the old onion first.
	if _, err := tor.Publish(context.Background(), Publication{ID: "node", Target: "127.0.0.1:8081"}); err != nil {
		t.Fatal(err)
	}
	n := len(f.cmds)
	if !strings.HasPrefix(f.cmds[n-2], "DEL_ONION") || !strings.HasPrefix(f.cmds[n-1], "ADD_ONION ED25519-V3:secretkeyblob") {
		t.Fatalf("republish commands = %q", f.cmds[n-2:])
	}

	// A failed update must not take the onion offline: the old one is restored.
	if _, err := tor.Publish(context.Background(), Publication{ID: "node", Target: "127.0.0.1:8081", ClientAuth: []string{"REJECTME"}}); err == nil {
		t.Fatal("expected tor to reject the bad key")
	}
	if last := lastCmd(f, "ADD_ONION"); !strings.HasPrefix(last, "ADD_ONION ED25519-V3:") || strings.Contains(last, "REJECTME") {
		t.Fatalf("previous onion not restored, last command %q", last)
	}
	if _, ok := tor.Status().Addresses["node"]; !ok {
		t.Fatal("node onion dropped from status after failed update")
	}

	if err := tor.AuthorizeClient(strings.Repeat("y", 56)+".onion:80", "PRIV"); err != nil {
		t.Fatal(err)
	}
	if got := f.cmds[len(f.cmds)-1]; got != "ONION_CLIENT_AUTH_ADD "+strings.Repeat("y", 56)+" x25519:PRIV" {
		t.Fatalf("client auth = %q", got)
	}
	if _, err := tor.Publish(context.Background(), Publication{ID: "../evil", Target: "x"}); err == nil {
		t.Fatal("path traversal in publication id accepted")
	}
}

func lastCmd(f *fakeTorControl, prefix string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.cmds) - 1; i >= 0; i-- {
		if strings.HasPrefix(f.cmds[i], prefix) {
			return f.cmds[i]
		}
	}
	return ""
}

func TestStatusMeansReachable(t *testing.T) {
	tor := NewTor(t.TempDir(), nil)
	tor.state = StateReady
	if tor.Status().State != StateReady {
		t.Fatal("bootstrapped client with no onions should be ready")
	}
	tor.onions["node"] = &onion{serviceID: "x"}
	if st := tor.Status(); st.State != StateStarting || st.Detail == "" {
		t.Fatalf("unverified onion reported as %+v", st)
	}
	tor.reachable = true
	if st := tor.Status(); st.State != StateReady || st.Detail != "online and reachable" {
		t.Fatalf("verified onion reported as %+v", st)
	}
}

func TestNetSignatureIgnoresVirtual(t *testing.T) {
	for _, n := range []string{"lo", "docker0", "veth12ab", "virbr0"} {
		if !ignoredInterface(n) {
			t.Errorf("%s should be ignored", n)
		}
	}
	for _, n := range []string{"wlan0", "enp1s0", "proton0", "tun0", "wg0"} {
		if ignoredInterface(n) {
			t.Errorf("%s should be watched", n)
		}
	}
	first := netSignature()
	if second := netSignature(); first != second {
		t.Fatalf("signature not stable: %q vs %q", first, second)
	}
}

func TestFieldValue(t *testing.T) {
	line := `NOTICE BOOTSTRAP PROGRESS=45 TAG=loading SUMMARY="Loading relay descriptors"`
	if fieldValue(line, "PROGRESS") != "45" || fieldValue(line, "SUMMARY") != `"Loading relay descriptors"` {
		t.Fatal("fieldValue parse failed")
	}
}

// --- Clearnet + Registry --------------------------------------------------------

func TestClearnetForwards(t *testing.T) {
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	cn := NewClearnet("127.0.0.1:0", "")
	cn.Start(context.Background())
	defer cn.Stop()
	addr, err := cn.Publish(context.Background(), Publication{ID: "node", Target: echo.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := cn.Dial(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte("hi\n"))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if line != "hi\n" {
		t.Fatalf("echo = %q", line)
	}
	cn.Unpublish("node")
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Fatal("unpublished listener still accepting")
	}
}

func TestRegistryRouting(t *testing.T) {
	r := NewRegistry()
	r.Add(NewClearnet("127.0.0.1:0", ""))
	r.Add(NewTor(t.TempDir(), nil))
	r.Add(NewI2P("", nil))
	cases := map[string]string{
		strings.Repeat("a", 56) + ".onion": "tor",
		"abc.b32.i2p:80":                   "i2p",
		"192.168.1.2:7700":                 "clearnet",
	}
	for addr, want := range cases {
		tr, err := r.For(addr)
		if err != nil || tr.Name() != want {
			t.Errorf("For(%q) = %v, %v; want %s", addr, tr, err, want)
		}
	}
	if names := len(r.Statuses()); names != 3 {
		t.Fatalf("Statuses = %d", names)
	}
}
