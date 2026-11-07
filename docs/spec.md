# Sol: Spec

> **Status:** this is the original product and design spec. It is still the source of truth for
> goals, use cases, visibility modes, the threat model, and the roadmap. Where the implementation
> deliberately differs, the newer documents win:
>
> | Spec section | Superseded by | Change |
> |---|---|---|
> | 6.2 Transport interface | [ARCHITECTURE.md](ARCHITECTURE.md) | `Publish` takes a `Publication` (ID, target, client-auth keys) so each public service gets its own onion; added `Unpublish` and `Handles` |
> | 6.3 Wire protocol | [PROTOCOL.md](PROTOCOL.md) | Per-request HTTP signing replaced by one signed challenge-response handshake per TCP connection. Works for any TCP service, needs no clock sync or replay cache |
> | 6.5 Service proxy | [ARCHITECTURE.md](ARCHITECTURE.md) | Private services are a raw TCP pipe; the HTTP reverse proxy is used only for public `http` services |
> | 6.2.1 Runtime providers | [ARCHITECTURE.md](ARCHITECTURE.md) | `native` (tor on PATH) is the default; `bundled` needs `-tags bundled` |
> | 15 Repo layout | [ARCHITECTURE.md](ARCHITECTURE.md) | Current layout |

**One line:** Sol is a single-binary node that lets anyone self-host private services reachable from anywhere over Tor, I2P, or clearnet, with no port forwarding, no VPN, and no accounts.

## 1. Problem

Self-hosting means fighting ports, CGNAT, DNS, certificates, and VPN setup. Sol removes all of it: install one binary, click "Add service", scan a QR, and the service works from any device.

## 2. Principles

1. **Zero network config.** The node only makes outbound connections.
2. **Private by default.** Unpaired clients get nothing, not even an error.
3. **Transport-agnostic.** Tor, I2P, and clearnet are interchangeable pipes behind one interface.
4. **Few dependencies.** Go standard library, plain HTML/CSS/JS, Tor and i2pd as external processes.
5. **Honest threat model.** Sol inherits anonymity from Tor and I2P. It does not invent a new anonymity network.

## 3. Non-goals (all versions)

- A new anonymity or routing network.
- Hiding the user from a malicious gateway they chose to trust (the strict mode in Section 13 reduces this).
- Replacing Tor Browser or I2P for general browsing.

## 4. Use cases and solutions

| # | Use case | Who | Pain today | Sol solution | Mode | Ships |
|---|---|---|---|---|---|---|
| 1 | Private self-hosting (music, photos, passwords, notes) | Home users | Ports, DNS, TLS, VPN setup | Add service, scan QR, open from any device | `private` | MVP |
| 2 | Share a service with family or friends | Home users | Giving out VPN configs or accounts | One QR per person, each device individually revocable | `private` | MVP |
| 3 | Host from behind CGNAT or a changing IP | Anyone on home or mobile internet | Can't accept inbound connections at all | Node only dials out through Tor/I2P, so no inbound needed | all | MVP |
| 4 | Open a Tor/I2P service from a plain phone browser (needs a gateway) | Non-technical users | Needs Tor Browser or an I2P router | Gateway turns HTTPS into Tor/I2P, nothing to install | `private`, `public` | v0.2 (deferred from MVP) |
| 5 | Same service on Tor, I2P, and clearweb | Self-hosters | Three separate setups | One config, one identity, three addresses | all | MVP |
| 6 | Stream personal media | Audiophiles, collectors | Needs a VPS or exposed home server | Blob store with range requests plus Navidrome proxy | `private` | MVP |
| 7 | Verify what you downloaded | Anyone sharing files | Trust the host | Content hashing, same file means same hash on every transport | all | MVP (Should) |
| 8 | Find hosts and public services | Everyone | Scattered lists, dead links, no trust signal | Known-hosts directory with signed entries and live status | n/a | MVP (Should) |
| 9 | Publish a public site without exposing your IP | Bloggers, activists, researchers | Hosting providers know who you are | Anonymous Public Mode: hidden origin, egress locked, separate gateway | `anonymous-public` | MVP demo, v0.5 full |
| 10 | Dark web hosting with less pain | Onion/eepsite operators | Hand-editing torrc and tunnels.conf, key backups, leaks | Panel, templates, leak audit, one-click publish to both networks | `anonymous-public` | v0.2 |
| 11 | Stay online when the home machine is off | Anyone | Sleep, power cuts, ISP drops | Replicate to your other devices, then community pinning | all | v0.5 / v1.0 |
| 12 | Resist takedown of a single endpoint | Publishers | One domain or host gets removed | Many interchangeable gateways, same address on three networks | `public`, `anonymous-public` | v1.0 |
| 13 | Run apps you don't fully trust | Node operators | Hosting strangers' code is dangerous | WASM sandbox with capability permissions | hosted apps | v1.0 |
| 14 | Publish a music library | Audiophiles | Sharing means uploading to someone else's platform | Rip Galore publishes into your node, Subsonic clients play it | `private`, `public` | v1.0 |

