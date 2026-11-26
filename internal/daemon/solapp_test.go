package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/apv022/sol/sdk/go/sol"
)

// TestSolThemeActions: the built-in "sol" app lists theme actions with schemas and runs them
// like any app's, with dry run.
func TestSolThemeActions(t *testing.T) {
	d := testDaemon(t)
	c := adminClient(t, d)
	ctx := context.Background()
	var apps []ActionApp
	if err := c.Do(ctx, "GET", "/admin/v1/actions?app=sol", nil, &apps); err != nil || len(apps) != 1 || len(apps[0].Actions) != len(solActions().Actions) {
		t.Fatalf("%v %+v", err, apps)
	}
	call := func(action, args string, dry bool) (json.RawMessage, error) {
		var out json.RawMessage
		err := c.Do(ctx, "POST", "/admin/v1/actions/call", sol.ActionCall{Action: action, Args: json.RawMessage(args), DryRun: dry}, &out)
		return out, err
	}
	out, err := call("sol.theme.list", "", false)
	if err != nil || !strings.Contains(string(out), `"graphite"`) {
		t.Fatalf("%s %v", out, err)
	}
	if out, err = call("sol.theme.set", `{"name":"graphite"}`, true); err != nil || d.activeTheme() != "sol" {
		t.Fatalf("dry run changed something: %s %v %s", out, err, d.activeTheme())
	}
	if _, err = call("sol.theme.set", `{"name":"graphite"}`, false); err != nil || d.activeTheme() != "graphite" {
		t.Fatalf("%v %s", err, d.activeTheme())
	}
	if _, err = call("sol.theme.set", `{"name":"nope"}`, false); err == nil {
		t.Fatal("unknown theme accepted")
	}
	if _, err = call("sol.theme.settings", `{"density":"compact"}`, false); err != nil || d.themeSettings()["density"] != "compact" {
		t.Fatalf("%v", err)
	}
	if _, err = call("sol.theme.settings", `{"density":"huge"}`, false); err == nil {
		t.Fatal("bad value accepted")
	}
	if out, err = call("sol.theme.settings", "", false); err != nil || !strings.Contains(string(out), "compact") {
		t.Fatalf("%s %v", out, err)
	}
}
