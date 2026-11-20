//go:build bundled

package runtime

import "embed"

// Release builds for darwin-arm64 embed tor and its libraries (filled by scripts/fetch-tor.sh).
//
//go:embed bin/checksums.json bin/darwin-arm64
var embedded embed.FS

func init() { bundledFS = embedded }