### Visibility modes

| Mode | Who can reach it | Origin IP exposure | Gateway | Network-layer auth |
|---|---|---|---|---|
| `private` | Paired devices only | None | Optional, user's own | Yes |
| `public` | Anyone | None if outbound-only (see Section 7.6 for guarantees) | Optional | No |
| `anonymous-public` | Anyone | None, enforced (egress lock, audit) | Must be on a different machine and network | No |

## 5. Glossary

| Term | Meaning |
|---|---|
| Node | A running Sol instance on the user's machine |
| Service | A local app (e.g. Navidrome on port 4533) that the node exposes |
| Device | A paired client with its own ed25519 key |
| Transport | A pipe that carries traffic (Tor, I2P, clearnet) |
| Gateway | A Sol instance in `gateway` mode that gives plain browsers access over HTTPS |
| Directory | Public web page listing known gateways, relays, and opt-in services |
| Pairing | One-time exchange that authorizes a device |

## 6. Architecture

```
Phone browser ──HTTPS──> Gateway ──Tor/I2P──> Node ──localhost──> Service   (v0.2, not in MVP)
Laptop (sol connect) ──────────Tor/I2P──────> Node ──localhost──> Service
```

Components, all in one Go binary with three modes:

| Mode | Command | Role |
|---|---|---|
| `node` | `sol` | Runs services, control panel, transports, auth |
| `connect` | `sol connect <link>` | Local tunnel for a paired device (opens `localhost:9000`) |
| `gateway` | `sol gateway` | HTTPS front door that proxies for browsers. **Deferred to v0.2.** |

### 6.1 Identity

- Node keypair: ed25519, generated on first run, stored in `~/.sol/node.key` (mode 0600).
- Node ID: base32 of the public key. Used as the stable name for the node.
- Each device has its own ed25519 keypair. The node stores only the public keys.

### 6.2 Transport interface

```go
type Transport interface {
    Name() string                              // "tor", "i2p", "clearnet"
    Start(ctx context.Context) error
    Publish(localAddr string) (string, error)  // returns public address
    Dial(ctx context.Context, addr string) (net.Conn, error)
    Status() TransportStatus                   // ready, building, failed
    Stop() error
}
```

- **Tor:** control port (`ADD_ONION`) or a mounted `torrc` with a `HiddenServicePort`. Dial via SOCKS5.
- **I2P:** `i2pd` server tunnel in `tunnels.conf`. Dial via SOCKS or HTTP proxy.
- **Clearnet:** plain HTTPS listener, used for LAN and testing.

A node can run any combination. The panel shows each transport's status and address.

#### 6.2.1 Runtime providers: bundled, native, or Docker

Tor and i2pd are C/C++ programs, so Sol cannot link them as Go libraries. "Bundling" means shipping the official prebuilt binaries with Sol and running them as child processes. Each transport gets its binary through a **provider**, tried in this order:

