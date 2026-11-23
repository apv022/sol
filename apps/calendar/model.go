package calendar

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/apv022/sol/sdk/go/crdt"
)

// A calendar's state is one crdt.Map of string keys (design/apps/CALENDAR.md "Model"). This
// file defines the keys, validates ops arriving from feeds (written by other people, so nothing
// in them is trusted) and folds a Map into Calendar and Event views.

// Modes of the person looking at a calendar.
const (
	modeOwner  = "owner"
	modeWrite  = "write"
	modeRead   = "read"
	modeInvite = "invite" // an event someone invited me to
)

const (
	maxTitle  = 1000
	maxText   = 20000
	maxValue  = 24000 // any single op value, bytes of JSON
	maxAlerts = 8
)

// keyRe admits exactly the keys the app writes; ops with any other key are dropped.
var keyRe = regexp.MustCompile(`^(?:c\.(?:owner|name|color|order|deleted)|g\.[a-z0-9]{10,64}|` +
	`e\.[0-9a-f]{12}\.(?:title|start|end|tz|location|notes|color|uid|alerts|rrule|of|rid|created|deleted|` +
	`x\.(?:\d{8}|\d{8}T\d{6}Z)|a\.[a-z0-9]{10,64})|` +
	`i\.[0-9a-f]{8}\.[0-9a-f]{12}|r\.[0-9a-f]{8}\.[0-9a-f]{12})$`)

// ownerOnly keys may only come from a calendar's owner.
func ownerOnly(key string) bool {
	return strings.HasPrefix(key, "g.") || key == "c.owner" || key == "c.deleted"
}

var palette = []string{"red", "orange", "yellow", "green", "teal", "blue", "indigo", "purple", "pink", "brown", "gray"}

func validColor(s string) bool {
	for _, c := range palette {
		if c == s {
			return true
		}
	}
	return false
}

var responses = map[string]bool{"accepted": true, "declined": true, "tentative": true}

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
	s, isStr := v.(string)
	switch {
	case strings.HasPrefix(op.Key, "g."):
		return isStr && (s == modeRead || s == modeWrite)
	case strings.HasPrefix(op.Key, "i."):
		_, ok := v.(map[string]any)
		return ok
	case strings.HasPrefix(op.Key, "r."):
		return isStr && responses[s]
	case strings.Contains(op.Key, ".x."):
		b, ok := v.(bool)
		return ok && b
	case strings.Contains(op.Key, ".a."):
		return isStr && s == "invited"
	}
	switch field(op.Key) {
	case "title":
		return isStr && len(s) <= maxTitle
	case "notes", "location":
		return isStr && len(s) <= maxText
	case "start", "end", "tz", "color", "uid", "rrule", "of", "rid", "name", "owner":
		return isStr && len(s) <= 1000
	case "order", "created":
		_, ok := v.(float64)
		return ok
	case "deleted":
		_, ok := v.(bool)
		return ok
	case "alerts":
		arr, ok := v.([]any)
		if !ok || len(arr) > maxAlerts {
			return false
		}
		for _, x := range arr {
			n, ok := x.(float64)
			if !ok || n < 0 || n > 40320 || n != float64(int(n)) {
				return false
			}
		}
		return true
	}
	return false
}

// field is the last dot-separated part of a key.
func field(key string) string { return key[strings.LastIndexByte(key, '.')+1:] }

// Attendee is one invited person and their answer.
type Attendee struct {
	Person string `json:"person"`
	Name   string `json:"name"`
	Status string `json:"status"` // invited, accepted, declined, tentative
}

// Member is one person a calendar is shared with.
type Member struct {
	Person string `json:"person"`
	Name   string `json:"name"`
	Mode   string `json:"mode"`
}

// Calendar is a calendar as the UI and the actions see it.
type Calendar struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Color     string   `json:"color"`
	Order     float64  `json:"order"`
	Owner     string   `json:"owner"`
	OwnerName string   `json:"owner_name"`
	Mode      string   `json:"mode"`
	Shared    bool     `json:"shared"`
	Shown     bool     `json:"shown"`
	Members   []Member `json:"members,omitempty"`
	Events    int      `json:"events"`
}

