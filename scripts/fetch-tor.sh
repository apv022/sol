#!/bin/sh
# Fetch the tor that release builds embed (`make build-bundled`, go build -tags bundled):
# tor 0.4.9.16 from the Tor Browser 16.0.1 expert bundles, for every platform Sol bundles tor
# on, verified twice before anything is kept:
#
#   1. every .asc signature (the tarballs and sha256sums-unsigned-build.txt) must be a good
#      signature by the Tor Browser signing key, primary fingerprint
#      EF6E286DDA85EA2A4BA7DE684E2C6E8793298290, fetched by WKD (torbrowser@torproject.org)
#      into a throwaway GNUPGHOME;
#   2. every tarball's SHA-256 must be the one sha256sums-unsigned-build.txt lists.
#
# Then tor and the shared libraries it needs next to it (Linux: libcrypto, libssl, libevent;
# macOS: libevent) go to internal/runtime/bin/<goos>-<goarch>/ (gitignored), and every
# file's SHA-256 is checked against internal/runtime/bin/checksums.json (committed). The
# bundled provider refuses to run any file whose checksum is not pinned there.
#
#   scripts/fetch-tor.sh          fetch, verify, and check against the pinned checksums
#   scripts/fetch-tor.sh --pin    the same, then (re)write checksums.json and VERSIONS.txt
#                                 (only when moving to a new tor; review the diff)
#
# Windows on arm64 has no expert bundle: Sol there uses a tor on PATH (native provider).
# Android builds its own tor (android/).
#
# Needs: sh, curl, gpg, tar, sha256sum (or shasum).
set -eu

TB_VERSION=16.0.1
TOR_VERSION=0.4.9.16
FPR=EF6E286DDA85EA2A4BA7DE684E2C6E8793298290
BASE=https://dist.torproject.org/torbrowser/$TB_VERSION
# expert bundle platform -> Go platform
PLATFORMS="linux-x86_64:linux-amd64 linux-aarch64:linux-arm64 macos-x86_64:darwin-amd64 macos-aarch64:darwin-arm64 windows-x86_64:windows-amd64"

pin=0
[ "${1:-}" = "--pin" ] && pin=1

root=$(cd "$(dirname "$0")/.." && pwd)
out=$root/internal/runtime/bin
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

die() { echo "fetch-tor: $*" >&2; exit 1; }
sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
fetch() { curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$work/dl/$1" "$BASE/$1" || die "download failed: $BASE/$1"; }

# verify FILE: FILE.asc must be a good signature whose primary key is $FPR.
verify() {
	status=$(gpg --batch --status-fd 1 --verify "$work/dl/$1.asc" "$work/dl/$1" 2>/dev/null) || die "bad signature on $1"
	# VALIDSIG <subkey fpr> ... <primary fpr>
	echo "$status" | grep '^\[GNUPG:\] VALIDSIG ' | awk '{print $NF}' | grep -qx "$FPR" || die "$1 is not signed by $FPR"
}

mkdir -p "$work/dl"
GNUPGHOME=$work/gnupg
export GNUPGHOME
mkdir -m 700 "$GNUPGHOME"
echo "fetching the Tor Browser signing key (WKD)"
gpg --batch --quiet --auto-key-locate clear,nodefault,wkd --locate-external-keys torbrowser@torproject.org >/dev/null 2>&1 ||
	die "could not fetch torbrowser@torproject.org by WKD"
gpg --batch --with-colons --fingerprint "$FPR" 2>/dev/null | grep -q "^fpr:::::::::$FPR:" || die "WKD returned a key other than $FPR"

fetch sha256sums-unsigned-build.txt
fetch sha256sums-unsigned-build.txt.asc
verify sha256sums-unsigned-build.txt

sums=$work/sums
: >"$sums"
for pair in $PLATFORMS; do
	tb=${pair%%:*}
	gp=${pair##*:}
	tgz=tor-expert-bundle-$tb-$TB_VERSION.tar.gz
	echo "fetching $tgz"
	fetch "$tgz"
	fetch "$tgz.asc"
	verify "$tgz"
	want=$(awk -v f="$tgz" '$2 == f {print $1}' "$work/dl/sha256sums-unsigned-build.txt")
	[ -n "$want" ] || die "$tgz is not in sha256sums-unsigned-build.txt"
	[ "$(sha256 "$work/dl/$tgz")" = "$want" ] || die "$tgz does not match sha256sums-unsigned-build.txt"

	x=$work/x/$tb
	mkdir -p "$x"
	tar -xzf "$work/dl/$tgz" -C "$x"
	[ -d "$x/tor" ] || die "$tgz has no tor/ directory"
	rm -rf "${out:?}/$gp"
	mkdir -p "$out/$gp"
	# tor itself and the shared libraries next to it; not the pluggable transports or geoip.
	for f in "$x"/tor/*; do
		[ -f "$f" ] || continue
		n=$(basename "$f")
		case "$n" in
		tor | tor.exe | *.so | *.so.* | *.dylib | *.dll) ;;
		*) continue ;;
		esac
		cp "$f" "$out/$gp/$n"
		chmod 0755 "$out/$gp/$n"
		echo "$gp/$n $(sha256 "$out/$gp/$n")" >>"$sums"
	done
	[ -f "$out/$gp/tor" ] || [ -f "$out/$gp/tor.exe" ] || die "$tgz has no tor binary"
done

sort -o "$sums" "$sums"
json=$work/checksums.json
{
	echo "{"
	n=$(wc -l <"$sums")
	i=0
	while read -r key sum; do
		i=$((i + 1))
		sep=","
		[ "$i" -eq "$n" ] && sep=""
		printf '  "%s": "%s"%s\n' "$key" "$sum" "$sep"
	done <"$sums"
	echo "}"
} >"$json"

if [ "$pin" = 1 ]; then
	cp "$json" "$out/checksums.json"
	{
		echo "# Versions of the binaries placed here by scripts/fetch-tor.sh."
		echo "# Format: <os>-<arch>/<name> <version>"
		while read -r key sum; do
			case "$key" in */tor | */tor.exe) echo "$key tor $TOR_VERSION (Tor Browser $TB_VERSION expert bundle)" ;; esac
		done <"$sums"
	} >"$out/VERSIONS.txt"
	echo "pinned $(wc -l <"$sums" | tr -d ' ') files in internal/runtime/bin/checksums.json"
else
	cmp -s "$json" "$out/checksums.json" || {
		diff "$out/checksums.json" "$json" >&2 || true
		die "the fetched files do not match internal/runtime/bin/checksums.json (run with --pin only when moving to a new tor)"
	}
	echo "all files match internal/runtime/bin/checksums.json"
fi
echo "now: make build-bundled"
