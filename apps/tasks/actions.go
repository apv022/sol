package tasks

import (
	"context"
	"sort"
	"strings"

	"github.com/apv022/sol/sdk/go/sol"
)

// Every action reads the feeds that have news first (a.fresh), so what it returns and what it
// refuses are current. Every write takes a dry run: it resolves and validates everything exactly
// as the real call would and then only describes the change.

func (a *App) registerActions(act *sol.Actions) {
	h := func(name string, f func(ctx context.Context, c sol.Call) (any, error)) {
		act.Handle(name, func(ctx context.Context, c sol.Call) (any, error) {
			if !c.Caller.Self {
				return nil, sol.Errorf("forbidden", "Tasks is for my own devices and agents only")
			}
			if err := a.fresh(ctx); err != nil {
				return nil, err
			}
			return f(ctx, c)
		})
	}
	h("tasks.list_lists", a.actListLists)
	h("tasks.create_list", a.actCreateList)
	h("tasks.update_list", a.actUpdateList)
	h("tasks.delete_list", a.actDeleteList)
	h("tasks.list", a.actList)
	h("tasks.get", a.actGet)
	h("tasks.add", a.actAdd)
	h("tasks.update", a.actUpdate)
	h("tasks.complete", a.actComplete)
	h("tasks.delete", a.actDelete)
	h("tasks.move", a.actMove)
	h("tasks.search", a.actSearch)
	h("tasks.today", a.actToday)
	h("tasks.share", a.actShare)
	h("tasks.export", a.actExport)
	h("tasks.import", a.actImport)
	h("tasks.snapshot", a.actSnapshot)
}

// fresh folds whatever feed entries the events stream has not delivered yet.
func (a *App) fresh(ctx context.Context) error {
	if a.crdtClk == nil {
		return sol.Errorf("unavailable", "Tasks has not started")
	}
	return a.sync(ctx)
}

func dry(would any) map[string]any { return map[string]any{"dry_run": true, "would": would} }

// ---------- lookups ----------

func (a *App) findList(ref string) (*listState, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, bad("which list?")
	}
	lists := a.view()
	for _, ls := range lists {
		if ls.id == ref {
			return ls, nil
		}
	}
	var hit *listState
	for _, ls := range lists {
		if strings.EqualFold(ls.info.Name, ref) {
			if hit != nil {
				return nil, bad("more than one list is named %q: use its id", ref)
			}
			hit = ls
		}
	}
	if hit == nil {
		return nil, missing("list " + ref)
	}
	return hit, nil
}

func (a *App) findTask(id string) (*listState, *Task, error) {
	for _, ls := range a.view() {
		if t := ls.byID[id]; t != nil {
			return ls, t, nil
		}
	}
	return nil, nil, missing("task " + id)
}

// writableDefault is the list a task lands in when none is named: the first list I may write to.
func (a *App) writableDefault() *listState {
	for _, ls := range a.view() {
		if ls.mode != modeRead {
			return ls
		}
	}
	return nil
}

// decorate returns a copy of t with the fields that depend on today.
func (a *App) decorate(t *Task) Task {
	c := *t
	c.Overdue = !c.Done && c.Due != "" && c.Due < a.now().Format(dateLayout)
	return c
}

func (a *App) decorateAll(ts []*Task) []Task {
	out := make([]Task, 0, len(ts))
	for _, t := range ts {
		out = append(out, a.decorate(t))
	}
	return out
}

// smart returns the tasks of a smart list.
func (a *App) smart(name string) ([]*Task, error) {
	today := a.now().Format(dateLayout)
	var out []*Task
	for _, ls := range a.view() {
		for _, t := range ls.tasks {
			ok := false
			switch name {
			case "today":
				ok = !t.Done && t.Due != "" && t.Due <= today
			case "scheduled":
				ok = !t.Done && t.Due != ""
			case "flagged":
				ok = !t.Done && t.Flag
			case "all":
				ok = !t.Done
			case "completed":
				ok = t.Done
			default:
				return nil, bad("unknown smart list %q", name)
			}
			if ok {
				out = append(out, t)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i], out[j]
		switch name {
		case "completed":
			return x.DoneAt > y.DoneAt
		case "today", "scheduled":
			if x.Due != y.Due {
				return x.Due < y.Due
			}
			if x.Time != y.Time {
				return x.Time < y.Time
			}
		}
		return false
	})
	return out, nil
}

