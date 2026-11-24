// Command iconrender draws Sol's app icons to files, at dev time, with no Node.js: Go's stdlib
// cannot rasterize SVG and the daemon must serve PNGs (a web app manifest needs them, as do
// Android shortcuts), so the PNGs are rendered once here and committed.
//
// For every apps/<id>/ with a UI it opens a page of the kit's own icon (the glyph and hue
// ui/home/util.js appIconSpec picks, drawn like <sol-app-icon>, full bleed) in headless Firefox
// at 192 and 512 px and screenshots it, and has the page post back the matching SVG. For the Sol
// app it renders ui/home/icon.svg. Output: apps/<id>/ui/icon.svg, icon-192.png, icon-512.png and
// ui/home/icon-192.png, icon-512.png. Run it from the repository root with `make icons`.
//
// The browser is $SOL_BROWSER, else ~/.local/opt/firefox/firefox, else firefox on PATH.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// page draws the icon of app ?id= full bleed and posts its rounded SVG to /__svg. It imports the
// kit's own glyph table and appIconSpec so an icon here is the icon every UI shows.
const page = `<!doctype html><meta charset="utf-8"><body style="margin:0;background:#0d1117;overflow:hidden">
<div id="out" style="width:100vw;height:100vh"></div>
<script type="module">
import { icons } from '/ui/kit/icons.js';
import { appIconSpec } from '/ui/home/util.js';
import { appIconColors, ICON_STROKE } from '/ui/kit/appicon.js';
const q = new URLSearchParams(location.search);
const { glyph, hue } = appIconSpec({ id: q.get('id'), icon: 'icon.svg' }, icons);
// The same gradient <sol-app-icon> draws (ui/kit/appicon.js).
const [a, b] = appIconColors(hue);
const svg = (rx) => '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512"><defs><linearGradient id="g" x1="0" y1="0" x2=".35" y2="1">' +
  '<stop offset="0" stop-color="' + a + '"/><stop offset="1" stop-color="' + b + '"/></linearGradient></defs>' +
  '<rect width="512" height="512" rx="' + rx + '" fill="url(#g)"/>' +
  '<g transform="translate(128 128) scale(10.667)" fill="none" stroke="#fff" stroke-width="' + ICON_STROKE + '" stroke-linecap="round" stroke-linejoin="round">' +
  icons[glyph].map((d) => '<path d="' + d + '"/>').join('') + '</g></svg>';
const out = document.getElementById('out');
out.innerHTML = svg(0).replace('<svg ', '<svg width="100%" height="100%" ');
await fetch('/__svg?id=' + encodeURIComponent(q.get('id')), { method: 'POST', body: svg(112) });
</script>`

// rootPage shows the Sol logo on its own background colour, so the PNG is a full square.
const rootPage = `<!doctype html><meta charset="utf-8"><body style="margin:0;background:#0d1117;overflow:hidden">
<img src="/ui/home/icon.svg" style="width:100vw;height:100vh;display:block">`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "iconrender:", err)
		os.Exit(1)
	}
}

func browser() (string, error) {
	if b := os.Getenv("SOL_BROWSER"); b != "" {
		return b, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local/opt/firefox/firefox")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return exec.LookPath("firefox")
}

func run() error {
	bin, err := browser()
	if err != nil {
		return fmt.Errorf("no browser found (set SOL_BROWSER): %w", err)
	}
	svgs := make(chan [2]string, 64)
	mux := http.NewServeMux()
	files := http.FileServer(http.Dir("."))
	mux.Handle("/", files)
	mux.HandleFunc("GET /__icon", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, page) })
	mux.HandleFunc("GET /__root", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, rootPage) })
	mux.HandleFunc("POST /__svg", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		svgs <- [2]string{r.URL.Query().Get("id"), string(b)}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()
	addr := ln.Addr().String()

	manifests, _ := filepath.Glob("apps/*/app.json")
	for _, m := range manifests {
		var mf struct {
			ID string `json:"id"`
			UI string `json:"ui"`
		}
		b, err := os.ReadFile(m)
		if err != nil || json.Unmarshal(b, &mf) != nil || mf.UI == "" {
			continue
		}
		out := filepath.Join(filepath.Dir(m), "ui")
		for _, size := range []int{192, 512} {
			u := "http://" + addr + "/__icon?id=" + url.QueryEscape(mf.ID)
			if err := shot(bin, u, filepath.Join(out, fmt.Sprintf("icon-%d.png", size)), size); err != nil {
				return fmt.Errorf("%s: %w", mf.ID, err)
			}
		}
		// Both loads post (the same SVG); take this app's, skipping any other app's late post.
		for got := false; !got; {
			select {
			case s := <-svgs:
				if s[0] != mf.ID {
					continue
				}
				got = true
				if err := os.WriteFile(filepath.Join(out, "icon.svg"), []byte(s[1]), 0o644); err != nil {
					return err
				}
			case <-time.After(5 * time.Second):
				return fmt.Errorf("%s: the page did not post its SVG", mf.ID)
			}
		}
		fmt.Println("rendered", mf.ID)
	}
	for _, size := range []int{192, 512} {
		if err := shot(bin, "http://"+addr+"/__root", filepath.Join("ui/home", fmt.Sprintf("icon-%d.png", size)), size); err != nil {
			return fmt.Errorf("sol: %w", err)
		}
	}
	fmt.Println("rendered sol")
	return nil
}

// shot screenshots url at size x size into out, then checks the image really is that size.
func shot(bin, u, out string, size int) error {
	profile, err := os.MkdirTemp("", "iconrender-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profile)
	if out, err = filepath.Abs(out); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--headless", "--no-remote", "--profile", profile,
		"--screenshot", out, fmt.Sprintf("--window-size=%d,%d", size, size), u)
	cmd.Env = append(os.Environ(), "MOZ_HEADLESS=1")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("firefox: %v: %s", err, b)
	}
	f, err := os.Open(out)
	if err != nil {
		return err
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		return err
	}
	if cfg.Width != size || cfg.Height != size {
		return fmt.Errorf("%s is %dx%d, want %d", out, cfg.Width, cfg.Height, size)
	}
	return nil
}
