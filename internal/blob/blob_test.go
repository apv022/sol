package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
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
