// Package web embeds the control panel (plain HTML/CSS/JS, no build step)
// so `go build` and `go install` always produce a binary with the panel.
// Edit the files in this directory directly; there is nothing to compile.
package web

import "embed"

// FS holds the panel's static files.
//
//go:embed index.html app.js style.css vendor/qrcode.js
var FS embed.FS
