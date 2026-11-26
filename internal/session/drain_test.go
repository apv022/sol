package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// testHandler serves the path tests: POST /echo streams the request body back as it
// arrives (full duplex, like a port-sharing tunnel); anything else is whoami.
func testHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/echo" {
			whoami(w, r)
			return
		}
		rc := http.NewResponseController(w)
		_ = rc.EnableFullDuplex()
		w.WriteHeader(http.StatusOK)
		_ = rc.Flush()
		buf := make([]byte, 8<<10)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				_ = rc.Flush()
			}
			if err != nil {
				return
			}
		}
	})
}

// echoStream is one /echo exchange in progress over a session.
type echoStream struct {
	in   *io.PipeWriter
	resp *http.Response
}

func openEcho(t *testing.T, s *Session) *echoStream {
	t.Helper()
	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, "http://peer/echo", pr)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return &echoStream{in: pw, resp: resp}
}

// round sends n random bytes and checks they come back intact.
func (e *echoStream) round(t *testing.T, n int) {
	t.Helper()
	msg := make([]byte, n)
	rand.Read(msg)
	errc := make(chan error, 1)
	go func() { _, err := e.in.Write(msg); errc <- err }()
	got := make([]byte, n)
	if _, err := io.ReadFull(e.resp.Body, got); err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("echo write: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("echo came back corrupted")
	}
}

func (e *echoStream) close() {
	e.in.Close()
	io.Copy(io.Discard, e.resp.Body)
	e.resp.Body.Close()
}

// movePath moves the session between a and b to the other one of its two paths, from the
// side that dialed it, the way the keeper does after a successful probe (dialPath), and
// returns the old and the new session.
func movePath(t *testing.T, a, b *pathNode) (old, moved *Session) {
	t.Helper()
	old, ok := dialedSession(a, b)
	if !ok {
		t.Fatal("no session")
	}
	caller, callee := a, b
	if bytes.Equal(old.Caller(), b.kr.DevicePublic()) {
		caller, callee = b, a
	}
	to := identity.Endpoint{Transport: "tor", Address: callee.tor.Addr()}
	if old.Path().Transport == "tor" {
		to = identity.Endpoint{Transport: "direct", Address: callee.wg.Addr()}
	}
	caller.m.dialPath(context.Background(), old, to, to.Transport+" is better")
	moved, ok = caller.m.Session(callee.kr.DevicePublic())
	if !ok || moved == old || moved.Path().Transport != to.Transport {
		t.Fatalf("the session did not move to %s", to.Transport)
	}
	return old, moved
}

// TestPathMoveKeepsStreams: a stream that is open when its session moves to another path
// finishes intact on the old session; requests started afterwards go over the new one, on
// both sides, even through a handle to the old one; the old session closes once its last
// stream ends.
func TestPathMoveKeepsStreams(t *testing.T) {
	alice, bob := newPathNode(t, "alice"), newPathNode(t, "bob")
	pairPathNodes(t, alice, bob)
	ad, bd := alice.kr.DevicePublic(), bob.kr.DevicePublic()
	s, _ := dialedSession(alice, bob)
	if s.Path().Transport != "direct" {
		t.Fatalf("first path: %+v", s.Path())
	}
	oldA, _ := alice.m.Session(bd)
	oldB, _ := bob.m.Session(ad)
	e := openEcho(t, s)
	e.round(t, 300<<10) // more than one mux window
	movePath(t, alice, bob)
	e.round(t, 300<<10) // still flowing on the old session
	newA, okA := alice.m.Session(bd)
	newB, okB := bob.m.Session(ad)
	if !okA || !okB || newA == oldA || newB == oldB || newA.Path().Transport == "direct" && newB.Path().Transport == "direct" {
		t.Fatal("the move did not reach both sides")
	}
	for _, old := range []*Session{oldA, oldB} {
		select {
		case <-old.Retired():
		default:
			t.Fatal("a replaced session is not retired")
		}
		if !old.Alive() {
			t.Fatal("a replaced session closed under its open stream")
		}
		// New work goes to the current session, even through the old handle.
		cur, _ := old.m.Session(old.peer.Device)
		o, c := old.mux.Opened(), cur.mux.Opened()
		if got := get(t, old, "/whoami"); got == "" {
			t.Fatal("a request through the old handle failed")
		}
		if old.mux.Opened() != o || cur.mux.Opened() != c+1 {
			t.Fatal("a request through the old handle did not go over the new session")
		}
	}
	e.round(t, 64<<10)
	e.close()
	eventually(t, "the drained sessions closed", func() bool { return !oldA.Alive() && !oldB.Alive() })
	if !newA.Alive() || !newB.Alive() {
		t.Fatal("the new session died")
	}
	if got := get(t, newA, "/whoami"); got == "" {
		t.Fatal("the moved session does not serve")
	}
}

// TestDrainGrace: a stream that never ends keeps a replaced session only for DrainGrace.
func TestDrainGrace(t *testing.T) {
	defer func(g time.Duration) { DrainGrace = g }(DrainGrace)
	DrainGrace = 500 * time.Millisecond
	alice, bob := newPathNode(t, "alice"), newPathNode(t, "bob")
	pairPathNodes(t, alice, bob)
	s, _ := dialedSession(alice, bob)
	e := openEcho(t, s)
	defer e.close()
	e.round(t, 1<<10)
	old, _ := movePath(t, alice, bob)
	t0 := time.Now()
	time.Sleep(DrainGrace / 2)
	if !old.Alive() {
		t.Fatal("closed before the grace")
	}
	eventually(t, "the grace to end the old session", func() bool { return !old.Alive() })
	if d := time.Since(t0); d > DrainGrace+2*time.Second {
		t.Fatalf("closed after %v", d)
	}
}

// TestRetiredWithoutStreamsCloses: a replaced session with no stream closes after DrainIdle
// (the time a peer may take to learn of the new session), not after the grace.
func TestRetiredWithoutStreamsCloses(t *testing.T) {
	alice, bob := newPathNode(t, "alice"), newPathNode(t, "bob")
	pairPathNodes(t, alice, bob)
	old, _ := movePath(t, alice, bob)
	t0 := time.Now()
	eventually(t, "the idle old session to close", func() bool { return !old.Alive() })
	if d := time.Since(t0); d > DrainIdle+2*time.Second {
		t.Fatalf("closed after %v", d)
	}
}

// TestCloseEndsDraining: closing the current session (a removal, Forget) also ends the
// sessions it replaced, streams and all.
func TestCloseEndsDraining(t *testing.T) {
	alice, bob := newPathNode(t, "alice"), newPathNode(t, "bob")
	pairPathNodes(t, alice, bob)
	s, _ := dialedSession(alice, bob)
	e := openEcho(t, s)
	defer e.close()
	e.round(t, 1<<10)
	old, _ := movePath(t, alice, bob)
	alice.m.Forget(bob.kr.DevicePublic())
	bob.m.Forget(alice.kr.DevicePublic())
	eventually(t, "the draining session to close", func() bool { return !old.Alive() })
}
