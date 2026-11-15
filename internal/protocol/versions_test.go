package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// countConn records every byte written through it.
type countConn struct {
	net.Conn
	mu      sync.Mutex
	written bytes.Buffer
}

func (c *countConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.mu.Lock()
	c.written.Write(b[:n])
	c.mu.Unlock()
	return n, err
}

func (c *countConn) sent() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.written.Bytes())
}

func TestNegotiate(t *testing.T) {
	for _, tc := range []struct {
		a, b Versions
		want uint16
		ok   bool
	}{
		{Versions{1, 1}, Versions{1, 1}, 1, true},
		{Versions{1, 3}, Versions{2, 5}, 3, true},
		{Versions{2, 5}, Versions{1, 3}, 3, true},
		{Versions{1, 2}, Versions{1, 1}, 1, true},
		{Versions{1, 1}, Versions{2, 2}, 0, false},
		{Versions{3, 4}, Versions{1, 2}, 0, false},
		{Versions{0, 1}, Versions{1, 1}, 0, false}, // invalid range
		{Versions{2, 1}, Versions{1, 2}, 0, false}, // min > max
	} {
		got, ok := Negotiate(tc.a, tc.b)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Negotiate(%v, %v) = %d, %v; want %d, %v", tc.a, tc.b, got, ok, tc.want, tc.ok)
		}
	}
}

// handshake runs both sides with the given ranges and returns what each
// side got, plus every byte the callee wrote.
func handshake(t *testing.T, caller, callee Versions) (req *Request, sent []byte, srvErr, cliErr error) {
	t.Helper()
	node, _ := identity.Generate()
	dev, _ := identity.Generate()
	c, s := net.Pipe()
	cs := &countConn{Conn: s}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer s.Close()
		req, srvErr = callee.ReadRequest(cs, node)
		if srvErr == nil {
			Accept(cs, nil)
		}
	}()
	cliErr = caller.ClientHandshake(c, dev, node.NodeID(), Request{Op: OpConnect, Service: "music"})
	c.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("callee hung")
	}
	return req, cs.sent(), srvErr, cliErr
}

func TestVersionsWithinRange(t *testing.T) {
	for _, tc := range []struct {
		name           string
		caller, callee Versions
		want           int
	}{
		{"same", Versions{1, 1}, Versions{1, 1}, 1},
		{"caller newer", Versions{1, 2}, Versions{1, 1}, 1},
		{"callee newer", Versions{1, 1}, Versions{1, 2}, 1},
		{"both newer", Versions{1, 2}, Versions{1, 3}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _, srvErr, cliErr := handshake(t, tc.caller, tc.callee)
			if srvErr != nil || cliErr != nil {
				t.Fatalf("callee %v, caller %v", srvErr, cliErr)
			}
			if req.V != tc.want {
				t.Fatalf("negotiated %d, want %d", req.V, tc.want)
			}
		})
	}
}

func TestVersionsOutOfRange(t *testing.T) {
	for _, tc := range []struct {
		name           string
		caller, callee Versions
		calleeOlder    bool
	}{
		{"caller newer", Versions{2, 3}, Versions{1, 1}, true},
		{"callee newer", Versions{1, 1}, Versions{2, 4}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, sent, srvErr, cliErr := handshake(t, tc.caller, tc.callee)
			var ie *IncompatibleError
			if !errors.As(cliErr, &ie) || !errors.Is(cliErr, ErrIncompatible) {
				t.Fatalf("caller: want IncompatibleError, got %v", cliErr)
			}
			if ie.Remote != tc.callee || ie.Local != tc.caller || ie.PeerOlder() != tc.calleeOlder {
				t.Fatalf("caller error %+v (peer older %v)", ie, ie.PeerOlder())
			}
			if !errors.As(srvErr, &ie) || ie.Remote != tc.caller || req != nil {
				t.Fatalf("callee: want IncompatibleError, got %v", srvErr)
			}
			// The callee sent the fixed hello prefix (what every Sol caller
			// gets first) and nothing else: no key, no nonce, no reply.
			if !bytes.Equal(sent, prefix(tc.callee)) {
				t.Fatalf("callee wrote %x, want only the prefix %x", sent, prefix(tc.callee))
			}
		})
	}
}

