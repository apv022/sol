package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/netutil"
)

// BLE is the Android shell's Bluetooth LE links as a transport (design/PROTOCOL.md §Nearby
// "Bluetooth LE"). The shell owns the radio: every L2CAP channel it opens or accepts reaches
// this transport as one loopback TCP connection to the bridge port, starting with one line,
// "SOLBLE1 in\n" (accepted: forwarded to the publication's target, the session layer's BLE
// listener) or "SOLBLE1 out <link>\n" (opened for a link handle Expect issued: kept for Dial).
//
// Invariants:
//
//   - A link carries bytes only; the session handshake on it authenticates both ends. Nothing
//     about a link is trusted, and no address is ever published (Publish returns "").
//   - An "out" link is accepted only for a handle Expect issued in the last LinkTTL, once, and
//     is dialable only to the device that handle was issued for; Dial hands it over once.
//     Unknown handles, malformed lines and links while Allow says no are closed without a word.
//   - Handles are 128-bit random and never logged.
//   - CloseLinks (BLE switched off) and Stop cut every bridged connection.
type BLE struct {
	// Allow reports whether links are accepted now (the switch is on); nil allows.
	Allow func() bool
	// OnLink is told when a dialable link to addr arrived; it must not block.
	OnLink func(addr string)
	Log    *slog.Logger

	mu     sync.Mutex
	state  State
	ln     net.Listener
	target string
	expect map[string]bleExpect // link handle -> the address it was issued for
	ready  map[string]*bleConn  // address -> a link waiting for Dial
	open   map[*bleConn]struct{}
}

// BLELinkTTL is how long a link handle, and then the link, waits to be used.
var BLELinkTTL = time.Minute

const blePreface = "SOLBLE1 "

type bleExpect struct {
	addr string
	at   time.Time
}

// bleConn is a bridged connection; closing it forgets it.
type bleConn struct {
	net.Conn
	b    *BLE
	once sync.Once
}

func (c *bleConn) Close() error {
	c.once.Do(func() {
		c.b.mu.Lock()
		delete(c.b.open, c)
		c.b.mu.Unlock()
	})
	return c.Conn.Close()
}

// ErrNoBLELink is Dial's answer when no link to that device waits.
var ErrNoBLELink = errors.New("no Bluetooth link to that device now")

// BLEAddr is the dial address of device over BLE: "ble:" and the key in hex.
func BLEAddr(device ed25519.PublicKey) string { return "ble:" + hex.EncodeToString(device) }

// IsBLE reports whether addr is a BLE address.
func IsBLE(addr string) bool { return strings.HasPrefix(addr, "ble:") }

// NewBLE returns the transport, stopped.
func NewBLE() *BLE {
	return &BLE{state: StateStopped, expect: map[string]bleExpect{}, ready: map[string]*bleConn{}, open: map[*bleConn]struct{}{}}
}

// Name implements Transport.
func (b *BLE) Name() string { return "ble" }

// Start opens the bridge port on loopback.
func (b *BLE) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.state = StateFailed
		return err
	}
	b.ln, b.state = ln, StateReady
	go b.serve(ln)
	return nil
}

// BridgePort is the loopback port the shell bridges links to (0 while stopped).
func (b *BLE) BridgePort() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ln == nil {
		return 0
	}
	return b.ln.Addr().(*net.TCPAddr).Port
}

// Publish records where accepted links go. BLE has no public address: it returns "".
func (b *BLE) Publish(ctx context.Context, p Publication) (string, error) {
	if p.ID == "node" {
		b.mu.Lock()
		b.target = p.Target
		b.mu.Unlock()
	}
	return "", nil
}

// Unpublish implements Transport.
func (b *BLE) Unpublish(id string) error {
	if id == "node" {
		b.mu.Lock()
		b.target = ""
		b.mu.Unlock()
	}
	return nil
}

// Handles reports whether addr is a BLE address.
func (b *BLE) Handles(addr string) bool { return IsBLE(addr) }

// Expect issues a link handle for addr: the shell bridges the channel it opens with it, and the
// link becomes dialable as addr.
func (b *BLE) Expect(addr string) string {
	var r [16]byte
	rand.Read(r[:])
	h := hex.EncodeToString(r[:])
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, e := range b.expect {
		if now.Sub(e.at) >= BLELinkTTL {
			delete(b.expect, k)
		}
	}
	if len(b.expect) >= 64 {
		return h // never stored: a flood of decisions can't grow the table
	}
	b.expect[h] = bleExpect{addr: addr, at: now}
	return h
}

