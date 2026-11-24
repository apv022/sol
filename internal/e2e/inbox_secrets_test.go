package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/sdk/go/sol"
)

// vaultManifest is a password-manager-like app whose actions take and return secrets.
const vaultManifest = `{"id": "vault", "name": "Vault", "version": "0.1.0", "exec": "sol-vault", "scopes": ["inbox"],
 "actions": [
  {"name": "vault.unlock", "effect": "write", "approval": "agents",
   "input": {"type": "object", "properties": {"user": {"type": "string"}, "password": {"type": "string", "secret": true}}}},
  {"name": "vault.put", "effect": "write", "approval": "agents",
   "input": {"type": "object", "properties": {"name": {"type": "string"},
     "item": {"type": "object", "properties": {"password": {"type": "string", "secret": true}}}}}},
  {"name": "vault.get", "effect": "read", "approval": "agents", "output_secret": true,
   "input": {"type": "object", "properties": {"name": {"type": "string"}}}},
  {"name": "vault.note", "effect": "write", "approval": "agents",
   "input": {"type": "object", "properties": {"text": {"type": "string"}}}}
 ]}`

const (
	masterPW  = "MASTER-pw-1f3a9c"
	itemPW    = "ITEM-pw-77be20"
	outputPW  = "OUTPUT-pw-c0ffee"
	plainNote = "plain-note-5150"
)

