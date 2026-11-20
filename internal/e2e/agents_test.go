package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/builtins"
	"github.com/apv022/sol/internal/cli"
	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/sdk/go/sol"
)

// newAgent issues an agent on d the way `sol agent new` does and returns its key file.
func newAgent(t *testing.T, d *sold.Daemon, name string, scopes ...string) string {
	t.Helper()
	k, pub, err := sold.NewAgentKey(name)
	if err != nil {
		t.Fatal(err)
	}
	info, err := d.IssueAgent(name, pub, scopes, 0)
	if err != nil {
		t.Fatal(err)
	}
	k.Cert = info.Cert
	p := filepath.Join(t.TempDir(), name+".key")
	if err := k.Save(p); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", st.Mode())
	}
	return p
}

// do runs `sol do` through the CLI logic and returns the exit code and stdout.
func do(t *testing.T, c cli.Conn, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := cli.Do(context.Background(), c, args, strings.NewReader(""), &out, &errb)
	return code, strings.TrimSpace(out.String() + errb.String())
}

// TestAgentApprovalFlow: an agent's files.delete waits for approval in the inbox; approving it
// runs the delete as that agent and records the result. Scopes are enforced, and the app sees
// Sol-Agent.
func TestAgentApprovalFlow(t *testing.T) {
	d := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, builtins.All())
	docs := attachDocs(t, d)
	_ = docs
	admin, err := sold.Connect(d.Dir())
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "Files running", func() bool { code, _ := do(t, admin, "files.list", "--json"); return code == 0 })
	if code, out := do(t, admin, "files.write", "--path", "/a.txt", "--text", "hello"); code != 0 {
		t.Fatalf("write: %d %s", code, out)
	}
	ag, err := sold.ConnectAgent(d.Dir(), newAgent(t, d, "helper", "files.*", "docs.count"))
	if err != nil {
		t.Fatal(err)
	}
	if code, out := do(t, ag, "files.list", "--json"); code != 0 || !strings.Contains(out, "a.txt") {
		t.Fatalf("agent list: %d %s", code, out)
	}
	// Outside its scopes.
	if code, out := do(t, ag, "notes.list"); code == 0 || !strings.Contains(out, "no action") {
		t.Fatalf("notes as agent: %d %s", code, out)
	}
	var raw json.RawMessage
	err = ag.Do(context.Background(), "POST", "/agent/v1/actions/call", sol.ActionCall{Action: "notes.list"}, &raw)
	if !errors.Is(err, sol.ErrForbidden) {
		t.Fatalf("direct call outside scopes: %v", err)
	}
	// The app sees the agent.
	if code, out := do(t, ag, "docs.count", "--json"); code != 0 || !strings.Contains(out, `"agent":"`) || strings.Contains(out, `"agent":""`) {
		t.Fatalf("docs.count as agent: %d %s", code, out)
	}
	// Destructive: needs approval.
	code, id := do(t, ag, "files.delete", "--paths", "/a.txt")
	id, _, _ = strings.Cut(id, "\n")
	if code != cli.ExitApproval || !strings.HasPrefix(id, "in-") {
		t.Fatalf("delete as agent: %d %s", code, id)
	}
	if _, out := do(t, admin, "files.list", "--json"); !strings.Contains(out, "a.txt") {
		t.Fatal("deleted before approval")
	}
	var items []sol.InboxItem
	if err := ag.Do(context.Background(), "GET", "/agent/v1/inbox", nil, &items); err != nil || len(items) != 1 || items[0].ID != id || items[0].Answer != "" {
		t.Fatalf("agent's inbox: %+v %v", items, err)
	}
	var it sol.InboxItem
	if err := admin.Do(context.Background(), "POST", "/admin/v1/inbox/"+id+"/approve", nil, &it); err != nil {
		t.Fatal(err)
	}
	if it.Answer != "approved" || it.Error != nil || len(it.Result) == 0 || it.Agent == "" {
		t.Fatalf("answered: %+v", it)
	}
	if _, out := do(t, admin, "files.list", "--json"); strings.Contains(out, "a.txt") {
		t.Fatalf("not deleted after approval: %s", out)
	}
	if err := admin.Do(context.Background(), "POST", "/admin/v1/inbox/"+id+"/approve", nil, &it); !errors.Is(err, sol.ErrConflict) {
		t.Fatalf("second answer: %v", err)
	}
	// The activity log names the agent.
	act, _ := docs.Activity(context.Background(), 0)
	found := false
	for _, a := range act {
		found = found || a.Action == "files.delete" && a.Agent == it.Agent && a.Result == "ok"
	}
	if !found {
		t.Fatalf("activity: %+v", act)
	}
}

