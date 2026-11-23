package calendar

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

// Every action reads the feeds that have news first (a.fresh), so what it returns and what it
// refuses are current. Every write takes a dry run: it resolves and validates everything exactly
// as the real call would and then only describes the change. Reads take a zone and return times
// in it, so callers never do zone math.

func (a *App) registerActions(act *sol.Actions) {
	h := func(name string, f func(ctx context.Context, c sol.Call) (any, error)) {
		act.Handle(name, func(ctx context.Context, c sol.Call) (any, error) {
			if !c.Caller.Self {
				return nil, sol.Errorf("forbidden", "Calendar is for my own devices and agents only")
			}
			if err := a.fresh(ctx); err != nil {
				return nil, err
			}
			return f(ctx, c)
		})
	}
	h("calendar.list_calendars", a.actListCalendars)
	h("calendar.create_calendar", a.actCreateCalendar)
	h("calendar.update_calendar", a.actUpdateCalendar)
	h("calendar.delete_calendar", a.actDeleteCalendar)
	h("calendar.agenda", a.actAgenda)
	h("calendar.get", a.actGet)
	h("calendar.add", a.actAdd)
	h("calendar.update", a.actUpdate)
	h("calendar.move", a.actMove)
	h("calendar.delete", a.actDelete)
	h("calendar.search", a.actSearch)
	h("calendar.invite", a.actInvite)
	h("calendar.invitations", a.actInvitations)
	h("calendar.respond", a.actRespond)
	h("calendar.share", a.actShare)
	h("calendar.export", a.actExport)
	h("calendar.import", a.actImport)
	h("calendar.snapshot", a.actSnapshot)
}

// fresh folds whatever feed entries the events stream has not delivered yet.
func (a *App) fresh(ctx context.Context) error {
	if a.crdtClk == nil {
		return sol.Errorf("unavailable", "Calendar has not started")
	}
	return a.sync(ctx)
}

func dry(would any) map[string]any { return map[string]any{"dry_run": true, "would": would} }

func readOnly() error { return sol.Errorf("forbidden", "this calendar is shared with you read-only") }

// invitationsID names the pseudo calendar that holds events people invited me to.
const invitationsID = "invitations"

// ---------- lookups ----------

func (a *App) findCal(ref string) (*calState, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, bad("which calendar?")
	}
	cals := a.view()
	for _, cs := range cals {
		if cs.id == ref {
			return cs, nil
		}
	}
	var hit *calState
	for _, cs := range cals {
		if strings.EqualFold(cs.info.Name, ref) {
			if hit != nil {
				return nil, bad("more than one calendar is named %q: use its id", ref)
			}
			hit = cs
		}
	}
	if hit == nil {
		return nil, missing("calendar " + ref)
	}
	return hit, nil
}

// writableDefault is where an event lands when no calendar is named: the first shown calendar I
// may write to, else any I may write to.
func (a *App) writableDefault() *calState {
	var any *calState
	for _, cs := range a.view() {
		if cs.mode == modeRead {
			continue
		}
		if cs.info.Shown {
			return cs
		}
		if any == nil {
			any = cs
		}
	}
	return any
}

// findEvent resolves an event reference: "<id>", "<id>@<rid>" or an override's id (which names
// its occurrence of the master). at, when set, names the occurrence instead.
func (a *App) findEvent(ref, at string) (*calState, *Event, string, error) {
	id, rid, _ := strings.Cut(strings.TrimSpace(ref), "@")
	if at != "" {
		rid = at
	}
	if rid != "" && !validRID(rid) {
		return nil, nil, "", bad("%q is not an occurrence (use 20261124T090000Z or 20261124)", rid)
	}
	for _, cs := range a.view() {
		e := cs.all[id]
		if e == nil {
			continue
		}
		if e.Of != "" {
			if m := cs.events[e.Of]; m != nil && m.overrides[e.RID] == e {
				return cs, m, e.RID, nil
			}
		}
		if e.rule == nil {
			rid = ""
		}
		return cs, e, rid, nil
	}
	return nil, nil, "", missing("event " + id)
}

func (a *App) zoneLoc(name string) (*time.Location, string, error) {
	if strings.TrimSpace(name) == "" {
		return a.loc, a.zone, nil
	}
	l, err := loadZone(name, a.loc)
	if err != nil {
		return nil, "", bad("%v", err)
	}
	return l, name, nil
}

