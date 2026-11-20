package appapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/sdk/go/sol"
)

// dialTimeout bounds setting up a stream (not the stream itself).
const dialTimeout = 30 * time.Second

// bufConn returns bytes buffered during a handshake before reading the connection.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

// Read drains the handshake buffer first.
func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// upgrade sends an "Upgrade: sol-stream" request over conn and returns the stream once the
// other side answered 101. Any other answer becomes an API error.
func upgrade(conn net.Conn, req *http.Request) (net.Conn, error) {
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "sol-stream")
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, sol.Errorf("unavailable", "stream setup failed")
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, sol.Errorf("unavailable", "stream setup failed")
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer conn.Close()
		defer resp.Body.Close()
		_, err := relay(resp)
		if err == nil {
			err = sol.Errorf("internal", "the app did not accept the stream")
		}
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufConn{Conn: conn, r: br}, nil
}

// dialApp opens a stream to a local app at path, as who.
func (s *Server) dialApp(ctx context.Context, appID, path string, who caller) (net.Conn, error) {
	a, ok := s.cfg.Apps.App(appID)
	if !ok {
		return nil, sol.Errorf("not_found", "app does not accept streams")
	}
	if a.Addr == "" {
		return nil, sol.Errorf("unavailable", "app "+appID+" is not running")
	}
	network, addr := dialer(a.Addr)
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, sol.Errorf("unavailable", "app "+appID+" is not answering")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://app"+path, nil)
	who.set(req.Header)
	return upgrade(conn, req)
}

// dial serves POST /v1/dial: the connection becomes a raw stream to the target app.
func (s *Server) dial(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "sol-stream") {
		return nil, sol.Errorf("bad_request", "dial needs Upgrade: sol-stream")
	}
	var b sol.DialRequest
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if !feeds.ValidApp(b.App) || !strings.HasPrefix(b.Path, "/") {
		return nil, sol.Errorf("bad_request", "bad app or path")
	}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	defer cancel()
	var stream net.Conn
	var err error
	me, dev := s.me()
	if b.Person == me && (b.Device == "" || b.Device == dev) {
		stream, err = s.dialApp(ctx, b.App, b.Path, s.localCaller())
	} else {
		stream, err = s.dialPeer(ctx, b)
	}
	if err != nil {
		return nil, err
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		stream.Close()
		return nil, done
	}
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: sol-stream\r\n\r\n")); err != nil {
		conn.Close()
		stream.Close()
		return nil, done
	}
	_ = conn.SetDeadline(time.Time{})
	var c net.Conn = conn
	if rw.Reader.Buffered() > 0 {
		c = &bufConn{Conn: conn, r: rw.Reader}
	}
	pipe(c, stream)
	return nil, done
}

// dialPeer opens a stream to a person's app over a session: POST /p/dial.
func (s *Server) dialPeer(ctx context.Context, b sol.DialRequest) (net.Conn, error) {
	ss, err := s.sessionTo(b.Person, b.Device)
	if err != nil {
		return nil, err
	}
	conn, err := ss.Open(ctx)
	if err != nil {
		return nil, sol.Errorf("unavailable", "person is unreachable")
	}
	body, _ := json.Marshal(peerDialBody{App: b.App, Path: b.Path})
	req, _ := http.NewRequest(http.MethodPost, "http://peer/p/dial", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	return upgrade(conn, req)
}

type peerDialBody struct {
	App  string `json:"app"`
	Path string `json:"path"`
}

// peerDial serves POST /p/dial to a verified session peer: it opens the stream to my app, with
// the peer as the caller, and splices the two.
func (s *Server) peerDial(w http.ResponseWriter, r *http.Request) {
	p, ok := session.PeerFrom(r.Context())
	if !ok {
		sol.WriteError(w, sol.Errorf("forbidden", "not a session peer"))
		return
	}
	var b peerDialBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&b); err != nil || !feeds.ValidApp(b.App) || !strings.HasPrefix(b.Path, "/") {
		sol.WriteError(w, sol.Errorf("bad_request", "bad dial"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	stream, err := s.dialApp(ctx, b.App, b.Path, peerCaller(p))
	cancel()
	if err != nil {
		sol.WriteError(w, err)
		return
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		stream.Close()
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: sol-stream\r\n\r\n")); err != nil {
		conn.Close()
		stream.Close()
		return
	}
	var c net.Conn = conn
	if rw.Reader.Buffered() > 0 {
		c = &bufConn{Conn: conn, r: rw.Reader}
	}
	pipe(c, stream)
}

// pipe copies both ways until either side ends, then closes both.
func pipe(a, b net.Conn) {
	var once sync.Once
	end := func() { once.Do(func() { a.Close(); b.Close() }) }
	go func() {
		_, _ = io.Copy(a, b)
		end()
	}()
	_, _ = io.Copy(b, a)
	end()
}