var smartNames = []struct{ id, name string }{{"today", "Today"}, {"scheduled", "Scheduled"}, {"flagged", "Flagged"}, {"all", "All"}, {"completed", "Completed"}}

func (a *App) listInfos() []List {
	out := []List{}
	for _, ls := range a.view() {
		out = append(out, ls.info)
	}
	return out
}

// ---------- reads ----------

func (a *App) actListLists(_ context.Context, _ sol.Call) (any, error) {
	type smart struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	var sm []smart
	for _, s := range smartNames {
		ts, _ := a.smart(s.id)
		sm = append(sm, smart{s.id, s.name, len(ts)})
	}
	return map[string]any{"lists": a.listInfos(), "smart": sm}, nil
}

func (a *App) actSnapshot(_ context.Context, _ sol.Call) (any, error) {
	var all []Task
	for _, ls := range a.view() {
		all = append(all, a.decorateAll(ls.tasks)...)
	}
	if all == nil {
		all = []Task{}
	}
	a.mu.RLock()
	rev := a.rev
	a.mu.RUnlock()
	return map[string]any{"rev": rev, "today": a.now().Format(dateLayout), "lists": a.listInfos(), "tasks": all}, nil
}

func (a *App) actList(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		List      string `json:"list"`
		Smart     string `json:"smart"`
		Completed bool   `json:"completed"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	switch {
	case in.Smart != "":
		ts, err := a.smart(in.Smart)
		if err != nil {
			return nil, err
		}
		return map[string]any{"tasks": a.decorateAll(ts)}, nil
	case in.List != "":
		ls, err := a.findList(in.List)
		if err != nil {
			return nil, err
		}
		var ts []*Task
		for _, t := range ls.tasks {
			if in.Completed || !t.Done {
				ts = append(ts, t)
			}
		}
		return map[string]any{"list": ls.info, "tasks": a.decorateAll(ts)}, nil
	}
	ts, _ := a.smart("all")
	return map[string]any{"tasks": a.decorateAll(ts)}, nil
}

func (a *App) actGet(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	ls, t, err := a.findTask(in.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"task": a.decorate(t), "subtasks": a.decorateAll(ls.subtasksOf(t.ID)), "list": ls.info}, nil
}

func (a *App) actToday(_ context.Context, _ sol.Call) (any, error) {
	ts, _ := a.smart("today")
	return map[string]any{"date": a.now().Format(dateLayout), "tasks": a.decorateAll(ts)}, nil
}

func (a *App) actSearch(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		Query string `json:"query"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	words := strings.Fields(strings.ToLower(in.Query))
	if len(words) == 0 {
		return nil, bad("search for something")
	}
	var out []*Task
	for _, ls := range a.view() {
		for _, t := range ls.tasks {
			hay := strings.ToLower(t.Title + "\n" + t.Notes + "\n" + ls.info.Name)
			ok := true
			for _, w := range words {
				ok = ok && strings.Contains(hay, w)
			}
			if ok {
				out = append(out, t)
			}
		}
	}
	return map[string]any{"tasks": a.decorateAll(out)}, nil
}

func (a *App) actExport(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		List string `json:"list"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	ls, err := a.findList(in.List)
	if err != nil {
		return nil, err
	}
	return map[string]any{"name": ls.info.Name, "markdown": exportMarkdown(ls)}, nil
}

// ---------- lists ----------

func checkListFields(name, color, icon string, nameRequired bool) error {
	if nameRequired && strings.TrimSpace(name) == "" {
		return bad("a list needs a name")
	}
	if len(name) > 200 {
		return bad("name too long")
	}
	if color != "" && !palette[color] {
		return bad("color must be one of %s", strings.Join(sortedKeys(palette), ", "))
	}
	if icon != "" && !validIcon(icon) {
		return bad("bad icon name")
	}
	return nil
}

func (a *App) actCreateList(ctx context.Context, c sol.Call) (any, error) {
	var in struct{ Name, Color, Icon string }
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	in.Name = oneLine(in.Name)
	if err := checkListFields(in.Name, in.Color, in.Icon, true); err != nil {
		return nil, err
	}
	if in.Color == "" {
		in.Color = "blue"
	}
	if in.Icon == "" {
		in.Icon = "list"
	}
	if c.DryRun {
		return dry(map[string]any{"create_list": in.Name, "color": in.Color, "icon": in.Icon}), nil
	}
	id, err := a.createList(ctx, in.Name, in.Color, in.Icon)
	if err != nil {
		return nil, err
	}
	ls, err := a.findList(id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"list": ls.info}, nil
}

func (a *App) actUpdateList(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		List  string  `json:"list"`
		Name  *string `json:"name"`
		Color *string `json:"color"`
		Icon  *string `json:"icon"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	ls, err := a.findList(in.List)
	if err != nil {
		return nil, err
	}
	var name, color, icon string
	b := a.newBatch()
	if in.Name != nil {
		name = oneLine(*in.Name)
	}
	if in.Color != nil {
		color = *in.Color
	}
	if in.Icon != nil {
		icon = *in.Icon
	}
	if err := checkListFields(name, color, icon, in.Name != nil); err != nil {
		return nil, err
	}
	if in.Name != nil {
		b.set("l.name", name)
	}
	if in.Color != nil && color != "" {
		b.set("l.color", color)
	}
	if in.Icon != nil && icon != "" {
		b.set("l.icon", icon)
	}
	if c.DryRun {
		if ls.mode == modeRead {
			return nil, sol.Errorf("forbidden", "this list is shared with you read-only")
		}
		return dry(map[string]any{"update_list": ls.info.Name, "name": name, "color": color, "icon": icon}), nil
	}
	if err := a.commit(ctx, ls, b); err != nil {
		return nil, err
	}
	ls, _ = a.findList(ls.id)
	return map[string]any{"list": ls.info}, nil
}