func (a *App) resolvePerson(ctx context.Context, ref string) (sol.Contact, error) {
	ppl, err := a.api.People(ctx)
	if err != nil {
		return sol.Contact{}, err
	}
	for _, p := range ppl {
		if p.ID == ref {
			return p, nil
		}
	}
	for _, p := range ppl {
		if strings.EqualFold(p.Petname, ref) || strings.EqualFold(p.Name, ref) {
			return p, nil
		}
	}
	return sol.Contact{}, missing("person " + ref)
}

// ---------- output ----------

// Occ is one occurrence (or a one-off event) as the actions return it, times in the asked zone.
type Occ struct {
	ID           string     `json:"id"`    // <event> or <event>@<rid>
	Event        string     `json:"event"` // the master (or one-off) event id
	RID          string     `json:"rid,omitempty"`
	Calendar     string     `json:"calendar"`
	CalendarName string     `json:"calendar_name"`
	Color        string     `json:"color"`
	Mode         string     `json:"mode"`
	Title        string     `json:"title"`
	Start        string     `json:"start"`
	End          string     `json:"end"`
	AllDay       bool       `json:"all_day"`
	TZ           string     `json:"tz,omitempty"`
	Location     string     `json:"location,omitempty"`
	Notes        string     `json:"notes,omitempty"`
	Alerts       []int      `json:"alerts"`
	RRule        string     `json:"rrule,omitempty"`
	Repeat       string     `json:"repeat"` // "", daily, weekdays, weekly, biweekly, monthly, yearly or custom
	Recurring    bool       `json:"recurring"`
	Override     bool       `json:"override,omitempty"`
	Attendees    []Attendee `json:"attendees,omitempty"`
	Invite       string     `json:"invite,omitempty"` // invitations: the ref to answer
	Status       string     `json:"status,omitempty"` // invitations: my answer
	From         string     `json:"from_name,omitempty"`
}

// repeatName is the UI's preset for a rule, or "custom".
func repeatName(rule string) string {
	if rule == "" {
		return ""
	}
	for _, n := range []string{"daily", "weekdays", "weekly", "biweekly", "monthly", "yearly"} {
		if shortRule(n) == rule {
			return n
		}
	}
	return "custom"
}

func fmtTime(t time.Time, allDay bool, loc *time.Location) string {
	if allDay {
		return t.Format(dateLayout)
	}
	return t.In(loc).Format(time.RFC3339)
}

func (a *App) occOut(cs *calState, o occurrence, loc *time.Location) Occ {
	ev := o.ev
	master := o.master
	if master == nil {
		master = ev
	}
	color := ev.Color
	if color == "" {
		color = master.Color
	}
	if color == "" {
		color = cs.info.Color
	}
	out := Occ{ID: master.ID, Event: master.ID, RID: o.rid, Calendar: cs.id, CalendarName: cs.info.Name, Color: color, Mode: cs.mode,
		Title: ev.Title, Start: fmtTime(o.start, ev.AllDay, loc), End: fmtTime(o.end, ev.AllDay, loc), AllDay: ev.AllDay, TZ: ev.TZ,
		Location: ev.Location, Notes: ev.Notes, Alerts: ev.Alerts, RRule: master.RRule, Repeat: repeatName(master.RRule),
		Recurring: master.rule != nil, Override: ev != master, Attendees: master.Attendees}
	if out.Alerts == nil {
		out.Alerts = []int{}
	}
	if o.rid != "" {
		out.ID = master.ID + "@" + o.rid
	}
	return out
}

// eventOut is a whole event (a series as its first occurrence).
func (a *App) eventOut(cs *calState, e *Event, loc *time.Location) Occ {
	return a.occOut(cs, occurrence{ev: e, start: e.start, end: e.end}, loc)
}

func (a *App) inviteOut(iv *invite, o occurrence, loc *time.Location) Occ {
	cal := &calState{id: invitationsID, mode: modeInvite, info: Calendar{Name: "Invitations", Color: "gray"}}
	out := a.occOut(cal, o, loc)
	out.ID, out.Event = iv.ref, iv.ref
	if o.rid != "" {
		out.ID = iv.ref + "@" + o.rid
	}
	out.Invite, out.Status = iv.ref, iv.status
	if out.Status == "" {
		out.Status = "invited"
	}
	a.mu.RLock()
	out.From = a.names[iv.from]
	a.mu.RUnlock()
	if iv.snap.Calendar != "" {
		out.CalendarName = iv.snap.Calendar
	}
	return out
}

