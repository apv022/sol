//go:build bundled

package runtime

import "embed"

// Release builds for linux-amd64 embed tor and its libraries (filled by scripts/fetch-tor.sh).
//
//go:embed bin/checksums.json bin/linux-amd64
var embedded embed.FS

func init() { bundledFS = embedded }