| Order | Provider | How it works | Status |
|---|---|---|---|
| 1 | **Bundled** | Binaries embedded with `go:embed` (or in a `bin/` folder beside Sol), extracted to `~/.sol/bin/` on first run. Sol writes the config and starts them. | **MVP (demo platform only)** |
| 2 | **Native** | Uses `tor` or `i2pd` found on `PATH` | Optional fallback |
| 3 | **Docker** | Runs `tor` and `i2pd` as containers (`container_runtime` is `docker` or `podman`) | Optional fallback. Docker is still used for Navidrome and the egress-lock demo. |

All three expose the same thing to the rest of Sol: a SOCKS port, a control or tunnel config, and a start/stop lifecycle. Switching providers is a config change, not a code change.

**Hackathon: bundled binaries from the start.** The demo runs on one known machine, so the MVP bundles Tor and i2pd for that platform only and runs them as child processes. No Docker is needed for Tor or i2pd. The native and Docker providers sit behind the same interface and are added only if time allows or the bundled binary fails on the day. Multi-platform bundles come in v0.2.

**Bundling plan:**
- Source Tor from the Tor Expert Bundle and i2pd from its official releases. Verify checksums and signatures at build time. (MVP: download the binaries for the dev machine by hand and check their checksums once.)
- Per-platform builds (Linux, macOS, Windows on amd64 and arm64) in v0.2. MVP covers one platform. Check i2pd availability per OS, and fall back to the native provider where no binary exists.
- Binary size grows by tens of MB.
- Update path: bundled Tor and i2pd go stale, so releases track upstream security fixes and the panel shows the bundled versions.
- The Anonymous Public Mode egress lock needs containers or network namespaces, so Docker stays in use for that demo step even though Tor and i2pd are bundled.
- Lifecycle: the runtime layer picks a free SOCKS and control port, writes `torrc` and `tunnels.conf` under `~/.sol/`, starts each process, waits until it is ready, restarts it on crash, and stops it when Sol exits.

### 6.3 Wire protocol

> Superseded by [PROTOCOL.md](PROTOCOL.md). Kept for history.

HTTP/1.1 over the transport, with these additions:

- Request header `X-Sol-Key`: device public key (base64).
- Request header `X-Sol-Ts`: unix timestamp (reject if more than 60 s off).
- Request header `X-Sol-Sig`: ed25519 signature over `METHOD \n PATH \n TS \n SHA256(body)`.
- Node verifies the key is on the allowed list and the signature is valid. Otherwise it **closes the connection with no response**.
- Replay protection: remember `(key, ts, sig)` for 2 minutes.

### 6.4 Content addressing

- File blobs are stored at `~/.sol/blobs/<sha256>`.
- `GET /blob/<sha256>` serves the file with HTTP range support, so audio and video stream and seek.
- Same file means same hash on every transport.

### 6.5 Service proxy

- Config entry: `name`, `local_port`, `visibility` (`private` | `public`), `transports`.
- Authenticated requests are forwarded with Go's `httputil.ReverseProxy` to `127.0.0.1:<port>`.
- WebSocket upgrade supported (needed by many apps).

### 6.6 Config

`~/.sol/config.json`. Everything has a default and the app works without editing it.

```json
{
  "panel_port": 8080,
  "transports": { "tor": true, "i2p": true, "clearnet": false },
  "services": [
    { "name": "navidrome", "local_port": 4533, "visibility": "private" }
  ],
  "devices": [
    { "name": "phone", "pubkey": "...", "added": "2026-11-07T12:00:00Z", "kind": "gateway-session" }
  ]
}
```

## 7. Security model

### 7.1 Two layers of access control

1. **Network layer (Tor client authorization, v3 onion).** Each paired device or gateway holds an x25519 key. The node writes its public half to `authorized_clients/`. Without the private half, a client cannot decrypt the service descriptor, so the service is invisible. I2P gets the equivalent via encrypted leasesets where the i2pd version supports it.
2. **App layer (ed25519 signatures).** Section 6.3. Works on every transport, including ones with no native client auth.

