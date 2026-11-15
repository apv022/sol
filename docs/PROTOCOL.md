# Sol connection protocol

This is the contract between two Sol nodes. Every connection, in either direction, uses it: one
node (the caller) opens a stream to the other (the callee) over any transport and runs this
handshake. The implementation is [`internal/protocol`](../internal/protocol/protocol.go); how peers
are created and what they may open is in [PEERS.md](PEERS.md).

This page specifies **protocol version 1**, locked for v0.1.0. A locked version never changes;
a change is a new version, negotiated as described under Versioning.

## Goals

- **Only peers get through.** Everyone else is disconnected without a reply.
- **Any TCP service.** After the handshake the connection is a plain byte pipe, so HTTP,
  WebSockets, range requests, SSH, databases, and game servers all work unchanged.
- **No clocks, no replay cache.** Every connection gets a fresh random challenge.
- **Silent to scanners.** The caller speaks first, and the callee sends nothing to anything that
  doesn't open with the Sol magic bytes.
- **Predictable across versions.** Nodes on different Sol releases agree on the highest protocol
  version both speak, or each learns which side has to update, without the callee ever sending
  a rejection.
- **Transport-agnostic.** The handshake runs on any byte stream (Tor, I2P, clearnet, or a future
  direct path). Transport-specific data (addresses, access keys) only appears inside maps keyed by
  transport name.

## Message flow

```
caller                                   callee
  |  ClientHello  "SOLH" | min | max      |
  |-------------------------------------->|   anything else: close, send nothing
  |                                       |
  |  Hello  "SOLH" | min | max            |   no common version: only these 8 bytes, then close
  |         | callee_pub | nonce          |
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

`min` and `max` are 2-byte unsigned integers: the inclusive range of protocol versions the sender
speaks (this build: `1`..`1`). A range is valid when `1 <= min <= max`; a ClientHello with an
invalid range is treated like a non-Sol client (closed, nothing sent), and a Hello with one is
`ErrMalformed` to the caller. The first 8 bytes of both hellos (magic, `min`, `max`) are the
same in every protocol version, past and future. Everything after them is defined by the
negotiated version; this page describes version 1.

### Negotiation

Each side computes the same thing from the two ranges: the negotiated version is
`min(caller.max, callee.max)` if that is at least `max(caller.min, callee.min)`, otherwise there
is none.

- **A common version exists.** The callee sends its full Hello. The caller puts the negotiated
  version in `Request.v` and signs it together with both ranges (below). The callee checks that
  `v` is exactly the version it computed itself.
- **No common version.** The callee sends only the 8-byte Hello prefix and closes. That prefix is
  exactly what every Sol caller receives first anyway, so an incompatible caller learns nothing
  a compatible one doesn't (it learns less: no key, no nonce), and the callee still sends nothing
  that depends on who the caller is or what it asked for. The caller sees the callee's range and
  stops before sending a request, with `ErrIncompatible` saying which side is older:
  "alpha runs an older Sol (protocol 1; this node needs 2 or newer). Update Sol on alpha", or
  "... runs a newer Sol ... Update Sol on this machine".

Why the version travels in the hello and not in peer records or pairing cards: a peer's version
changes whenever it updates, without anything being re-exchanged, and the hello is the one
message that reaches the caller before any decision is made. Putting the callee's range in the
existing first bytes means the dialer can always tell "update Sol" apart from "rejected" without
the callee ever replying to a request it refuses (AGENTS.md rule 2).

### Request (JSON)

| Field | Type | Meaning |
|---|---|---|
| `v` | int | The negotiated protocol version (`1`) |
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
lp("sol handshake") | lp(versions) | lp(callee_pub) | lp(nonce) | lp(op) | lp(service) | lp(secret)
  | lp(card_json) | lp(direct_json)
```

