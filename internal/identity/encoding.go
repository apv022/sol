package identity

import (
	"encoding/binary"
	"errors"
	"math"
)

// This file is the whole of IDENTITY.md §Encoding: lp(x) is a 4-byte
// big-endian length then x, u64(n) is 8 bytes big-endian, a list is
// u64(count) then its items. Every other file builds signed bytes only
// through these helpers, so there is one place to get it right.

// errMalformed is returned for any byte string that does not decode exactly.
var errMalformed = errors.New("identity: malformed encoding")

// maxField bounds a single lp field on decode, so a hostile peer cannot make
// us allocate much. Signed objects are small.
const maxField = 64 << 10

// enc appends fields to a byte string.
type enc struct{ b []byte }

// lp appends a length-prefixed byte string.
func (e *enc) lp(x []byte) {
	if uint64(len(x)) > math.MaxUint32 {
		panic("identity: field too long") // unreachable: callers validate sizes
	}
	e.b = binary.BigEndian.AppendUint32(e.b, uint32(len(x)))
	e.b = append(e.b, x...)
}

// str appends a length-prefixed UTF-8 string.
func (e *enc) str(s string) { e.lp([]byte(s)) }

// u64 appends an 8-byte big-endian integer.
func (e *enc) u64(n uint64) { e.b = binary.BigEndian.AppendUint64(e.b, n) }

// count appends a list's item count; the items follow.
func (e *enc) count(n int) { e.u64(uint64(n)) }

// dec reads fields from a byte string. The first error sticks; later reads
// return zero values, and done reports it.
type dec struct {
	b   []byte
	err error
}

func (d *dec) fail() { d.err, d.b = errMalformed, nil }

// lp reads a length-prefixed byte string (a copy, never aliasing the input).
func (d *dec) lp() []byte {
	if d.err != nil || len(d.b) < 4 {
		d.fail()
		return nil
	}
	n := binary.BigEndian.Uint32(d.b)
	if n > maxField || uint64(len(d.b)-4) < uint64(n) {
		d.fail()
		return nil
	}
	out := append([]byte{}, d.b[4:4+n]...)
	d.b = d.b[4+n:]
	return out
}

// str reads a length-prefixed string.
func (d *dec) str() string { return string(d.lp()) }

// u64 reads an 8-byte big-endian integer.
func (d *dec) u64() uint64 {
	if d.err != nil || len(d.b) < 8 {
		d.fail()
		return 0
	}
	n := binary.BigEndian.Uint64(d.b)
	d.b = d.b[8:]
	return n
}

// count reads a list count and rejects anything above max.
func (d *dec) count(max int) int {
	n := d.u64()
	if d.err == nil && n > uint64(max) {
		d.fail()
		return 0
	}
	return int(n)
}

// expect reads a context string and fails unless it equals want.
func (d *dec) expect(want string) {
	if got := d.str(); d.err == nil && got != want {
		d.fail()
	}
}

// done reports the first error, or an error if bytes are left over.
func (d *dec) done() error {
	if d.err == nil && len(d.b) != 0 {
		d.fail()
	}
	return d.err
}
