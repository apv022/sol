# AGENTS.md

Guide for anyone (human or AI agent) maintaining Sol. Read this first; it's short on purpose.

## Commands

```sh
make test        # go test -race ./...  (includes full e2e over clearnet, no Tor needed)
make check       # gofmt + vet + staticcheck + tests. Must pass before every commit
make build       # bin/sol
make test-tor    # real-Tor e2e (SOL_TEST_TOR=1), needs tor on PATH, takes minutes
make hooks       # once per clone: every `git push` runs `make check` first
```

CI (GitHub Actions) runs `make check` on Linux, macOS, and Windows. Locally you only cover your
own OS: watch for OS limits such as the 104-byte Unix socket path on macOS.

Run two local nodes without touching your real `~/.sol` (give each its own ports in
`config.json`, or use `"tor": false, "clearnet": true` for a quick LAN test):

```sh
SOL_DATA=/tmp/sol-a go run ./cmd/sol            # node A
SOL_DATA=/tmp/sol-a go run ./cmd/sol invite     # talks to node A over /tmp/sol-a/control.sock
SOL_DATA=/tmp/sol-b go run ./cmd/sol pair '<link>'
```

## Where things are

- `docs/PEERS.md`: the model. Every machine is a node; nodes pair as peers.
- `cmd/sol/main.go`: the CLI. Keep logic out of it: it runs the node, or sends one request to the
  running node over `internal/control`.
- `internal/node/`: wires everything together. `node.go` (listener, routing, publications),
  `dial.go` (reaching peers, pairing as the joiner, unpair), `inbox.go` (files),
  `ctlserver.go` (CLI socket), `views.go` (panel backend).
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
6. **Protocol changes:** Sol is pre-release, so change the handshake in place and update
   `docs/PROTOCOL.md` and the tests. From the first public release, version it instead.
7. Files with secrets are `0600`, directories `0700`, config writes go through `config.WriteFileAtomic`.
8. **Be honest about stubs.** Unbuilt features return `ErrNotImplemented` or an audit `skip`, never fake success.
9. Godoc on every exported identifier; a package doc comment in every package explaining its job
   and invariants. Comment the *why*, not the *what*.
10. Behaviour changes need a test. Bug fixes need a regression test.
11. **A peer reaches only what it is shared.** New peers get `_inbox` only; peer credentials never
    appear in the panel API or the control socket.

## Gotchas

- Tor needs ~30 s to bootstrap and a new onion can take a minute to become reachable. Dialing a
  peer retries (`Options.DialAttempts`), and the real-Tor e2e test retries pairing.
- Changing an onion's client-auth keys means DEL_ONION + ADD_ONION with the same key
  (`Tor.addOnion`). `node.refreshNodePublication` skips this when the key set hasn't changed, so
  pairing doesn't bounce live connections.
- Tor "ready" means *reachable*: `Tor.Status` stays `starting` until the self-check reaches our
  own onion (`torhealth.go`). A network change restarts tor on purpose.
- `config.Config` must only be touched through its methods (it holds a mutex). Use `Snapshot()`
  for read-only views. Peer maps are deep-copied (`Peer.Clone`); keep it that way.
- Only one node may run per data directory: the control socket doubles as the lock.
- `web/vendor/qrcode.js` is vendored (MIT, version and checksum in `web/vendor/VENDORED.txt`).

## Docs to keep in sync

If you change behaviour, update the matching doc in the same commit: `docs/PROTOCOL.md`
(handshake/link), `docs/ARCHITECTURE.md` (packages, flows, on-disk layout),
`docs/THREAT_MODEL.md` (security properties), `docs/PEERS.md` (the peer model), `README.md`
(status table, commands), `CHANGELOG.md`.
