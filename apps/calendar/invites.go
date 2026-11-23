package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/apv022/sol/sdk/go/crdt"
	"github.com/apv022/sol/sdk/go/sol"
)

// Invitations (design/apps/CALENDAR.md "Invites"). The organiser (the calendar's owner) keeps
// one feed per invitee, inv.<person>, with a snapshot of every event that invites them; the
// invitee answers in its own rsvp.<organiser>. Snapshots are rewritten only when they differ,
// so two devices of the organiser converge on the same values and stop.

// snapshot is what an invitee sees of an event.
type snapshot struct {
	Title     string         `json:"title,omitempty"`
	Start     string         `json:"start,omitempty"`
	End       string         `json:"end,omitempty"`
	TZ        string         `json:"tz,omitempty"`
	Location  string         `json:"location,omitempty"`
	Notes     string         `json:"notes,omitempty"`
	RRule     string         `json:"rrule,omitempty"`
	Alerts    []int          `json:"alerts,omitempty"`
	Exdates   []string       `json:"exdates,omitempty"`
	Overrides []snapOverride `json:"overrides,omitempty"`
	Attendees []string       `json:"attendees,omitempty"`
	Calendar  string         `json:"calendar,omitempty"`
	Cancelled bool           `json:"cancelled,omitempty"`
}