// occurrencesIn lists every occurrence in [from, to): of the calendars named (ids), or of the
// shown ones when none are, plus invitations I have not declined.
func (a *App) occurrencesIn(from, to time.Time, loc *time.Location, only map[string]bool) []Occ {
	w := newWindow(from, to, loc)
	out := []Occ{}
	for _, cs := range a.view() {
		if only != nil && !only[cs.id] || only == nil && !cs.info.Shown {
			continue
		}
		for _, e := range cs.events {
			for _, o := range e.occurrences(w) {
				out = append(out, a.occOut(cs, o, loc))
			}
		}
	}
	a.mu.RLock()
	hideInv := a.hidden[invitationsID]
	a.mu.RUnlock()
	if only != nil && only[invitationsID] || only == nil && !hideInv {
		for _, iv := range a.invitesView() {
			if iv.status == "declined" {
				continue
			}
			for _, o := range iv.ev.occurrences(w) {
				out = append(out, a.inviteOut(iv, o, loc))
			}
		}
	}
	sortOccs(out)
	return out
}

func sortOccs(out []Occ) {
	key := func(o Occ) string {
		if o.AllDay {
			return o.Start + " " // dates sort before times on the same day
		}
		t, _ := time.Parse(time.RFC3339, o.Start)
		return t.UTC().Format("2006-01-02T15:04:05")
	}
	sort.SliceStable(out, func(i, j int) bool {
		ki, kj := key(out[i]), key(out[j])
		if ki != kj {
			return ki < kj
		}
		if out[i].Title != out[j].Title {
			return out[i].Title < out[j].Title
		}
		return out[i].ID < out[j].ID
	})
}

// rangeIn reads from/to (dates or times) in loc; defaults are today and a week later.
func (a *App) rangeIn(fromS, toS string, loc *time.Location, days int) (time.Time, time.Time, error) {
	now := a.clock.Now().In(loc)
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	parse := func(s string) (time.Time, error) {
		t, allDay, err := parseWhen(s, loc)
		if err != nil {
			return t, bad("%v", err)
		}
		if allDay {
			t = wallTime(t.Year(), t.Month(), t.Day(), 0, 0, 0, loc)
		}
		return t, nil
	}
	var err error
	if fromS != "" {
		if from, err = parse(fromS); err != nil {
			return from, from, err
		}
	}
	to := from.AddDate(0, 0, days)
	if toS != "" {
		if to, err = parse(toS); err != nil {
			return from, to, err
		}
	}
	if !to.After(from) {
		return from, to, bad("to must be after from")
	}
	if to.Sub(from) > 400*24*time.Hour {
		return from, to, bad("ask for at most 400 days at once")
	}
	return from, to, nil
}

func (a *App) calInfos() []Calendar {
	out := []Calendar{}
	for _, cs := range a.view() {
		out = append(out, cs.info)
	}
	return out
}

// ---------- reads ----------

func (a *App) actListCalendars(_ context.Context, _ sol.Call) (any, error) {
	return map[string]any{"calendars": a.calInfos()}, nil
}

func (a *App) actSnapshot(_ context.Context, c sol.Call) (any, error) {
	var in struct{ From, To, Zone string }
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	loc, zone, err := a.zoneLoc(in.Zone)
	if err != nil {
		return nil, err
	}
	from, to, err := a.rangeIn(in.From, in.To, loc, 42)
	if err != nil {
		return nil, err
	}
	occ := a.occurrencesIn(from, to, loc, nil)
	invs := a.invitationsOut(loc)
	a.mu.RLock()
	rev, hideInv := a.rev, a.hidden[invitationsID]
	a.mu.RUnlock()
	if zone == "" {
		zone = a.zone
	}
	return map[string]any{"rev": rev, "today": a.clock.Now().In(loc).Format(dateLayout), "zone": zone,
		"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339),
		"calendars": a.calInfos(), "invitations": invs, "invitations_shown": !hideInv, "occurrences": occ, "palette": palette}, nil
}

