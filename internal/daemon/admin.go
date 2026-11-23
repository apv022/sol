package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apv022/sol/internal/appapi"
	"github.com/apv022/sol/internal/apphost"
	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/sdk/go/sol"
)

// The admin API (design/ADMIN.md): /admin/v1/... served to the CLI over the API socket with
// admin.token, and to the Sol app in the browser at the UI root with a session cookie
// (browser.go). One handler serves both; every change is written to the activity log as
// sol.* and pushed on GET /admin/v1/events.

// Status is the reply of GET /admin/v1/status (sol status).
type Status struct {
	// Version is the running daemon's build (Version), so an upgrade that wasn't restarted shows.
	Version      string           `json:"version"`
	Me           StatusMe         `json:"me"`
	Endpoints    []EndpointInfo   `json:"endpoints"`
	Transports   []TransportState `json:"transports"`
	PeerSessions []SessionInfo    `json:"peer_sessions"`
	Sessions     []BrowserSession `json:"sessions"`
	Storage      *Storage         `json:"storage,omitempty"`
	PID          int              `json:"pid"`
	API          string           `json:"api"`
	UI           string           `json:"ui"`
	Files        string           `json:"files"`
}

// StatusMe is who this device is.
type StatusMe struct {
	Person IDName `json:"person"`
	Device IDName `json:"device"`
}

// IDName is an ID with its display name.
type IDName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// EndpointInfo is one published address; Kind is its transport.
type EndpointInfo struct {
	Addr string `json:"addr"`
	Kind string `json:"kind"`
}

// SessionInfo is one live session with a device (and, as an event, one that came up or ended).
type SessionInfo struct {
	ID       string    `json:"id"` // the peer device's ID
	Person   string    `json:"person"`
	Device   string    `json:"device"`
	Peer     string    `json:"peer"` // display name: the person's name, and the device's if known
	Relation string    `json:"relation"`
	Since    time.Time `json:"since,omitzero"`
	Up       *bool     `json:"up,omitempty"` // events only
	// Path says how the session was reached and why (the Sol app's Advanced view); Paths
	// lists the paths to that device that have had trouble (backoff or probation).
	Path  *session.PathInfo   `json:"path,omitempty"`
	Paths []session.PathState `json:"paths,omitempty"`
}

// Storage says what the Sol folder holds and how much room the device has. Used is the size
// of the synced folder (SolDrive), measured in the background and refreshed at most once a
// minute when status is asked (Measured is false until the first measurement finishes); Free
// is what the device has available to this user and Total the size of its disk.
type Storage struct {
	Used     uint64 `json:"used"`
	Free     uint64 `json:"free"`
	Total    uint64 `json:"total"`
	Measured bool   `json:"measured"`
}

// AppInfo is one row of GET /admin/v1/apps (sol apps).
type AppInfo struct {
	apphost.Status
	URL         string   `json:"url,omitempty"`
	Icon        string   `json:"icon,omitempty"`
	Description string   `json:"description,omitempty"`
	Scopes      []string `json:"scopes"`
}

// Status describes this device.
func (d *Daemon) Status() Status {
	return d.status("")
}

func (d *Daemon) status(browser string) Status {
	st := Status{Version: Version, PID: os.Getpid(), API: d.apiURL, UI: d.UIURL(), Files: d.cfg.Files, Endpoints: []EndpointInfo{},
		Transports: d.Transports(), PeerSessions: []SessionInfo{}, Sessions: d.browser.list(browser)}
	st.Me.Person.ID, st.Me.Device.ID = d.me()
	if b, err := d.Keyring.Bundle(); err == nil {
		st.Me.Person.Name = b.Person.Name
		if dv, ok := b.Devices.Find(d.Keyring.DevicePublic()); ok {
			st.Me.Device.Name = dv.Name
		}
	}
	for _, e := range d.Keyring.Endpoints().Endpoints {
		st.Endpoints = append(st.Endpoints, EndpointInfo{Addr: e.Address, Kind: e.Transport})
	}
	for _, s := range d.Sessions.Sessions() {
		st.PeerSessions = append(st.PeerSessions, d.liveSessionInfo(s))
	}
	if d.cfg.Files != "" {
		if free, total, ok := diskSpace(d.cfg.Files); ok {
			used, measured := d.folder.get(d.cfg.Files, d.ctx, &d.wg)
			st.Storage = &Storage{Used: used, Free: free, Total: total, Measured: measured}
		}
	}
	return st
}

