package crdt

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

func testClock(dev string, start int64) *Clock {
	c := NewClock(dev)
	t := start
	c.Wall = func() int64 { t++; return t }
	return c
}

func shuffleDup(r *rand.Rand, ops []any) []any {
	out := append([]any(nil), ops...)
	for i := 0; i < len(ops)/2; i++ { // duplicates
		out = append(out, ops[r.Intn(len(ops))])
	}
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func TestStampOrder(t *testing.T) {
	a := Stamp{1, 0, "a"}
	b := Stamp{1, 0, "b"}
	c := Stamp{1, 1, "a"}
	d := Stamp{2, 0, "a"}
	if !(a.Less(b) && b.Less(c) && c.Less(d)) || a.Compare(a) != 0 || d.Compare(a) != 1 {
		t.Fatal("bad stamp order")
	}
	clk := testClock("x", 10)
	s1 := clk.Now()
	clk.Observe(Stamp{Wall: 1000, Counter: 5})
	if s2 := clk.Now(); !s1.Less(s2) || s2.Wall != 1000 || s2.Counter != 6 {
		t.Fatalf("clock after observe: %+v", s2)
	}
}

func TestMapBasics(t *testing.T) {
	a, b := NewMap(), NewMap()
	ca, cb := testClock("a", 0), testClock("b", 0)
	o1, _ := a.Set(ca, "k", "from-a")
	o2, _ := b.Set(cb, "k", "from-b") // same wall, device b wins
	a.Apply(o2)
	b.Apply(o1)
	v, _ := a.Get("k")
	if string(v) != `"from-b"` || !a.Equal(b) {
		t.Fatalf("got %s", v)
	}
	d := a.Delete(ca, "k")
	b.Apply(d)
	if _, ok := b.Get("k"); ok || len(b.Keys()) != 0 {
		t.Fatal("delete lost")
	}
	if b.Apply(o1) { // stale write after delete must not resurrect
		t.Fatal("stale op changed state")
	}
}

func TestMapConverges(t *testing.T) {
	for seed := int64(0); seed < 200; seed++ {
		r := rand.New(rand.NewSource(seed))
		var ops []any
		clocks := []*Clock{testClock("a", 0), testClock("b", 0), testClock("c", 0)}
		maps := []*Map{NewMap(), NewMap(), NewMap()}
		for i := 0; i < 40; i++ {
			k := r.Intn(3)
			key := fmt.Sprint("k", r.Intn(5))
			var op MapOp
			if r.Intn(4) == 0 {
				op = maps[k].Delete(clocks[k], key)
			} else {
				op, _ = maps[k].Set(clocks[k], key, r.Intn(100))
			}
			ops = append(ops, op)
			if r.Intn(3) == 0 { // partial sync
				j := r.Intn(3)
				maps[j].Apply(op)
				clocks[j].ObserveOp(op)
			}
		}
		var first *Map
		for rep := 0; rep < 4; rep++ {
			m := NewMap()
			for _, o := range shuffleDup(r, ops) {
				m.Apply(o.(MapOp))
			}
			if first == nil {
				first = m
			} else if !first.Equal(m) {
				t.Fatalf("seed %d: maps diverged", seed)
			}
		}
	}
}

func texts(l *List) []string {
	var out []string
	for _, it := range l.Items() {
		var s string
		_ = json.Unmarshal(it.Value, &s)
		out = append(out, s)
	}
	return out
}

func TestListConcurrentInsertSamePlace(t *testing.T) {
	a, b := NewList(), NewList()
	ca, cb := testClock("a", 0), testClock("b", 0)
	base, _ := a.Insert(ca, 0, "x")
	b.Apply(base)
	cb.ObserveOp(base)
	oa, _ := a.Insert(ca, 1, "A")
	ob, _ := b.Insert(cb, 1, "B")
	a.Apply(ob)
	b.Apply(oa)
	if !reflect.DeepEqual(texts(a), texts(b)) || len(texts(a)) != 3 || texts(a)[0] != "x" {
		t.Fatalf("a=%v b=%v", texts(a), texts(b))
	}
	del, ok := a.Delete(ca, 0)
	if !ok {
		t.Fatal("delete")
	}
	b.Apply(del)
	b.Apply(del)
	if !reflect.DeepEqual(texts(a), texts(b)) || a.Len() != 2 {
		t.Fatalf("after delete a=%v b=%v", texts(a), texts(b))
	}
	if _, ok := a.Delete(ca, 9); ok {
		t.Fatal("out of range delete")
	}
}

func TestListOutOfOrderBuffering(t *testing.T) {
	src := NewList()
	c := testClock("a", 0)
	var ops []ListOp
	for i := 0; i < 5; i++ {
		op, _ := src.Insert(c, i, fmt.Sprint(i))
		ops = append(ops, op)
	}
	dst := NewList()
	for i := len(ops) - 1; i >= 0; i-- {
		dst.Apply(ops[i])
	}
	if dst.Pending() != 0 || !reflect.DeepEqual(texts(dst), texts(src)) {
		t.Fatalf("pending=%d %v", dst.Pending(), texts(dst))
	}
}

func TestListConverges(t *testing.T) {
	for seed := int64(0); seed < 300; seed++ {
		r := rand.New(rand.NewSource(seed))
		var ops []any
		devs := []string{"a", "b", "c"}
		clocks := make([]*Clock, 3)
		lists := make([]*List, 3)
		for i := range devs {
			clocks[i], lists[i] = testClock(devs[i], int64(r.Intn(3))), NewList()
		}
		for i := 0; i < 50; i++ {
			k := r.Intn(3)
			var op ListOp
			if r.Intn(4) == 0 && lists[k].Len() > 0 {
				op, _ = lists[k].Delete(clocks[k], r.Intn(lists[k].Len()))
			} else {
				op, _ = lists[k].Insert(clocks[k], r.Intn(lists[k].Len()+1), i)
			}
			ops = append(ops, op)
			for j := range lists { // causal-ish partial sync of recent ops
				if j != k && r.Intn(3) == 0 {
					lists[j].Apply(op)
					clocks[j].ObserveOp(op)
				}
			}
		}
		var want []string
		for rep := 0; rep < 4; rep++ {
			l := NewList()
			for _, o := range shuffleDup(r, ops) {
				l.Apply(o.(ListOp))
			}
			if l.Pending() != 0 {
				t.Fatalf("seed %d: %d pending", seed, l.Pending())
			}
			got := texts(l)
			if rep == 0 {
				want = got
			} else if !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d: diverged\n%v\n%v", seed, want, got)
			}
		}
	}
}

