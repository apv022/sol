# Extending Sol

Every planned feature has a seam in the code already, so adding it should not require changing
the rest of the system. This page says where each seam is and what to touch.

After any change, run `make check` (gofmt, vet, staticcheck, race tests). If you touch the
handshake, pairing, routing, or revocation, the e2e tests in `internal/e2e` must still pass. Add a
case there for the new behaviour.

## Adding a transport (I2P is next)

The stub is [`internal/transport/i2p.go`](../internal/transport/i2p.go); its comment has the plan.

1. Implement `transport.Transport`:
   - `Start` launches the program through a `runtime.Provider` (see Tor's `launch`) and returns
     once `Publish` can be called. Report progress through `Status`.
   - `Publish(ctx, Publication)` exposes `p.Target` under the stable `p.ID` and returns the
     public address. Persist one key per ID so addresses survive restarts. Publishing the same
     ID again must replace the earlier publication.
   - `Handles(addr)` claims the addresses this transport can dial (for example `.i2p`).
   - `Dial` reaches a public address. `dialSOCKS5` in `socks.go` is reusable.
   - If the network has access control like Tor client auth, honour `p.ClientAuth`, and implement
     `transport.ClientAuthorizer` for the client side.
2. Construct it in `node.buildTransports` (node side) and `connect.DefaultTransports` (client side).
3. Make sure the pairing link carries the address: `Node.NewPairing` maps status names to link
   fields, and `pairing.Link` already has an `I2P` field.
4. Add a `transport_test.go` case with a fake daemon (see the fake Tor control port), plus an
   opt-in real-network e2e test like `internal/e2e/tor_test.go`.

Nothing in `protocol`, `auth`, `pairing`, or `api` needs to change.

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

## Adding a config field

1. Add the field to `config.Config` (or a nested struct) with a `json` tag.
2. Give it a default in `Default()` and fill zero values in `applyDefaults()`, so old config
   files keep working.
3. Validate it in `validateLocked()`.
4. If the change is incompatible, bump `CurrentVersion` and add a step to `migrate()`.

## The gateway (v0.2)

The design is in the package doc of [`internal/gateway`](../internal/gateway/gateway.go) and
spec.md 7.4. The pieces already exist:
- `config.DeviceKindGatewaySession` for per-browser devices.
- `connect.Client.Dial(ctx, service)` opens an authenticated stream, which is all the gateway needs
  per browser request.
- The node already treats every device the same way, so per-session revocation works.

## Changing the protocol

Don't change `sol/1` in place. Follow "Versioning" in [PROTOCOL.md](PROTOCOL.md): new magic bytes,
and branch on the ClientHello in `protocol.ReadRequest`.
