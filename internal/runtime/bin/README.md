# Bundled binaries

Release builds (`make build-bundled`, i.e. `go build -tags bundled`) embed tor so `sol` works
without Tor installed. Only this README, `checksums.json` and `VERSIONS.txt` are committed; the
platform folders are gitignored and filled by `scripts/fetch-tor.sh`.

What is bundled: tor 0.4.9.16 from the Tor Browser 16.0.1 expert bundles, signed by the Tor
Browser key `EF6E286DDA85EA2A4BA7DE684E2C6E8793298290` (the script checks every signature and
`sha256sums-unsigned-build.txt` before keeping anything):

```
linux-amd64/   tor libcrypto.so.3 libssl.so.3 libevent-2.1.so.7
linux-arm64/   tor libcrypto.so.3 libssl.so.3 libevent-2.1.so.7
darwin-amd64/  tor libevent-2.1.7.dylib          (tor loads it via @executable_path)
darwin-arm64/  tor libevent-2.1.7.dylib
windows-amd64/ tor.exe                           (statically linked)
checksums.json  {"linux-amd64/tor": "<sha256 hex>", ...} every file, pinned
VERSIONS.txt
```

Each build embeds only its own platform's folder. At run time the bundled provider
(`../bundled.go`) extracts the folder into `<data dir>/bin/<os>-<arch>/`, refuses any file
whose SHA-256 is not pinned in `checksums.json`, runs tor with that folder on
`LD_LIBRARY_PATH` (`DYLD_LIBRARY_PATH` on macOS), and falls back to a tor on PATH if it
can't. Windows on arm64 has no expert bundle: those builds embed no tor and use a tor on PATH
(for example the x86_64 expert bundle, which runs under emulation). Android ships its own tor
(`android/`) and points `SOL_TOR_BIN` at it (the "platform" provider, tried before the bundled
one, without a checksum: the APK is signed); `sol init --tor-bin <path>` does the same for any
other packaged tor.

```sh
scripts/fetch-tor.sh          # fetch + verify; fails unless every file matches checksums.json
scripts/fetch-tor.sh --pin    # moving to a new tor: rewrite checksums.json and VERSIONS.txt
make build-bundled            # bin/sol with the tor for this machine embedded
```
