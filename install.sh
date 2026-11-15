#!/bin/sh
# Installs the latest Sol release binary.
#
#   curl -fsSL https://raw.githubusercontent.com/apv022/sol/main/install.sh | sh
#
# It downloads the archive for this OS and CPU from GitHub Releases, checks
# it against the release's checksums.txt, and puts `sol` in /usr/local/bin
# (if you can write there) or ~/.local/bin. Nothing else is touched, and it
# never uses sudo.
#
#   SOL_VERSION=v0.1.0   install that release instead of the latest
#   SOL_BIN_DIR=/path    install there
set -eu

repo=apv022/sol
say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "this needs $1"; }
need curl; need tar; need uname

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "no build for $(uname -s). On Windows, download the zip from https://github.com/$repo/releases" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "no build for $(uname -m) CPUs; build from source: go install github.com/$repo/cmd/sol@latest" ;;
esac

tag=${SOL_VERSION:-}
if [ -z "$tag" ]; then
  # /releases/latest redirects to /releases/tag/<tag>, or to /releases when nothing is
  # published yet (pre-releases don't count as "latest"), so only trust a /tag/ URL.
  url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest") || url=
  case $url in
    */releases/tag/?*) tag=${url##*/tag/} ;;
    *) die "no Sol release is published yet; build from source (go install github.com/$repo/cmd/sol@latest), or set SOL_VERSION to a pre-release from https://github.com/$repo/releases" ;;
  esac
fi
version=${tag#v}
file="sol_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$tag"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
say "Downloading Sol $tag for $os/$arch..."
curl -fsSL -o "$tmp/$file" "$base/$file" || die "no $file in release $tag"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "release $tag has no checksums.txt"

want=$(grep " $file\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$want" ] || die "$file is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$file" | cut -d' ' -f1)
elif command -v shasum >/dev/null 2>&1; then got=$(shasum -a 256 "$tmp/$file" | cut -d' ' -f1)
else die "this needs sha256sum or shasum to check the download"; fi
[ "$want" = "$got" ] || die "checksum mismatch for $file: the download is damaged or was tampered with"

tar -xzf "$tmp/$file" -C "$tmp" sol

dir=${SOL_BIN_DIR:-}
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir=$HOME/.local/bin; fi
fi
mkdir -p "$dir"
mv "$tmp/sol" "$dir/sol"
chmod 755 "$dir/sol"
say "Installed $dir/sol ($("$dir/sol" version))"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "Add it to your PATH:  export PATH=\"$dir:\$PATH\"" ;;
esac
if ! command -v tor >/dev/null 2>&1; then
  say ""
  say "Sol also needs tor, which isn't installed:"
  case "$os" in
    darwin) say "  brew install tor" ;;
    *) say "  sudo apt install tor      (Debian, Ubuntu)"
       say "  sudo pacman -S tor        (Arch)"
       say "  sudo dnf install tor      (Fedora)" ;;
  esac
fi
say ""
say "Start it with:  sol"