func TestDocMergeBlocks(t *testing.T) {
	a, b := NewDoc(), NewDoc()
	ca, cb := testClock("a", 0), testClock("b", 0)
	o1 := a.InsertBlock(ca, 0, BlockFields{Type: "p", Text: "one"})
	o2 := a.InsertBlock(ca, 1, BlockFields{Type: "p", Text: "two"})
	b.Apply(o1)
	b.Apply(o2)
	cb.ObserveOp(o1)
	cb.ObserveOp(o2)
	ids := a.Blocks()
	// concurrent edits to different blocks both survive
	ea := a.SetText(ca, ids[0].ID, "ONE")
	eb := b.SetText(cb, ids[1].ID, "TWO")
	a.Apply(eb)
	b.Apply(ea)
	for _, d := range []*Doc{a, b} {
		bl := d.Blocks()
		if bl[0].Text != "ONE" || bl[1].Text != "TWO" {
			t.Fatalf("%+v", bl)
		}
	}
	// same block: last writer wins
	xa := a.SetText(ca, ids[0].ID, "from a")
	xb := b.SetText(cb, ids[0].ID, "from b")
	a.Apply(xb)
	b.Apply(xa)
	want := "from a"
	if xa.Stamp.Less(xb.Stamp) {
		want = "from b"
	}
	if !reflect.DeepEqual(a.Blocks(), b.Blocks()) || a.Blocks()[0].Text != want {
		t.Fatalf("%+v %+v", a.Blocks(), b.Blocks())
	}
	// type/attrs are separate registers from text
	ta := a.SetType(ca, ids[0].ID, "h1")
	at, _ := b.SetAttrs(cb, ids[0].ID, map[string]int{"level": 2})
	a.Apply(at)
	b.Apply(ta)
	bl := a.Blocks()[0]
	if bl.Type != "h1" || string(bl.Attrs) != `{"level":2}` || bl.Text != want || !reflect.DeepEqual(a.Blocks(), b.Blocks()) {
		t.Fatalf("%+v", bl)
	}
	b.Apply(a.DeleteBlock(ids[1].ID))
	if len(b.Blocks()) != 1 {
		t.Fatal("block delete")
	}
}

func TestDocConverges(t *testing.T) {
	for seed := int64(0); seed < 200; seed++ {
		r := rand.New(rand.NewSource(seed))
		var ops []any
		clocks := []*Clock{testClock("a", 0), testClock("b", 0), testClock("c", 0)}
		docs := []*Doc{NewDoc(), NewDoc(), NewDoc()}
		for i := 0; i < 50; i++ {
			k := r.Intn(3)
			d, c := docs[k], clocks[k]
			bl := d.Blocks()
			var op DocOp
			switch n := r.Intn(8); {
			case n < 3 || len(bl) == 0:
				op = d.InsertBlock(c, r.Intn(len(bl)+1), BlockFields{Type: "p", Text: fmt.Sprint(i)})
			case n < 4:
				op = d.DeleteBlock(bl[r.Intn(len(bl))].ID)
			case n < 6:
				op = d.SetText(c, bl[r.Intn(len(bl))].ID, fmt.Sprint("t", i))
			case n < 7:
				op = d.SetType(c, bl[r.Intn(len(bl))].ID, fmt.Sprint("ty", i))
			default:
				op, _ = d.SetAttrs(c, bl[r.Intn(len(bl))].ID, map[string]int{"n": i})
			}
			ops = append(ops, op)
			for j := range docs {
				if j != k && r.Intn(3) == 0 {
					docs[j].Apply(op)
					clocks[j].ObserveOp(op)
				}
			}
		}
		var want []Block
		for rep := 0; rep < 4; rep++ {
			d := NewDoc()
			for _, o := range shuffleDup(r, ops) {
				d.Apply(o.(DocOp))
			}
			got := d.Blocks()
			if rep == 0 {
				want = got
			} else if !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d diverged", seed)
			}
		}
	}
}

func TestOpsJSONRoundTrip(t *testing.T) {
	d := NewDoc()
	op := d.InsertBlock(testClock("a", 0), 0, BlockFields{Type: "p", Text: "hi"})
	b, _ := json.Marshal(op)
	var back DocOp
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	d2 := NewDoc()
	d2.Apply(back)
	if !reflect.DeepEqual(d.Blocks(), d2.Blocks()) {
		t.Fatalf("%+v vs %+v", d.Blocks(), d2.Blocks())
	}
}
