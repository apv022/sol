package tasks

import (
	"regexp"
	"strings"
)

// Markdown checklists, the format Tasks exports and imports so nothing is locked in:
//
//	# Groceries
//	- [ ] Milk (due 2026-11-21 09:00, repeats weekly, flagged)
//	  > a note line
//	  - [x] a subtask
//
// Anything the importer does not understand is ignored, never an error.

var (
	reItem   = regexp.MustCompile(`^(\s*)[-*+]\s+\[([ xX])\]\s+(.*)$`)
	reParen  = regexp.MustCompile(`\s*\(([^()]*)\)\s*$`)
	reDueTok = regexp.MustCompile(`^due\s+(\d{4}-\d{2}-\d{2})(?:\s+(\d{1,2}:\d{2}))?$`)
	reRepTok = regexp.MustCompile(`^repeats\s+(daily|weekly|monthly|yearly)$`)
)

// mdTask is one parsed checklist line.
type mdTask struct {
	taskIn
	Done bool
	Subs []mdTask
}

// exportMarkdown renders one list. Open tasks come first, then completed ones, each with its
// subtasks below it.
func exportMarkdown(ls *listState) string {
	var b strings.Builder
	b.WriteString("# " + oneLine(ls.info.Name) + "\n\n")
	line := func(indent string, t *Task) {
		b.WriteString(indent + "- [")
		if t.Done {
			b.WriteString("x")
		} else {
			b.WriteString(" ")
		}
		b.WriteString("] " + oneLine(t.Title))
		var meta []string
		if t.Due != "" {
			d := "due " + t.Due
			if t.Time != "" {
				d += " " + t.Time
			}
			meta = append(meta, d)
		}
		if t.Repeat != "" {
			meta = append(meta, "repeats "+t.Repeat)
		}
		if t.Flag {
			meta = append(meta, "flagged")
		}
		if len(meta) > 0 {
			b.WriteString(" (" + strings.Join(meta, ", ") + ")")
		}
		b.WriteString("\n")
		for _, n := range strings.Split(strings.TrimRight(t.Notes, "\n"), "\n") {
			if t.Notes != "" {
				b.WriteString(indent + "  > " + strings.TrimRight(n, "\r") + "\n")
			}
		}
	}
	for _, pass := range []bool{false, true} {
		for _, t := range ls.tasks {
			if t.Parent != "" || t.Done != pass {
				continue
			}
			line("", t)
			for _, s := range ls.subtasksOf(t.ID) {
				line("  ", s)
			}
		}
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// parseMarkdown reads a checklist: the list name from the first "# " heading ("" when none) and
// its tasks.
func parseMarkdown(md string) (name string, tasks []mdTask) {
	var cur *mdTask  // the top-level task notes and subtasks attach to
	var last *mdTask // the most recent item (a note belongs to it)
	for _, raw := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		if m := reItem.FindStringSubmatch(raw); m != nil {
			t := parseItem(m[3])
			t.Done = m[2] != " "
			if len(m[1]) >= 2 && cur != nil {
				cur.Subs = append(cur.Subs, t)
				last = &cur.Subs[len(cur.Subs)-1]
			} else {
				tasks = append(tasks, t)
				cur = &tasks[len(tasks)-1]
				last = cur
			}
			continue
		}
		trim := strings.TrimSpace(raw)
		switch {
		case name == "" && strings.HasPrefix(raw, "# "):
			name = strings.TrimSpace(raw[2:])
		case last != nil && strings.HasPrefix(trim, ">") && strings.HasPrefix(raw, " "):
			n := strings.TrimPrefix(strings.TrimPrefix(trim, ">"), " ")
			if last.Notes != "" {
				last.Notes += "\n"
			}
			last.Notes += n
		}
	}
	// cur and last point into the slices as they were when taken; re-resolve is unnecessary
	// because appends only happen at the top level before they are taken again.
	return name, tasks
}

// parseItem splits "Title (due ..., repeats ..., flagged)".
func parseItem(s string) mdTask {
	t := mdTask{}
	s = strings.TrimSpace(s)
	if m := reParen.FindStringSubmatchIndex(s); m != nil {
		var meta taskIn
		known := false
		for _, tok := range strings.Split(s[m[2]:m[3]], ",") {
			tok = strings.TrimSpace(strings.ToLower(tok))
			switch {
			case tok == "flagged":
				meta.Flag, known = true, true
			case reDueTok.MatchString(tok):
				g := reDueTok.FindStringSubmatch(tok)
				meta.Due, known = g[1], true
				if g[2] != "" {
					if len(g[2]) == 4 {
						g[2] = "0" + g[2]
					}
					meta.Time = g[2]
				}
			case reRepTok.MatchString(tok):
				meta.Repeat, known = reRepTok.FindStringSubmatch(tok)[1], true
			}
		}
		if known {
			t.taskIn = meta
			s = strings.TrimSpace(s[:m[0]])
		}
	}
	t.Title = s
	if !validDue(t.Due) || !validTime(t.Time) || t.Due == "" {
		t.Due, t.Time = "", ""
	}
	return t
}
