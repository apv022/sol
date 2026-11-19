// Package mux is Sol's stream multiplexer (design/PROTOCOL.md §Sessions):
// many independent, flow-controlled, bidirectional streams over one
// authenticated connection, opened by either side.
//
// Frame: u32 stream_id | u8 type | u16 len | payload, big-endian.
//
// Invariants:
//
//   - Stream IDs opened by the caller (the side that dialed) are odd, by the
//     callee even; 0 is the session itself (ping, pong). Each side opens IDs
//     in increasing order, so an ID is never reused within a session.
//   - A sender never has more than the receiver's window in flight on a
//     stream (InitialWindow, then whatever window frames grant). A peer that
//     overruns a window, or sends any frame that breaks the rules, kills the
//     session: it is buggy or hostile, and a session is cheap to redial.
//   - The read loop never blocks on a write. Control frames (window, pong,
//     close, reset) go through an internal queue the writer goroutine
//     drains first, so two sessions writing to each other cannot deadlock.
//   - A ping goes out every PingInterval; MaxMissedPongs unanswered pings in
//     a row close the session, and every stream with it.
package mux

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Frame types.
const (
	typeOpen   byte = 0
	typeData   byte = 1
	typeWindow byte = 2
	typeClose  byte = 3
	typeReset  byte = 4
	typePing   byte = 5
	typePong   byte = 6
)

const (
	headerSize = 7
	// InitialWindow is each stream's receive window at open, in bytes.
	InitialWindow = 256 << 10
	// MaxPayload is the largest frame payload the format allows.
	MaxPayload = 1<<16 - 1
	// maxCtrlQueue bounds queued control frames; a peer that makes us queue
	// more (pings it never reads pongs for) is cut off.
	maxCtrlQueue = 1 << 14
)

// Tunables. Package variables so tests can shorten them; set them before
// creating sessions.
var (
	// PingInterval is how often a session pings its peer.
	PingInterval = 20 * time.Second
	// MaxMissedPongs is how many pings in a row may go unanswered before
	// the session is closed.
	MaxMissedPongs = 3
	// AcceptBacklog is how many opened streams may wait for Accept; more
	// are reset.
	AcceptBacklog = 256
	// ChunkSize is the largest data frame a stream writes, so one large
	// write doesn't hold up the other streams for long.
	ChunkSize = 16 << 10
)

// Errors.
var (
	// ErrSessionClosed is returned by operations on a closed session and
	// by its streams once it dies.
	ErrSessionClosed = errors.New("mux: session closed")
	// ErrReset is returned by a stream that either side reset.
	ErrReset = errors.New("mux: stream reset")
	// ErrProtocol is the cause of a session killed for a malformed frame.
	ErrProtocol = errors.New("mux: protocol violation")
	// ErrKeepalive is the cause of a session whose peer stopped answering
	// pings.
	ErrKeepalive = errors.New("mux: peer stopped answering pings")
)

type header struct {
	id  uint32
	typ byte
	n   uint16
}

func parseHeader(b []byte) header {
	return header{binary.BigEndian.Uint32(b), b[4], binary.BigEndian.Uint16(b[5:])}
}

func frame(id uint32, typ byte, payload []byte) []byte {
	b := make([]byte, headerSize, headerSize+len(payload))
	binary.BigEndian.PutUint32(b, id)
	b[4] = typ
	binary.BigEndian.PutUint16(b[5:], uint16(len(payload)))
	return append(b, payload...)
}

// readFrame reads one frame. The payload is a fresh slice.
func readFrame(r io.Reader, hdr []byte) (header, []byte, error) {
	if _, err := io.ReadFull(r, hdr[:headerSize]); err != nil {
		return header{}, nil, err
	}
	h := parseHeader(hdr)
	if h.n == 0 {
		return h, nil, nil
	}
	p := make([]byte, h.n)
	if _, err := io.ReadFull(r, p); err != nil {
		return header{}, nil, err
	}
	return h, p, nil
}

type writeReq struct {
	frame []byte
	done  chan error
}

// Session is one multiplexed connection.
type Session struct {
	conn   net.Conn
	caller bool

	mu         sync.Mutex
	streams    map[uint32]*Stream
	nextID     uint32 // next ID this side opens
	lastRemote uint32 // highest ID the peer opened
	missed     int    // pings sent since the last pong

	pingEvery time.Duration // PingInterval and MaxMissedPongs at creation
	maxMissed int

	accept chan *Stream

	ctrlMu  sync.Mutex
	ctrlQ   [][]byte
	ctrlSig chan struct{}
	dataCh  chan writeReq

	done      chan struct{}
	closeOnce sync.Once
	err       error // set before done closes
}

