package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// serve runs the node side on one end of a pipe. If accept is true and the
// request verifies, it replies OK; otherwise it closes silently.
func serve(t *testing.T, node *identity.Identity, conn net.Conn, accept func(*Request) bool) <-chan *Request {
	t.Helper()
	got := make(chan *Request, 1)
	go func() {
		defer conn.Close()
		req, err := ReadRequest(conn, node)
		if err != nil || !accept(req) {
			got <- nil
			return
		}
		Accept(conn, nil)
		got <- req
		io.Copy(io.Discard, conn)
	}()
	return got
}

func TestHandshakeOK(t *testing.T) {
	node, _ := identity.Generate()
	dev, _ := identity.Generate()
	c, s := net.Pipe()
	got := serve(t, node, s, func(*Request) bool { return true })
	err := ClientHandshake(c, dev, node.NodeID(), Request{Op: OpConnect, Service: "music"})
	if err != nil {
		t.Fatal(err)
	}
	r := <-got
	if r == nil || r.Service != "music" || !bytes.Equal(r.Key, dev.Public()) {
		t.Fatalf("server saw %+v", r)
	}
	c.Close()
}

func TestHandshakeRejectedIsSilent(t *testing.T) {
	node, _ := identity.Generate()
	dev, _ := identity.Generate()
	c, s := net.Pipe()
	serve(t, node, s, func(*Request) bool { return false })
	err := ClientHandshake(c, dev, node.NodeID(), Request{Op: OpConnect, Service: "music"})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
}

func TestWrongNodeDetected(t *testing.T) {
	node, _ := identity.Generate()
	other, _ := identity.Generate()
	dev, _ := identity.Generate()
	c, s := net.Pipe()
	serve(t, node, s, func(*Request) bool { return true })
	err := ClientHandshake(c, dev, other.NodeID(), Request{Op: OpConnect})
	if !errors.Is(err, ErrWrongNode) {
		t.Fatalf("want ErrWrongNode, got %v", err)
	}
	c.Close()
}

// readHello reads the node's hello from a raw client connection.
func readHello(t *testing.T, c net.Conn) (nodePub, nonce []byte) {
	t.Helper()
	c.Write([]byte(magic))
	h := make([]byte, helloSize)
	if _, err := io.ReadFull(c, h); err != nil {
		t.Fatal(err)
	}
	return h[4:36], h[36:]
}

func sendFrame(c net.Conn, v any) {
	data, _ := json.Marshal(v)
	writeFrame(c, data)
}

func TestTamperedServiceFailsSignature(t *testing.T) {
	node, _ := identity.Generate()
	dev, _ := identity.Generate()
	c, s := net.Pipe()
	errc := make(chan error, 1)
	go func() { _, err := ReadRequest(s, node); errc <- err; s.Close() }()

	pub, nonce := readHello(t, c)
	req := Request{V: Version, Op: OpConnect, Service: "music", PubKey: identity.EncodePublicKey(dev.Public())}
	req.Sig = b64(dev.Sign(signedMessage(pub, nonce, &req)))
	req.Service = "ssh" // attacker swaps the target after signing
	sendFrame(c, req)
	if err := <-errc; !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestReplayedSignatureFails(t *testing.T) {
	node, _ := identity.Generate()
	dev, _ := identity.Generate()

	// First connection: capture a valid signed request.
	c1, s1 := net.Pipe()
	go func() { ReadRequest(s1, node); s1.Close() }()
	pub, nonce := readHello(t, c1)
	req := Request{V: Version, Op: OpConnect, Service: "music", PubKey: identity.EncodePublicKey(dev.Public())}
	req.Sig = b64(dev.Sign(signedMessage(pub, nonce, &req)))
	sendFrame(c1, req)
	c1.Close()

	// Second connection: replay it. The nonce differs, so it must fail.
	c2, s2 := net.Pipe()
	errc := make(chan error, 1)
	go func() { _, err := ReadRequest(s2, node); errc <- err; s2.Close() }()
	readHello(t, c2)
	sendFrame(c2, req)
	if err := <-errc; !errors.Is(err, ErrBadSignature) {
		t.Fatalf("replay: want ErrBadSignature, got %v", err)
	}
}

func TestMalformedInputs(t *testing.T) {
	node, _ := identity.Generate()
	cases := map[string]func(c net.Conn){
		"truncated frame": func(c net.Conn) { c.Write([]byte{0x00, 0x10, 'x'}); c.Close() },
		"zero length":     func(c net.Conn) { c.Write([]byte{0, 0}) },
		"oversize":        func(c net.Conn) { var h [2]byte; binary.BigEndian.PutUint16(h[:], 60000); c.Write(h[:]) },
		"not json":        func(c net.Conn) { writeFrame(c, []byte("nope")) },
		"bad version":     func(c net.Conn) { sendFrame(c, map[string]any{"v": 9, "op": "connect"}) },
		"bad op":          func(c net.Conn) { sendFrame(c, map[string]any{"v": 1, "op": "rm -rf"}) },
		"bad key":         func(c net.Conn) { sendFrame(c, map[string]any{"v": 1, "op": "connect", "pubkey": "AA=="}) },
	}
	for name, send := range cases {
		t.Run(name, func(t *testing.T) {
			c, s := net.Pipe()
			errc := make(chan error, 1)
			go func() { _, err := ReadRequest(s, node); errc <- err; s.Close() }()
			readHello(t, c)
			go send(c)
			select {
			case err := <-errc:
				if err == nil {
					t.Fatal("accepted malformed input")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ReadRequest hung")
			}
			c.Close()
		})
	}
}

func TestNonSolClientGetsNoBytes(t *testing.T) {
	node, _ := identity.Generate()
	c, s := net.Pipe()
	errc := make(chan error, 1)
	go func() { _, err := ReadRequest(s, node); errc <- err; s.Close() }()
	go c.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	n, _ := c.Read(make([]byte, 64))
	if n != 0 {
		t.Fatalf("node sent %d bytes to a non-Sol client", n)
	}
	if err := <-errc; !errors.Is(err, ErrMalformed) {
		t.Fatalf("want ErrMalformed, got %v", err)
	}
}

func TestServiceListAfterAccept(t *testing.T) {
	node, _ := identity.Generate()
	dev, _ := identity.Generate()
	c, s := net.Pipe()
	go func() {
		defer s.Close()
		if _, err := ReadRequest(s, node); err == nil {
			Accept(s, ServiceList{NodeID: node.NodeID(), Services: []ServiceInfo{{Name: "music", Protocol: "tcp"}}})
		}
	}()
	if err := ClientHandshake(c, dev, node.NodeID(), Request{Op: OpList}); err != nil {
		t.Fatal(err)
	}
	sl, err := ReadServiceList(c)
	if err != nil || len(sl.Services) != 1 || sl.Services[0].Name != "music" {
		t.Fatalf("ReadServiceList = %+v, %v", sl, err)
	}
}
