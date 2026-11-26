package mux

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tcpPair returns two ends of a loopback TCP connection.
func tcpPair(t testing.TB) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return a, <-ch
}

func pair(t testing.TB) (*Session, *Session) {
	a, b := tcpPair(t)
	c, s := Client(a), Server(b)
	t.Cleanup(func() { c.Close(); s.Close() })
	return c, s
}

// echo serves every accepted stream by copying it back.
func echo(s *Session) {
	for {
		st, err := s.Accept()
		if err != nil {
			return
		}
		go func() {
			io.Copy(st, st)
			st.CloseWrite()
			st.Close()
		}()
	}
}

func roundTrip(t testing.TB, s *Session, size int) error {
	st, err := s.Open(context.Background())
	if err != nil {
		return err
	}
	defer st.Close()
	data := make([]byte, size)
	rand.Read(data)
	errc := make(chan error, 1)
	go func() {
		_, err := st.Write(data)
		if err == nil {
			err = st.CloseWrite()
		}
		errc <- err
	}()
	got, err := io.ReadAll(st)
	if err != nil {
		return err
	}
	if err := <-errc; err != nil {
		return err
	}
	if !bytes.Equal(got, data) {
		return errors.New("echo mismatch")
	}
	return nil
}

func TestManyStreamsBothDirections(t *testing.T) {
	c, s := pair(t)
	go echo(c)
	go echo(s)
	var wg sync.WaitGroup
	for i := range 40 {
		for _, side := range []*Session{c, s} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := roundTrip(t, side, 1000+i*20000); err != nil {
					t.Error(err, c.Err(), s.Err())
				}
			}()
		}
	}
	wg.Wait()
	// Every stream is fully closed on both sides and forgotten.
	waitFor(t, func() bool { return c.NumStreams() == 0 && s.NumStreams() == 0 })
}

func TestStreamIDParity(t *testing.T) {
	c, s := pair(t)
	for range 3 {
		a, _ := c.Open(context.Background())
		b, _ := s.Open(context.Background())
		if a.ID()%2 != 1 || b.ID()%2 != 0 {
			t.Fatalf("caller opened %d, callee %d", a.ID(), b.ID())
		}
	}
}