func (a *App) actAgenda(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		From, To, Zone string
		Calendars      []string
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	loc, _, err := a.zoneLoc(in.Zone)
	if err != nil {
		return nil, err
	}
	from, to, err := a.rangeIn(in.From, in.To, loc, 7)
	if err != nil {
		return nil, err
	}
	var only map[string]bool
	if len(in.Calendars) > 0 {
		only = map[string]bool{}
		for _, ref := range in.Calendars {
			if ref == invitationsID {
				only[ref] = true
				continue
			}
			cs, err := a.findCal(ref)
			if err != nil {
				return nil, err
			}
			only[cs.id] = true
		}
	}
	return map[string]any{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339), "occurrences": a.occurrencesIn(from, to, loc, only)}, nil
}

func (a *App) actGet(_ context.Context, c sol.Call) (any, error) {
	var in struct{ ID, At, Zone string }
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	loc, _, err := a.zoneLoc(in.Zone)
	if err != nil {
		return nil, err
	}
	if strings.Contains(in.ID, "/") {
		ref, rid, _ := strings.Cut(in.ID, "@")
		for _, iv := range a.invitesView() {
			if iv.ref == ref {
				o := occurrence{ev: iv.ev, start: iv.ev.start, end: iv.ev.end}
				if rid != "" {
					if st, en, ok := occAt(iv.ev, rid); ok {
						o = occurrence{ev: iv.ev, master: iv.ev, rid: rid, start: st, end: en}
					}
					if ov := iv.ev.overrides[rid]; ov != nil {
						o = occurrence{ev: ov, master: iv.ev, rid: rid, start: ov.start, end: ov.end}
					}
				}
				return map[string]any{"event": a.inviteOut(iv, o, loc)}, nil
			}
		}
		return nil, missing("invitation " + ref)
	}
	cs, e, rid, err := a.findEvent(in.ID, in.At)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"event": a.eventOut(cs, e, loc), "calendar": cs.info}
	if rid != "" {
		o, ok := a.occurrenceOf(e, rid)
		if !ok {
			return nil, missing("occurrence " + rid)
		}
		out["occurrence"] = a.occOut(cs, o, loc)
	}
	if len(e.Exdates) > 0 {
		out["exdates"] = e.Exdates
	}
	return out, nil
}

// occurrenceOf is occurrence rid of series e, with its override if it has one.
func (a *App) occurrenceOf(e *Event, rid string) (occurrence, bool) {
	if ov := e.overrides[rid]; ov != nil {
		return occurrence{ev: ov, master: e, rid: rid, start: ov.start, end: ov.end}, true
	}
	st, en, ok := occAt(e, rid)
	if !ok {
		return occurrence{}, false
	}
	if e.AllDay {
		en = st.AddDate(0, 0, max(int(e.end.Sub(e.start).Hours()/24+0.5), 1))
	}
	return occurrence{ev: e, master: e, rid: rid, start: st, end: en}, true
}

func (a *App) actSearch(_ context.Context, c sol.Call) (any, error) {
	var in struct{ Query, Zone string }
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	loc, _, err := a.zoneLoc(in.Zone)
	if err != nil {
		return nil, err
	}
	words := strings.Fields(strings.ToLower(in.Query))
	if len(words) == 0 {
		return nil, bad("search for something")
	}
	out := []Occ{}
	for _, cs := range a.view() {
		for _, e := range cs.events {
			hay := strings.ToLower(e.Title + "\n" + e.Location + "\n" + e.Notes)
			ok := true
			for _, w := range words {
				ok = ok && strings.Contains(hay, w)
			}
			if ok {
				out = append(out, a.eventOut(cs, e, loc))
			}
		}
	}
	sortOccs(out)
	return map[string]any{"events": out}, nil
}

func (a *App) invitationsOut(loc *time.Location) []Occ {
	out := []Occ{}
	for _, iv := range a.invitesView() {
		out = append(out, a.inviteOut(iv, occurrence{ev: iv.ev, start: iv.ev.start, end: iv.ev.end}, loc))
	}
	return out
}

func (a *App) actInvitations(_ context.Context, c sol.Call) (any, error) {
	var in struct{ Zone string }
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	loc, _, err := a.zoneLoc(in.Zone)
	if err != nil {
		return nil, err
	}
	return map[string]any{"invitations": a.invitationsOut(loc)}, nil
}

