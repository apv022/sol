// Package uie2e runs the Sol app UI against two real daemons in headless Firefox and asserts
// what it renders (ticket A5b). It builds sol into a temp dir, inits Alice (laptop; Files and
// Notes come with every init) and Bob, pairs them, logs a fresh Firefox profile in with a
// one-time login code, and reads the page's self-test report (app.js, `?selftest=1`, honoured
// only by a daemon started with SOL_UI_SELFTEST=1).
//
// It is slow and needs Firefox, so it only runs with SOL_UI_E2E=1 (`make ui-e2e`). With
// SOL_UI_SHOTS=<dir> it also saves screenshots (desktop and phone, light and dark).
package uie2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func firefox(t *testing.T) string {
	if b := os.Getenv("SOL_BROWSER"); b != "" {
		return b
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local/opt/firefox/firefox")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, err := exec.LookPath("firefox")
	if err != nil {
		t.Skip("no Firefox (set SOL_BROWSER)")
	}
	return p
}

func run(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("sol %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// start runs a foreground daemon and waits until it answers.
func start(t *testing.T, bin, dir string) {
	t.Helper()
	cmd := exec.Command(bin, "run", "--dir", dir)
	cmd.Env = append(os.Environ(), "SOL_UI_SELFTEST=1")
	log, _ := os.Create(filepath.Join(dir, "daemon.log"))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
		log.Close()
		if t.Failed() {
			b, _ := os.ReadFile(log.Name())
			t.Logf("%s daemon log:\n%s", dir, b)
		}
	})
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(filepath.Join(dir, "ui.url")); err == nil {
			if exec.Command(bin, "status", "--dir", dir).Run() == nil {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("daemon did not start in " + dir)
}

type screen struct {
	Title, Greet, Me, Avatar, Empty string
	Widgets                         []struct{ Title, Value, Sub string }
	Apps, Rows                      []string
	Overflow                        bool
}

type report struct {
	Screens map[string]screen
	Errors  []string
}

func TestSolAppAgainstRealDaemon(t *testing.T) {
	if os.Getenv("SOL_UI_E2E") != "1" {
		t.Skip("set SOL_UI_E2E=1 (make ui-e2e)")
	}
	ff := firefox(t)
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "sol")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, "./cmd/sol")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	a, b := filepath.Join(tmp, "alice"), filepath.Join(tmp, "bob")
	for _, p := range []struct{ dir, name, device string }{{a, "Alice", "laptop"}, {b, "Bob", "phone"}} {
		run(t, bin, "init", "--dir", p.dir, "--name", p.name, "--device", p.device,
			"--listen", "127.0.0.1:0", "--ui", "127.0.0.1:0", "--files", p.dir+"-files")
	}
	start(t, bin, a)
	start(t, bin, b)
	run(t, bin, "pair", "--dir", b, run(t, bin, "invite", "--dir", a))
	// Wait until Alice has a session with Bob's device (presence is what People shows).
	for i := 0; ; i++ {
		if strings.Contains(run(t, bin, "status", "--dir", a), "Bob") {
			break
		}
		if i > 100 {
			t.Fatal("Bob never came online for Alice")
		}
		time.Sleep(100 * time.Millisecond)
	}
	run(t, bin, "service", "add", "--dir", a, "blog", "8080", "--http")

	login := run(t, bin, "open", "--print", "--dir", a)
	if !strings.HasPrefix(login, "http://localhost:") {
		t.Fatalf("login url %q", login)
	}
	base := login[:strings.Index(login, "/login")]

	profile := filepath.Join(tmp, "profile")
	_ = os.MkdirAll(profile, 0o700)
	ffRun := func(args ...string) *exec.Cmd {
		cmd := exec.Command(ff, append([]string{"--headless", "--no-remote", "--profile", profile}, args...)...)
		cmd.Env = append(os.Environ(), "MOZ_HEADLESS=1")
		return cmd
	}
	cmd := ffRun(login + "&selftest=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	for i := 0; i < 300 && raw == nil; i++ {
		raw, _ = os.ReadFile(filepath.Join(a, "selftest.json"))
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if raw == nil {
		t.Fatal("the page never reported (not logged in, or the page failed to load)")
	}
	var rep report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", raw)

	home, people, devices, sharing := rep.Screens["home"], rep.Screens["people"], rep.Screens["devices"], rep.Screens["sharing"]
	if !strings.Contains(home.Greet, "Alice") {
		t.Errorf("greeting %q has no name", home.Greet)
	}
	if home.Avatar != "Alice" || !strings.Contains(home.Me, "laptop") {
		t.Errorf("sidebar me %q avatar %q", home.Me, home.Avatar)
	}
	widget := func(title string) (v, sub string) {
		for _, w := range home.Widgets {
			if strings.Contains(w.Title, title) {
				return w.Value, w.Sub
			}
		}
		t.Errorf("no %q widget in %+v", title, home.Widgets)
		return "", ""
	}
	if v, _ := widget("My devices"); v != "1" {
		t.Errorf("My devices = %q, want 1", v)
	}
	if v, _ := widget("People online"); v != "1" {
		t.Errorf("People online = %q, want 1", v)
	}
	if v, sub := widget("Storage"); v == "" || !strings.HasPrefix(sub, "of ") {
		t.Errorf("Storage widget %q %q", v, sub)
	}
	for _, want := range []string{"Files", "Notes"} {
		if !anyHas(home.Apps, want) {
			t.Errorf("app grid %v lacks %s", home.Apps, want)
		}
	}
	if home.Empty != "" {
		t.Errorf("home says %q", home.Empty)
	}
	if !anyHas(people.Rows, "bob", "Online") {
		t.Errorf("People rows %v: want Bob online", people.Rows)
	}
	if !anyHas(devices.Rows, "laptop", "This device") {
		t.Errorf("Devices rows %v", devices.Rows)
	}
	if !anyHas(sharing.Rows, "blog", "Port 8080") {
		t.Errorf("Sharing rows %v lack the blog service", sharing.Rows)
	}
	for _, e := range rep.Errors {
		t.Errorf("page error: %s", e)
	}

	if dir := os.Getenv("SOL_UI_SHOTS"); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
		for _, sc := range []struct {
			name string
			w, h int
		}{{"desktop", 1280, 800}, {"phone", 390, 844}} {
			for _, th := range []string{"light", "dark"} {
				out := filepath.Join(dir, fmt.Sprintf("sol-app-%s-%s.png", sc.name, th))
				c := ffRun("--screenshot", out, "--window-size", fmt.Sprintf("%d,%d", sc.w, sc.h), base+"/?shot=1&theme="+th)
				if o, err := c.CombinedOutput(); err != nil {
					t.Errorf("screenshot %s: %v\n%s", out, err, o)
				}
			}
		}
	}
}

func anyHas(rows []string, parts ...string) bool {
	for _, r := range rows {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(r, p)
		}
		if ok {
			return true
		}
	}
	return false
}
