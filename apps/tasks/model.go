package tasks

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/apv022/sol/sdk/go/crdt"
)

// The list state is one crdt.Map of string keys (design/apps/TASKS.md). This file defines the
// keys, validates ops that arrive from feeds (a feed is written by other people, so nothing in
// it is trusted), and folds a Map into Task and list views.

// Modes of the person looking at a list.
const (
	modeOwner = "owner"
	modeWrite = "write"
	modeRead  = "read"
)

const (
	maxTitle = 1000
	maxNotes = 20000
	maxValue = 24000 // any single op value, bytes of JSON
)

// keyRe admits exactly the keys the app writes; ops with any other key are dropped.
var keyRe = regexp.MustCompile(`^(?:l\.(?:owner|name|color|icon|order|deleted)|g\.[a-z0-9]{10,64}|t\.[0-9a-f]{8}\.(?:title|notes|done|due|time|flag|repeat|parent|order|deleted|created))$`)

// ownerOnly keys may only come from the list's owner: who it is shared with, and whose list it is.
func ownerOnly(key string) bool {
	return strings.HasPrefix(key, "g.") || key == "l.owner" || key == "l.deleted"
}

// validOp reports whether an op from a feed is well formed (not whether its writer may write it).
func validOp(op crdt.MapOp, maxWall int64) bool {
	if !keyRe.MatchString(op.Key) || op.Stamp.IsZero() || op.Stamp.Device == "" || op.Stamp.Wall > maxWall {
		return false
	}
	if op.Delete {
		return true
	}
	if len(op.Value) > maxValue {
		return false
	}
	var v any
	if json.Unmarshal(op.Value, &v) != nil {
		return false
	}
	switch field(op.Key) {
	case "title", "notes", "due", "time", "repeat", "parent", "name", "color", "icon", "owner":
		s, ok := v.(string)
		return ok && (field(op.Key) != "title" || len(s) <= maxTitle) && (field(op.Key) != "notes" || len(s) <= maxNotes)
	case "done", "order", "created":
		_, ok := v.(float64)
		return ok
	case "flag", "deleted":
		_, ok := v.(bool)
		return ok
	}
	if strings.HasPrefix(op.Key, "g.") {
		s, ok := v.(string)
		return ok && (s == modeRead || s == modeWrite)
	}
	return false
}

// field is the last dot-separated part of a key.
func field(key string) string { return key[strings.LastIndexByte(key, '.')+1:] }

// Task is one task as the UI and the actions see it.
type Task struct {
	ID        string  `json:"id"`
	List      string  `json:"list"`
	ListName  string  `json:"list_name"`
	ListColor string  `json:"list_color"`
	Title     string  `json:"title"`
	Notes     string  `json:"notes"`
	Done      bool    `json:"done"`
	DoneAt    int64   `json:"done_at"`
	Due       string  `json:"due"`
	Time      string  `json:"time"`
	Flag      bool    `json:"flag"`
	Repeat    string  `json:"repeat"`
	Parent    string  `json:"parent"`
	Order     float64 `json:"order"`
	Created   int64   `json:"created"`
	Subtasks  int     `json:"subtasks"`
	SubDone   int     `json:"subtasks_done"`
	Overdue   bool    `json:"overdue"`
}

// Member is one person a list is shared with.
type Member struct {
	Person string `json:"person"`
	Name   string `json:"name"`
	Mode   string `json:"mode"`
}

// List is a list as the UI and the actions see it.
type List struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Color     string   `json:"color"`
	Icon      string   `json:"icon"`
	Order     float64  `json:"order"`
	Owner     string   `json:"owner"`
	OwnerName string   `json:"owner_name"`
	Mode      string   `json:"mode"`
	Shared    bool     `json:"shared"`
	Members   []Member `json:"members,omitempty"`
	Count     int      `json:"count"`
}

// listState is a folded list.
type listState struct {
	key    string // address of the owner's feed
	id     string
	feed   string
	owner  string
	m      *crdt.Map
	grants map[string]string
	mode   string // what I may do
	tasks  []*Task
	byID   map[string]*Task
	info   List
}

func str(m *crdt.Map, key string) string {
	var s string
	if raw, ok := m.Get(key); ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func num(m *crdt.Map, key string) float64 {
	var f float64
	if raw, ok := m.Get(key); ok {
		_ = json.Unmarshal(raw, &f)
	}
	return f
}

func flagOf(m *crdt.Map, key string) bool {
	var b bool
	if raw, ok := m.Get(key); ok {
		_ = json.Unmarshal(raw, &b)
	}
	return b
}

// build derives the list's tasks and metadata from its map.
func (ls *listState) build() {
	m := ls.m
	ls.info = List{ID: ls.id, Name: str(m, "l.name"), Color: str(m, "l.color"), Icon: str(m, "l.icon"),
		Order: num(m, "l.order"), Owner: ls.owner, Mode: ls.mode}
	if ls.info.Name == "" {
		ls.info.Name = "Untitled"
	}
	ids := map[string]bool{}
	for _, k := range m.Keys() {
		if strings.HasPrefix(k, "t.") {
			ids[k[2:10]] = true
		}
	}
	ls.byID = map[string]*Task{}
	for id := range ids {
		p := "t." + id + "."
		if _, ok := m.Get(p + "title"); !ok || flagOf(m, p+"deleted") {
			continue
		}
		t := &Task{ID: id, List: ls.id, ListName: ls.info.Name, ListColor: ls.info.Color,
			Title: str(m, p+"title"), Notes: str(m, p+"notes"), DoneAt: int64(num(m, p+"done")),
			Due: str(m, p+"due"), Time: str(m, p+"time"), Flag: flagOf(m, p+"flag"), Repeat: str(m, p+"repeat"),
			Parent: str(m, p+"parent"), Order: num(m, p+"order"), Created: int64(num(m, p+"created"))}
		t.Done = t.DoneAt > 0
		ls.byID[id] = t
	}
	ls.tasks = ls.tasks[:0]
	for _, t := range ls.byID {
		if t.Parent != "" {
			// Subtasks are one level deep: a task whose parent is gone, or is itself a subtask, is shown top level.
			if p, ok := ls.byID[t.Parent]; ok && p.Parent == "" && t.Parent != t.ID {
				p.Subtasks++
				if t.Done {
					p.SubDone++
				}
				ls.tasks = append(ls.tasks, t)
				continue
			}
			t.Parent = ""
		}
		ls.tasks = append(ls.tasks, t)
	}
	sort.Slice(ls.tasks, func(i, j int) bool { return taskLess(ls.tasks[i], ls.tasks[j]) })
	ls.info.Shared = len(ls.grants) > 0
	for _, t := range ls.tasks {
		if !t.Done && t.Parent == "" {
			ls.info.Count++
		}
	}
	if ls.info.Color == "" {
		ls.info.Color = "blue"
	}
	if ls.info.Icon == "" {
		ls.info.Icon = "list"
	}
	for _, t := range ls.tasks {
		t.ListColor = ls.info.Color
	}
}

func taskLess(a, b *Task) bool {
	if a.Order != b.Order {
		return a.Order < b.Order
	}
	return a.ID < b.ID
}
