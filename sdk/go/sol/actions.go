package sol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Call is one incoming action invocation.
type Call struct {
	Caller Caller
	// Args is the raw JSON argument object (already validated against the manifest schema).
	Args json.RawMessage
	// DryRun is true when the handler must describe what it would do and change nothing.
	DryRun bool
}

// Decode unmarshals the args into v.
func (c Call) Decode(v any) error {
	if len(c.Args) == 0 {
		return nil
	}
	return json.Unmarshal(c.Args, v)
}

// ActionFunc handles one action and returns its JSON-encodable output. Return an *Error to
// pick the HTTP status.
type ActionFunc func(ctx context.Context, c Call) (any, error)

// Actions routes POST /_sol/actions/{name} to handlers registered by action name. Mount it on
// the app's mux at "/_sol/actions/". It validates args against the manifest's schemas.
type Actions struct {
	manifest *Manifest
	handlers map[string]ActionFunc
}

// NewActions creates a router. m may be nil, in which case no validation happens and any
// registered name is callable.
func NewActions(m *Manifest) *Actions {
	return &Actions{manifest: m, handlers: map[string]ActionFunc{}}
}

// Handle registers the handler for an action name such as "files.upload". It panics on a
// duplicate, or on a name the manifest does not declare.
func (a *Actions) Handle(name string, f ActionFunc) {
	if _, dup := a.handlers[name]; dup {
		panic("sol: duplicate action handler " + name)
	}
	if a.manifest != nil {
		if _, ok := a.manifest.Action(name); !ok {
			panic("sol: action " + name + " is not declared in app.json")
		}
	}
	a.handlers[name] = f
}

// Missing lists manifest actions that have no handler (empty when there is no manifest).
func (a *Actions) Missing() []string {
	var out []string
	if a.manifest != nil {
		for _, s := range a.manifest.Actions {
			if a.handlers[s.Name] == nil {
				out = append(out, s.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ServeHTTP implements http.Handler.
func (a *Actions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, "/_sol/actions/")
	if !ok || name == "" {
		WriteError(w, Errorf("not_found", "no such path"))
		return
	}
	if r.Method != http.MethodPost {
		WriteError(w, Errorf("bad_request", "actions are called with POST"))
		return
	}
	f := a.handlers[name]
	if f == nil {
		WriteError(w, Errorf("not_found", "no such action "+name))
		return
	}
	args, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		WriteError(w, Errorf("bad_request", err.Error()))
		return
	}
	if a.manifest != nil {
		spec, _ := a.manifest.Action(name)
		if err := spec.Input.Validate(args); err != nil {
			WriteError(w, err)
			return
		}
	}
	out, err := f(r.Context(), Call{Caller: callerOf(r), Args: args, DryRun: r.Header.Get("Sol-Dry-Run") == "1"})
	if err != nil {
		WriteError(w, err)
		return
	}
	if out == nil {
		out = struct{}{}
	}
	WriteJSON(w, http.StatusOK, out)
}

// String describes the router for debugging.
func (a *Actions) String() string { return fmt.Sprintf("sol.Actions(%d handlers)", len(a.handlers)) }
