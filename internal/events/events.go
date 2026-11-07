// Package events is an in-memory ring buffer of recent security and
// lifecycle events, shown on the panel's Logs screen.
//
// Messages must never contain secrets, pairing tokens, keys, or request data.
package events

import (
	"log/slog"
	"sync"
	"time"
)

// Event kinds.
const (
	KindReject    = "reject"    // a connection failed the handshake
	KindPair      = "pair"      // a device paired
	KindRevoke    = "revoke"    // a device was removed
	KindTransport = "transport" // a transport changed state
	KindService   = "service"   // a service was added/removed/changed
	KindAudit     = "audit"
)

// Event is one log line.
type Event struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

// Log is a fixed-size ring buffer, safe for concurrent use. Every event is
// also written to the process logger (slog) so it appears on stderr.
type Log struct {
	mu   sync.Mutex
	buf  []Event
	next int  // index of the slot the next event goes into
	full bool // buffer has wrapped at least once
}

// New creates a Log keeping the most recent max events (max <= 0 means 500).
func New(max int) *Log {
	if max <= 0 {
		max = 500
	}
	return &Log{buf: make([]Event, max)}
}

// Add records an event, dropping the oldest one if the buffer is full.
func (l *Log) Add(kind, message string) {
	slog.Info(message, "kind", kind)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf[l.next] = Event{Time: time.Now().UTC(), Kind: kind, Message: message}
	l.next = (l.next + 1) % len(l.buf)
	if l.next == 0 {
		l.full = true
	}
}

// Recent returns up to n events, newest first (n <= 0 means all).
func (l *Log) Recent(n int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := l.next
	if l.full {
		count = len(l.buf)
	}
	if n <= 0 || n > count {
		n = count
	}
	out := make([]Event, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, l.buf[(l.next-i+len(l.buf))%len(l.buf)])
	}
	return out
}
