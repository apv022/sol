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

### Default transports and where tor comes from (built in C10)

A new setup (`sol init`, or plain `sol` on a fresh machine) turns on **WireGuard direct, TLS
direct and Tor** together: `wg` on a free UDP port, `direct` on the TCP port of the same number,
and `tor`. Path ranking (§Liveness and healing) picks the best per peer, so nothing else needs
choosing. `sol init --transport` is repeatable (or comma-separated) to pick a different set;
existing setups change theirs with `sol transports list|enable|disable <name>`, `POST
/admin/v1/transports/{name}` `{enabled}`, the `sol.transports.*` actions or the switches in the Sol
app's Settings → Advanced (kept in `config.json`; the last transport stays on). Turning one off
stops it and re-signs the endpoint record without its address.

Tor never holds up startup: the daemon is usable on WireGuard and TLS while tor launches and
bootstraps; the endpoint record keeps the last known onion meanwhile and gains it again when tor
has registered it, and is pushed to every peer then. A machine without any tor runs without it
(the transport shows `failed` with the reason).

The tor program comes from the first of (`internal/runtime`):

1. **the platform's**: `SOL_TOR_BIN`, else `tor_bin` in `config.json` (`sol init --tor-bin
   <path>`). It runs without a checksum check, because the platform already verified it: Android
   sets `SOL_TOR_BIN` to the signed APK's `lib/<abi>/libtor.so`. Its `--version` is logged. A
   named file that is missing is an error, not a silent fallback.
2. **bundled** (desktop release builds, `make build-bundled`): tor 0.4.9.16 from the Tor
   Browser 16.0.1 expert bundles, fetched by `scripts/fetch-tor.sh`, which checks every signature
   against the Tor Browser key `EF6E286DDA85EA2A4BA7DE684E2C6E8793298290` (fetched by WKD into a
   throwaway `GNUPGHOME`) and every tarball against the signed `sha256sums-unsigned-build.txt`.
   tor and its shared libraries (Linux: libcrypto, libssl, libevent; macOS: libevent) are
   embedded, extracted to `<data dir>/bin/<os>-<arch>/`, each checked against the SHA-256 pinned
   in `internal/runtime/bin/checksums.json` (an unpinned or changed file refuses the whole
   bundle), and run with that folder on `LD_LIBRARY_PATH` (`DYLD_LIBRARY_PATH` on macOS). If it
   can't run, the next provider does, and why is logged.
3. **native**: a tor on `PATH` (development: `apt install tor`).

### Tor client authorization (built in C6b)

A device's onion lists one ClientAuthV3 key per device allowed to reach it, and Tor serves its
descriptor to nobody else, so a stranger who learns the address can't even tell it is up. The keys
are derived, never sent: for owner device `O` and client device `C`,

```
shared = X25519(x(D_O), x(D_C))   x() = the Ed25519 key in Montgomery form (both sides can compute it)
key    = X25519 private key SHA-256("sol tor access v1" | O_pub | C_pub | shared)
```

`O` lists the public half, `C` installs the private half (`ONION_CLIENT_AUTH_ADD`) before dialing.
The endpoint record's `access` for the onion is `x25519-dh1`, which says only that: a record is
seen by everyone it is relayed to, so it must never carry a per-peer secret. The set on `O`'s onion
is: every device in my device list (including `O` itself, so an onion with nobody else on it is
still closed), every device in the newest list of every contact, and one key per pending pair or
join secret, `SHA-256("sol tor pair v1" | secret)` as an X25519 private key, which whoever holds
the link derives too (and `sol restore --from <link>` uses). It is recomputed whenever my device
list, a contact's device list, the contacts list or the pending secrets change, and the onion is
re-added (`DEL_ONION` + `ADD_ONION` with the same service key, so the same address) only when
the set really changed. Tor is never sent `SIGNAL NEWNYM` (it drops installed client-auth keys).
Removing a contact or a device takes its keys off at once; their tor still holds a key that no
longer matches anything.

## Handshake

All integers big-endian. A frame is `u16 len | bytes` (1..16384); a bundle frame, which carries an
identity bundle, is `u32 len | bytes` (1..1 MiB). The handshake must finish within 45 seconds.

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
| `op` | `session`, `pair` (add a person), `join` (add my device), `recover`, `probe` (check a path) |
| `device` | caller's device public key `D_a` (base64) |
| `nonce` | caller's 32-byte nonce `nonce_a` (base64) |
| `secret` | one-time secret, `pair` and `join` only |
| `name` | `join` only: the joining device's display name (optional) |
| `person`, `rsig` | `recover` only: `R_pub`, and R's signature of `T` |
| `sig` | caller's signature (below) |

The identity bundle frame follows (IDENTITY.md §The identity bundle), binary encoded.

### Signatures

Transcript, with `lp` as in IDENTITY.md:
```
T = lp("sol handshake v2") | versions(10 bytes, as in v1) | lp(D_a) | lp(D_b)
    | lp(nonce_a) | lp(nonce_b) | lp(op) | lp(secret) | lp(sha256(bundle_a))
    [ | lp(name) for join, lp(R_pub) for recover ]
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
| `probe` | as `session` | Accept, then close: the path works, and the live session is left alone |

