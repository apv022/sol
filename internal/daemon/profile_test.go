package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/sdk/go/sol"
)

// TestProfileAdminAndActions: my name and picture through the admin API (CLI and browser), the
// UI root's /avatars route, and the sol.profile.* actions (dry run, validation, agent approval).
func TestProfileAdminAndActions(t *testing.T) {
	d := testDaemon(t)
	c := adminClient(t, d)
	ctx := context.Background()
	var me MeInfo
	if err := c.Do(ctx, "GET", "/admin/v1/me", nil, &me); err != nil || me.NameSource != NameFromCertificate || !me.CanSign || me.Name == "" || me.Avatar != "" {
		t.Fatalf("%+v %v", me, err)
	}
	cert := me.CertName
	if err := c.Do(ctx, "PATCH", "/admin/v1/me", map[string]string{"name": "  Ada L.  "}, &me); err != nil || me.Name != "Ada L." || me.NameSource != NameFromProfile || me.CertName != cert {
		t.Fatalf("%+v %v", me, err)
	}
	if err := c.Do(ctx, "PATCH", "/admin/v1/me", map[string]string{"name": strings.Repeat("x", 65)}, nil); err == nil {
		t.Fatal("a long name was accepted")
	}
	if st := d.Status(); st.Me.Person.Name != "Ada L." || st.Me.Profile.Name != "Ada L." {
		t.Fatalf("status.me: %+v", st.Me)
	}

	// The picture, uploaded raw from the browser; served back on the UI root to a session only.
	root := "localhost:" + strconv.Itoa(d.UIPort())
	origin := "http://" + root
	cookie := login(t, d)
	pic := "\x89PNG\r\n\x1a\n" + "tiny"
	r := uiReq(t, d, "PUT", root, "/admin/v1/me/avatar", origin, cookie, pic, map[string]string{"Content-Type": "image/png"})
	if err := json.NewDecoder(r.Body).Decode(&me); err != nil || r.StatusCode != 200 || me.AvatarHash != identity.AvatarHash([]byte(pic)) || me.Avatar == "" {
		t.Fatalf("%d %v %+v", r.StatusCode, err, me)
	}
	if r := uiReq(t, d, "PUT", root, "/admin/v1/me/avatar", origin, cookie, "GIF89a....", nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("a GIF: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "PUT", root, "/admin/v1/me/avatar", origin, cookie, "\xff\xd8\xff"+strings.Repeat("x", identity.MaxAvatarSize), nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("a large picture: %d", r.StatusCode)
	}
	r = uiReq(t, d, "GET", root, me.Avatar, "", cookie, "", nil)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || string(b) != pic || r.Header.Get("Content-Type") != "image/png" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%d %q %v", r.StatusCode, b, r.Header)
	}
	if r := uiReq(t, d, "GET", root, me.Avatar, "", "", "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a session: %d", r.StatusCode)
	}
	for _, p := range []string{"/avatars/..%2f..%2fidentity.key", "/avatars/" + strings.Repeat("0", 64)} {
		if r := uiReq(t, d, "GET", root, p, "", cookie, "", nil); r.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
	}
	if fi, err := os.Stat(filepath.Join(d.Dir(), "avatars", strings.TrimPrefix(me.AvatarHash, "sha256:"))); err != nil || !isPrivate(fi) {
		t.Fatalf("stored picture: %v", err)
	}
	me = MeInfo{}
	if err := c.Do(ctx, "DELETE", "/admin/v1/me/avatar", nil, &me); err != nil || me.Avatar != "" || me.AvatarHash != "" {
		t.Fatalf("%+v %v", me, err)
	}

	// The actions.
	call := func(action, args string, dry bool) (json.RawMessage, error) {
		var out json.RawMessage
		err := c.Do(ctx, "POST", "/admin/v1/actions/call", sol.ActionCall{Action: action, Args: json.RawMessage(args), DryRun: dry}, &out)
		return out, err
	}
	if out, err := call("sol.profile.show", "", false); err != nil || !strings.Contains(string(out), `"Ada L."`) {
		t.Fatalf("%s %v", out, err)
	}
	if out, err := call("sol.profile.set", `{"name":"Countess"}`, true); err != nil || !strings.Contains(string(out), "dry_run") || d.Me().Name != "Ada L." {
		t.Fatalf("dry run: %s %v", out, err)
	}
	if _, err := call("sol.profile.set", `{"name":"Countess"}`, false); err != nil || d.Me().Name != "Countess" {
		t.Fatalf("%v %+v", err, d.Me())
	}
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 9, 9, 9}
	args, _ := json.Marshal(map[string]string{"data": base64.StdEncoding.EncodeToString(jpeg)})
	if _, err := call("sol.profile.avatar", string(args), false); err != nil || d.Me().AvatarHash != identity.AvatarHash(jpeg) {
		t.Fatalf("data: %v %+v", err, d.Me())
	}
	file := filepath.Join(t.TempDir(), "me.webp")
	webp := []byte("RIFF\x00\x00\x00\x00WEBPVP8 x")
	_ = os.WriteFile(file, webp, 0o600)
	args, _ = json.Marshal(map[string]string{"path": file})
	if out, err := call("sol.profile.avatar", string(args), true); err != nil || !strings.Contains(string(out), identity.AvatarHash(webp)) || d.Me().AvatarHash != identity.AvatarHash(jpeg) {
		t.Fatalf("dry run: %s %v", out, err)
	}
	if _, err := call("sol.profile.avatar", string(args), false); err != nil || d.Me().AvatarHash != identity.AvatarHash(webp) {
		t.Fatalf("path: %v", err)
	}
	for _, bad := range []string{`{}`, `{"data":"%%%"}`, `{"path":"relative.png"}`, `{"data":"R0lGODlh"}`} {
		if _, err := call("sol.profile.avatar", bad, false); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	if _, err := call("sol.profile.clear_avatar", "", false); err != nil || d.Me().Avatar != "" {
		t.Fatalf("clear: %v", err)
	}

	// An agent's change waits for the person's approval.
	k, pub, _ := NewAgentKey("bot")
	ac, err := d.Keyring.IssueAgent(pub, "bot", []string{"sol.*"}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.agents.put(infoOf(ac)); err != nil {
		t.Fatal(err)
	}
	k.Cert = ac.Marshal()
	kp := filepath.Join(t.TempDir(), "bot.key")
	_ = k.Save(kp)
	agent, err := ConnectAgent(d.Dir(), kp)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := agent.Do(ctx, "POST", "/agent/v1/actions/call", sol.ActionCall{Action: "sol.profile.set", Args: json.RawMessage(`{"name":"Bot was here"}`)}, &res); err != nil || res["approval"] == nil {
		t.Fatalf("%v %v", res, err)
	}
	if d.Me().Name != "Countess" {
		t.Fatal("an agent renamed me without approval")
	}
	if err := agent.Do(ctx, "POST", "/agent/v1/actions/call", sol.ActionCall{Action: "sol.profile.show"}, &res); err != nil || res["name"] != "Countess" {
		t.Fatalf("show: %v %v", res, err)
	}
}

func isPrivate(fi os.FileInfo) bool {
	return os.PathSeparator == '\\' || fi.Mode().Perm() == 0o600
}
