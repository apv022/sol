package apphost

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/apv022/sol/ui/home"
)

// swJS is the service worker every origin serves at /_sol/sw.js (M1 in design/APPS_EVERYWHERE.md).
//
//go:embed sw.js
var swJS []byte

// staticPWA is the set of paths that make an origin installable as a web app. Browsers fetch the
// manifest and icons without cookies, so these are served before the sign-in gate; they hold
// nothing but the app's public name and icon. Nothing else is exempt.
var staticPWA = map[string]bool{
	"/_sol/manifest.webmanifest": true, "/_sol/icon.svg": true, "/_sol/icon-192.png": true,
	"/_sol/icon-512.png": true, "/_sol/sw.js": true,
}

// IsStaticPWA reports whether r is a GET of one of the sign-in-free installability paths.
func IsStaticPWA(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && staticPWA[r.URL.Path]
}

// servePWA serves the manifest and icons of app a (nil: the Sol app at the UI root) and reports
// whether r was one of them. sw.js is common to all origins (ServeShared).
func (h *Host) servePWA(w http.ResponseWriter, r *http.Request, a *app) bool {
	if !IsStaticPWA(r) || r.URL.Path == "/_sol/sw.js" {
		return false
	}
	if r.URL.Path == "/_sol/manifest.webmanifest" {
		name, desc, theme := "Sol", "Your own cloud, shared with the people you choose.", "#0d1117"
		if a != nil {
			name, desc, theme = a.m.Name, a.m.Description, "#f5f5f7"
		}
		m := map[string]any{
			"name": name, "short_name": name, "description": desc, "id": "/", "start_url": "/", "scope": "/",
			"display": "standalone", "theme_color": theme, "background_color": theme,
			"icons": []map[string]string{
				{"src": "/_sol/icon.svg", "sizes": "any", "type": "image/svg+xml"},
				{"src": "/_sol/icon-192.png", "sizes": "192x192", "type": "image/png"},
				{"src": "/_sol/icon-512.png", "sizes": "512x512", "type": "image/png"},
				{"src": "/_sol/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "maskable"},
			},
		}
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Header().Set("Cache-Control", "no-cache")
		_ = json.NewEncoder(w).Encode(m)
		return true
	}
	b, ctype := h.icon(a, strings.TrimPrefix(r.URL.Path, "/_sol/"))
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
	return true
}

// icon returns the bytes and type of file (icon.svg, icon-192.png, icon-512.png) for an app: a
// builtin's pre-rendered icons (cmd/iconrender), a third-party app's own PNG or SVG icon file,
// else a generated coloured square. The Sol app (nil) uses the logo of ui/home.
func (h *Host) icon(a *app, file string) ([]byte, string) {
	ctype := "image/png"
	if strings.HasSuffix(file, ".svg") {
		ctype = "image/svg+xml"
	}
	var src fs.FS = home.FS
	hue := -1
	if a != nil {
		src, hue = nil, hueOf(a.m.ID)
		if name, ok := strings.CutPrefix(a.m.Exec, "builtin:"); ok {
			src = h.cfg.Builtins[name].Icons
		}
		if src == nil {
			if b := ownIcon(a, file); b != nil {
				return b, ctype
			}
		}
	}
	if src != nil {
		if b, err := fs.ReadFile(src, file); err == nil {
			return b, ctype
		}
	}
	if ctype == "image/svg+xml" {
		return []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="` + hexColor(hue) + `"/></svg>`), ctype
	}
	size := 192
	if strings.Contains(file, "512") {
		size = 512
	}
	return squarePNG(size, hexRGBA(hue)), ctype
}

// ownIcon reads an installed app's icon file (manifest "icon" naming a .png or .svg inside its
// directory) when it fits the wanted file's type: an SVG for icon.svg, a PNG for the PNG sizes
// (browsers scale it). A name that leaves the app's directory is ignored.
func ownIcon(a *app, file string) []byte {
	ic := a.m.Icon
	ext := strings.ToLower(filepath.Ext(ic))
	if ic == "" || filepath.IsAbs(ic) || strings.Contains(ic, "..") || ext != filepath.Ext(file) {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(a.dir, filepath.FromSlash(ic)))
	if err != nil || len(b) > 1<<20 {
		return nil
	}
	return b
}

// hueOf is ui/home/util.js hueOf: the same colour for an app everywhere.
func hueOf(key string) int {
	h := 0
	for _, c := range key {
		h = (h*31 + int(c)) % 360
	}
	return h
}

func hexRGBA(hue int) color.RGBA {
	if hue < 0 {
		return color.RGBA{0x0d, 0x11, 0x17, 255}
	}
	// hsl(hue 70% 50%)
	const c = 0.7 // chroma of l=.5, s=.7
	x := c * (1 - math.Abs(math.Mod(float64(hue)/60, 2)-1))
	m := 0.5 - c/2
	var r, g, b float64
	switch hue / 60 {
	case 0:
		r, g, b = c, x, 0
	case 1:
		r, g, b = x, c, 0
	case 2:
		r, g, b = 0, c, x
	case 3:
		r, g, b = 0, x, c
	case 4:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return color.RGBA{uint8((r + m) * 255), uint8((g + m) * 255), uint8((b + m) * 255), 255}
}

func hexColor(hue int) string {
	c := hexRGBA(hue)
	const d = "0123456789abcdef"
	return string([]byte{'#', d[c.R>>4], d[c.R&15], d[c.G>>4], d[c.G&15], d[c.B>>4], d[c.B&15]})
}

// squarePNG is the generated fallback icon: one solid colour.
func squarePNG(size int, c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// MenuApp is an app with a UI as the desktop's app menu lists it.
type MenuApp struct {
	ID, Name string
	Icon     []byte // the 512 px PNG
}

// MenuApps lists the installed apps that have a UI, sorted by id, with their icons.
func (h *Host) MenuApps() []MenuApp {
	h.mu.Lock()
	var as []*app
	for _, a := range h.apps {
		if a.m.UI != "" {
			as = append(as, a)
		}
	}
	h.mu.Unlock()
	sort.Slice(as, func(i, j int) bool { return as[i].m.ID < as[j].m.ID })
	out := make([]MenuApp, 0, len(as))
	for _, a := range as {
		b, _ := h.icon(a, "icon-512.png")
		out = append(out, MenuApp{ID: a.m.ID, Name: a.m.Name, Icon: b})
	}
	return out
}
