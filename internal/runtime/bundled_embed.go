//go:build bundled

package runtime

import "embed"

// Release builds embed bin/ (filled by scripts/fetch-binaries.sh).
//
//go:embed bin
var embedded embed.FS

func init() { bundledFS = embedded }
