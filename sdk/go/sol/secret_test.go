package sol_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/apv022/sol/sdk/go/sol"
)

// TestManifestSecretFields: "secret" on input properties (nested too) and "output_secret" on an
// action parse; "secret" on the input schema itself or of the wrong type is refused.
func TestManifestSecretFields(t *testing.T) {
	m, err := sol.ParseManifest([]byte(`{"id":"vault","version":"1","exec":"x","scopes":[],"actions":[
	 {"name":"vault.unlock","effect":"write","approval":"agents",
	  "input":{"type":"object","properties":{"password":{"type":"string","secret":true}}}},
	 {"name":"vault.put","effect":"write",
	  "input":{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"seed":{"type":"string","secret":true}}}}}}},
	 {"name":"vault.get","effect":"read","output_secret":true},
	 {"name":"vault.list","effect":"read","input":{"type":"object","properties":{"q":{"type":"string"}}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"vault.unlock": true, "vault.put": true, "vault.get": false, "vault.list": false} {
		if a, _ := m.Action(name); a.SecretArgs() != want {
			t.Errorf("%s: SecretArgs %v", name, !want)
		}
	}
	if a, _ := m.Action("vault.get"); !a.OutputSecret {
		t.Error("output_secret lost")
	}
	for _, bad := range []string{
		`{"id":"v","version":"1","exec":"x","scopes":[],"actions":[{"name":"v.a","effect":"write","input":{"type":"object","secret":true}}]}`,
		`{"id":"v","version":"1","exec":"x","scopes":[],"actions":[{"name":"v.a","effect":"write","input":{"properties":{"p":{"secret":"yes"}}}}]}`,
		`{"id":"v","version":"1","exec":"x","scopes":[],"actions":[{"name":"v.a","effect":"read","output_secret":1}]}`,
	} {
		if _, err := sol.ParseManifest([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

// TestSchemaRedact: secret values are replaced at any depth, the rest is kept, and args with
// nothing secret (or a schema without secrets) come back unchanged.
func TestSchemaRedact(t *testing.T) {
	s := &sol.Schema{Type: "object", Properties: map[string]*sol.Schema{
		"user": {Type: "string"},
		"pw":   {Type: "string", Secret: true},
		"list": {Type: "array", Items: &sol.Schema{Type: "object", Properties: map[string]*sol.Schema{"key": {Secret: true}}}},
	}}
	out, ok := s.Redact(json.RawMessage(`{"user":"al","pw":"hunter2","list":[{"key":"k1","n":1},{"n":2}],"extra":3}`))
	if !ok {
		t.Fatal("nothing redacted")
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	if v["user"] != "al" || v["pw"] != sol.Redacted || v["extra"] != 3.0 || strings.Contains(string(out), "hunter2") || strings.Contains(string(out), "k1") {
		t.Fatalf("redacted: %s", out)
	}
	if raw := json.RawMessage(`{"user":"al"}`); string(must(s.Redact(raw))) != string(raw) {
		t.Fatal("args without secrets changed")
	}
	if out, ok := s.Redact(json.RawMessage(`{"pw":`)); !ok || string(out) != `"`+sol.Redacted+`"` {
		t.Fatalf("bad JSON: %s %v", out, ok)
	}
	plain := &sol.Schema{Type: "object", Properties: map[string]*sol.Schema{"pw": {Type: "string"}}}
	if _, ok := plain.Redact(json.RawMessage(`{"pw":"x"}`)); ok {
		t.Fatal("a schema without secrets redacted")
	}
	var nilSchema *sol.Schema
	if _, ok := nilSchema.Redact(json.RawMessage(`{"pw":"x"}`)); ok || nilSchema.HasSecret() {
		t.Fatal("nil schema")
	}
}

func must(b json.RawMessage, _ bool) json.RawMessage { return b }
