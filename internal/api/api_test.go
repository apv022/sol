package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/transport"
)

// fakeBackend implements the parts of Backend these tests reach; any other
// method panics through the nil embedded interface.
type fakeBackend struct {
	Backend
	opts     []config.Option
	impactOf map[string]Impact
	asked    []string
	blobs    *blob.Store
}

func (f *fakeBackend) Blobs() *blob.Store       { return f.blobs }
func (f *fakeBackend) Options() []config.Option { return append([]config.Option(nil), f.opts...) }
func (f *fakeBackend) SetOption(key, value string) (bool, error) {
	for i, o := range f.opts {
		if o.Key == key {
			f.opts[i].Value = value
			return o.Restart, nil
		}
	}
	return false, fmt.Errorf("setting %q: %w", key, config.ErrNotFound)
}
func (f *fakeBackend) OptionImpact(key, value string) (Impact, error) {
	f.asked = append(f.asked, key+"="+value)
	im, ok := f.impactOf[key]
	if !ok {
		return Impact{}, fmt.Errorf("setting %q: %w", key, config.ErrNotFound)
	}
	im.Key, im.Value = key, value
	return im, nil
}
func (f *fakeBackend) Status() StatusInfo {
	return StatusInfo{NodeID: "n", Name: "box", Transports: []transport.Status{{
		Name: "tor", State: transport.StateStarting, Phase: "retrying", Progress: 45, WaitingFor: "a network connection",
		PhaseSince: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Attempt: 2,
		RetryAt: time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC), Problem: "no route", Restarts: 1,
	}}}
}

func newFake(t *testing.T) *fakeBackend {
	bs, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &fakeBackend{
		blobs: bs,
		opts: []config.Option{
			{Key: "tor", Label: "Tor", Kind: "bool", Value: "on", Restart: true},
			{Key: "name", Label: "Name", Kind: "text", Value: "box"},
		},
		impactOf: map[string]Impact{
			"tor": {Changes: true, Restart: true, Confirm: true, Unreachable: []ImpactPeer{{ID: "p1", Name: "laptop"}}},
			// Effects and Unreachable left nil: the handler must send [] not null.
			"name": {Changes: true},
		},
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "127.0.0.1:8080"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSettingImpact(t *testing.T) {
	b := newFake(t)
	h := Handler(b, fstest.MapFS{})
	w := get(t, h, "/api/settings/tor/impact?value=off")
	if w.Code != 200 {
		t.Fatalf("impact: %d %s", w.Code, w.Body)
	}
	var im Impact
	if err := json.Unmarshal(w.Body.Bytes(), &im); err != nil {
		t.Fatal(err)
	}
	if im.Key != "tor" || im.Value != "off" || !im.Restart || !im.Confirm || len(im.Unreachable) != 1 || im.Unreachable[0].Name != "laptop" {
		t.Fatalf("impact = %+v", im)
	}
	// It is a GET and changes nothing.
	if b.opts[0].Value != "on" {
		t.Fatal("asking for the impact changed the setting")
	}

	w = get(t, h, "/api/settings/name/impact?value=x")
	if body := w.Body.String(); !strings.Contains(body, `"effects":[]`) || !strings.Contains(body, `"unreachable":[]`) {
		t.Fatalf("empty lists should be [] for the panel: %s", body)
	}

	if w = get(t, h, "/api/settings/nope/impact?value=1"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown key: %d", w.Code)
	}
	// The Host check still applies.
	r := httptest.NewRequest("GET", "/api/settings/tor/impact?value=off", nil)
	r.Host = "evil.example"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != http.StatusForbidden {
		t.Fatalf("foreign host: %d", rw.Code)
	}
}

func TestSettingsShowRunningValueAndRestartNeeded(t *testing.T) {
	b := newFake(t)
	h := Handler(b, fstest.MapFS{})
	status := func() map[string]any {
		var m map[string]any
		json.Unmarshal(get(t, h, "/api/status").Body.Bytes(), &m)
		return m
	}
	if status()["restart_needed"] != false {
		t.Fatal("restart needed before any change")
	}
	b.SetOption("tor", "off")
	b.SetOption("name", "other") // applies at once: never pending
	var list []SettingView
	json.Unmarshal(get(t, h, "/api/settings").Body.Bytes(), &list)
	for _, v := range list {
		switch v.Key {
		case "tor":
			if v.Value != "off" || v.Running != "on" || !v.Pending {
				t.Fatalf("tor: %+v", v)
			}
		case "name":
			if v.Running != "other" || v.Pending {
				t.Fatalf("name: %+v", v)
			}
		}
	}
	if status()["restart_needed"] != true {
		t.Fatal("restart not needed after changing tor")
	}
	b.SetOption("tor", "on") // changed back: nothing to restart for
	if status()["restart_needed"] != false {
		t.Fatal("restart still needed after changing tor back")
	}
}

// The panel shows Tor's start-up from these fields; they must reach it.
func TestStatusPassesTransportProgress(t *testing.T) {
	h := Handler(newFake(t), fstest.MapFS{})
	var st struct {
		Transports []map[string]any `json:"transports"`
	}
	if err := json.Unmarshal(get(t, h, "/api/status").Body.Bytes(), &st); err != nil || len(st.Transports) != 1 {
		t.Fatalf("status: %v %+v", err, st)
	}
	tor := st.Transports[0]
	for _, k := range []string{"phase", "progress", "waiting_for", "phase_since", "attempt", "retry_at", "problem", "restarts"} {
		if _, ok := tor[k]; !ok {
			t.Errorf("status lacks transports[].%s: %v", k, tor)
		}
	}
	if tor["phase"] != "retrying" || tor["progress"] != float64(45) {
		t.Fatalf("tor = %v", tor)
	}
}
