package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apv022/sol/sdk/go/sol"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func apps(t *testing.T) []App {
	t.Helper()
	b, err := os.ReadFile("testdata/apps.json")
	if err != nil {
		t.Fatal(err)
	}
	var a []App
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestHelpGolden(t *testing.T) {
	a := apps(t)
	var b bytes.Buffer
	if err := HelpApps(&b, a); err != nil {
		t.Fatal(err)
	}
	golden(t, "help_apps", b.Bytes())
	b.Reset()
	if err := HelpApp(&b, a[0]); err != nil {
		t.Fatal(err)
	}
	golden(t, "help_files", b.Bytes())
	for _, n := range []string{"files.delete", "files.keep", "files.share", "notes.read"} {
		s, _ := FindAction(a, n)
		b.Reset()
		if err := HelpAction(&b, s); err != nil {
			t.Fatal(err)
		}
		golden(t, "help_"+strings.ReplaceAll(n, ".", "_"), b.Bytes())
	}
}

func build(t *testing.T, action string, args ...string) (string, error) {
	t.Helper()
	d, err := ParseDo(append([]string{action}, args...))
	if err != nil {
		return "", err
	}
	s, ok := FindAction(apps(t), action)
	if !ok {
		t.Fatalf("no %s", action)
	}
	out, err := BuildArgs(s, d, strings.NewReader(`{"path":"/from-stdin"}`))
	return string(out), err
}

func TestCoercion(t *testing.T) {
	for _, c := range []struct {
		action string
		args   []string
		want   string
	}{
		{"files.delete", []string{"--paths", "/a", "--paths", "/b,/c"}, `{"paths":["/a","/b","/c"]}`},
		{"files.delete", []string{"--paths=/x\\,y"}, `{"paths":["/x,y"]}`},
		{"files.keep", []string{"--path", "/p", "--keep"}, `{"keep":true,"path":"/p"}`},
		{"files.keep", []string{"--keep", "false", "--path", "/p", "--limit", "-3", "--ratio", "0.5", "--meta", `{"a":[1]}`},
			`{"keep":false,"limit":-3,"meta":{"a":[1]},"path":"/p","ratio":0.5}`},
		{"files.keep", []string{"--args", `{"path":"/j","keep":true}`, "--keep=no"}, `{"keep":false,"path":"/j"}`},
		{"files.list", []string{"--args", "-"}, `{"path":"/from-stdin"}`},
		{"files.share", []string{"--path", "/s", "--people", "bob,carol", "--mode", "read"}, `{"mode":"read","path":"/s","people":["bob","carol"]}`},
		{"notes.read", []string{"--anything", "12", "--word", "hi", "--flag"}, `{"anything":12,"flag":true,"word":"hi"}`},
	} {
		got, err := build(t, c.action, c.args...)
		if err != nil || got != c.want {
			t.Errorf("%s %v: got %s %v, want %s", c.action, c.args, got, err, c.want)
		}
	}
	for _, c := range [][]string{
		{"files.delete"},                                  // required
		{"files.list", "--nope", "x"},                     // unknown
		{"files.keep", "--path", "/p", "--keep", "maybe"}, // bad bool
		{"files.keep", "--path", "/p", "--keep", "--limit", "1.5"},
		{"files.keep", "--path", "/p", "--keep", "--ratio", "x"},
		{"files.keep", "--path", "/p", "--keep", "--meta", "{"},
		{"files.share", "--path", "/s", "--people", "b", "--mode", "admin"}, // enum
		{"files.list", "--path"},                                            // needs a value
	} {
		_, err := build(t, c[0], c[1:]...)
		var ue *UsageError
		if !errors.As(err, &ue) {
			t.Errorf("%v: want a usage error, got %v", c, err)
		}
	}
	if _, err := ParseDo([]string{"files.list", "extra"}); err == nil {
		t.Error("positional accepted")
	}
	d, err := ParseDo([]string{"--help", "files"})
	if err != nil || !d.Help || d.HelpApp != "files" {
		t.Errorf("%+v %v", d, err)
	}
	d, _ = ParseDo([]string{"files.list", "--json", "--dry-run", "--person", "p1"})
	if !d.JSON || !d.DryRun || d.Person != "p1" {
		t.Errorf("%+v", d)
	}
}

func TestRenderGolden(t *testing.T) {
	var b bytes.Buffer
	for _, in := range []string{
		`[{"name":"a.txt","size":3,"dir":false},{"name":"b","dir":true,"tags":["x","y"]}]`,
		`{"path":"/","count":2,"entries":[{"name":"a","size":1},{"name":"b","size":2}],"nested":{"k":[1,{"z":null}]}}`,
		`["one","two"]`,
		`[]`,
		`"just text"`,
		`not json`,
	} {
		b.WriteString("> " + in + "\n")
		if err := Render(&b, []byte(in)); err != nil {
			t.Fatal(err)
		}
		b.WriteString("\n")
	}
	golden(t, "render", b.Bytes())
}

// fake is a Conn over canned replies.
type fake struct {
	apps  []App
	reply json.RawMessage
	err   error
	got   sol.ActionCall
}

func (f *fake) Prefix() string { return "/agent/v1" }

func (f *fake) Do(_ context.Context, method, path string, in, out any) error {
	if strings.HasPrefix(path, "/agent/v1/actions?") || path == "/agent/v1/actions" {
		b, _ := json.Marshal(f.apps)
		return json.Unmarshal(b, out)
	}
	f.got = in.(sol.ActionCall)
	if f.err != nil {
		return f.err
	}
	*(out.(*json.RawMessage)) = f.reply
	return nil
}

func TestDoExitCodes(t *testing.T) {
	f := &fake{apps: apps(t), reply: json.RawMessage(`{"approval":"in-1"}`)}
	run := func(args ...string) (int, string, string) {
		var o, e bytes.Buffer
		code := Do(context.Background(), f, args, strings.NewReader(""), &o, &e)
		return code, o.String(), e.String()
	}
	if code, out, _ := run("files.delete", "--paths", "/a"); code != ExitApproval || out != "in-1\n" || string(f.got.Args) != `{"paths":["/a"]}` {
		t.Fatalf("approval: %d %q %s", code, out, f.got.Args)
	}
	f.reply = json.RawMessage(`{"ok":true}`)
	if code, out, _ := run("files.list", "--json", "--dry-run"); code != ExitOK || out != "{\"ok\":true}\n" || !f.got.DryRun {
		t.Fatalf("json: %d %q", code, out)
	}
	if code, out, _ := run("files.list"); code != ExitOK || out != "ok: true\n" {
		t.Fatalf("human: %d %q", code, out)
	}
	if code, _, _ := run("files.delete"); code != ExitUsage {
		t.Fatalf("missing arg: %d", code)
	}
	if code, _, _ := run("files.nope"); code != ExitUsage {
		t.Fatalf("unknown action: %d", code)
	}
	f.err = sol.Errorf("forbidden", "no")
	if code, _, msg := run("files.list"); code != ExitError || !strings.Contains(msg, "no") {
		t.Fatalf("error: %d", code)
	}
	if code, out, _ := run("--help"); code != ExitOK || !strings.Contains(out, "files") {
		t.Fatalf("help: %d", code)
	}
	if code, out, _ := run("files.delete", "--help"); code != ExitOK || !strings.Contains(out, "--paths") {
		t.Fatalf("action help: %d", code)
	}
}
