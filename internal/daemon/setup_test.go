package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSetupInBrowser: the setup server refuses calls without the token or from another
// origin, "Start new" makes an identity and ends setup mode, and the URL file goes away.
func TestSetupInBrowser(t *testing.T) {
	dir := t.TempDir()
	if err := PrepareSetup(dir, Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "drive")}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type out struct {
		res SetupResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := RunSetup(ctx, SetupOptions{Dir: dir, Token: "tok", Name: "Ann", Device: "pc"})
		done <- out{r, err}
	}()
	var url string
	for i := 0; i < 100 && url == ""; i++ {
		if b, err := os.ReadFile(SetupURLFile(dir)); err == nil {
			url = strings.TrimSpace(string(b))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.HasSuffix(url, "/?setup=tok") {
		t.Fatalf("setup url %q", url)
	}
	base := strings.TrimSuffix(url, "/?setup=tok")
	post := func(path, token, origin, body string) int {
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
		req.Header.Set(SetupHeader, token)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("/setup/v1/new", "nope", base, `{"name":"Ann"}`); c != http.StatusForbidden {
		t.Errorf("wrong token: %d", c)
	}
	if c := post("/setup/v1/new", "tok", "http://evil.example", `{"name":"Ann"}`); c != http.StatusForbidden {
		t.Errorf("foreign origin: %d", c)
	}
	if Initialised(dir) {
		t.Fatal("a refused call set Sol up")
	}
	if c := post("/setup/v1/new", "tok", base, `{"name":"Ann","device":"pc"}`); c != http.StatusOK {
		t.Fatalf("new: %d", c)
	}
	o := <-done
	if o.err != nil || o.res.JoinLink != "" {
		t.Fatalf("%+v", o)
	}
	if !Initialised(dir) {
		t.Fatal("no identity after Start new")
	}
	if _, err := os.Stat(SetupURLFile(dir)); err == nil {
		t.Error("setup.url left behind")
	}
}

// TestArchiveIdentity: everything but the config, the logs and earlier archives moves aside.
func TestArchiveIdentity(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"config.json", "sol.log", "sol.log.1", "peers.json"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"keys", "feeds", "removed-20200101-000000"} {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dst, err := ArchiveIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"config.json", "sol.log", "sol.log.1", "removed-20200101-000000"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s should stay: %v", n, err)
		}
	}
	for _, n := range []string{"keys", "feeds", "peers.json"} {
		if _, err := os.Stat(filepath.Join(dst, n)); err != nil {
			t.Errorf("%s not archived: %v", n, err)
		}
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			t.Errorf("%s still in place", n)
		}
	}
}