// Client starts a session on conn as the caller (odd stream IDs).
func Client(conn net.Conn) *Session { return newSession(conn, true) }

// Server starts a session on conn as the callee (even stream IDs).
func Server(conn net.Conn) *Session { return newSession(conn, false) }

func newSession(conn net.Conn, caller bool) *Session {
	s := &Session{
		conn:    conn,
		caller:  caller,
		streams: map[uint32]*Stream{},
		nextID:  2,
		accept:  make(chan *Stream, AcceptBacklog),
		ctrlSig: make(chan struct{}, 1),
		dataCh:  make(chan writeReq),
		done:    make(chan struct{}),

		pingEvery: PingInterval,
		maxMissed: MaxMissedPongs,
	}
	if caller {
		s.nextID = 1
	}
	go s.readLoop()
	go s.writeLoop()
	go s.keepalive()
	return s
}

// Done is closed when the session dies.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns why the session died (nil while it lives; ErrSessionClosed
// after a local Close).
func (s *Session) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// Close closes the session and every stream on it.
func (s *Session) Close() error {
	s.shutdown(ErrSessionClosed)
	return nil
}

// LocalAddr and RemoteAddr are those of the underlying connection.
func (s *Session) LocalAddr() net.Addr { return s.conn.LocalAddr() }

// RemoteAddr returns the underlying connection's remote address.
func (s *Session) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }

// NumStreams returns the number of streams still tracked (not fully closed).
func (s *Session) NumStreams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

func (s *Session) shutdown(err error) {
	s.closeOnce.Do(func() {
		s.err = err
		close(s.done)
		s.conn.Close()
		s.mu.Lock()
		for _, st := range s.streams {
			st.notify()
		}
		s.mu.Unlock()
	})
}

// Open opens a new stream to the peer.
func (s *Session) Open(ctx context.Context) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return nil, ErrSessionClosed
	default:
	}
	if s.nextID > 1<<31 {
		s.mu.Unlock()
		return nil, errors.New("mux: stream IDs exhausted")
	}
	id := s.nextID
	s.nextID += 2
	st := newStream(s, id)
	s.streams[id] = st
	// Queued under s.mu so opens reach the wire in ID order, which the
	// peer enforces.
	s.queue(frame(id, typeOpen, nil))
	s.mu.Unlock()
	return st, nil
}

// Accept waits for a stream the peer opened.
func (s *Session) Accept() (*Stream, error) {
	select {
	case <-s.done: // a dead session's backlog is useless
		return nil, ErrSessionClosed
	default:
	}
	select {
	case st := <-s.accept:
		return st, nil
	case <-s.done:
		return nil, ErrSessionClosed
	}
}

// Listener returns the session as a net.Listener of incoming streams, so an
// http.Server can serve them. Closing the listener closes the session.
func (s *Session) Listener() net.Listener { return listener{s} }

type listener struct{ s *Session }

func (l listener) Accept() (net.Conn, error) {
	st, err := l.s.Accept()
	if err != nil {
		return nil, err
	}
	return st, nil
}
func (l listener) Close() error   { return l.s.Close() }
func (l listener) Addr() net.Addr { return l.s.LocalAddr() }

// queue adds a control frame for the writer; it never blocks and may be
// called with s.mu held.
func (s *Session) queue(f []byte) {
	s.ctrlMu.Lock()
	if len(s.ctrlQ) >= maxCtrlQueue {
		s.ctrlMu.Unlock()
		// Not inline: callers may hold s.mu, which shutdown takes.
		go s.shutdown(fmt.Errorf("%w: peer does not read", ErrProtocol))
		return
	}
	s.ctrlQ = append(s.ctrlQ, f)
	s.ctrlMu.Unlock()
	select {
	case s.ctrlSig <- struct{}{}:
	default:
	}
}

// writeData hands a data frame to the writer and waits until it is written.
func (s *Session) writeData(f []byte, deadline <-chan struct{}) error {
	req := writeReq{frame: f, done: make(chan error, 1)}
	select {
	case s.dataCh <- req:
	case <-s.done:
		return ErrSessionClosed
	case <-deadline:
		return errTimeout
	}
	select {
	case err := <-req.done:
		return err
	case <-s.done:
		return ErrSessionClosed
	}
}

