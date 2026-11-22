package tasks

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/crdt"
	"github.com/apv022/sol/sdk/go/sol"
)

func id(m map[string]any, k string) string {
	if t, ok := m[k].(map[string]any); ok {
		return t["id"].(string)
	}
	return ""
}

func listID(t *testing.T, a *App, name string) string {
	t.Helper()
	out := call(t, a, "tasks.create_list", map[string]any{"name": name, "color": "green"})
	return out["list"].(map[string]any)["id"].(string)
}

func TestParseQuickTable(t *testing.T) {
	raw, err := os.ReadFile("testdata/dates.json")
	if err != nil {
		t.Fatal(err)
	}
	var tab struct {
		Now   string
		Cases []struct {
			In, Title, Due, Time, Repeat, List string
			Flag                               bool
		}
	}
	if err := json.Unmarshal(raw, &tab); err != nil {
		t.Fatal(err)
	}
	now, _ := time.ParseInLocation("2006-01-02T15:04", tab.Now, time.UTC)
	for _, c := range tab.Cases {
		got := ParseQuick(c.In, now)
		want := Parsed{Title: c.Title, Due: c.Due, Time: c.Time, Repeat: c.Repeat, Flag: c.Flag, List: c.List}
		if got != want {
			t.Errorf("%q:\n got  %+v\n want %+v", c.In, got, want)
		}
	}
}

func TestNextDue(t *testing.T) {
	now, _ := time.ParseInLocation("2006-01-02T15:04", "2026-11-18T10:30", time.UTC)
	for _, c := range []struct{ due, rep, want string }{
		{"2026-11-18", "daily", "2026-11-19"},
		{"2026-11-10", "daily", "2026-11-19"}, // completed late: next one is still ahead
		{"2026-11-25", "daily", "2026-11-26"}, // completed early
		{"2026-11-18", "weekly", "2026-11-25"},
		{"2026-10-28", "weekly", "2026-11-25"},
		{"2026-01-31", "monthly", "2026-11-30"}, // clamps, counting from the 31st
		{"2026-11-30", "monthly", "2026-12-30"},
		{"2026-12-31", "monthly", "2027-01-31"},
		{"2024-02-29", "yearly", "2027-02-28"},
		{"", "weekly", "2026-11-25"},
		{"2026-11-18", "", ""},
	} {
		if got := NextDue(c.due, c.rep, now); got != c.want {
			t.Errorf("NextDue(%q,%q) = %q, want %q", c.due, c.rep, got, c.want)
		}
	}
}

func TestValidOp(t *testing.T) {
	st := crdt.Stamp{Wall: 1000, Device: "d"}
	ok := func(key, val string) bool {
		return validOp(crdt.MapOp{Key: key, Value: json.RawMessage(val), Stamp: st}, 2000)
	}
	if !ok("t.0123abcd.title", `"x"`) || !ok("l.color", `"red"`) || !ok("t.0123abcd.done", `1700000000000`) {
		t.Error("good ops rejected")
	}
	for _, c := range [][2]string{{"t.0123abcd.title", `5`}, {"t.XYZ.title", `"x"`}, {"x.y", `"x"`}, {"t.0123abcd.flag", `"yes"`}, {"g.abcdefghijk", `"admin"`}, {"t.0123abcd.title", `{`}} {
		if ok(c[0], c[1]) {
			t.Errorf("%v accepted", c)
		}
	}
	if validOp(crdt.MapOp{Key: "l.name", Value: json.RawMessage(`"x"`), Stamp: crdt.Stamp{Wall: 5000, Device: "d"}}, 2000) {
		t.Error("op from the far future accepted")
	}
	if validOp(crdt.MapOp{Key: "l.name", Value: json.RawMessage(`"x"`)}, 2000) {
		t.Error("op without a stamp accepted")
	}
}

