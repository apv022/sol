package apphost

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apv022/sol/sdk/go/sol"
)

const certA = "aa00000000000000000000000000000000000000000000000000000000000001"
const certB = "bb00000000000000000000000000000000000000000000000000000000000002"

func extHost(t *testing.T, dir string) (*Host, *fakeTokens) {
	t.Helper()
	tok := &fakeTokens{m: map[string]string{}}
	h, err := New(Config{AppsDir: filepath.Join(dir, "apps"), DataDir: filepath.Join(dir, "data"), RunDir: filepath.Join(dir, "run"),
		Tokens: tok, Log: slog.New(slog.DiscardHandler),
		Builtins: map[string]Builtin{"echo": {Manifest: []byte(echoManifest)}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, tok
}

func granted(tok *fakeTokens, token string) string {
	tok.mu.Lock()
	defer tok.mu.Unlock()
	return tok.m[fmt.Sprintf("sha:%x", sha256.Sum256([]byte(token)))]
}

func TestExternalRegisterBindAndReload(t *testing.T) {
	dir := t.TempDir()
	h, tok := extHost(t, dir)
	m := &sol.Manifest{ID: "music", Name: "Music", Scopes: []string{"people", "blobs"}, Exec: "./evil"}
	got, token, err := h.RegisterExternal(m, "org.example.music", strings.ToUpper("AA:"+certA[2:]), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec != "external:android:org.example.music" || got.Version != "1" || len(token) != 64 {
		t.Fatalf("manifest %+v token %d", got, len(token))
	}
	if granted(tok, token) != "music" {
		t.Fatal("token not granted")
	}
	// Nothing of the token is on disk but its hash.
	raw, _ := os.ReadFile(filepath.Join(dir, "apps", "music", ExternalFile))
	if strings.Contains(string(raw), token) || !strings.Contains(string(raw), fmt.Sprintf("%x", sha256.Sum256([]byte(token)))) {
		t.Fatalf("binding %s", raw)
	}
	if st, _ := os.Stat(filepath.Join(dir, "apps", "music", ExternalFile)); runtime0600(st.Mode().Perm()) {
		t.Fatalf("binding mode %v", st.Mode())
	}
	if b, ok := h.External("music"); !ok || b.Cert != certA || b.Package != "org.example.music" {
		t.Fatalf("binding %+v", b)
	}
	// Refusals: a UI, folders, an unknown scope, the reserved id, a builtin's id, another
	// package or cert for the id, the same package under another cert.
	for _, c := range []struct {
		m         sol.Manifest
		pkg, cert string
	}{
		{sol.Manifest{ID: "x", UI: "/"}, "org.x.y", certA},
		{sol.Manifest{ID: "x", Folders: []sol.FolderAccess{{Path: "Music", Mode: "read"}}}, "org.x.y", certA},
		{sol.Manifest{ID: "x", Scopes: []string{"root"}}, "org.x.y", certA},
		{sol.Manifest{ID: "sol"}, "org.x.y", certA},
		{sol.Manifest{ID: "echo"}, "org.x.y", certA},
		{sol.Manifest{ID: "music"}, "org.other", certA},
		{sol.Manifest{ID: "music"}, "org.example.music", certB},
		{sol.Manifest{ID: "music2"}, "org.example.music", certB},
		{sol.Manifest{ID: "x"}, "not a package", certA},
		{sol.Manifest{ID: "x"}, "org.x.y", "short"},
		{sol.Manifest{ID: "x"}, "org.x.y", ""},
	} {
		if _, _, err := h.RegisterExternal(&c.m, c.pkg, c.cert, nil); err == nil {
			t.Errorf("%+v %s %s: accepted", c.m, c.pkg, c.cert)
		}
	}
	if err := h.Restart("music"); err == nil {
		t.Error("restart of an external app")
	}
	// Re-approval of the same binding: a new token, the old one revoked.
	_, token2, err := h.RegisterExternal(&sol.Manifest{ID: "music", Name: "Music"}, "org.example.music", certA, nil)
	if err != nil || granted(tok, token) != "" || granted(tok, token2) != "music" {
		t.Fatalf("re-register: %v old=%q new=%q", err, granted(tok, token), granted(tok, token2))
	}
	h.Close()

	// A restart grants the stored hash again; remove revokes it and deletes the binding.
	h2, tok2 := extHost(t, dir)
	if granted(tok2, token2) != "music" {
		t.Fatal("token lost on restart")
	}
	var found bool
	for _, s := range h2.Status() {
		if s.ID == "music" {
			found = !s.Running && s.Exec == "external:android:org.example.music"
		}
	}
	if !found {
		t.Fatalf("status %+v", h2.Status())
	}
	if err := h2.Remove("music"); err != nil {
		t.Fatal(err)
	}
	if granted(tok2, token2) != "" {
		t.Fatal("token survives remove")
	}
	if _, err := os.Stat(filepath.Join(dir, "apps", "music")); !os.IsNotExist(err) {
		t.Fatal("app dir survives remove")
	}
	if _, err := os.Stat(RemovedMarker(filepath.Join(dir, "apps"), "music")); !os.IsNotExist(err) {
		t.Fatal("removed marker for an external app")
	}
}

// runtime0600 reports whether a secret file's mode is wrong (Windows has no Unix modes).
func runtime0600(m os.FileMode) bool { return m != 0o600 && os.PathSeparator == '/' }

// testPNG is a w×h PNG.
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExternalIcon: an external app's icon is validated (PNG, size, dimensions), stored as
// icon.png beside the binding, replaced or removed by the next registration and gone with the app.
func TestExternalIcon(t *testing.T) {
	big := append(testPNG(t, 8, 8), make([]byte, MaxExternalIconBytes)...)
	for name, b := range map[string][]byte{
		"not a png":  []byte("GIF89a......"),
		"bad header": append(append([]byte{}, pngMagic...), "garbage"...),
		"too wide":   testPNG(t, 513, 16),
		"too big":    big,
	} {
		if err := ValidateIconPNG(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	dir := t.TempDir()
	h, _ := extHost(t, dir)
	if _, _, err := h.RegisterExternal(&sol.Manifest{ID: "music", Name: "Music"}, "org.example.music", certA, []byte("nope")); err == nil {
		t.Fatal("bad icon accepted")
	}
	if _, ok := h.External("music"); ok {
		t.Fatal("registered despite a bad icon")
	}
	icon := testPNG(t, 192, 192)
	if _, _, err := h.RegisterExternal(&sol.Manifest{ID: "music", Name: "Music"}, "org.example.music", certA, icon); err != nil {
		t.Fatal(err)
	}
	p, _, ok := h.ExternalIcon("music")
	if b, _ := os.ReadFile(p); !ok || !bytes.Equal(b, icon) || filepath.Base(p) != ExternalIconFile {
		t.Fatalf("icon %q %v", p, ok)
	}
	if _, _, ok := h.ExternalIcon("echo"); ok {
		t.Fatal("icon for an app that is not external")
	}
	icon2 := testPNG(t, 96, 96)
	if _, _, err := h.RegisterExternal(&sol.Manifest{ID: "music", Name: "Music"}, "org.example.music", certA, icon2); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, icon2) {
		t.Fatal("re-connect kept the old icon")
	}
	if _, _, err := h.RegisterExternal(&sol.Manifest{ID: "music", Name: "Music"}, "org.example.music", certA, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := h.ExternalIcon("music"); ok {
		t.Fatal("re-connect without an icon kept the old one")
	}
	if _, _, err := h.RegisterExternal(&sol.Manifest{ID: "music", Name: "Music"}, "org.example.music", certA, icon); err != nil {
		t.Fatal(err)
	}
	if err := h.Remove("music"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("icon survives remove")
	}
}