// liveSessionInfo describes a live session, with its path.
func (d *Daemon) liveSessionInfo(s *session.Session) SessionInfo {
	p := s.Peer()
	si := d.sessionInfo(p.Person, p.Device, string(p.Relation))
	path := s.Path()
	si.Path, si.Paths = &path, d.Sessions.Paths(p.Device)
	return si
}

// sessionInfo describes a session with a device.
func (d *Daemon) sessionInfo(person, device []byte, relation string) SessionInfo {
	si := SessionInfo{ID: identity.ID(device), Person: identity.ID(person), Device: identity.ID(device), Relation: relation}
	name, _ := d.Contacts.Name(person)
	var dl identity.DeviceList
	if relation == "self" {
		if b, err := d.Keyring.Bundle(); err == nil {
			name, dl = b.Person.Name, b.Devices
		}
	} else if _, l, ok := d.Cache.Latest(person); ok {
		dl = l
	}
	si.Peer = name
	if dv, ok := dl.Find(device); ok && dv.Name != "" {
		si.Peer = strings.TrimSpace(name + " · " + dv.Name)
	}
	d.mu.Lock()
	si.Since = d.since[si.Device]
	d.mu.Unlock()
	return si
}

// Apps lists the installed apps with their UI URLs.
func (d *Daemon) Apps() []AppInfo {
	out := []AppInfo{}
	for _, s := range d.Host.Status() {
		out = append(out, d.appInfo(s))
	}
	return out
}

func (d *Daemon) appInfo(s apphost.Status) AppInfo {
	a := AppInfo{Status: s, Scopes: []string{}}
	if s.UI {
		a.URL = apphost.AppURL(s.ID, d.uiPort)
	}
	if m, ok := d.Host.Manifest(s.ID); ok {
		a.Icon, a.Description = m.Icon, m.Description
		if m.Scopes != nil {
			a.Scopes = m.Scopes
		}
	}
	return a
}

// appChanged reports an app change on the admin stream (apphost.Config.OnChange).
func (d *Daemon) appChanged(id string) {
	if d.Host == nil {
		return
	}
	defer d.menuChanged()
	for _, s := range d.Host.Status() {
		if s.ID == id {
			d.emit(EvApp, d.appInfo(s))
			return
		}
	}
	d.emit(EvApp, removed("id", id))
}

// activity appends a sol.* row to the activity log: what changed, a digest of the arguments
// (never the arguments, which may hold secrets), and the result.
func (d *Daemon) activity(action string, args any, err error) {
	b, _ := json.Marshal(args)
	sum := sha256.Sum256(b)
	result := "ok"
	if err != nil {
		result = "error: " + err.Error()
	}
	me, dev := d.me()
	row := sol.ActivityEntry{Time: time.Now().UTC(), Person: me, Device: dev, App: "sol", Action: action,
		ArgsDigest: "sha256:" + hex.EncodeToString(sum[:]), Result: result}
	out, _ := json.Marshal(row)
	if me == "" {
		return // no person yet: no feed to write to
	}
	if _, err := d.Store.Append(feeds.InternalApp, appapi.ActivityFeed, out); err != nil {
		d.log.Warn("activity log", "err", err)
	}
}

// BackupState is the reply of GET /admin/v1/backup.
type BackupState struct {
	// Words are the 24 words while the paper backup is unconfirmed on this device.
	Words []string `json:"words"`
	// Confirmed is true on an admin device that holds no unconfirmed words.
	Confirmed bool `json:"confirmed"`
}

