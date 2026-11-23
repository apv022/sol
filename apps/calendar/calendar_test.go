package calendar

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/crdt"
	"github.com/apv022/sol/sdk/go/sol"
)

func TestAddAgendaGetInZones(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false, "Europe/Berlin")
	cid := calID(t, a, "Work")
	id := add(t, a, map[string]any{"calendar": cid, "title": "Standup", "start": "2026-11-19T09:30", "alerts": []int{10, 0, 10}})
	add(t, a, map[string]any{"title": "Trip", "start": "2026-11-20", "end": "2026-11-23", "location": "Lisbon"})
	occ := agenda(t, a, "2026-11-19", "2026-11-25", "Europe/Berlin")
	if got := titles(occ); !reflect.DeepEqual(got, []string{"Standup", "Trip"}) {
		t.Fatalf("agenda: %v", got)
	}
	if occ[0]["start"] != "2026-11-19T09:30:00+01:00" || occ[0]["end"] != "2026-11-19T10:30:00+01:00" || occ[0]["tz"] != "Europe/Berlin" {
		t.Errorf("standup: %v", occ[0])
	}
	if !reflect.DeepEqual(occ[0]["alerts"], []any{0.0, 10.0}) {
		t.Errorf("alerts not sorted and deduplicated: %v", occ[0]["alerts"])
	}
	if occ[1]["all_day"] != true || occ[1]["start"] != "2026-11-20" || occ[1]["end"] != "2026-11-23" {
		t.Errorf("trip: %v", occ[1])
	}
	// Seen from New York the standup is at 03:30; the all-day trip keeps its dates.
	ny := agenda(t, a, "2026-11-19", "2026-11-25", "America/New_York")
	if ny[0]["start"] != "2026-11-19T03:30:00-05:00" || ny[1]["start"] != "2026-11-20" {
		t.Errorf("from New York: %v / %v", ny[0]["start"], ny[1]["start"])
	}
	g := call(t, a, "calendar.get", map[string]any{"id": id})
	if g["event"].(map[string]any)["title"] != "Standup" || g["calendar"].(map[string]any)["name"] != "Work" {
		t.Errorf("get: %v", g)
	}
	// Bad input is refused.
	for _, args := range []map[string]any{
		{"title": "", "start": "2026-11-19T09:00"},
		{"title": "x", "start": "tomorrow"},
		{"title": "x", "start": "2026-11-19T09:00", "end": "2026-11-19T08:00"},
		{"title": "x", "start": "2026-11-19T09:00", "zone": "Mars/Base"},
		{"title": "x", "start": "2026-11-19T09:00", "repeat": "FREQ=HOURLY"},
		{"title": "x", "start": "2026-11-19T09:00", "color": "chartreuse"},
	} {
		if _, err := tryCall(a, "calendar.add", args); err == nil || !strings.Contains(err.Error(), "bad_request") {
			t.Errorf("add %v: %v", args, err)
		}
	}
}