// Two devices of one person edit the same task from the same starting point; every field merges
// on its own and both devices end up identical.
func TestConcurrentFieldEditsMerge(t *testing.T) {
	e := newEnv(t)
	d1 := start(t, e.alice.Devices[0], e.clk, "", false)
	d2 := start(t, e.alice.AddDevice("phone", false), e.clk, "", false)
	lid := listID(t, d1, "Home")
	tid := id(call(t, d1, "tasks.add", map[string]any{"list": lid, "title": "Fix tap"}), "task")
	if err := d2.sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Build both edits from the shared state, before either device reads the other's change.
	l1, _ := d1.findList(lid)
	l2, _ := d2.findList(lid)
	b1, b2 := d1.newBatch(), d2.newBatch()
	b1.task(tid, "title", "Fix the kitchen tap")
	b1.task(tid, "flag", true)
	b1.task(tid, "notes", "from laptop")
	e.clk.set(e.clk.Now().Add(time.Second))
	b2.task(tid, "due", "2026-11-21")
	b2.task(tid, "time", "09:00")
	b2.task(tid, "notes", "from phone") // same field as the laptop: the later write wins
	if err := d1.commit(t.Context(), l1, b1); err != nil {
		t.Fatal(err)
	}
	if err := d2.commit(t.Context(), l2, b2); err != nil {
		t.Fatal(err)
	}
	for _, d := range []*App{d1, d2} {
		if err := d.sync(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	g1 := call(t, d1, "tasks.get", map[string]any{"id": tid})["task"].(map[string]any)
	g2 := call(t, d2, "tasks.get", map[string]any{"id": tid})["task"].(map[string]any)
	if !reflect.DeepEqual(g1, g2) {
		t.Fatalf("devices differ:\n%v\n%v", g1, g2)
	}
	if g1["title"] != "Fix the kitchen tap" || g1["flag"] != true || g1["due"] != "2026-11-21" || g1["time"] != "09:00" {
		t.Errorf("a field was lost: %v", g1)
	}
	if g1["notes"] != "from phone" {
		t.Errorf("same-field conflict should go to the later write, got %q", g1["notes"])
	}
}

func TestCompleteRecurringSchedulesNext(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false)
	lid := listID(t, a, "Chores")
	tid := id(call(t, a, "tasks.add", map[string]any{"list": lid, "title": "Water plants", "due": "2026-11-18", "time": "08:00", "repeat": "weekly", "flag": true}), "task")
	sub := id(call(t, a, "tasks.add", map[string]any{"title": "Ferns", "parent": tid}), "task")
	out := call(t, a, "tasks.complete", map[string]any{"id": tid})
	next := out["next"].(map[string]any)
	if next["due"] != "2026-11-25" || next["time"] != "08:00" || next["repeat"] != "weekly" || next["flag"] != true || next["done"] != false {
		t.Fatalf("next occurrence: %v", next)
	}
	if out["task"].(map[string]any)["done"] != true {
		t.Fatal("original not done")
	}
	g := call(t, a, "tasks.get", map[string]any{"id": next["id"]})
	if subs := g["subtasks"].([]any); len(subs) != 1 || subs[0].(map[string]any)["title"] != "Ferns" || subs[0].(map[string]any)["id"] == sub {
		t.Errorf("next occurrence subtasks: %v", g["subtasks"])
	}
	open := tasksOf(t, call(t, a, "tasks.list", map[string]any{"list": lid}))
	if len(open) != 2 { // the new Water plants and its Ferns... (the old Ferns stays with the completed one)
		t.Logf("open tasks: %v", titles(open))
	}
	// Reopening does not schedule anything.
	call(t, a, "tasks.complete", map[string]any{"id": tid, "done": false})
	if n := len(tasksOf(t, call(t, a, "tasks.search", map[string]any{"query": "water"}))); n != 2 {
		t.Errorf("tasks named water: %d, want 2", n)
	}
}

// Two devices complete the same recurring task at the same moment: one next occurrence, not two.
func TestRecurringConcurrentCompleteConverges(t *testing.T) {
	e := newEnv(t)
	d1 := start(t, e.alice.Devices[0], e.clk, "", false)
	d2 := start(t, e.alice.AddDevice("phone", false), e.clk, "", false)
	lid := listID(t, d1, "Chores")
	tid := id(call(t, d1, "tasks.add", map[string]any{"list": lid, "title": "Bins", "due": "2026-11-18", "repeat": "daily"}), "task")
	_ = d2.sync(t.Context())
	l1, _ := d1.findList(lid)
	l2, _ := d2.findList(lid)
	t1, t2 := l1.byID[tid], l2.byID[tid]
	b1, _ := d1.completeOps(l1, t1, true)
	b2, _ := d2.completeOps(l2, t2, true)
	_ = d1.commit(t.Context(), l1, b1)
	_ = d2.commit(t.Context(), l2, b2)
	_ = d1.sync(t.Context())
	_ = d2.sync(t.Context())
	for _, d := range []*App{d1, d2} {
		open := tasksOf(t, call(t, d, "tasks.list", map[string]any{"list": lid}))
		if len(open) != 1 || open[0]["due"] != "2026-11-19" {
			t.Errorf("open tasks after double completion: %v", open)
		}
	}
}

func TestMoveAndReorder(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false)
	l1, l2 := listID(t, a, "One"), listID(t, a, "Two")
	var ids []string
	for _, n := range []string{"a", "b", "c", "d"} {
		ids = append(ids, id(call(t, a, "tasks.add", map[string]any{"list": l1, "title": n}), "task"))
	}
	order := func(list string) string {
		var top []map[string]any
		for _, x := range tasksOf(t, call(t, a, "tasks.list", map[string]any{"list": list})) {
			if x["parent"] == "" {
				top = append(top, x)
			}
		}
		return strings.Join(titles(top), "")
	}
	call(t, a, "tasks.move", map[string]any{"id": ids[3], "before": ids[0]})
	if got := order(l1); got != "dabc" {
		t.Fatalf("after move to front: %s", got)
	}
	call(t, a, "tasks.move", map[string]any{"id": ids[0], "after": ids[2]})
	if got := order(l1); got != "dbca" {
		t.Fatalf("after move to end: %s", got)
	}
	// Many insertions into the same gap force a renumbering; the order must stay right.
	for i := 0; i < 60; i++ {
		call(t, a, "tasks.move", map[string]any{"id": ids[0], "before": ids[1]})
		call(t, a, "tasks.move", map[string]any{"id": ids[0], "after": ids[3]})
	}
	if got := order(l1); got != "dabc" {
		t.Fatalf("after many moves: %s", got)
	}
	sub := id(call(t, a, "tasks.add", map[string]any{"title": "sub", "parent": ids[1]}), "task")
	call(t, a, "tasks.move", map[string]any{"id": ids[1], "list": l2})
	if order(l1) != "dac" || order(l2) != "b" {
		t.Fatalf("after move to list two: %s / %s", order(l1), order(l2))
	}
	g := call(t, a, "tasks.get", map[string]any{"id": ids[1]})
	if g["list"].(map[string]any)["id"] != l2 || len(g["subtasks"].([]any)) != 1 {
		t.Errorf("moved task: %v", g)
	}
	_ = sub
}

