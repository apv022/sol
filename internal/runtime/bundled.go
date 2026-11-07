package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
)

// bundledFS is set by bundled_embed.go in builds with -tags bundled.
// In normal builds it is nil and the bundled provider is unavailable.
var bundledFS fs.FS

// Bundled runs binaries embedded in the sol executable at build time.
type Bundled struct {
	BinDir string // extraction target, normally ~/.sol/bin
}

func (b *Bundled) Name() string { return "bundled" }

func platformPath(program string) string {
	name := program
	if goruntime.GOOS == "windows" {
		name += ".exe"
	}
	return goruntime.GOOS + "-" + goruntime.GOARCH + "/" + name
}

// Available reports whether this build embeds program for this OS/arch.
func (b *Bundled) Available(program string) bool {
	if bundledFS == nil || b.BinDir == "" {
		return false
	}
	_, err := fs.Stat(bundledFS, "bin/"+platformPath(program))
	return err == nil
}

// Start extracts and verifies the binary, then runs it.
func (b *Bundled) Start(ctx context.Context, p Program) (*Proc, error) {
	path, err := ExtractBundled(p.Name, b.BinDir)
	if err != nil {
		return nil, err
	}
	return startBinary(ctx, path, p)
}

// ExtractBundled writes the embedded binary to destDir (0755) unless an
// identical copy is already there, and verifies its pinned checksum.
func ExtractBundled(program, destDir string) (string, error) {
	if bundledFS == nil {
		return "", errors.New("this build has no bundled binaries (build with -tags bundled)")
	}
	rel := platformPath(program)
	want, err := pinnedChecksum(rel)
	if err != nil {
		return "", err
	}
	data, err := fs.ReadFile(bundledFS, "bin/"+rel)
	if err != nil {
		return "", err
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
		return "", fmt.Errorf("embedded %s does not match its pinned checksum", rel)
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return "", err
	}
	dest := filepath.Join(destDir, filepath.Base(rel))
	if existing, err := os.ReadFile(dest); err == nil && bytes.Equal(existing, data) {
		return dest, nil
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return "", err
	}
	return dest, os.Rename(tmp, dest)
}

// VerifyChecksum checks the file at path against the pinned checksum for key
// ("<os>-<arch>/<name>").
func VerifyChecksum(path, key string) error {
	want, err := pinnedChecksum(key)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s: checksum mismatch", path)
	}
	return nil
}

func pinnedChecksum(key string) (string, error) {
	raw, err := fs.ReadFile(bundledFS, "bin/checksums.json")
	if err != nil {
		return "", fmt.Errorf("reading checksums.json: %w", err)
	}
	var sums map[string]string
	if err := json.Unmarshal(raw, &sums); err != nil {
		return "", fmt.Errorf("parsing checksums.json: %w", err)
	}
	want, ok := sums[key]
	if !ok {
		return "", fmt.Errorf("no pinned checksum for %s; refusing to run it", key)
	}
	return want, nil
}
