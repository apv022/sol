// Package crdt has small conflict-free replicated data types for apps built on Sol feeds: a
// last-writer-wins register and map (Map), an ordered list (List, RGA) and a block text
// document (Doc). Each writer appends its ops to its own feed as JSON; every reader applies all
// ops it sees, in any order and any number of times, and ends in the same state.
//
// Ordering uses a hybrid logical clock (Clock, Stamp) with the device id as the final tiebreak.
// Invariant for List and Doc: a replica must feed every Stamp it applies into its Clock
// (Clock.Observe, or Clock.ObserveOp for a whole op) so that a new op's stamp is greater than
// everything it has seen. Do this for every remote op you apply.
package crdt

import (
	"sync"
	"time"
)

// Stamp is a hybrid logical clock timestamp. The zero Stamp is "before everything" (the head
// of a List).
type Stamp struct {
	Wall    int64  `json:"w"` // milliseconds
	Counter uint32 `json:"c"`
	Device  string `json:"d"`
}

// IsZero reports whether s is the zero Stamp.
func (s Stamp) IsZero() bool { return s == Stamp{} }

// Compare orders stamps by wall, counter, then device id: -1, 0 or 1.
func (s Stamp) Compare(o Stamp) int {
	switch {
	case s.Wall != o.Wall:
		return cmp(s.Wall < o.Wall)
	case s.Counter != o.Counter:
		return cmp(s.Counter < o.Counter)
	case s.Device != o.Device:
		return cmp(s.Device < o.Device)
	}
	return 0
}

func cmp(less bool) int {
	if less {
		return -1
	}
	return 1
}

// Less reports s < o.
func (s Stamp) Less(o Stamp) bool { return s.Compare(o) < 0 }

// Clock issues Stamps for one device. Safe for concurrent use.
type Clock struct {
	// Wall returns the current time in milliseconds; tests may replace it before first use.
	Wall func() int64

	mu     sync.Mutex
	device string
	wall   int64
	ctr    uint32
}

// NewClock creates a clock for the device id.
func NewClock(device string) *Clock {
	return &Clock{device: device, Wall: func() int64 { return time.Now().UnixMilli() }}
}

// Now returns a Stamp greater than every Stamp issued or observed so far.
func (c *Clock) Now() Stamp {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w := c.Wall(); w > c.wall {
		c.wall, c.ctr = w, 0
	} else {
		c.ctr++
	}
	return Stamp{Wall: c.wall, Counter: c.ctr, Device: c.device}
}

// Observe folds a Stamp seen from elsewhere into the clock.
func (c *Clock) Observe(s Stamp) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.Wall > c.wall || (s.Wall == c.wall && s.Counter > c.ctr) {
		c.wall, c.ctr = s.Wall, s.Counter
	}
}

// ObserveOp folds every Stamp inside a MapOp, ListOp or DocOp (value or pointer) into the clock.
func (c *Clock) ObserveOp(op any) {
	switch o := op.(type) {
	case MapOp:
		c.Observe(o.Stamp)
	case ListOp:
		c.Observe(o.ID)
	case DocOp:
		c.Observe(o.ID)
		c.Observe(o.Stamp)
	case *MapOp:
		c.ObserveOp(*o)
	case *ListOp:
		c.ObserveOp(*o)
	case *DocOp:
		c.ObserveOp(*o)
	}
}