// startVaultNode starts a daemon in dir (initialising it first when init is set) with the vault
// app attached; got receives the args each action call reached the app with.
func startVaultNode(t *testing.T, dir string, init bool, got *sync.Map) *sold.Daemon {
	t.Helper()
	if init {
		o := sold.InitOptions{Name: "Alice", Device: "laptop", Config: sold.Config{UI: "127.0.0.1:0",
			Files: filepath.Join(dir, "SolDrive"), Transports: []sold.TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}}}}
		if err := sold.Init(dir, o); err != nil {
			t.Fatal(err)
		}
	}
	d, err := sold.Start(sold.Options{Dir: dir, InProcessApps: true, Log: slog.New(slog.DiscardHandler),
		SameUser: func(_, _ netip.AddrPort) (bool, error) { return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	m, err := sol.ParseManifest([]byte(vaultManifest))
	if err != nil {
		t.Fatal(err)
	}
	acts := sol.NewActions(m)
	for _, a := range m.Actions {
		acts.Handle(a.Name, func(_ context.Context, c sol.Call) (any, error) {
			got.Store(a.Name, string(c.Args))
			if a.Name == "vault.get" {
				return map[string]string{"password": outputPW}, nil
			}
			return map[string]bool{"done": true}, nil
		})
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: acts}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	if _, err := d.Host.Attach(m, ln.Addr().String()); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestApprovalSecretsStayOffDisk is issue #4: an agent's approvals whose args or output are
// secrets never put them in inbox.json; secret args are shown redacted; an approved call's
// output reaches the agent once; a restart expires an open approval with secret args.
func TestApprovalSecretsStayOffDisk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var got sync.Map
	d := startVaultNode(t, dir, true, &got)
	closed := false
	t.Cleanup(func() {
		if !closed {
			d.Close()
		}
	})
	admin, err := sold.Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	ag, err := sold.ConnectAgent(dir, newAgent(t, d, "helper", "vault.*"))
	if err != nil {
		t.Fatal(err)
	}
	inboxFile := filepath.Join(dir, "inbox.json")
	noSecretsOnDisk := func(when string) {
		t.Helper()
		b, err := os.ReadFile(inboxFile)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{masterPW, itemPW, outputPW} {
			if bytes.Contains(b, []byte(s)) {
				t.Fatalf("%s: inbox.json holds a secret: %s", when, b)
			}
		}
	}
	call := func(action, args string) string {
		t.Helper()
		var res map[string]string
		if err := ag.Do(ctx, "POST", "/agent/v1/actions/call", sol.ActionCall{Action: action, Args: json.RawMessage(args)}, &res); err != nil || res["approval"] == "" {
			t.Fatalf("%s as agent: %v %v", action, res, err)
		}
		return res["approval"]
	}
	agentItem := func(id string) sol.InboxItem {
		t.Helper()
		var items []sol.InboxItem
		if err := ag.Do(ctx, "GET", "/agent/v1/inbox", nil, &items); err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.ID == id {
				return it
			}
		}
		t.Fatalf("agent's inbox lacks %s: %+v", id, items)
		return sol.InboxItem{}
	}
	answer := func(id, verb string) sol.InboxItem {
		t.Helper()
		var it sol.InboxItem
		if err := admin.Do(ctx, "POST", "/admin/v1/inbox/"+id+"/"+verb, nil, &it); err != nil {
			t.Fatal(err)
		}
		return it
	}

	// A secret argument: shown redacted, kept in memory, used for real on approval.
	unlockArgs := `{"user":"alice","password":"` + masterPW + `"}`
	id := call("vault.unlock", unlockArgs)
	noSecretsOnDisk("unlock filed")
	open := agentItem(id)
	sum := sha256.Sum256([]byte(unlockArgs))
	if !open.SecretArgs || open.ArgsDigest != "sha256:"+hex.EncodeToString(sum[:]) || open.Action == nil ||
		!strings.Contains(string(open.Action.Args), sol.Redacted) || !strings.Contains(string(open.Action.Args), `"user":"alice"`) {
		t.Fatalf("open approval: %+v %s", open, open.Action.Args)
	}
	var all []sol.InboxItem
	if err := admin.Do(ctx, "GET", "/admin/v1/inbox", nil, &all); err != nil || len(all) != 1 || strings.Contains(string(all[0].Action.Args), masterPW) {
		t.Fatalf("admin inbox: %+v %v", all, err)
	}
	it := answer(id, "approve")
	if v, _ := got.Load("vault.unlock"); !strings.Contains(v.(string), masterPW) {
		t.Fatalf("the app got %v", v)
	}
	if it.Answer != "approved" || it.Outcome != "ok" || string(it.Result) != `{"done":true}` || it.Action.Args != nil {
		t.Fatalf("approve reply: %+v", it)
	}
	noSecretsOnDisk("unlock approved")
	if b, _ := os.ReadFile(inboxFile); bytes.Contains(b, []byte(`"alice"`)) || bytes.Contains(b, []byte(`done`)) {
		t.Fatalf("an answered approval kept args or result: %s", b)
	}
	if first := agentItem(id); string(first.Result) != `{"done":true}` || first.Outcome != "ok" {
		t.Fatalf("first fetch: %+v", first)
	}
	if again := agentItem(id); again.Result != nil || again.Outcome != "ok" || again.Answer != "approved" {
		t.Fatalf("second fetch: %+v", again)
	}

	// A nested secret, denied.
	id = call("vault.put", `{"name":"bank","item":{"password":"`+itemPW+`"}}`)
	noSecretsOnDisk("put filed")
	if it := agentItem(id); !strings.Contains(string(it.Action.Args), `"name":"bank"`) || strings.Contains(string(it.Action.Args), itemPW) {
		t.Fatalf("nested secret: %s", it.Action.Args)
	}
	if it := answer(id, "deny"); it.Answer != "denied" || it.Action.Args != nil {
		t.Fatalf("deny reply: %+v", it)
	}
	noSecretsOnDisk("put denied")
	if _, ran := got.Load("vault.put"); ran {
		t.Fatal("a denied call ran")
	}

	// A secret output: only the agent gets it, once; the approver's reply leaves it out.
	id = call("vault.get", `{"name":"bank"}`)
	if it := answer(id, "approve"); it.Result != nil || it.Outcome != "ok" {
		t.Fatalf("approve reply carries a secret output: %+v", it)
	}
	noSecretsOnDisk("get approved")
	if first := agentItem(id); !strings.Contains(string(first.Result), outputPW) {
		t.Fatalf("agent's first fetch: %+v", first)
	}
	if again := agentItem(id); again.Result != nil {
		t.Fatalf("output delivered twice: %+v", again)
	}

	// A restart expires an open approval with secret args; one without secrets stays open.
	secretID := call("vault.unlock", unlockArgs)
	plainID := call("vault.note", `{"text":"`+plainNote+`"}`)
	noSecretsOnDisk("before restart")
	d.Close()
	closed = true
	d = startVaultNode(t, dir, false, &got)
	closed = false
	noSecretsOnDisk("after restart")
	if it, ok := d.API.InboxItem(secretID); !ok || it.Answer != "expired" || it.Error == nil || it.Error.Code != "unavailable" || it.Action.Args != nil {
		t.Fatalf("secret approval after restart: %+v", it)
	}
	if it, ok := d.API.InboxItem(plainID); !ok || it.Answer != "" || !strings.Contains(string(it.Action.Args), plainNote) {
		t.Fatalf("plain approval after restart: %+v", it)
	}
	if _, err := d.API.Answer(ctx, secretID, true); err == nil {
		t.Fatal("an expired approval was answered")
	}
}