func TestConcurrentFieldEditsMerge(t *testing.T) {
	e := newEnv(t)
	d1 := start(t, e.alice.Devices[0], e.clk, "", false)
	d2 := start(t, e.alice.AddDevice("phone", false), e.clk, "", false)
	cid := calID(t, d1, "Home")
	id := add(t, d1, map[string]any{"calendar": cid, "title": "Dinner", "start": "2026-11-20T19:00"})
	syncAll(t, d2)
	c1, _ := d1.findCal(cid)
	c2, _ := d2.findCal(cid)
	// One device renames and adds a place; the other moves it. Built from the same state.
	b1, b2 := d1.newBatch(), d2.newBatch()
	b1.ev(id, "title", "Dinner with Sam")
	b1.ev(id, "location", "Luigi's")
	b1.ev(id, "notes", "laptop")
	e.clk.set(e.clk.Now().Add(time.Second))
	b2.ev(id, "start", "2026-11-21T20:00:00Z")
	b2.ev(id, "end", "2026-11-21T21:30:00Z")
	b2.ev(id, "notes", "phone") // same field: the later write wins
	if err := d1.commit(t.Context(), c1, b1); err != nil {
		t.Fatal(err)
	}
	if err := d2.commit(t.Context(), c2, b2); err != nil {
		t.Fatal(err)
	}
	syncAll(t, d1, d2)
	g1 := call(t, d1, "calendar.get", map[string]any{"id": id})["event"]
	g2 := call(t, d2, "calendar.get", map[string]any{"id": id})["event"]
	if !reflect.DeepEqual(g1, g2) {
		t.Fatalf("devices differ:\n%v\n%v", g1, g2)
	}
	g := g1.(map[string]any)
	if g["title"] != "Dinner with Sam" || g["location"] != "Luigi's" || g["start"] != "2026-11-21T20:00:00Z" || g["end"] != "2026-11-21T21:30:00Z" || g["notes"] != "phone" {
		t.Errorf("merge lost a field: %v", g)
	}
	// A concurrent end before the start is repaired to the default duration.
	b := d1.newBatch()
	b.ev(id, "end", "2026-11-21T19:00:00Z")
	if err := d1.commit(t.Context(), c1, b); err != nil {
		t.Fatal(err)
	}
	if g := call(t, d1, "calendar.get", map[string]any{"id": id})["event"].(map[string]any); g["end"] != "2026-11-21T21:00:00Z" {
		t.Errorf("end before start not repaired: %v", g["end"])
	}
	// Tombstones win over the older fields and stay deleted on both devices.
	call(t, d2, "calendar.delete", map[string]any{"id": id})
	syncAll(t, d1)
	for _, d := range []*App{d1, d2} {
		if _, err := tryCall(d, "calendar.get", map[string]any{"id": id}); err == nil {
			t.Error("deleted event still there")
		}
	}
}

func TestMoveBetweenCalendars(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false)
	home, work := calID(t, a, "Home"), calID(t, a, "Work")
	id := add(t, a, map[string]any{"calendar": home, "title": "Review", "start": "2026-11-20T10:00", "repeat": "weekly"})
	call(t, a, "calendar.update", map[string]any{"id": id, "at": "20261127T100000Z", "title": "Review (long)"})
	call(t, a, "calendar.move", map[string]any{"id": id, "calendar": "Work"})
	g := call(t, a, "calendar.get", map[string]any{"id": id})
	if g["calendar"].(map[string]any)["id"] != work {
		t.Fatalf("not moved: %v", g["calendar"])
	}
	occ := agenda(t, a, "2026-11-20", "2026-12-05", "UTC")
	if got := titles(occ); !reflect.DeepEqual(got, []string{"Review", "Review (long)", "Review"}) {
		t.Errorf("override lost in the move: %v", got)
	}
	for _, o := range occ {
		if o["calendar"] != work {
			t.Errorf("occurrence still in %v", o["calendar"])
		}
	}
	// A move in time keeps the length.
	call(t, a, "calendar.move", map[string]any{"id": id, "span": "all", "at": "20261120T100000Z", "start": "2026-11-20T14:00"})
	occ = agenda(t, a, "2026-11-20", "2026-11-21", "UTC")
	if occ[0]["start"] != "2026-11-20T14:00:00Z" || occ[0]["end"] != "2026-11-20T15:00:00Z" {
		t.Errorf("moved series: %v", occ[0])
	}
	// One occurrence cannot change calendar on its own.
	if _, err := tryCall(a, "calendar.move", map[string]any{"id": id + "@20261204T140000Z", "calendar": home}); err == nil {
		t.Error("a single occurrence changed calendar")
	}
}

