package calendar

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/apv022/sol/sdk/go/crdt"
	"github.com/apv022/sol/sdk/go/sol"
)

// Everything that changes a calendar goes through a batch: the keys to set, stamped by the
// device's clock, appended as one feed entry so a change shows up all at once on every device.

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
		panic("calendar: unmarshalable op value: " + err.Error()) // only plain values reach here
	}
	b.ops = append(b.ops, op)
}

func (b *batch) del(key string) { b.ops = append(b.ops, b.m.Delete(b.a.crdtClk, key)) }

func (b *batch) ev(id, f string, v any) { b.set("e."+id+"."+f, v) }

func randHex(n int) string {
	x := make([]byte, n/2)
	_, _ = rand.Read(x)
	return hex.EncodeToString(x)
}

// deriveID is a function of its parts, so two devices making the same override, split or
// import produce the same event and converge.
func deriveID(parts ...string) string {
	s := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(s[:6])
}

// commit appends a batch to my copy of the calendar's feed: the owner's own feed, or for a
// calendar shared with me for writing, my co-author feed of the same name (shared back to the
// owner first, API.md §5).
func (a *App) commit(ctx context.Context, cs *calState, b *batch) error {
	if cs.mode == modeRead {
		return sol.Errorf("forbidden", "this calendar is shared with you read-only")
	}
	if len(b.ops) == 0 {
		return nil
	}
	if cs.owner != a.me {
		if err := a.shareOnce(ctx, cs.feed, cs.owner); err != nil {
			return err
		}
	}
	if err := a.appendOps(ctx, cs.feed, b.ops); err != nil {
		return err
	}
	a.afterChange(ctx)
	return nil
}

// createCalendar makes a new calendar owned by me and returns its id.
func (a *App) createCalendar(ctx context.Context, name, color string) (string, error) {
	id := randHex(8)
	var order float64
	for _, cs := range a.view() {
		order = max(order, cs.info.Order)
	}
	b := a.newBatch()
	b.set("c.owner", a.me)
	b.set("c.name", name)
	b.set("c.color", color)
	b.set("c.order", order+1024)
	if err := a.appendOps(ctx, "cal."+id, b.ops); err != nil {
		return "", err
	}
	a.afterChange(ctx)
	return id, nil
}

// fields are the content of an event, resolved.
type fields struct {
	Title, Location, Notes, Color, RRule, TZ string
	Start, End                               time.Time
	AllDay                                   bool
	Alerts                                   []int
}

func fieldsOf(e *Event) fields {
	return fields{Title: e.Title, Location: e.Location, Notes: e.Notes, Color: e.Color, RRule: e.RRule, TZ: e.TZ,
		Start: e.start, End: e.end, AllDay: e.AllDay, Alerts: append([]int(nil), e.Alerts...)}
}

func (a *App) zoneOf(f fields) *time.Location {
	l, err := loadZone(f.TZ, a.loc)
	if err != nil {
		return a.loc
	}
	return l
}

func (a *App) timeStr(f fields, t time.Time) string {
	if f.AllDay {
		return t.Format(dateLayout)
	}
	return t.In(a.zoneOf(f)).Format(time.RFC3339)
}

// check validates resolved fields.
func (f *fields) check() error {
	if strings.TrimSpace(f.Title) == "" {
		return bad("an event needs a title")
	}
	if len(f.Title) > maxTitle || len(f.Notes) > maxText || len(f.Location) > maxText {
		return bad("title, notes or location too long")
	}
	if f.Color != "" && !validColor(f.Color) {
		return bad("unknown color %q (one of %s)", f.Color, strings.Join(palette, ", "))
	}
	if f.End.Before(f.Start) {
		return bad("the event ends before it starts")
	}
	if f.AllDay && !f.End.After(f.Start) {
		return bad("an all-day event lasts at least a day")
	}
	if len(f.Alerts) > 0 {
		sort.Ints(f.Alerts)
		u := f.Alerts[:1]
		for _, m := range f.Alerts[1:] {
			if m != u[len(u)-1] {
				u = append(u, m)
			}
		}
		f.Alerts = u
	}
	if len(f.Alerts) > maxAlerts {
		return bad("at most %d alerts", maxAlerts)
	}
	for _, m := range f.Alerts {
		if m < 0 || m > 40320 {
			return bad("alerts are minutes before the start, 0 to 40320")
		}
	}
	if f.RRule != "" {
		r, err := ParseRule(f.RRule)
		if err != nil {
			return bad("repeat: %v", err)
		}
		f.RRule = r.String()
	}
	return nil
}

