package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apv022/sol/internal/channels"
	"github.com/apv022/sol/internal/identity"
)

// TestChannelNewAndOfflinePublish: `sol channel new` writes a 0600 key that is never
// overwritten, and `sol channel publish --out` signs a record without a daemon (CI), which
// verifies under that key and names the artifact's hash.
func TestChannelNewAndOfflinePublish(t *testing.T) {
	dir := t.TempDir()
	if err := run([]string{"channel", "new", "demo", "--dir", dir}); err != nil {
		t.Fatal(err)
	}
	kp := filepath.Join(dir, "channel-keys", "demo.key")
	st, err := os.Stat(kp)
	if err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator == '/' && st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode())
	}
	if err := run([]string{"channel", "new", "demo", "--dir", dir}); err == nil {
		t.Fatal("overwrote the key")
	}
	art := filepath.Join(dir, "demo.tar")
	os.WriteFile(art, []byte("tar bytes"), 0o600)
	rec := filepath.Join(dir, "demo.rec")
	if err := run([]string{"channel", "publish", "--dir", dir, "--key", "demo", "--app", "demo", "--version", "1.0.0",
		"--artifact", "linux-amd64=" + art, "--mirror", "linux-amd64=https://example.org/demo.tar", "--out", rec}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(rec)
	r, err := identity.ParseRelease(raw)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := channels.LoadKey(kp)
	if err := r.VerifyFrom(k.Public()); err != nil || r.Artifacts[0].Size != 9 || r.Artifacts[0].Mirrors[0] != "https://example.org/demo.tar" {
		t.Fatalf("%v %+v", err, r)
	}
	if err := run([]string{"channel", "publish", "--dir", dir, "--key", "demo", "--app", "demo", "--version", "1.0.0",
		"--artifact", "linux-amd64=" + art, "--mirror", "darwin-arm64=https://example.org/x", "--out", rec}); err == nil {
		t.Fatal("a mirror for a platform without an artifact")
	}
}
