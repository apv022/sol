package tasks

import (
	"embed"
	"io/fs"
)

//go:embed ui/icon.svg ui/icon-192.png ui/icon-512.png
var iconFS embed.FS

// Icons is the app's icon as the daemon serves it at /_sol/icon.svg, icon-192.png and
// icon-512.png (web app manifest, home-screen shortcuts), rendered from the kit's glyph by
// `make icons`.
func Icons() fs.FS {
	f, _ := fs.Sub(iconFS, "ui")
	return f
}