// adminHandler serves the admin API under /admin/v1/. Callers are authenticated before it
// (appapi for the token, browserAdmin for the cookie).
func (d *Daemon) adminHandler() http.Handler {
	mx := http.NewServeMux()
	// j registers a JSON endpoint; a non-empty action logs every call as sol.<action>.
	j := func(pattern, action string, f func(r *http.Request) (any, error)) {
		mx.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			out, err := f(r)
			if err != nil {
				var se *sol.Error
				if !errors.As(err, &se) {
					err = sol.Errorf("bad_request", err.Error())
				}
			}
			if action != "" {
				args := map[string]string{}
				for _, k := range []string{"id", "name"} {
					if v := r.PathValue(k); v != "" {
						args[k] = v
					}
				}
				d.activity("sol."+action, args, err)
			}
			if err != nil {
				sol.WriteError(w, err)
				return
			}
			if out == nil {
				out = struct{}{}
			}
			sol.WriteJSON(w, http.StatusOK, out)
		})
	}
	body := func(r *http.Request, v any) error {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
			return sol.Errorf("bad_request", "bad JSON")
		}
		return nil
	}
	tokenOnly := func(r *http.Request) error {
		if browserSession(r.Context()) != "" {
			return sol.Errorf("forbidden", "only the CLI may do this")
		}
		return nil
	}

	j("GET /admin/v1/status", "", func(r *http.Request) (any, error) { return d.status(browserSession(r.Context())), nil })

	// Browser login.
	j("POST /admin/v1/login-code", "login-code", func(r *http.Request) (any, error) {
		if err := tokenOnly(r); err != nil {
			return nil, err
		}
		c := d.browser.code()
		return map[string]any{"code": c, "url": d.UIURL() + "login?code=" + c,
			"expires": time.Now().Add(LoginCodeTTL).UTC()}, nil
	})
	// "Remove Sol from this device": the person types their name to confirm. The daemon stops;
	// the process archives the identity and serves setup at the same address (setup.go).
	j("POST /admin/v1/reset", "reset", func(r *http.Request) (any, error) {
		var in struct {
			Confirm string `json:"confirm"`
		}
		if err := body(r, &in); err != nil {
			return nil, err
		}
		name := ""
		if b, err := d.Keyring.Bundle(); err == nil {
			name = b.Person.Name
		}
		if strings.TrimSpace(in.Confirm) == "" || !strings.EqualFold(strings.TrimSpace(in.Confirm), strings.TrimSpace(name)) {
			return nil, sol.Errorf("bad_request", "type your name ("+name+") to confirm")
		}
		tok := d.requestReset()
		return map[string]string{"setup": "/?setup=" + tok}, nil
	})
	mx.HandleFunc("POST /admin/v1/logout", func(w http.ResponseWriter, r *http.Request) {
		id := browserSession(r.Context())
		if id == "" {
			sol.WriteError(w, sol.Errorf("bad_request", "not a browser session"))
			return
		}
		d.browser.revoke(id)
		d.activity("sol.logout", map[string]string{"session": id}, nil)
		setSessionCookie(w, "", -1)
		setSignedOutCookie(w, true) // or the same-user auto-login would sign this browser straight back in
		d.emitStatus()
		sol.WriteJSON(w, http.StatusOK, struct{}{})
	})
	j("GET /admin/v1/sessions", "", func(r *http.Request) (any, error) { return d.browser.list(browserSession(r.Context())), nil })
	j("DELETE /admin/v1/sessions/{id}", "sessions.revoke", func(r *http.Request) (any, error) {
		if !d.browser.revoke(r.PathValue("id")) {
			return nil, sol.Errorf("not_found", "no such session")
		}
		d.emitStatus()
		return nil, nil
	})

	// People and devices.
	j("GET /admin/v1/people", "", func(r *http.Request) (any, error) { return d.People(), nil })
	j("PATCH /admin/v1/people/{id}", "people.update", func(r *http.Request) (any, error) {
		var b struct {
			Petname   *string `json:"petname"`
			Anonymous *bool   `json:"anonymous"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		if b.Petname == nil && b.Anonymous == nil {
			return nil, sol.Errorf("bad_request", "nothing to change (petname, anonymous)")
		}
		if b.Anonymous != nil {
			if _, err := d.SetAnonymous(r.PathValue("id"), *b.Anonymous); err != nil {
				return nil, err
			}
		}
		if b.Petname != nil {
			return d.SetPetname(r.PathValue("id"), *b.Petname)
		}
		p, _ := d.person(r.PathValue("id"))
		return p, nil
	})
	j("DELETE /admin/v1/people/{id}", "people.unpair", func(r *http.Request) (any, error) {
		return nil, d.Unpair(r.Context(), r.PathValue("id"))
	})
	j("GET /admin/v1/devices", "", func(r *http.Request) (any, error) { return d.Devices(), nil })
	j("PATCH /admin/v1/devices/{id}", "devices.update", func(r *http.Request) (any, error) {
		var p DevicePatch
		if err := body(r, &p); err != nil {
			return nil, err
		}
		return d.UpdateDevice(r.PathValue("id"), p)
	})
	j("DELETE /admin/v1/devices/{id}", "devices.remove", func(r *http.Request) (any, error) {
		return nil, d.RemoveDevice(r.PathValue("id"))
	})
	j("POST /admin/v1/invite", "invite", func(r *http.Request) (any, error) {
		var b struct {
			Join      bool `json:"join"`
			Anonymous bool `json:"anonymous"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		var l string
		var err error
		switch {
		case b.Anonymous && b.Join:
			return nil, sol.Errorf("bad_request", "a join link adds your own device; it can't be anonymous")
		case b.Anonymous:
			l, err = d.InviteAnonymous()
		default:
			l, err = d.Invite(b.Join)
		}
		return map[string]any{"link": l, "expires": time.Now().Add(pairing.TTL).UTC()}, err
	})
	j("POST /admin/v1/pair", "pair", func(r *http.Request) (any, error) {
		var b struct {
			Link string `json:"link"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		kind, person, err := d.Pair(ctx, b.Link)
		return map[string]string{"kind": kind, "person": person}, err
	})

	// Transports.
	j("GET /admin/v1/transports", "", func(r *http.Request) (any, error) { return d.Transports(), nil })
	j("POST /admin/v1/transports/{name}", "", func(r *http.Request) (any, error) {
		var b struct {
			Enabled *bool `json:"enabled"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		if b.Enabled == nil {
			return nil, sol.Errorf("bad_request", `say {"enabled": true} or false`)
		}
		return d.SetTransport(r.PathValue("name"), *b.Enabled)
	})

	// Apps.
	j("GET /admin/v1/apps", "", func(r *http.Request) (any, error) { return d.Apps(), nil })
	j("POST /admin/v1/apps/install", "apps.install", func(r *http.Request) (any, error) {
		if err := tokenOnly(r); err != nil {
			return nil, err // a directory on this computer: the CLI's job, not a web page's
		}
		var b struct {
			Dir string `json:"dir"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		return d.Host.Install(b.Dir)
	})
	j("POST /admin/v1/apps/{id}/remove", "apps.remove", func(r *http.Request) (any, error) {
		return nil, d.Host.Remove(r.PathValue("id"))
	})
	j("POST /admin/v1/apps/{id}/restart", "apps.restart", func(r *http.Request) (any, error) {
		return nil, d.Host.Restart(r.PathValue("id"))
	})

	// Port sharing.
	j("GET /admin/v1/services", "", func(r *http.Request) (any, error) { return d.ports.Services(), nil })
	j("POST /admin/v1/services", "services.add", func(r *http.Request) (any, error) {
		var s Service
		if err := body(r, &s); err != nil {
			return nil, err
		}
		return d.ports.AddService(s)
	})
	j("PATCH /admin/v1/services/{name}", "services.update", func(r *http.Request) (any, error) {
		var p ServicePatch
		if err := body(r, &p); err != nil {
			return nil, err
		}
		return d.ports.UpdateService(r.PathValue("name"), p)
	})
	j("DELETE /admin/v1/services/{name}", "services.remove", func(r *http.Request) (any, error) {
		return nil, d.ports.RemoveService(r.PathValue("name"))
	})
	j("GET /admin/v1/tunnels", "", func(r *http.Request) (any, error) { return d.ports.Tunnels(), nil })
	j("POST /admin/v1/tunnels", "tunnels.open", func(r *http.Request) (any, error) {
		var b struct {
			Person  string `json:"person"`
			Service string `json:"service"`
			Port    int    `json:"port"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		t, err := d.ports.OpenTunnel(d.resolvePerson(b.Person), b.Service, b.Port)
		if err == nil {
			d.emit(EvTunnel, t)
		}
		return t, err
	})
	j("DELETE /admin/v1/tunnels/{id}", "tunnels.close", func(r *http.Request) (any, error) {
		id := r.PathValue("id")
		if err := d.ports.CloseTunnel(id); err != nil {
			return nil, err
		}
		d.emit(EvTunnel, removed("id", id))
		return nil, nil
	})

	// Inbox.
	j("GET /admin/v1/inbox", "", func(r *http.Request) (any, error) { return d.API.InboxItems(), nil })
	answer := func(approve bool) func(r *http.Request) (any, error) {
		return func(r *http.Request) (any, error) {
			id := r.PathValue("id")
			if it, ok := d.API.InboxItem(id); ok && it.Answer != "" {
				return nil, sol.Errorf("conflict", "already "+it.Answer)
			}
			_, err := d.API.Answer(r.Context(), id, approve)
			// Once answered, the item records what the action did (or how it failed).
			if it, ok := d.API.InboxItem(id); ok && it.Answer != "" {
				return it, nil
			}
			return nil, err
		}
	}
	j("POST /admin/v1/inbox/{id}/approve", "inbox.approve", answer(true))
	j("POST /admin/v1/inbox/{id}/deny", "inbox.deny", answer(false))
	j("POST /admin/v1/inbox/{id}/dismiss", "inbox.dismiss", func(r *http.Request) (any, error) {
		id := r.PathValue("id")
		if err := d.API.Dismiss(id); err != nil {
			return nil, err
		}
		d.emit(EvInbox, removed("id", id))
		return nil, nil
	})

	// Actions (sol do).
	j("GET /admin/v1/actions", "", func(r *http.Request) (any, error) {
		return d.actionApps(r.URL.Query().Get("app"), nil), nil
	})
	mx.HandleFunc("POST /admin/v1/actions/call", func(w http.ResponseWriter, r *http.Request) {
		var c sol.ActionCall
		if err := body(r, &c); err != nil {
			sol.WriteError(w, err)
			return
		}
		out, ok, err := d.callSol(r.Context(), c)
		if !ok {
			out, err = d.API.CallAs(r.Context(), "", c)
		}
		if err != nil {
			sol.WriteError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})

	// Agents.
	j("GET /admin/v1/agents", "", func(r *http.Request) (any, error) { return d.AllAgents(), nil })
	j("POST /admin/v1/agents", "agents.issue", func(r *http.Request) (any, error) {
		if err := tokenOnly(r); err != nil {
			return nil, err // the key file is written where the agent runs: the CLI's job
		}
		var b struct {
			Name   string   `json:"name"`
			Key    string   `json:"key"` // the agent's public key, as an ID
			Scopes []string `json:"scopes"`
			Days   int      `json:"days"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		pub, err := identity.ParseID(b.Key)
		if err != nil {
			return nil, sol.Errorf("bad_request", "bad agent key")
		}
		return d.IssueAgent(b.Name, pub, b.Scopes, time.Duration(b.Days)*24*time.Hour)
	})
	j("POST /admin/v1/agents/{name}/renew", "agents.renew", func(r *http.Request) (any, error) {
		var b struct {
			Days int `json:"days"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		return d.RenewAgent(r.PathValue("name"), time.Duration(b.Days)*24*time.Hour)
	})
	j("POST /admin/v1/agents/{name}/revoke", "agents.revoke", func(r *http.Request) (any, error) {
		id, err := d.RevokeAgent(r.PathValue("name"))
		return map[string]string{"id": id}, err
	})

	// A key made here, for the Sol app's "New agent key": the daemon makes the key pair (the
	// browser cannot write a key file where the agent runs), signs the certificate, keeps the key
	// file where `sol agent new` does (<dir>/agents/<name>.key, 0600) and returns the key file's
	// text once so it can be copied to the machine the agent runs on. Nothing here is logged.
	j("POST /admin/v1/agents/new", "agents.new", func(r *http.Request) (any, error) {
		var b struct {
			Name   string   `json:"name"`
			Scopes []string `json:"scopes"`
			Person string   `json:"person"`
			Days   int      `json:"days"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		if !agentNameRe.MatchString(b.Name) {
			return nil, sol.Errorf("bad_request", "an agent name is lowercase letters, digits, . _ - (up to 64)")
		}
		scopes := slices.Clone(b.Scopes)
		if b.Person == "self" {
			b.Person, _ = d.me()
		}
		if b.Person != "" {
			scopes = append(scopes, "person:"+b.Person)
		}
		k, pub, err := NewAgentKey(b.Name)
		if err != nil {
			return nil, err
		}
		info, err := d.IssueAgent(b.Name, pub, scopes, time.Duration(b.Days)*24*time.Hour)
		if err != nil {
			return nil, err
		}
		k.Cert = info.Cert
		file := filepath.Join(d.dir, "agents", b.Name+".key")
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			return nil, err
		}
		if err := k.Save(file); err != nil {
			return nil, err
		}
		keyText, _ := json.MarshalIndent(k, "", "  ")
		return map[string]any{"agent": info, "key_file": string(keyText), "path": file}, nil
	})

	// Wallpapers: the person's own images (wallpapers.go). The upload is the raw image with its
	// Content-Type; the Sol app shows one when the wallpaper setting is custom:<id>.
	j("GET /admin/v1/wallpapers", "", func(r *http.Request) (any, error) { return d.Wallpapers(), nil })
	j("POST /admin/v1/wallpapers", "wallpapers.add", func(r *http.Request) (any, error) {
		return d.AddWallpaper(r.Header.Get("Content-Type"), r.Body)
	})
	j("DELETE /admin/v1/wallpapers/{id}", "wallpapers.remove", func(r *http.Request) (any, error) {
		return nil, d.DeleteWallpaper(r.PathValue("id"))
	})

	// Appearance.
	j("GET /admin/v1/themes", "", func(r *http.Request) (any, error) { return d.Themes(), nil })
	j("GET /admin/v1/theme", "", func(r *http.Request) (any, error) {
		return map[string]any{"name": d.activeTheme(), "settings": d.themeSettings()}, nil
	})
	j("POST /admin/v1/theme", "theme.set", func(r *http.Request) (any, error) {
		var b struct {
			Name string `json:"name"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		return nil, d.SetTheme(b.Name)
	})
	j("POST /admin/v1/theme/settings", "theme.settings", func(r *http.Request) (any, error) {
		var b map[string]string
		if err := body(r, &b); err != nil {
			return nil, err
		}
		return d.UpdateThemeSettings(b)
	})

	// Desktop notifications of Inbox items (notify.go); per device, not synced.
	j("GET /admin/v1/notifications", "", func(r *http.Request) (any, error) { return d.Notifications(), nil })
	j("POST /admin/v1/notifications", "notifications.set", func(r *http.Request) (any, error) {
		var b struct {
			Desktop bool `json:"desktop"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		return d.SetNotifications(b.Desktop)
	})

	// Backup.
	j("GET /admin/v1/backup", "", func(r *http.Request) (any, error) {
		w, ok := d.Keyring.PendingWords()
		if !ok {
			return BackupState{Words: []string{}, Confirmed: d.Keyring.IsAdmin()}, nil
		}
		return BackupState{Words: w}, nil
	})
	j("POST /admin/v1/backup/confirm", "backup.confirm", func(r *http.Request) (any, error) { return nil, d.ConfirmBackup() })

	j("POST /admin/v1/network-changed", "", func(r *http.Request) (any, error) {
		var b struct {
			Reason string `json:"reason"`
		}
		_ = body(r, &b)
		d.NetworkChanged(b.Reason)
		return nil, nil
	})
	// sol stop: answer first, then let the owner of the process (sol run) call Close.
	j("POST /admin/v1/shutdown", "", func(r *http.Request) (any, error) {
		if err := tokenOnly(r); err != nil {
			return nil, err
		}
		d.RequestShutdown()
		return nil, nil
	})
	mx.HandleFunc("GET /admin/v1/events", func(w http.ResponseWriter, r *http.Request) { d.events.serve(w, r) })
	mx.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		sol.WriteError(w, sol.Errorf("not_found", "no such endpoint"))
	})
	return mx
}

// resolvePerson accepts a person ID or a contact's petname.
func (d *Daemon) resolvePerson(s string) string {
	if _, err := identity.ParseID(s); err == nil {
		return s
	}
	for _, p := range d.People() {
		if p.Petname == strings.ToLower(s) {
			return p.ID
		}
	}
	return s
}

func itoa(n int) string { return strconv.Itoa(n) }

// Client is the CLI's connection to a running daemon: its admin API (Connect), or its agent
// API as one agent key (ConnectAgent).
type Client struct {
	base  string
	token string
	hc    *http.Client

	agent     *AgentKey // set for an agent client
	agentPath string    // its key file, rewritten when the daemon renews the certificate
}

// Prefix is where this client's endpoints live: "/admin/v1" or "/agent/v1".
func (c *Client) Prefix() string {
	if c.agent != nil {
		return "/agent/v1"
	}
	return "/admin/v1"
}

// IsAgent reports whether the client calls as an agent key.
func (c *Client) IsAgent() bool { return c.agent != nil }

// ConnectAgent reaches the daemon running on dir as the agent whose key file is keyPath. It
// does not read the admin token. SOL_API, when set, overrides dir's api.url.
func ConnectAgent(dir, keyPath string) (*Client, error) {
	k, err := LoadAgentKey(keyPath)
	if err != nil {
		return nil, err
	}
	api := os.Getenv("SOL_API")
	if api == "" {
		u, err := os.ReadFile(filepath.Join(dir, "api.url"))
		if err != nil {
			return nil, ErrNotRunning
		}
		api = string(u)
	}
	c, err := dialAPI(strings.TrimSpace(api))
	if err != nil {
		return nil, err
	}
	c.agent, c.agentPath = &k, keyPath
	return c, nil
}

// ErrNotRunning means no daemon answers for the data directory.
var ErrNotRunning = errors.New("daemon: sol is not running here (start it with sol run)")

// Connect reaches the daemon running on dir, through api.url and admin.token.
func Connect(dir string) (*Client, error) {
	u, err := os.ReadFile(filepath.Join(dir, "api.url"))
	if err != nil {
		return nil, ErrNotRunning
	}
	tok, err := os.ReadFile(filepath.Join(dir, "admin.token"))
	if err != nil {
		return nil, ErrNotRunning
	}
	c, err := dialAPI(strings.TrimSpace(string(u)))
	if err != nil {
		return nil, err
	}
	c.token = strings.TrimSpace(string(tok))
	return c, nil
}

// dialAPI returns a client for an API address in SOL_API form.
func dialAPI(api string) (*Client, error) {
	c := &Client{}
	network, addr := "tcp", ""
	if p, ok := strings.CutPrefix(api, "unix://"); ok {
		network, addr, c.base = "unix", p, "http://sol"
	} else if h, ok := strings.CutPrefix(api, "http://"); ok {
		addr, c.base = h, api
	} else {
		return nil, fmt.Errorf("daemon: bad api.url %q", api)
	}
	c.hc = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}}}
	return c, nil
}

// renewed stores a certificate the daemon renewed in the agent's key file.
func (c *Client) renewed(b64 string) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return
	}
	nc, err := identity.ParseAgentCert(raw)
	if err != nil || !bytes.Equal(nc.Agent, c.agent.private().Public().(ed25519.PublicKey)) {
		return
	}
	c.agent.Cert = raw
	if c.agentPath != "" {
		_ = c.agent.Save(c.agentPath)
	}
}

// Do calls an admin endpoint with a JSON body (nil for GET) and decodes the reply into out.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	if c.agent != nil {
		c.agent.sign(r, body, time.Now())
	} else {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(r)
	if err != nil {
		return ErrNotRunning
	}
	defer resp.Body.Close()
	if n := resp.Header.Get("Sol-Agent-Renewed"); n != "" && c.agent != nil {
		c.renewed(n)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		var w struct {
			Error *sol.Error `json:"error"`
		}
		if json.Unmarshal(b, &w) == nil && w.Error != nil {
			return w.Error
		}
		return fmt.Errorf("daemon: %s", resp.Status)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
