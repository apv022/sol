package channels

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
)

type entry struct {
	name string
	typ  byte
	body string
	link string
}

func mkTar(t *testing.T, gz bool, es ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w *tar.Writer
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = tar.NewWriter(zw)
	} else {
		w = tar.NewWriter(&buf)
	}
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o755, Size: int64(len(e.body)), Linkname: e.link}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			w.Write([]byte(e.body))
		}
	}
	w.Close()
	if zw != nil {
		zw.Close()
	}
	return buf.Bytes()
}

func TestUnpack(t *testing.T) {
	base := t.TempDir()
	good := mkTar(t, false, entry{name: "app.json", typ: tar.TypeReg, body: "{}"}, entry{name: "ui/", typ: tar.TypeDir},
		entry{name: "ui/index.html", typ: tar.TypeReg, body: "<p>"}, entry{name: "run", typ: tar.TypeReg, body: "#!/bin/sh"})
	root, err := Unpack(bytes.NewReader(good), filepath.Join(base, "a"))
	if err != nil || root != filepath.Join(base, "a") {
		t.Fatal(root, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "ui", "index.html")); string(b) != "<p>" {
		t.Fatalf("ui: %q", b)
	}
	// One top directory, gzip.
	sub := mkTar(t, true, entry{name: "myapp/app.json", typ: tar.TypeReg, body: "{}"})
	root, err = Unpack(bytes.NewReader(sub), filepath.Join(base, "b"))
	if err != nil || root != filepath.Join(base, "b", "myapp") {
		t.Fatal(root, err)
	}

	for name, es := range map[string][]entry{
		"dotdot":    {{name: "../evil", typ: tar.TypeReg, body: "x"}},
		"deep":      {{name: "ui/../../evil", typ: tar.TypeReg, body: "x"}},
		"absolute":  {{name: "/tmp/evil", typ: tar.TypeReg, body: "x"}},
		"backslash": {{name: `..\evil`, typ: tar.TypeReg, body: "x"}},
		"symlink":   {{name: "app.json", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"hardlink":  {{name: "app.json", typ: tar.TypeLink, link: "x"}},
		"twice":     {{name: "app.json", typ: tar.TypeReg, body: "{}"}, {name: "app.json", typ: tar.TypeReg, body: "{}"}},
	} {
		dir := filepath.Join(base, "bad-"+name)
		if _, err := Unpack(bytes.NewReader(mkTar(t, false, es...)), dir); !errors.Is(err, ErrUnsafeArchive) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("%s: the staging directory stayed", name)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "evil")); err == nil {
		t.Fatal("a file was written outside the archive's directory")
	}
	if _, err := Unpack(bytes.NewReader(mkTar(t, false, entry{name: "x", typ: tar.TypeReg})), filepath.Join(base, "c")); err == nil {
		t.Fatal("no app.json accepted")
	}
}

func TestBlobsAndServer(t *testing.T) {
	b, err := OpenBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("the artifact")
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	if _, _, err := b.Put(bytes.NewReader(data), 100, strings.Repeat("0", 64), int64(len(data))); !errors.Is(err, ErrTampered) {
		t.Fatalf("wrong hash: %v", err)
	}
	if got, n, err := b.Put(bytes.NewReader(data), 100, "", 0); err != nil || got != h || n != int64(len(data)) {
		t.Fatal(got, n, err)
	}
	if st, _ := os.Stat(b.Path(h)); st.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Fatalf("mode %v", st.Mode())
	}

	k, _ := NewKey("demo")
	r := identity.Release{Seq: 1, App: "demo", Version: "1.0", Artifacts: []identity.Artifact{{Platform: "linux-amd64", Kind: identity.KindSolApp, SHA256: sum[:], Size: uint64(len(data))}}}
	if err := r.Sign(k.Private()); err != nil {
		t.Fatal(err)
	}
	s := NewServer(k.ID(), b, []identity.Release{r})
	srv := httptest.NewServer(s)
	defer srv.Close()
	defer s.Close()

	got := make(chan identity.Release, 4)
	ready := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Stream(ctx, srv.Client(), srv.URL, func() { ready <- struct{}{} }, func(raw []byte) {
		if rr, err := identity.ParseRelease(raw); err == nil && rr.VerifyFrom(k.Public()) == nil {
			got <- rr
		}
	})
	if rr := <-got; rr.Version != "1.0" {
		t.Fatalf("first: %+v", rr)
	}
	<-ready
	r2 := r
	r2.Seq, r2.Version = 2, "1.1"
	r2.Sign(k.Private())
	s.Publish(r2)
	select {
	case rr := <-got:
		if rr.Version != "1.1" {
			t.Fatalf("pushed: %+v", rr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the new record was not pushed")
	}

	// A follower downloads with the hash checked.
	f, _ := OpenBlobs(t.TempDir())
	if err := f.Fetch(ctx, srv.Client(), srv.URL+"/blob/"+h, h, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(b.Path(h), []byte("the artifacT"), 0o600) // tampered at the source
	g, _ := OpenBlobs(t.TempDir())
	if err := g.Fetch(ctx, srv.Client(), srv.URL+"/blob/"+h, h, int64(len(data))); !errors.Is(err, ErrTampered) {
		t.Fatalf("tampered: %v", err)
	}
	if g.Has(h) {
		t.Fatal("kept tampered bytes")
	}
	resp, _ := http.Get(srv.URL + "/blob/" + strings.Repeat("1", 64))
	if resp.StatusCode != 404 {
		t.Fatal(resp.Status)
	}
}

func TestLinks(t *testing.T) {
	k, _ := NewKey("demo")
	onion := strings.Repeat("a", 56) + ".onion"
	pub, o, err := ParseLink(FormatLink(k.Public(), onion))
	if err != nil || !bytes.Equal(pub, k.Public()) || o != onion {
		t.Fatal(err)
	}
	for _, bad := range []string{"sol://pair?x", LinkScheme + "?k=abc&o=" + onion, LinkScheme + "?k=" + k.ID(), LinkScheme + "?k=" + k.ID() + "&o=example.com"} {
		if _, _, err := ParseLink(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := NewKey("Bad Name"); err == nil {
		t.Fatal("bad name")
	}
	p := filepath.Join(t.TempDir(), "demo.key")
	if err := k.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := k.Save(p); err == nil {
		t.Fatal("overwrote a channel key")
	}
	if l, err := LoadKey(p); err != nil || l.ID() != k.ID() {
		t.Fatal(err)
	}
}
