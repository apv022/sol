# AGENTS.md

Guide for anyone (human or AI agent) maintaining Sol. Read this first; it's short on purpose.

## Commands

```sh
make test        # go test -race ./...  (includes full e2e over clearnet, no Tor needed)
make check       # gofmt + vet + staticcheck + tests. Must pass before every commit
make build       # bin/sol
make test-tor    # real-Tor e2e (SOL_TEST_TOR=1), needs tor on PATH, takes minutes
```

Run a local node without touching your real `~/.sol`:

```sh
SOL_DATA=/tmp/sol-node go run ./cmd/sol
SOL_CLIENT_DIR=/tmp/sol-client go run ./cmd/sol connect '<link>'
```

## Where things are

- `cmd/sol/main.go`: the CLI. Keep logic out of it; it parses flags and calls packages.
- `internal/node/node.go`: wires everything together. Routing, pairing, revocation, publications.
- `internal/protocol`: the handshake. Security-critical. Spec in `docs/PROTOCOL.md`.
- `internal/transport`: `Transport` interface; `tor.go` (control port + SOCKS), `clearnet.go`, `i2p.go` (stub).
- `internal/runtime`: running `tor` as a child process (native / bundled / docker stub).
- `internal/api` + `web/`: the panel. Plain JS, no build step.
- `internal/e2e`: the tests that prove the product works. Extend them for any behaviour change.
- Full table: `docs/ARCHITECTURE.md`. How to add features: `docs/EXTENDING.md`.

## Rules

1. **No third-party Go dependencies** without a strong reason. The stdlib covers this project.
2. **Never send bytes to a rejected connection.** Use `node.reject` (close + log).
3. **Never log secrets**: pairing secrets, keys, Tor client-auth keys, request bodies.
4. **The panel stays loopback-only** with the Host check and CSRF. Don't loosen `api.guard`.
5. **Escape everything in the panel**: every API value goes through `esc()` in `web/app.js`.
6. **Don't change the `sol/1` wire format in place.** Version it (see `docs/PROTOCOL.md`).
7. Files with secrets are `0600`, directories `0700`, config writes go through `config.WriteFileAtomic`.
8. **Be honest about stubs.** Unbuilt features return `ErrNotImplemented` or an audit `skip`, never fake success.
9. Godoc on every exported identifier; a package doc comment in every package explaining its job
   and invariants. Comment the *why*, not the *what*.
10. Behaviour changes need a test. Bug fixes need a regression test.

## Gotchas

- Tor needs ~30 s to bootstrap and a new onion can take a minute to become reachable. The real-Tor
  e2e test retries pairing for this reason.
- Changing an onion's client-auth keys means DEL_ONION + ADD_ONION with the same key
  (`Tor.addOnion`). `node.refreshNodePublication` skips this when the key set hasn't changed, so
  pairing doesn't bounce live connections.
- Tor "ready" means *reachable*: `Tor.Status` stays `starting` until the self-check reaches our
  own onion (`torhealth.go`). A network change restarts tor on purpose.
- `config.Config` must only be touched through its methods (it holds a mutex). Use `Snapshot()`
  for read-only views.
- `web/vendor/qrcode.js` is vendored (MIT, version and checksum in `web/vendor/VENDORED.txt`).

## Docs to keep in sync

If you change behaviour, update the matching doc in the same commit: `docs/PROTOCOL.md`
(handshake/link), `docs/ARCHITECTURE.md` (packages, flows, on-disk layout),
`docs/THREAT_MODEL.md` (security properties), `README.md` (status table, commands),
`CHANGELOG.md`.
