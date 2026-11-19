package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func withBundle(t *testing.T, files fstest.MapFS) {
	t.Helper()
	old := bundledFS
	bundledFS = files
	t.Cleanup(func() { bundledFS = old })
}

func TestExtractBundledVerifiesChecksum(t *testing.T) {
	bin := []byte("#!/bin/sh\necho fake tor\n")
	sum := sha256.Sum256(bin)
	key := platformPath("tor")

	withBundle(t, fstest.MapFS{
		"bin/" + key:         {Data: bin},
		"bin/checksums.json": {Data: []byte(`{"` + key + `":"` + hex.EncodeToString(sum[:]) + `"}`)},
	})
	b := &Bundled{BinDir: t.TempDir()}
	if !b.Available("tor") || b.Available("i2pd") {
		t.Fatal("Available wrong")
	}
	path, err := ExtractBundled("tor", b.BinDir)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(bin) {
		t.Fatal("extracted content differs")
	}

	withBundle(t, fstest.MapFS{
		"bin/" + key:         {Data: []byte("tampered")},
		"bin/checksums.json": {Data: []byte(`{"` + key + `":"` + hex.EncodeToString(sum[:]) + `"}`)},
	})
	if _, err := ExtractBundled("tor", t.TempDir()); err == nil {
		t.Fatal("tampered binary was extracted")
	}

	withBundle(t, fstest.MapFS{"bin/" + key: {Data: bin}, "bin/checksums.json": {Data: []byte(`{}`)}})
	if _, err := ExtractBundled("tor", t.TempDir()); err == nil {
		t.Fatal("unpinned binary was extracted")
	}
}

func TestSelectFallsThrough(t *testing.T) {
	withBundle(t, nil)
	if _, err := Select([]string{"bundled", "docker"}, "definitely-not-a-program", Options{}); err == nil {
		t.Fatal("Select found a provider for a missing program")
	}
	if _, err := Select([]string{"nope"}, "tor", Options{}); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

// SOL_BIN_<NAME> names a program's file directly (Android has no PATH for
// app binaries); a path that isn't a file is an error, not a silent
// fall-back to PATH.
func TestNativeEnvOverride(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing on PATH
	n := &Native{}
	if n.Available("snowflake-proxy") {
		t.Fatal("found a program on an empty PATH")
	}
	bin := filepath.Join(t.TempDir(), "libsnow.so")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOL_BIN_SNOWFLAKE_PROXY", bin)
	if got, err := nativePath("snowflake-proxy"); err != nil || got != bin {
		t.Fatalf("nativePath = %q, %v", got, err)
	}
	t.Setenv("SOL_BIN_SNOWFLAKE_PROXY", filepath.Join(t.TempDir(), "missing"))
	if n.Available("snowflake-proxy") {
		t.Fatal("a missing SOL_BIN file counted as available")
	}
}
