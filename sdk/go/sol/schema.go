package sol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// Schema is the small JSON Schema subset used for action inputs: type, required, properties,
// items and enum. Unknown properties are allowed. An empty Schema accepts anything.
type Schema struct {
	Type       string             `json:"type,omitempty"`
	Required   []string           `json:"required,omitempty"`
	Properties map[string]*Schema `json:"properties,omitempty"`
	Items      *Schema            `json:"items,omitempty"`
	Enum       []json.RawMessage  `json:"enum,omitempty"`
	// Secret marks a property whose value is a secret (a password, a key, a seed). The daemon
	// keeps a call's secret values in memory only and shows them as Redacted (API.md section 9).
	// Valid on properties and array items, at any depth, but not on the input schema itself.
	Secret bool `json:"secret,omitempty"`
}

// Redacted replaces a secret value wherever a call is shown or stored (API.md section 8).
const Redacted = "••••"

// HasSecret reports whether any property under s is marked secret.
func (s *Schema) HasSecret() bool {
	if s == nil {
		return false
	}
	if s.Secret || s.Items.HasSecret() {
		return true
	}
	for _, p := range s.Properties {
		if p.HasSecret() {
			return true
		}
	}
	return false
}

// Redact returns raw with every value of a secret property replaced by Redacted, and whether
// it replaced anything. Args that are not valid JSON are replaced whole when the schema has a
// secret, since nothing in them can be told apart.
func (s *Schema) Redact(raw json.RawMessage) (json.RawMessage, bool) {
	if !s.HasSecret() || len(bytes.TrimSpace(raw)) == 0 {
		return raw, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return mustJSON(Redacted), true
	}
	v, changed := s.redact(v)
	if !changed {
		return raw, false
	}
	return mustJSON(v), true
}

func (s *Schema) redact(v any) (any, bool) {
	if s == nil {
		return v, false
	}
	if s.Secret {
		return Redacted, true
	}
	changed := false
	switch x := v.(type) {
	case map[string]any:
		for k, p := range s.Properties {
			if pv, ok := x[k]; ok {
				nv, c := p.redact(pv)
				x[k], changed = nv, changed || c
			}
		}
	case []any:
		if s.Items != nil {
			for i := range x {
				nv, c := s.Items.redact(x[i])
				x[i], changed = nv, changed || c
			}
		}
	}
	return v, changed
}

// Validate checks raw JSON against the schema. A nil Schema accepts everything; empty raw is
// treated as an empty object when the schema wants an object.
func (s *Schema) Validate(raw json.RawMessage) error {
	if s == nil {
		return nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return Errorf("bad_request", "args are not valid JSON: "+err.Error())
	}
	return s.check("$", v)
}

func (s *Schema) check(path string, v any) error {
	bad := func(f string, a ...any) error {
		return Errorf("bad_request", path+": "+fmt.Sprintf(f, a...))
	}
	if s == nil {
		return nil
	}
	if s.Type != "" {
		ok := false
		switch s.Type {
		case "object":
			_, ok = v.(map[string]any)
		case "array":
			_, ok = v.([]any)
		case "string":
			_, ok = v.(string)
		case "boolean":
			_, ok = v.(bool)
		case "null":
			ok = v == nil
		case "number":
			_, ok = v.(json.Number)
		case "integer":
			if n, isNum := v.(json.Number); isNum {
				f, err := n.Float64()
				ok = err == nil && f == math.Trunc(f)
			}
		default:
			return bad("unsupported schema type %q", s.Type)
		}
		if !ok {
			return bad("want %s", s.Type)
		}
	}
	if len(s.Enum) > 0 {
		got, _ := canonical(mustJSON(v))
		found := false
		for _, e := range s.Enum {
			if c, err := canonical(e); err == nil && bytes.Equal(c, got) {
				found = true
				break
			}
		}
		if !found {
			return bad("not one of the allowed values")
		}
	}
	if obj, ok := v.(map[string]any); ok {
		for _, r := range s.Required {
			if _, has := obj[r]; !has {
				return bad("missing required %q", r)
			}
		}
		keys := make([]string, 0, len(s.Properties))
		for k := range s.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if pv, has := obj[k]; has {
				if err := s.Properties[k].check(path+"."+k, pv); err != nil {
					return err
				}
			}
		}
	}
	if arr, ok := v.([]any); ok && s.Items != nil {
		for i, it := range arr {
			if err := s.Items.check(fmt.Sprintf("%s[%d]", path, i), it); err != nil {
				return err
			}
		}
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
