package tasks

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/apv022/sol/sdk/go/crdt"
	"github.com/apv022/sol/sdk/go/sol"
)

// Everything that changes a list goes through a batch: the keys to set, stamped by the device's
// clock, appended as one feed entry so a change shows up all at once on every device.

func bad(format string, a ...any) error { return sol.Errorf("bad_request", fmt.Sprintf(format, a...)) }
func missing(what string) error         { return sol.Errorf("not_found", "no such "+what) }

type batch struct {
	a   *App
	m   *crdt.Map // scratch: only used to mint stamped ops
	ops []crdt.MapOp
}

func (a *App) newBatch() *batch { return &batch{a: a, m: crdt.NewMap()} }

func (b *batch) set(key string, v any) {
	op, err := b.m.Set(b.a.crdtClk, key, v)
	if err != nil {
		panic("tasks: unmarshalable op value: " + err.Error()) // only strings, numbers and bools reach here
	}
	b.ops = append(b.ops, op)
}

func (b *batch) task(id, field string, v any) { b.set("t."+id+"."+field, v) }

func randID() string {
	var x [4]byte
	_, _ = rand.Read(x[:])
	return hex.EncodeToString(x[:])
}

// deriveID is the id of the next occurrence of a recurring task: a function of the old id and
// the new due date, so two devices that complete the same task at once create the same task.
func deriveID(id, due string) string {
	s := sha256.Sum256([]byte(id + "|" + due))
	return hex.EncodeToString(s[:4])
}

func validDue(s string) bool {
	_, err := time.Parse(dateLayout, s)
	return s == "" || err == nil
}

func validTime(s string) bool {
	_, err := time.Parse(timeLayout, s)
	return s == "" || err == nil
}

// commit appends a batch to my copy of the list's feed: the owner's own feed, or for a list
// shared with me for writing, my co-author feed of the same name (shared back to the owner first,
// API.md §5).
func (a *App) commit(ctx context.Context, ls *listState, b *batch) error {
	if ls.mode == modeRead {
		return sol.Errorf("forbidden", "this list is shared with you read-only")
	}
	if len(b.ops) == 0 {
		return nil
	}
	if ls.owner != a.me {
		a.mu.Lock()
		done := a.back[ls.key]
		a.mu.Unlock()
		if !done {
			if err := a.api.Share(ctx, sol.ShareRequest{Feed: ls.feed, People: []string{ls.owner}, Mode: sol.ModeRead}); err != nil {
				return err
			}
			a.mu.Lock()
			a.back[ls.key] = true
			a.mu.Unlock()
		}
	}
	if err := a.appendOps(ctx, ls.feed, b.ops); err != nil {
		return err
	}
	a.afterChange(ctx)
	return nil
}

var palette = map[string]bool{"red": true, "orange": true, "yellow": true, "green": true, "teal": true, "blue": true, "indigo": true, "purple": true, "pink": true, "brown": true, "gray": true}