// write puts the fields of event id into b: all of them, or only those that differ from old.
func (b *batch) write(id string, f fields, old *fields) {
	a := b.a
	put := func(name string, v, was any) {
		if old == nil || !reflect.DeepEqual(v, was) {
			b.ev(id, name, v)
		}
	}
	var o fields
	if old != nil {
		o = *old
	}
	put("title", f.Title, o.Title)
	if old == nil || !f.Start.Equal(o.Start) || f.AllDay != o.AllDay || f.TZ != o.TZ {
		b.ev(id, "start", a.timeStr(f, f.Start))
	}
	if old == nil || !f.End.Equal(o.End) || f.AllDay != o.AllDay || f.TZ != o.TZ {
		b.ev(id, "end", a.timeStr(f, f.End))
	}
	tz := f.TZ
	if f.AllDay {
		tz = ""
	}
	otz := o.TZ
	if o.AllDay {
		otz = ""
	}
	if old == nil || tz != otz {
		b.ev(id, "tz", tz)
	}
	put("location", f.Location, o.Location)
	put("notes", f.Notes, o.Notes)
	put("color", f.Color, o.Color)
	put("rrule", f.RRule, o.RRule)
	al := f.Alerts
	if al == nil {
		al = []int{}
	}
	oal := o.Alerts
	if oal == nil {
		oal = []int{}
	}
	put("alerts", al, oal)
}

// patch is what an update asks to change; nil means unchanged.
type patch struct {
	Title, Location, Notes, Color, RRule, Zone *string
	Start, End                                 *time.Time
	AllDay                                     *bool
	Alerts                                     *[]int
}

// apply changes f by p. A new start without a new end keeps the duration; switching to all-day
// keeps the dates, switching back starts at 09:00 for an hour.
func (a *App) apply(f fields, p patch) fields {
	if p.Title != nil {
		f.Title = *p.Title
	}
	if p.Location != nil {
		f.Location = *p.Location
	}
	if p.Notes != nil {
		f.Notes = *p.Notes
	}
	if p.Color != nil {
		f.Color = *p.Color
	}
	if p.RRule != nil {
		f.RRule = *p.RRule
	}
	if p.Alerts != nil {
		f.Alerts = *p.Alerts
	}
	if p.Zone != nil && *p.Zone != f.TZ && !f.AllDay {
		f.TZ = *p.Zone
	}
	if p.AllDay != nil && *p.AllDay != f.AllDay {
		loc := a.zoneOf(f)
		if *p.AllDay {
			s := f.Start.In(loc)
			en := f.End.In(loc)
			f.Start = time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, time.UTC)
			f.End = time.Date(en.Year(), en.Month(), en.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
			if en.Hour() == 0 && en.Minute() == 0 && en.After(s) {
				f.End = f.End.AddDate(0, 0, -1)
			}
			f.AllDay = true
		} else {
			if f.TZ == "" {
				f.TZ = a.zone
			}
			loc = a.zoneOf(f)
			f.Start = wallTime(f.Start.Year(), f.Start.Month(), f.Start.Day(), 9, 0, 0, loc)
			f.End = f.Start.Add(time.Hour)
			f.AllDay = false
		}
	}
	dur := f.End.Sub(f.Start)
	if p.Start != nil {
		f.Start = *p.Start
		if f.AllDay {
			f.Start = time.Date(f.Start.Year(), f.Start.Month(), f.Start.Day(), 0, 0, 0, 0, time.UTC)
		}
		if p.End == nil {
			f.End = f.Start.Add(dur)
		}
	}
	if p.End != nil {
		f.End = *p.End
		if f.AllDay {
			f.End = time.Date(f.End.Year(), f.End.Month(), f.End.Day(), 0, 0, 0, 0, time.UTC)
		}
	}
	return f
}

// shiftRID moves a recurrence id by d (whole days for all-day series).
func shiftRID(rid string, d time.Duration) string {
	t, allDay, ok := parseRID(rid)
	if !ok || d == 0 {
		return rid
	}
	return ridOf(t.Add(d), allDay)
}

// occAt returns the occurrence of series e named by rid.
func occAt(e *Event, rid string) (start, end time.Time, ok bool) {
	t, allDay, valid := parseRID(rid)
	if !valid || allDay != e.AllDay || e.rule == nil {
		return time.Time{}, time.Time{}, false
	}
	dur := e.end.Sub(e.start)
	from := t.Add(-time.Second)
	for _, st := range e.rule.Expand(e.start, e.loc, e.AllDay, from, t.Add(time.Second), 2) {
		if ridOf(st, e.AllDay) == rid {
			return st, st.Add(dur), true
		}
	}
	return time.Time{}, time.Time{}, false
}

