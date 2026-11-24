package appapi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

const secret = "S3CRET-value-4c1d"

func readInbox(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "inbox.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestInboxMigrationScrubs: an inbox.json written by an older version, with args and results
// on answered approvals, is scrubbed and rewritten on open; open approvals keep their args.
func TestInboxMigrationScrubs(t *testing.T) {
	dir := t.TempDir()
	args := json.RawMessage(`{"password":"` + secret + `"}`)
	old := []item{
		{ID: "in-1", App: "vault", Kind: "approval", Answer: "approved", Action: &sol.ActionCall{Action: "vault.get", Args: args},
			Result: json.RawMessage(`{"password":"` + secret + `"}`)},
		{ID: "in-2", App: "vault", Kind: "approval", Answer: "denied", Action: &sol.ActionCall{Action: "vault.unlock", Args: args}},
		{ID: "in-3", App: "vault", Kind: "approval", Answer: "approved", Action: &sol.ActionCall{Action: "vault.unlock", Args: args},
			Error: sol.Errorf("forbidden", "wrong password")},
		{ID: "in-4", App: "vault", Kind: "approval", Action: &sol.ActionCall{Action: "vault.note", Args: json.RawMessage(`{"text":"keep"}`)}},
		{ID: "in-5", App: "vault", Kind: "notice", Title: "hi"},
	}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, "inbox.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := openInbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	if disk := readInbox(t, dir); bytes.Contains(disk, []byte(secret)) || !bytes.Contains(disk, []byte(`"keep"`)) {
		t.Fatalf("after migration: %s", disk)
	}
	want := argsDigest(args)
	for _, id := range []string{"in-1", "in-2", "in-3"} {
		it, _ := in.get(id)
		if it.Action == nil || it.Action.Args != nil || it.Result != nil || it.ArgsDigest != want {
			t.Fatalf("%s: %+v", id, it)
		}
	}
	if it, _ := in.get("in-1"); it.Outcome != "ok" {
		t.Fatalf("in-1 outcome %q", it.Outcome)
	}
	if it, _ := in.get("in-3"); it.Outcome != "error" || it.Error == nil || it.Error.Code != "forbidden" {
		t.Fatalf("in-3: %+v", it)
	}
	if it, _ := in.get("in-2"); it.Outcome != "" {
		t.Fatalf("a denied approval has an outcome: %+v", it)
	}
	// A second open changes nothing.
	before := readInbox(t, dir)
	if _, err := openInbox(dir); err != nil || !bytes.Equal(before, readInbox(t, dir)) {
		t.Fatalf("second open rewrote the inbox: %v", err)
	}
}

// TestInboxSecretArgsAndResults: secret args stay in memory and come back for the run; the
// stored item keeps only the action's name; an output is delivered once and never written;
// a restart expires an open approval whose secret args it lost.
func TestInboxSecretArgsAndResults(t *testing.T) {
	dir := t.TempDir()
	in, err := openInbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"user":"a","password":"` + secret + `"}`)
	spec := &sol.Schema{Type: "object", Properties: map[string]*sol.Schema{"password": {Type: "string", Secret: true}}}
	red, ok := spec.Redact(args)
	if !ok || bytes.Contains(red, []byte(secret)) {
		t.Fatalf("redact: %s %v", red, ok)
	}
	id, err := in.post(item{App: "vault", Kind: "approval", Agent: "ag", SecretArgs: true, ArgsDigest: argsDigest(args),
		Action: &sol.ActionCall{Action: "vault.unlock", Args: red}}, args)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(readInbox(t, dir), []byte(secret)) {
		t.Fatal("secret args on disk while open")
	}
	full, err := in.answer(id, true)
	if err != nil || !bytes.Equal(full.Action.Args, args) {
		t.Fatalf("answer: %+v %v", full, err)
	}
	if err := in.setResult(id, json.RawMessage(`{"token":"`+secret+`"}`), nil); err != nil {
		t.Fatal(err)
	}
	if disk := readInbox(t, dir); bytes.Contains(disk, []byte(secret)) || bytes.Contains(disk, []byte(`"user"`)) {
		t.Fatalf("answered item on disk: %s", disk)
	}
	all := func(item) bool { return true }
	if l := in.list(all, nil); l[0].Result != nil {
		t.Fatal("the admin listing took the result")
	}
	if l := in.list(all, func(it item) bool { return it.Agent == "" }); l[0].Result != nil {
		t.Fatal("an agent's result went to the app")
	}
	agent := func(it item) bool { return it.Agent == "ag" }
	if l := in.list(agent, agent); !strings.Contains(string(l[0].Result), secret) || l[0].Outcome != "ok" {
		t.Fatalf("first delivery: %+v", l)
	}
	if l := in.list(agent, agent); l[0].Result != nil {
		t.Fatal("delivered twice")
	}

	// Open secret approval, then a restart.
	id2, _ := in.post(item{App: "vault", Kind: "approval", SecretArgs: true, Action: &sol.ActionCall{Action: "vault.unlock", Args: red}}, args)
	in2, err := openInbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	it, _ := in2.get(id2)
	if it.Answer != "expired" || it.Error == nil || it.Error.Code != "unavailable" || it.Action.Args != nil {
		t.Fatalf("after restart: %+v", it)
	}
	if _, err := in2.answer(id2, true); err == nil {
		t.Fatal("answered an expired approval")
	}
}

// TestInboxResultTTL: an output nobody fetched is dropped after resultTTL.
func TestInboxResultTTL(t *testing.T) {
	old := resultTTL
	resultTTL = 20 * time.Millisecond
	t.Cleanup(func() { resultTTL = old })
	in, _ := openInbox("")
	id, _ := in.post(item{App: "a", Kind: "approval", Action: &sol.ActionCall{Action: "a.b"}}, nil)
	if _, err := in.answer(id, true); err != nil {
		t.Fatal(err)
	}
	_ = in.setResult(id, json.RawMessage(`{"x":1}`), nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		in.mu.Lock()
		n := len(in.results)
		in.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the result outlived its TTL")
		}
		time.Sleep(5 * time.Millisecond)
	}
	all := func(item) bool { return true }
	if l := in.list(all, all); l[0].Result != nil || l[0].Outcome != "ok" {
		t.Fatalf("%+v", l)
	}
}
