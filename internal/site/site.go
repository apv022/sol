// Package site serves a folder as a static website, for `sol site add`.
//
// The handler is built for a site that may be published to strangers, so it
// gives away as little as it can about the machine it runs on:
//
//   - Nothing outside the folder is reachable. The folder is opened as an
//     os.Root, so "..", absolute paths and symlinks that lead out of it fail.
//   - Hidden files (any path element starting with a dot: .git, .env,
//     .htpasswd) are never served.
//   - A directory without an index.html is "not found": no file listings.
//   - Responses carry no Last-Modified header. File times show when, and in
//     which time zone, the owner works on the site.
//   - Only GET and HEAD are answered.
//
// Every failure is a plain 404, so a visitor can't tell a missing file from a
// hidden or unreadable one.
package site

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// cacheControl lets browsers reuse a page for a few minutes. Without
// Last-Modified there is nothing to revalidate against, and refetching every
// asset over Tor on each click would be slow.
const cacheControl = "max-age=300"

// Handler serves the files under dir. Close the returned io.Closer when the
// site is removed. It fails if dir is not a readable directory.
func Handler(dir string) (http.Handler, io.Closer, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	files := http.FileServerFS(guarded{root.FS()})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", cacheControl)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	})
	return h, root, nil
}

// guarded hides dotfiles and directories without an index page, and turns
// every error into "not found".
type guarded struct{ fs fs.FS }

func (g guarded) Open(name string) (fs.File, error) {
	notFound := &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	for _, el := range strings.Split(name, "/") {
		if strings.HasPrefix(el, ".") && el != "." {
			return nil, notFound
		}
	}
	f, err := g.fs.Open(name)
	if err != nil {
		return nil, notFound
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, notFound
	}
	if fi.IsDir() {
		idx, err := g.fs.Open(path.Join(name, "index.html"))
		if err != nil {
			f.Close()
			return nil, notFound
		}
		idx.Close()
	}
	return file{f}, nil
}

// file forwards Seek (net/http needs it for ranges and type sniffing) and
// hides the modification time.
type file struct{ fs.File }

func (f file) Stat() (fs.FileInfo, error) {
	fi, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	return timeless{fi}, nil
}

func (f file) Seek(offset int64, whence int) (int64, error) {
	s, ok := f.File.(io.Seeker)
	if !ok {
		return 0, errors.ErrUnsupported
	}
	return s.Seek(offset, whence)
}

// timeless reports a zero modification time, which net/http leaves out of
// the response.
type timeless struct{ fs.FileInfo }

func (timeless) ModTime() time.Time { return time.Time{} }
