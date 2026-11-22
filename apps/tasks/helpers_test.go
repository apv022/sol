package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
	"github.com/apv022/sol/sdk/go/sol/memsol"
)

// fakeClock is a manual clock: time moves only when a test says so, and timers fire then.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func newClock(s string) *fakeClock {
	t, err := time.ParseInLocation("2006-01-02T15:04", s, time.UTC)
	if err != nil {
		panic(err)
	}
	return &fakeClock{now: t}
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool { c.mu.Lock(); defer c.mu.Unlock(); was := !t.stopped; t.stopped = true; return was }
}

// armed is the due time of the next live timer, zero when none.
func (c *fakeClock) armed() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var next time.Time
	for _, t := range c.timers {
		if !t.stopped && (next.IsZero() || t.at.Before(next)) {
			next = t.at
		}
	}
	return next
}

// set moves time to t and runs the timers that are due.
func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	var due []*fakeTimer
	for _, x := range c.timers {
		if !x.stopped && !x.at.After(t) {
			x.stopped = true
			due = append(due, x)
		}
	}
	c.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
	for _, x := range due {
		x.f()
	}
}

// start creates and starts a Tasks instance on a device. follow=false leaves feed reading to the test.
func start(t *testing.T, d *memsol.Device, clk Clock, data string, follow bool) *App {
	t.Helper()
	a := New(d.Client("tasks"), Config{Clock: clk, Loc: time.UTC, DataDir: data, Logf: t.Logf})
	a.noFollow = !follow
	ctx, cancel := context.WithCancel(context.Background())
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop(); cancel() })
	return a
}

type env struct {
	net      *memsol.Network
	clk      *fakeClock
	alice    *memsol.Person
	bob, car *memsol.Person
}

func newEnv(t *testing.T) *env {
	t.Helper()
	n := memsol.New()
	e := &env{net: n, clk: newClock("2026-11-18T10:30")}
	n.Now = e.clk.Now
	e.alice, e.bob, e.car = n.AddPerson("Alice"), n.AddPerson("Bob"), n.AddPerson("Carol")
	n.Pair(e.alice, e.bob)
	n.Pair(e.alice, e.car)
	for _, p := range []*memsol.Person{e.alice, e.bob, e.car} {
		m, err := sol.ParseManifest(ManifestJSON)
		if err != nil {
			t.Fatal(err)
		}
		p.Install(m, http.NotFoundHandler())
	}
	return e
}

// call runs an action through the app's real HTTP handler as one of my own devices.
func call(t *testing.T, a *App, name string, args map[string]any, dryRun ...bool) map[string]any {
	t.Helper()
	out, err := tryCall(a, name, args, dryRun...)
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return out
}

func tryCall(a *App, name string, args map[string]any, dryRun ...bool) (map[string]any, error) {
	if args == nil {
		args = map[string]any{}
	}
	b, _ := json.Marshal(args)
	r := httptest.NewRequest("POST", "/_sol/actions/"+name, bytes.NewReader(b))
	r.Header.Set("Sol-Person", a.me)
	r.Header.Set("Sol-Device", a.dev)
	r.Header.Set("Sol-Self", "1")
	if len(dryRun) > 0 && dryRun[0] {
		r.Header.Set("Sol-Dry-Run", "1")
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 {
		msg := w.Body.String()
		if e, ok := out["error"].(map[string]any); ok {
			msg, _ = e["code"].(string)
			m, _ := e["message"].(string)
			msg += ": " + m
		}
		return nil, &callErr{w.Code, msg}
	}
	return out, nil
}

type callErr struct {
	status int
	msg    string
}

func (e *callErr) Error() string { return e.msg }

func tasksOf(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	var res []map[string]any
	raw, _ := json.Marshal(out["tasks"])
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func titles(ts []map[string]any) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t["title"].(string))
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newReq(action, body string, h map[string]string) *http.Request {
	r := httptest.NewRequest("POST", "/_sol/actions/"+action, strings.NewReader(body))
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
