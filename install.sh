#!/bin/sh
# Installs Sol on Linux or macOS:
#
#   gh api repos/apv022/sol-test/contents/install.sh -H 'Accept: application/vnd.github.raw' | sh
#
# (while the repo is private; once public: curl -fsSL <raw URL of this file> | sh)
#
# It downloads the release binary for this OS and CPU (tor is built in), checks it against the
# release's SHA256SUMS, installs it as ~/.local/bin/sol (or $SOL_BIN_DIR), and makes sure plain
# `sol` works in new terminals: it adds the folder to PATH in your shell's startup files when it
# isn't there yet. It never uses sudo and touches nothing else.
#
#   SOL_VERSION=v0.2.0-test.3   install that release instead of the newest one
#   SOL_BIN_DIR=/path           install there instead of ~/.local/bin
#   SOL_REPO=owner/name         download from that GitHub repo (default apv022/sol-test)
#   SOL_NO_PATH=1               leave your shell startup files alone
#   SOL_NO_START=1              don't start Sol when the install is done
#
# While the repo is private, downloads need a GitHub login: either the gh CLI signed in
# (`gh auth login`) or GITHUB_TOKEN set to a token that can read the repo.
set -eu

repo=${SOL_REPO:-apv022/sol-test}
say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "this needs $1"; }
need curl; need uname

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "no installer for $(uname -s). On Windows, download sol-windows-amd64.exe from https://github.com/$repo/releases" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "no build for $(uname -m) CPUs" ;;
esac
asset="sol-$os-$arch"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

# fetch NAME: downloads release asset NAME of $tag into $tmp, by gh when it is signed in, else
# by the GitHub API (which also works for private repos with GITHUB_TOKEN).
api() { # api PATH [extra curl args]: a GitHub API GET
  p=$1; shift
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" -H 'X-GitHub-Api-Version: 2022-11-28' "$@" "https://api.github.com/repos/$repo$p"
  else
    curl -fsSL -H 'X-GitHub-Api-Version: 2022-11-28' "$@" "https://api.github.com/repos/$repo$p"
  fi
}
use_gh=
if [ -z "${GITHUB_TOKEN:-}" ] && command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then use_gh=1; fi

tag=${SOL_VERSION:-}
if [ -z "$tag" ]; then
  if [ -n "$use_gh" ]; then
    tag=$(gh release list -R "$repo" --limit 1 --json tagName -q '.[0].tagName' 2>/dev/null) || tag=
  else
    # The newest release, pre-releases included (test builds are pre-releases).
    tag=$(api "/releases?per_page=1" 2>/dev/null | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1) || tag=
  fi
  [ -n "$tag" ] || die "can't find a release in $repo. If it's private, sign in with \`gh auth login\` or set GITHUB_TOKEN."
fi

fetch() {
  if [ -n "$use_gh" ]; then
    gh release download "$tag" -R "$repo" -p "$1" -D "$tmp" --clobber >/dev/null 2>&1 || die "no $1 in release $tag"
    return
  fi
  # The asset's API URL works for public and private repos alike.
  id=$(api "/releases/tags/$tag" | tr ',{' '\n\n' | awk -v n="\"$1\"" '
    /^[[:space:]]*"id":[[:space:]]*[0-9]+[[:space:]]*$/ { gsub(/[^0-9]/, "", $0); last = $0 }
    index($0, "\"name\":") && index($0, n) { print last; exit }') || id=
  [ -n "$id" ] || die "no $1 in release $tag"
  api "/releases/assets/$id" -H 'Accept: application/octet-stream' -o "$tmp/$1" || die "downloading $1 failed"
}

say "Downloading Sol $tag for $os/$arch..."
fetch "$asset"
fetch SHA256SUMS

want=$(awk -v f="$asset" '$2 == f || $2 == "*"f { print $1 }' "$tmp/SHA256SUMS")
[ -n "$want" ] || die "$asset is not listed in SHA256SUMS"
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$asset" | cut -d' ' -f1)
elif command -v shasum >/dev/null 2>&1; then got=$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)
else die "this needs sha256sum or shasum to check the download"; fi
[ "$want" = "$got" ] || die "checksum mismatch for $asset: the download is damaged or was tampered with"

dir=${SOL_BIN_DIR:-$HOME/.local/bin}
mkdir -p "$dir"
# A running Sol keeps its binary open: install next to it and rename over it (atomic).
cp "$tmp/$asset" "$dir/.sol.new"
chmod 755 "$dir/.sol.new"
mv -f "$dir/.sol.new" "$dir/sol"
say "Installed $dir/sol"

# Make plain `sol` work: put $dir on PATH in the startup files of the shells people use.
on_path() { case ":$PATH:" in *":$1:"*) return 0 ;; esac; return 1; }
added=
if [ -z "${SOL_NO_PATH:-}" ] && ! on_path "$dir"; then
  line="export PATH=\"$dir:\$PATH\"  # added by the Sol installer"
  for rc in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.zshrc"; do
    case $rc in
      */.profile) ;; # always: login shells and most desktop sessions read it
      *) [ -f "$rc" ] || continue ;;
    esac
    if ! grep -qs "added by the Sol installer" "$rc"; then
      printf '\n%s\n' "$line" >> "$rc"
      added="$added $rc"
    fi
  done
  fish_conf="$HOME/.config/fish/conf.d/sol.fish"
  if command -v fish >/dev/null 2>&1 && [ ! -f "$fish_conf" ]; then
    mkdir -p "$(dirname "$fish_conf")"
    printf 'fish_add_path -g %s  # added by the Sol installer\n' "$dir" > "$fish_conf"
    added="$added $fish_conf"
  fi
fi

say ""
if [ -n "$added" ]; then
  say "Added $dir to your PATH in:$added"
  say "Open a new terminal (or run: export PATH=\"$dir:\$PATH\") and \`sol\` works from anywhere."
fi

if [ -z "${SOL_NO_START:-}" ]; then
  say "Starting Sol (it sets itself up on first run and opens in your browser)..."
  "$dir/sol" || say "Sol didn't start; run \`sol\` to try again, \`sol logs\` to see why."
  "$dir/sol" launcher on >/dev/null 2>&1 || true
else
  say "Start it with:  sol"
fi
say ""
say "Your synced folder is ~/SolDrive. Useful commands:"
say "  sol open          open the Sol app"
say "  sol status        this device, people and connections"
say "  sol stop          stop Sol;  sol autostart on   start it when you log in"
say "  sol restore --words \"...\"   bring back your Sol from your 24 backup words (on a fresh device)"
