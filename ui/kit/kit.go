// Package kit embeds Sol's shared design system (tokens, base styles, custom elements, icons)
// so the daemon can serve it to every app UI and the suite looks like one product.
//
// Apps style with the CSS custom properties in tokens.css and never hard-code colours.
package kit

import "embed"

// FS holds tokens.css, kit.css, kit.js, icons.js and demo.html at its root.
//
//go:embed tokens.css kit.css kit.js icons.js demo.html
var FS embed.FS
