# sol/1 connection protocol

This is the contract between a Sol client (`sol connect`, and later the gateway) and a Sol node.
The implementation is [`internal/protocol`](../internal/protocol/protocol.go). Any change here is a
breaking change and needs a new version.

## Goals

- **Only paired devices get through.** Everyone else is disconnected without a reply.
- **Any TCP service.** After the handshake the connection is a plain byte pipe, so HTTP,
  WebSockets, range requests, SSH, databases, and game servers all work unchanged.
- **No clocks, no replay cache.** Every connection gets a fresh random challenge.
- **Silent to scanners.** The client speaks first, and the node sends nothing to anything that
  doesn't open with the Sol magic bytes.

## Message flow

```
client                                   node
  |  ClientHello  "SOL1"                  |
  |-------------------------------------->|   anything else: close, send nothing
  |                                       |
  |  Hello  "SOL1" | node_pub | nonce     |
  |<--------------------------------------|   node_pub: 32-byte ed25519 key
  |                                       |   nonce:    32 fresh random bytes
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
| `v` | int | Protocol version, `1` |
| `op` | string | `connect`, `pair`, or `list` |
| `service` | string | Target service for `connect` |
| `name` | string | Device label for `pair` |
| `secret` | string | One-time pairing secret for `pair` |
| `pubkey` | string | Device ed25519 public key, standard base64 |
| `sig` | string | ed25519 signature, standard base64 (see below) |

### Signature

The device signs this byte string, where `lp(x)` is a 2-byte length followed by `x`:

```
lp("sol/1 handshake") | lp(node_pub) | lp(nonce) | lp(op) | lp(service) | lp(name) | lp(secret)
```

Covering `node_pub` and `nonce` means a signature works for exactly one connection to exactly one
node. Covering every field means a man in the middle cannot change the target service or the
pairing data. Length prefixes stop fields from being shifted into one another.

### Node decision

| op | Accepted when | Reply after `0x01` |
|---|---|---|
| `pair` | `secret` is a valid, unexpired, unused token (it is burned) | `ServiceList` JSON frame, then close |
| `list` | `pubkey` belongs to a paired device | `ServiceList` JSON frame, then close |
| `connect` | `pubkey` is paired **and** `service` exists **and** the node can reach it | nothing. The stream becomes the service |

`ServiceList` is `{"node_id", "device_id", "services": [{"name", "protocol"}]}`.

In every other case the node closes the connection without sending anything. The client sees a
rejection (`ErrRejected`) and cannot tell why. The node records the reason in its local log, with
no secrets.

### Client checks

The client must compare `node_pub` against the node ID from the pairing link and abort on a
mismatch (`ErrWrongNode`). Over Tor this is belt-and-braces, because the onion address already
authenticates the node.

## What this protocol does not do

- **No encryption of its own.** Confidentiality comes from the transport: Tor and I2P encrypt end
  to end. The clearnet transport is unencrypted and meant for LANs and tests. Someone on that path
  can read the traffic and could relay a connection, though they cannot forge a device's signature.
- **No per-request authorization.** Once a `connect` succeeds, the pipe is trusted until it
  closes. Revoking a device closes its live connections immediately (the node tracks them per
  device), and removing a service closes the connections to it.

## Layering with Tor client authorization

On Tor there is a second, outer lock. The node's onion is published with `ClientAuthV3` keys for
every paired device and every pending pairing token. Without the matching x25519 private key, a
client can't even fetch the onion descriptor, so the address is unreachable as well as refusing
connections. The private key travels in the pairing link (`auth=`) and is installed by the client
with `ONION_CLIENT_AUTH_ADD`. Revoking a device removes its key and republishes the onion.

## Pairing link

```
sol://pair?v=1&node=<node id>&tor=<onion>&lan=<host:port>&auth=<x25519 priv, base64>&s=<secret>
```

`node` and `s` are required, plus at least one address (`tor`, `i2p`, or `lan`). The link is a
password: it is single use and expires after 10 minutes.

## Versioning

A future `sol/2` must change the magic bytes (for example `SOL2`) so that old nodes reject new
clients silently rather than misparsing them. Nodes may support several versions by branching on
the ClientHello.