// Event is one stored event (a series master, a one-off, or an override of one occurrence).
type Event struct {
	ID        string     `json:"id"`
	Calendar  string     `json:"calendar"`
	Title     string     `json:"title"`
	Start     string     `json:"start"`
	End       string     `json:"end"`
	TZ        string     `json:"tz,omitempty"`
	AllDay    bool       `json:"all_day"`
	Location  string     `json:"location,omitempty"`
	Notes     string     `json:"notes,omitempty"`
	Color     string     `json:"color,omitempty"`
	UID       string     `json:"uid,omitempty"`
	Alerts    []int      `json:"alerts,omitempty"`
	RRule     string     `json:"rrule,omitempty"`
	Exdates   []string   `json:"exdates,omitempty"`
	Of        string     `json:"of,omitempty"`
	RID       string     `json:"rid,omitempty"`
	Attendees []Attendee `json:"attendees,omitempty"`
	Created   int64      `json:"created,omitempty"`

	start, end time.Time
	loc        *time.Location
	rule       *Rule
	overrides  map[string]*Event // rid → override (masters only)
	invitees   []string          // person ids from a.<person> keys
}

// calState is a folded calendar.
type calState struct {
	key    string // address of the owner's feed
	id     string
	feed   string
	owner  string
	m      *crdt.Map
	grants map[string]string
	mode   string
	events map[string]*Event // masters and one-offs by id
	all    map[string]*Event // every event, overrides too
	info   Calendar
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

// resolveTimes parses an event's stored start and end, repairing what concurrent writes can
// leave behind: an end that is not after the start gets the default duration. It reports
// false for an event whose start cannot be read.
func (e *Event) resolveTimes(local *time.Location) bool {
	loc, err := loadZone(e.TZ, local)
	if err != nil {
		loc = local
	}
	e.loc = loc
	if d, ok := parseDate(e.Start); ok {
		e.AllDay, e.TZ = true, ""
		e.start = d
		if ed, ok := parseDate(e.End); ok && ed.After(d) {
			e.end = ed
		} else {
			e.end = d.AddDate(0, 0, 1)
			e.End = e.end.Format(dateLayout)
		}
		return true
	}
	t, err := time.Parse(time.RFC3339, e.Start)
	if err != nil {
		return false
	}
	e.start = t.In(loc)
	if et, err := time.Parse(time.RFC3339, e.End); err == nil && !et.Before(t) {
		e.end = et.In(loc)
	} else {
		e.end = e.start.Add(time.Hour)
		e.End = e.end.Format(time.RFC3339)
	}
	return true
}

// build derives the calendar's events and metadata from its map.
func (cs *calState) build(local *time.Location) {
	m := cs.m
	cs.info = Calendar{ID: cs.id, Name: str(m, "c.name"), Color: str(m, "c.color"), Order: num(m, "c.order"), Owner: cs.owner, Mode: cs.mode}
	if cs.info.Name == "" {
		cs.info.Name = "Untitled"
	}
	if !validColor(cs.info.Color) {
		cs.info.Color = "blue"
	}
	ids := map[string]bool{}
	for _, k := range m.Keys() {
		if strings.HasPrefix(k, "e.") && len(k) > 15 {
			ids[k[2:14]] = true
		}
	}
	cs.all = map[string]*Event{}
	for id := range ids {
		p := "e." + id + "."
		if _, ok := m.Get(p + "title"); !ok || flagOf(m, p+"deleted") {
			continue
		}
		e := &Event{ID: id, Calendar: cs.id, Title: str(m, p+"title"), Start: str(m, p+"start"), End: str(m, p+"end"),
			TZ: str(m, p+"tz"), Location: str(m, p+"location"), Notes: str(m, p+"notes"), Color: str(m, p+"color"),
			UID: str(m, p+"uid"), RRule: str(m, p+"rrule"), Of: str(m, p+"of"), RID: str(m, p+"rid"), Created: int64(num(m, p+"created"))}
		if !validColor(e.Color) {
			e.Color = ""
		}
		if raw, ok := m.Get(p + "alerts"); ok {
			_ = json.Unmarshal(raw, &e.Alerts)
		}
		if !e.resolveTimes(local) {
			continue
		}
		if e.RRule != "" && e.Of == "" {
			if r, err := ParseRule(e.RRule); err == nil {
				e.rule = r
			} else {
				e.RRule = ""
			}
		}
		cs.all[id] = e
	}
	// Exceptions and invitees live under per-item keys.
	for _, k := range m.Keys() {
		if !strings.HasPrefix(k, "e.") || len(k) < 17 {
			continue
		}
		e := cs.all[k[2:14]]
		if e == nil {
			continue
		}
		rest := k[15:]
		switch {
		case strings.HasPrefix(rest, "x."):
			e.Exdates = append(e.Exdates, rest[2:])
		case strings.HasPrefix(rest, "a."):
			e.invitees = append(e.invitees, rest[2:])
		}
	}
	cs.events = map[string]*Event{}
	for id, e := range cs.all {
		if e.Of == "" {
			e.overrides = map[string]*Event{}
			cs.events[id] = e
		}
	}
	for _, e := range cs.all {
		if e.Of == "" {
			continue
		}
		if master := cs.events[e.Of]; master != nil && master.rule != nil && validRID(e.RID) {
			e.RRule = ""
			master.overrides[e.RID] = e
		}
	}
	for _, e := range cs.all {
		sort.Strings(e.Exdates)
		sort.Strings(e.invitees)
	}
	cs.info.Shared = len(cs.grants) > 0
	cs.info.Events = len(cs.events)
}

// rangeOverlaps reports whether [s, e) meets [from, to); a zero-length event at from counts.
func rangeOverlaps(s, e, from, to time.Time) bool {
	return s.Before(to) && (e.After(from) || (s.Equal(e) && !s.Before(from)))
}

// occurrence is one concrete happening of an event.
type occurrence struct {
	ev         *Event // the event whose fields to show (an override, or the master)
	master     *Event // the series (nil for one-offs)
	rid        string
	start, end time.Time
}

// window is a range of time as instants (for timed events) and as the viewer's dates (UTC
// midnights, for all-day events, which are the same days in every zone).
type window struct {
	from, to   time.Time
	dfrom, dto time.Time
}

// newWindow makes the window [from, to) seen from zone loc.
func newWindow(from, to time.Time, loc *time.Location) window {
	day := func(t time.Time, up bool) time.Time {
		l := t.In(loc)
		d := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
		if up && (l.Hour() != 0 || l.Minute() != 0 || l.Second() != 0) {
			d = d.AddDate(0, 0, 1)
		}
		return d
	}
	return window{from: from, to: to, dfrom: day(from, false), dto: day(to, true)}
}

// occurrences expands one master event into the window.
func (e *Event) occurrences(w window) []occurrence {
	from, to := w.from, w.to
	if e.AllDay {
		from, to = w.dfrom, w.dto
	}
	if e.rule == nil {
		if rangeOverlaps(e.start, e.end, from, to) {
			return []occurrence{{ev: e, start: e.start, end: e.end}}
		}
		return nil
	}
	dur := e.end.Sub(e.start)
	var out []occurrence
	ex := map[string]bool{}
	for _, x := range e.Exdates {
		ex[x] = true
	}
	// Look back by the duration so occurrences that started before from but still run are kept.
	for _, st := range e.rule.Expand(e.start, e.loc, e.AllDay, from.Add(-dur-time.Second), to, 0) {
		rid := ridOf(st, e.AllDay)
		if ex[rid] {
			continue
		}
		if ov := e.overrides[rid]; ov != nil {
			continue // shown on its own below, wherever it moved to
		}
		var en time.Time
		if e.AllDay {
			days := int(dur.Hours()/24 + 0.5)
			en = st.AddDate(0, 0, max(days, 1))
		} else {
			en = st.Add(dur)
		}
		if rangeOverlaps(st, en, from, to) {
			out = append(out, occurrence{ev: e, master: e, rid: rid, start: st, end: en})
		}
	}
	for rid, ov := range e.overrides {
		if ex[rid] {
			continue
		}
		f, t := w.from, w.to
		if ov.AllDay {
			f, t = w.dfrom, w.dto
		}
		if rangeOverlaps(ov.start, ov.end, f, t) {
			out = append(out, occurrence{ev: ov, master: e, rid: rid, start: ov.start, end: ov.end})
		}
	}
	return out
}