func TestRecurrenceAcrossDST(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false, "America/Los_Angeles")
	add(t, a, map[string]any{"title": "Standup", "start": "2026-10-19T09:00", "repeat": "weekly"})
	// In Los Angeles it stays at 09:00 although DST ends on Nov 1.
	var got []string
	for _, o := range agenda(t, a, "2026-10-19", "2026-11-10", "America/Los_Angeles") {
		got = append(got, o["start"].(string))
	}
	want := []string{"2026-10-19T09:00:00-07:00", "2026-10-26T09:00:00-07:00", "2026-11-02T09:00:00-08:00", "2026-11-09T09:00:00-08:00"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("los angeles:\n got %v\nwant %v", got, want)
	}
	// From Berlin (DST ended Oct 25) it moves: 18:00, 17:00 in the odd week, then 18:00.
	got = nil
	for _, o := range agenda(t, a, "2026-10-19", "2026-11-10", "Europe/Berlin") {
		got = append(got, o["start"].(string)[11:16])
	}
	if !reflect.DeepEqual(got, []string{"18:00", "17:00", "18:00", "18:00"}) {
		t.Errorf("berlin: %v", got)
	}
	// A time in the spring-forward gap moves forward by the gap.
	add(t, a, map[string]any{"title": "Night job", "start": "2026-03-07T02:30", "repeat": "daily"})
	got = nil
	for _, o := range agenda(t, a, "2026-03-07", "2026-03-10", "America/Los_Angeles") {
		if o["title"] == "Night job" {
			got = append(got, o["start"].(string))
		}
	}
	if !reflect.DeepEqual(got, []string{"2026-03-07T02:30:00-08:00", "2026-03-08T03:30:00-07:00", "2026-03-09T02:30:00-07:00"}) {
		t.Errorf("gap: %v", got)
	}
}

func TestRuleExpansion(t *testing.T) {
	utc := time.UTC
	exp := func(rule, start string, n int) []string {
		t.Helper()
		r, err := ParseRule(rule)
		if err != nil {
			t.Fatalf("%s: %v", rule, err)
		}
		st, _ := time.Parse(time.RFC3339, start)
		var out []string
		for _, x := range r.Expand(st, utc, false, st, st.AddDate(5, 0, 0), n) {
			out = append(out, x.Format("2006-01-02"))
		}
		return out
	}
	for _, c := range []struct {
		rule, start string
		want        []string
	}{
		{"FREQ=MONTHLY", "2026-01-31T10:00:00Z", []string{"2026-01-31", "2026-03-31", "2026-05-31", "2026-07-31"}},
		{"FREQ=YEARLY", "2028-02-29T10:00:00Z", []string{"2028-02-29", "2032-02-29"}},
		{"FREQ=MONTHLY;BYDAY=-1FR", "2026-11-27T10:00:00Z", []string{"2026-11-27", "2026-12-25", "2027-01-29"}},
		{"FREQ=MONTHLY;BYDAY=2TU", "2026-11-10T10:00:00Z", []string{"2026-11-10", "2026-12-08", "2027-01-12"}},
		{"FREQ=WEEKLY;BYDAY=MO,WE,FR;COUNT=4", "2026-11-16T10:00:00Z", []string{"2026-11-16", "2026-11-18", "2026-11-20", "2026-11-23"}},
		{"FREQ=DAILY;INTERVAL=3;UNTIL=20261125T000000Z", "2026-11-16T10:00:00Z", []string{"2026-11-16", "2026-11-19", "2026-11-22"}},
		{"FREQ=MONTHLY;BYMONTHDAY=-1", "2026-11-30T10:00:00Z", []string{"2026-11-30", "2026-12-31", "2027-01-31", "2027-02-28"}},
		{"FREQ=YEARLY;BYMONTH=11;BYDAY=4TH", "2026-11-26T10:00:00Z", []string{"2026-11-26", "2027-11-25"}},
	} {
		n := len(c.want)
		if got := exp(c.rule, c.start, n); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s from %s:\n got %v\nwant %v", c.rule, c.start, got, c.want)
		}
	}
	// A hostile rule is bounded.
	r, _ := ParseRule("FREQ=DAILY")
	st := time.Date(1900, 1, 1, 0, 0, 0, 0, utc)
	if n := len(r.Expand(st, utc, false, st, st.AddDate(500, 0, 0), 0)); n > maxOccurrences {
		t.Errorf("unbounded expansion: %d", n)
	}
}

