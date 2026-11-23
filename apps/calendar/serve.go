package calendar

import (
	"embed"
	"io/fs"
	"net/http"
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
		panic("calendar: actions without a handler: " + strings.Join(miss, ", "))
	}
	mux.Handle("POST /_sol/actions/", act)
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