// ---------- calendars ----------

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func checkCalFields(name, color string, nameRequired bool) error {
	if nameRequired && name == "" {
		return bad("a calendar needs a name")
	}
	if len(name) > 200 {
		return bad("name too long")
	}
	if color != "" && !validColor(color) {
		return bad("color must be one of %s", strings.Join(palette, ", "))
	}
	return nil
}

func (a *App) actCreateCalendar(ctx context.Context, c sol.Call) (any, error) {
	var in struct{ Name, Color string }
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	in.Name = oneLine(in.Name)
	if err := checkCalFields(in.Name, in.Color, true); err != nil {
		return nil, err
	}
	if in.Color == "" {
		in.Color = a.nextColor()
	}
	if c.DryRun {
		return dry(map[string]any{"create_calendar": in.Name, "color": in.Color}), nil
	}
	id, err := a.createCalendar(ctx, in.Name, in.Color)
	if err != nil {
		return nil, err
	}
	cs, err := a.findCal(id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"calendar": cs.info}, nil
}

// nextColor is the first palette colour no calendar of mine uses yet.
func (a *App) nextColor() string {
	used := map[string]bool{}
	for _, cs := range a.view() {
		used[cs.info.Color] = true
	}
	for _, c := range []string{"blue", "red", "green", "orange", "purple", "teal", "yellow", "pink", "indigo", "brown", "gray"} {
		if !used[c] {
			return c
		}
	}
	return "blue"
}

func (a *App) setHidden(id string, hidden bool) {
	a.mu.Lock()
	if hidden {
		a.hidden[id] = true
	} else {
		delete(a.hidden, id)
	}
	a.dirty = true
	a.rev++
	cp := map[string]bool{}
	for k, v := range a.hidden {
		cp[k] = v
	}
	a.mu.Unlock()
	a.save("hidden.json", cp)
	a.rearm()
}