func TestOverridesExcludesAndSpans(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false)
	id := add(t, a, map[string]any{"title": "Gym", "start": "2026-11-16T18:00", "repeat": "daily", "location": "Club"})
	day := func(from, to string) []string {
		var out []string
		for _, o := range agenda(t, a, from, to, "UTC") {
			out = append(out, o["start"].(string)[5:16]+" "+o["title"].(string))
		}
		return out
	}
	// This event: moved and renamed; another one deleted.
	call(t, a, "calendar.update", map[string]any{"id": id + "@20261117T180000Z", "title": "Gym with Jo", "start": "2026-11-17T19:00"})
	call(t, a, "calendar.delete", map[string]any{"id": id, "at": "20261118T180000Z", "span": "this"})
	want := []string{"11-16T18:00 Gym", "11-17T19:00 Gym with Jo", "11-19T18:00 Gym"}
	if got := day("2026-11-16", "2026-11-20"); !reflect.DeepEqual(got, want) {
		t.Fatalf("after this-event edits:\n got %v\nwant %v", got, want)
	}
	// Editing the override again changes it, not the series.
	call(t, a, "calendar.update", map[string]any{"id": id + "@20261117T180000Z", "location": "Park"})
	o := call(t, a, "calendar.get", map[string]any{"id": id + "@20261117T180000Z"})["occurrence"].(map[string]any)
	if o["location"] != "Park" || o["title"] != "Gym with Jo" || o["override"] != true {
		t.Errorf("override: %v", o)
	}
	// All events: a shift moves the series and its exceptions with it.
	call(t, a, "calendar.update", map[string]any{"id": id + "@20261119T180000Z", "span": "all", "start": "2026-11-19T17:00", "title": "Workout"})
	want = []string{"11-16T17:00 Workout", "11-17T19:00 Gym with Jo", "11-19T17:00 Workout"}
	if got := day("2026-11-16", "2026-11-20"); !reflect.DeepEqual(got, want) {
		t.Fatalf("after all-events edit:\n got %v\nwant %v", got, want)
	}
	// This and following: the series ends, a new one starts with the change.
	call(t, a, "calendar.update", map[string]any{"id": id + "@20261121T170000Z", "span": "following", "title": "Run"})
	want = []string{"11-19T17:00 Workout", "11-20T17:00 Workout", "11-21T17:00 Run", "11-22T17:00 Run"}
	if got := day("2026-11-19", "2026-11-23"); !reflect.DeepEqual(got, want) {
		t.Fatalf("after following edit:\n got %v\nwant %v", got, want)
	}
	// Deleting following ends the new series.
	run := agenda(t, a, "2026-11-22", "2026-11-23", "UTC")[0]["id"].(string)
	call(t, a, "calendar.delete", map[string]any{"id": run, "span": "following"})
	if got := day("2026-11-21", "2026-11-25"); !reflect.DeepEqual(got, []string{"11-21T17:00 Run"}) {
		t.Errorf("after deleting following: %v", got)
	}
	// Deleting all removes the series with its overrides.
	call(t, a, "calendar.delete", map[string]any{"id": id, "span": "all"})
	if got := day("2026-11-16", "2026-11-21"); len(got) != 0 {
		t.Errorf("after deleting all: %v", got)
	}
}