// TestAgentRevokeAndExpiry: a revoked key and an expired certificate are refused; a key
// another person's device signed is refused.
func TestAgentRevokeAndExpiry(t *testing.T) {
	d := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	attachDocs(t, d)
	ctx := context.Background()
	p := newAgent(t, d, "bot", "*")
	ag, err := sold.ConnectAgent(d.Dir(), p)
	if err != nil {
		t.Fatal(err)
	}
	var who sold.AgentWhoAmI
	if err := ag.Do(ctx, "GET", "/agent/v1/whoami", nil, &who); err != nil || who.Name != "bot" || who.Person != d.PersonID() {
		t.Fatalf("%+v %v", who, err)
	}
	if _, err := d.RevokeAgent("bot"); err != nil {
		t.Fatal(err)
	}
	if err := ag.Do(ctx, "GET", "/agent/v1/whoami", nil, &who); !errors.Is(err, sol.ErrUnauthorized) {
		t.Fatalf("revoked: %v", err)
	}
	// A new agent may take the name of a revoked one.
	newAgent(t, d, "bot", "*")

	// Expired: signed two hours ago for one hour.
	k, pub, _ := sold.NewAgentKey("old")
	c, err := d.Keyring.IssueAgent(pub, "old", []string{"*"}, time.Hour, time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	k.Cert = c.Marshal()
	op := filepath.Join(t.TempDir(), "old.key")
	if err := k.Save(op); err != nil {
		t.Fatal(err)
	}
	old, _ := sold.ConnectAgent(d.Dir(), op)
	err = old.Do(ctx, "GET", "/agent/v1/whoami", nil, &who)
	if !errors.Is(err, sol.ErrUnauthorized) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired: %v", err)
	}
	// Bob's device signed it: not Alice's agent.
	bob := startNode(t, sold.InitOptions{Name: "Bob", Device: "desktop"}, nil)
	bc, _ := bob.Keyring.IssueAgent(pub, "bobs", []string{"*"}, time.Hour, time.Now())
	k.Cert = bc.Marshal()
	_ = k.Save(op)
	other, _ := sold.ConnectAgent(d.Dir(), op)
	if err := other.Do(ctx, "GET", "/agent/v1/whoami", nil, &who); !errors.Is(err, sol.ErrUnauthorized) {
		t.Fatalf("another person's agent: %v", err)
	}
	// The admin token is not an agent credential, and agent paths need agent headers.
	admin, _ := sold.Connect(d.Dir())
	if err := admin.Do(ctx, "GET", "/agent/v1/whoami", nil, &who); !errors.Is(err, sol.ErrUnauthorized) {
		t.Fatalf("admin on agent path: %v", err)
	}
	if _, err := identity.ParseAgentCert(c.Marshal()); err != nil {
		t.Fatal(err)
	}
}

