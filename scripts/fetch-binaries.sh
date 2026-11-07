#!/usr/bin/env bash
# Place a tor binary into internal/runtime/bin/<os>-<arch>/ for a bundled build
# (`make build-bundled`) and pin its SHA-256 in checksums.json.
#
# Usage:
#   scripts/fetch-binaries.sh /path/to/tor [version]
#   scripts/fetch-binaries.sh "$(command -v tor)" "$(tor --version | head -1)"
#
# Where to get tor:
#   - macOS / Windows: the "Tor Expert Bundle" from https://www.torproject.org/download/tor/
#     (verify its signature first: https://support.torproject.org/tbb/how-to-verify-signature/)
#   - Linux: distribution tor builds link against system libraries, so on Linux the
#     native provider (`sudo apt install tor`) is the recommended setup and bundling is optional.
#
# The bundled provider refuses to run any binary whose checksum is not pinned here.
set -euo pipefail
cd "$(dirname "$0")/.."

src="${1:?usage: $0 /path/to/tor [version]}"
ver="${2:-unknown}"
os=$(go env GOOS); arch=$(go env GOARCH)
name=tor; [ "$os" = windows ] && name=tor.exe
dst="internal/runtime/bin/${os}-${arch}"
mkdir -p "$dst"
cp "$src" "$dst/$name"
chmod 0755 "$dst/$name"

sum=$(sha256sum "$dst/$name" 2>/dev/null | cut -d' ' -f1 || shasum -a 256 "$dst/$name" | cut -d' ' -f1)
key="${os}-${arch}/${name}"
python3 - "$key" "$sum" <<'PY'
import json, sys
path = "internal/runtime/bin/checksums.json"
key, s = sys.argv[1], sys.argv[2]
data = json.load(open(path))
data[key] = s
json.dump(data, open(path, "w"), indent=2, sort_keys=True)
open(path, "a").write("\n")
PY
echo "${key} ${ver}" >> internal/runtime/bin/VERSIONS.txt
echo "pinned ${key} = ${sum}"
echo "now run: make build-bundled"
