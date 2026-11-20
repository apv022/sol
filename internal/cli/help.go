package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/apv022/sol/sdk/go/sol"
)

// App is one app's actions as the daemon lists them (GET /admin/v1/actions or
// /agent/v1/actions; the same shape as daemon.ActionApp).
type App struct {
	ID          string           `json:"id"`
	Name        string           `json:"name,omitempty"`
	Description string           `json:"description,omitempty"`
	Actions     []sol.ActionSpec `json:"actions"`
}

// FindAction returns the spec of an action among apps.
func FindAction(apps []App, name string) (sol.ActionSpec, bool) {
	for _, a := range apps {
		for _, s := range a.Actions {
			if s.Name == name {
				return s, true
			}
		}
	}
	return sol.ActionSpec{}, false
}

// HelpApps writes `sol do --help`: the usage and every app with its action count.
func HelpApps(w io.Writer, apps []App) error {
	fmt.Fprintln(w, "usage: sol do <app>.<action> [--name value ...] [--args JSON|-] [--json] [--dry-run] [--person ID]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Runs an action of an installed app, as you (or, with SOL_AGENT_KEY or --as-agent, as an agent).")
	fmt.Fprintln(w, "Exit codes: 0 done, 1 failed, 2 bad arguments, 3 waiting for your approval (sol inbox).")
	fmt.Fprintln(w)
	if len(apps) == 0 {
		fmt.Fprintln(w, "No apps with actions are installed.")
		return nil
	}
	fmt.Fprintln(w, "Apps:")
	tw := newTab(w, 2)
	for _, a := range apps {
		fmt.Fprintf(tw, "  %s\t%d actions\t%s\n", a.ID, len(a.Actions), oneLine(a.Description))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "More: sol do --help <app>, sol do <app>.<action> --help")
	return nil
}

// HelpApp writes `sol do --help <app>`: the app and each action in one line.
func HelpApp(w io.Writer, a App) error {
	title := a.ID
	if a.Name != "" {
		title = a.Name + " (" + a.ID + ")"
	}
	fmt.Fprintln(w, title)
	if a.Description != "" {
		fmt.Fprintln(w, "  "+oneLine(a.Description))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions:")
	tw := newTab(w, 2)
	specs := append([]sol.ActionSpec(nil), a.Actions...)
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	for _, s := range specs {
		mark := s.Effect
		if s.Approval == sol.ApprovalAgents || s.Approval == sol.ApprovalAlways {
			mark += ", approval: " + s.Approval
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", s.Name, mark, oneLine(s.Description))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "More: sol do %s.<action> --help\n", a.ID)
	return nil
}

// HelpAction writes `sol do <app>.<action> --help`, generated from the action's schema.
func HelpAction(w io.Writer, s sol.ActionSpec) error {
	keys := sortedKeys(s.Input)
	req := map[string]bool{}
	if s.Input != nil {
		for _, r := range s.Input.Required {
			req[r] = true
		}
	}
	use := "usage: sol do " + s.Name
	for _, k := range keys {
		ps := s.Input.Properties[k]
		arg := "--" + k + " " + placeholder(ps)
		if ps != nil && ps.Type == "boolean" {
			arg = "--" + k
		}
		if !req[k] {
			arg = "[" + arg + "]"
		}
		use += " " + arg
	}
	fmt.Fprintln(w, use+" [--json] [--dry-run] [--person ID]")
	if s.Description != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, oneLine(s.Description))
	}
	fmt.Fprintln(w)
	if len(keys) > 0 {
		fmt.Fprintln(w, "Arguments:")
		tw := newTab(w, 2)
		for _, k := range keys {
			ps := s.Input.Properties[k]
			notes := []string{}
			if req[k] {
				notes = append(notes, "required")
			}
			if e := enumOf(ps); e != "" {
				notes = append(notes, "one of: "+e)
			}
			if ps != nil && ps.Type == "array" {
				notes = append(notes, "repeat the flag or give a comma list")
			}
			if ps != nil && ps.Type == "object" {
				notes = append(notes, "as JSON")
			}
			fmt.Fprintf(tw, "  --%s\t%s\t%s\n", k, typeName(ps), strings.Join(notes, "; "))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(w)
	} else {
		fmt.Fprintln(w, "Arguments: none (or any JSON object with --args)")
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Effect:   %s\n", effectText(s.Effect))
	fmt.Fprintf(w, "Approval: %s\n", approvalText(s.Approval))
	return nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func typeName(s *sol.Schema) string {
	switch {
	case s == nil || s.Type == "":
		return "any"
	case s.Type == "array":
		return "list of " + typeName(s.Items)
	}
	return s.Type
}

func placeholder(s *sol.Schema) string {
	if e := enumOf(s); e != "" {
		return strings.ReplaceAll(e, ", ", "|")
	}
	if s != nil && s.Type == "array" {
		return "<" + typeName(s.Items) + ">..."
	}
	return "<" + typeName(s) + ">"
}

func enumOf(s *sol.Schema) string {
	if s == nil || len(s.Enum) == 0 {
		return ""
	}
	var vs []string
	for _, e := range s.Enum {
		var x any
		if json.Unmarshal(e, &x) == nil {
			if str, ok := x.(string); ok {
				vs = append(vs, str)
				continue
			}
		}
		vs = append(vs, string(e))
	}
	return strings.Join(vs, ", ")
}

func effectText(e string) string {
	switch e {
	case sol.EffectRead:
		return "read (changes nothing)"
	case sol.EffectWrite:
		return "write (changes data; --dry-run shows what it would do)"
	case sol.EffectDestructive:
		return "destructive (removes data; --dry-run shows what it would do)"
	}
	return e
}

func approvalText(a string) string {
	switch a {
	case sol.ApprovalAgents:
		return "agents (an agent's call waits for your approval in sol inbox)"
	case sol.ApprovalAlways:
		return "always (every call waits for your approval in sol inbox)"
	}
	return "never"
}
