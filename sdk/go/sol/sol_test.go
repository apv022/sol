package sol_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

const manifestJSON = `{
  "id": "files", "name": "Files", "version": "0.1.0", "exec": "sol-files",
  "scopes": ["feeds", "blobs", "people", "actions"],
  "actions": [
    {"name": "files.list", "input": {"type": "object", "properties": {"path": {"type": "string"}}}, "effect": "read"},
    {"name": "files.delete", "effect": "destructive", "approval": "agents",
     "input": {"type": "object", "required": ["paths"],
       "properties": {"paths": {"type": "array", "items": {"type": "string"}},
                      "mode": {"enum": ["soft", "hard"]}, "n": {"type": "integer"}}}}
  ]
}`

func TestManifest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.json")
	if err := os.WriteFile(p, []byte(manifestJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := sol.LoadManifest(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.RunsOn[0] != "any" || m.Actions[0].Approval != "never" || m.Actions[1].Approval != "agents" {
		t.Fatalf("defaults: %+v", m)
	}
	if _, ok := m.Action("files.delete"); !ok {
		t.Fatal("action lookup")
	}
	for _, bad := range []string{
		`{"id":"X","version":"1","exec":"e"}`,
		`{"id":"files","version":"1","exec":"e","scopes":["root"]}`,
		`{"id":"files","version":"1","exec":"e","actions":[{"name":"docs.x","effect":"read"}]}`,
		`{"id":"files","version":"1","exec":"e","actions":[{"name":"files.x","effect":"boom"}]}`,
	} {
		if _, err := sol.ParseManifest([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestSchema(t *testing.T) {
	m, _ := sol.ParseManifest([]byte(manifestJSON))
	s := m.Actions[1].Input
	ok := []string{`{"paths":["a"]}`, `{"paths":[],"mode":"soft","n":3,"extra":1}`, `{"paths":["a"],"n":3.0}`}
	bad := []string{``, `{}`, `{"paths":"a"}`, `{"paths":[1]}`, `{"paths":[],"mode":"x"}`, `{"paths":[],"n":1.5}`, `[1]`, `{`}
	for _, a := range ok {
		if err := s.Validate(json.RawMessage(a)); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	for _, a := range bad {
		err := s.Validate(json.RawMessage(a))
		if !errors.Is(err, sol.ErrBadRequest) {
			t.Errorf("%q: want bad_request, got %v", a, err)
		}
	}
	var nilSchema *sol.Schema
	if nilSchema.Validate(json.RawMessage(`1`)) != nil {
		t.Error("nil schema must accept")
	}
}

func TestActionsRouter(t *testing.T) {
	m, _ := sol.ParseManifest([]byte(manifestJSON))
	a := sol.NewActions(m)
	if got := a.Missing(); len(got) != 2 {
		t.Fatalf("missing %v", got)
	}
	var seen sol.Call
	a.Handle("files.delete", func(_ context.Context, c sol.Call) (any, error) {
		seen = c
		if c.DryRun {
			return map[string]string{"would": "delete"}, nil
		}
		return map[string]string{"did": "delete"}, nil
	})
	a.Handle("files.list", func(context.Context, sol.Call) (any, error) {
		return nil, sol.Errorf("forbidden", "no")
	})
	mux := http.NewServeMux()
	mux.Handle("/_sol/actions/", a)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	post := func(name, body string, hdr map[string]string) (int, string) {
		r, _ := http.NewRequest("POST", ts.URL+"/_sol/actions/"+name, strings.NewReader(body))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	code, body := post("files.delete", `{"paths":["/a"]}`, map[string]string{"Sol-Person": "p1", "Sol-Device": "d1", "Sol-Agent": "ag", "Sol-Self": "1"})
	if code != 200 || body != `{"did":"delete"}` {
		t.Fatalf("%d %s", code, body)
	}
	if want := (sol.Caller{Person: "p1", Device: "d1", Agent: "ag", Self: true}); seen.Caller != want || !seen.Caller.IsAgent() {
		t.Fatalf("caller %+v", seen.Caller)
	}
	if code, body = post("files.delete", `{"paths":["/a"]}`, map[string]string{"Sol-Dry-Run": "1"}); code != 200 || !strings.Contains(body, "would") || !seen.DryRun {
		t.Fatalf("dry run %d %s", code, body)
	}
	if code, _ = post("files.delete", `{"paths":1}`, nil); code != 400 {
		t.Fatalf("invalid args: %d", code)
	}
	if code, _ = post("files.list", `{}`, nil); code != 403 {
		t.Fatalf("handler error status: %d", code)
	}
	if code, _ = post("files.nope", `{}`, nil); code != 404 {
		t.Fatalf("unknown: %d", code)
	}
	r, _ := http.Get(ts.URL + "/_sol/actions/files.list")
	if r.StatusCode != 400 {
		t.Fatalf("GET: %d", r.StatusCode)
	}
	r.Body.Close()
	defer func() {
		if recover() == nil {
			t.Error("undeclared action must panic")
		}
	}()
	a.Handle("files.ghost", nil)
}

func TestCallerHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if c := sol.CallerOf(r); c != (sol.Caller{}) || c.IsAgent() {
		t.Fatalf("%+v", c)
	}
	r.Header.Set("Sol-Person", "p")
	r.Header.Set("Sol-Self", "0")
	if c := sol.CallerOf(r); c.Person != "p" || c.Self {
		t.Fatalf("%+v", c)
	}
}

func TestServeAndFromEnv(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "app.sock")
	t.Setenv("SOL_APP_LISTEN", "unix://"+sock)
	t.Setenv("SOL_API", "unix:///nonexistent.sock")
	t.Setenv("SOL_APP_TOKEN", "t")
	api, err := sol.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- sol.Serve(ctx, api, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if sol.FromContext(r.Context()) != sol.API(api) {
				http.Error(w, "no api", 500)
			}
			_, _ = w.Write([]byte(sol.CallerOf(r).Person))
		}))
	}()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	var resp *http.Response
	for i := 0; i < 100; i++ {
		req, _ := http.NewRequest("GET", "http://app/", nil)
		req.Header.Set("Sol-Person", "alice")
		if resp, err = hc.Do(req); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	var b [16]byte
	n, _ := resp.Body.Read(b[:])
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b[:n]) != "alice" {
		t.Fatalf("%d %q", resp.StatusCode, b[:n])
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFromEnvErrors(t *testing.T) {
	t.Setenv("SOL_API", "")
	if _, err := sol.FromEnv(); err == nil {
		t.Fatal("want error")
	}
	if _, err := sol.New("ftp://x", ""); err == nil {
		t.Fatal("want error")
	}
	t.Setenv("SOL_APP_LISTEN", "")
	if err := sol.Serve(context.Background(), nil, http.NotFoundHandler()); err == nil {
		t.Fatal("want error")
	}
}

func TestAddressAndErrors(t *testing.T) {
	if _, _, _, err := sol.ParseAddress("a/b"); err == nil {
		t.Fatal("bad address")
	}
	p, a, f, err := sol.ParseAddress("abc/docs/doc.1")
	if err != nil || p != "abc" || a != "docs" || f != "doc.1" || sol.Address(p, a, f) != "abc/docs/doc.1" {
		t.Fatal("address round trip")
	}
	e := sol.Errorf("not_found", "x")
	if !errors.Is(e, sol.ErrNotFound) || errors.Is(e, sol.ErrForbidden) || e.HTTPStatus() != 404 {
		t.Fatal("error matching")
	}
	w := httptest.NewRecorder()
	sol.WriteError(w, errors.New("boom"))
	if w.Code != 500 || !strings.Contains(w.Body.String(), `"internal"`) {
		t.Fatal(w.Body.String())
	}
}

func TestVerifyChain(t *testing.T) {
	mk := func(seq uint64, dev, prev string) sol.Entry {
		e := sol.Entry{Feed: "p/a/f", Seq: seq, Pos: seq, Device: dev, Time: time.Unix(1, 0), Body: json.RawMessage(`{"b":1,"a":2}`), Prev: prev}
		e.Hash, _ = e.ComputeHash()
		return e
	}
	a1 := mk(1, "d1", "")
	a2 := mk(2, "d1", a1.Hash)
	b1 := mk(1, "d2", "")
	if err := sol.VerifyChain([]sol.Entry{a1, b1, a2}); err != nil {
		t.Fatal(err)
	}
	bad := mk(2, "d1", "sha256:00")
	if err := sol.VerifyChain([]sol.Entry{a1, bad}); err == nil {
		t.Fatal("broken prev accepted")
	}
	a1.Body = json.RawMessage(`{"a":3}`)
	if err := sol.VerifyChain([]sol.Entry{a1}); err == nil {
		t.Fatal("tampered body accepted")
	}
}
