package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/apv022/sol/internal/identity"
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
	name := func(names []string) *sol.Schema {
		return &sol.Schema{Type: "object", Required: []string{"name"}, Properties: map[string]*sol.Schema{
			"name": {Type: "string", Enum: enumOf(names)}}}
	}
	return ActionApp{ID: SolAppID, Name: "Sol", Description: "Sol itself: my profile, appearance, transports, people's privacy and opening apps", Actions: []sol.ActionSpec{
		{Name: "sol.profile.show", Description: "Show my profile: the name and picture my devices and contacts see", Effect: sol.EffectRead,
			Input: &sol.Schema{Type: "object"}},
		{Name: "sol.profile.set", Description: "Change my name, on all my devices and for my contacts (an empty name shows the name I started with)",
			Effect: sol.EffectWrite, Approval: sol.ApprovalAgents, Input: &sol.Schema{Type: "object", Required: []string{"name"}, Properties: map[string]*sol.Schema{
				"name": {Type: "string"}}}},
		{Name: "sol.profile.avatar", Description: "Change my picture: a square JPEG, PNG or WebP of at most 64 KiB, as base64 data or the absolute path of a file on this computer. Anonymous contacts never see it",
			Effect: sol.EffectWrite, Approval: sol.ApprovalAgents, Input: &sol.Schema{Type: "object", Properties: map[string]*sol.Schema{
				"data": {Type: "string"},
				"path": {Type: "string"}}}},
		{Name: "sol.profile.clear_avatar", Description: "Remove my picture (the letter shows again)",
			Effect: sol.EffectWrite, Approval: sol.ApprovalAgents, Input: &sol.Schema{Type: "object"}},
		{Name: "sol.transports.list", Description: "List the transports, whether each is on and how it is doing", Effect: sol.EffectRead,
			Input: &sol.Schema{Type: "object"}, Columns: []string{"name", "enabled", "state", "detail"}},
		{Name: "sol.transports.enable", Description: "Turn a transport on (wg, direct, tor), now and after restarts", Effect: sol.EffectWrite, Input: name(KnownTransports)},
		// clearnet (tests, LAN experiments) can be turned off, never on from here.
		{Name: "sol.transports.disable", Description: "Turn a transport off; the last one stays on", Effect: sol.EffectWrite,
			Input: name(append(slices.Clone(KnownTransports), "clearnet"))},
		{Name: "sol.people.set", Description: "Make a contact anonymous (Tor only both ways; they only learn your onion) or a buddy (every transport)",
			Effect: sol.EffectWrite, Input: &sol.Schema{Type: "object", Required: []string{"person", "privacy"}, Properties: map[string]*sol.Schema{
				"person":  {Type: "string"},
				"privacy": {Type: "string", Enum: enumOf([]string{"anonymous", "buddy"})}}}},
		{Name: "sol.theme.list", Description: "List the themes, the active one and the appearance settings", Effect: sol.EffectRead,
			Input: &sol.Schema{Type: "object"}, Columns: []string{"name", "title", "author", "builtin"}},
		{Name: "sol.theme.set", Description: "Make a theme the active one on every app and device", Effect: sol.EffectWrite,
			Input: &sol.Schema{Type: "object", Required: []string{"name"}, Properties: map[string]*sol.Schema{"name": {Type: "string"}}}},
		{Name: "sol.theme.settings", Description: "Show or change appearance settings (an empty value resets one; no arguments shows them)", Effect: sol.EffectWrite,
			Input: &sol.Schema{Type: "object", Properties: props}},
		{Name: "sol.apps.open", Description: "Open an app's UI in this computer's browser (an Android app connected to Sol opens from Sol's Apps grid on the phone)",
			Effect: sol.EffectWrite, Input: &sol.Schema{Type: "object", Required: []string{"id"}, Properties: map[string]*sol.Schema{"id": {Type: "string"}}}},
		{Name: "sol.onions.list", Description: "List the public onion services: local ports anyone with the .onion address can reach", Effect: sol.EffectRead,
			Input: &sol.Schema{Type: "object"}, Columns: []string{"name", "port", "onion_port", "address"}},
		{Name: "sol.onions.host", Description: "Publish a local port as a public Tor onion service (anyone with the address can connect; the service must do its own login). Tor must be on.",
			Effect: sol.EffectWrite, Approval: sol.ApprovalAgents, Input: &sol.Schema{Type: "object", Required: []string{"name", "port"}, Properties: map[string]*sol.Schema{
				"name":       {Type: "string"},
				"port":       {Type: "integer"},
				"onion_port": {Type: "integer"}}}},
		{Name: "sol.onions.remove", Description: "Stop a public onion service and delete its key: its .onion address is gone for good",
			Effect: sol.EffectDestructive, Approval: sol.ApprovalAgents, Input: &sol.Schema{Type: "object", Required: []string{"name"}, Properties: map[string]*sol.Schema{"name": {Type: "string"}}}},
		{Name: "sol.notifications", Description: "Show or set desktop notifications for Inbox items on this device (desktop: on|off; no arguments shows the state)", Effect: sol.EffectWrite,
			Input: &sol.Schema{Type: "object", Properties: map[string]*sol.Schema{"desktop": {Type: "string", Enum: enumOf([]string{"on", "off"})}}}},
		{Name: "sol.nearby", Description: "Show or set LAN discovery: private beacons that let my devices and my buddies' devices on the same network find each other without the internet (state: on|off; no arguments shows it)",
			Effect: sol.EffectWrite, Input: &sol.Schema{Type: "object", Properties: map[string]*sol.Schema{"state": {Type: "string", Enum: enumOf([]string{"on", "off"})}}}},
	}}
}

