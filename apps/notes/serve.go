package notes

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"

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
		panic("notes: actions without a handler: " + strings.Join(miss, ", "))
	}
	mux.Handle("POST /_sol/actions/", act)
	mux.HandleFunc("GET /file", a.serveFile)
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

// serveFile serves a file from Notes/ (an attachment an image tag points at): GET
// /file?path=Folder/attachments/a.png. Anything that could run script on the app's origin is
// served as plain text.
func (a *App) serveFile(w http.ResponseWriter, r *http.Request) {
	if !sol.CallerOf(r).Self {
		sol.WriteError(w, sol.Errorf("forbidden", "for my own devices only"))
		return
	}
	rel, err := cleanRel(r.URL.Query().Get("path"), false)
	if err != nil {
		sol.WriteError(w, bad("%v", err))
		return
	}
	full, err := a.abs(rel)
	if err != nil {
		sol.WriteError(w, bad("%v", err))
		return
	}
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		sol.WriteError(w, missing("file"))
		return
	}
	f, err := os.Open(full)
	if err != nil {
		sol.WriteError(w, missing("file"))
		return
	}
	defer f.Close()
	name := baseOf(rel)
	ct := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	switch {
	case ct == "":
		ct = "application/octet-stream"
	case strings.HasPrefix(ct, "text/html"), strings.HasPrefix(ct, "image/svg"), strings.Contains(ct, "xml"), strings.HasPrefix(ct, "text/javascript"):
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "no-cache")
	disp := "inline"
	if !strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "text/plain") {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", disp+"; filename*=UTF-8''"+urlEscape(name))
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

func urlEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}