// untilBefore is the rule of e ended just before the occurrence at t.
func untilBefore(e *Event, t time.Time) string {
	r := *e.rule
	r.Count = 0
	r.UntilFloat = false
	if e.AllDay {
		r.Until, r.UntilDate = t.AddDate(0, 0, -1), true
	} else {
		r.Until, r.UntilDate = t.Add(-time.Second).UTC(), false
	}
	return r.String()
}

// editPlan is what an update or a move resolved to.
type editPlan struct {
	b       *batch
	id      string // the event that holds the result
	summary string
}

// planUpdate builds the batch for changing event e (a master or one-off) of cs: the whole series
// (span all), one occurrence (this) or it and every later one (following).
func (a *App) planUpdate(cs *calState, e *Event, rid, span string, p patch) (*editPlan, error) {
	b := a.newBatch()
	if e.rule == nil || (rid == "" && span != "this" && span != "following") {
		old := fieldsOf(e)
		f := a.apply(old, p)
		if err := f.check(); err != nil {
			return nil, err
		}
		b.write(e.ID, f, &old)
		if e.rule != nil && (!f.Start.Equal(old.Start) || f.AllDay != old.AllDay) {
			a.reKeyExceptions(b, e, e.ID, f.Start.Sub(old.Start), "", f.AllDay != old.AllDay)
		}
		return &editPlan{b, e.ID, "change " + e.Title}, nil
	}
	if rid == "" {
		return nil, bad("say which occurrence (at) for span %s", span)
	}
	ov := e.overrides[rid]
	occStart, occEnd, ok := occAt(e, rid)
	if ov == nil && !ok {
		return nil, missing("occurrence " + rid + " of " + e.Title)
	}
	if ov != nil {
		occStart, occEnd = ov.start, ov.end
	}
	switch span {
	case "", "this":
		if ov != nil {
			old := fieldsOf(ov)
			f := a.apply(old, p)
			f.RRule = ""
			if err := f.check(); err != nil {
				return nil, err
			}
			b.write(ov.ID, f, &old)
			return &editPlan{b, ov.ID, "change one occurrence of " + e.Title}, nil
		}
		base := fieldsOf(e)
		base.Start, base.End, base.RRule = occStart, occEnd, ""
		f := a.apply(base, p)
		f.RRule = ""
		if err := f.check(); err != nil {
			return nil, err
		}
		oid := deriveID(e.ID, rid)
		b.write(oid, f, nil)
		b.ev(oid, "of", e.ID)
		b.ev(oid, "rid", rid)
		b.ev(oid, "deleted", false)
		b.ev(oid, "created", float64(a.clock.Now().UnixMilli()))
		return &editPlan{b, oid, "change one occurrence of " + e.Title}, nil
	case "all":
		old := fieldsOf(e)
		master := old
		// Changes to times are relative to the occurrence the person looked at.
		occ := old
		occ.Start, occ.End = occStart, occEnd
		f := a.apply(occ, p)
		master.Title, master.Location, master.Notes, master.Color, master.Alerts, master.TZ = f.Title, f.Location, f.Notes, f.Color, f.Alerts, f.TZ
		if p.RRule != nil {
			master.RRule = f.RRule
		}
		shift := f.Start.Sub(occStart)
		if f.AllDay != old.AllDay {
			master.AllDay = f.AllDay
			master.Start = f.Start.Add(old.Start.Sub(occStart))
			if f.AllDay {
				master.Start = time.Date(old.Start.Year(), old.Start.Month(), old.Start.Day(), 0, 0, 0, 0, time.UTC)
			} else {
				master.Start = wallTime(old.Start.Year(), old.Start.Month(), old.Start.Day(), f.Start.In(a.zoneOf(f)).Hour(), f.Start.In(a.zoneOf(f)).Minute(), 0, a.zoneOf(f))
			}
		} else {
			master.Start = old.Start.Add(shift)
		}
		master.End = master.Start.Add(f.End.Sub(f.Start))
		if err := master.check(); err != nil {
			return nil, err
		}
		b.write(e.ID, master, &old)
		if shift != 0 || f.AllDay != old.AllDay {
			a.reKeyExceptions(b, e, e.ID, shift, "", f.AllDay != old.AllDay)
		}
		return &editPlan{b, e.ID, "change every occurrence of " + e.Title}, nil
	case "following":
		if !occStart.After(e.start) && rid == ridOf(e.start, e.AllDay) {
			return a.planUpdate(cs, e, rid, "all", p)
		}
		base := fieldsOf(e)
		base.Start, base.End = occStart, occEnd
		f := a.apply(base, p)
		if p.RRule == nil {
			f.RRule = e.RRule
			if e.rule.Count > 0 {
				left := e.rule.Count - e.rule.countBefore(e.start, e.loc, e.AllDay, occStart)
				r := *e.rule
				r.Count = max(left, 1)
				f.RRule = r.String()
			}
		}
		if err := f.check(); err != nil {
			return nil, err
		}
		nid := deriveID(e.ID, "following", rid)
		b.write(nid, f, nil)
		b.ev(nid, "deleted", false)
		b.ev(nid, "created", float64(a.clock.Now().UnixMilli()))
		if e.UID != "" {
			b.ev(nid, "uid", e.UID+"-"+rid)
		}
		for _, p := range e.invitees {
			b.ev(nid, "a."+p, "invited")
		}
		b.ev(e.ID, "rrule", untilBefore(e, occStart))
		a.reKeyExceptions(b, e, nid, f.Start.Sub(occStart), rid, f.AllDay != e.AllDay)
		return &editPlan{b, nid, "change " + e.Title + " from " + rid + " on"}, nil
	}
	return nil, bad("span must be this, following or all")
}

