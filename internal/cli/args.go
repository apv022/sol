// Package cli is the logic behind `sol do` (design/API.md §6 "The same actions everywhere"):
// turning command-line flags into an action's JSON args using the action's JSON Schema,
// generating help from the installed apps' manifests, and rendering an action's JSON output
// for people (tables and key: value lines) when --json is not given. It does no I/O of its
// own beyond what it is handed, so cmd/sol stays a thin dispatcher and all of this is tested
// with golden files.
//
// Invariants: coercion follows the schema only (never guesses from the value when the schema
// names a type); an unknown flag for an action whose schema lists its properties is an error,
// not silently passed; errors here are usage errors (exit code 2).
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/apv022/sol/sdk/go/sol"
)

// Exit codes of `sol do`.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitApproval = 3
)

// UsageError is a mistake in the command line (exit code 2).
type UsageError struct{ Msg string }

// Error implements error.
func (e *UsageError) Error() string { return e.Msg }

func usagef(f string, a ...any) error { return &UsageError{Msg: fmt.Sprintf(f, a...)} }

// DoArgs is a parsed `sol do` command line.
type DoArgs struct {
	Action string
	// Flags are the action's own --key value pairs, in order; a key given without a value
	// (followed by another flag or nothing) has Bare set.
	Flags []Flag
	// ArgsJSON is --args (raw JSON, or "-" for stdin).
	ArgsJSON string
	JSON     bool
	DryRun   bool
	Person   string
	Help     bool
	// HelpApp is the app named after `--help` with no action (`sol do --help files`).
	HelpApp string
}

// Flag is one --key value of an action.
type Flag struct {
	Key, Value string
	Bare       bool
}

// ParseDo splits the arguments after `sol do` into the action, the CLI's own flags and the
// action's flags. Global flags (--dir, --as-agent) must already be removed.
func ParseDo(args []string) (DoArgs, error) {
	var d DoArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			if d.Action == "" && !d.Help {
				d.Action = a
				continue
			}
			if d.Help && d.Action == "" && d.HelpApp == "" {
				d.HelpApp = a
				continue
			}
			return d, usagef("unexpected argument %q (action arguments are --name value)", a)
		}
		key, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		next := func() (string, bool) {
			if hasVal {
				return val, true
			}
			if i+1 < len(args) && (!strings.HasPrefix(args[i+1], "--") || isNumber(args[i+1])) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch key {
		case "h", "help":
			d.Help = true
		case "json":
			d.JSON = !hasVal || val != "false"
		case "dry-run", "dry_run":
			d.DryRun = !hasVal || val != "false"
		case "args":
			v, ok := next()
			if !ok {
				return d, usagef("--args needs JSON (or - to read it from stdin)")
			}
			d.ArgsJSON = v
		case "person":
			v, ok := next()
			if !ok {
				return d, usagef("--person needs a person ID")
			}
			d.Person = v
		case "":
			return d, usagef("bad flag %q", a)
		default:
			v, ok := next()
			d.Flags = append(d.Flags, Flag{Key: key, Value: v, Bare: !ok})
		}
	}
	if d.Action == "" && !d.Help {
		return d, usagef("usage: sol do <app>.<action> [--name value ...] [--args JSON|-] [--json] [--dry-run] [--person ID]")
	}
	return d, nil
}

func isNumber(s string) bool { _, err := strconv.ParseFloat(s, 64); return err == nil }

