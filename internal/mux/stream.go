package mux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// errTimeout is returned when a deadline passes; it matches
// os.ErrDeadlineExceeded and is a net.Error with Timeout() true.
var errTimeout error = os.ErrDeadlineExceeded

// Stream is one bidirectional stream. It implements net.Conn, including
// deadlines and CloseWrite (half-close).
type Stream struct {
	s  *Session
	id uint32

	mu         sync.Mutex
	rbuf       bytes.Buffer
	recvWindow uint32 // bytes the peer may still send before a grant
	unacked    uint32 // bytes read by the app, not yet granted back
	sendWindow uint32 // bytes we may send before the peer grants more
	remoteFin  bool   // peer half-closed: EOF after rbuf
	localFin   bool   // we sent close
	closed     bool   // Close called: drop incoming data
	reset      bool

	sig  chan struct{} // wakes readers and writers on any change
	rdl  deadline
	wdl  deadline
	once sync.Once // removal
}

func newStream(s *Session, id uint32) *Stream {
	return &Stream{s: s, id: id, recvWindow: InitialWindow, sendWindow: InitialWindow,
		sig: make(chan struct{}, 1), rdl: makeDeadline(), wdl: makeDeadline()}
}

// ID returns the stream ID.
func (st *Stream) ID() uint32 { return st.id }

// Session returns the session the stream belongs to.
func (st *Stream) Session() *Session { return st.s }

func (st *Stream) notify() {
	select {
	case st.sig <- struct{}{}:
	default:
	}
}

// maybeRemoveLocked drops the stream from the session once nothing more
// can arrive or be sent on it.
func (st *Stream) maybeRemoveLocked() {
	if st.reset || (st.remoteFin && st.localFin) {
		st.once.Do(func() { st.s.remove(st.id) })
	}
}

// Read reads data the peer sent. It returns io.EOF after the peer
// half-closed and every byte was read.
func (st *Stream) Read(p []byte) (int, error) {
	for {
		st.mu.Lock()
		switch {
		case st.closed:
			st.mu.Unlock()
			return 0, net.ErrClosed
		case st.rbuf.Len() > 0:
			n, _ := st.rbuf.Read(p)
			st.unacked += uint32(n)
			var grant uint32
			if st.unacked >= InitialWindow/2 && !st.remoteFin {
				grant, st.unacked = st.unacked, 0
				st.recvWindow += grant
			}
			st.mu.Unlock()
			if grant > 0 {
				st.s.queue(frame(st.id, typeWindow, binary.BigEndian.AppendUint32(nil, grant)))
			}
			return n, nil
		case st.reset:
			st.mu.Unlock()
			return 0, ErrReset
		case st.remoteFin:
			st.mu.Unlock()
			return 0, io.EOF
		}
		st.mu.Unlock()
		if len(p) == 0 {
			return 0, nil
		}
		select {
		case <-st.sig:
		case <-st.rdl.wait():
			return 0, errTimeout
		case <-st.s.done:
			st.mu.Lock()
			ready := st.rbuf.Len() > 0 || st.remoteFin
			st.mu.Unlock()
			if !ready {
				return 0, ErrSessionClosed
			}
		}
	}
}

// Write sends p, blocking while the peer's window is full.
func (st *Stream) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		st.mu.Lock()
		for {
			if err := st.writeErrLocked(); err != nil {
				st.mu.Unlock()
				return total, err
			}
			if st.sendWindow > 0 {
				break
			}
			st.mu.Unlock()
			select {
			case <-st.sig:
			case <-st.wdl.wait():
				return total, errTimeout
			case <-st.s.done:
				return total, ErrSessionClosed
			}
			st.mu.Lock()
		}
		n := min(len(p), int(st.sendWindow), ChunkSize)
		st.sendWindow -= uint32(n)
		st.mu.Unlock()
		if err := st.s.writeData(frame(st.id, typeData, p[:n]), st.wdl.wait()); err != nil {
			st.mu.Lock()
			st.sendWindow += uint32(n) // never sent
			st.mu.Unlock()
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

func (st *Stream) writeErrLocked() error {
	switch {
	case st.reset:
		return ErrReset
	case st.closed || st.localFin:
		return net.ErrClosed
	}
	select {
	case <-st.s.done:
		return ErrSessionClosed
	default:
		return nil
	}
}

// CloseWrite half-closes the stream: the peer reads EOF after the data
// already written; this side can still read.
func (st *Stream) CloseWrite() error {
	st.mu.Lock()
	if st.reset {
		st.mu.Unlock()
		return ErrReset
	}
	if st.localFin {
		st.mu.Unlock()
		return nil
	}
	st.localFin = true
	st.maybeRemoveLocked()
	st.mu.Unlock()
	st.s.queue(frame(st.id, typeClose, nil))
	st.notify()
	return nil
}

// Close closes both directions. Data the peer still sends is dropped (and
// its window granted back, so the peer never stalls on a closed stream).
func (st *Stream) Close() error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return nil
	}
	st.closed = true
	grant := st.unacked + uint32(st.rbuf.Len())
	st.unacked = 0
	st.rbuf = bytes.Buffer{}
	sendFin := !st.localFin && !st.reset
	st.localFin = true
	if st.remoteFin || st.reset {
		grant = 0
	}
	st.recvWindow += grant
	st.maybeRemoveLocked()
	st.mu.Unlock()
	if grant > 0 {
		st.s.queue(frame(st.id, typeWindow, binary.BigEndian.AppendUint32(nil, grant)))
	}
	if sendFin {
		st.s.queue(frame(st.id, typeClose, nil))
	}
	st.rdl.close()
	st.wdl.close()
	st.notify()
	return nil
}