func TestSmartListsAndSearch(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false)
	lid := listID(t, a, "Mixed")
	add := func(args map[string]any) string {
		args["list"] = lid
		return id(call(t, a, "tasks.add", args), "task")
	}
	add(map[string]any{"title": "overdue", "due": "2026-11-10"})
	add(map[string]any{"title": "today timed", "due": "2026-11-18", "time": "16:00"})
	add(map[string]any{"title": "later", "due": "2026-12-01", "flag": true})
	add(map[string]any{"title": "someday"})
	d := add(map[string]any{"title": "finished", "notes": "had some Milk"})
	call(t, a, "tasks.complete", map[string]any{"id": d})
	want := map[string]string{"today": "overdue,today timed", "scheduled": "overdue,today timed,later", "flagged": "later", "all": "overdue,today timed,later,someday", "completed": "finished"}
	for smart, w := range want {
		got := strings.Join(titles(tasksOf(t, call(t, a, "tasks.list", map[string]any{"smart": smart}))), ",")
		if got != w {
			t.Errorf("smart %s = %q, want %q", smart, got, w)
		}
	}
	today := tasksOf(t, call(t, a, "tasks.today", nil))
	if len(today) != 2 || today[0]["overdue"] != true || today[1]["overdue"] != false {
		t.Errorf("today: %v", today)
	}
	if got := titles(tasksOf(t, call(t, a, "tasks.search", map[string]any{"query": "MILK finished"}))); len(got) != 1 {
		t.Errorf("search: %v", got)
	}
	out := call(t, a, "tasks.list_lists", nil)
	lists := out["lists"].([]any)
	if lists[0].(map[string]any)["count"] != float64(4) {
		t.Errorf("list count: %v", lists[0])
	}
}