// BuildArgs turns --args and the flags into the action's JSON args, coercing each flag by the
// property's schema type: string, number, integer, boolean (a bare flag is true), array
// (repeat the flag or give a comma list; items are coerced by the item schema) and object (a
// JSON value). Flags override keys of --args. stdin is read for --args -.
func BuildArgs(spec sol.ActionSpec, d DoArgs, stdin io.Reader) (json.RawMessage, error) {
	obj := map[string]any{}
	if d.ArgsJSON != "" {
		raw := []byte(d.ArgsJSON)
		if d.ArgsJSON == "-" {
			var err error
			if raw, err = io.ReadAll(io.LimitReader(stdin, 8<<20)); err != nil {
				return nil, err
			}
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&obj); err != nil {
			return nil, usagef("--args is not a JSON object: %v", err)
		}
	}
	in := spec.Input
	var props map[string]*sol.Schema
	if in != nil {
		props = in.Properties
	}
	arrays := map[string]bool{}
	for _, f := range d.Flags {
		key := propName(props, f.Key)
		var ps *sol.Schema
		if props != nil {
			ps = props[key]
			if ps == nil {
				return nil, usagef("%s has no argument --%s (see sol do %s --help)", spec.Name, f.Key, spec.Name)
			}
		}
		if ps != nil && ps.Type == "array" {
			if f.Bare {
				return nil, usagef("--%s needs a value", f.Key)
			}
			if !arrays[key] {
				obj[key] = []any{}
				arrays[key] = true
			}
			for _, part := range splitList(f.Value) {
				v, err := coerce(f.Key, ps.Items, part, false)
				if err != nil {
					return nil, err
				}
				obj[key] = append(obj[key].([]any), v)
			}
			continue
		}
		v, err := coerce(f.Key, ps, f.Value, f.Bare)
		if err != nil {
			return nil, err
		}
		obj[key] = v
	}
	if in != nil {
		var missing []string
		for _, r := range in.Required {
			if _, ok := obj[r]; !ok {
				missing = append(missing, "--"+r)
			}
		}
		if len(missing) > 0 {
			return nil, usagef("%s needs %s (see sol do %s --help)", spec.Name, strings.Join(missing, ", "), spec.Name)
		}
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	if err := in.Validate(b); err != nil {
		var se *sol.Error
		if errors.As(err, &se) {
			return nil, usagef("%s", se.Message)
		}
		return nil, usagef("%v", err)
	}
	return b, nil
}

// propName maps a flag to a property: exact, or with dashes as underscores.
func propName(props map[string]*sol.Schema, key string) string {
	if _, ok := props[key]; ok || props == nil {
		return key
	}
	if alt := strings.ReplaceAll(key, "-", "_"); props[alt] != nil {
		return alt
	}
	return key
}

// splitList splits a comma list; "\," keeps a comma.
func splitList(s string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == ',':
			cur.WriteByte(',')
			i++
		case s[i] == ',':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	return append(out, cur.String())
}

// coerce turns one flag value into the JSON value its schema wants.
func coerce(flag string, s *sol.Schema, v string, bare bool) (any, error) {
	t := ""
	if s != nil {
		t = s.Type
	}
	if bare && t != "boolean" && t != "" {
		return nil, usagef("--%s needs a value", flag)
	}
	switch t {
	case "string":
		return v, nil
	case "boolean":
		if bare {
			return true, nil
		}
		switch strings.ToLower(v) {
		case "true", "yes", "on", "1":
			return true, nil
		case "false", "no", "off", "0":
			return false, nil
		}
		return nil, usagef("--%s wants true or false, not %q", flag, v)
	case "number":
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, usagef("--%s wants a number, not %q", flag, v)
		}
		return json.Number(strconv.FormatFloat(f, 'f', -1, 64)), nil
	case "integer":
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, usagef("--%s wants a whole number, not %q", flag, v)
		}
		return n, nil
	case "object", "array", "null":
		var x any
		dec := json.NewDecoder(strings.NewReader(v))
		dec.UseNumber()
		if err := dec.Decode(&x); err != nil {
			return nil, usagef("--%s wants JSON (%s): %v", flag, t, err)
		}
		return x, nil
	}
	// No type: a bare flag is true, JSON if it parses, else the string.
	if bare {
		return true, nil
	}
	var x any
	dec := json.NewDecoder(strings.NewReader(v))
	dec.UseNumber()
	if dec.Decode(&x) == nil && !dec.More() {
		return x, nil
	}
	return v, nil
}

// sortedKeys returns a schema's property names, required ones first.
func sortedKeys(s *sol.Schema) []string {
	if s == nil {
		return nil
	}
	req := map[string]bool{}
	for _, r := range s.Required {
		req[r] = true
	}
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if req[keys[i]] != req[keys[j]] {
			return req[keys[i]]
		}
		return keys[i] < keys[j]
	})
	return keys
}
