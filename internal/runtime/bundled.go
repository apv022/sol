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
	"log/slog"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// bundledFS is set by the bundled_embed_*.go files in builds with -tags bundled: bin/
// checksums.json and bin/<goos>-<goarch>/ (tor and the shared libraries it loads), filled by
// scripts/fetch-tor.sh. In normal builds it is nil and the bundled provider is unavailable.
var bundledFS fs.FS

// Bundled runs programs embedded in the sol executable at build time (tor 0.4.9.16 from the
// Tor Browser expert bundles, design/PLAN.md C10).
//
// Start extracts the program and every file next to it (Linux: libcrypto, libssl,
// libevent; macOS: libevent) into BinDir/<goos>-<goarch>/, refusing any file whose SHA-256 is
// not pinned in checksums.json, and runs it with that directory on the library path
// (LD_LIBRARY_PATH; DYLD_LIBRARY_PATH on macOS, where tor also finds its dylib through
// @executable_path; Windows looks next to the .exe by itself). If extraction or start fails
// and Fallback is set (Select sets it to the next provider that can run the program, the
// system's tor), that runs instead, and the reason is logged.
type Bundled struct {
	BinDir   string // extraction target, normally <data dir>/bin
	Fallback Provider
}

// Name implements Provider.
func (b *Bundled) Name() string { return "bundled" }

// platform is this build's folder in bin/.
func platform() string { return goruntime.GOOS + "-" + goruntime.GOARCH }

// exeName is program's file name on this OS.
func exeName(program string) string {
	if goruntime.GOOS == "windows" {
		return program + ".exe"
	}
	return program
}

func platformPath(program string) string { return platform() + "/" + exeName(program) }

// Available reports whether this build embeds program for this OS/arch.
func (b *Bundled) Available(program string) bool {
	if bundledFS == nil || b.BinDir == "" {
		return false
	}
	_, err := fs.Stat(bundledFS, "bin/"+platformPath(program))
	return err == nil
}

// Start extracts and verifies the program and its libraries, then runs it; on failure it
// runs Fallback instead, if there is one.
func (b *Bundled) Start(ctx context.Context, p Program) (*Proc, error) {
	proc, err := b.start(ctx, p)
	if err == nil || b.Fallback == nil || !b.Fallback.Available(p.Name) {
		return proc, err
	}
	slog.Warn("the bundled program could not run; using the system's", "program", p.Name, "fallback", b.Fallback.Name(), "err", err)
	return b.Fallback.Start(ctx, p)
}

func (b *Bundled) start(ctx context.Context, p Program) (*Proc, error) {
	exe, err := ExtractBundled(p.Name, b.BinDir)
	if err != nil {
		return nil, err
	}
	return startBinaryEnv(ctx, exe, p, LibraryEnv(filepath.Dir(exe)))
}

// LibraryEnv is the environment that makes a program load its shared libraries from dir:
// LD_LIBRARY_PATH on Linux and other Unix systems, DYLD_LIBRARY_PATH on macOS (dir first,
// then what was already set), nothing on Windows (it searches the program's own folder).
func LibraryEnv(dir string) []string {
	key := "LD_LIBRARY_PATH"
	switch goruntime.GOOS {
	case "windows":
		return nil
	case "darwin", "ios":
		key = "DYLD_LIBRARY_PATH"
	}
	v := dir
	if old := os.Getenv(key); old != "" {
		v += string(os.PathListSeparator) + old
	}
	return []string{key + "=" + v}
}

// ExtractBundled writes the embedded program and every file next to it to
// destDir/<goos>-<goarch>/ (0755, atomically, only where the copy there differs), after
// checking each against its pinned checksum, and returns the program's path. A file without a
// pinned checksum, or that doesn't match it, is refused and nothing is run.
func ExtractBundled(program, destDir string) (string, error) {
	if bundledFS == nil {
		return "", errors.New("this build has no bundled binaries (build with -tags bundled)")
	}
	sums, err := pinnedChecksums()
	if err != nil {
		return "", err
	}
	dir := "bin/" + platform()
	if _, err := fs.Stat(bundledFS, "bin/"+platformPath(program)); err != nil {
		return "", fmt.Errorf("this build bundles no %s for %s", program, platform())
	}
	ents, err := fs.ReadDir(bundledFS, dir)
	if err != nil {
		return "", err
	}
	type file struct {
		name string
		data []byte
	}
	var files []file
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		key := platform() + "/" + e.Name()
		want, ok := sums[key]
		if !ok {
			return "", fmt.Errorf("no pinned checksum for %s; refusing to run it", key)
		}
		data, err := fs.ReadFile(bundledFS, path.Join(dir, e.Name()))
		if err != nil {
			return "", err
		}
		if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
			return "", fmt.Errorf("embedded %s does not match its pinned checksum; refusing to run it", key)
		}
		files = append(files, file{e.Name(), data})
	}
	out := filepath.Join(destDir, platform())
	if err := os.MkdirAll(out, 0o700); err != nil {
		return "", err
	}
	for _, f := range files {
		dest := filepath.Join(out, f.name)
		if existing, err := os.ReadFile(dest); err == nil && bytes.Equal(existing, f.data) {
			continue
		}
		tmp := dest + ".tmp"
		if err := os.WriteFile(tmp, f.data, 0o755); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, dest); err != nil {
			return "", err
		}
	}
	return filepath.Join(out, exeName(program)), nil
}

// VerifyChecksum checks the file at path against the pinned checksum for key
// ("<os>-<arch>/<name>").
func VerifyChecksum(path, key string) error {
	sums, err := pinnedChecksums()
	if err != nil {
		return err
	}
	want, ok := sums[key]
	if !ok {
		return fmt.Errorf("no pinned checksum for %s; refusing to run it", key)
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

// pinnedChecksums reads bin/checksums.json: "<os>-<arch>/<file>" -> SHA-256 hex.
func pinnedChecksums() (map[string]string, error) {
	if bundledFS == nil {
		return nil, errors.New("this build has no bundled binaries")
	}
	raw, err := fs.ReadFile(bundledFS, "bin/checksums.json")
	if err != nil {
		return nil, fmt.Errorf("reading checksums.json: %w", err)
	}
	var sums map[string]string
	if err := json.Unmarshal(raw, &sums); err != nil {
		return nil, fmt.Errorf("parsing checksums.json: %w", err)
	}
	for k, v := range sums {
		sums[k] = strings.ToLower(v)
	}
	return sums, nil
}
