package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
