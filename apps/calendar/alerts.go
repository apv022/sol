package calendar

import (
	"context"
	"strconv"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

// Alerts: each occurrence of an event with alerts posts an Inbox notice to myself that many
// minutes before it starts. One timer runs for the earliest unfired alert within the horizon
// (or for the horizon itself, to look further), re-armed after every change, so nothing polls.
// An alert missed by more than alertGrace while the app was off is dropped silently.

const (
	alertGrace   = 15 * time.Minute
	alertHorizon = 8 * 24 * time.Hour
	firedKeep    = 45 * 24 * time.Hour
)

type pendingAlert struct {
	key   string
	when  time.Time
	title string
	body  string
}

// alertsBetween lists every alert of shown calendars and accepted (or unanswered) invitations
// that is due in [from, to).
func (a *App) alertsBetween(from, to time.Time) []pendingAlert {
	var out []pendingAlert
	// An alert fires up to 4 weeks before its event, so look that much further ahead.
	w := newWindow(from, to.Add(28*24*time.Hour), a.loc)
	add := func(o occurrence, id string) {
		for _, m := range o.ev.Alerts {
			when := o.start.Add(-time.Duration(m) * time.Minute)
			if o.ev.AllDay {
				// All-day events alert relative to 09:00 local on their first day, like Apple Calendar.
				when = wallTime(o.start.Year(), o.start.Month(), o.start.Day(), 9, 0, 0, a.loc).Add(-time.Duration(m) * time.Minute)
			}
			if when.Before(from) || !when.Before(to) {
				continue
			}
			out = append(out, pendingAlert{key: id + "@" + o.rid + "|" + strconv.Itoa(m) + "|" + strconv.FormatInt(o.start.Unix(), 10),
				when: when, title: o.ev.Title, body: describeWhen(&Event{AllDay: o.ev.AllDay, start: o.start, end: o.end}, a.loc) + placeSuffix(o.ev.Location)})
		}
	}
	for _, cs := range a.view() {
		if !cs.info.Shown {
			continue
		}
		for _, e := range cs.events {
			for _, o := range e.occurrences(w) {
				add(o, cs.id+"."+e.ID)
			}
		}
	}
	for _, iv := range a.invitesView() {
		if iv.status == "declined" {
			continue
		}
		for _, o := range iv.ev.occurrences(w) {
			add(o, iv.ref)
		}
	}
	return out
}

// rearm posts the alerts that are due now and arms the timer for the next one.
func (a *App) rearm() {
	if a.api == nil || a.crdtClk == nil {
		return
	}
	now := a.clock.Now()
	list := a.alertsBetween(now.Add(-alertGrace), now.Add(alertHorizon))
	a.noteMu.Lock()
	defer a.noteMu.Unlock()
	if a.timer != nil {
		a.timer()
		a.timer = nil
	}
	next := now.Add(alertHorizon - time.Hour)
	changed := false
	for _, p := range list {
		if a.fired[p.key] != 0 {
			continue
		}
		if !p.when.After(now) {
			a.fired[p.key] = now.UnixMilli()
			changed = true
			if _, err := a.api.PostInbox(context.Background(), sol.InboxPost{Kind: "notice", Title: p.title, Body: p.body, To: "self"}); err != nil {
				a.logf("calendar: alert not delivered: %v", err)
			}
			continue
		}
		if p.when.Before(next) {
			next = p.when
		}
	}
	if changed {
		for k, ms := range a.fired {
			if now.Sub(time.UnixMilli(ms)) > firedKeep {
				delete(a.fired, k)
			}
		}
		a.save("fired.json", a.fired)
	}
	a.timer = a.clock.AfterFunc(next.Sub(now), a.rearm)
}
