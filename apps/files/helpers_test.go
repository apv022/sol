package files

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
	"github.com/apv022/sol/sdk/go/sol/memsol"
)

// flaky wraps an API and makes every network call fail with "unavailable" while down is set:
// a device that has lost its daemon (or the network) for a while.
type flaky struct {
	sol.API
	down atomic.Bool
}

func (f *flaky) err() error {
	if f.down.Load() {
		return sol.Errorf("unavailable", "offline")
	}
	return nil
}
func (f *flaky) Append(ctx context.Context, feed string, b any) (sol.Entry, error) {
	if e := f.err(); e != nil {
		return sol.Entry{}, e
	}
	return f.API.Append(ctx, feed, b)
}
func (f *flaky) Read(ctx context.Context, a string, from uint64, n int) ([]sol.Entry, error) {
	if e := f.err(); e != nil {
		return nil, e
	}
	return f.API.Read(ctx, a, from, n)
}
func (f *flaky) Feeds(ctx context.Context, o string) ([]sol.FeedInfo, error) {
	if e := f.err(); e != nil {
		return nil, e
	}
	return f.API.Feeds(ctx, o)
}
func (f *flaky) PutBlob(ctx context.Context, r io.Reader, n string) (sol.BlobInfo, error) {
	if e := f.err(); e != nil {
		return sol.BlobInfo{}, e
	}
	return f.API.PutBlob(ctx, r, n)
}
func (f *flaky) GetBlob(ctx context.Context, h string, o sol.BlobOptions) (io.ReadCloser, error) {
	if e := f.err(); e != nil {
		return nil, e
	}
	return f.API.GetBlob(ctx, h, o)
}
func (f *flaky) StatBlob(ctx context.Context, h string, o sol.BlobOptions) (sol.BlobStat, error) {
	if e := f.err(); e != nil {
		return sol.BlobStat{}, e
	}
	return f.API.StatBlob(ctx, h, o)
}
func (f *flaky) PostInbox(ctx context.Context, p sol.InboxPost) (sol.InboxResult, error) {
	if e := f.err(); e != nil {
		return sol.InboxResult{}, e
	}
	return f.API.PostInbox(ctx, p)
}

type node struct {
	app  *App
	api  *flaky
	root string
	data string
}

func startNode(t *testing.T, dev *memsol.Device, mut func(*Config)) *node {
	t.Helper()
	n := &node{api: &flaky{API: dev.Client(appID)}, root: t.TempDir(), data: t.TempDir()}
	cfg := Config{Root: n.root, DataDir: n.data, Debounce: 20 * time.Millisecond, RetryEvery: 30 * time.Millisecond,
		ScanEvery: 60 * time.Millisecond, Logf: t.Logf}
	if mut != nil {
		mut(&cfg)
	}
	n.app = New(n.api, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	if err := n.app.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); n.app.Stop() })
	return n
}

// restart stops the node's app and starts a fresh one on the same folder and data directory.
func (n *node) restart(t *testing.T, dev *memsol.Device) {
	t.Helper()
	n.app.Stop()
	n.app = New(n.api, Config{Root: n.root, DataDir: n.data, Debounce: 20 * time.Millisecond, RetryEvery: 30 * time.Millisecond, Logf: t.Logf})
	ctx, cancel := context.WithCancel(context.Background())
	if err := n.app.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); n.app.Stop() })
}

func (n *node) path(p string) string { return filepath.Join(n.root, filepath.FromSlash(p)) }

func (n *node) write(t *testing.T, p, content string) {
	t.Helper()
	full := n.path(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (n *node) writeAt(t *testing.T, p, content string, mtime time.Time) {
	t.Helper()
	n.write(t, p, content)
	if err := os.Chtimes(n.path(p), mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func (n *node) read(p string) (string, bool) {
	b, err := os.ReadFile(n.path(p))
	return string(b), err == nil
}

func (n *node) has(p, content string) bool {
	got, ok := n.read(p)
	return ok && got == content
}

func (n *node) gone(p string) bool {
	_, err := os.Lstat(n.path(p))
	return os.IsNotExist(err)
}

// files lists every regular file in the folder, sorted, relative.
func (n *node) files() []string {
	var out []string
	_ = filepath.WalkDir(n.root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			r, _ := filepath.Rel(n.root, p)
			out = append(out, filepath.ToSlash(r))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(4 * time.Millisecond)
	}
}

// settle waits until nothing is pending on the node and the debounce has long passed.
func (n *node) settle(t *testing.T) {
	t.Helper()
	time.Sleep(6 * n.app.cfg.Debounce)
	waitFor(t, "node idle", func() bool {
		n.app.mu.RLock()
		defer n.app.mu.RUnlock()
		return n.app.busy == 0 && len(n.app.pending) == 0
	})
}

// act calls an action on the node's handler like the daemon does for my own UI.
func (n *node) act(t *testing.T, name string, args any, dry bool) (map[string]any, int) {
	t.Helper()
	return actOn(t, n.app.Handler(), name, args, dry, true)
}

func actOn(t *testing.T, h http.Handler, name string, args any, dry, self bool) (map[string]any, int) {
	t.Helper()
	b, _ := json.Marshal(args)
	r := httptest.NewRequest("POST", "/_sol/actions/"+name, bytes.NewReader(b))
	if self {
		r.Header.Set("Sol-Self", "1")
	}
	if dry {
		r.Header.Set("Sol-Dry-Run", "1")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out, w.Code
}

func (n *node) must(t *testing.T, name string, args any) map[string]any {
	t.Helper()
	out, code := n.act(t, name, args, false)
	if code != 200 {
		t.Fatalf("%s %v: %d %v", name, args, code, out)
	}
	return out
}

func (n *node) list(t *testing.T, dir string) []string {
	t.Helper()
	out := n.must(t, "files.list", map[string]any{"path": dir})
	var names []string
	for _, e := range out["entries"].([]any) {
		m := e.(map[string]any)
		s := m["name"].(string)
		if m["type"] == "dir" {
			s += "/"
		}
		names = append(names, s)
	}
	return names
}

func treeEntries(t *testing.T, api sol.API, person string) []body {
	t.Helper()
	es, err := api.Read(context.Background(), sol.Address(person, appID, treeFeed), 1, 5000)
	if err != nil {
		return nil
	}
	var out []body
	for _, e := range es {
		var b body
		_ = json.Unmarshal(e.Body, &b)
		out = append(out, b)
	}
	return out
}

func notices(p *memsol.Person) []memsol.InboxItem {
	var out []memsol.InboxItem
	for _, it := range p.Inbox() {
		if it.Kind == "notice" && strings.HasPrefix(it.Title, "Edit conflict") {
			out = append(out, it)
		}
	}
	return out
}

// world is Alice with two devices and Bob, paired.
type world struct {
	net   *memsol.Network
	alice *memsol.Person
	bob   *memsol.Person
	a1    *memsol.Device
	a2    *memsol.Device
	b1    *memsol.Device
}

func newWorld() *world {
	n := memsol.New()
	w := &world{net: n, alice: n.AddPerson("Alice"), bob: n.AddPerson("Bob")}
	w.a1 = w.alice.Devices[0]
	w.a2 = w.alice.AddDevice("laptop2", false)
	w.b1 = w.bob.Devices[0]
	n.Pair(w.alice, w.bob)
	return w
}

func osStat(p string) (os.FileInfo, error) { return os.Stat(p) }
func osRename(a, b string) error           { return os.Rename(a, b) }
func osRemove(p string) error              { return os.Remove(p) }