`versions` is 10 bytes, five 2-byte integers: the negotiated version, the caller's `min` and
`max` (as in its ClientHello), and the callee's `min` and `max` (as in its Hello). Each side uses
what it sent itself and what it received, so if anyone on the path rewrote either hello (say, to
make both sides settle on an older version with a known weakness), the two sides disagree on
these bytes and the signature fails. `direct_json` is the `direct` map of a `list` request
(sorted keys), or empty.

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
it asks for current addresses (`list`, at most every 5 minutes). A direct address that failed
(a cancelled dial, or one whose deadline was already spent, doesn't count) is tried after the
other transports until it has proved itself again: the dialer probes it in the background after
a minute, doubling with each failure up to an hour, and prefers it again only after three
successful probes over at least two minutes (see "Liveness"). Peer-supplied candidates must be plain IP addresses (no names, loopback, link-local or
multicast). Only the node publication is carried: public and anonymous services are never
reachable directly.

## Liveness

The wire has no keepalive message: a `connect` stream is raw bytes after the OK. A node instead
checks the *path* each of its outbound streams uses:

- Every 30 s (±20% jitter), for each address that carries one of its live outbound streams to a
  peer, it sends an ordinary `list` request over exactly that address and reads the `PeerInfo`
  answer. The probe carries no `direct` field, so the callee changes nothing. A probe gets 10 s
  over a direct or LAN path and 60 s over Tor or I2P.
- Two failed probes in a row mean the path is dead: the dialer closes its streams over it, marks
  a direct path as failed (above), and dials the peer again over the best remaining path. A peer
  it can't reach is redialled with backoff from 2 s to 5 minutes, with jitter, while a saved
  tunnel wants it or for 30 minutes after its streams were cut.
- A network change reported by the OS, or a wall clock that ran ahead of the monotonic clock
  (the machine slept), makes all of this due at once (debounced 2 s). Outbound TCP streams whose
  local address is gone are closed straight away.
- Underneath, direct TLS and clearnet sockets use TCP keepalive (15 s idle, then 4 probes 5 s
  apart), so a stream whose path died also ends on the side that accepted it. A WireGuard
  tunnel sends a keepalive every 25 s while up; 80 s without receiving anything marks it down,
  closes its streams on both ends, and restarts punching.

A callee can't tell a probe from any other `list` request.

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
sol://pair?v=1&node=<node id>&name=<name>&s=<secret>&tor=<onion>&clearnet=<host:port>&auth.tor=<key>
```

`v` is the link format version (`1`) and is checked before anything else: a link with a higher
`v` fails with "this pairing link was made by a newer Sol; update Sol on this machine" whatever
the rest holds, and a link without `v` (from a pre-release Sol) or with a non-numeric one is
refused as invalid. The link format is versioned separately from the handshake, since it is read
by people and other apps (QR codes, the Android app) and changes for different reasons.
`node` and `s` are required, plus at least one address. Every parameter other than `v`, `node`,
`name` and `s` names a transport: `<transport>=<address>` or `auth.<transport>=<credential>`
(for example `direct=sol-direct:...`). The
link is a password: it is single use and expires after 10 minutes.

## Versioning

The protocol is versioned from v0.1.0, and version 1 is the format on this page. Rules:

- **Never change a version in place.** Any change to the bytes after the hello prefix, the
  request fields, the signed message, or the meaning of a reply is a new version number.
- **The hello prefix never changes.** `"SOLH" | min | max` is how every version, old or new,
  learns the other side's range. Don't change the magic again: it would make old nodes treat new
  ones as scanners, and they could not say "update Sol".
- **Support a range.** A release adds a version by raising `protocol.MaxVersion` and keeping the
  older one's code, and drops one only on purpose by raising `protocol.MinVersion` (say, a
  release or two later, or for a security fix). Code after the hellos branches on the negotiated
  version.
- **Additive JSON fields** that old nodes can ignore (unknown fields are ignored) still need a new
  version if they are signed, because the signed message is fixed per version.
- **Pairing links** carry their own `v` (above); a new link format raises it.

A dialer that finds a peer incompatible logs it once (again only if the peer's range changes),
shows it on the peer ("alpha runs an older Sol; update Sol on alpha"), fails user dials at once
with that message (no retries, no other paths), and the liveness supervisor redials such a peer
only at its slowest pace (5 minutes), so the peer's update is noticed without a restart. The
callee logs an incompatible caller at most once an hour per version range; it can't say which
peer it was, since nothing about the caller is authenticated before the request.
