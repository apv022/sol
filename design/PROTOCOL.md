# Wire protocol, version 2

Status: spec for tickets C3–C5. Builds on the PoC handshake (`~/solbak/docs/PROTOCOL.md`, version
1), keeping its good properties: silent to scanners, no clocks, fresh challenges, transport
agnostic, version negotiated in the first 8 bytes. New in v2: **both** sides prove their identity,
identities are people (via [IDENTITY.md](IDENTITY.md)), and a connection becomes a long-lived,
multiplexed **session** instead of one stream per request.

Clean break: this build speaks only version 2. A v1 node gets the standard "update Sol" answer
from the hello prefix, nothing more.

## Transports underneath

The handshake runs on any byte stream. Confidentiality comes from the transport:

| Transport | Encryption | Used for |
|---|---|---|
| LAN / direct TLS | TLS 1.3, self-signed cert whose key is the device key `D` (pinned, no CAs) | same network, or direct reachability |
| WireGuard direct | WireGuard (keys exchanged in endpoint records) | across NATs with hole punching |
| relay via own device | the relayed bytes are TLS end to end between the two devices | symmetric NATs |
| Tor | onion service with client authorization | fallback, always available |
| clearnet | none (tests only, refuses non-loopback unless forced) | the e2e tests |

## Handshake

All integers big-endian. A frame is `u16 len | bytes` (1..16384). The handshake must finish within
45 seconds.

```
caller                                         callee
 | ClientHello "SOLH" | u16 min | u16 max        |   min=max=2 in this build
 |---------------------------------------------->|   anything else: close, send nothing
 | Hello "SOLH" | min | max | D_b | nonce_b      |   no common version: 8-byte prefix, close
 |<----------------------------------------------|
 | Request frame (JSON) + identity bundle frame  |
 |---------------------------------------------->|   verify chain + sig, then decide
 | Accept: 0x01 | frame: identity bundle | sig_b |   or close with nothing sent
 |<----------------------------------------------|
 |============ session (mux) or result ==========|
```

`D_b` is the callee's 32-byte device public key; nonces are 32 random bytes.

### Request

| Field | Meaning |
|---|---|
| `v` | negotiated version (2) |
| `op` | `session`, `pair` (add a person), `join` (add my device) |
| `device` | caller's device public key `D_a` (base64) |
| `nonce` | caller's 32-byte nonce `nonce_a` (base64) |
| `secret` | one-time secret, `pair` and `join` only |
| `sig` | caller's signature (below) |

The identity bundle frame follows (IDENTITY.md §The identity bundle), binary encoded.

### Signatures

Transcript, with `lp` as in IDENTITY.md:
```
T = lp("sol handshake v2") | versions(10 bytes, as in v1) | lp(D_a) | lp(D_b)
    | lp(nonce_a) | lp(nonce_b) | lp(op) | lp(secret) | lp(sha256(bundle_a))
```
The caller signs `T` with `D_a`. The callee, if it accepts, signs `T | lp(sha256(bundle_b))` with
`D_b`. Both nonces and both device keys are covered, so each signature is valid for exactly one
connection between exactly these two devices, and neither side can be impersonated even on a
transport that doesn't authenticate the callee (unlike v1).

### Callee decision

| op | Accepted when | Then |
|---|---|---|
| `session` | the caller's verified person is a contact or me, and its device is in that person's current device list | the stream becomes a session |
| `pair` | `secret` is a valid, unused, unexpired `pair` token (burned on use) | both store each other as contacts, then a session |
| `join` | `secret` is a valid `join` token issued from an admin device | the admin device adds `D_a` to the device list, signs it, sends it, then a session |

Every other case: close without sending anything (rule: never send bytes to a rejected
connection). The reason goes to the local log.

The caller checks that `D_b` is the device it meant to reach (from the pairing link or the cached
bundle) and that `sig_b` verifies; otherwise it closes.

### Settled in C3 (and gaps for C3b)

- **Layouts.** `T` uses the raw 10 version bytes. Accept is `0x01 | bundle frame | 64-byte sig_b`.
  The Request JSON must be canonical (sorted keys, no extra whitespace).
