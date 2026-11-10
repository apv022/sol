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

## Direct mode

With `transports.direct` on, peers connect straight to each other when they can and over Tor
when they can't. Two transports carry it, tried in this order:

1. **WireGuard** (`wg`, UDP `direct_listen` port, default 7701): the official `wireguard-go`,
   in userspace with its own network stack (no root, no TUN device). It punches through NATs.
2. **TLS** (`direct`, TCP, same port number): for networks that block UDP.

### WireGuard

Each node has a WireGuard key (`wg.key`) and, inside the tunnel, the address
`fd53:4f4c:` + the first 96 bits of `SHA-256(WireGuard public key)`. Its address is

```
sol-wg:<base64url WireGuard public key>@<host:port>,<host:port>,...
```

The candidates are the public address (learned on WireGuard's own socket, so it is the mapping
WireGuard's packets use), then LAN IPv4, global IPv6 and other addresses (at most 8).

The public address comes from **peer reflection**: every 2 minutes (every 15 s while it has
none) a node sends each paired peer's candidates a query, `SOLQ` + its 32-byte WireGuard public
key. A node answers only keys of its paired peers, at most once a second each, with `SOLR` +
the source address it saw, as text (`198.51.100.7:7701`, `[2001:db8::1]:7701`). The asker
accepts a reply only from an address it queried in the last 5 s, and only a public address (not
private, CGNAT or loopback). So a peer with a public IP or a forwarded port tells the others
their mapping, and no server outside the user's own machines is involved. If, and only if, the
user lists STUN servers in config (`stun`: presets `cloudflare`, `google`, `mozilla`,
`twilio`, or `host:port`), the node also asks them (RFC 5389 Binding requests). With neither,
direct paths form only on the same network, over IPv6 or a tailnet, or to forwarded ports.

Making a path:

1. Peers exchange WireGuard addresses over the authenticated channels: the pairing link and card,
   `PeerInfo.wg` in answers, and the caller's `direct` map in every `list` request (signed with
   the rest of the request). Each side adds the other to its WireGuard device: WireGuard then
   answers that key and nothing else, and pins it to its tunnel address (AllowedIPs).
2. Both sides send **punch packets** (`SOLP` + their 32-byte WireGuard public key, filtered
   out before WireGuard sees them) to each other's candidates. They open each NAT towards the
   other and tell the receiver where the sender really appears from.
3. Only the side with the **lower WireGuard key** starts handshakes: two simultaneous
   initiations cancel each other. It starts one when a punch arrives (to the address it came
   from), and blindly from the second round on. The other side answers punches with a punch.
4. Once a handshake completes, keepalives (25 s) hold the NAT mappings open and WireGuard
   follows the peer's address as it moves (roaming).
5. Inside the tunnel the caller connects to `[peer tunnel address]:7700` and runs the normal
   handshake above, which still checks the node key.

A punch from a peer we think is connected means its side lost the session (it restarted or
moved): the initiator drops the stale session and handshakes again (at most every 10 s, since
punch packets are unauthenticated). A dial waits up to 3 s for a handshake; until then Tor
carries the connection, and a refresh over Tor swaps fresh addresses so the next one goes direct.
Pairing never tries WireGuard (the inviter can't know the joiner's key yet). Symmetric NATs
can't be punched; those peers stay on Tor. Nothing is relayed through third parties.

### TLS

```
sol-direct:<node id>@<host:port>,<host:port>,...
```

A dialer connects to every candidate at once (3 s) and keeps the first that succeeds; sends the
preamble `SOLD1` + the first 16 bytes of `SHA-256("sol direct" | node public key)` (the
listener closes, sending nothing, on anything else, so only someone who already knows which node
they want ever sees its certificate); runs TLS 1.3, where the server certificate is self-signed
with the node's ed25519 key and the dialer accepts only the key named in the address; then runs
the normal handshake inside TLS.

### Both

Peers learn each other's current addresses in `PeerInfo` and `list` requests, and a peer may only
announce a TLS address for its own node key. When a dialer reaches a peer over another transport,
it asks for current addresses (`list`, at most every 5 minutes). A direct address that just
failed is tried last for a minute, doubling with each failure up to an hour (a cancelled dial, or
one whose deadline was already spent, doesn't count), unless WireGuard reports the tunnel live
again. Peer-supplied candidates must be plain IP addresses (no names, loopback, link-local or
multicast). Only the node publication is carried: public and anonymous services are never
reachable directly.

## What this protocol does not do

- **No encryption of its own.** Confidentiality comes from the transport: Tor and I2P encrypt end
  to end, and direct mode uses WireGuard, or TLS 1.3 pinned to the node key. The clearnet transport is unencrypted and meant for LANs and tests. Someone on that path
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
