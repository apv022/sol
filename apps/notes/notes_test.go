package notes

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
	"github.com/apv022/sol/sdk/go/sol/memsol"
)

type env struct {
	t    *testing.T
	app  *App
	root string
}

func newEnv(t *testing.T, files map[string]string) *env { return newEnvAPI(t, files, nil) }

// newEnvAPI is newEnv with a daemon API (memsol) for the actions that use blobs.
func newEnvAPI(t *testing.T, files map[string]string, api sol.API) *env {
	t.Helper()
	root := t.TempDir()
	for p, s := range files {
		full := filepath.Join(root, NotesDir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app := New(api, Config{Root: root, DataDir: t.TempDir(), NoWatch: true})
	if err := app.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Stop)
	return &env{t, app, root}
}

// do calls an action through the HTTP handler as my own device.
func (e *env) do(name string, args any, dryRun bool) (int, map[string]any) {
	e.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	b, _ := json.Marshal(args)
	r := httptest.NewRequest("POST", "/_sol/actions/"+name, bytes.NewReader(b))
	r.Header.Set("Sol-Self", "1")
	if dryRun {
		r.Header.Set("Sol-Dry-Run", "1")
	}
	w := httptest.NewRecorder()
	e.app.Handler().ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (e *env) ok(name string, args any) map[string]any {
	e.t.Helper()
	code, out := e.do(name, args, false)
	if code != 200 {
		e.t.Fatalf("%s: %d %v", name, code, out)
	}
	return out
}

func (e *env) read(p string) string {
	b, err := os.ReadFile(filepath.Join(e.root, NotesDir, filepath.FromSlash(p)))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) exists(p string) bool {
	_, err := os.Lstat(filepath.Join(e.root, NotesDir, filepath.FromSlash(p)))
	return err == nil
}

func paths(out map[string]any, key string) []string {
	var ps []string
	for _, n := range out[key].([]any) {
		ps = append(ps, n.(map[string]any)["path"].(string))
	}
	return ps
}

var seed = map[string]string{
	"Welcome.md":        "# Welcome\n\nHello #intro and [[Groceries]] and [[Projects/Plan|the plan]].\n`[[Code]] #notatag`\n",
	"Groceries.md":      "# Groceries\n\n#home #shopping\n\n- [ ] milk\n- [x] eggs\n",
	"Projects/Plan.md":  "# Big Plan\n\n#work/launch\n\n```\n#fenced [[Nope]]\n```\n\nSee [[groceries]] and [[Big Plan]].\n",
	"Projects/Other.md": "no heading here, about elephants and elephant seals\n",
}

func TestIndexTitlesTagsLinks(t *testing.T) {
	e := newEnv(t, seed)
	out := e.ok("notes.list", map[string]any{})
	if len(out["notes"].([]any)) != 4 {
		t.Fatalf("list: %v", out)
	}
	byPath := map[string]map[string]any{}
	for _, n := range out["notes"].([]any) {
		m := n.(map[string]any)
		byPath[m["path"].(string)] = m
	}
	if byPath["Projects/Other.md"]["title"] != "Other" || byPath["Projects/Plan.md"]["title"] != "Big Plan" {
		t.Fatalf("titles: %v", byPath)
	}
	tags := e.ok("notes.tags", nil)["tags"].([]any)
	got := map[string]bool{}
	for _, x := range tags {
		got[x.(map[string]any)["tag"].(string)] = true
	}
	for _, w := range []string{"intro", "home", "shopping", "work/launch"} {
		if !got[w] {
			t.Errorf("missing tag %s in %v", w, got)
		}
	}
	if got["notatag"] || got["fenced"] {
		t.Errorf("tags from code were indexed: %v", got)
	}
	// Folder filter is direct children only; a tag filter includes sub-tags.
	if p := paths(e.ok("notes.list", map[string]any{"folder": "Projects"}), "notes"); len(p) != 2 {
		t.Errorf("folder filter: %v", p)
	}
	if p := paths(e.ok("notes.list", map[string]any{"tag": "work"}), "notes"); len(p) != 1 || p[0] != "Projects/Plan.md" {
		t.Errorf("tag filter: %v", p)
	}
	if f := e.ok("notes.list", nil)["folders"].([]any); len(f) != 1 || f[0] != "Projects" {
		t.Errorf("folders: %v", f)
	}
}

func TestBacklinks(t *testing.T) {
	e := newEnv(t, seed)
	bl := paths(e.ok("notes.backlinks", map[string]any{"path": "Groceries.md"}), "backlinks")
	if len(bl) != 2 || bl[0] != "Projects/Plan.md" || bl[1] != "Welcome.md" { // case-insensitive, by file name
		t.Fatalf("backlinks: %v", bl)
	}
	// A path-form link with an alias, and a link by title.
	if bl := paths(e.ok("notes.backlinks", map[string]any{"path": "Projects/Plan.md"}), "backlinks"); len(bl) != 1 { // Welcome by path; Plan's own [[Big Plan]] is not a backlink
		t.Errorf("plan backlinks: %v", bl)
	}
	if c, _ := e.do("notes.backlinks", map[string]any{"path": "Nope.md"}, false); c != 404 {
		t.Errorf("missing note: %d", c)
	}
}

func TestSearch(t *testing.T) {
	e := newEnv(t, seed)
	search := func(q string) []string { return paths(e.ok("notes.search", map[string]any{"query": q}), "results") }
	if r := search("eleph"); len(r) != 1 || r[0] != "Projects/Other.md" { // prefix
		t.Errorf("prefix: %v", r)
	}
	if r := search("elephant seals"); len(r) != 1 {
		t.Errorf("and: %v", r)
	}
	if r := search("elephant milk"); len(r) != 0 {
		t.Errorf("and across notes: %v", r)
	}
	if r := search("#home"); len(r) != 1 || r[0] != "Groceries.md" {
		t.Errorf("tag: %v", r)
	}
	if r := search("#work milk"); len(r) != 0 {
		t.Errorf("tag+word: %v", r)
	}
	if r := search("big"); len(r) == 0 || r[0] != "Projects/Plan.md" { // title match ranks first
		t.Errorf("title: %v", r)
	}
	if r := search("   "); len(r) != 0 {
		t.Errorf("empty: %v", r)
	}
	out := e.ok("notes.search", map[string]any{"query": "seals"})
	if s := out["results"].([]any)[0].(map[string]any)["snippet"].(string); !strings.Contains(s, "seals") {
		t.Errorf("snippet: %q", s)
	}
}

func TestSearchScales(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 10000; i++ {
		files["bulk/n"+strings.Repeat("0", 5-len(itoa(i)))+itoa(i)+".md"] = "# Note " + itoa(i) + "\n\nlorem ipsum dolor sit amet word" + itoa(i%500) + " #t" + itoa(i%50) + "\n"
	}
	t0 := time.Now()
	e := newEnv(t, files)
	build := time.Since(t0)
	t0 = time.Now()
	for i := 0; i < 20; i++ {
		e.ok("notes.search", map[string]any{"query": "wor lorem"})
	}
	per := time.Since(t0) / 20
	t.Logf("index of 10k notes: %v, search: %v", build, per)
	if per > 200*time.Millisecond && !raceEnabled {
		t.Errorf("search too slow: %v", per)
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestWriteCreateReadAndDryRun(t *testing.T) {
	e := newEnv(t, nil)
	if c, out := e.do("notes.create", map[string]any{"title": "My: note?"}, true); c != 200 || out["dry_run"] != true || e.exists("My- note.md") {
		t.Fatalf("dry create: %d %v", c, out)
	}
	r := e.ok("notes.create", map[string]any{"title": "My: note?"})
	if r["path"] != "My- note.md" || !strings.HasPrefix(e.read("My- note.md"), "# My- note") {
		t.Fatalf("create: %v", r)
	}
	if r2 := e.ok("notes.create", map[string]any{"title": "My: note?"}); r2["path"] != "My- note 2.md" {
		t.Errorf("unique: %v", r2)
	}
	if r := e.ok("notes.create", map[string]any{"kind": "folder", "title": "Ideas"}); r["path"] != "Ideas" || !e.exists("Ideas") {
		t.Errorf("folder: %v", r)
	}
	if f := e.ok("notes.list", nil)["folders"].([]any); len(f) != 1 {
		t.Errorf("empty folder listed: %v", f)
	}

	if c, _ := e.do("notes.write", map[string]any{"path": "a/b.md", "text": "dry"}, true); c != 200 || e.exists("a") {
		t.Fatal("dry write changed the disk")
	}
	w := e.ok("notes.write", map[string]any{"path": "a/b.md", "text": "# B\nhello"})
	rd := e.ok("notes.read", map[string]any{"path": "a/b.md"})
	if rd["text"] != "# B\nhello" || rd["etag"] != w["etag"] || rd["title"] != "B" {
		t.Fatalf("read: %v", rd)
	}
	// base protects against overwriting a newer edit.
	e.ok("notes.write", map[string]any{"path": "a/b.md", "text": "second", "base": w["etag"]})
	if c, out := e.do("notes.write", map[string]any{"path": "a/b.md", "text": "stale", "base": w["etag"]}, false); c != 409 {
		t.Errorf("stale base: %d %v", c, out)
	}
	if e.read("a/b.md") != "second" {
		t.Error("stale write got through")
	}
	// No temp files are left next to the note.
	ents, _ := os.ReadDir(filepath.Join(e.root, NotesDir, "a"))
	if len(ents) != 1 {
		t.Errorf("leftovers: %v", ents)
	}
}

func TestMoveRewritesLinks(t *testing.T) {
	e := newEnv(t, seed)
	code, dr := e.do("notes.move", map[string]any{"from": "Groceries.md", "to": "Shopping.md"}, true)
	if code != 200 || e.exists("Shopping.md") || len(dr["rewritten"].([]any)) != 2 {
		t.Fatalf("dry move: %d %v", code, dr)
	}
	r := e.ok("notes.move", map[string]any{"from": "Groceries.md", "to": "Shopping"}) // .md is added
	if r["path"] != "Shopping.md" || len(r["rewritten"].([]any)) != 2 {
		t.Fatalf("move: %v", r)
	}
	w := e.read("Welcome.md")
	if !strings.Contains(w, "[[Shopping]]") || !strings.Contains(w, "[[Projects/Plan|the plan]]") || !strings.Contains(w, "`[[Code]] #notatag`") {
		t.Errorf("welcome: %q", w)
	}
	if p := e.read("Projects/Plan.md"); !strings.Contains(p, "[[Shopping]]") || !strings.Contains(p, "[[Big Plan]]") || !strings.Contains(p, "[[Nope]]") {
		t.Errorf("plan (title link and fenced code must stay): %q", p)
	}
	if bl := paths(e.ok("notes.backlinks", map[string]any{"path": "Shopping.md"}), "backlinks"); len(bl) != 2 {
		t.Errorf("backlinks follow the rename: %v", bl)
	}
	// Path-form links are rewritten too, and aliases survive.
	e.ok("notes.move", map[string]any{"from": "Projects/Plan.md", "to": "Projects/Roadmap.md"})
	if w := e.read("Welcome.md"); !strings.Contains(w, "[[Projects/Roadmap|the plan]]") {
		t.Errorf("path link: %q", w)
	}
	// rewrite_links=false leaves links alone; moving to another folder keeps stem links valid.
	e.ok("notes.move", map[string]any{"from": "Shopping.md", "to": "Home/List.md", "rewrite_links": false})
	if w := e.read("Welcome.md"); !strings.Contains(w, "[[Shopping]]") {
		t.Errorf("rewrite_links=false: %q", w)
	}
	// Clashes and bad moves are refused.
	if c, _ := e.do("notes.move", map[string]any{"from": "Welcome.md", "to": "Home/List.md"}, false); c != 409 {
		t.Errorf("clash: %d", c)
	}
	if c, _ := e.do("notes.move", map[string]any{"from": "Projects", "to": "Projects/Sub"}, false); c != 400 {
		t.Errorf("into itself: %d", c)
	}
	// Folders move with their notes and pins follow.
	e.ok("notes.pin", map[string]any{"path": "Projects/Other.md", "pinned": true})
	e.ok("notes.move", map[string]any{"from": "Projects", "to": "Work"})
	if !e.exists("Work/Other.md") || e.exists("Projects") || e.app.idx.get("Work/Other.md") == nil || e.app.idx.get("Projects/Other.md") != nil {
		t.Error("folder move")
	}
	if p := e.ok("notes.list", map[string]any{"pinned": true})["notes"].([]any); len(p) != 1 {
		t.Errorf("pin followed: %v", p)
	}
}

func TestTidyNames(t *testing.T) {
	e := newEnv(t, nil)
	// What "New note" makes: Untitled.md, Untitled 2.md, Untitled 3.md, each with an empty heading.
	for range 3 {
		e.ok("notes.create", map[string]any{"title": "Untitled", "text": "# \n\n"})
	}
	e.ok("notes.write", map[string]any{"path": "Untitled.md", "text": "# Groceries\n\nmilk"})
	e.ok("notes.write", map[string]any{"path": "Untitled 2.md", "text": "# Groceries\n\neggs"}) // same title
	e.ok("notes.write", map[string]any{"path": "Mine.md", "text": "# Something else\n[[Untitled]]"})
	e.ok("notes.write", map[string]any{"path": "Kept.md", "text": "# Not the name"})

	code, dr := e.do("notes.tidy_names", nil, true)
	if code != 200 || dr["dry_run"] != true || !e.exists("Untitled.md") || len(dr["renamed"].([]any)) != 2 {
		t.Fatalf("dry run: %d %v", code, dr)
	}
	r := e.ok("notes.tidy_names", nil)
	if got := r["renamed"].([]any); len(got) != 2 {
		t.Fatalf("renamed: %v", r)
	}
	if !e.exists("Groceries.md") || !e.exists("Groceries 2.md") || e.exists("Untitled.md") || e.exists("Untitled 2.md") {
		t.Error("untitled notes not renamed after their heading")
	}
	if !e.exists("Untitled 3.md") {
		t.Error("a note without a heading must keep its name")
	}
	if !e.exists("Kept.md") || !e.exists("Mine.md") {
		t.Error("a chosen name was changed")
	}
	if m := e.read("Mine.md"); !strings.Contains(m, "[[Groceries]]") {
		t.Errorf("links to the renamed note: %q", m)
	}
	// paths limits it to the given notes.
	e.ok("notes.write", map[string]any{"path": "Untitled 3.md", "text": "# Ideas"})
	e.ok("notes.write", map[string]any{"path": "Untitled 9.md", "text": "# Later"})
	if r := e.ok("notes.tidy_names", map[string]any{"paths": []string{"Untitled 3.md"}}); len(r["renamed"].([]any)) != 1 || !e.exists("Ideas.md") || !e.exists("Untitled 9.md") {
		t.Errorf("paths: %v", r)
	}
}

func TestDeleteAndPin(t *testing.T) {
	e := newEnv(t, seed)
	if c, out := e.do("notes.delete", map[string]any{"paths": []string{"Groceries.md", "Projects"}}, true); c != 200 || out["dry_run"] != true || !e.exists("Groceries.md") {
		t.Fatalf("dry delete: %d %v", c, out)
	}
	spec, _ := e.app.manifest.Action("notes.delete")
	if spec.Effect != sol.EffectDestructive || spec.Approval != sol.ApprovalAgents {
		t.Fatalf("policy: %+v", spec)
	}
	e.ok("notes.pin", map[string]any{"path": "Groceries.md", "pinned": true})
	if l := paths(e.ok("notes.list", nil), "notes"); l[0] != "Groceries.md" {
		t.Errorf("pinned first: %v", l)
	}
	e.ok("notes.delete", map[string]any{"paths": []string{"Groceries.md", "Projects"}})
	if e.exists("Groceries.md") || e.exists("Projects") || len(paths(e.ok("notes.list", nil), "notes")) != 1 {
		t.Error("delete")
	}
	if c, _ := e.do("notes.delete", map[string]any{"paths": []string{"Nope.md"}}, false); c != 404 {
		t.Errorf("missing: %d", c)
	}
	if c, _ := e.do("notes.delete", map[string]any{"paths": []string{""}}, false); c != 400 {
		t.Errorf("notes root: %d", c)
	}
}

func TestToggleTask(t *testing.T) {
	e := newEnv(t, seed)
	rd := e.ok("notes.read", map[string]any{"path": "Groceries.md"})
	if c, _ := e.do("notes.toggle_task", map[string]any{"path": "Groceries.md", "line": 4}, true); c != 200 || e.read("Groceries.md") != rd["text"] {
		t.Fatal("dry run changed the note")
	}
	r := e.ok("notes.toggle_task", map[string]any{"path": "Groceries.md", "line": 4, "base": rd["etag"]})
	if r["checked"] != true || !strings.Contains(e.read("Groceries.md"), "- [x] milk") {
		t.Fatalf("toggle: %v %q", r, e.read("Groceries.md"))
	}
	e.ok("notes.toggle_task", map[string]any{"path": "Groceries.md", "line": 5, "checked": false})
	if !strings.Contains(e.read("Groceries.md"), "- [ ] eggs") {
		t.Error("explicit uncheck")
	}
	if c, _ := e.do("notes.toggle_task", map[string]any{"path": "Groceries.md", "line": 0}, false); c != 400 {
		t.Errorf("not a task: %d", c)
	}
	if c, _ := e.do("notes.toggle_task", map[string]any{"path": "Groceries.md", "line": 4, "base": rd["etag"]}, false); c != 409 {
		t.Errorf("stale base: %d", c)
	}
}

func TestAttach(t *testing.T) {
	e := newEnv(t, seed)
	data := base64.StdEncoding.EncodeToString([]byte("\x89PNG fake"))
	args := map[string]any{"note": "Projects/Plan.md", "name": "../../My Pic.png", "data": data}
	if c, out := e.do("notes.attach", args, true); c != 200 || out["dry_run"] != true || e.exists("Projects/attachments") {
		t.Fatalf("dry: %d %v", c, out)
	}
	r := e.ok("notes.attach", args)
	if r["path"] != "Projects/attachments/My Pic.png" || r["markdown"] != "![My Pic.png](attachments/My%20Pic.png)" {
		t.Fatalf("attach: %v", r)
	}
	if r2 := e.ok("notes.attach", args); r2["path"] != r["path"] { // same bytes: no second copy
		t.Errorf("dedupe: %v", r2)
	}
	args["data"] = base64.StdEncoding.EncodeToString([]byte("different"))
	if r3 := e.ok("notes.attach", args); r3["path"] != "Projects/attachments/My Pic-2.png" {
		t.Errorf("unique: %v", r3)
	}
	for _, bad := range []map[string]any{
		{"note": "Projects/Plan.md", "name": "x.html", "data": data},
		{"note": "Projects/Plan.md", "name": "x.png", "data": "!!"},
		{"note": "Nope.md", "name": "x.png", "data": data},
		{"note": "Projects/Plan.md", "name": "x.png", "data": base64.StdEncoding.EncodeToString(make([]byte, maxAttach+1))},
	} {
		if c, _ := e.do("notes.attach", bad, false); c < 400 {
			t.Errorf("accepted %v", bad["name"])
		}
	}
	// Attachments are not notes and are served safely.
	if n := len(e.ok("notes.list", nil)["notes"].([]any)); n != 4 {
		t.Errorf("attachments listed as notes: %d", n)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/file?path=Projects/attachments/My+Pic.png", nil)
	req.Header.Set("Sol-Self", "1")
	e.app.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("serve: %d %v", rec.Code, rec.Header())
	}
}

func TestAttachBlob(t *testing.T) {
	dev := memsol.New().AddPerson("Alice").Devices[0]
	api := dev.Client("notes")
	e := newEnvAPI(t, seed, api)
	// Bigger than the inline cap: blobs have none.
	big := bytes.Repeat([]byte("x"), maxAttach+1000)
	info, err := api.PutBlob(t.Context(), bytes.NewReader(big), "big.png")
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"note": "Projects/Plan.md", "name": "big.png", "blob": info.Hash}
	if c, out := e.do("notes.attach", args, true); c != 200 || out["dry_run"] != true || e.exists("Projects/attachments") {
		t.Fatalf("dry: %d %v", c, out)
	}
	r := e.ok("notes.attach", args)
	if r["path"] != "Projects/attachments/big.png" || r["markdown"] != "![big.png](attachments/big.png)" {
		t.Fatalf("attach: %v", r)
	}
	if got, _ := os.ReadFile(filepath.Join(e.root, NotesDir, "Projects/attachments/big.png")); !bytes.Equal(got, big) {
		t.Fatal("attachment bytes differ from the blob")
	}
	if r2 := e.ok("notes.attach", args); r2["path"] != r["path"] {
		t.Errorf("same blob must not be copied twice: %v", r2)
	}
	other, _ := api.PutBlob(t.Context(), strings.NewReader("different"), "")
	args["blob"] = other.Hash
	if r3 := e.ok("notes.attach", args); r3["path"] != "Projects/attachments/big-2.png" {
		t.Errorf("unique: %v", r3)
	}
	for _, bad := range []map[string]any{
		{"note": "Projects/Plan.md", "name": "x.png", "blob": "sha256:" + strings.Repeat("0", 64)}, // unknown
		{"note": "Projects/Plan.md", "name": "x.png", "blob": "nope"},
		{"note": "Projects/Plan.md", "name": "x.png", "blob": info.Hash, "data": "AAAA"},
		{"note": "Projects/Plan.md", "name": "x.html", "blob": info.Hash},
		{"note": "Nope.md", "name": "x.png", "blob": info.Hash},
		{"note": "Projects/Plan.md", "name": "x.png"},
	} {
		if c, _ := e.do("notes.attach", bad, false); c < 400 {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestPathSafety(t *testing.T) {
	e := newEnv(t, seed)
	outside := filepath.Join(e.root, "secret.md")
	_ = os.WriteFile(outside, []byte("secret"), 0o644)
	evil := []string{"../secret.md", "/etc/passwd", "a/../../secret.md", `..\secret.md`, "Projects/../../secret.md", ".hidden.md", "a/.x/b.md", "con.md", "x:y.md", "nul\x00.md"}
	for _, p := range evil {
		for _, a := range []struct {
			n    string
			args map[string]any
		}{
			{"notes.read", map[string]any{"path": p}},
			{"notes.write", map[string]any{"path": p, "text": "pwned"}},
			{"notes.move", map[string]any{"from": "Welcome.md", "to": p}},
			{"notes.move", map[string]any{"from": p, "to": "Z.md"}},
			{"notes.delete", map[string]any{"paths": []string{p}}},
			{"notes.attach", map[string]any{"note": p, "name": "a.png", "data": "AAAA"}},
			{"notes.create", map[string]any{"folder": p, "title": "x"}},
		} {
			if c, _ := e.do(a.n, a.args, false); c < 400 {
				t.Errorf("%s accepted %q", a.n, p)
			}
		}
	}
	if b, _ := os.ReadFile(outside); string(b) != "secret" {
		t.Fatal("file outside Notes was modified")
	}
	req := httptest.NewRequest("GET", "/file?path=../secret.md", nil)
	req.Header.Set("Sol-Self", "1")
	rec := httptest.NewRecorder()
	e.app.Handler().ServeHTTP(rec, req)
	if rec.Code < 400 {
		t.Error("/file escaped Notes/")
	}
	// A symlink inside Notes/ that points outside is not followed.
	if runtime.GOOS != "windows" {
		if err := os.Symlink(e.root, filepath.Join(e.root, NotesDir, "link")); err == nil {
			for _, p := range []string{"link/secret.md"} {
				if c, _ := e.do("notes.read", map[string]any{"path": p}, false); c < 400 {
					t.Error("read through symlink")
				}
				if c, _ := e.do("notes.write", map[string]any{"path": p, "text": "x"}, false); c < 400 {
					t.Error("write through symlink")
				}
			}
			e.app.Refresh("")
			for _, p := range e.app.idx.paths() {
				if strings.HasPrefix(p, "link/") {
					t.Error("indexed through symlink:", p)
				}
			}
		}
	}
	// Peers get nothing.
	r := httptest.NewRequest("POST", "/_sol/actions/notes.list", strings.NewReader("{}"))
	rec = httptest.NewRecorder()
	e.app.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-self caller: %d", rec.Code)
	}
}

func TestExternalEditsAreSeen(t *testing.T) {
	e := newEnv(t, seed)
	rd := e.ok("notes.read", map[string]any{"path": "Groceries.md"})
	rev := e.app.Revision()
	full := filepath.Join(e.root, NotesDir, "Groceries.md")
	if err := os.WriteFile(full, []byte("# Groceries v2\n\n#fresh zebra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(full, time.Now().Add(time.Second), time.Now().Add(time.Second))
	e.app.Refresh("Groceries.md") // what the watcher does
	if e.app.Revision() == rev {
		t.Fatal("revision did not change")
	}
	if r := paths(e.ok("notes.search", map[string]any{"query": "zebra"}), "results"); len(r) != 1 {
		t.Errorf("new text not searchable: %v", r)
	}
	if r := paths(e.ok("notes.search", map[string]any{"query": "milk"}), "results"); len(r) != 0 {
		t.Errorf("old text still searchable: %v", r)
	}
	now := e.ok("notes.read", map[string]any{"path": "Groceries.md"})
	if now["etag"] == rd["etag"] || now["title"] != "Groceries v2" {
		t.Errorf("read: %v", now)
	}
	// The editor's autosave, still based on the old etag, is refused instead of clobbering it.
	if c, _ := e.do("notes.write", map[string]any{"path": "Groceries.md", "text": "mine", "base": rd["etag"]}, false); c != 409 {
		t.Errorf("stale autosave: %d", c)
	}
	// New and deleted files and folders.
	_ = os.MkdirAll(filepath.Join(e.root, NotesDir, "Fresh", "deep"), 0o755)
	_ = os.WriteFile(filepath.Join(e.root, NotesDir, "Fresh", "deep", "x.md"), []byte("# X"), 0o644)
	e.app.Refresh("Fresh")
	if e.app.idx.get("Fresh/deep/x.md") == nil {
		t.Error("new folder not indexed")
	}
	_ = os.RemoveAll(filepath.Join(e.root, NotesDir, "Fresh"))
	e.app.Refresh("Fresh")
	if e.app.idx.get("Fresh/deep/x.md") != nil || len(e.app.idx.folders()) != 1 {
		t.Errorf("removal: %v", e.app.idx.folders())
	}
	// Our own write leaves nothing for the watcher to change.
	e.ok("notes.write", map[string]any{"path": "Welcome.md", "text": "# W\nown"})
	rev = e.app.Revision()
	e.app.Refresh("Welcome.md")
	if e.app.Revision() != rev {
		t.Error("own write echoed")
	}
}

func TestWatcherPicksUpChanges(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("inotify only")
	}
	root := t.TempDir()
	app := New(nil, Config{Root: root, DataDir: t.TempDir(), Debounce: 20 * time.Millisecond})
	if err := app.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer app.Stop()
	if !app.watching {
		t.Skip("no watcher")
	}
	_ = os.MkdirAll(filepath.Join(root, NotesDir, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(root, NotesDir, "sub", "vim.md"), []byte("# From vim\n#edited"), 0o644)
	deadline := time.Now().Add(5 * time.Second)
	for app.idx.get("sub/vim.md") == nil {
		if time.Now().After(deadline) {
			t.Fatal("watcher did not index the new note")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = os.Remove(filepath.Join(root, NotesDir, "sub", "vim.md"))
	for app.idx.get("sub/vim.md") != nil {
		if time.Now().After(deadline) {
			t.Fatal("watcher did not drop the deleted note")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRestartRebuildsFromFiles(t *testing.T) {
	e := newEnv(t, seed)
	e.ok("notes.write", map[string]any{"path": "Later.md", "text": "# Later\n#again"})
	e.ok("notes.pin", map[string]any{"path": "Later.md", "pinned": true})
	e.app.Stop()
	app2 := New(nil, Config{Root: e.root, DataDir: e.app.data, NoWatch: true})
	if err := app2.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer app2.Stop()
	if app2.idx.get("Later.md") == nil || !app2.isPinned("Later.md") || len(app2.idx.paths()) != 5 {
		t.Errorf("rebuilt: %v", app2.idx.paths())
	}
}

func TestParseNoteAndHelpers(t *testing.T) {
	i := parseNote("x/Name.md", "---\ntitle: no\n---\n# Real #1 title ##\n#123 #ok_tag, a#b (#paren) http://x/#frag [[A|b]] [[C#h]] [[ b ]]\n~~~\n#no\n~~~\n")
	if i.Title != "Real #1 title" || strings.Join(i.Tags, ",") != "ok_tag,paren" || strings.Join(i.Links, ",") != "A,C,b" {
		t.Errorf("%+v", i)
	}
	if got := sanitizeName(`a/b:c`); got != "a-b-c" {
		t.Error(got)
	}
	if got := sanitizeName("CON"); got != "Untitled" {
		t.Error(got)
	}
}

func TestUIAndManifest(t *testing.T) {
	e := newEnv(t, nil)
	for p, ct := range map[string]string{"/": "text/html", "/app.js": "javascript", "/md.js": "javascript", "/app.css": "text/css", "/kit/kit.css": "text/css", "/sol.js": "javascript"} {
		rec := httptest.NewRecorder()
		e.app.Handler().ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), ct) || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	m, err := sol.ParseManifest(ManifestJSON)
	if err != nil || len(m.Folders) != 1 || m.Folders[0] != (sol.FolderAccess{Path: "Notes", Mode: "write"}) {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	for _, a := range m.Actions {
		if a.Input == nil || a.Input.Type != "object" {
			t.Errorf("%s has no schema", a.Name)
		}
	}
	src, _ := os.ReadFile("ui/app.js")
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(string(src), banned) {
			t.Errorf("app.js uses %s", banned)
		}
	}
	if strings.Contains(string(src), "fetch(") {
		t.Error("app.js must call actions through sol.js")
	}
	if c, _ := e.do("notes.list", map[string]any{"limit": "x"}, false); c != 400 {
		t.Errorf("schema validation: %d", c)
	}
}

// The UI's change stream pushes the index revision at once and again after every change.
func TestChangesStream(t *testing.T) {
	e := newEnv(t, nil)
	srv := httptest.NewServer(e.app.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/changes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	rd := bufio.NewReader(resp.Body)
	next := func() string {
		for {
			l, err := rd.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(l, "data: ") {
				return strings.TrimSpace(strings.TrimPrefix(l, "data: "))
			}
		}
	}
	first := next()
	w := e.ok("notes.write", map[string]any{"path": "a.md", "text": "# A"})
	want := fmt.Sprint(w["rev"])
	for got := next(); got != want; got = next() {
		if got == first {
			t.Fatalf("an event without a change: %s", got)
		}
	}
}
