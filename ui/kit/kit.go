// Package kit embeds Sol's shared design system (tokens, base styles, custom elements, icons)
// so the daemon can serve it to every app UI and the suite looks like one product.
//
// Apps style with the CSS custom properties in tokens.css and never hard-code colours.
package kit

import "embed"

// FS holds tokens.css, kit.css, kit.js, icons.js, demo.html, TOKENS.md, fonts/ and themes/.
//
//go:embed tokens.css kit.css kit.js icons.js qr.js testing.js demo.html TOKENS.md fonts themes vendor
var FS embed.FS