// Dial hands over the link waiting for addr, once.
func (b *BLE) Dial(ctx context.Context, addr string) (net.Conn, error) {
	b.mu.Lock()
	c := b.ready[addr]
	delete(b.ready, addr)
	b.mu.Unlock()
	if c == nil {
		return nil, ErrNoBLELink
	}
	return c, nil
}

// Waiting reports whether a link to addr waits for Dial.
func (b *BLE) Waiting(addr string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ready[addr] != nil
}

func (b *BLE) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go b.handle(c)
	}
}

func (b *BLE) log() *slog.Logger {
	if b.Log != nil {
		return b.Log
	}
	return slog.Default()
}

// handle reads a bridged connection's first line and routes it.
func (b *BLE) handle(raw net.Conn) {
	if b.Allow != nil && !b.Allow() {
		raw.Close()
		return
	}
	_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := readLine(raw, 64)
	_ = raw.SetReadDeadline(time.Time{})
	if err != nil || !strings.HasPrefix(line, blePreface) {
		b.log().Info("ble: refused a bridge connection without a valid first line")
		raw.Close()
		return
	}
	c := &bleConn{Conn: raw, b: b}
	b.mu.Lock()
	if b.ln == nil {
		b.mu.Unlock()
		raw.Close()
		return
	}
	b.open[c] = struct{}{}
	target := b.target
	b.mu.Unlock()
	switch f := strings.Fields(strings.TrimPrefix(line, blePreface)); {
	case len(f) == 1 && f[0] == "in":
		if target == "" {
			c.Close()
			return
		}
		out, err := net.Dial("tcp", target)
		if err != nil {
			c.Close()
			return
		}
		netutil.Pipe(c, out)
	case len(f) == 2 && f[0] == "out":
		now := time.Now()
		b.mu.Lock()
		e, ok := b.expect[f[1]]
		delete(b.expect, f[1])
		ok = ok && now.Sub(e.at) < BLELinkTTL
		var old *bleConn
		if ok {
			old = b.ready[e.addr]
			b.ready[e.addr] = c
		}
		b.mu.Unlock()
		if !ok {
			b.log().Info("ble: refused a link with an unknown or expired handle")
			c.Close()
			return
		}
		if old != nil {
			old.Close()
		}
		time.AfterFunc(BLELinkTTL, func() {
			b.mu.Lock()
			stale := b.ready[e.addr] == c
			if stale {
				delete(b.ready, e.addr)
			}
			b.mu.Unlock()
			if stale {
				c.Close()
			}
		})
		if b.OnLink != nil {
			b.OnLink(e.addr)
		}
	default:
		b.log().Info("ble: refused a bridge connection without a valid first line")
		c.Close()
	}
}

// readLine reads up to max bytes one at a time (nothing past the newline is consumed) and
// returns the line without its newline.
func readLine(c net.Conn, max int) (string, error) {
	var sb strings.Builder
	var b [1]byte
	for sb.Len() < max {
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return "", err
		}
		if b[0] == '\n' {
			return sb.String(), nil
		}
		sb.WriteByte(b[0])
	}
	return "", errors.New("line too long")
}

// Links is how many bridged connections are open.
func (b *BLE) Links() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.open)
}

// CloseLinks cuts every bridged connection and forgets every handle (the switch went off).
func (b *BLE) CloseLinks() {
	b.mu.Lock()
	cs := make([]*bleConn, 0, len(b.open))
	for c := range b.open {
		cs = append(cs, c)
	}
	clear(b.expect)
	clear(b.ready)
	b.mu.Unlock()
	for _, c := range cs {
		c.Close()
	}
}

// Status implements Transport.
func (b *BLE) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := Status{Name: b.Name(), State: b.state}
	if b.state == StateReady {
		st.Detail = fmt.Sprintf("Bluetooth LE links: %d", len(b.open))
	}
	return st
}

// Stop closes the bridge port and every link.
func (b *BLE) Stop() error {
	b.mu.Lock()
	ln := b.ln
	b.ln, b.state = nil, StateStopped
	b.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	b.CloseLinks()
	return nil
}