Revocation removes the device from both lists.

### 7.2 Control panel safety

- Panel binds to `127.0.0.1` only.
- CSRF token on all state-changing API calls.
- Panel is never exposed through a transport or the gateway.

### 7.3 Pairing

1. Panel generates a one-time secret (32 random bytes, expires in 10 minutes, single use).
2. Pairing link: `sol://pair?node=<id>&tor=<onion>&i2p=<b32>&s=<secret>`. For browsers: `https://<gateway>/pair#<secret>`. The secret is in the fragment so it never reaches server logs.
3. The device generates its keypair, sends `{pubkey, device_name, secret}`.
4. Node validates the secret, stores the pubkey, burns the secret.

### 7.4 Gateway trust (deferred to v0.2)

Not built in the MVP. Kept here so the design is settled when gateways are added.


- The gateway is itself a paired device of the node.
- For each browser, the gateway generates a **session keypair** and stores it server-side. The browser holds only an opaque session cookie (`HttpOnly`, `Secure`, `SameSite=Strict`).
- The node records the session public key as a device of kind `gateway-session`, so each phone is individually listed and revocable.
- **The gateway can read traffic it proxies.** The recommended setup is a gateway the user runs themselves.

### 7.5 Threat model

| Threat | Handled by |
|---|---|
| Stranger finds the address | Tor client auth makes it undiscoverable. App layer drops anything unsigned. |
| Stolen phone | Revoke the device in the panel. Key is removed from both layers. |
| Network observer | Tor/I2P encryption plus end-to-end signing. |
| Malicious relay or exit | No exit nodes used (hidden services). Content integrity checked by hash. |
| Origin IP leak (anonymous-public) | Egress lock, separate gateway, `sol audit` (Section 7.6) |
| Malicious gateway | Not defended in the hackathon build. Use your own gateway. Strict mode (Section 13) mitigates. |
| Compromised service app | Out of scope. Services run as ordinary local processes. WASM sandbox in v1.0 covers hosted apps. |

### 7.6 Anonymous Public Mode

**MVP scope:** Tor and I2P visitors only. Clearweb visitors need a gateway, which arrives in v0.2.

**Goal:** a service open to all of Tor, all of I2P, and the clearweb, where no public machine ever learns the origin's IP.

```
Tor Browser ──> .onion ─────────────┐
I2P browser ──> .b32.i2p ───────────┼──> ORIGIN (hidden, outbound-only)
Normal browser ──> Gateway ──Tor/I2P┘
```

**Rules the node enforces** when `visibility = "anonymous-public"`:

1. No clearnet listener on the origin.
2. All egress from the service goes through Tor or I2P. The node refuses to start the service otherwise (container or namespace with no direct route out).
3. The gateway runs on a different machine and a different network from the origin. Never at the origin's home connection. (A gateway must accept public connections, so I2P cannot hide a gateway's IP.)
4. Dedicated keypair per anonymous service. Never reuse an identity linked to the user.
5. Response headers are stripped of server, host, and version data. Error pages are generic. EXIF is removed from uploads.
6. Tor `vanguards` protection enabled for the onion service.

**Leak map**

| Leak | Fix |
|---|---|
| Service makes outbound requests (updates, webhooks, DNS, link previews) from the real IP | Egress lock, DNS over Tor only |
| Gateway hosted at the origin's connection | Hard check: gateway and origin must differ, panel warns |
| VPS, domain, or TLS records tied to the operator | Separate identities and payments for gateway, or use community gateways |
| Metadata in files and headers | Stripping (rule 5) |
| Uptime correlation with the home connection | Replication to other nodes (v0.5) |
| Guard discovery against the onion service | Vanguards (rule 6) |

**`sol audit` command:** checks each rule and prints pass/fail. The panel shows the same result as a "Leak check" card. Proof for demos: `curl ifconfig.me` from inside the service container returns a Tor exit IP, never the home IP.

