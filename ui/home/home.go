// Package home embeds the Sol app UI (design/ADMIN.md, "The Sol app (UI)"): the home screen,
// people, devices, inbox, sharing and settings. The daemon serves these static files at its UI
// root and the page talks to the admin API on the same origin; the kit (/kit/) and sol.js are
// served by the daemon as for every other UI. The files are plain ES modules with no build step.
package home

import "embed"

// FS holds the UI files at its root (index.html is the entry point).
//
//go:embed index.html setup.html setup.js icon.svg icon-192.png icon-512.png home.css wallpaper.css admin.js util.js app.js
var FS embed.FS
