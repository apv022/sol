# Bundled binaries

Release builds (`go build -tags bundled`) embed this folder so `sol` works without Tor installed.
Nothing in here except this README, `checksums.json` and `VERSIONS.txt` is committed to git.

Layout, one folder per platform:

```
linux-amd64/tor
darwin-arm64/tor
windows-amd64/tor.exe
checksums.json   # {"linux-amd64/tor": "<sha256 hex>", ...}
VERSIONS.txt
```

Fill it with `scripts/fetch-binaries.sh` (downloads the Tor Expert Bundle and records checksums).
At runtime the bundled provider refuses to run a binary whose SHA-256 is not listed in
`checksums.json`.
