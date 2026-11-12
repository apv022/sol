package site

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHandler(t *testing.T) {
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret.txt"), "outside the site")
	dir := t.TempDir()
	write(t, filepath.Join(dir, "index.html"), "home")
	write(t, filepath.Join(dir, "blog", "index.html"), "blog index")
	write(t, filepath.Join(dir, "blog", "post.txt"), "a post")
	write(t, filepath.Join(dir, "raw", "file.txt"), "no index beside me")
	write(t, filepath.Join(dir, ".env"), "PASSWORD=hunter2")
	write(t, filepath.Join(dir, ".git", "config"), "[core]")
	write(t, filepath.Join(dir, "blog", ".draft"), "unfinished")
	symlinks := runtime.GOOS != "windows"
	if symlinks {
		if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "leak.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
			t.Fatal(err)
		}
	}

	h, closer, err := Handler(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	srv := httptest.NewServer(h)
	defer srv.Close()

	get := func(path string) (int, string, http.Header) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}

	for path, want := range map[string]string{
		"/":              "home",
		"/blog/":         "blog index",
		"/blog/post.txt": "a post",
		"/raw/file.txt":  "no index beside me",
	} {
		if code, body, _ := get(path); code != 200 || body != want {
			t.Errorf("GET %s = %d %q, want 200 %q", path, code, body, want)
		}
	}

	hidden := []string{
		"/.env", "/.git/config", "/.git/", "/blog/.draft", // dotfiles
		"/raw/",               // no index.html: no listing
		"/missing.html",       // really missing
		"/../" + "etc/passwd", // traversal
	}
	if symlinks {
		hidden = append(hidden, "/leak.txt", "/out/secret.txt")
	}
	for _, path := range hidden {
		if code, body, _ := get(path); code != 404 {
			t.Errorf("GET %s = %d %q, want 404", path, code, body)
		}
	}

	_, _, hdr := get("/blog/post.txt")
	for _, k := range []string{"Last-Modified", "Server", "Etag"} {
		if v := hdr.Get(k); v != "" {
			t.Errorf("response has %s: %q", k, v)
		}
	}

	resp, err := http.Post(srv.URL+"/", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d, want 405", resp.StatusCode)
	}
}

func TestHandlerNeedsADirectory(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Handler(filepath.Join(dir, "nope")); err == nil {
		t.Error("a missing folder was accepted")
	}
	f := filepath.Join(dir, "file")
	write(t, f, "x")
	if _, _, err := Handler(f); err == nil {
		t.Error("a file was accepted as a site folder")
	}
}