Every other case: close without sending anything (rule: never send bytes to a rejected
connection). The reason goes to the local log.

The caller checks that `D_b` is the device it meant to reach (from the pairing link or the cached
bundle) and that `sig_b` verifies; otherwise it closes.

### Settled in C3 (and gaps for C3b)

- **Layouts.** `T` uses the raw 10 version bytes. Accept is `0x01 | bundle frame | 64-byte sig_b`.
  The Request JSON must be canonical (sorted keys, no extra whitespace).
- **Bundle frames** use a 4-byte length (max 1 MiB), not the 16 KiB frame limit, so a large device
  list fits. *(Built in C3b.)*
- **Join.** A joining device has no bundle yet: it sends its signed endpoint record in the bundle
  frame, and the Request carries `name` (the device's display name) for `join`, covered by `T`.
  *(Built in C7.)*
- **Recover.** A device restoring from the words has no bundle, so it can't fetch the newest one
  through a normal handshake. Op `recover`: the caller (a throwaway device key) presents its
  endpoint record like `join`, and the Request carries `person` = `R_pub` and `rsig`, R's
  signature of `T`. A callee that is that person's device or holds them as a contact accepts as
  usual, then writes `u32 len | person cert | u32 len | device list` (the newest it holds) and
  closes; anyone else closes silently. `sol restore --from <link>` uses it to start its seqs
  above that state. *(Built in C7.)*
- **Pushing changes.** Whenever a device list or endpoint record changes, the device pushes it to
  every open session (`POST /p/bundle`), so contacts learn of a removed device immediately, not at
  their next handshake. *(Built in C3b.)* Body: one or more `u32 len | bundle` records; each
  verifies on its own against its endpoint record's device. A peer may push its own person's
  bundles; only my own devices may push my person's, or relay a contact's.
- Contacts are replicated between my own devices through the internal feed `sol/contacts` (C4).
  Their **bundles are relayed** (C3b): whatever a device learns that is new about a contact (in a
  handshake or a push from them) it pushes to my other devices, and a device of mine that comes up
  gets every contact bundle the other holds. So my phone can reach a friend first met on my
  laptop. A relay may arrive before the contacts feed does; it is cached (the cache grants
  nothing) and the friend is dialed once they are a contact there too. Relays are not relayed
  again, and only news is, so nothing loops.
- **Persistence.** Every verified bundle that was news is kept in `peers.json` (0600, atomic) and
  folded back into the identity cache, oldest first, when the daemon starts. *(Built in C3b.)*

## Pairing links

```
sol://pair?v=3&k=<base64url(payload)>
sol://join?v=3&k=<base64url(payload)>

payload = R_pub (32) | D_pub (32) | secret (32) | [flags] | up to two addresses
flags   = 0x06 | u8 (bit 0: anonymous; written only when non-zero)
address = 0x01 | onion v3 public key (32)
        | 0x02 | IPv4 (4) | port (2)      a candidate of the direct (TLS) transport
        | 0x03 | IPv6 (16) | port (2)
        | 0x04 / 0x05                     the same for clearnet (tests)
```
`pair` adds a person (both become contacts). `join` adds a device to *my* identity. Secrets are 32
random bytes, valid once, for 10 minutes. Shown as a QR code and as text.

A link carries only what first contact needs, so it is at most 200 characters (a QR code a phone
scans at a glance): the onion if there is one (it works from anywhere) and the best IPv4 address
that still fits (same room, same Wi-Fi), or without an onion the two best IP addresses. The
addresses are unsigned hints; the handshake checks that whoever answers is `D_pub` speaking for
`R_pub`, and brings the full signed bundle, endpoint record included. The onion's checksum is
recomputed from the key. A pending secret opens the onion (§Tor client authorization).

An **anonymous** pair link (§Anonymous contacts) sets the anonymous flag and carries only the
onion: the redeemer treats the inviter as an anonymous contact too (Tor only both ways, its
handshake bundle and every later one onion-only, the inviter's non-Tor dials refused). A field or
flag this build does not know is refused as made by a newer Sol, so new fields need no new
version.

Version 2 links (full endpoint records and the inviter's name, about 600 characters) are refused
with "made by an older Sol"; a link with a higher version says to update Sol.

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
| `POST /p/bundle` | any (see §Settled in C3) | push bundles: my changed bundle, or a contact's relayed between my devices; 204 |
| `GET /p/profile` | self, contacts | my latest profile (IDENTITY.md §Profile) as `u32 len \| profile` records: a buddy gets the full form, an anonymous contact the form without the avatar, my own devices both; 404 when I never set one |
| `POST /p/profile` | self, contacts | push profiles (same body): my changed profile, or (between my devices) a contact's, relayed like bundles; 204. Pushed to every open session when it changes here (set here, or learned from a sibling), and fetched by the other side when a session comes up; a sibling that comes up also gets every contact profile, after their bundles |
| `POST /p/profile/edit` | self | a device of mine without P asks an admin device to sign a change, JSON `{name?, avatar? (base64 bytes), clear_avatar?}`; answers the new records (as `GET /p/profile` to self), 409 on a device without P |
| `GET /p/avatar/{hash}` | self, buddies (never anonymous contacts) | a picture: to a buddy only my current or previous avatar; to my own devices any picture this device holds (mine and contacts', so a relayed contact profile shows its picture); 403 is decided before looking it up |
| `POST /p/sync` | self, contacts | feed replication: body is the list of feeds + `have` seqs; the response streams missing entries, then new ones live |
| `GET /p/blobs/{hash}` | per API.md §4 rule; self always | blob bytes, `Range` supported |
| `POST /p/actions/{name}` | per sharing + scopes | run an action on this device's app, caller headers attached |
| `POST /p/dial` `{app, path}` | self, contacts (the app decides by the caller headers; `path` never under `/_sol/`) | upgrade to a raw stream into that app |
| `GET /p/service/{name}` | per sharing | upgrade to a raw stream into a shared local port (port sharing) |
| `POST /p/services` | self, contacts | the services this device offers the caller, `{"services":[{name,http}]}`; pushed when the session comes up and whenever the list changes (port sharing) |
| `GET /p/ui/{app}/{path}` | per sharing (any feed of the app shared with the caller; self always) | the app's UI, caller headers attached, for `http://<app>.<petname>.localhost` (API.md §10); `/_sol/` is never passed |
| `POST /p/unpair` | contacts | the caller unpaired me: I drop them as a contact too |
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
fork-free without coordination. A device stamps a new entry after every entry of the feed it
already holds (`unix_ms` is at least the newest one + 1, even if its own clock is behind), so an
entry written knowing another one sorts after it; blob releases (API.md §4) rely on this. The
wire format is unchanged: `release` is an ordinary body field that each daemon reads from the
entries it stores.

### `/p/sync` format (settled in C4)

Request JSON: `{"feeds": {"<address>": {"<device>": <have seq>}}, "all": <bool>}` (`all` is used
between my own devices: every feed). Response: newline-delimited JSON messages: `feed` (readable
with mode, or no longer readable), `entry` (a signed entry; time in unix ms), then one `live`
once the backlog is sent, then live `entry`/`feed` messages as they happen. Shares and contacts
are themselves internal feeds (`sol/shares`, `sol/contacts`) replicated between my devices.
What contacts share with me is the internal feed `sol/incoming`; pins are `sol/pins`.

## Blobs on the wire

`GET /p/blobs/{hash}` with `Range`. Large blobs are fetched in 4 MiB ranges, in parallel from every
online device that has them (`HEAD` first), verified as a whole by SHA-256 when complete; ranges
are cached so an interrupted fetch resumes.

### Blob headers (settled in C5)

`HEAD|GET /p/blobs/sha256:<hex>` answers with `Sol-Blob-Size`, `Sol-Blob-Ranges: a-b,c-d` (bytes
held, inclusive, merged), `Sol-Blob-Complete: true|false`, `Accept-Ranges: bytes` and
`ETag: "sha256:<hex>"`. 206 for a single range from complete or partial data; 200 only for a
complete blob; 416 (`Content-Range: bytes */size`) for a range out of bounds, not held, or
multi-part; 403 is decided before looking the blob up, so it reveals nothing about what I hold.
Anchors pull pinned blobs themselves when the replicated `sol/pins` feed changes.

## Liveness and healing

Built in C3b (`internal/session/paths.go`, `push.go`, `internal/netwatch`), ported from the PoC's
dialer and liveness supervisor. One session per pair of devices; if both dial at once, the session
opened by the device with the lower ID wins and the other is closed; between two sessions dialed
by the same side, the newer wins (the older is likely dead).

- **Ranking.** A device's endpoints are tried in order: WireGuard, TLS direct, clearnet (LAN,
  tests), relay, Tor, I2P. Among the healthy ones, direct paths come first, then the path that last
  worked, then transports that are ready right now. A path that failed goes behind every healthy
  one (a last resort). Pair and join skip WireGuard: the callee can't know this device's key yet.
- **Hedged dialing.** The best path starts at once; the next starts `HedgeDelay` (2 s) later, or
  as soon as the one before fails; the first handshake to complete wins. Only one attempt at a
  time may send its Request (after the Hello), so a losing attempt is abandoned before the callee
  could complete a second session. An attempt that started before the winner and hasn't answered
  is slow and counts as failed. A refusal or garbage on one path doesn't stop the others; a bad
  signature, an incompatible version or the wrong person does. On Tor, the transport also hedges
  its own descriptor fetch (HSFETCH, `internal/transport/tordial.go`).
- **Backoff and probation.** A failed path backs off (1 min, doubling to 1 h). While a session
  this device dialed runs on a worse path, its keeper probes the better one (op `probe`) when the
  backoff ends; it is preferred again only after 3 probes in a row succeeded over at least 2 min,
  and then the session moves to it (a new session over that path replaces the old one). Any
  failure meanwhile doubles the backoff, so a flaky path drifts towards being tried rarely
  instead of flapping. A healthy better path the session doesn't use (a new endpoint, a lost
  hedge) is probed a minute after the session came up. Only the dialing side moves a session.
- **Redial.** A kept peer whose session died is redialed with backoff (2 s to 5 min, jittered).
  The mux pings every 20 s and closes a session after three missed pongs.
- **Network changes and sleep.** `internal/netwatch` watches the wall clock against the
  monotonic clock (a jump of over 20 s means the machine slept), the kernel's route netlink
  socket on Linux and Android (address, link and default-route changes), and elsewhere scans the
  interface addresses every 5 s; bursts collapse after 2 s. A change, or `sol network-changed`,
  tells the transports (Tor, WireGuard re-check themselves), closes sessions whose local address
  is gone, checks every other session with a request (closed if no answer within 10 s, 60 s over
  Tor), makes every path backoff and redial due now, and re-signs the endpoint record if this
  device's published addresses moved (again 5 s later, once transports have relearned them),
  which pushes it to every peer.
- **What it chose.** Every session records its path and why (`{transport, inbound, reason}`:
  "best path", "direct failed: connection refused; tor answered", "direct was slower; tor
  answered", "direct is stable again"), and each peer device's troubled paths (`backoff` or
  `probation`, failures, last error, next probe); the admin API's `peer_sessions` shows both for
  the Sol app's Advanced view.

## Nearby (LAN discovery, built in C12)

Endpoint records only travel over sessions, so two devices whose LAN addresses changed while they
were apart (a new Wi-Fi, a new DHCP lease), or that share a network with no internet, would never
find each other. Nearby fixes that with a small multicast **beacon** that only my own devices and
my buddies' devices can recognise. A beacon grants nothing: a match only hands the session layer
a candidate address, and the handshake authenticates whoever answers as usual.

**Where.** UDP to `239.255.83.79:7748` (IPv4, administratively scoped), TTL 1 (never routed off
the link), sent from and received on every interface that is up, multicast-capable, not loopback,
not a container or VM bridge, and has a routable IPv4 address. Multicast loopback is on, so two
Sol devices on one machine see each other. Where the interfaces can't be listed (Android 11+), one
socket on the default interface. Not IPv6 yet: on an IPv6-only link the source of a link-local
multicast is `fe80::/10`, which the transports refuse as a peer candidate (addresses carry no
zone); IPv4 covers every home and office LAN today.

**When.** At start, at every network change (and again 5 s later, once DHCP and the transports
settle), every 30 s ±20 %, and once (at most every 5 s) when a beacon matched a device this
device has no session with, so the other side learns this device's address at once too. Nothing
is ever sent to the source of a beacon; a beacon is answered only by this device's own beacon to
the group.

**Who can recognise it.** For every pair of devices, both derive a key nobody else can, from the
device keys, as for Tor client authorization:

```
lo, hi = the two device keys, sorted bytewise
K      = SHA-256("sol nearby v1" | lo | hi | X25519(x(D_me), x(D_peer)))
w      = floor(unix seconds / 300)                       the time window
out    = HMAC-SHA256(K, "beacon" | D_sender | u64(w))
entry  = out[0:8] | (ports XOR out[8:16])                16 bytes
ports  = u16 wg | u16 direct | u16 clearnet | u16 0      this device's listening ports (0: off)
```

A beacon has one entry for each device this device keeps a session with: my own devices and the
devices of every buddy, never an anonymous contact (§Anonymous contacts), never anyone not paired.

```
datagram = "SOLN" | u8 version (1) | u8 n | n entries
```

`n` is a multiple of 8 (random entries pad it) and at most 64; more devices take several
datagrams. Entries are shuffled. There is nothing else: no public key, name, device ID or
counter.

**Receiving.** A receiver precomputes, for every device it keeps that is not an anonymous
contact's, the 8-byte tags for windows `w-1`, `w` and `w+1` (clocks may differ by a few minutes),
recomputed when the window or the set of kept devices changes. Each entry is one map lookup. On a
match it unmasks the ports (the last two bytes must be zero), and gives the session layer, as an
unsigned hint for that one device, the beacon's source address with each advertised port, for
each transport that device's newest signed endpoint record has: `wg` and `direct` get the address
prepended to the record's candidates (the record's keys stay), `clearnet` (tests) gets an extra
endpoint. Hints last 3 minutes (beacons renew them). The device's keeper is woken: without a
session it redials now; on a worse path (Tor) the better one is probed and the session moves as
in §Liveness and healing. Malformed datagrams, sources that are not unicast IPv4 (loopback only in
tests) and anything over 50 datagrams a second are dropped without a word.

**What an observer on the network learns, and what it doesn't.**

- That a Sol device is there (the port and the `SOLN` magic), and roughly how many devices it
  keeps (`n`, rounded up to 8). Discovery can't hide that something is being discovered.
- Not who it is: tags are HMAC outputs under keys only the two devices of a pair hold, and change
  every window, so beacons can't be linked across windows (or networks) without a key. Within one
  5-minute window a device's beacons repeat, as its IP and MAC address do anyway.
- Not who knows whom: tags are directed (`D_sender` is in the input), so the entry A sends for B
  never equals the one B sends for A, and two devices' beacons have nothing in common. Once they
  connect, the connection between their addresses is visible, as any direct path is.
- Not the ports in a recognisable form: they are masked per pair and window, so stable listening
  ports can't link a device across networks.
- **Replay** is the residual risk: someone on the same link can rebroadcast a captured beacon
  within about 10 minutes (the window tolerance); a device that recognises it dials the replayer,
  which tells the replayer that a device related to the original sender is on its network (and
  shows that dial's usual first bytes). It can't make anyone accept anything: the handshake still
  needs the right device key.

**Who is never told.** An anonymous contact gets no entry (it could match nothing) and its
devices' beacons are never matched (they aren't in the table), so I never dial them on a LAN.
Removing a contact or a device drops its entry at the next beacon: the keys are per pair and
derived, so there is nothing to revoke or rotate.

**The switch.** On by default; `"nearby": false` in `config.json` turns it off, as do `sol do
sol.nearby --state off`, `sol transports disable nearby` and `POST /admin/v1/transports/nearby`
`{enabled}` (the Sol app's Advanced view). Off closes the sockets: nothing is sent or received.
It also stays quiet when no transport listens on a non-loopback address (nothing on the LAN could
reach this device). `sol transports list` and `sol status` show it as the `nearby` row: `up` with
the interfaces and how many kept devices were seen in the last 3 minutes, `off`, or `idle` and why.

**Platforms.** Standard library only: a `net.ListenMulticastUDP` socket per interface (it binds
the wildcard address, so datagrams that arrive on several sockets are deduplicated), plus
`IP_MULTICAST_LOOP` set per OS (Linux and Android, macOS and the BSDs, Windows, where it is a
receive-side option); other systems report nearby as unavailable. On Android the shell holds a
`WifiManager.MulticastLock` while the node runs and nearby isn't off in its config (many Wi-Fi
drivers drop multicast without it), released when the node stops.

## Anonymous contacts (built in C10)

Per person, I choose how much of my network a contact may see. **Buddies** (every contact by
default) and my own devices get my full endpoint record and are dialed WireGuard first, then TLS
direct, Tor last. An **anonymous** contact is Tor only, both ways, and never learns an address of
mine that isn't an onion:

- **Invites.** `sol invite --anonymous` (the Anonymous switch in Add a person, `POST invite`
  `{anonymous: true}`) issues a pair link that carries only this device's onion (an error while it
  has none; never a join link). Its secret is marked anonymous: redeeming it over anything but an
  anonymous transport is refused without burning it, and whoever redeems it is added to my
  contacts feed with `anonymous: true`. The link carries the anonymous flag (§Pairing links),
  so the redeemer adds me with `anonymous: true` as well and sends the onion-only form of its
  bundle already in the pairing handshake: Tor only, both ways. *(Built in C10b.)*