func TestQuickAddAction(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false)
	listID(t, a, "Family")
	g := listID(t, a, "Groceries")
	out := call(t, a, "tasks.add", map[string]any{"quick": "milk every monday 9am #groceries !"})
	task := out["task"].(map[string]any)
	if task["title"] != "milk" || task["due"] != "2026-11-23" || task["time"] != "09:00" || task["repeat"] != "weekly" || task["flag"] != true || task["list"] != g {
		t.Errorf("quick add: %v", task)
	}
	// No list given and none tagged: it lands in the first list.
	out = call(t, a, "tasks.add", map[string]any{"quick": "call mom tomorrow"})
	if out["task"].(map[string]any)["list_name"] != "Family" {
		t.Errorf("default list: %v", out["task"])
	}
	for _, bad := range []map[string]any{{"title": ""}, {"title": "x", "due": "tomorrow"}, {"title": "x", "time": "9am"}, {"title": "x", "time": "09:00"}, {"title": "x", "repeat": "hourly"}, {"title": "x", "list": "nope"}} {
		if _, err := tryCall(a, "tasks.add", bad); err == nil {
			t.Errorf("add %v accepted", bad)
		}
	}
}

func TestMarkdownRoundTrip(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false)
	lid := listID(t, a, "Trip")
	p := id(call(t, a, "tasks.add", map[string]any{"list": lid, "title": "Pack", "due": "2026-11-21", "time": "09:00", "repeat": "weekly", "flag": true, "notes": "bag\n\nand charger"}), "task")
	call(t, a, "tasks.add", map[string]any{"title": "Socks", "parent": p})
	s2 := id(call(t, a, "tasks.add", map[string]any{"title": "Shirts", "parent": p}), "task")
	call(t, a, "tasks.complete", map[string]any{"id": s2})
	call(t, a, "tasks.add", map[string]any{"list": lid, "title": "Plain one"})
	done := id(call(t, a, "tasks.add", map[string]any{"list": lid, "title": "Book hotel (confirmed)", "due": "2026-11-19"}), "task")
	call(t, a, "tasks.complete", map[string]any{"id": done})

	md := call(t, a, "tasks.export", map[string]any{"list": "trip"})["markdown"].(string)
	want := "# Trip\n\n- [ ] Pack (due 2026-11-21 09:00, repeats weekly, flagged)\n  > bag\n  > \n  > and charger\n  - [ ] Socks\n  - [x] Shirts\n- [ ] Plain one\n- [x] Book hotel (confirmed) (due 2026-11-19)\n"
	if md != want {
		t.Fatalf("export:\n%s\nwant:\n%s", md, want)
	}
	imp := call(t, a, "tasks.import", map[string]any{"markdown": md, "name": "Trip copy"})
	if imp["added"] != float64(5) {
		t.Fatalf("import: %v", imp)
	}
	if again := call(t, a, "tasks.export", map[string]any{"list": "Trip copy"})["markdown"].(string); again != strings.Replace(want, "# Trip", "# Trip copy", 1) {
		t.Fatalf("round trip differs:\n%s", again)
	}
	// Into an existing list, hand-written and sloppy: unknown bits are ignored, not errors.
	imp = call(t, a, "tasks.import", map[string]any{"list": lid, "markdown": "intro text\n* [X] star bullet (due 2026-12-01 9:30)\n- [ ] a (b)\nnothing\n"})
	if imp["added"] != float64(2) {
		t.Fatalf("sloppy import: %v", imp)
	}
	got := tasksOf(t, call(t, a, "tasks.search", map[string]any{"query": "star"}))
	if len(got) != 1 || got[0]["due"] != "2026-12-01" || got[0]["time"] != "09:30" || got[0]["done"] != true {
		t.Errorf("sloppy item: %v", got)
	}
	if got := tasksOf(t, call(t, a, "tasks.search", map[string]any{"query": "a (b)"})); len(got) != 1 {
		t.Errorf("unknown parenthetical should stay in the title: %v", got)
	}
	if _, err := tryCall(a, "tasks.import", map[string]any{"markdown": "no items"}); err == nil {
		t.Error("import without items accepted")
	}
}