func (s *Session) flushCtrl() error {
	s.ctrlMu.Lock()
	q := s.ctrlQ
	s.ctrlQ = nil
	s.ctrlMu.Unlock()
	if len(q) == 0 {
		return nil
	}
	var buf []byte
	for _, f := range q {
		buf = append(buf, f...)
	}
	_, err := s.conn.Write(buf)
	return err
}

func (s *Session) writeLoop() {
	fail := func(err error) { s.shutdown(fmt.Errorf("mux: write: %w", err)) }
	for {
		if err := s.flushCtrl(); err != nil {
			fail(err)
			return
		}
		select {
		case <-s.ctrlSig:
		case req := <-s.dataCh:
			// Control frames queued before this data (an open) go first.
			if err := s.flushCtrl(); err != nil {
				req.done <- err
				fail(err)
				return
			}
			_, err := s.conn.Write(req.frame)
			req.done <- err
			if err != nil {
				fail(err)
				return
			}
		case <-s.done:
			return
		}
	}
}

func (s *Session) keepalive() {
	t := time.NewTicker(s.pingEvery)
	defer t.Stop()
	var seq uint64
	for {
		select {
		case <-t.C:
		case <-s.done:
			return
		}
		s.mu.Lock()
		missed := s.missed
		s.missed++
		s.mu.Unlock()
		if missed >= s.maxMissed {
			s.shutdown(ErrKeepalive)
			return
		}
		seq++
		s.queue(frame(0, typePing, binary.BigEndian.AppendUint64(nil, seq)))
	}
}

func (s *Session) readLoop() {
	hdr := make([]byte, headerSize)
	for {
		h, p, err := readFrame(s.conn, hdr)
		if err != nil {
			s.shutdown(fmt.Errorf("mux: read: %w", err))
			return
		}
		if err := s.handle(h, p); err != nil {
			s.shutdown(fmt.Errorf("%w: %v", ErrProtocol, err))
			return
		}
	}
}

// remoteParity reports whether id has the parity of the peer's streams.
func (s *Session) remoteParity(id uint32) bool { return (id%2 == 1) != s.caller }

func (s *Session) handle(h header, p []byte) error {
	if h.typ == typePing || h.typ == typePong {
		if h.id != 0 || len(p) != 8 {
			return fmt.Errorf("bad %d frame", h.typ)
		}
		if h.typ == typePing {
			s.queue(frame(0, typePong, p))
		} else {
			s.mu.Lock()
			s.missed = 0
			s.mu.Unlock()
		}
		return nil
	}
	if h.id == 0 {
		return errors.New("stream frame on stream 0")
	}
	s.mu.Lock()
	if h.typ == typeOpen {
		if !s.remoteParity(h.id) || h.id <= s.lastRemote || len(p) != 0 {
			s.mu.Unlock()
			return fmt.Errorf("bad open of stream %d", h.id)
		}
		s.lastRemote = h.id
		st := newStream(s, h.id)
		s.streams[h.id] = st
		s.mu.Unlock()
		select {
		case s.accept <- st:
		default: // backlog full: refuse this stream
			st.abort(true)
		}
		return nil
	}
	st := s.streams[h.id]
	known := st != nil || (s.remoteParity(h.id) && h.id <= s.lastRemote) || (!s.remoteParity(h.id) && h.id < s.nextID)
	s.mu.Unlock()
	if !known {
		return fmt.Errorf("frame for unopened stream %d", h.id)
	}
	if st == nil {
		return nil // a stream already gone (reset or closed): late frames are fine
	}
	switch h.typ {
	case typeData:
		return st.recv(p)
	case typeWindow:
		if len(p) != 4 {
			return errors.New("bad window frame")
		}
		return st.grant(binary.BigEndian.Uint32(p))
	case typeClose:
		if len(p) != 0 {
			return errors.New("bad close frame")
		}
		return st.remoteClose()
	case typeReset:
		st.remoteReset()
		return nil
	default:
		return fmt.Errorf("unknown frame type %d", h.typ)
	}
}

func (s *Session) remove(id uint32) {
	s.mu.Lock()
	delete(s.streams, id)
	s.mu.Unlock()
}
