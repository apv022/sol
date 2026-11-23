package calendar

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

// The UI must escape what it shows, call only actions through sol.js, never poll, never carry
// inline styles (the app origin's CSP is style-src 'self') and use the safe-area tokens.
func TestUIRules(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	js := read("ui/app.js") + read("ui/dates.js")
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "setInterval", "fetch("} {
		if strings.Contains(js, banned) {
			t.Errorf("ui uses %s", banned)
		}
	}
	for _, line := range strings.Split(read("ui/app.js"), "\n") {
		if strings.HasPrefix(line, "import ") && !strings.Contains(line, "'/sol.js'") && !strings.Contains(line, "'/kit/kit.js'") && !strings.Contains(line, "from '/dates.js'") && !strings.HasPrefix(line, "import {") {
			t.Errorf("unexpected import: %s", line)
		}
	}
	style := regexp.MustCompile(`(?i)\sstyle\s*=|<style`)
	for _, f := range []string{"ui/app.js", "ui/index.html", "ui/dates.js"} {
		if style.MatchString(read(f)) {
			t.Errorf("%s has an inline style", f)
		}
	}
	if !strings.Contains(read("ui/index.html"), "<sol-nav></sol-nav>") {
		t.Error("no way back to Sol (<sol-nav>)")
	}
	if strings.Contains(read("ui/app.css"), "env(safe-area") {
		t.Error("app.css uses env(safe-area-*): use var(--safe-*)")
	}
	calls := regexp.MustCompile(`'(calendar\.[a-z_]+)'`).FindAllStringSubmatch(js, -1)
	for _, c := range calls {
		if !strings.Contains(string(ManifestJSON), `"`+c[1]+`"`) {
			t.Errorf("UI calls %s, not in app.json", c[1])
		}
	}
	if len(calls) < 10 {
		t.Errorf("found only %d action calls in the UI", len(calls))
	}
	if regexp.MustCompile(`#[0-9a-fA-F]{6}\b`).MatchString(js) {
		t.Error("the UI has a colour literal")
	}
}
