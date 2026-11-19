// Package js embeds the browser client (sol.js) so the daemon can serve it to app UIs.
//
// The file is plain ES module source with no build step; it must keep escaping every value that
// reaches the DOM (see js_test.go).
package js

import "embed"

// FS holds sol.js at its root.
//
//go:embed sol.js
var FS embed.FS
