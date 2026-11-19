package crdt

import "encoding/json"

// Block is one block of a Doc: a paragraph, heading, list item and so on.
type Block struct {
	ID    Stamp
	Type  string
	Attrs json.RawMessage // JSON object, nil if none
	Text  string
}

// BlockFields are the initial values of a new block.
type BlockFields struct {
	Type  string          `json:"type,omitempty"`
	Attrs json.RawMessage `json:"attrs,omitempty"`
	Text  string          `json:"text,omitempty"`
}

// DocOp is an operation on a Doc. Op is "insert" (new block ID after After, with Block as its
// initial fields), "delete" (remove block ID) or "set" (write Field "type", "attrs" or "text" of
// block ID to Value at Stamp).
type DocOp struct {
	Op    string          `json:"op"`
	ID    Stamp           `json:"id"`
	After Stamp           `json:"after,omitempty"`
	Block *BlockFields    `json:"block,omitempty"`
	Field string          `json:"field,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
	Stamp Stamp           `json:"stamp,omitempty"`
}

type blockRegs struct{ typ, attrs, text Register }

// Doc is a block-structured text document: an ordered List of blocks, each with a type, attrs
// and text that are independent last-writer-wins registers. Edits to different blocks (or to
// different fields of one block) merge; concurrent edits of the same field resolve to the last
// writer, so whole-block text is not merged character by character.
type Doc struct {
	list *List
	regs map[Stamp]*blockRegs
}

// NewDoc returns an empty Doc.
func NewDoc() *Doc { return &Doc{list: NewList(), regs: map[Stamp]*blockRegs{}} }

func (d *Doc) r(id Stamp) *blockRegs {
	b := d.regs[id]
	if b == nil {
		b = &blockRegs{}
		d.regs[id] = b
	}
	return b
}

func str(s string) json.RawMessage { b, _ := json.Marshal(s); return b }

// Apply applies an op in any order, any number of times.
func (d *Doc) Apply(op DocOp) {
	switch op.Op {
	case "insert":
		d.list.Apply(ListOp{Op: "insert", ID: op.ID, After: op.After})
		if f := op.Block; f != nil {
			b := d.r(op.ID)
			b.typ.Set(str(f.Type), op.ID)
			if f.Attrs != nil {
				b.attrs.Set(f.Attrs, op.ID)
			}
			b.text.Set(str(f.Text), op.ID)
		}
	case "delete":
		d.list.Apply(ListOp{Op: "delete", ID: op.ID})
	case "set":
		b := d.r(op.ID)
		switch op.Field {
		case "type":
			b.typ.Set(op.Value, op.Stamp)
		case "attrs":
			b.attrs.Set(op.Value, op.Stamp)
		case "text":
			b.text.Set(op.Value, op.Stamp)
		}
	}
}

// Blocks returns the visible blocks in order.
func (d *Doc) Blocks() []Block {
	var out []Block
	for _, it := range d.list.Items() {
		b := Block{ID: it.ID}
		if r := d.regs[it.ID]; r != nil {
			_ = json.Unmarshal(r.typ.Value(), &b.Type)
			_ = json.Unmarshal(r.text.Value(), &b.Text)
			b.Attrs = r.attrs.Value()
		}
		out = append(out, b)
	}
	return out
}

// InsertBlock adds a block at visible index i and returns the op to append.
func (d *Doc) InsertBlock(clk *Clock, i int, f BlockFields) DocOp {
	items := d.list.Items()
	if i < 0 || i > len(items) {
		i = len(items)
	}
	var after Stamp
	if i > 0 {
		after = items[i-1].ID
	}
	op := DocOp{Op: "insert", ID: clk.Now(), After: after, Block: &f}
	d.Apply(op)
	return op
}

// DeleteBlock removes block id.
func (d *Doc) DeleteBlock(id Stamp) DocOp {
	op := DocOp{Op: "delete", ID: id}
	d.Apply(op)
	return op
}

// SetText replaces the text of block id.
func (d *Doc) SetText(clk *Clock, id Stamp, text string) DocOp {
	return d.set(clk, id, "text", str(text))
}

// SetType changes the type of block id.
func (d *Doc) SetType(clk *Clock, id Stamp, typ string) DocOp {
	return d.set(clk, id, "type", str(typ))
}

// SetAttrs replaces the attrs (a JSON object) of block id.
func (d *Doc) SetAttrs(clk *Clock, id Stamp, attrs any) (DocOp, error) {
	raw, err := json.Marshal(attrs)
	if err != nil {
		return DocOp{}, err
	}
	return d.set(clk, id, "attrs", raw), nil
}

func (d *Doc) set(clk *Clock, id Stamp, field string, v json.RawMessage) DocOp {
	op := DocOp{Op: "set", ID: id, Field: field, Value: v, Stamp: clk.Now()}
	d.Apply(op)
	return op
}