func TestAllDayAndToggle(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false, "Europe/Berlin")
	id := add(t, a, map[string]any{"title": "Holiday", "start": "2026-12-24", "end": "2026-12-27", "repeat": "yearly"})
	occ := agenda(t, a, "2027-12-01", "2028-01-01", "Pacific/Auckland")
	if len(occ) != 1 || occ[0]["start"] != "2027-12-24" || occ[0]["end"] != "2027-12-27" {
		t.Fatalf("all-day yearly: %v", occ)
	}
	// Switching to a timed event starts at 09:00 local for an hour.
	call(t, a, "calendar.update", map[string]any{"id": id, "all_day": false})
	g := call(t, a, "calendar.get", map[string]any{"id": id})["event"].(map[string]any)
	if g["all_day"] != false || g["start"] != "2026-12-24T09:00:00+01:00" || g["end"] != "2026-12-24T10:00:00+01:00" {
		t.Errorf("to timed: %v", g)
	}
	call(t, a, "calendar.update", map[string]any{"id": id, "start": "2026-12-24"})
	g = call(t, a, "calendar.get", map[string]any{"id": id})["event"].(map[string]any)
	if g["all_day"] != true || g["start"] != "2026-12-24" || g["end"] != "2026-12-25" {
		t.Errorf("back to all day: %v", g)
	}
}