**Limits (state these in the docs):** Sol does not defend against a global adversary correlating traffic at both ends. Gateway operators carry legal and takedown exposure. Public anonymous hosting attracts abuse, so the content-policy tooling in Section 13 matters most here.

## 8. Control panel

Plain HTML/CSS/JS embedded with `go:embed`. No framework, no build step. JSON API under `/api`.

| Screen | Contents |
|---|---|
| Dashboard | Node ID, transport status (Tor ✅ / I2P ⏳), uptime |
| Services | List, add (template or port), visibility toggle, delete |
| Devices | List with last-seen, add (QR), revoke |
| Files | Upload, hash list, built-in `<audio>` player |
| Logs | Rejected connections, pairing events |

API:

```
GET  /api/status
GET  /api/services        POST /api/services       DELETE /api/services/:id
GET  /api/devices         POST /api/devices/pair   DELETE /api/devices/:id
GET  /api/files           POST /api/files
```

## 9. Known-hosts directory (website)

A public web page listing gateways, relays, and opt-in public services, so people can find where to host, where to connect, and what's alive. It makes Sol useful for both self-hosting and dark web hosting on day one.

### 9.1 What it lists, and what it does not

- **Lists infrastructure and opt-in services:** gateways, relays, community pinning hosts, and public services whose operators chose to be listed.
- **Does not crawl or index onion/I2P content.** It is not a search engine.
- **Federates with existing directories** as external links. Candidates: tor.taxi, tor.watch, notbob.i2p. Verify what each one actually is and its terms before listing it.

### 9.2 Entry format

Each entry is a JSON record, signed by the host's own key:

```json
{
  "name": "example-gateway",
  "kind": "gateway",
  "addresses": { "clearnet": "https://gw.example.com", "tor": "abc...onion", "i2p": "xyz...b32.i2p" },
  "pubkey": "base32...",
  "description": "Public gateway, no logs",
  "contact": "...",
  "tags": ["gateway", "no-logs"],
  "added": "2026-11-07"
}
```

- **Proof of control:** the host serves `/.well-known/sol.json` containing its pubkey, and signs the entry. The directory refuses entries it cannot verify.
- **`hosts.json`:** the whole list, signed by the directory key, so mirrors and clients can verify it and the page can run offline.

### 9.3 Page features

| Feature | Notes |
|---|---|
| Filter | By kind (gateway, relay, service) and by network (clearnet, Tor, I2P) |
| Live status | Green/yellow/red from periodic health checks over each transport |
| Copy / open | Copy address, or "open via gateway" for plain browsers |
| Submit | Pull request or signed JSON upload, no accounts |
| Report | Abuse and dead-link reports, with a takedown path |
| Mirrors | The page itself is served over clearweb, Tor, and I2P, using Sol |

### 9.4 Stack

Static HTML/CSS/JS plus `hosts.json`, no backend required. A small `checker` script (Go) updates status. No framework, no build step.

### 9.5 Policy

Listing criteria are published. Entries that serve illegal content are removed on report. The directory lists addresses, it does not host or mirror the content behind them.

## 10. Hackathon scope (MVP)

### 10.1 Decisions (locked)

| Decision | Choice |
|---|---|
| Language | Go, standard library only (plus an ed25519/x25519 helper from `golang.org/x/crypto`) |
| Dev environment | Native on own hardware, no VM. |
| Tor and i2pd | **Bundled binaries** for the demo machine only (Section 6.2.1), run as child processes by Sol |
| Primary transport | **Tor** |
| Second transport | **I2P**, started an hour before the demo so tunnels are built |
| Client access | **`sol connect`** on a second laptop (opens `localhost:9000`). Gateway is deferred to v0.2, so no plain-browser or phone access in the MVP. |
| Demo service | Navidrome in Docker (Docker is needed only for this and the egress-lock demo) |
| UI | Plain HTML/CSS/JS served from the binary |

### 10.2 Build order and time budget