func TestRemindersFireOnce(t *testing.T) {
	e := newEnv(t)
	data := t.TempDir()
	a := start(t, e.alice.Devices[0], e.clk, data, false)
	lid := listID(t, a, "Calls")
	call(t, a, "tasks.add", map[string]any{"list": lid, "title": "Call mom", "due": "2026-11-18", "time": "14:00"})
	call(t, a, "tasks.add", map[string]any{"list": lid, "title": "No time", "due": "2026-11-18"})
	call(t, a, "tasks.add", map[string]any{"list": lid, "title": "Tomorrow", "due": "2026-11-19", "time": "08:00"})
	fin := id(call(t, a, "tasks.add", map[string]any{"list": lid, "title": "Done already", "due": "2026-11-18", "time": "13:00"}), "task")
	call(t, a, "tasks.complete", map[string]any{"id": fin})
	notices := func() []string {
		var out []string
		for _, it := range e.alice.Inbox() {
			if it.Kind == "notice" {
				out = append(out, it.Title)
			}
		}
		return out
	}
	if len(notices()) != 0 {
		t.Fatal("notice before the due time")
	}
	if got, want := e.clk.armed(), time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC); !got.Equal(want) && !got.Equal(want.Add(time.Hour)) {
		t.Errorf("timer armed for %v", got)
	}
	// 14:00: the timer fires and posts exactly one notice.
	e.clk.set(time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC))
	if got := notices(); !reflect.DeepEqual(got, []string{"Call mom"}) {
		t.Fatalf("notices at 14:00 = %v", got)
	}
	if got, want := e.clk.armed(), time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("next timer %v, want %v", got, want)
	}
	// Changes re-arm without repeating.
	call(t, a, "tasks.add", map[string]any{"list": lid, "title": "Walk", "due": "2026-11-18", "time": "14:20"})
	e.clk.set(time.Date(2026, 11, 18, 14, 20, 0, 0, time.UTC))
	if got := notices(); !reflect.DeepEqual(got, []string{"Call mom", "Walk"}) {
		t.Fatalf("notices at 14:20 = %v", got)
	}
	if it := e.alice.Inbox()[0]; it.Title != "Call mom" || !strings.Contains(it.Body, "14:00") || !strings.Contains(it.Body, "Calls") {
		t.Errorf("notice content: %+v", it)
	}
	// A restart does not repeat what was already delivered, and drops reminders that are long past.
	a.Stop()
	e.clk.set(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)) // "Tomorrow" 08:00 passed hours ago, while the app was off
	b := start(t, e.alice.Devices[0], e.clk, data, false)
	_ = b
	if got := notices(); len(got) != 2 {
		t.Errorf("restart notices = %v", got)
	}
	if !e.clk.armed().IsZero() {
		t.Errorf("a timer is armed with nothing left: %v", e.clk.armed())
	}
	// Within the grace period a missed reminder is still delivered on start.
	call(t, b, "tasks.add", map[string]any{"list": lid, "title": "Just missed", "due": "2026-11-19", "time": "11:50"})
	if got := notices(); len(got) != 3 || got[2] != "Just missed" {
		t.Errorf("late reminder: %v", got)
	}
}

