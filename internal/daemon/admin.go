package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apv022/sol/internal/apphost"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/sdk/go/sol"
)

// Status is the reply of GET /admin/v1/status (sol status).
type Status struct {
	Person    string        `json:"person"`
	Name      string        `json:"name"`
	Device    string        `json:"device"`
	DevName   string        `json:"device_name"`
	Endpoints []string      `json:"endpoints"`
	Sessions  []SessionInfo `json:"sessions"`
	API       string        `json:"api"`
	UI        string        `json:"ui"`
	Files     string        `json:"files"`
}

// SessionInfo is one live session.
type SessionInfo struct {
	Person   string `json:"person"`
	Device   string `json:"device"`
	Relation string `json:"relation"`
}

// AppInfo is one row of GET /admin/v1/apps (sol apps).
type AppInfo struct {
	apphost.Status
	URL string `json:"url,omitempty"`
}

// Status describes this device.
func (d *Daemon) Status() Status {
	st := Status{Device: d.DeviceID(), Person: d.PersonID(), API: d.apiURL, UI: d.UIURL(), Files: d.cfg.Files,
		Endpoints: []string{}, Sessions: []SessionInfo{}}
	if b, err := d.Keyring.Bundle(); err == nil {
		st.Name = b.Person.Name
		if dv, ok := b.Devices.Find(d.Keyring.DevicePublic()); ok {
			st.DevName = dv.Name
		}
	}
	for _, e := range d.Keyring.Endpoints().Endpoints {
		st.Endpoints = append(st.Endpoints, e.Transport+" "+e.Address)
	}
	for _, s := range d.Sessions.Sessions() {
		p := s.Peer()
		st.Sessions = append(st.Sessions, SessionInfo{Person: identity.ID(p.Person), Device: identity.ID(p.Device), Relation: string(p.Relation)})
	}
	return st
}

// Apps lists the installed apps with their UI URLs.
func (d *Daemon) Apps() []AppInfo {
	out := []AppInfo{}
	for _, s := range d.Host.Status() {
		a := AppInfo{Status: s}
		if s.UI {
			a.URL = apphost.AppURL(s.ID, d.uiPort)
		}
		out = append(out, a)
	}
	return out
}

// adminHandler serves the admin API the CLI uses (under /admin/v1/, admin token only).
func (d *Daemon) adminHandler() http.Handler {
	mx := http.NewServeMux()
	j := func(pattern string, f func(r *http.Request) (any, error)) {
		mx.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			out, err := f(r)
			if err != nil {
				var se *sol.Error
				if !errors.As(err, &se) {
					err = sol.Errorf("bad_request", err.Error())
				}
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
		return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
	}
	j("GET /admin/v1/status", func(r *http.Request) (any, error) { return d.Status(), nil })
	j("POST /admin/v1/invite", func(r *http.Request) (any, error) {
		var b struct {
			Join bool `json:"join"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		l, err := d.Invite(b.Join)
		return map[string]string{"link": l}, err
	})
	j("POST /admin/v1/pair", func(r *http.Request) (any, error) {
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
	j("GET /admin/v1/apps", func(r *http.Request) (any, error) { return d.Apps(), nil })
	j("POST /admin/v1/apps/install", func(r *http.Request) (any, error) {
		var b struct {
			Dir string `json:"dir"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		return d.Host.Install(b.Dir)
	})
	j("POST /admin/v1/apps/remove", func(r *http.Request) (any, error) {
		var b struct {
			ID string `json:"id"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		return nil, d.Host.Remove(b.ID)
	})
	j("GET /admin/v1/inbox", func(r *http.Request) (any, error) { return d.API.InboxItems(), nil })
	j("POST /admin/v1/inbox/answer", func(r *http.Request) (any, error) {
		var b struct {
			ID      string `json:"id"`
			Approve bool   `json:"approve"`
		}
		if err := body(r, &b); err != nil {
			return nil, err
		}
		return d.API.Answer(r.Context(), b.ID, b.Approve)
	})
	return mx
}

// Client is the CLI's connection to a running daemon's admin API.
type Client struct {
	base  string
	token string
	hc    *http.Client
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
	api := strings.TrimSpace(string(u))
	c := &Client{token: strings.TrimSpace(string(tok))}
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

// Do calls an admin endpoint with a JSON body (nil for GET) and decodes the reply into out.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var rd io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		rd = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(r)
	if err != nil {
		return ErrNotRunning
	}
	defer resp.Body.Close()
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