- **Records.** Every bundle of mine an anonymous contact receives (handshake, push, `GET
  /p/bundle`, probe) carries the onion-only form of my endpoint record: the same person
  certificate and device list, and an endpoint record with only the `tor` (and `i2p`) entries,
  signed by the same device key with the same `seq` as the full one. A contact only ever gets one
  of the two forms of a `seq`. My devices never relay their own or each other's bundles to
  contacts (relays go to my own devices only), so each of my devices tells an anonymous contact
  only its own onion, and only itself.
- **Dialing.** I dial an anonymous contact over anonymous transports only (their record is
  filtered before ranking), never tell WireGuard their key (so nothing punches towards them) and
  never probe a direct path. Nearby beacons carry no entry for them and never match theirs
  (§Nearby).
- **Refusing.** The onion's target is a listener of its own; a session with an anonymous contact
  is admitted only on it. Their dial on any other transport (they may know an old address) is
  refused at the callee decision like every refusal: the connection closes unanswered, and no
  bundle of mine is sent on it.
- **Switching.** The flag lives in my contacts feed (`anonymous` on the contact's record), so
  all my devices agree; `sol people set <name> --anonymous|--buddy`, `PATCH people/{id}`
  `{anonymous}`, the `sol.people.set` action or the person page's switch change it. Each device
  that sees a change (made there or replicated) closes that contact's sessions that no longer fit
  (direct ones, when they become anonymous), updates WireGuard, and re-signs its own endpoint
  record with a higher `seq`, which is pushed in the form each peer may see: the newest record an
  anonymous contact holds is always onion-only. Switching to anonymous warns that any IP address
  they already saw can't be taken back. The other side is not told; to them I am just a contact
  who is reachable over Tor.

The e2e test (`internal/e2e/transports_test.go`) records every byte an anonymous contact
receives over (fake) Tor and asserts no non-onion address of any of my devices is in it, before
and after my addresses change, and that its direct dial is refused; `TestAnonymousBothWays`
checks the same in both directions for the inviter and the redeemer.

## Versioning

The first 8 bytes of both hellos never change. Any change to what follows is a new version number;
old versions' code stays until it is deliberately dropped, which is a documented clean break.
