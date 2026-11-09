# Sol connection protocol

This is the contract between two Sol nodes. Every connection, in either direction, uses it: one
node (the caller) opens a stream to the other (the callee) over any transport and runs this
handshake. The implementation is [`internal/protocol`](../internal/protocol/protocol.go); how peers
are created and what they may open is in [PEERS.md](PEERS.md).

Sol is pre-release, so this format changes in place. From the first public release on, any change
needs a new magic (see Versioning).

## Goals

- **Only peers get through.** Everyone else is disconnected without a reply.
- **Any TCP service.** After the handshake the connection is a plain byte pipe, so HTTP,
  WebSockets, range requests, SSH, databases, and game servers all work unchanged.
- **No clocks, no replay cache.** Every connection gets a fresh random challenge.
- **Silent to scanners.** The caller speaks first, and the callee sends nothing to anything that
  doesn't open with the Sol magic bytes.
- **Transport-agnostic.** The handshake runs on any byte stream (Tor, I2P, clearnet, or a future
  direct path). Transport-specific data (addresses, access keys) only appears inside maps keyed by
  transport name.

## Message flow

```
caller                                   callee
  |  ClientHello  "SOL1"                  |
  |-------------------------------------->|   anything else: close, send nothing
  |                                       |
  |  Hello  "SOL1" | callee_pub | nonce   |
  |<--------------------------------------|   callee_pub: 32-byte ed25519 key
  |                                       |   nonce:      32 fresh random bytes
  |  Request  u16 len | JSON              |
  |-------------------------------------->|
  |                                       |   verify signature, then decide
  |  0x01  (+ optional u16 len | JSON)    |
  |<--------------------------------------|   or close with nothing sent
  |                                       |
  |========= raw bytes both ways =========|   (op "connect" only)
```

All integers are big-endian. A frame is a 2-byte length (1 to 8192) followed by that many bytes.
The whole handshake must finish within 45 seconds.

### Request (JSON)

| Field | Type | Meaning |
|---|---|---|
| `v` | int | `1` |
| `op` | string | `connect`, `pair`, `list`, or `unpair` |
| `service` | string | Target service for `connect` |
| `secret` | string | One-time pairing secret for `pair` |
| `card` | object | `pair` only: the joiner's `{name, addrs, access}` (below) |
| `pubkey` | string | Caller's ed25519 public key, standard base64 |
| `sig` | string | ed25519 signature, standard base64 (see below) |

The card introduces the joining node: `name` (1-64 printable characters), `addrs`
(transport → address, e.g. `{"tor": "….onion"}`), and `access` (transport → the credential the
joiner issued for the inviter, e.g. the Tor client-auth private key).

### Signature

The caller signs this byte string, where `lp(x)` is a 2-byte length followed by `x`:

```
lp("sol handshake") | lp(callee_pub) | lp(nonce) | lp(op) | lp(service) | lp(secret) | lp(card_json)
```

`card_json` is `encoding/json`'s encoding of the card (deterministic: struct order, sorted map
keys), or empty when there is no card. Covering `callee_pub` and `nonce` means a signature works
for exactly one connection to exactly one node. Covering every field means a man in the middle
cannot change the target service, the secret, or the joiner's address and key. Length prefixes
stop fields from being shifted into one another.

### Callee decision

| op | Accepted when | Reply after `0x01` |
|---|---|---|
| `pair` | `secret` is a valid, unexpired, unused token (it is burned) and a card is present | `PeerInfo` frame, then close |
| `list` | `pubkey` belongs to a peer | `PeerInfo` frame, then close |
| `connect` | `pubkey` belongs to a peer **and** `service` is in that peer's shares **and** the service is reachable | nothing. The stream becomes the service |
| `unpair` | `pubkey` belongs to a peer (which is then removed) | nothing, then close |

`PeerInfo` is `{"node_id", "name", "peer_id", "services": [{"name", "protocol"}]}`: who the callee
is, the caller's id as the callee knows it, and what the callee shares with the caller.

