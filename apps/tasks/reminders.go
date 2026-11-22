package tasks

import (
	"context"
	"path/filepath"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

// Reminders: a task with a due date and a time posts an Inbox notice to myself when that moment
// comes. One timer runs for the earliest unfired reminder and is re-armed after every change, so
// nothing polls. A reminder that came due while the app was off is dropped silently when it is
// older than the grace period, so starting the app does not bury the person in old notices.

// reminderGrace is how late a reminder may still be delivered.
const reminderGrace = 15 * time.Minute

// firedKeep is how long fired reminders are remembered.
const firedKeep = 60 * 24 * time.Hour

func reminderKey(t *Task) string { return t.ID + "|" + t.Due + "|" + t.Time }

// rearm posts the reminders that are due now and arms the timer for the next one.
func (a *App) rearm() {
	if a.api == nil || a.crdtClk == nil {
		return
	}
	lists := a.view()
	a.remMu.Lock()
	defer a.remMu.Unlock()
	if a.timer != nil {
		a.timer()
		a.timer = nil
	}
	now := a.now()
	var next time.Time
	changed := false
	for _, ls := range lists {
		for _, t := range ls.tasks {
			if t.Done {
				continue
			}
			when, ok := at(t.Due, t.Time, a.loc)
			if !ok {
				continue
			}
			key := reminderKey(t)
			if a.fired[key] != 0 {
				continue
			}
			if !when.After(now) {
				a.fired[key] = now.UnixMilli()
				changed = true
				if now.Sub(when) <= reminderGrace {
					a.notify(t, ls)
				}
				continue
			}
			if next.IsZero() || when.Before(next) {
				next = when
			}
		}
	}
	if changed {
		a.saveFired(now)
	}
	if !next.IsZero() {
		a.timer = a.clock.AfterFunc(next.Sub(now), a.rearm)
	}
}

func (a *App) notify(t *Task, ls *listState) {
	body := "Due " + t.Time
	if ls.info.Name != "" {
		body += " · " + ls.info.Name
	}
	_, err := a.api.PostInbox(context.Background(), sol.InboxPost{Kind: "notice", Title: t.Title, Body: body, To: "self"})
	if err != nil {
		a.logf("tasks: reminder not delivered: %v", err)
	}
}

func (a *App) saveFired(now time.Time) {
	for k, ms := range a.fired {
		if now.Sub(time.UnixMilli(ms)) > firedKeep {
			delete(a.fired, k)
		}
	}
	if a.data != "" {
		if err := saveJSON(filepath.Join(a.data, "fired.json"), a.fired); err != nil {
			a.logf("tasks: saving reminders: %v", err)
		}
	}
}
