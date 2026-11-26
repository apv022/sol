package channels

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits of an unpacked sol-app tar: a Sol app is an executable, a manifest and a UI, so these
// are generous, and they stop a small archive from filling the disk.
const (
	MaxUnpackedSize = 1 << 30
	MaxEntries      = 20000
)

// ErrUnsafeArchive is returned for a tar that tries to write outside its directory or holds
// something other than regular files and directories.
var ErrUnsafeArchive = errors.New("channels: unsafe archive")

// Unpack extracts a sol-app tar (plain or gzip-compressed) into dir, which must not exist yet.
// Only regular files and directories are written; a name that is absolute, has "..", or a
// backslash, a symlink, a hard link or a device fails the whole unpack (ErrUnsafeArchive), and
// dir is removed. Files are 0600 (0700 when executable), directories 0700.
//
// When the archive holds a single top-level directory and no app.json at its root, that
// directory is the app (tar -czf app.tar.gz myapp/); the returned path is the app directory.
func Unpack(r io.Reader, dir string) (string, error) {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	root, err := unpack(r, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return root, nil
}

func unsafe(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsafeArchive, fmt.Sprintf(format, a...))
}

func unpack(r io.Reader, dir string) (string, error) {
	br := bufio.NewReader(r)
	var in io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return "", err
		}
		defer zr.Close()
		in = zr
	}
	tr := tar.NewReader(in)
	var total int64
	tops := map[string]bool{}
	rootManifest := false
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if n >= MaxEntries {
			return "", unsafe("more than %d entries", MaxEntries)
		}
		name := h.Name
		if strings.Contains(name, `\`) || strings.HasPrefix(name, "/") || path.IsAbs(name) {
			return "", unsafe("%q is not a relative path", name)
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return "", unsafe("%q leaves the archive", name)
			}
		}
		clean := path.Clean(name)
		if clean == "." {
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(clean)) {
			return "", unsafe("%q leaves the archive", name)
		}
		top, _, _ := strings.Cut(clean, "/")
		tops[top] = true
		if clean == "app.json" {
			rootManifest = true
		}
		dst := filepath.Join(dir, filepath.FromSlash(clean))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if h.Size < 0 || total+h.Size > MaxUnpackedSize {
				return "", unsafe("larger than %d MiB unpacked", MaxUnpackedSize>>20)
			}
			total += h.Size
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return "", err
			}
			mode := os.FileMode(0o600)
			if h.Mode&0o100 != 0 {
				mode = 0o700
			}
			// O_EXCL: a name twice in one archive is refused rather than overwritten.
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return "", unsafe("%q: %v", name, err)
			}
			_, err = io.CopyN(f, tr, h.Size)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return "", err
			}
		case tar.TypeXGlobalHeader:
			continue
		default:
			return "", unsafe("%q is not a regular file or directory", name)
		}
	}
	if rootManifest {
		return dir, nil
	}
	if len(tops) == 1 {
		for top := range tops {
			sub := filepath.Join(dir, top)
			if st, err := os.Stat(filepath.Join(sub, "app.json")); err == nil && st.Mode().IsRegular() {
				return sub, nil
			}
		}
	}
	return "", errors.New("channels: the archive has no app.json")
}