func TestSharedListRoundTrip(t *testing.T) {
	e := newEnv(t)
	alice := start(t, e.alice.Devices[0], e.clk, "", true)
	bob := start(t, e.bob.Devices[0], e.clk, "", true)
	carol := start(t, e.car.Devices[0], e.clk, "", true)
	lid := listID(t, alice, "Groceries")
	milk := id(call(t, alice, "tasks.add", map[string]any{"list": lid, "title": "Milk"}), "task")

	// Nothing is visible until it is shared.
	if n := len(call(t, bob, "tasks.list_lists", nil)["lists"].([]any)); n != 0 {
		t.Fatalf("bob sees %d lists before sharing", n)
	}
	call(t, alice, "tasks.share", map[string]any{"list": lid, "person": "Bob", "mode": "write"}, true)
	if n := len(call(t, bob, "tasks.list_lists", nil)["lists"].([]any)); n != 0 {
		t.Fatalf("a dry run shared the list")
	}
	call(t, alice, "tasks.share", map[string]any{"list": lid, "person": "Bob", "mode": "write"})
	call(t, alice, "tasks.share", map[string]any{"list": lid, "person": e.car.ID, "mode": "read"})
	eventually(t, "bob to see the list", func() bool {
		return len(call(t, bob, "tasks.list_lists", nil)["lists"].([]any)) == 1
	})
	bl := call(t, bob, "tasks.list_lists", nil)["lists"].([]any)[0].(map[string]any)
	if bl["name"] != "Groceries" || bl["mode"] != "write" || bl["owner"] != e.alice.ID || bl["shared"] != true {
		t.Fatalf("bob's list: %v", bl)
	}
	if got := titles(tasksOf(t, call(t, bob, "tasks.list", map[string]any{"list": lid}))); !reflect.DeepEqual(got, []string{"Milk"}) {
		t.Fatalf("bob sees %v", got)
	}
	// Until Bob writes, Alice cannot read his feed (the write share does not expose it).
	bobFeed := sol.Address(e.bob.ID, "tasks", "list."+lid)
	if _, err := alice.api.Read(t.Context(), bobFeed, 1, 10); err == nil {
		t.Fatal("alice could read bob's feed before he shared it back")
	}
	// Bob edits: completes Milk, adds Eggs, renames the list.
	call(t, bob, "tasks.complete", map[string]any{"id": milk})
	eggs := id(call(t, bob, "tasks.add", map[string]any{"list": lid, "title": "Eggs", "due": "2026-11-19"}), "task")
	call(t, bob, "tasks.update_list", map[string]any{"list": lid, "name": "Groceries (shared)"})
	eventually(t, "alice to see bob's edits", func() bool {
		ts := tasksOf(t, call(t, alice, "tasks.list", map[string]any{"list": lid, "completed": true}))
		return len(ts) == 2
	})
	got := call(t, alice, "tasks.get", map[string]any{"id": eggs})
	if got["task"].(map[string]any)["due"] != "2026-11-19" || got["list"].(map[string]any)["name"] != "Groceries (shared)" {
		t.Errorf("alice's view of bob's edit: %v", got)
	}
	if call(t, alice, "tasks.get", map[string]any{"id": milk})["task"].(map[string]any)["done"] != true {
		t.Error("bob's completion not seen by alice")
	}
	// Alice edits too; Bob follows.
	call(t, alice, "tasks.update", map[string]any{"id": eggs, "title": "Free-range eggs"})
	eventually(t, "bob to see alice's edit", func() bool {
		return call(t, bob, "tasks.get", map[string]any{"id": eggs})["task"].(map[string]any)["title"] == "Free-range eggs"
	})
	// Bob's ops were relayed into Alice's own feed, so a collaborator who only reads Alice's feed sees them.
	entries, _ := alice.api.Read(t.Context(), sol.Address(e.alice.ID, "tasks", "list."+lid), 1, 100)
	relayed := false
	for _, en := range entries {
		relayed = relayed || strings.Contains(string(en.Body), "Eggs")
	}
	if !relayed {
		t.Error("bob's task was not relayed into the owner's feed")
	}

	// Carol has read access only: she sees the list, cannot change it.
	eventually(t, "carol to see the list", func() bool {
		return len(call(t, carol, "tasks.list_lists", nil)["lists"].([]any)) == 1
	})
	for name, args := range map[string]map[string]any{
		"tasks.add":    {"list": lid, "title": "Sneaky"},
		"tasks.update": {"id": eggs, "title": "Hacked"},
		"tasks.delete": {"ids": []string{eggs}},
	} {
		if _, err := tryCall(carol, name, args); err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Errorf("carol %s: %v", name, err)
		}
	}
	// Even a forged co-author feed from Carol (shared back to Alice) is ignored: Alice holds no write grant for her.
	forged := crdt.NewMap()
	op, _ := forged.Set(carol.crdtClk, "t."+eggs+".title", "Hacked")
	del, _ := forged.Set(carol.crdtClk, "l.deleted", true)
	own, _ := forged.Set(carol.crdtClk, "g."+e.car.ID, "write")
	if _, err := carol.api.Append(t.Context(), "list."+lid, map[string]any{"ops": []crdt.MapOp{op, del, own}}); err != nil {
		t.Fatal(err)
	}
	if err := carol.api.Share(t.Context(), sol.ShareRequest{Feed: "list." + lid, People: []string{e.alice.ID}, Mode: sol.ModeRead}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if g := call(t, alice, "tasks.get", map[string]any{"id": eggs}); g["task"].(map[string]any)["title"] != "Free-range eggs" {
		t.Errorf("forged op applied: %v", g)
	}
	// Only the owner can share or delete the list for everyone.
	if _, err := tryCall(bob, "tasks.share", map[string]any{"list": lid, "person": "Alice"}); err == nil {
		t.Error("a co-author shared the list")
	}
	// Alice stops sharing with Bob: his list disappears.
	call(t, alice, "tasks.share", map[string]any{"list": lid, "person": "Bob", "mode": "none"})
	eventually(t, "bob to lose the list", func() bool {
		return len(call(t, bob, "tasks.list_lists", nil)["lists"].([]any)) == 0
	})
	// Carol leaves: the owner keeps the list.
	call(t, carol, "tasks.delete_list", map[string]any{"list": lid})
	if n := len(call(t, carol, "tasks.list_lists", nil)["lists"].([]any)); n != 0 {
		t.Errorf("carol still sees %d lists", n)
	}
	if n := len(call(t, alice, "tasks.list_lists", nil)["lists"].([]any)); n != 1 {
		t.Errorf("alice sees %d lists", n)
	}
}