// reKeyExceptions moves exclusions and overrides of series e (those at or after from, or all
// when from is empty) to series to, shifted by d; when the series changes between timed and
// all-day they cannot match any more and are dropped.
func (a *App) reKeyExceptions(b *batch, e *Event, to string, d time.Duration, from string, drop bool) {
	after := func(rid string) bool {
		if from == "" {
			return true
		}
		t, _, _ := parseRID(rid)
		f, _, _ := parseRID(from)
		return !t.Before(f)
	}
	for _, x := range e.Exdates {
		if !after(x) {
			continue
		}
		nx := shiftRID(x, d)
		if to == e.ID && nx == x && !drop {
			continue
		}
		b.del("e." + e.ID + ".x." + x)
		if !drop {
			b.ev(to, "x."+nx, true)
		}
	}
	for rid, ov := range e.overrides {
		if !after(rid) {
			continue
		}
		nr := shiftRID(rid, d)
		if drop {
			b.ev(ov.ID, "deleted", true)
			continue
		}
		if to != e.ID {
			b.ev(ov.ID, "of", to)
		}
		if nr != rid {
			b.ev(ov.ID, "rid", nr)
		}
	}
}

// planDelete builds the batch that deletes event e: all of it, one occurrence or the rest.
func (a *App) planDelete(e *Event, rid, span string) (*batch, string, error) {
	b := a.newBatch()
	whole := func() {
		b.ev(e.ID, "deleted", true)
		for _, ov := range e.overrides {
			b.ev(ov.ID, "deleted", true)
		}
	}
	if e.rule == nil || span == "all" || (rid == "" && span == "") {
		whole()
		return b, "delete " + e.Title, nil
	}
	if rid == "" {
		return nil, "", bad("say which occurrence (at) for span %s", span)
	}
	ov := e.overrides[rid]
	occStart, _, ok := occAt(e, rid)
	if ov == nil && !ok {
		return nil, "", missing("occurrence " + rid + " of " + e.Title)
	}
	switch span {
	case "", "this":
		b.ev(e.ID, "x."+rid, true)
		if ov != nil {
			b.ev(ov.ID, "deleted", true)
		}
		return b, "delete one occurrence of " + e.Title, nil
	case "following":
		t, _, _ := parseRID(rid)
		if ok {
			t = occStart
		}
		if !t.After(e.start) {
			whole()
			return b, "delete " + e.Title, nil
		}
		b.ev(e.ID, "rrule", untilBefore(e, t))
		for r, o := range e.overrides {
			if r >= rid {
				b.ev(o.ID, "deleted", true)
			}
		}
		return b, "delete " + e.Title + " from " + rid + " on", nil
	}
	return nil, "", bad("span must be this, following or all")
}

// copyEvent writes event e (with its exceptions, overrides and invitees) into b under the same id.
func (a *App) copyEvent(b *batch, e *Event) {
	f := fieldsOf(e)
	b.write(e.ID, f, nil)
	b.ev(e.ID, "deleted", false)
	b.ev(e.ID, "created", float64(e.Created))
	if e.UID != "" {
		b.ev(e.ID, "uid", e.UID)
	}
	for _, x := range e.Exdates {
		b.ev(e.ID, "x."+x, true)
	}
	for _, p := range e.invitees {
		b.ev(e.ID, "a."+p, "invited")
	}
	for rid, ov := range e.overrides {
		b.write(ov.ID, fieldsOf(ov), nil)
		b.ev(ov.ID, "of", e.ID)
		b.ev(ov.ID, "rid", rid)
		b.ev(ov.ID, "deleted", false)
	}
}
