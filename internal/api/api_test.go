package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
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
	edits    map[string]config.ServiceEdit
}

func (f *fakeBackend) EditService(name string, e config.ServiceEdit) (config.Service, error) {
	if name != "web" {
		return config.Service{}, fmt.Errorf("service %q: %w", name, config.ErrNotFound)
	}
	if f.edits == nil {
		f.edits = map[string]config.ServiceEdit{}
	}
	f.edits[name] = e
	s := config.Service{Name: "web", LocalPort: 80, Protocol: "tcp", Visibility: "private"}
	e.Apply(&s)
	return s, nil
}
func (f *fakeBackend) AddFile(r io.Reader, name string, replace bool) (string, error) {
	return f.blobs.Add(r, name, replace)
}
func (f *fakeBackend) RenameFile(ref, name string) error { return f.blobs.Rename(ref, name) }
func (f *fakeBackend) DeleteSharedFile(ref string) error { return f.blobs.Delete(ref) }
func (f *fakeBackend) DeleteReceived(hash string) error  { return f.blobs.DeleteReceived(hash) }

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

// do sends a state-changing request with the CSRF token unless csrf is "".
func do(t *testing.T, h http.Handler, method, path, csrf, ctype string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	r.Host = "127.0.0.1:8080"
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func csrfOf(t *testing.T, h http.Handler) string {
	t.Helper()
	var st struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(get(t, h, "/api/status").Body.Bytes(), &st)
	if st.CSRF == "" {
		t.Fatal("no csrf token")
	}
	return st.CSRF
}

func TestEditServiceRoute(t *testing.T) {
	b := newFake(t)
	h := Handler(b, fstest.MapFS{})
	tok := csrfOf(t, h)
	body := `{"local_port":8080,"protocol":"http","name":"site"}`
	if w := do(t, h, "PATCH", "/api/services/web", "", "application/json", strings.NewReader(body)); w.Code != http.StatusForbidden || b.edits != nil {
		t.Fatalf("no CSRF token: %d, edits %v", w.Code, b.edits)
	}
	w := do(t, h, "PATCH", "/api/services/web", tok, "application/json", strings.NewReader(body))
	if w.Code != 200 {
		t.Fatalf("edit: %d %s", w.Code, w.Body)
	}
	var got config.Service
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Name != "site" || got.LocalPort != 8080 || got.Protocol != "http" || got.Visibility != "private" {
		t.Fatalf("edited = %+v", got)
	}
	if e := b.edits["web"]; e.Visibility != nil || e.Dir != nil || e.Record != nil {
		t.Fatalf("unset fields reached the backend: %+v", e)
	}
	if w := do(t, h, "PATCH", "/api/services/nope", tok, "application/json", strings.NewReader(`{}`)); w.Code != http.StatusNotFound {
		t.Fatalf("unknown service: %d", w.Code)
	}
	if w := do(t, h, "PATCH", "/api/services/web", tok, "application/json", strings.NewReader(`{"prot":"http"}`)); w.Code != http.StatusBadRequest {
		t.Fatalf("misspelt field: %d", w.Code)
	}
}

func TestFilesCRUDRoutes(t *testing.T) {
	b := newFake(t)
	h := Handler(b, fstest.MapFS{})
	tok := csrfOf(t, h)
	upload := func(name, content string, replace bool) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if replace {
			mw.WriteField("replace", "1")
		}
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write([]byte(content))
		mw.Close()
		return do(t, h, "POST", "/api/files", tok, mw.FormDataContentType(), &buf)
	}
	if w := upload("a.txt", "one", false); w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if w := upload("a.txt", "two", false); w.Code != http.StatusConflict {
		t.Fatalf("taken name: %d", w.Code)
	}
	if w := upload("a.txt", "two", true); w.Code != http.StatusCreated {
		t.Fatalf("replace: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "PUT", "/api/files/a.txt/name", "", "application/json", strings.NewReader(`{"name":"b.txt"}`)); w.Code != http.StatusForbidden {
		t.Fatalf("rename without CSRF: %d", w.Code)
	}
	if w := do(t, h, "PUT", "/api/files/a%20b/name", tok, "application/json", strings.NewReader(`{"name":"c.txt"}`)); w.Code != http.StatusNotFound {
		t.Fatalf("rename unknown: %d", w.Code)
	}
	if w := do(t, h, "PUT", "/api/files/a.txt/name", tok, "application/json", strings.NewReader(`{"name":"b c.txt"}`)); w.Code != 200 {
		t.Fatalf("rename: %d %s", w.Code, w.Body)
	}
	list, _ := b.blobs.List()
	if len(list) != 1 || list[0].Name != "b c.txt" {
		t.Fatalf("after rename: %+v", list)
	}
	if w := do(t, h, "DELETE", "/api/files/b%20c.txt", "", "", nil); w.Code != http.StatusForbidden {
		t.Fatalf("delete without CSRF: %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/files/b%20c.txt", tok, "", nil); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if list, _ := b.blobs.List(); len(list) != 0 {
		t.Fatalf("after delete: %+v", list)
	}
	hash, _ := b.blobs.PutLimited(strings.NewReader("sent"), "s.txt", "peer1", 100)
	if w := do(t, h, "DELETE", "/api/files/"+hash, tok, "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("the shared-files route reached an inbox file: %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/inbox/"+hash, tok, "", nil); w.Code != 200 || b.blobs.Has(hash) {
		t.Fatalf("inbox delete: %d", w.Code)
	}
}