func (a *App) actDeleteList(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		List string `json:"list"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	ls, err := a.findList(in.List)
	if err != nil {
		return nil, err
	}
	if c.DryRun {
		what := "delete the list for everyone"
		if ls.owner != a.me {
			what = "leave the list (the owner keeps it)"
		}
		return dry(map[string]any{"delete_list": ls.info.Name, "action": what, "tasks": len(ls.tasks)}), nil
	}
	if ls.owner == a.me {
		b := a.newBatch()
		b.set("l.deleted", true)
		if err := a.commit(ctx, ls, b); err != nil {
			return nil, err
		}
	} else {
		a.mu.Lock()
		a.hidden[ls.key] = true
		a.dirty = true
		a.mu.Unlock()
		if a.data != "" {
			if err := saveJSON(a.data+"/hidden.json", a.hidden); err != nil {
				a.logf("tasks: saving hidden lists: %v", err)
			}
		}
		_ = a.api.Unshare(ctx, sol.ShareRequest{Feed: ls.feed, People: []string{ls.owner}, Mode: sol.ModeRead})
		a.rearm()
	}
	return map[string]any{"deleted": ls.info.Name}, nil
}

// ---------- tasks ----------

func (a *App) actAdd(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		List, Title, Quick, Notes, Due, Time, Repeat, Parent string
		Flag                                                 *bool
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	t := taskIn{Title: in.Title, Notes: in.Notes, Due: in.Due, Time: in.Time, Repeat: in.Repeat, Parent: in.Parent}
	list := in.List
	if in.Quick != "" {
		p := ParseQuick(in.Quick, a.now())
		if t.Title == "" {
			t.Title = p.Title
		}
		if t.Due == "" && in.Time == "" {
			t.Due, t.Time = p.Due, p.Time
		}
		if t.Repeat == "" {
			t.Repeat = p.Repeat
		}
		t.Flag = p.Flag
		if list == "" {
			list = p.List
		}
	}
	if in.Flag != nil {
		t.Flag = *in.Flag
	}
	t.Title = oneLine(t.Title)
	if err := t.check(); err != nil {
		return nil, err
	}
	var ls *listState
	if t.Parent != "" {
		pl, pt, err := a.findTask(t.Parent)
		if err != nil {
			return nil, err
		}
		if pt.Parent != "" {
			return nil, bad("subtasks go one level deep")
		}
		ls = pl
	} else if list != "" {
		var err error
		if ls, err = a.findList(list); err != nil {
			return nil, err
		}
	} else {
		ls = a.writableDefault()
	}
	if ls == nil {
		if c.DryRun {
			return dry(map[string]any{"create_list": "Tasks", "add": t.Title}), nil
		}
		id, err := a.createList(ctx, "Tasks", "blue", "list")
		if err != nil {
			return nil, err
		}
		if ls, err = a.findList(id); err != nil {
			return nil, err
		}
	}
	if ls.mode == modeRead {
		return nil, sol.Errorf("forbidden", "this list is shared with you read-only")
	}
	if c.DryRun {
		return dry(map[string]any{"add": t, "list": ls.info.Name}), nil
	}
	id := randID()
	b := a.newBatch()
	b.addTask(ls, id, t, ls.endOrder(t.Parent))
	if err := a.commit(ctx, ls, b); err != nil {
		return nil, err
	}
	_, nt, err := a.findTask(id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"task": a.decorate(nt)}, nil
}

func (a *App) actUpdate(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		ID     string  `json:"id"`
		Title  *string `json:"title"`
		Notes  *string `json:"notes"`
		Due    *string `json:"due"`
		Time   *string `json:"time"`
		Flag   *bool   `json:"flag"`
		Repeat *string `json:"repeat"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	ls, t, err := a.findTask(in.ID)
	if err != nil {
		return nil, err
	}
	if ls.mode == modeRead {
		return nil, sol.Errorf("forbidden", "this list is shared with you read-only")
	}
	// Validate the task as it will be, so a time can never outlive its date.
	n := taskIn{Title: t.Title, Notes: t.Notes, Due: t.Due, Time: t.Time, Repeat: t.Repeat, Flag: t.Flag}
	b := a.newBatch()
	if in.Title != nil {
		n.Title = oneLine(*in.Title)
		b.task(t.ID, "title", n.Title)
	}
	if in.Notes != nil {
		n.Notes = *in.Notes
		b.task(t.ID, "notes", n.Notes)
	}
	if in.Due != nil {
		n.Due = *in.Due
		if n.Due == "" {
			n.Time = ""
			b.task(t.ID, "time", "")
		}
		b.task(t.ID, "due", n.Due)
	}
	if in.Time != nil {
		n.Time = *in.Time
		b.task(t.ID, "time", n.Time)
	}
	if in.Flag != nil {
		n.Flag = *in.Flag
		b.task(t.ID, "flag", n.Flag)
	}
	if in.Repeat != nil {
		n.Repeat = *in.Repeat
		b.task(t.ID, "repeat", n.Repeat)
	}
	if err := n.check(); err != nil {
		return nil, err
	}
	if c.DryRun {
		return dry(map[string]any{"update": t.ID, "result": n}), nil
	}
	if err := a.commit(ctx, ls, b); err != nil {
		return nil, err
	}
	_, nt, err := a.findTask(t.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"task": a.decorate(nt)}, nil
}

