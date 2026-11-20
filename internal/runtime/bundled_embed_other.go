//go:build bundled && !((linux && (amd64 || arm64)) || (darwin && (amd64 || arm64)) || (windows && amd64))

package runtime

import "embed"

// Platforms without a Tor expert bundle (Windows on arm64, the BSDs, ...) embed only the
// checksums: the bundled provider is unavailable there, and a tor on PATH is used instead.
//
//go:embed bin/checksums.json
var embedded embed.FS

func init() { bundledFS = embedded }