- **Bundle frames** use a 4-byte length (max 1 MiB), not the 16 KiB frame limit, so a large device
  list fits. *(C3b: implement; today's code still uses the small frame.)*
- **Join.** A joining device has no bundle yet: it sends its signed endpoint record in the bundle
  frame, and the Request carries `name` (the device's display name) for `join`. *(C3b: add `name`.)*
- **Recover.** A device restoring from the words has no bundle, so it can't fetch the newest one
  through a normal handshake. New op `recover`: the caller additionally signs `T` with the
  recovery key `R`; an own device or a contact that verifies it answers with the newest bundle
  it holds, then closes. *(C3b.)*
- **Pushing changes.** Whenever a device list or endpoint record changes, the device pushes it to
  every open session (`POST /p/bundle`), so contacts learn of a removed device immediately, not at
  their next handshake. *(C3b.)*
- Contacts are replicated between my own devices through a Sol-internal feed once feeds exist
  (C4); until then each device keeps its own store.

## Pairing links

```
sol://pair?v=2&person=<R_pub id>&device=<D id>&s=<secret>&e=<endpoints, base64url>&n=<name>
sol://join?v=2&person=…&device=…&s=…&e=…
```
`pair` adds a person (both become contacts). `join` adds a device to *my* identity. Secrets are 32
random bytes, valid once, for 10 minutes. Shown as a QR code and as text.

## Sessions

After `0x01` the stream carries a **mux**: many independent bidirectional streams over one
connection, opened by either side.

Frame: `u32 stream_id | u8 type | u16 len | payload`. Types: `open`, `data`, `window` (grant
`u32` more bytes of receive window), `close` (half-close), `reset`, `ping`, `pong`. Odd stream IDs
are opened by the caller, even by the callee. Initial window 256 KiB per stream. A `ping` every
20 s; three missed `pong`s close the session and the reconnect logic takes over.

Each mux stream carries one HTTP/1.1 request and response (or an upgraded raw stream). So peer
calls are plain HTTP handlers, testable with the stdlib, in both directions.

### Session endpoints (served by every daemon to authenticated peers)

The caller's verified person, device and relation (`self` or `contact`) are known to every handler.

| Request | Who may call | Does |
|---|---|---|
| `GET /p/bundle` | any | latest identity bundle (to pick up rotations) |
| `POST /p/sync` | self, contacts | feed replication: body is the list of feeds + `have` seqs; the response streams missing entries, then new ones live |
| `GET /p/blobs/{hash}` | per API.md §4 rule; self always | blob bytes, `Range` supported |
| `POST /p/actions/{name}` | per sharing + scopes | run an action on this device's app, caller headers attached |
| `GET /p/dial/{app}/{path}` | per sharing | upgrade to a raw stream into that app |
| `GET /p/service/{name}` | per sharing | upgrade to a raw stream into a shared local port (port sharing) |
| `POST /p/inbox` | self, contacts | deliver an Inbox item |
| `GET /p/presence` | self, contacts | stream: online status, which apps are shared with the caller |

## Feeds on the wire

An entry is signed by the device key of the device that wrote it:
```
E = lp("sol entry v1") | lp(feed address) | u64(seq) | lp(prev hash) | u64(unix_ms)
    | lp(D_pub) | lp(app id) | lp(body: canonical JSON bytes)
sig = ed25519(D, E);  hash = sha256(E | sig)
```
A receiver accepts an entry only if: the device was in the owner's device list (at any `seq` it
has seen, unless the device was later removed **and** the entry's seq is after the removal
point it knows of), `seq` is exactly `last+1`, `prev` matches its last hash, and the signature
verifies. A gap means fetch the missing range first. Two different entries with the same `seq`
from the owner (a fork, e.g. two devices appending offline) are both kept: feeds are per
**device** under the hood (`<person>/<app>/<feed>@<device>`), and the API.md feed is the merge of
a person's device feeds ordered by `(unix_ms, device)`. This keeps every chain linear and
fork-free without coordination.

## Blobs on the wire

`GET /p/blobs/{hash}` with `Range`. Large blobs are fetched in 4 MiB ranges, in parallel from every
online device that has them (`HEAD` first), verified as a whole by SHA-256 when complete; ranges
are cached so an interrupted fetch resumes.

## Liveness and healing

Ported from the PoC: dial over the best transport first (LAN, WireGuard direct, TLS direct, relay,
Tor) with the PoC's backoff, probation and fall-back logic; a network change or wake from sleep
re-dials immediately; endpoint records are re-pushed whenever they change. One session per
pair of devices; if both dial at once, the session opened by the device with the lower ID wins
and the other is closed.

## Versioning

The first 8 bytes of both hellos never change. Any change to what follows is a new version number;
old versions' code stays until it is deliberately dropped, which is a documented clean break.