| # | Task | Priority | Est. |
|---|---|---|---|
| 1 | Keygen, config load/save | Must | 1 h |
| 2 | Signed-request verification middleware | Must | 1.5 h |
| 3 | Service reverse proxy | Must | 1 h |
| 4 | Runtime provider (extract, configure, start bundled `tor`) and Tor transport (hidden service) | Must | 3 h |
| 5 | Pairing flow and device list | Must | 2 h |
| 6 | Control panel (status, services, devices, QR) | Must | 3 h |
| 7 | `sol connect` local tunnel (signs requests, dials Tor) | Must | 2.5 h |
| 8 | Blob store with range streaming | Should | 1.5 h |
| 9 | I2P transport (bundled `i2pd`, server tunnel) | Should | 2.5 h |
| 10 | Tor client authorization | Should | 1.5 h |
| 11 | Known-hosts directory page (static, seeded with 8-10 entries) | Should | 2 h |
| 12 | Anonymous Public Mode: egress-locked origin container and `sol audit` | Should | 2 h |

**Cut line:** if you are behind, drop 9 and 10 first, then 12, then 11. Tasks 1-8 are the demo.

### 10.3 Definition of done

- [ ] `sol` starts, generates keys, shows a Tor address in the panel.
- [ ] Adding Navidrome in the panel proxies it with no router changes.
- [ ] A second laptop on a different network (phone hotspot) pairs by pasting or scanning the link, runs `sol connect`, and plays music from `localhost:9000`.
- [ ] An unpaired client gets nothing.
- [ ] Clicking Remove kills the second laptop's access.
- [ ] (Should) The same file has the same hash when fetched over Tor and I2P.
- [ ] (Should) The directory page loads over clearweb and lists seeded hosts with live status.
- [ ] (Should) `sol audit` passes and `curl ifconfig.me` inside the origin container shows a Tor exit IP.

### 10.4 Demo script (2 minutes)

1. Show a laptop with no open router ports and Navidrome running locally.
2. Open the Sol panel. Add the Navidrome service. Show the Tor address.
3. On a second laptop (on a phone hotspot), run `sol connect <link>` from the pairing link. Open `localhost:9000`. Music plays.
4. Try the same address from a third machine without pairing. Nothing loads.
5. Click Remove on the second laptop. Reload. Access is gone.
6. (If built) Show the same file hash fetched over Tor and I2P.
7. (If built) Open the directory page, point at a seeded entry with its status light.
8. (If built) Flip a service to `anonymous-public`, run `sol audit`, then show `curl ifconfig.me` from the origin container returning a Tor exit IP.

### 10.5 Explicitly out of scope for the hackathon

DHT, relays, hole punching, native QUIC transport, directory federation and signed-entry verification (the MVP page is hand-curated), WASM sandbox, Rip Galore, Subsonic compatibility layer, multi-node replication, community pinning, mobile apps, multi-platform bundled binaries, and the gateway (so no phone or plain-browser access yet). These go on the roadmap slide.

## 11. Risks and mitigations (hackathon)

| Risk | Mitigation |
|---|---|
| I2P tunnels not ready at demo time | Start i2pd early. Tor is the fallback. |
| Tor slow on first connection | Pre-warm Tor. Use small test tracks. |
| Bundled `tor` or `i2pd` fails to start on the demo machine | Test on the demo machine early. Keep the Docker provider as a fallback if time allows. |
| Stale bundled versions | Pin and record the versions. Note in the pitch that an update path is planned. |
| Docker not running when needed for Navidrome or the egress-lock step | Start and test Docker before the event. These are the only Docker dependencies. |
| `sol connect` fails over Tor on the second laptop | Test on a different network by hour 8. Keep a clearnet transport as a fallback for the demo. |
| Navidrome WebSocket/stream issues through proxy | Test range requests early. |
| Directory lists a bad actor | Hand-curate the MVP list, publish listing criteria, add a report link. |

## 12. Roadmap