func TestFlowControlSlowReader(t *testing.T) {
	c, s := pair(t)
	var accepted = make(chan *Stream, 2)
	go func() {
		for {
			st, err := s.Accept()
			if err != nil {
				return
			}
			accepted <- st
		}
	}()
	st, _ := c.Open(context.Background())
	var written atomic.Int64
	data := make([]byte, 4*InitialWindow)
	rand.Read(data)
	go func() {
		for i := 0; i < len(data); i += 4096 {
			n, err := st.Write(data[i : i+4096])
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
		st.CloseWrite()
	}()
	slow := <-accepted
	time.Sleep(200 * time.Millisecond)
	if w := written.Load(); w > InitialWindow {
		t.Fatalf("writer got %d bytes ahead of a reader that reads nothing (window %d)", w, InitialWindow)
	}

	// A stalled stream does not hold up the others.
	go func() {
		other := <-accepted
		io.Copy(other, other)
		other.Close()
	}()
	st2, _ := c.Open(context.Background())
	st2.Write([]byte("ping"))
	st2.CloseWrite()
	if got, _ := io.ReadAll(st2); string(got) != "ping" {
		t.Fatalf("second stream got %q", got)
	}

	var got bytes.Buffer
	buf := make([]byte, 1000)
	for {
		n, err := slow.Read(buf)
		got.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatal("slow reader got different bytes")
	}
}

func TestHalfClose(t *testing.T) {
	c, s := pair(t)
	done := make(chan error, 1)
	go func() {
		st, err := s.Accept()
		if err != nil {
			done <- err
			return
		}
		got, err := io.ReadAll(st)
		if err != nil || string(got) != "hello" {
			done <- errors.New("server read " + string(got))
			return
		}
		st.Write([]byte("world"))
		st.Close()
		done <- nil
	}()
	st, _ := c.Open(context.Background())
	st.Write([]byte("hello"))
	st.CloseWrite()
	if _, err := st.Write([]byte("x")); err == nil {
		t.Fatal("write after CloseWrite succeeded")
	}
	got, err := io.ReadAll(st)
	if err != nil || string(got) != "world" {
		t.Fatalf("client read %q, %v", got, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReset(t *testing.T) {
	c, s := pair(t)
	got := make(chan *Stream, 1)
	go func() { st, _ := s.Accept(); got <- st }()
	st, _ := c.Open(context.Background())
	st.Write([]byte("x"))
	remote := <-got
	remote.Reset()
	if _, err := st.Read(make([]byte, 1)); !errors.Is(err, ErrReset) {
		t.Fatalf("read after reset: %v", err)
	}
	if _, err := st.Write([]byte("x")); !errors.Is(err, ErrReset) {
		t.Fatalf("write after reset: %v", err)
	}
	waitFor(t, func() bool { return c.NumStreams() == 0 && s.NumStreams() == 0 })
	// The session survives a reset stream.
	go echo(s)
	if err := roundTrip(t, c, 10); err != nil {
		t.Fatal(err)
	}
}

func TestSessionDeathClosesStreams(t *testing.T) {
	a, b := tcpPair(t)
	c, s := Client(a), Server(b)
	defer s.Close()
	var streams []*Stream
	for range 5 {
		st, _ := c.Open(context.Background())
		streams = append(streams, st)
	}
	errs := make(chan error, len(streams))
	for _, st := range streams {
		go func() { _, err := st.Read(make([]byte, 1)); errs <- err }()
	}
	b.Close() // the peer's connection dies
	<-s.Done()
	for range streams {
		select {
		case err := <-errs:
			if !errors.Is(err, ErrSessionClosed) {
				t.Fatalf("stream read: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stream outlived its session")
		}
	}
	<-c.Done()
	if c.Err() == nil {
		t.Fatal("dead session has no error")
	}
	if _, err := c.Open(context.Background()); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("open on dead session: %v", err)
	}
	if _, err := s.Accept(); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("accept on dead session: %v", err)
	}
}

func TestDeadlines(t *testing.T) {
	c, s := pair(t)
	go func() { s.Accept() }()
	st, _ := c.Open(context.Background())
	st.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	_, err := st.Read(make([]byte, 1))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read past deadline: %v", err)
	}
	st.SetReadDeadline(time.Time{})
	st.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	// Nobody reads: the window fills, then the deadline fires.
	_, err = st.Write(make([]byte, 2*InitialWindow))
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("write past deadline: %v", err)
	}
}

func TestKeepalive(t *testing.T) {
	oldI := PingInterval
	PingInterval = 20 * time.Millisecond
	defer func() { PingInterval = oldI }()

	// A healthy pair answers pings and stays up.
	c, s := pair(t)
	time.Sleep(10 * PingInterval)
	if c.Err() != nil || s.Err() != nil {
		t.Fatalf("healthy session died: %v / %v", c.Err(), s.Err())
	}

	// A peer that reads but never answers is cut off after the missed pongs.
	a, b := tcpPair(t)
	defer b.Close()
	go io.Copy(io.Discard, b)
	dead := Client(a)
	select {
	case <-dead.Done():
		if !errors.Is(dead.Err(), ErrKeepalive) {
			t.Fatalf("closed for %v", dead.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session to a silent peer stayed up")
	}
}

func TestProtocolViolations(t *testing.T) {
	cases := map[string][]byte{
		"data on unopened stream": frame(3, typeData, []byte("x")),
		"open with wrong parity":  frame(2, typeOpen, nil),
		"unknown type":            frame(0, 9, nil),
		"ping with bad length":    frame(0, typePing, []byte{1}),
		"stream frame on 0":       frame(0, typeData, nil),
		"window overrun": append(frame(1, typeOpen, nil),
			bytes.Repeat(frame(1, typeData, make([]byte, MaxPayload)), InitialWindow/MaxPayload+1)...),
		"reopen": append(frame(3, typeOpen, nil), frame(1, typeOpen, nil)...),
		"double close": append(frame(1, typeOpen, nil),
			append(frame(1, typeClose, nil), frame(1, typeClose, nil)...)...),
	}
	for name, raw := range cases {
		a, b := tcpPair(t)
		srv := Server(b)
		go io.Copy(io.Discard, a)
		a.Write(raw)
		select {
		case <-srv.Done():
			if !errors.Is(srv.Err(), ErrProtocol) {
				t.Errorf("%s: closed for %v", name, srv.Err())
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: session survived", name)
		}
		a.Close()
	}
}

func TestHTTPStyleListener(t *testing.T) {
	c, s := pair(t)
	ln := s.Listener()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(conn, conn)
		conn.Close()
	}()
	if err := roundTrip(t, c, 5); err != nil {
		t.Fatal(err)
	}
	ln.Close()
	<-s.Done()
}

func waitFor(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fuzzConn feeds fixed input to a session and discards its output.
type fuzzConn struct {
	r *bytes.Reader
	net.Conn
}

func (f fuzzConn) Read(p []byte) (int, error)  { return f.r.Read(p) }
func (f fuzzConn) Write(p []byte) (int, error) { return len(p), nil }
func (f fuzzConn) Close() error                { return nil }
func (f fuzzConn) LocalAddr() net.Addr         { return &net.TCPAddr{} }
func (f fuzzConn) RemoteAddr() net.Addr        { return &net.TCPAddr{} }

func FuzzReadFrame(f *testing.F) {
	f.Add(frame(1, typeData, []byte("hello")))
	f.Add([]byte{0, 0, 0, 0, typePing, 0, 8, 1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		h, p, err := readFrame(bytes.NewReader(b), make([]byte, headerSize))
		if err != nil {
			return
		}
		if int(h.n) != len(p) || len(b) < headerSize+len(p) {
			t.Fatalf("header says %d, payload %d", h.n, len(p))
		}
		if !bytes.Equal(frame(h.id, h.typ, p), b[:headerSize+len(p)]) {
			t.Fatal("frame does not re-encode to its input")
		}
	})
}

// FuzzSession feeds arbitrary bytes to a session: it must never panic and
// must end when the input does.
func FuzzSession(f *testing.F) {
	open := frame(1, typeOpen, nil)
	f.Add(open)
	f.Add(append(open, frame(1, typeData, []byte("abc"))...))
	f.Add(append(append(open, frame(1, typeClose, nil)...), frame(1, typeReset, nil)...))
	f.Add(append(open, frame(1, typeWindow, binary.BigEndian.AppendUint32(nil, 100))...))
	f.Add(frame(0, typePing, make([]byte, 8)))
	f.Add(frame(0, typePong, make([]byte, 8)))
	f.Fuzz(func(t *testing.T, b []byte) {
		s := Server(fuzzConn{r: bytes.NewReader(b)})
		go func() {
			for {
				st, err := s.Accept()
				if err != nil {
					return
				}
				go func() { io.Copy(io.Discard, st); st.Write([]byte("x")); st.Close() }()
			}
		}()
		select {
		case <-s.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("session did not end with its input")
		}
	})
}

// TestWriteWhileBlockedRead is a regression test: a writer waiting for
// window must wake on a grant even while another goroutine is blocked in
// Read on the same stream (as net/http's server does while a handler
// streams a large response). A shared wake-up channel let the reader
// swallow the grant and deadlocked the writer after one window.
func TestWriteWhileBlockedRead(t *testing.T) {
	c, s := pair(t)
	accepted := make(chan *Stream, 1)
	go func() {
		st, err := s.Accept()
		if err == nil {
			accepted <- st
		}
	}()
	cst, _ := c.Open(context.Background())
	cst.Write([]byte{1})
	sst := <-accepted
	var one [1]byte
	sst.Read(one[:])
	go sst.Read(one[:]) // blocks: the client sends nothing more
	data := make([]byte, 4*InitialWindow)
	done := make(chan error, 1)
	go func() {
		_, err := sst.Write(data)
		done <- err
	}()
	cst.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(io.LimitReader(cst, int64(len(data))))
	if err != nil || len(got) != len(data) {
		t.Fatalf("read %d bytes: %v", len(got), err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writer stuck")
	}
}

// TestIdleAndCloseIfIdle: Idle signals when the last stream goes away, CloseIfIdle refuses
// while a stream is open and closes once none is, and Opened counts this side's opens.
func TestIdleAndCloseIfIdle(t *testing.T) {
	c, s := pair(t)
	go echo(s)
	if c.Opened() != 0 || s.Opened() != 0 {
		t.Fatalf("opened before any stream: %d %d", c.Opened(), s.Opened())
	}
	st, err := c.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.Opened() != 1 {
		t.Fatalf("opened: %d", c.Opened())
	}
	if c.CloseIfIdle() {
		t.Fatal("closed with a stream open")
	}
	if _, err := st.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	st.CloseWrite()
	io.ReadAll(st)
	st.Close()
	select {
	case <-c.Idle():
	case <-time.After(5 * time.Second):
		t.Fatal("no idle signal after the last stream ended")
	}
	waitFor(t, func() bool { return c.NumStreams() == 0 })
	if !c.CloseIfIdle() {
		t.Fatal("an idle session did not close")
	}
	if c.Err() == nil {
		t.Fatal("still alive after CloseIfIdle")
	}
	if _, err := c.Open(context.Background()); err == nil {
		t.Fatal("opened a stream on a closed session")
	}
	<-s.Done() // the peer sees the connection go
	if s.Opened() != 0 {
		t.Fatalf("callee opened: %d", s.Opened())
	}
}
