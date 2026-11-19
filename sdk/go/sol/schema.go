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