func (a *App) actComplete(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		ID   string `json:"id"`
		Done *bool  `json:"done"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	done := in.Done == nil || *in.Done
	ls, t, err := a.findTask(in.ID)
	if err != nil {
		return nil, err
	}
	if ls.mode == modeRead {
		return nil, sol.Errorf("forbidden", "this list is shared with you read-only")
	}
	b, next := a.completeOps(ls, t, done)
	if c.DryRun {
		w := map[string]any{"task": t.ID, "done": done}
		if next != "" {
			w["next_due"] = NextDue(t.Due, t.Repeat, a.now())
		}
		return dry(w), nil
	}
	if err := a.commit(ctx, ls, b); err != nil {
		return nil, err
	}
	out := map[string]any{}
	if _, nt, err := a.findTask(t.ID); err == nil {
		out["task"] = a.decorate(nt)
	}
	if next != "" {
		if _, nt, err := a.findTask(next); err == nil {
			out["next"] = a.decorate(nt)
		}
	}
	return out, nil
}

func (a *App) actDelete(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	byList := map[string][]*Task{}
	lists := map[string]*listState{}
	var titles []string
	for _, id := range in.IDs {
		ls, t, err := a.findTask(id)
		if err != nil {
			return nil, err
		}
		if ls.mode == modeRead {
			return nil, sol.Errorf("forbidden", "list "+ls.info.Name+" is shared with you read-only")
		}
		byList[ls.key] = append(byList[ls.key], t)
		lists[ls.key] = ls
		titles = append(titles, t.Title)
	}
	if c.DryRun {
		return dry(map[string]any{"delete": titles}), nil
	}
	for _, k := range sortedKeys(byList) {
		if err := a.deleteOps(ctx, lists[k], byList[k]); err != nil {
			return nil, err
		}
	}
	return map[string]any{"deleted": len(in.IDs)}, nil
}

func (a *App) actMove(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		ID, List, Before, After string
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	src, t, err := a.findTask(in.ID)
	if err != nil {
		return nil, err
	}
	dst := src
	if in.List != "" {
		if dst, err = a.findList(in.List); err != nil {
			return nil, err
		}
	}
	for _, other := range []string{in.Before, in.After} {
		if other == "" {
			continue
		}
		if o := dst.byID[other]; o == nil || o.ID == t.ID {
			return nil, bad("%s is not another task of the list", other)
		}
	}
	if src.mode == modeRead || dst.mode == modeRead {
		return nil, sol.Errorf("forbidden", "a list involved is shared with you read-only")
	}
	if c.DryRun {
		return dry(map[string]any{"move": t.Title, "to": dst.info.Name}), nil
	}
	if err := a.moveTask(ctx, src, t, dst, in.Before, in.After); err != nil {
		return nil, err
	}
	_, nt, err := a.findTask(t.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"task": a.decorate(nt)}, nil
}

// ---------- sharing ----------

// resolvePerson finds a contact by id, petname or name.
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

func (a *App) actShare(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		List, Person, Mode string
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if in.Mode == "" {
		in.Mode = modeRead
	}
	if in.Mode != modeRead && in.Mode != modeWrite && in.Mode != "none" {
		return nil, bad("mode must be read, write or none")
	}
	ls, err := a.findList(in.List)
	if err != nil {
		return nil, err
	}
	if ls.owner != a.me {
		return nil, sol.Errorf("forbidden", "only the owner of a list can share it")
	}
	p, err := a.resolvePerson(ctx, in.Person)
	if err != nil {
		return nil, err
	}
	if c.DryRun {
		return dry(map[string]any{"share": ls.info.Name, "with": p.Name, "mode": in.Mode}), nil
	}
	b := a.newBatch()
	if in.Mode == "none" {
		b.ops = append(b.ops, a.deleteOp(b, "g."+p.ID))
	} else {
		// The grant goes into the feed first, so the person finds it when the feed becomes readable.
		b.set("g."+p.ID, in.Mode)
	}
	if err := a.commit(ctx, ls, b); err != nil {
		return nil, err
	}
	req := sol.ShareRequest{Feed: ls.feed, People: []string{p.ID}, Mode: in.Mode}
	if in.Mode == "none" {
		req.Mode = sol.ModeRead
		err = a.api.Unshare(ctx, req)
	} else {
		err = a.api.Share(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	a.loadNames(ctx)
	ls, _ = a.findList(ls.id)
	return map[string]any{"list": ls.info}, nil
}

// ---------- import ----------

func (a *App) actImport(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		Markdown, List, Name string
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if len(in.Markdown) > 2<<20 {
		return nil, bad("checklist too large")
	}
	head, items := parseMarkdown(in.Markdown)
	if len(items) == 0 {
		return nil, bad("no checklist items (- [ ] ...) found")
	}
	count := 0
	for _, it := range items {
		count += 1 + len(it.Subs)
		if err := it.check(); err != nil {
			return nil, bad("%q: %v", it.Title, err)
		}
		for _, s := range it.Subs {
			if err := s.check(); err != nil {
				return nil, bad("%q: %v", s.Title, err)
			}
		}
	}
	var ls *listState
	name := oneLine(in.Name)
	if name == "" {
		name = oneLine(head)
	}
	if in.List != "" {
		var err error
		if ls, err = a.findList(in.List); err != nil {
			return nil, err
		}
		if ls.mode == modeRead {
			return nil, sol.Errorf("forbidden", "this list is shared with you read-only")
		}
	}
	if name == "" {
		name = "Imported"
	}
	if c.DryRun {
		into := name + " (new list)"
		if ls != nil {
			into = ls.name()
		}
		return dry(map[string]any{"import": count, "into": into}), nil
	}
	if ls == nil {
		id, err := a.createList(ctx, name, "blue", "list")
		if err != nil {
			return nil, err
		}
		if ls, err = a.findList(id); err != nil {
			return nil, err
		}
	}
	b := a.newBatch()
	order := ls.endOrder("")
	for _, it := range items {
		id := randID()
		b.addTask(ls, id, it.taskIn, order)
		if it.Done {
			b.task(id, "done", float64(a.clock.Now().UnixMilli()))
		}
		order += 1024
		for i, s := range it.Subs {
			sid := randID()
			s.Parent = id
			b.addTask(ls, sid, s.taskIn, float64(i+1)*1024)
			if s.Done {
				b.task(sid, "done", float64(a.clock.Now().UnixMilli()))
			}
		}
	}
	if err := a.commit(ctx, ls, b); err != nil {
		return nil, err
	}
	return map[string]any{"list": ls.id, "name": ls.name(), "added": count}, nil
}

func (ls *listState) name() string { return ls.info.Name }
