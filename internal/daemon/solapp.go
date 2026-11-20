package daemon

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/apv022/sol/sdk/go/sol"
)

// SolAppID is the id of the built-in "sol" pseudo-app: the actions of Sol itself (appearance
// today) that agents reach through the same `sol do` and /agent/v1/actions as any app's. It has
// no process and no manifest file; the daemon lists and runs its actions directly, and the
// agent's scopes ("sol.*", "sol.theme.list") apply to them like to any other action.
const SolAppID = "sol"

func enumOf(vals []string) []json.RawMessage {
	var out []json.RawMessage
	for _, v := range vals {
		b, _ := json.Marshal(v)
		out = append(out, b)
	}
	return out
}

func solActions() ActionApp {
	props := map[string]*sol.Schema{}
	for k, vals := range ThemeSettingValues {
		s := &sol.Schema{Type: "string"}
		if vals != nil {
			s.Enum = enumOf(append([]string{}, vals...))
		}
		props[k] = s
	}
	return ActionApp{ID: SolAppID, Name: "Sol", Description: "Sol itself: appearance", Actions: []sol.ActionSpec{
		{Name: "sol.theme.list", Description: "List the themes, the active one and the appearance settings", Effect: sol.EffectRead,
			Input: &sol.Schema{Type: "object"}, Columns: []string{"name", "title", "author", "builtin"}},
		{Name: "sol.theme.set", Description: "Make a theme the active one on every app and device", Effect: sol.EffectWrite,
			Input: &sol.Schema{Type: "object", Required: []string{"name"}, Properties: map[string]*sol.Schema{"name": {Type: "string"}}}},
		{Name: "sol.theme.settings", Description: "Show or change appearance settings (an empty value resets one; no arguments shows them)", Effect: sol.EffectWrite,
			Input: &sol.Schema{Type: "object", Properties: props}},
	}}
}

// callSol runs a built-in action. handled is false when the action is not Sol's own.
func (d *Daemon) callSol(_ context.Context, c sol.ActionCall) (out json.RawMessage, handled bool, err error) {
	if !strings.HasPrefix(c.Action, SolAppID+".") {
		return nil, false, nil
	}
	if c.Person != "" && c.Person != d.PersonID() {
		return nil, true, sol.Errorf("bad_request", "sol actions run on this device only")
	}
	var spec *sol.ActionSpec
	for _, s := range solActions().Actions {
		if s.Name == c.Action {
			spec = &s
		}
	}
	if spec == nil {
		return nil, true, sol.Errorf("not_found", "no action "+c.Action)
	}
	if err := spec.Input.Validate(c.Args); err != nil {
		return nil, true, err
	}
	var args map[string]string
	if len(c.Args) > 0 {
		if err := json.Unmarshal(c.Args, &args); err != nil {
			return nil, true, sol.Errorf("bad_request", "args are not a JSON object of strings")
		}
	}
	var res any
	switch c.Action {
	case "sol.theme.list":
		res = d.Themes()
	case "sol.theme.set":
		if c.DryRun {
			res = map[string]any{"dry_run": true, "would_set": args["name"]}
		} else if err := d.SetTheme(args["name"]); err != nil {
			return nil, true, err
		} else {
			res = map[string]string{"active": args["name"]}
		}
	case "sol.theme.settings":
		switch {
		case len(args) == 0:
			res = d.themeSettings()
		case c.DryRun:
			res = map[string]any{"dry_run": true, "would_set": args}
		default:
			cur, err := d.UpdateThemeSettings(args)
			if err != nil {
				return nil, true, err
			}
			res = cur
		}
	}
	b, err := json.Marshal(res)
	return b, true, err
}
