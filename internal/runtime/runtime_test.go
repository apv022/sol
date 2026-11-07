package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
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