### v0.2 (after hackathon)
- **Gateway mode** (sessions, pairing page, HTTPS front door) so phones and plain browsers work with nothing installed.
- QR-scan mobile app.
- Service templates (Jellyfin, Nextcloud, Vaultwarden, Immich).
- Encrypted I2P leasesets everywhere supported.
- Panel template for one-click publish to Tor and I2P (dark web hosting without hand-editing config).
- **Multi-platform bundles** (Section 6.2.1): Linux, macOS, and Windows on amd64 and arm64, checksum verification in the release build, and an update path for bundled Tor and i2pd. Native and Docker stay as optional providers.

### v0.5: Connectivity and storage
- Native transport: QUIC with Noise encryption, for fast non-anonymous use.
- Connectivity ladder: outbound-only, UPnP/NAT-PMP/PCP, hole punching, relay fallback.
- Multi-device replication ("pin to my other devices").
- Cache and stream node roles.
- Address-advertisement record: signed list of a node's current transport endpoints.
- Anonymous Public Mode fully enforced: egress lock, header and EXIF stripping, vanguards, gateway-separation check.
- Directory: signed entries, proof of control via `/.well-known/sol.json`, automated status checks.

### v1.0: The decentralized web layer
- **DHT** for pubkey → endpoints discovery, with petnames and bookmarks.
- Sandboxed hosted services (WASM via wasmtime, capability-based permissions), so a node can run apps it doesn't trust.
- Community and paid pinning nodes for always-on availability.
- Directory federation with external lists and mirrors, so the client can pick any gateway from a verified list.
- Rip Galore: CD ripping (whipper, FLAC, AccurateRip, MusicBrainz) publishing straight into a node.
- Subsonic API compatibility so existing music apps work as clients.
- One-click installs for Umbrel, Unraid, Synology, Raspberry Pi, and Docker.

## 13. Final-version features

| Feature | Description |
|---|---|
| Strict mode | WebCrypto in-browser client so the gateway only sees ciphertext |
| Sol-native tunnels | Sol relays carry traffic over the native transport once enough nodes exist |
| Federation | Gateways and relays discoverable and replaceable, no hardcoded bootstrap |
| Content policy | Per-node allowlists and opt-in pinning so operators control what they host |
| Audit log | Signed, local, tamper-evident record of access |

## 14. Open questions

1. Sol-native name resolution: do we want a TLD-like suffix (`<pubkey>.sol`) in clients, and how does a plain browser resolve it through the gateway?
2. Does i2pd's encrypted leaseset support meet our needs, or do we require Java I2P for that feature?
3. Bundle the `tor` binary or arti? Arti is Rust, so it would ship as a separate binary too. Check its onion service client authorization support at build time. The `tor` binary is the safer default.
4. Licensing: AGPL (keeps forks open) or MIT/Apache (max adoption)?
5. Directory governance: who curates, how listings are disputed, and which external directories to federate with.
6. Project name: "Sol" is fine for now, but check for trademark collisions before publishing.

## 15. Repo layout

```
sol/
├── cmd/sol/main.go          # mode switch: node | connect | gateway
├── internal/
│   ├── identity/            # keys, signing
│   ├── transport/           # interface + tor.go, i2p.go, clearnet.go
│   ├── runtime/             # providers: bundled, native, docker; start/stop/ready
│   ├── auth/                # signature middleware, device list
│   ├── proxy/               # service reverse proxy
│   ├── blob/                # content store, range serving
│   ├── gateway/             # sessions, pairing endpoint
│   ├── audit/               # sol audit: leak checks for anonymous-public
│   └── api/                 # panel JSON API
├── web/                     # panel HTML/CSS/JS (go:embed)
├── directory/               # known-hosts site: index.html, hosts.json, checker/
├── docker-compose.yml       # navidrome and the egress-lock demo (optional tor/i2pd fallback)
├── bin/                     # bundled tor and i2pd (MVP: demo platform only, v0.2: all platforms)
└── SPEC.md
```
