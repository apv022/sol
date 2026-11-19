package files

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/apv022/sol/sdk/go/sol"
)

func TestUIAndAssetsAreServed(t *testing.T) {
	w := newWorld()
	a := startNode(t, w.a1, nil)
	get := func(p string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", p, nil)
		rec := httptest.NewRecorder()
		a.app.Handler().ServeHTTP(rec, r)
		return rec
	}
	for p, ct := range map[string]string{"/": "text/html", "/app.js": "javascript", "/app.css": "text/css", "/kit/kit.css": "text/css",
		"/kit/tokens.css": "text/css", "/kit/kit.js": "javascript", "/kit/icons.js": "javascript", "/sol.js": "javascript"} {
		rec := get(p)
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), ct) {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", p)
		}
	}
	if !strings.Contains(get("/").Body.String(), "/kit/kit.css") {
		t.Error("index must use the kit")
	}
}

func TestUIEscapesEverythingAndUsesOnlyKitAndSolJS(t *testing.T) {
	src, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "localStorage.setItem"} {
		if strings.Contains(s, banned) && !(banned == "localStorage.setItem" && strings.Contains(s, "store.set")) {
			t.Errorf("ui/app.js uses %s", banned)
		}
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "import ") && !strings.Contains(line, "'/sol.js'") && !strings.Contains(line, "'/kit/kit.js'") {
			t.Errorf("unexpected import: %s", line)
		}
	}
	// Every network call goes through sol.js: no direct fetch except the /raw preview of text.
	if n := strings.Count(s, "fetch("); n != 1 {
		t.Errorf("fetch( used %d times", n)
	}
}

func TestManifestMatchesHandlers(t *testing.T) {
	m, err := sol.ParseManifest(ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	if m.Exec != "builtin:files" || m.UI != "/" || len(m.Folders) != 1 || m.Folders[0].Path != "*" {
		t.Fatalf("manifest: %+v", m)
	}
	for _, a := range m.Actions {
		if a.Input == nil || a.Input.Type != "object" {
			t.Errorf("%s has no input schema", a.Name)
		}
	}
	// Every declared action is wired (New panics otherwise) and the policy for delete holds.
	a := New(nil, Config{Root: t.TempDir(), DataDir: t.TempDir()})
	if a.Handler() == nil {
		t.Fatal("no handler")
	}
	spec, _ := m.Action("files.delete")
	if spec.Effect != sol.EffectDestructive || spec.Approval != sol.ApprovalAgents {
		t.Fatalf("delete: %+v", spec)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/_sol/actions/files.nonexistent", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown action: %d", rec.Code)
	}
}
