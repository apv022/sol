package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/apv022/sol/sdk/go/sol"
)

// Conn is the daemon connection `sol do` uses: the admin API, or the agent API of one agent key
// (internal/daemon.Client implements it).
type Conn interface {
	// Do calls method on path with a JSON body (nil for none) and decodes the reply into out.
	Do(ctx context.Context, method, path string, in, out any) error
	// Prefix is "/admin/v1" or "/agent/v1".
	Prefix() string
}

// Do runs `sol do` with args (what follows "do") and returns the exit code. Output goes to
// stdout, messages to stderr.
func Do(ctx context.Context, c Conn, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	d, err := ParseDo(args)
	if err != nil {
		fmt.Fprintln(stderr, "sol do:", err)
		return ExitUsage
	}
	if d.Help && d.Action == "" && strings.Contains(d.HelpApp, ".") {
		d.Action, d.HelpApp = d.HelpApp, ""
	}
	q := ""
	app := d.HelpApp
	if d.Action != "" {
		app, _, _ = strings.Cut(d.Action, ".")
	}
	if app != "" {
		q = "?" + url.Values{"app": {app}}.Encode()
	}
	var apps []App
	if err := c.Do(ctx, "GET", c.Prefix()+"/actions"+q, nil, &apps); err != nil {
		fmt.Fprintln(stderr, "sol do:", err)
		return ExitError
	}
	if d.Help && d.Action == "" {
		if d.HelpApp == "" {
			err = HelpApps(stdout, apps)
		} else if len(apps) == 0 {
			fmt.Fprintf(stderr, "sol do: no app %q with actions (sol do --help lists them)\n", d.HelpApp)
			return ExitUsage
		} else {
			err = HelpApp(stdout, apps[0])
		}
		if err != nil {
			return ExitError
		}
		return ExitOK
	}
	spec, ok := FindAction(apps, d.Action)
	if !ok {
		fmt.Fprintf(stderr, "sol do: no action %q here (sol do --help %s)\n", d.Action, app)
		return ExitUsage
	}
	if d.Help {
		if HelpAction(stdout, spec) != nil {
			return ExitError
		}
		return ExitOK
	}
	argsJSON, err := BuildArgs(spec, d, stdin)
	if err != nil {
		fmt.Fprintln(stderr, "sol do:", err)
		return ExitUsage
	}
	var out json.RawMessage
	call := sol.ActionCall{Action: d.Action, Args: argsJSON, Person: d.Person, DryRun: d.DryRun}
	if err := c.Do(ctx, "POST", c.Prefix()+"/actions/call", call, &out); err != nil {
		fmt.Fprintln(stderr, "sol do:", err)
		var se *sol.Error
		if errors.As(err, &se) && se.Code == "bad_request" {
			return ExitUsage
		}
		return ExitError
	}
	var ap map[string]json.RawMessage
	if json.Unmarshal(out, &ap) == nil && len(ap) == 1 && ap["approval"] != nil {
		var id string
		_ = json.Unmarshal(ap["approval"], &id)
		if d.JSON {
			fmt.Fprintf(stdout, "{\"approval\":%q}\n", id)
		} else {
			fmt.Fprintln(stdout, id)
			fmt.Fprintf(stderr, "%s needs approval: sol inbox approve %s\n", d.Action, id)
		}
		return ExitApproval
	}
	if d.JSON {
		fmt.Fprintln(stdout, strings.TrimSpace(string(out)))
		return ExitOK
	}
	if Render(stdout, out) != nil {
		return ExitError
	}
	return ExitOK
}
