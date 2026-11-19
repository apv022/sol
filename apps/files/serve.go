package files

import (
	"context"
	"embed"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
	"github.com/apv022/sol/sdk/js"
	"github.com/apv022/sol/ui/kit"
)

//go:embed ui
var uiFS embed.FS

func (a *App) buildHandler() http.Handler {
	mux := http.NewServeMux()
	act := sol.NewActions(a.manifest)
	a.registerActions(act)
	if miss := act.Missing(); len(miss) > 0 {
		panic("files: actions without a handler: " + strings.Join(miss, ", "))
	}
	mux.Handle("POST /_sol/actions/", act)
	mux.HandleFunc("GET /raw", a.serveRaw)
	mux.Handle("GET /kit/", http.StripPrefix("/kit/", http.FileServerFS(kit.FS)))
	mux.HandleFunc("GET /sol.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		b, _ := js.FS.ReadFile("sol.js")
		_, _ = w.Write(b)
	})
	ui, _ := fs.Sub(uiFS, "ui")
	mux.Handle("GET /", http.FileServerFS(ui))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

// blobReader reads a blob by hash through the API, so playback and downloads work for files
// that are not on disk: Range requests become blob ranges and nothing is written locally.
type blobReader struct {
	ctx  context.Context
	api  sol.API
	hash string
	from string
	size int64
	off  int64
	cur  io.ReadCloser
}

func (b *blobReader) Read(p []byte) (int, error) {
	if b.off >= b.size {
		return 0, io.EOF
	}
	if b.cur == nil {
		rc, err := b.api.GetBlob(b.ctx, b.hash, sol.BlobOptions{From: b.from, Offset: b.off})
		if err != nil {
			return 0, err
		}
		b.cur = rc
	}
	n, err := b.cur.Read(p)
	b.off += int64(n)
	return n, err
}

func (b *blobReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += b.off
	case io.SeekEnd:
		off += b.size
	}
	if off < 0 {
		return 0, errors.New("negative position")
	}
	if off != b.off && b.cur != nil {
		_ = b.cur.Close()
		b.cur = nil
	}
	b.off = off
	return off, nil
}

func (b *blobReader) Close() error {
	if b.cur != nil {
		return b.cur.Close()
	}
	return nil
}

// serveRaw streams one file by path with Range support: GET /raw?path=Music/a.mp3[&download=1].
// Content comes from the blob (never from the folder), so it works for online-only files.
func (a *App) serveRaw(w http.ResponseWriter, r *http.Request) {
	if !sol.CallerOf(r).Self {
		sol.WriteError(w, sol.Errorf("forbidden", "for my own devices only"))
		return
	}
	p, err := CleanPath(r.URL.Query().Get("path"), false)
	if err != nil {
		sol.WriteError(w, bad("%v", err))
		return
	}
	v := a.getView()
	f := v.files[p]
	if f == nil {
		if _, perr := a.pullAll(r.Context()); perr == nil {
			f = a.getView().files[p]
		}
	}
	if f == nil {
		sol.WriteError(w, sol.Errorf("not_found", "no such file"))
		return
	}
	if _, err := a.api.StatBlob(r.Context(), f.Blob, sol.BlobOptions{From: f.From}); err != nil {
		sol.WriteError(w, err)
		return
	}
	br := &blobReader{ctx: r.Context(), api: a.api, hash: f.Blob, from: f.From, size: f.Size}
	defer br.Close()
	name := baseOf(p)
	ct := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	switch {
	case ct == "":
		ct = "application/octet-stream"
	case strings.HasPrefix(ct, "text/html"), strings.HasPrefix(ct, "image/svg"), strings.Contains(ct, "xml"), strings.HasPrefix(ct, "text/javascript"):
		ct = "text/plain; charset=utf-8" // never let a file run script on the app's origin
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Etag", `"`+f.Blob+`"`)
	disp := "inline"
	if r.URL.Query().Get("download") == "1" {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", disp+"; filename*=UTF-8''"+urlEscape(name))
	http.ServeContent(w, r, name, time.UnixMilli(f.Mtime), br)
}

func urlEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(strconv.FormatInt(int64(c)+0x100, 16)[1:]))
		}
	}
	return b.String()
}
