package crdt

import "encoding/json"

// ListOp is an operation on a List: insert Value as a new element ID after the element After
// (the zero Stamp means the head), or delete element ID.
type ListOp struct {
	Op    string          `json:"op"` // "insert" or "delete"
	ID    Stamp           `json:"id"`
	After Stamp           `json:"after,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// Item is one visible list element.
type Item struct {
	ID    Stamp
	Value json.RawMessage
}

type elem struct {
	id      Stamp
	value   json.RawMessage
	deleted bool
}

// List is an ordered list CRDT (RGA). Elements are identified by the Stamp of their insert.
// Concurrent inserts at the same place order newest-first, ties by device id. Ops whose
// causal predecessor has not arrived yet are held back and applied when it does, so any
// delivery order converges.
type List struct {
	elems   []elem
	index   map[Stamp]bool // ids present in elems
	pending []ListOp       // inserts waiting for their parent
	dead    map[Stamp]bool // deletes seen (applies to elements arriving later)
}

// NewList returns an empty List.
func NewList() *List {
	return &List{index: map[Stamp]bool{}, dead: map[Stamp]bool{}}
}

// Apply applies an op in any order, any number of times. It reports whether the visible state
// may have changed.
func (l *List) Apply(op ListOp) bool {
	switch op.Op {
	case "delete":
		if l.dead[op.ID] {
			return false
		}
		l.dead[op.ID] = true
		for i := range l.elems {
			if l.elems[i].id == op.ID {
				l.elems[i].deleted = true
			}
		}
		return l.index[op.ID]
	case "insert":
		if l.index[op.ID] {
			return false
		}
		for _, p := range l.pending {
			if p.ID == op.ID {
				return false
			}
		}
		if !op.After.IsZero() && !l.index[op.After] {
			l.pending = append(l.pending, op)
			return false
		}
		l.insert(op)
		// Drain pending ops whose parent is now present, until nothing moves.
		for moved := true; moved; {
			moved = false
			rest := l.pending[:0]
			for _, p := range l.pending {
				if p.After.IsZero() || l.index[p.After] {
					l.insert(p)
					moved = true
				} else {
					rest = append(rest, p)
				}
			}
			l.pending = rest
		}
		return true
	}
	return false
}

func (l *List) insert(op ListOp) {
	pos := 0
	if !op.After.IsZero() {
		for i, e := range l.elems {
			if e.id == op.After {
				pos = i + 1
				break
			}
		}
	}
	// Skip siblings (and their subtrees) that are newer than the new element.
	for pos < len(l.elems) && op.ID.Less(l.elems[pos].id) {
		pos++
	}
	e := elem{id: op.ID, value: append(json.RawMessage(nil), op.Value...), deleted: l.dead[op.ID]}
	l.elems = append(l.elems, elem{})
	copy(l.elems[pos+1:], l.elems[pos:])
	l.elems[pos] = e
	l.index[op.ID] = true
}

// Pending is the number of ops waiting for a missing predecessor.
func (l *List) Pending() int { return len(l.pending) }

// Items returns the visible elements in order.
func (l *List) Items() []Item {
	var out []Item
	for _, e := range l.elems {
		if !e.deleted {
			out = append(out, Item{ID: e.id, Value: e.value})
		}
	}
	return out
}

// Len is the number of visible elements.
func (l *List) Len() int { return len(l.Items()) }

// Insert adds v so it ends up at visible index i (0..Len) and returns the op to append.
func (l *List) Insert(clk *Clock, i int, v any) (ListOp, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return ListOp{}, err
	}
	items := l.Items()
	if i < 0 || i > len(items) {
		i = len(items)
	}
	var after Stamp
	if i > 0 {
		after = items[i-1].ID
	}
	op := ListOp{Op: "insert", ID: clk.Now(), After: after, Value: raw}
	l.Apply(op)
	return op, nil
}

// Delete removes the visible element at index i and returns the op; ok is false if out of range.
func (l *List) Delete(clk *Clock, i int) (op ListOp, ok bool) {
	items := l.Items()
	if i < 0 || i >= len(items) {
		return ListOp{}, false
	}
	op = ListOp{Op: "delete", ID: items[i].ID}
	l.Apply(op)
	return op, true
}
