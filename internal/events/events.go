// Package events is the daemon's in-process event bus: feeds publish new
// entries and readability changes on it, replication wakes up from it, and
// the app API's SSE stream (later) is fed from it.
//
// Invariants:
//
//   - Publish never blocks. Each subscriber has its own buffer; when it is
//     full the event is dropped for that subscriber only and counted
//     (Sub.Dropped), so one slow consumer can never stall the publisher or
//     the other subscribers.
//   - Consumers that must not miss anything treat an event as a wake-up and
//     re-read the durable source (a feed by pos), using Dropped to know
//     that they have to.
package events

import (
	"sync"
	"sync/atomic"
)

// Bus fans events of type T out to subscribers. The zero value is ready to
// use. Safe for concurrent use.
type Bus[T any] struct {
	mu   sync.Mutex
	subs map[*Sub[T]]struct{}
}

// Sub is one subscription. Read events from C; call Close when done.
type Sub[T any] struct {
	bus     *Bus[T]
	c       chan T
	dropped atomic.Uint64
	closed  bool // under bus.mu
}

// Subscribe returns a subscription whose channel buffers up to buf events
// (at least 1).
func (b *Bus[T]) Subscribe(buf int) *Sub[T] {
	s := &Sub[T]{bus: b, c: make(chan T, max(buf, 1))}
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[*Sub[T]]struct{}{}
	}
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

// Publish delivers v to every subscriber that has room, without blocking.
func (b *Bus[T]) Publish(v T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		select {
		case s.c <- v:
		default:
			s.dropped.Add(1)
		}
	}
}

// C returns the channel events arrive on. It is closed by Close.
func (s *Sub[T]) C() <-chan T { return s.c }

// Dropped returns how many events were dropped because the buffer was full.
func (s *Sub[T]) Dropped() uint64 { return s.dropped.Load() }

// Close ends the subscription and closes its channel. Safe to call twice.
func (s *Sub[T]) Close() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	delete(s.bus.subs, s)
	close(s.c)
}