func TestAlertsFire(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	a := start(t, e.alice.Devices[0], e.clk, dir, false)
	add(t, a, map[string]any{"title": "Dentist", "start": "2026-11-18T11:00", "alerts": []int{10}, "location": "Main St"})
	add(t, a, map[string]any{"title": "Missed", "start": "2026-11-18T09:00", "alerts": []int{30}}) // 08:30: missed by two hours
	add(t, a, map[string]any{"title": "Daily", "start": "2026-11-18T12:00", "repeat": "daily", "alerts": []int{0}})
	notices := func() []string {
		var out []string
		for _, it := range e.alice.Inbox() {
			if it.Kind == "notice" {
				out = append(out, it.Title+"|"+it.Body)
			}
		}
		return out
	}
	if n := notices(); len(n) != 0 {
		t.Fatalf("alerts fired early: %v", n)
	}
	if got := e.clk.armed(); !got.Equal(time.Date(2026, 11, 18, 10, 50, 0, 0, time.UTC)) {
		t.Fatalf("timer armed for %v", got)
	}
	e.clk.set(time.Date(2026, 11, 18, 10, 50, 0, 0, time.UTC))
	if n := notices(); !reflect.DeepEqual(n, []string{"Dentist|Wed Nov 18, 11:00–12:00 · Main St"}) {
		t.Fatalf("after 10:50: %v", n)
	}
	e.clk.set(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	e.clk.set(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	if n := notices(); len(n) != 3 {
		t.Fatalf("daily alerts: %v", n)
	}
	// Fired alerts are remembered: a restart does not repeat them.
	a.Stop()
	b := start(t, e.alice.Devices[0], e.clk, dir, false)
	_ = b
	if n := notices(); len(n) != 3 {
		t.Errorf("restart repeated alerts: %v", n)
	}
	// A hidden calendar does not alert.
	call(t, b, "calendar.update_calendar", map[string]any{"calendar": "Calendar", "shown": false})
	e.clk.set(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
	if n := notices(); len(n) != 3 {
		t.Errorf("hidden calendar alerted: %v", n)
	}
}

func TestSharingPermissions(t *testing.T) {
	e := newEnv(t)
	alice := start(t, e.alice.Devices[0], e.clk, "", true)
	bob := start(t, e.bob.Devices[0], e.clk, "", true)
	carol := start(t, e.car.Devices[0], e.clk, "", true)
	cid := calID(t, alice, "Family")
	id := add(t, alice, map[string]any{"calendar": cid, "title": "Lunch", "start": "2026-11-22T12:00"})
	if n := len(call(t, bob, "calendar.list_calendars", nil)["calendars"].([]any)); n != 0 {
		t.Fatalf("bob sees %d calendars before sharing", n)
	}
	call(t, alice, "calendar.share", map[string]any{"calendar": cid, "person": "Bob", "mode": "read"})
	eventually(t, "bob to see the calendar", func() bool {
		return len(call(t, bob, "calendar.list_calendars", nil)["calendars"].([]any)) == 1
	})
	if _, err := tryCall(bob, "calendar.update", map[string]any{"id": id, "title": "Hacked"}); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("read-only update: %v", err)
	}
	// Ops Bob writes into his own copy of the feed are ignored without a write grant.
	forge := func(key string, v any) {
		op, err := crdt.NewMap().Set(crdt.NewClock(e.bob.Devices[0].ID), key, v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bob.api.Append(t.Context(), "cal."+cid, map[string]any{"ops": []crdt.MapOp{op}}); err != nil {
			t.Fatal(err)
		}
		if err := bob.api.Share(t.Context(), sol.ShareRequest{Feed: "cal." + cid, People: []string{e.alice.ID}, Mode: sol.ModeRead}); err != nil {
			t.Fatal(err)
		}
	}
	forge("e."+id+".title", "Forged")
	syncAll(t, alice)
	if g := call(t, alice, "calendar.get", map[string]any{"id": id})["event"].(map[string]any); g["title"] != "Lunch" {
		t.Fatalf("op without a write grant applied: %v", g["title"])
	}
	// With a write grant Bob's edits count, but owner-only keys from him never do.
	call(t, alice, "calendar.share", map[string]any{"calendar": cid, "person": "Bob", "mode": "write"})
	eventually(t, "bob to get write access", func() bool {
		return call(t, bob, "calendar.list_calendars", nil)["calendars"].([]any)[0].(map[string]any)["mode"] == "write"
	})
	call(t, bob, "calendar.update", map[string]any{"id": id, "title": "Lunch at Bob's"})
	forge("g."+e.car.ID, "write")
	forge("c.deleted", true)
	eventually(t, "alice to see bob's edit", func() bool {
		return call(t, alice, "calendar.get", map[string]any{"id": id})["event"].(map[string]any)["title"] == "Lunch at Bob's"
	})
	if n := len(call(t, carol, "calendar.list_calendars", nil)["calendars"].([]any)); n != 0 {
		t.Error("bob granted carol access")
	}
	if n := len(call(t, alice, "calendar.list_calendars", nil)["calendars"].([]any)); n != 1 {
		t.Error("bob deleted alice's calendar")
	}
	// Only the owner shares.
	if _, err := tryCall(bob, "calendar.share", map[string]any{"calendar": cid, "person": "Carol"}); err == nil {
		t.Error("a co-author shared the calendar")
	}
	// Leaving a shared calendar hides it for Bob only.
	call(t, bob, "calendar.delete_calendar", map[string]any{"calendar": cid})
	if n := len(call(t, bob, "calendar.list_calendars", nil)["calendars"].([]any)); n != 0 {
		t.Error("bob still sees the calendar he left")
	}
	if n := len(call(t, alice, "calendar.list_calendars", nil)["calendars"].([]any)); n != 1 {
		t.Error("bob leaving deleted alice's calendar")
	}
}

func TestInvitationsAndAnswers(t *testing.T) {
	e := newEnv(t)
	alice := start(t, e.alice.Devices[0], e.clk, "", true)
	bob := start(t, e.bob.Devices[0], e.clk, "", true)
	id := add(t, alice, map[string]any{"title": "Party", "start": "2026-11-28T20:00", "location": "Alice's"})
	if _, err := tryCall(bob, "calendar.invite", map[string]any{"id": id, "people": []string{"Carol"}}); err == nil {
		t.Error("bob invited to alice's event")
	}
	call(t, alice, "calendar.invite", map[string]any{"id": id, "people": []string{"Bob"}})
	eventually(t, "bob to get the invitation", func() bool {
		return len(list(t, call(t, bob, "calendar.invitations", nil), "invitations")) == 1
	})
	inv := list(t, call(t, bob, "calendar.invitations", nil), "invitations")[0]
	if inv["title"] != "Party" || inv["status"] != "invited" || !strings.EqualFold(inv["from_name"].(string), "Alice") {
		t.Fatalf("invitation: %v", inv)
	}
	eventually(t, "bob's inbox to ask", func() bool {
		for _, it := range e.bob.Inbox() {
			if it.Kind == "approval" && strings.Contains(it.Title, "Party") {
				return true
			}
		}
		return false
	})
	// The invitation shows in Bob's agenda.
	if got := titles(agenda(t, bob, "2026-11-28", "2026-11-29", "UTC")); !reflect.DeepEqual(got, []string{"Party"}) {
		t.Errorf("bob's agenda: %v", got)
	}
	call(t, bob, "calendar.respond", map[string]any{"invite": inv["invite"], "response": "accepted"})
	eventually(t, "alice to see the answer", func() bool {
		at := call(t, alice, "calendar.get", map[string]any{"id": id})["event"].(map[string]any)["attendees"]
		as, _ := at.([]any)
		return len(as) == 1 && as[0].(map[string]any)["status"] == "accepted"
	})
	// Alice changes the event; Bob's copy follows. Deleting cancels it.
	call(t, alice, "calendar.update", map[string]any{"id": id, "title": "Party!"})
	eventually(t, "bob to see the change", func() bool {
		is := list(t, call(t, bob, "calendar.invitations", nil), "invitations")
		return len(is) == 1 && is[0]["title"] == "Party!"
	})
	call(t, alice, "calendar.delete", map[string]any{"id": id})
	eventually(t, "bob to see the cancellation", func() bool {
		return len(list(t, call(t, bob, "calendar.invitations", nil), "invitations")) == 0
	})
}

func TestEveryWriteHasDryRun(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false)
	cid := calID(t, a, "Home")
	id := add(t, a, map[string]any{"calendar": cid, "title": "Thing", "start": "2026-11-20T10:00", "repeat": "daily"})
	before, _ := a.api.Read(t.Context(), sol.Address(e.alice.ID, appID, "cal."+cid), 1, 1000)
	ics := call(t, a, "calendar.export", map[string]any{"calendar": cid})["ics"].(string)
	for name, args := range map[string]map[string]any{
		"calendar.create_calendar": {"name": "X"},
		"calendar.update_calendar": {"calendar": cid, "name": "Y"},
		"calendar.delete_calendar": {"calendar": cid},
		"calendar.add":             {"calendar": cid, "title": "New", "start": "2026-11-21T10:00"},
		"calendar.update":          {"id": id, "title": "Z"},
		"calendar.move":            {"id": id, "start": "2026-11-20T12:00"},
		"calendar.delete":          {"id": id},
		"calendar.share":           {"calendar": cid, "person": "Bob"},
		"calendar.invite":          {"id": id, "people": []string{"Bob"}},
		"calendar.import":          {"ics": strings.ReplaceAll(ics, "Thing", "Other")},
	} {
		out := call(t, a, name, args, true)
		if out["dry_run"] != true {
			t.Errorf("%s: no dry run: %v", name, out)
		}
	}
	after, _ := a.api.Read(t.Context(), sol.Address(e.alice.ID, appID, "cal."+cid), 1, 1000)
	if len(after) != len(before) || len(call(t, a, "calendar.list_calendars", nil)["calendars"].([]any)) != 1 {
		t.Error("a dry run changed something")
	}
	// Every action in the manifest exists and answers.
	for _, ac := range a.manifest.Actions {
		if !strings.HasPrefix(ac.Name, "calendar.") {
			t.Errorf("action %s", ac.Name)
		}
	}
	if !strings.Contains(string(ManifestJSON), `"calendar.delete", "description"`) || !strings.Contains(string(ManifestJSON), `"approval": "agents"`) {
		t.Error("destructive actions need approval for agents")
	}
}

func TestNonSelfCallerRefused(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false)
	w := serve(a.Handler(), newReq("calendar.list_calendars", "{}", map[string]string{"Sol-Person": e.bob.ID, "Sol-Device": "x"}))
	if w.Code != 403 {
		t.Errorf("a stranger got %d", w.Code)
	}
}