func TestEveryActionHasDryRun(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false)
	lid := listID(t, a, "Base")
	tid := id(call(t, a, "tasks.add", map[string]any{"list": lid, "title": "Existing", "due": "2026-11-20", "repeat": "daily"}), "task")
	other := listID(t, a, "Other")
	snap := func() string {
		b, _ := json.Marshal(call(t, a, "tasks.snapshot", nil)["tasks"])
		l, _ := json.Marshal(call(t, a, "tasks.list_lists", nil))
		return string(b) + string(l)
	}
	before := snap()
	writes := map[string]map[string]any{
		"tasks.create_list": {"name": "New"},
		"tasks.update_list": {"list": "Base", "name": "Renamed", "color": "red"},
		"tasks.delete_list": {"list": "Base"},
		"tasks.add":         {"list": lid, "quick": "x tomorrow"},
		"tasks.update":      {"id": tid, "title": "Changed", "due": ""},
		"tasks.complete":    {"id": tid},
		"tasks.delete":      {"ids": []string{tid}},
		"tasks.move":        {"id": tid, "list": other},
		"tasks.share":       {"list": lid, "person": "Bob", "mode": "write"},
		"tasks.import":      {"markdown": "- [ ] x", "list": lid},
	}
	reads := map[string]map[string]any{
		"tasks.list_lists": nil, "tasks.list": {"smart": "today"}, "tasks.get": {"id": tid}, "tasks.search": {"query": "exist"},
		"tasks.today": nil, "tasks.export": {"list": lid}, "tasks.snapshot": nil,
	}
	m, _ := sol.ParseManifest(ManifestJSON)
	for _, spec := range m.Actions {
		if w, ok := writes[spec.Name]; ok {
			if spec.Effect == sol.EffectRead {
				t.Errorf("%s writes but is declared read", spec.Name)
			}
			out := call(t, a, spec.Name, w, true)
			if out["dry_run"] != true || out["would"] == nil {
				t.Errorf("%s dry run answered %v", spec.Name, out)
			}
			if after := snap(); after != before {
				t.Errorf("%s changed state in a dry run", spec.Name)
			}
		} else if r, ok := reads[spec.Name]; ok {
			if spec.Effect != sol.EffectRead {
				t.Errorf("%s is declared %s but only reads", spec.Name, spec.Effect)
			}
			call(t, a, spec.Name, r)
		} else {
			t.Errorf("action %s is not covered by this test", spec.Name)
		}
	}
	for _, n := range []string{"tasks.delete", "tasks.delete_list"} {
		s, _ := m.Action(n)
		if s.Effect != sol.EffectDestructive || s.Approval != sol.ApprovalAgents {
			t.Errorf("%s must be destructive with approval for agents", n)
		}
	}
	// Real calls still work after the dry runs, and non-self callers are refused.
	call(t, a, "tasks.delete", map[string]any{"ids": []string{tid}})
	if n := len(tasksOf(t, call(t, a, "tasks.list", map[string]any{"list": lid}))); n != 0 {
		t.Errorf("task not deleted: %d left", n)
	}
	call(t, a, "tasks.delete_list", map[string]any{"list": other})
	if n := len(call(t, a, "tasks.list_lists", nil)["lists"].([]any)); n != 1 {
		t.Errorf("lists left: %d", n)
	}
}

func TestNonSelfCallerRefused(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false)
	h := a.Handler()
	r := newReq("tasks.list_lists", "{}", map[string]string{"Sol-Person": e.bob.ID})
	w := serve(h, r)
	if w.Code != 403 {
		t.Errorf("non-self call: %d %s", w.Code, w.Body)
	}
}
