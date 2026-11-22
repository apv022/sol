package tasks

import (
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestUIAndAssetsAreServed(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false)
	for p, ct := range map[string]string{"/": "text/html", "/app.js": "javascript", "/dates.js": "javascript", "/app.css": "text/css",
		"/kit/kit.css": "text/css", "/kit/tokens.css": "text/css", "/kit/kit.js": "javascript", "/sol.js": "javascript"} {
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), ct) {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", p)
		}
	}
}

// The UI must escape what it shows, call only actions through sol.js, and never carry inline
// styles (the app origin's CSP is style-src 'self').
func TestUIRules(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	js := read("ui/app.js")
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "setInterval", "fetch("} {
		if strings.Contains(js, banned) {
			t.Errorf("ui/app.js uses %s", banned)
		}
	}
	for _, line := range strings.Split(js, "\n") {
		if strings.HasPrefix(line, "import ") && !strings.Contains(line, "'/sol.js'") && !strings.Contains(line, "'/kit/kit.js'") && !strings.Contains(line, "'/dates.js'") {
			t.Errorf("unexpected import: %s", line)
		}
	}
	style := regexp.MustCompile(`(?i)\sstyle\s*=|<style`)
	for _, f := range []string{"ui/app.js", "ui/index.html", "ui/dates.js"} {
		if style.MatchString(read(f)) {
			t.Errorf("%s has an inline style", f)
		}
	}
	// Every action the UI calls exists in the manifest.
	calls := regexp.MustCompile(`'(tasks\.[a-z_]+)'`).FindAllStringSubmatch(js, -1)
	for _, c := range calls {
		if !strings.Contains(string(ManifestJSON), `"`+c[1]+`"`) {
			t.Errorf("UI calls %s, not in app.json", c[1])
		}
	}
	if len(calls) < 8 {
		t.Errorf("found only %d action calls in the UI", len(calls))
	}
	// Colours come from tokens and the list palette, never from literals in JS.
	if regexp.MustCompile(`#[0-9a-fA-F]{6}\b`).MatchString(js) {
		t.Error("ui/app.js has a colour literal")
	}
}

func TestManifestMatchesHandlers(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false)
	_ = a // New panics on a handler/manifest mismatch; start proves it built
}