In every other case the callee closes the connection without sending anything. The caller sees a
rejection (`ErrRejected`) and cannot tell why. The callee records the reason in its local log,
with no secrets.

### Caller checks

The caller compares `callee_pub` against the node ID it expects (from the pairing link or the peer
record) and aborts on a mismatch (`ErrWrongNode`). Over Tor this is belt-and-braces, because the
onion address already authenticates the callee.

## Built-in services

`connect` can also name a built-in service, which the callee serves itself instead of a local port:

| Service | Protocol over the stream |
|---|---|
| `_inbox` | HTTP. `PUT /files/{name}` with the file as the body → `201 {"hash", "size"}`. Requests carrying `Origin` or `Sec-Fetch-Site` are refused, so browsers can't be used to send |
| `_files` | HTTP. The shared-files page `/` and `/blob/<sha256>[/<name>][?download]` |

## Direct mode transport

With `transports.direct` on, a node also listens on `direct_listen` (default `:7701`) and
publishes the address

```
sol-direct:<node id>@<host:port>,<host:port>,...
```

listing its usable IPs (LAN IPv4, global IPv6, a tailnet address; container bridges and
link-local are skipped; at most 8). A dialer:

1. connects to every candidate at once (`DirectDialTimeout`, 3 s) and keeps the first that
   succeeds;
2. sends the preamble `SOLD1` + the first 16 bytes of `SHA-256("sol direct" | node public key)`.
   The listener closes, sending nothing, on anything else, so only someone who already knows
   which node they want ever sees its certificate;
3. runs TLS 1.3. The server certificate is self-signed with the **node's ed25519 key**, and the
   dialer accepts only the key named in the address (no CA, no trust on first use);
4. runs the normal handshake above inside TLS.

Direct addresses change with the network, so `PeerInfo` carries the callee's current one
(`"direct": "sol-direct:..."`, or `""` when off), and the caller replaces its record. When a
dialer reaches a peer over another transport although the peer has a direct address, it asks
for the current one (`list`, at most every 5 minutes). A direct address that just failed is
tried last for a minute, doubling with each failure up to an hour, so a dead path doesn't slow
every connection. A peer may only announce a direct address for its own node key; anything else
is dropped. Only the node
publication is carried: public and anonymous services are never reachable directly.

## What this protocol does not do

- **No encryption of its own.** Confidentiality comes from the transport: Tor and I2P encrypt end
  to end, and direct mode uses TLS 1.3 pinned to the node key. The clearnet transport is unencrypted and meant for LANs and tests. Someone on that path
  can read the traffic and could relay a connection, though they cannot forge a node's signature.
- **No per-request authorization.** Once a `connect` succeeds, the pipe is trusted until it
  closes. Removing a peer closes its live connections in both directions immediately, and
  unsharing a service closes the peer's connections to it.

## Layering with Tor client authorization

On Tor there is a second, outer lock. Each node's onion is published with one `ClientAuthV3` key
per peer (and per pending pairing token). The private half is given to that peer only: in the
pairing link (`auth.tor=`) for the joiner, and in the joiner's card for the inviter. It is
installed with `ONION_CLIENT_AUTH_ADD`. Without it, a node can't even fetch the onion descriptor.
Removing a peer drops its key and republishes the onion.

## Pairing link

```
sol://pair?node=<node id>&name=<name>&s=<secret>&tor=<onion>&clearnet=<host:port>&auth.tor=<key>
```

`node` and `s` are required, plus at least one address. Every parameter other than `v`, `node`,
`name` and `s` names a transport: `<transport>=<address>` or `auth.<transport>=<credential>`
(for example `direct=sol-direct:...`). The
link is a password: it is single use and expires after 10 minutes.

## Versioning

Until the first public release, the format changes in place and everyone re-pairs. After that, a
breaking change must use new magic bytes (for example `SOL2`) so that old nodes reject new callers
silently rather than misparsing them. Nodes may support several versions by branching on the
ClientHello.
