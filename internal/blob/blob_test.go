package blob

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/web"
)

func TestPutGetRange(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := "0123456789abcdef"
	hash, err := s.Put(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(content))
	if hash != hex.EncodeToString(want[:]) {
		t.Fatalf("hash = %s", hash)
	}
	if _, err := s.PutNamed(strings.NewReader(content), `C:\music\song.flac`); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.List(); len(l) != 1 || l[0].Name != "song.flac" {
		t.Fatalf("name not stored: %+v", l)
	}
	if !s.Has(hash) || s.Has("../etc/passwd") {
		t.Fatal("Has wrong")
	}
	list, _ := s.List()
	if len(list) != 1 || list[0].Size != int64(len(content)) {
		t.Fatalf("List = %+v", list)
	}

	srv := httptest.NewServer(s.Handler("/blob/"))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/blob/"+hash, nil)
	req.Header.Set("Range", "bytes=4-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "4567" {
		t.Fatalf("range: %d %q", resp.StatusCode, body)
	}

	for _, bad := range []string{"/blob/nothex", "/blob/" + strings.Repeat("0", 64), "/blob/../" + hash} {
		resp, err := http.Get(srv.URL + bad)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", bad, resp.StatusCode)
		}
	}
}

func TestBrowserListsAndEscapes(t *testing.T) {
	s, _ := New(t.TempDir())
	hash, _ := s.PutNamed(strings.NewReader("# notes"), "devpost.md")
	evil, _ := s.PutNamed(strings.NewReader("x"), "<script>alert(1)</script>.html")
	srv := httptest.NewServer(s.Browser())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(page)
	if !strings.Contains(body, "devpost.md") || !strings.Contains(body, "/blob/"+hash+"/devpost.md") {
		t.Fatalf("listing missing file: %s", body)
	}
	if strings.Contains(body, "<script>alert(1)") {
		t.Fatal("file name was not escaped")
	}

	// Named link: content type from the name, attachment on ?download.
	resp, err = http.Get(srv.URL + "/blob/" + hash + "/devpost.md?download")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename=devpost.md`) {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp != "sandbox" {
		t.Fatalf("blob CSP = %q", csp)
	}

	// Uploaded HTML is served sandboxed, never as a live page in this origin.
	resp, _ = http.Get(srv.URL + "/blob/" + evil)
	resp.Body.Close()
	if resp.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Fatal("uploaded HTML not sandboxed")
	}
}

func TestPutLimitedAndReceived(t *testing.T) {
	s, _ := New(t.TempDir())
	own, _ := s.PutNamed(strings.NewReader("my own file"), "own.txt")

	if _, err := s.PutLimited(strings.NewReader("0123456789"), "big.bin", "peer1", 9); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize: want ErrTooLarge, got %v", err)
	}
	// An oversize upload must leave nothing behind, not even a prefix.
	if list, _ := s.List(); len(list) != 1 {
		t.Fatalf("oversize upload left files: %+v", list)
	}

	got, err := s.PutLimited(strings.NewReader("from a peer"), "../../etc/passwd\x07", "peer1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Received(); n != int64(len("from a peer")) {
		t.Fatalf("Received = %d", n)
	}
	for _, e := range mustList(t, s) {
		if e.Hash == got && (e.From != "peer1" || e.Name != "passwd") {
			t.Fatalf("received entry: %+v", e)
		}
	}

	// A peer re-sending the owner's file doesn't turn it into a received one.
	if _, err := s.PutLimited(strings.NewReader("my own file"), "x", "peer1", 100); err != nil {
		t.Fatal(err)
	}
	for _, e := range mustList(t, s) {
		if e.Hash == own && (e.From != "" || e.Name != "own.txt") {
			t.Fatalf("owner file changed: %+v", e)
		}
	}

	// The shared-files page neither lists nor serves files peers sent.
	srv := httptest.NewServer(s.Browser())
	defer srv.Close()
	page := httpGet(t, srv.URL+"/")
	if !strings.Contains(page, "own.txt") || strings.Contains(page, "passwd") {
		t.Fatalf("page: %s", page)
	}
	if resp, _ := http.Get(srv.URL + "/blob/" + got); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("received file served on _files: %d", resp.StatusCode)
	}
}

func TestOldNamesFileStillReads(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	h, _ := s.Put(strings.NewReader("x"))
	os.WriteFile(filepath.Join(dir, namesFile), []byte(`{"`+h+`":"song.flac"}`), 0o600)
	if list := mustList(t, s); len(list) != 1 || list[0].Name != "song.flac" || list[0].From != "" {
		t.Fatalf("list = %+v", list)
	}
}

func mustList(t *testing.T, s *Store) []Entry {
	t.Helper()
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// The files page must be built from the panel's own base (tokens, buttons,
// row card), follow the visitor's light/dark setting, and keep strict headers:
// no script, and the one <style> block allowed by its hash only.
func TestFilesPageUsesPanelBase(t *testing.T) {
	s, _ := New(t.TempDir())
	s.PutNamed(strings.NewReader("x"), "a.txt")
	s.NodeName = func() string { return "my<node>" }
	srv := httptest.NewServer(s.Browser())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	if web.Base() == "" || !strings.Contains(body, web.Base()) {
		t.Fatal("page does not inline the panel's shared base")
	}
	if !strings.Contains(web.Base(), "button, .button") || !strings.Contains(web.Base(), ".rows") || !strings.Contains(web.Base(), `data-theme="system"`) {
		t.Fatal("shared base lost the buttons, row card or system theme")
	}
	if !strings.Contains(body, `data-theme="system"`) || !strings.Contains(body, `class="rows"`) || !strings.Contains(body, `class="brand"`) {
		t.Fatal("page does not use the panel's theme mechanism, row card or header")
	}
	for _, bad := range []string{"<script", "http://", "https://", "#e08a00", "style="} {
		if strings.Contains(body, bad) {
			t.Fatalf("page contains %q", bad)
		}
	}
	if strings.Contains(body, "my<node>") || !strings.Contains(body, "my&lt;node&gt;") {
		t.Fatal("node name is not escaped")
	}
	// The CSP must name the hash of the exact <style> text.
	i := strings.Index(body, "<style>") + len("<style>")
	j := strings.Index(body, "</style>")
	sum := sha256.Sum256([]byte(body[i:j]))
	want := "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	if csp := resp.Header.Get("Content-Security-Policy"); csp != want {
		t.Fatalf("CSP = %q, want %q", csp, want)
	}
	if strings.Contains(resp.Header.Get("Content-Security-Policy"), "unsafe-inline") {
		t.Fatal("CSP allows unsafe-inline")
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("security headers weakened")
	}
}

// The owner's panel.css reaches the page (and the CSP hash still matches);
// a stylesheet that tries to close the <style> element is ignored.
func TestFilesPageUserCSS(t *testing.T) {
	s, _ := New(t.TempDir())
	css := ":root { --accent: #123456; }"
	s.UserCSS = func() []byte { return []byte(css) }
	srv := httptest.NewServer(s.Browser())
	defer srv.Close()
	if page := httpGet(t, srv.URL+"/"); !strings.Contains(page, "#123456") {
		t.Fatal("panel.css not applied")
	}
	css = "</style><script>alert(1)</script>"
	if page := httpGet(t, srv.URL+"/"); strings.Contains(page, "<script") {
		t.Fatal("hostile panel.css broke out of the style block")
	}
}

func TestFilesPageEmptyState(t *testing.T) {
	s, _ := New(t.TempDir())
	srv := httptest.NewServer(s.Browser())
	defer srv.Close()
	if page := httpGet(t, srv.URL+"/"); !strings.Contains(page, "No files shared yet") {
		t.Fatal("no empty state")
	}
}

// Owner files are CRUD by name; the bytes go only with their last reference.
func TestRenameReplaceDeleteSharedHash(t *testing.T) {
	s, _ := New(t.TempDir())
	h, _ := s.Add(strings.NewReader("same bytes"), "a.txt", false)
	if h2, err := s.Add(strings.NewReader("same bytes"), "b.txt", false); err != nil || h2 != h {
		t.Fatalf("second name: %v", err)
	}
	if list := mustList(t, s); len(list) != 2 {
		t.Fatalf("want two names for one hash, got %+v", list)
	}
	if _, err := s.Add(strings.NewReader("other"), "a.txt", false); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("taken name: want ErrDuplicate, got %v", err)
	}
	if err := s.Rename("a.txt", "b.txt"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("rename onto a taken name: %v", err)
	}
	if err := s.Rename("a.txt", "c.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("nope", "d.txt"); !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("rename unknown: %v", err)
	}
	// Deleting one name keeps the bytes for the other.
	if err := s.Delete("c.txt"); err != nil {
		t.Fatal(err)
	}
	if !s.Has(h) {
		t.Fatal("blob removed while b.txt still refers to it")
	}
	if list := mustList(t, s); len(list) != 1 || list[0].Name != "b.txt" {
		t.Fatalf("after delete: %+v", list)
	}
	// Replacing moves the name to new bytes and drops the old ones.
	nh, err := s.Add(strings.NewReader("new version"), "b.txt", true)
	if err != nil {
		t.Fatal(err)
	}
	if s.Has(h) || !s.Has(nh) {
		t.Fatal("replace kept the old bytes or lost the new")
	}
	if list := mustList(t, s); len(list) != 1 || list[0].Hash != nh || list[0].Name != "b.txt" {
		t.Fatalf("after replace: %+v", list)
	}
	if err := s.Delete("b.txt"); err != nil || s.Has(nh) {
		t.Fatalf("delete last name: %v, still there: %v", err, s.Has(nh))
	}
	// An unnamed file is addressed by hash.
	u, _ := s.Put(strings.NewReader("unnamed"))
	if err := s.Rename(u, "named.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("named.txt"); err != nil || s.Has(u) {
		t.Fatalf("delete renamed unnamed: %v", err)
	}
	if list := mustList(t, s); len(list) != 0 {
		t.Fatalf("store not empty: %+v", list)
	}
}

// The same bytes in the inbox and on the shared page: deleting either one
// keeps the other, and the page never serves an inbox-only file.
func TestDeleteSharedWithInbox(t *testing.T) {
	s, _ := New(t.TempDir())
	h, _ := s.PutLimited(strings.NewReader("hello"), "from-peer.txt", "peer1", 100)
	if s.Shared(h) {
		t.Fatal("received file counts as shared")
	}
	if _, err := s.Add(strings.NewReader("hello"), "mine.txt", false); err != nil {
		t.Fatal(err)
	}
	if !s.Shared(h) || len(mustList(t, s)) != 2 {
		t.Fatalf("owner copy: %+v", mustList(t, s))
	}
	if err := s.Delete("mine.txt"); err != nil {
		t.Fatal(err)
	}
	if !s.Has(h) || s.Shared(h) {
		t.Fatal("deleting the owner's name removed the inbox file, or left it shared")
	}
	if list := mustList(t, s); len(list) != 1 || list[0].From != "peer1" {
		t.Fatalf("inbox entry lost: %+v", list)
	}
	if err := s.Delete(h); !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("Delete must not reach inbox files: %v", err)
	}
	if err := s.DeleteReceived(h); err != nil || s.Has(h) {
		t.Fatalf("DeleteReceived: %v, still there: %v", err, s.Has(h))
	}
	if err := s.DeleteReceived(h); !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("second DeleteReceived: %v", err)
	}
	// Inbox delete keeps bytes the owner also shares.
	h, _ = s.PutLimited(strings.NewReader("both"), "x", "peer1", 100)
	s.Add(strings.NewReader("both"), "both.txt", false)
	if err := s.DeleteReceived(h); err != nil || !s.Has(h) || !s.Shared(h) {
		t.Fatalf("inbox delete took the owner's file: %v", err)
	}
}

// A file the owner deletes disappears from the shared-files page at once.
func TestBrowserReflectsDelete(t *testing.T) {
	s, _ := New(t.TempDir())
	h, _ := s.Add(strings.NewReader("gone soon"), "gone.txt", false)
	srv := httptest.NewServer(s.Browser())
	defer srv.Close()
	if !strings.Contains(httpGet(t, srv.URL+"/"), "gone.txt") {
		t.Fatal("not listed")
	}
	s.Rename("gone.txt", "renamed.txt")
	if page := httpGet(t, srv.URL+"/"); !strings.Contains(page, "renamed.txt") || strings.Contains(page, "gone.txt") {
		t.Fatal("rename not reflected")
	}
	s.Delete("renamed.txt")
	if strings.Contains(httpGet(t, srv.URL+"/"), "renamed.txt") {
		t.Fatal("deleted file still listed")
	}
	if resp, _ := http.Get(srv.URL + "/blob/" + h); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted file still served: %d", resp.StatusCode)
	}
}