type snapOverride struct {
	RID      string `json:"rid"`
	Title    string `json:"title"`
	Start    string `json:"start"`
	End      string `json:"end"`
	TZ       string `json:"tz,omitempty"`
	Location string `json:"location,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

var cancelled = json.RawMessage(`{"cancelled":true}`)

// snapshotOf is the invitation for event e of calendar cs.
func snapshotOf(cs *calState, e *Event) snapshot {
	s := snapshot{Title: e.Title, Start: e.Start, End: e.End, TZ: e.TZ, Location: e.Location, Notes: e.Notes,
		RRule: e.RRule, Alerts: e.Alerts, Exdates: e.Exdates, Attendees: e.invitees, Calendar: cs.info.Name}
	for rid, ov := range e.overrides {
		s.Overrides = append(s.Overrides, snapOverride{RID: rid, Title: ov.Title, Start: ov.Start, End: ov.End, TZ: ov.TZ, Location: ov.Location, Notes: ov.Notes})
	}
	sort.Slice(s.Overrides, func(i, j int) bool { return s.Overrides[i].RID < s.Overrides[j].RID })
	return s
}

// event turns a received snapshot back into an Event (nil when it is not usable).
func (s snapshot) event(id string, local *time.Location) *Event {
	if s.Title == "" || len(s.Title) > maxTitle || len(s.Notes) > maxText || len(s.Location) > maxText {
		return nil
	}
	e := &Event{ID: id, Title: s.Title, Start: s.Start, End: s.End, TZ: s.TZ, Location: s.Location, Notes: s.Notes, RRule: s.RRule}
	for _, n := range s.Alerts {
		if n >= 0 && n <= 40320 && len(e.Alerts) < maxAlerts {
			e.Alerts = append(e.Alerts, n)
		}
	}
	if !e.resolveTimes(local) {
		return nil
	}
	if e.RRule != "" {
		r, err := ParseRule(e.RRule)
		if err != nil {
			e.RRule = ""
		} else {
			e.rule = r
		}
	}
	for _, x := range s.Exdates {
		if validRID(x) {
			e.Exdates = append(e.Exdates, x)
		}
	}
	e.overrides = map[string]*Event{}
	if e.rule != nil {
		for _, o := range s.Overrides {
			ov := &Event{ID: id, Title: o.Title, Start: o.Start, End: o.End, TZ: o.TZ, Location: o.Location, Notes: o.Notes, Of: id, RID: o.RID}
			if validRID(o.RID) && o.Title != "" && len(o.Title) <= maxTitle && ov.resolveTimes(local) {
				e.overrides[o.RID] = ov
			}
		}
	}
	return e
}

// syncInvites makes my inv.<person> feeds match the attendees of the events in calendars I own.
func (a *App) syncInvites(ctx context.Context) error {
	cals := a.view()
	want := map[string]map[string]json.RawMessage{} // person → key → value
	for _, cs := range cals {
		if cs.owner != a.me {
			continue
		}
		for _, e := range cs.events {
			if len(e.invitees) == 0 {
				continue
			}
			raw, err := json.Marshal(snapshotOf(cs, e))
			if err != nil || len(raw) > maxValue {
				continue
			}
			for _, p := range e.invitees {
				if p == a.me {
					continue
				}
				if want[p] == nil {
					want[p] = map[string]json.RawMessage{}
				}
				want[p]["i."+cs.id+"."+e.ID] = raw
			}
		}
	}
	a.mu.RLock()
	have := map[string]map[string]json.RawMessage{}
	for p, m := range a.myInv {
		have[p] = m
	}
	a.mu.RUnlock()
	people := map[string]bool{}
	for p := range want {
		people[p] = true
	}
	for p := range have {
		people[p] = true
	}
	for _, p := range sortedKeys(people) {
		var ops []crdt.MapOp
		scratch := crdt.NewMap()
		set := func(k string, v json.RawMessage) {
			op, _ := scratch.Set(a.crdtClk, k, v)
			ops = append(ops, op)
		}
		for k, v := range want[p] {
			if !bytes.Equal(have[p][k], v) {
				set(k, v)
			}
		}
		for k, v := range have[p] {
			if _, ok := want[p][k]; !ok && !bytes.Equal(v, cancelled) {
				set(k, cancelled)
			}
		}
		if len(ops) == 0 {
			continue
		}
		feed := "inv." + p
		if err := a.appendOps(ctx, feed, ops); err != nil {
			return err
		}
		if err := a.shareOnce(ctx, feed, p); err != nil {
			return err
		}
	}
	return nil
}

// respond writes my answer to an invitation into rsvp.<organiser>, shared with the organiser.
func (a *App) respond(ctx context.Context, iv *invite, answer string) error {
	feed := "rsvp." + iv.from
	op, err := crdt.NewMap().Set(a.crdtClk, "r."+iv.cal+"."+iv.event, answer)
	if err != nil {
		return err
	}
	if err := a.shareOnce(ctx, feed, iv.from); err != nil {
		return err
	}
	if err := a.appendOps(ctx, feed, []crdt.MapOp{op}); err != nil {
		return err
	}
	a.afterChange(ctx)
	return nil
}

// notifyInvites asks me (an Inbox approval) about each new invitation, and tells me (a notice)
// when someone answers one of mine.
func (a *App) notifyInvites(ctx context.Context) {
	if a.api == nil || a.crdtClk == nil {
		return
	}
	invs := a.invitesView()
	type answer struct{ key, who, title, status string }
	var news []answer
	a.mu.RLock()
	for _, cs := range a.cals {
		if cs.owner != a.me {
			continue
		}
		for _, e := range cs.events {
			for _, at := range e.Attendees {
				if at.Status != "invited" {
					news = append(news, answer{cs.id + "." + e.ID + "." + at.Person, at.Name, e.Title, at.Status})
				}
			}
		}
	}
	names := map[string]string{}
	for k, v := range a.names {
		names[k] = v
	}
	a.mu.RUnlock()

	a.noteMu.Lock()
	defer a.noteMu.Unlock()
	changedAsk, changedTold := false, false
	for _, iv := range invs {
		if iv.status != "" || a.asked[iv.ref] != "" {
			continue
		}
		who := names[iv.from]
		if who == "" {
			who = "Someone"
		}
		res, err := a.api.PostInbox(ctx, sol.InboxPost{Kind: "approval", Title: who + " invited you to " + iv.ev.Title,
			Body:   describeWhen(iv.ev, a.loc) + placeSuffix(iv.ev.Location) + ". Approve to accept, deny to decline.",
			Action: &sol.ActionCall{Action: "calendar.respond", Args: mustJSON(map[string]string{"invite": iv.ref, "response": "accepted"})}})
		if err != nil {
			a.logf("calendar: invitation not filed: %v", err)
			continue
		}
		a.asked[iv.ref] = res.ID
		changedAsk = true
	}
	for _, n := range news {
		if a.told[n.key] == n.status {
			continue
		}
		a.told[n.key] = n.status
		changedTold = true
		if !a.primed {
			continue // answers that were there before this device ever looked are not news
		}
		who := n.who
		if who == "" {
			who = "Someone"
		}
		verb := map[string]string{"accepted": "accepted", "declined": "declined", "tentative": "might come to"}[n.status]
		if _, err := a.api.PostInbox(ctx, sol.InboxPost{Kind: "notice", Title: who + " " + verb + " " + n.title, To: "self"}); err != nil {
			a.logf("calendar: answer notice not delivered: %v", err)
		}
	}
	if changedAsk {
		a.save("asked.json", a.asked)
	}
	if changedTold {
		a.save("told.json", a.told)
	}
}

// onInbox declines an invitation whose approval I denied.
func (a *App) onInbox(ctx context.Context, ev sol.InboxEvent) {
	if ev.Answer != "denied" {
		return
	}
	a.noteMu.Lock()
	ref := ""
	for r, id := range a.asked {
		if id == ev.ID {
			ref = r
		}
	}
	a.noteMu.Unlock()
	if ref == "" {
		return
	}
	for _, iv := range a.invitesView() {
		if iv.ref == ref && iv.status == "" {
			if err := a.respond(ctx, iv, "declined"); err != nil {
				a.logf("calendar: declining: %v", err)
			}
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func placeSuffix(loc string) string {
	if loc == "" {
		return ""
	}
	return " · " + loc
}

// describeWhen is a short human time for notices: "Tue Nov 24, 10:00–10:30" or "Tue Nov 24, all day".
func describeWhen(e *Event, loc *time.Location) string {
	if e.AllDay {
		s := e.start.Format("Mon Jan 2")
		if days := int(e.end.Sub(e.start).Hours()/24 + 0.5); days > 1 {
			s += " – " + e.end.AddDate(0, 0, -1).Format("Mon Jan 2")
		}
		return s + ", all day"
	}
	st, en := e.start.In(loc), e.end.In(loc)
	s := st.Format("Mon Jan 2, 15:04")
	if sameDay(st, en) {
		return s + "–" + en.Format("15:04")
	}
	return s + " – " + en.Format("Mon Jan 2, 15:04")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