// A man in the middle rewrites a hello so the two sides settle on an older
// version than both speak, and fixes up the request's version. Both ranges
// and the version are signed, so the signature breaks.
func TestVersionDowngradeFailsSignature(t *testing.T) {
	both := Versions{1, 2}
	for _, tc := range []struct {
		name  string
		hello Versions   // the ClientHello the callee sees
		nv    negotiated // what the honest caller signed
		v     int        // the request's version as the callee sees it
	}{
		// The callee's hello was rewritten to 1-1: the caller signs v1.
		{"callee hello", both, negotiated{1, both, Versions{1, 1}}, 2},
		// The caller's hello was rewritten to 1-1: the callee settles on 1,
		// the caller (seeing 1-2) signs v2; the request is rewritten to 1.
		{"caller hello", Versions{1, 1}, negotiated{2, both, both}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, _ := identity.Generate()
			dev, _ := identity.Generate()
			c, s := net.Pipe()
			errc := make(chan error, 1)
			go func() { _, err := both.ReadRequest(s, node); errc <- err; s.Close() }()
			c.Write(prefix(tc.hello))
			h := make([]byte, helloSize)
			if _, err := io.ReadFull(c, h); err != nil {
				t.Fatal(err)
			}
			pub, nonce := h[prefixSize:prefixSize+32], h[prefixSize+32:]
			req := Request{V: tc.v, Op: OpConnect, Service: "music", PubKey: identity.EncodePublicKey(dev.Public())}
			msg, _ := signedMessage(pub, nonce, tc.nv, &req)
			req.Sig = b64(dev.Sign(msg))
			sendFrame(c, req)
			if err := <-errc; !errors.Is(err, ErrBadSignature) {
				t.Fatalf("want ErrBadSignature, got %v", err)
			}
		})
	}
}

// Garbage in the ClientHello's range gets nothing back at all; garbage in
// the request's version gets nothing after the hello.
func TestGarbageVersionFields(t *testing.T) {
	node, _ := identity.Generate()
	raw := func(b ...byte) []byte { return append([]byte(magic), b...) }
	for name, hello := range map[string][]byte{
		"min zero":     raw(0, 0, 0, 1),
		"both zero":    raw(0, 0, 0, 0),
		"min over max": raw(0, 3, 0, 2),
		"old magic":    []byte("SOL1\x00\x01\x00\x01"),
		"wrong magic":  []byte("SOLX\x00\x01\x00\x01"),
	} {
		t.Run("hello/"+name, func(t *testing.T) {
			c, s := net.Pipe()
			cs := &countConn{Conn: s}
			errc := make(chan error, 1)
			go func() { _, err := ReadRequest(cs, node); errc <- err; s.Close() }()
			go c.Write(hello)
			io.Copy(io.Discard, c)
			if err := <-errc; !errors.Is(err, ErrMalformed) {
				t.Fatalf("want ErrMalformed, got %v", err)
			}
			if n := len(cs.sent()); n != 0 {
				t.Fatalf("callee wrote %d bytes", n)
			}
		})
	}
	for name, v := range map[string]any{
		"string": "1", "negative": -1, "huge": 1 << 40, "null": nil, "float": 1.5,
		"array": []int{1}, "next version": 2, "zero": 0, "uint16 wrap": 65537,
	} {
		t.Run("request/"+name, func(t *testing.T) {
			dev, _ := identity.Generate()
			c, s := net.Pipe()
			cs := &countConn{Conn: s}
			errc := make(chan error, 1)
			go func() { _, err := ReadRequest(cs, node); errc <- err; s.Close() }()
			pub, nonce := readHello(t, c)
			req := Request{V: Version, Op: OpConnect, Service: "music", PubKey: identity.EncodePublicKey(dev.Public())}
			req.Sig = sign(t, dev, pub, nonce, &req)
			m := map[string]any{}
			b, _ := json.Marshal(req)
			json.Unmarshal(b, &m)
			m["v"] = v
			go func() { sendFrame(c, m); io.Copy(io.Discard, c) }()
			if err := <-errc; !errors.Is(err, ErrMalformed) {
				t.Fatalf("want ErrMalformed, got %v", err)
			}
			if n := len(cs.sent()); n != helloSize {
				t.Fatalf("callee wrote %d bytes after the hello", n-helloSize)
			}
		})
	}
}

// A callee hello with a garbage range is malformed to the caller.
func TestCallerRejectsGarbageHello(t *testing.T) {
	dev, _ := identity.Generate()
	for name, hello := range map[string][]byte{
		"min zero":     append([]byte(magic), 0, 0, 0, 1),
		"min over max": append([]byte(magic), 0, 2, 0, 1),
		"wrong magic":  []byte("HTTP/1.1"),
	} {
		t.Run(name, func(t *testing.T) {
			c, s := net.Pipe()
			go func() { io.ReadFull(s, make([]byte, prefixSize)); s.Write(hello); s.Close() }()
			if err := ClientHandshake(c, dev, "", Request{Op: OpList}); !errors.Is(err, ErrMalformed) {
				t.Fatalf("want ErrMalformed, got %v", err)
			}
		})
	}
}

func TestIncompatibleErrorText(t *testing.T) {
	older := &IncompatibleError{Local: Versions{2, 3}, Remote: Versions{1, 1}}
	newer := &IncompatibleError{Local: Versions{1, 1}, Remote: Versions{2, 3}}
	if !older.PeerOlder() || newer.PeerOlder() {
		t.Fatal("PeerOlder")
	}
	if !bytes.Contains([]byte(older.Error()), []byte("update Sol there")) ||
		!bytes.Contains([]byte(newer.Error()), []byte("update Sol here")) {
		t.Fatalf("texts: %q / %q", older, newer)
	}
}