// Reset aborts the stream in both directions; the peer's reads and writes
// fail with ErrReset.
func (st *Stream) Reset() error {
	st.abort(true)
	return nil
}

func (st *Stream) abort(send bool) {
	st.mu.Lock()
	if st.reset {
		st.mu.Unlock()
		return
	}
	already := st.localFin && st.remoteFin
	st.reset = true
	st.rbuf = bytes.Buffer{}
	st.maybeRemoveLocked()
	st.mu.Unlock()
	if send && !already {
		st.s.queue(frame(st.id, typeReset, nil))
	}
	st.notify()
}

// recv handles a data frame from the peer.
func (st *Stream) recv(p []byte) error {
	st.mu.Lock()
	defer st.notify()
	defer st.mu.Unlock()
	if st.remoteFin {
		return errors.New("data after close")
	}
	if uint32(len(p)) > st.recvWindow {
		return errors.New("window overrun")
	}
	if st.reset {
		return nil
	}
	if st.closed {
		// Dropped, and granted straight back.
		if len(p) > 0 {
			st.s.queue(frame(st.id, typeWindow, binary.BigEndian.AppendUint32(nil, uint32(len(p)))))
		}
		return nil
	}
	st.recvWindow -= uint32(len(p))
	st.rbuf.Write(p)
	return nil
}

func (st *Stream) grant(n uint32) error {
	st.mu.Lock()
	defer st.notify()
	defer st.mu.Unlock()
	if uint64(st.sendWindow)+uint64(n) > 1<<31 {
		return errors.New("window overflow")
	}
	st.sendWindow += n
	return nil
}

func (st *Stream) remoteClose() error {
	st.mu.Lock()
	defer st.notify()
	defer st.mu.Unlock()
	if st.remoteFin {
		return errors.New("double close")
	}
	st.remoteFin = true
	st.maybeRemoveLocked()
	return nil
}

func (st *Stream) remoteReset() { st.abort(false) }

// LocalAddr returns the session's local address.
func (st *Stream) LocalAddr() net.Addr { return st.s.LocalAddr() }

// RemoteAddr returns the session's remote address.
func (st *Stream) RemoteAddr() net.Addr { return st.s.RemoteAddr() }

// SetDeadline sets both deadlines.
func (st *Stream) SetDeadline(t time.Time) error {
	st.rdl.set(t)
	st.wdl.set(t)
	return nil
}

// SetReadDeadline sets the read deadline.
func (st *Stream) SetReadDeadline(t time.Time) error { st.rdl.set(t); return nil }

// SetWriteDeadline sets the write deadline (waiting for window counts).
func (st *Stream) SetWriteDeadline(t time.Time) error { st.wdl.set(t); return nil }

// deadline is a resettable deadline whose wait channel closes when it
// passes (the same design as net.Pipe's).
type deadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel chan struct{}
	done   bool // stream closed: stop arming timers
}

func makeDeadline() deadline { return deadline{cancel: make(chan struct{})} }

func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done {
		return
	}
	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel // the old timer fired and closed cancel; wait is done with it
	}
	d.timer = nil
	closed := isClosed(d.cancel)
	if t.IsZero() {
		if closed {
			d.cancel = make(chan struct{})
		}
		return
	}
	if dur := time.Until(t); dur > 0 {
		if closed {
			d.cancel = make(chan struct{})
		}
		c := d.cancel
		d.timer = time.AfterFunc(dur, func() { close(c) })
		return
	}
	if !closed {
		close(d.cancel)
	}
}

func (d *deadline) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done = true
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

func (d *deadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}

func isClosed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}
