# Extending Sol

Every planned feature has a seam in the code already, so adding it should not require changing
the rest of the system. This page says where each seam is and what to touch.

After any change, run `make check` (gofmt, vet, staticcheck, race tests). If you touch the
handshake, pairing, routing, or removal, the e2e tests in `internal/e2e` must still pass. Add a
case there for the new behaviour.

## Adding a transport (I2P, a direct WireGuard path, our own protocol)

Peers are transport-agnostic by design (see [PEERS.md](PEERS.md)): a peer's addresses and access
credentials are maps keyed by transport name, pairing links carry `<transport>=` and
`auth.<transport>=` parameters, and the joiner's card is the same shape. So a new transport needs
no change to the handshake, pairing, the peer record, the panel, or the CLI.

The stub is [`internal/transport/i2p.go`](../internal/transport/i2p.go); its comment has the plan.

1. Implement `transport.Transport`:
   - `Start` launches the program through a `runtime.Provider` (see Tor's `launch`) and returns
     once `Publish` can be called. Report progress through `Status`.
   - `Publish(ctx, Publication)` exposes `p.Target` under the stable `p.ID` and returns the
     public address. Persist one key per ID so addresses survive restarts. Publishing the same
     ID again must replace the earlier publication.
   - `Handles(addr)` claims the addresses this transport can dial (for example `.i2p`).
   - `Dial` reaches a public address. `dialSOCKS5` in `socks.go` is reusable.
   - If the network can restrict who reaches a publication (Tor client auth, I2P encrypted
     leasesets): implement `transport.AccessIssuer` (`NewAccess` returns a per-peer
     `Access{Authorize, Secret}`), honour `p.ClientAuth` (the `Authorize` halves), and implement
     `transport.ClientAuthorizer` (install / forget a peer's `Secret` for its address).
   - Report the node's address in `Status().Addresses["node"]`; that is what goes into links and
     cards.
2. Construct it in `node.buildTransports` and add a config toggle.
3. If it should be tried before or after others, add it to `transportPreference` in
   `internal/node/dial.go` (unknown names sit between I2P and clearnet).
4. Add a `transport_test.go` case with a fake daemon (see the fake Tor control port), plus an
   opt-in real-network e2e test like `internal/e2e/tor_test.go`.

Nothing in `protocol`, `peers`, `pairing`, `api`, or `cmd/sol` needs to change.

## Using the signed service record (directory, Nostr)

A public `http` service or site with `record: true` serves a signed record at
`/.well-known/sol.json` (`internal/record`). A directory checker or a discovery channel should
fetch it from one of the service's addresses, call `record.Verify`, and require
`Record.Reaches(thatAddress)`. Anything that republishes records (a Nostr event, `hosts.json`)
can carry the bytes as they are: the signature covers them. New fields go into `record.Record`
with a new `Version`.

## Adding discovery (Nostr, a DHT)

Peers' addresses are remembered values, not part of their identity (that's the ed25519 key). A
discovery or rendezvous channel can publish a node's current addresses, signed with its node key,
and peers can update `Peer.Addrs` after checking the signature against the stored public key
(`peers.Peers.Update`). Pairing and the handshake don't change.

## Adding a runtime provider (Docker is next)

The stub is [`internal/runtime/docker.go`](../internal/runtime/docker.go).

1. Implement `runtime.Provider` (`Name`, `Available`, `Start` → `*Proc`).
2. Add the name to the `switch` in `runtime.Select` and to `knownProviders` in `internal/config`.

Transports never know which provider they got.

## Adding an audit check

1. Implement `audit.Check` (`Name`, `Run(ctx, env, svc) Result`). Return `StatusSkip` with a
   reason when a check can't run, never a fake pass.
2. Replace the matching `planned{...}` entry in `audit.Checks`, or append to it.

The CLI (`sol audit`) and the panel's Leak check card pick it up automatically.

## Adding a panel screen or API route

1. Add a method to `api.Backend` and implement it on `*node.Node` (there's a compile-time
   assertion `var _ api.Backend = (*Node)(nil)`).
2. Register the route in `api.Handler`. State-changing methods are CSRF-checked automatically.
3. Add the UI to `web/index.html` and `web/app.js`. **Always pass API values through `esc()`**
   before putting them in HTML.

## Adding a setting users can change

Add one entry to `optionDefs` in `internal/config/options.go` (key, label, help, kind, whether it
needs a restart, and get/set functions). `sol config` and the panel's Settings screen pick it up.
Never add a secret there.

## Adding a config field

1. Add the field to `config.Config` (or a nested struct) with a `json` tag.
2. Give it a default in `Default()` and fill zero values in `applyDefaults()`, so old config
   files keep working.
3. Validate it in `validateLocked()`.
4. If the change is incompatible, bump `CurrentVersion` and add a step to `migrate()`.

## Adding a built-in service

Built-ins are services a node serves itself over a verified stream (`_inbox`, `_files`). Add the
name to `peers.Builtins`, serve it in `node.handleConnect` (use `serveHTTPConn` for HTTP: the
handler then knows exactly which peer is connected), add it to `node.peerInfo`, and give it a label
in `web/app.js` and `cmd/sol` (`label`). Decide whether new peers get it by default
(`peers.DefaultShares`; the safe answer is no).

## The gateway (v0.2)

The design is in the package doc of [`internal/gateway`](../internal/gateway/gateway.go) and
spec.md 7.4. The pieces already exist: the gateway is a node, each browser session pairs as its
own peer (so each is listed, shared with, and revocable on its own), and `Node.DialPeer` opens an
authenticated stream, which is all it needs per browser request.

## Changing the protocol

Sol is pre-release: change the handshake in place, update [PROTOCOL.md](PROTOCOL.md) and the
tests, and re-pair. From the first public release on, follow "Versioning" in PROTOCOL.md (new magic
bytes, and branch on the ClientHello in `protocol.ReadRequest`).