func validIcon(s string) bool {
	if s == "" || len(s) > 24 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// createList makes a new list owned by me and returns its id.
func (a *App) createList(ctx context.Context, name, color, icon string) (string, error) {
	id := randID()
	var order float64
	for _, ls := range a.view() {
		order = max(order, ls.info.Order)
	}
	b := a.newBatch()
	b.set("l.owner", a.me)
	b.set("l.name", name)
	b.set("l.color", color)
	b.set("l.icon", icon)
	b.set("l.order", order+1024)
	if err := a.appendOps(ctx, "list."+id, b.ops); err != nil {
		return "", err
	}
	a.afterChange(ctx)
	return id, nil
}

// taskIn is the content of a new task.
type taskIn struct {
	Title  string `json:"title"`
	Notes  string `json:"notes,omitempty"`
	Due    string `json:"due,omitempty"`
	Time   string `json:"time,omitempty"`
	Repeat string `json:"repeat,omitempty"`
	Parent string `json:"parent,omitempty"`
	Flag   bool   `json:"flag,omitempty"`
}

func (in taskIn) check() error {
	if strings.TrimSpace(in.Title) == "" {
		return bad("a task needs a title")
	}
	if len(in.Title) > maxTitle || len(in.Notes) > maxNotes {
		return bad("title or notes too long")
	}
	if !validDue(in.Due) {
		return bad("due must be YYYY-MM-DD")
	}
	if !validTime(in.Time) {
		return bad("time must be HH:MM")
	}
	if in.Time != "" && in.Due == "" {
		return bad("a time needs a due date")
	}
	if !validRepeat(in.Repeat) {
		return bad("repeat must be daily, weekly, monthly or yearly")
	}
	return nil
}

// endOrder is an order value after every task with the same parent.
func (ls *listState) endOrder(parent string) float64 {
	var o float64
	for _, t := range ls.tasks {
		if t.Parent == parent {
			o = max(o, t.Order)
		}
	}
	return o + 1024
}

// addTask writes a new task into the batch and returns its id.
func (b *batch) addTask(ls *listState, id string, in taskIn, order float64) {
	b.task(id, "title", in.Title)
	b.task(id, "created", float64(b.a.clock.Now().UnixMilli()))
	b.task(id, "order", order)
	if in.Notes != "" {
		b.task(id, "notes", in.Notes)
	}
	if in.Due != "" {
		b.task(id, "due", in.Due)
	}
	if in.Time != "" {
		b.task(id, "time", in.Time)
	}
	if in.Flag {
		b.task(id, "flag", true)
	}
	if in.Repeat != "" {
		b.task(id, "repeat", in.Repeat)
	}
	if in.Parent != "" {
		b.task(id, "parent", in.Parent)
	}
}

// subtasksOf are the (one level of) children of a task, in order.
func (ls *listState) subtasksOf(id string) []*Task {
	var out []*Task
	for _, t := range ls.tasks {
		if t.Parent == id {
			out = append(out, t)
		}
	}
	return out
}

// complete marks a task done, or open again. Completing a recurring task also writes the next
// occurrence (with fresh copies of its subtasks) in the same batch.
func (a *App) completeOps(ls *listState, t *Task, done bool) (*batch, string) {
	b := a.newBatch()
	if !done {
		b.task(t.ID, "done", float64(0))
		return b, ""
	}
	if t.Done {
		return b, ""
	}
	b.task(t.ID, "done", float64(a.clock.Now().UnixMilli()))
	if t.Repeat == "" || t.Parent != "" {
		return b, ""
	}
	next := NextDue(t.Due, t.Repeat, a.now())
	nid := deriveID(t.ID, next)
	b.addTask(ls, nid, taskIn{Title: t.Title, Notes: t.Notes, Due: next, Time: t.Time, Repeat: t.Repeat, Flag: t.Flag}, t.Order)
	for _, s := range ls.subtasksOf(t.ID) {
		b.addTask(ls, deriveID(s.ID, next), taskIn{Title: s.Title, Notes: s.Notes, Parent: nid}, s.Order)
	}
	return b, nid
}

// siblings are the tasks of the list with parent p except skip, in order.
func (ls *listState) siblings(p, skip string) []*Task {
	var out []*Task
	for _, t := range ls.tasks {
		if t.Parent == p && t.ID != skip {
			out = append(out, t)
		}
	}
	return out
}

// placeOrder computes the order for task id to sit before `before` or after `after` among its
// siblings (the end when neither is given), writing fresh orders for the siblings into b when
// the gap between neighbours has become too small to halve again.
func (ls *listState) placeOrder(b *batch, id, parent, before, after string) float64 {
	sib := ls.siblings(parent, id)
	idx := len(sib)
	for i, s := range sib {
		if s.ID == before {
			idx = i
		}
		if s.ID == after {
			idx = i + 1
		}
	}
	lo, hi := 0.0, 0.0
	switch {
	case len(sib) == 0:
		return 1024
	case idx == 0:
		hi = sib[0].Order
		lo = hi - 2048
	case idx == len(sib):
		lo = sib[idx-1].Order
		hi = lo + 2048
	default:
		lo, hi = sib[idx-1].Order, sib[idx].Order
	}
	if hi-lo > 1e-6 {
		return (lo + hi) / 2
	}
	// Renumber: the siblings with the moved task spliced in, 1024 apart.
	seq := append(append(append([]*Task{}, sib[:idx]...), &Task{ID: id}), sib[idx:]...)
	var mine float64
	for i, s := range seq {
		o := float64(i+1) * 1024
		if s.ID == id {
			mine = o
		} else {
			b.task(s.ID, "order", o)
		}
	}
	return mine
}

// moveTask moves a task to another list (copying it, and its subtasks, under the same ids and
// tombstoning the originals) or, within a list, to a position.
func (a *App) moveTask(ctx context.Context, src *listState, t *Task, dst *listState, before, after string) error {
	if dst == nil || dst.key == src.key {
		b := a.newBatch()
		parent := t.Parent
		b.task(t.ID, "order", src.placeOrder(b, t.ID, parent, before, after))
		return a.commit(ctx, src, b)
	}
	if dst.mode == modeRead || src.mode == modeRead {
		return sol.Errorf("forbidden", "a list involved is read-only for you")
	}
	copyIn := a.newBatch()
	subs := src.subtasksOf(t.ID)
	for _, x := range append([]*Task{t}, subs...) {
		id := x.ID
		copyIn.task(id, "title", x.Title)
		copyIn.task(id, "deleted", false)
		copyIn.task(id, "created", float64(x.Created))
		copyIn.task(id, "notes", x.Notes)
		copyIn.task(id, "due", x.Due)
		copyIn.task(id, "time", x.Time)
		copyIn.task(id, "flag", x.Flag)
		copyIn.task(id, "repeat", x.Repeat)
		copyIn.task(id, "done", float64(x.DoneAt))
		if x.ID == t.ID {
			copyIn.task(id, "parent", "")
			copyIn.task(id, "order", dst.placeOrder(copyIn, id, "", before, after))
		} else {
			copyIn.task(id, "parent", t.ID)
			copyIn.task(id, "order", x.Order)
		}
	}
	if err := a.commit(ctx, dst, copyIn); err != nil {
		return err
	}
	return a.deleteOps(ctx, src, []*Task{t})
}

// deleteOps tombstones tasks and their subtasks.
func (a *App) deleteOps(ctx context.Context, ls *listState, ts []*Task) error {
	b := a.newBatch()
	seen := map[string]bool{}
	for _, t := range ts {
		for _, x := range append([]*Task{t}, ls.subtasksOf(t.ID)...) {
			if !seen[x.ID] {
				seen[x.ID] = true
				b.task(x.ID, "deleted", true)
			}
		}
	}
	return a.commit(ctx, ls, b)
}

// sortedKeys is for deterministic output.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// deleteOp is a tombstone for a key.
func (a *App) deleteOp(b *batch, key string) crdt.MapOp { return b.m.Delete(a.crdtClk, key) }
