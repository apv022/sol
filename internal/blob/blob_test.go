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

func TestBrowserServesSandboxed(t *testing.T) {
	s, _ := New(t.TempDir())
	hash, _ := s.PutNamed(strings.NewReader("# notes"), "devpost.md")
	evil, _ := s.PutNamed(strings.NewReader("x"), "<script>alert(1)</script>.html")
	srv := httptest.NewServer(s.Browser())
	defer srv.Close()

	var err error
	var resp *http.Response
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

	// The shared-files handler does not serve files peers sent.
	srv := httptest.NewServer(s.Browser())
	defer srv.Close()
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
	if err := s.Rename("nope", "d.txt"); !errors.Is(err, ErrNotFound) {
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
	if err := s.Delete(h); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete must not reach inbox files: %v", err)
	}
	if err := s.DeleteReceived(h); err != nil || s.Has(h) {
		t.Fatalf("DeleteReceived: %v, still there: %v", err, s.Has(h))
	}
	if err := s.DeleteReceived(h); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second DeleteReceived: %v", err)
	}
	// Inbox delete keeps bytes the owner also shares.
	h, _ = s.PutLimited(strings.NewReader("both"), "x", "peer1", 100)
	s.Add(strings.NewReader("both"), "both.txt", false)
	if err := s.DeleteReceived(h); err != nil || !s.Has(h) || !s.Shared(h) {
		t.Fatalf("inbox delete took the owner's file: %v", err)
	}
}
