package crdt

import (
	"bytes"
	"encoding/json"
	"sort"
)

// Register is a last-writer-wins register holding raw JSON.
type Register struct {
	val   json.RawMessage
	stamp Stamp
}

// Set applies a write; it wins if stamp is newer than the current one. Reports whether the
// register changed.
func (r *Register) Set(v json.RawMessage, stamp Stamp) bool {
	if !r.stamp.Less(stamp) {
		return false
	}
	r.val, r.stamp = append(json.RawMessage(nil), v...), stamp
	return true
}

// Value returns the current value (nil if never written).
func (r *Register) Value() json.RawMessage { return r.val }

// Stamp returns the stamp of the winning write.
func (r *Register) Stamp() Stamp { return r.stamp }

// MapOp is one write to a Map: set Key to Value, or delete it.
type MapOp struct {
	Key    string          `json:"key"`
	Value  json.RawMessage `json:"value,omitempty"`
	Delete bool            `json:"delete,omitempty"`
	Stamp  Stamp           `json:"stamp"`
}

type mapCell struct {
	reg     Register
	deleted bool
}

// Map is a last-writer-wins map from string keys to JSON values. Deletes are tombstones, so a
// delete and a concurrent set resolve by stamp like any two writes.
type Map struct{ cells map[string]*mapCell }

// NewMap returns an empty Map.
func NewMap() *Map { return &Map{cells: map[string]*mapCell{}} }

// Apply applies an op (idempotent, commutative) and reports whether the state changed.
func (m *Map) Apply(op MapOp) bool {
	c := m.cells[op.Key]
	if c == nil {
		c = &mapCell{}
		m.cells[op.Key] = c
	}
	v := op.Value
	if op.Delete {
		v = nil
	}
	if !c.reg.Set(v, op.Stamp) {
		return false
	}
	c.deleted = op.Delete
	return true
}

// Set writes key locally and returns the op to append to your feed.
func (m *Map) Set(clk *Clock, key string, v any) (MapOp, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return MapOp{}, err
	}
	op := MapOp{Key: key, Value: raw, Stamp: clk.Now()}
	m.Apply(op)
	return op, nil
}

// Delete removes key locally and returns the op.
func (m *Map) Delete(clk *Clock, key string) MapOp {
	op := MapOp{Key: key, Delete: true, Stamp: clk.Now()}
	m.Apply(op)
	return op
}

// Get returns the value of key.
func (m *Map) Get(key string) (json.RawMessage, bool) {
	c := m.cells[key]
	if c == nil || c.deleted {
		return nil, false
	}
	return c.reg.Value(), true
}

// Keys returns the live keys, sorted.
func (m *Map) Keys() []string {
	var ks []string
	for k, c := range m.cells {
		if !c.deleted {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	return ks
}

// Equal reports whether two maps have the same live content.
func (m *Map) Equal(o *Map) bool {
	a, b := m.Keys(), o.Keys()
	if len(a) != len(b) {
		return false
	}
	for i, k := range a {
		if k != b[i] {
			return false
		}
		x, _ := m.Get(k)
		y, _ := o.Get(k)
		if !bytes.Equal(x, y) {
			return false
		}
	}
	return true
}