// solSpec returns the spec of a built-in action.
func solSpec(action string) (sol.ActionSpec, bool) {
	for _, s := range solActions().Actions {
		if s.Name == action {
			return s, true
		}
	}
	return sol.ActionSpec{}, false
}

// callSol runs a built-in action. handled is false when the action is not Sol's own.
func (d *Daemon) callSol(ctx context.Context, c sol.ActionCall) (out json.RawMessage, handled bool, err error) {
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
	// Every built-in action takes flat scalar arguments (the schema checked them above).
	var raw map[string]any
	if len(c.Args) > 0 {
		if err := json.Unmarshal(c.Args, &raw); err != nil {
			return nil, true, sol.Errorf("bad_request", "args are not a JSON object")
		}
	}
	args := map[string]string{}
	for k, v := range raw {
		switch v := v.(type) {
		case string:
			args[k] = v
		case float64, bool:
			args[k] = fmt.Sprint(v)
		default:
			return nil, true, sol.Errorf("bad_request", "argument "+k+" must be a string, number or boolean")
		}
	}
	var res any
	switch c.Action {
	case "sol.profile.show":
		res = d.Me()
	case "sol.profile.set":
		if c.DryRun {
			res = map[string]any{"dry_run": true, "would_set": map[string]string{"name": strings.TrimSpace(args["name"])}}
		} else if res, err = d.SetName(ctx, args["name"]); err != nil {
			return nil, true, err
		}
	case "sol.profile.avatar":
		b, err := avatarArg(args)
		if err != nil {
			return nil, true, err
		}
		if _, err := identity.AvatarType(b); err != nil {
			return nil, true, sol.Errorf("bad_request", strings.TrimPrefix(err.Error(), "identity: invalid object: "))
		}
		if c.DryRun {
			res = map[string]any{"dry_run": true, "would_set": map[string]any{"avatar": identity.AvatarHash(b), "size": len(b)}}
		} else if res, err = d.SetAvatar(ctx, b); err != nil {
			return nil, true, err
		}
	case "sol.profile.clear_avatar":
		if c.DryRun {
			res = map[string]any{"dry_run": true, "would_clear": "avatar"}
		} else if res, err = d.ClearAvatar(ctx); err != nil {
			return nil, true, err
		}
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
	case "sol.transports.list":
		res = d.Transports()
	case "sol.transports.enable", "sol.transports.disable":
		on := c.Action == "sol.transports.enable"
		if c.DryRun {
			res = map[string]any{"dry_run": true, "name": args["name"], "enabled": on}
		} else if res, err = d.SetTransport(args["name"], on); err != nil {
			return nil, true, err
		}
	case "sol.people.set":
		id := d.resolvePerson(args["person"])
		anon := args["privacy"] == "anonymous"
		switch {
		case c.DryRun:
			out := map[string]any{"dry_run": true, "person": id, "anonymous": anon}
			if anon {
				out["warning"] = AnonymousWarning
			}
			res = out
		default:
			p, err := d.SetAnonymous(id, anon)
			if err != nil {
				return nil, true, err
			}
			res = p
		}
	case "sol.apps.open":
		if res, err = d.openAppAction(args["id"], c.DryRun); err != nil {
			return nil, true, err
		}
	case "sol.onions.list":
		res = d.onions.List()
	case "sol.onions.host":
		port, _ := strconv.Atoi(args["port"])
		oport, _ := strconv.Atoi(args["onion_port"])
		ps := PublicService{Name: args["name"], Port: port, OnionPort: oport}
		if c.DryRun {
			res = map[string]any{"dry_run": true, "would_host": ps, "warning": PublicOnionWarning}
		} else if res, err = d.onions.Add(ps); err != nil {
			return nil, true, err
		}
	case "sol.onions.remove":
		if c.DryRun {
			res = map[string]any{"dry_run": true, "would_remove": args["name"], "warning": "its .onion address is gone for good"}
		} else if err := d.onions.Remove(args["name"]); err != nil {
			return nil, true, err
		} else {
			res = map[string]string{"removed": args["name"]}
		}
	case "sol.notifications":
		switch {
		case args["desktop"] == "":
			res = d.Notifications()
		case c.DryRun:
			res = map[string]any{"dry_run": true, "would_set": args["desktop"]}
		default:
			if res, err = d.SetNotifications(args["desktop"] == "on"); err != nil {
				return nil, true, err
			}
		}
	case "sol.nearby":
		switch {
		case args["state"] == "":
			res = d.Nearby()
		case c.DryRun:
			res = map[string]any{"dry_run": true, "would_set": args["state"]}
		default:
			if res, err = d.SetNearby(args["state"] == "on"); err != nil {
				return nil, true, err
			}
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

// openAppAction is sol.apps.open: an app with a UI opens in the default browser of this
// computer (what `sol open <app>` does). The daemon has no channel to the Android shell, so on
// Android, and for an external app (a native program, M5), it answers not_implemented and says
// where the app opens instead: the shell's Apps grid launches a connected Android app itself.
func (d *Daemon) openAppAction(id string, dry bool) (any, error) {
	var app *AppInfo
	for _, a := range d.Apps() {
		if strings.EqualFold(a.ID, id) || strings.EqualFold(a.Name, id) {
			app = &a
			break
		}
	}
	switch {
	case app == nil:
		return nil, sol.Errorf("not_found", "no app "+id)
	case app.External && app.Package != "":
		return nil, sol.Errorf("not_implemented", app.Name+" is an Android app: open it from Sol's Apps grid on the phone it is connected on")
	case app.External:
		return nil, sol.Errorf("not_implemented", app.Name+" runs outside Sol and has no UI Sol can open")
	case app.URL == "":
		return nil, sol.Errorf("bad_request", app.Name+" has no UI to open")
	case isAndroid():
		return nil, sol.Errorf("not_implemented", "on Android, open "+app.Name+" from Sol's Apps grid")
	case dry:
		return map[string]any{"dry_run": true, "id": app.ID, "url": app.URL}, nil
	}
	return map[string]any{"id": app.ID, "url": app.URL, "opened": openURL(app.URL)}, nil
}
