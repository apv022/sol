// Package web embeds the control panel (plain HTML/CSS/JS, no build step)
// so `go build` and `go install` always produce a binary with the panel.
// Edit the files in this directory directly; there is nothing to compile.
package web

import (
	"embed"
	"strings"
)

// FS holds the panel's static files.
//
//go:embed index.html app.js style.css manifest.json icon.svg vendor/qrcode.js
var FS embed.FS

const (
	tokensBegin = "/* sol:tokens:begin"
	tokensEnd   = "/* sol:tokens:end */"
	baseEnd     = "/* sol:base:end */"
)

// Tokens returns the design-token block of style.css (colours, font, size
// and the themes that replace them), delimited there by sol:tokens comments.
// Server-rendered pages outside the panel, such as the shared files page,
// inline it so they share the panel's look by construction instead of by a
// copy that can drift. It returns "" if the delimiters are gone; a test in
// this package keeps that from happening unnoticed.
func Tokens() string { return region(tokensEnd) }

// Base returns the tokens plus the shared visual base of style.css (the
// sol:base region: typography, header branding, buttons, the row card and
// empty states). Server-rendered pages inline it, so a button on the shared
// files page is the panel's button, not a lookalike. "" if the delimiters are
// gone.
func Base() string { return region(baseEnd) }

func region(end string) string {
	b, err := FS.ReadFile("style.css")
	if err != nil {
		return ""
	}
	css := string(b)
	i := strings.Index(css, tokensBegin)
	j := strings.Index(css, end)
	if i < 0 || j < i {
		return ""
	}
	return css[i : j+len(end)]
}