// TestThemeEvents: setting a theme, changing a setting and editing a theme file each push a
// theme event to an app's event stream.
func TestThemeEvents(t *testing.T) {
	d := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	docs := attachDocs(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs, err := docs.Events(ctx, sol.EventOptions{})
	if err != nil {
		t.Fatal(err)
	}
	next := func() *sol.ThemeEvent {
		t.Helper()
		for {
			select {
			case ev := <-evs:
				if ev.Type == sol.EventTheme {
					return ev.Theme
				}
			case <-time.After(10 * time.Second):
				t.Fatal("no theme event")
			}
		}
	}
	if err := d.SetTheme("graphite"); err != nil {
		t.Fatal(err)
	}
	if th := next(); th.Name != "graphite" || th.Settings["scheme"] != "auto" {
		t.Fatalf("%+v", th)
	}
	if _, err := d.UpdateThemeSettings(map[string]string{"accent": "#b58900", "density": "compact"}); err != nil {
		t.Fatal(err)
	}
	if th := next(); th.Settings["accent"] != "#b58900" || th.Settings["density"] != "compact" {
		t.Fatalf("%+v", th)
	}
	if _, err := d.UpdateThemeSettings(map[string]string{"glass": "maybe"}); !errors.Is(err, sol.ErrBadRequest) {
		t.Fatalf("bad setting: %v", err)
	}
	if err := d.SetTheme("nope"); !errors.Is(err, sol.ErrNotFound) {
		t.Fatalf("unknown theme: %v", err)
	}
	st := d.Themes()
	if st.Active != "graphite" || len(st.Themes) < 4 {
		t.Fatalf("%+v", st)
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "android" {
		return // no watcher elsewhere yet
	}
	css := "/* @theme Mine\n   @author APV\n   @supports dark */\n:root { --tint: red; }\n"
	if err := os.WriteFile(filepath.Join(d.Status().Files, ".sol", "themes", "mine.css"), []byte(css), 0o644); err != nil {
		t.Fatal(err)
	}
	next()
	found := false
	for _, th := range d.Themes().Themes {
		found = found || th.Name == "mine" && th.Title == "Mine" && th.Author == "APV" && !th.Builtin
	}
	if !found {
		t.Fatalf("%+v", d.Themes())
	}
}

// TestRestoreFromWords: after every device is lost, a fresh directory restores the person from
// the words, asking a contact for the newest state so the new seqs start above it; the contact
// accepts the restored identity. The join request carries the joining device's name.
func TestRestoreFromWords(t *testing.T) {
	alice := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	bob := startNode(t, sold.InitOptions{Name: "Bob", Device: "desktop"}, nil)
	pairNodes(t, alice, bob)
	phone := startNode(t, sold.InitOptions{Join: true, Device: "phone"}, nil)
	link, _ := alice.Invite(true)
	if _, _, err := phone.Pair(context.Background(), link); err != nil {
		t.Fatal(err)
	}
	ab, _ := alice.Keyring.Bundle()
	if len(ab.Devices.Devices) != 2 || ab.Devices.Devices[1].Name != "phone" {
		t.Fatalf("joined device name: %+v", ab.Devices.Devices)
	}
	words, err := alice.BackupWords()
	if err != nil || len(words) != 24 {
		t.Fatal(err)
	}
	waitSession(t, bob, alice)
	from, _ := bob.Invite(false)
	_, bobsList, _ := bob.Cache.Latest(ab.Person.Person) // what Bob knows of Alice's devices

	dir := t.TempDir()
	cfg := sold.Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "SolDrive"),
		Transports: []sold.TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}}}
	if err := sold.Restore(context.Background(), dir, sold.RestoreOptions{Words: "bad words", Device: "new", Config: cfg}); !errors.Is(err, identity.ErrBadWords) {
		t.Fatalf("bad words: %v", err)
	}
	if err := sold.Restore(context.Background(), dir, sold.RestoreOptions{Words: strings.Join(words, " "), Device: "new",
		From: from, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	kr, err := identity.Open(filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	rb, err := kr.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID(rb.Person.Person) != alice.PersonID() || rb.Person.Name != "Alice" || rb.Person.Seq <= ab.Person.Seq ||
		rb.Devices.Seq <= bobsList.Seq || len(rb.Devices.Devices) != 1 {
		t.Fatalf("restored: %+v (old pc %d dl %d)", rb, ab.Person.Seq, ab.Devices.Seq)
	}
	if _, err := bob.Cache.Accept(rb, kr.DevicePublic()); err != nil {
		t.Fatalf("Bob refuses the restored identity: %v", err)
	}
	if err := sold.Restore(context.Background(), dir, sold.RestoreOptions{Words: strings.Join(words, " "), Name: "x", Device: "y", Config: cfg}); !errors.Is(err, sold.ErrExists) {
		t.Fatalf("second restore: %v", err)
	}
	// Confirming the backup removes the words.
	if err := alice.ConfirmBackup(); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.BackupWords(); !errors.Is(err, sol.ErrNotFound) {
		t.Fatalf("words after confirm: %v", err)
	}
	// network-changed is accepted.
	admin, _ := sold.Connect(alice.Dir())
	if err := admin.Do(context.Background(), "POST", "/admin/v1/network-changed", map[string]string{"reason": "wifi"}, nil); err != nil {
		t.Fatal(err)
	}
}