func (a *App) actUpdateCalendar(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		Calendar string  `json:"calendar"`
		Name     *string `json:"name"`
		Color    *string `json:"color"`
		Shown    *bool   `json:"shown"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if in.Calendar == invitationsID {
		if in.Name != nil || in.Color != nil {
			return nil, bad("the Invitations calendar can only be shown or hidden")
		}
		if c.DryRun {
			return dry(map[string]any{"update_calendar": "Invitations", "shown": in.Shown}), nil
		}
		if in.Shown != nil {
			a.setHidden(invitationsID, !*in.Shown)
		}
		return map[string]any{"calendar": map[string]any{"id": invitationsID, "name": "Invitations", "shown": in.Shown == nil || *in.Shown}}, nil
	}
	cs, err := a.findCal(in.Calendar)
	if err != nil {
		return nil, err
	}
	var name, color string
	if in.Name != nil {
		name = oneLine(*in.Name)
	}
	if in.Color != nil {
		color = *in.Color
	}
	if err := checkCalFields(name, color, in.Name != nil); err != nil {
		return nil, err
	}
	b := a.newBatch()
	if in.Name != nil {
		b.set("c.name", name)
	}
	if in.Color != nil && color != "" {
		b.set("c.color", color)
	}
	if len(b.ops) > 0 && cs.mode == modeRead {
		return nil, readOnly()
	}
	if c.DryRun {
		return dry(map[string]any{"update_calendar": cs.info.Name, "name": in.Name, "color": in.Color, "shown": in.Shown}), nil
	}
	if len(b.ops) > 0 {
		if err := a.commit(ctx, cs, b); err != nil {
			return nil, err
		}
	}
	if in.Shown != nil {
		a.setHidden(cs.id, !*in.Shown)
	}
	cs, err = a.findCal(cs.id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"calendar": cs.info}, nil
}

func (a *App) actDeleteCalendar(ctx context.Context, c sol.Call) (any, error) {
	var in struct{ Calendar string }
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	cs, err := a.findCal(in.Calendar)
	if err != nil {
		return nil, err
	}
	if c.DryRun {
		what := "delete the calendar for everyone"
		if cs.owner != a.me {
			what = "leave the calendar (the owner keeps it)"
		}
		return dry(map[string]any{"delete_calendar": cs.info.Name, "action": what, "events": len(cs.events)}), nil
	}
	if cs.owner == a.me {
		b := a.newBatch()
		b.set("c.deleted", true)
		if err := a.commit(ctx, cs, b); err != nil {
			return nil, err
		}
	} else {
		a.mu.Lock()
		a.left[cs.key] = true
		a.dirty = true
		a.rev++
		cp := map[string]bool{}
		for k, v := range a.left {
			cp[k] = v
		}
		a.mu.Unlock()
		a.save("left.json", cp)
		if cs.mode == modeWrite {
			_ = a.api.Unshare(ctx, sol.ShareRequest{Feed: cs.feed, People: []string{cs.owner}, Mode: sol.ModeRead})
		}
		a.rearm()
	}
	return map[string]any{"deleted": cs.info.Name}, nil
}

func (a *App) actShare(ctx context.Context, c sol.Call) (any, error) {
	var in struct{ Calendar, Person, Mode string }
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if in.Mode == "" {
		in.Mode = modeRead
	}
	if in.Mode != modeRead && in.Mode != modeWrite && in.Mode != "none" {
		return nil, bad("mode must be read, write or none")
	}
	cs, err := a.findCal(in.Calendar)
	if err != nil {
		return nil, err
	}
	if cs.owner != a.me {
		return nil, sol.Errorf("forbidden", "only the owner of a calendar can share it")
	}
	p, err := a.resolvePerson(ctx, in.Person)
	if err != nil {
		return nil, err
	}
	if c.DryRun {
		return dry(map[string]any{"share": cs.info.Name, "with": p.Name, "mode": in.Mode}), nil
	}
	b := a.newBatch()
	if in.Mode == "none" {
		b.del("g." + p.ID)
	} else {
		// The grant goes into the feed first, so the person finds it when the feed becomes readable.
		b.set("g."+p.ID, in.Mode)
	}
	if err := a.commit(ctx, cs, b); err != nil {
		return nil, err
	}
	req := sol.ShareRequest{Feed: cs.feed, People: []string{p.ID}, Mode: sol.ModeRead}
	if in.Mode == "none" {
		err = a.api.Unshare(ctx, req)
	} else {
		req.Mode = in.Mode
		err = a.api.Share(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	a.loadNames(ctx)
	cs, _ = a.findCal(cs.id)
	return map[string]any{"calendar": cs.info}, nil
}

// ---------- events ----------

// eventIn is what add, update and move accept; nil pointers are not given.
type eventIn struct {
	ID       string  `json:"id"`
	At       string  `json:"at"`
	Span     string  `json:"span"`
	Calendar string  `json:"calendar"`
	Title    *string `json:"title"`
	Start    *string `json:"start"`
	End      *string `json:"end"`
	Zone     *string `json:"zone"`
	AllDay   *bool   `json:"all_day"`
	Location *string `json:"location"`
	Notes    *string `json:"notes"`
	Repeat   *string `json:"repeat"`
	Alerts   *[]int  `json:"alerts"`
	Color    *string `json:"color"`
}

// toPatch turns input into a patch, parsing times in the given zone (or the event's).
func (a *App) toPatch(in eventIn, eventZone string, eventAllDay bool) (patch, error) {
	var p patch
	zone := eventZone
	if in.Zone != nil && *in.Zone != "" {
		if _, err := loadZone(*in.Zone, a.loc); err != nil {
			return p, bad("%v", err)
		}
		zone = *in.Zone
		p.Zone = &zone
	}
	loc, err := loadZone(zone, a.loc)
	if err != nil {
		loc = a.loc
	}
	if in.Title != nil {
		t := oneLine(*in.Title)
		p.Title = &t
	}
	p.Location, p.Notes, p.Color, p.Alerts = in.Location, in.Notes, in.Color, in.Alerts
	if in.Repeat != nil {
		r := shortRule(*in.Repeat)
		p.RRule = &r
	}
	allDay := eventAllDay
	if in.AllDay != nil {
		allDay = *in.AllDay
		p.AllDay = in.AllDay
	}
	when := func(s string) (time.Time, error) {
		t, isDate, err := parseWhen(s, loc)
		if err != nil {
			return t, bad("%v", err)
		}
		if isDate != allDay {
			if in.AllDay == nil {
				allDay = isDate
				p.AllDay = &allDay
			} else if allDay {
				l := t.In(loc)
				t = time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
			} else {
				t = wallTime(t.Year(), t.Month(), t.Day(), 9, 0, 0, loc)
			}
		}
		return t, nil
	}
	if in.Start != nil {
		t, err := when(*in.Start)
		if err != nil {
			return p, err
		}
		p.Start = &t
	}
	if in.End != nil && *in.End != "" {
		t, err := when(*in.End)
		if err != nil {
			return p, err
		}
		p.End = &t
	}
	return p, nil
}

func (a *App) actAdd(ctx context.Context, c sol.Call) (any, error) {
	var in eventIn
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if in.Title == nil || in.Start == nil {
		return nil, bad("an event needs a title and a start")
	}
	zone := a.zone
	if in.Zone != nil && *in.Zone != "" {
		zone = *in.Zone
	}
	// A new event is a patch over an empty one starting now.
	base := fields{TZ: zone}
	p, err := a.toPatch(in, zone, false)
	if err != nil {
		return nil, err
	}
	allDay := p.AllDay != nil && *p.AllDay
	if allDay {
		base.AllDay, base.TZ = true, ""
		base.Start = *p.Start
		base.End = base.Start.AddDate(0, 0, 1)
		p.AllDay = nil
	} else {
		base.Start = *p.Start
		base.End = base.Start.Add(time.Hour)
	}
	p.Zone = nil
	f := a.apply(base, p)
	if err := f.check(); err != nil {
		return nil, err
	}
	var cs *calState
	if in.Calendar != "" {
		if cs, err = a.findCal(in.Calendar); err != nil {
			return nil, err
		}
		if cs.mode == modeRead {
			return nil, readOnly()
		}
	} else {
		cs = a.writableDefault()
	}
	if cs == nil {
		if c.DryRun {
			return dry(map[string]any{"create_calendar": "Calendar", "add": f.Title}), nil
		}
		id, err := a.createCalendar(ctx, "Calendar", "blue")
		if err != nil {
			return nil, err
		}
		if cs, err = a.findCal(id); err != nil {
			return nil, err
		}
	}
	if c.DryRun {
		return dry(map[string]any{"add": f.Title, "start": a.timeStr(f, f.Start), "end": a.timeStr(f, f.End), "repeat": f.RRule, "calendar": cs.info.Name}), nil
	}
	id := randHex(12)
	b := a.newBatch()
	b.write(id, f, nil)
	b.ev(id, "deleted", false)
	b.ev(id, "created", float64(a.clock.Now().UnixMilli()))
	if err := a.commit(ctx, cs, b); err != nil {
		return nil, err
	}
	cs, e, _, err := a.findEvent(id, "")
	if err != nil {
		return nil, err
	}
	return map[string]any{"event": a.eventOut(cs, e, a.loc)}, nil
}

func (a *App) actUpdate(ctx context.Context, c sol.Call) (any, error) {
	var in eventIn
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	return a.update(ctx, c, in)
}

func (a *App) update(ctx context.Context, c sol.Call, in eventIn) (any, error) {
	cs, e, rid, err := a.findEvent(in.ID, in.At)
	if err != nil {
		return nil, err
	}
	if cs.mode == modeRead {
		return nil, readOnly()
	}
	p, err := a.toPatch(in, e.TZ, e.AllDay)
	if err != nil {
		return nil, err
	}
	if rid != "" {
		if ov := e.overrides[rid]; ov != nil && in.Span != "all" && in.Span != "following" {
			// Times of an override parse in its own zone and kind.
			if p, err = a.toPatch(in, ov.TZ, ov.AllDay); err != nil {
				return nil, err
			}
		}
	}
	plan, err := a.planUpdate(cs, e, rid, in.Span, p)
	if err != nil {
		return nil, err
	}
	if c.DryRun {
		return dry(map[string]any{"update": plan.summary, "keys": len(plan.b.ops)}), nil
	}
	if err := a.commit(ctx, cs, plan.b); err != nil {
		return nil, err
	}
	cs, ne, nrid, err := a.findEvent(plan.id, "")
	if err != nil {
		return nil, err
	}
	out := map[string]any{"event": a.eventOut(cs, ne, a.loc)}
	if nrid != "" {
		if o, ok := a.occurrenceOf(ne, nrid); ok {
			out["occurrence"] = a.occOut(cs, o, a.loc)
		}
	}
	return out, nil
}

func (a *App) actMove(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		ID, At, Span, Start, Zone, Calendar string
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if in.Start == "" && in.Calendar == "" {
		return nil, bad("say where to: start and/or calendar")
	}
	cs, e, rid, err := a.findEvent(in.ID, in.At)
	if err != nil {
		return nil, err
	}
	if cs.mode == modeRead {
		return nil, readOnly()
	}
	var to *calState
	if in.Calendar != "" {
		if to, err = a.findCal(in.Calendar); err != nil {
			return nil, err
		}
		if to.mode == modeRead {
			return nil, readOnly()
		}
		if to == cs {
			to = nil
		} else if rid != "" && in.Span != "all" {
			return nil, bad("only a whole repeating event can change calendar: pass span all")
		}
	}
	if to != nil {
		if c.DryRun {
			return dry(map[string]any{"move": e.Title, "from": cs.info.Name, "to": to.info.Name, "start": in.Start}), nil
		}
		b := a.newBatch()
		a.copyEvent(b, e)
		if err := a.commit(ctx, to, b); err != nil {
			return nil, err
		}
		del, _, _ := a.planDelete(e, "", "all")
		if err := a.commit(ctx, cs, del); err != nil {
			return nil, err
		}
		if in.Start == "" {
			ncs, ne, _, err := a.findEvent(e.ID, "")
			if err != nil {
				return nil, err
			}
			return map[string]any{"event": a.eventOut(ncs, ne, a.loc)}, nil
		}
		in.ID = e.ID
		if rid != "" {
			in.ID += "@" + rid
		}
	}
	st := in.Start
	upd := eventIn{ID: in.ID, At: in.At, Span: in.Span, Start: &st}
	if in.Zone != "" {
		upd.Zone = &in.Zone
	}
	return a.update(ctx, c, upd)
}

func (a *App) actDelete(ctx context.Context, c sol.Call) (any, error) {
	var in struct{ ID, At, Span string }
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	cs, e, rid, err := a.findEvent(in.ID, in.At)
	if err != nil {
		return nil, err
	}
	if cs.mode == modeRead {
		return nil, readOnly()
	}
	b, summary, err := a.planDelete(e, rid, in.Span)
	if err != nil {
		return nil, err
	}
	if c.DryRun {
		return dry(map[string]any{"delete": summary}), nil
	}
	if err := a.commit(ctx, cs, b); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": summary}, nil
}

// ---------- invitations ----------

func (a *App) actInvite(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		ID     string
		People []string
		Remove bool
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	cs, e, _, err := a.findEvent(in.ID, "")
	if err != nil {
		return nil, err
	}
	if cs.owner != a.me {
		return nil, sol.Errorf("forbidden", "only the calendar's owner can invite people to its events")
	}
	if len(in.People) == 0 {
		return nil, bad("invite whom?")
	}
	b := a.newBatch()
	var names []string
	for _, ref := range in.People {
		p, err := a.resolvePerson(ctx, ref)
		if err != nil {
			return nil, err
		}
		if p.ID == a.me {
			return nil, bad("you are always at your own events")
		}
		names = append(names, p.Name)
		if in.Remove {
			b.del("e." + e.ID + ".a." + p.ID)
		} else {
			b.ev(e.ID, "a."+p.ID, "invited")
		}
	}
	if c.DryRun {
		return dry(map[string]any{"invite": names, "to": e.Title, "remove": in.Remove}), nil
	}
	if err := a.commit(ctx, cs, b); err != nil {
		return nil, err
	}
	cs, e, _, err = a.findEvent(e.ID, "")
	if err != nil {
		return nil, err
	}
	return map[string]any{"event": a.eventOut(cs, e, a.loc)}, nil
}

func (a *App) actRespond(ctx context.Context, c sol.Call) (any, error) {
	var in struct{ Invite, Response string }
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if !responses[in.Response] {
		return nil, bad("response must be accepted, declined or tentative")
	}
	ref, _, _ := strings.Cut(in.Invite, "@")
	for _, iv := range a.invitesView() {
		if iv.ref != ref {
			continue
		}
		if c.DryRun {
			return dry(map[string]any{"respond": iv.ev.Title, "response": in.Response}), nil
		}
		if err := a.respond(ctx, iv, in.Response); err != nil {
			return nil, err
		}
		return map[string]any{"invite": ref, "status": in.Response}, nil
	}
	return nil, missing("invitation " + ref)
}
