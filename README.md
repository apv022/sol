# Sol

**Connect your own machines over Tor: open each other's apps, send each other files.**
No port forwarding, no VPN, no accounts, no domain, no cloud.

[![ci](https://github.com/apv022/sol/actions/workflows/ci.yml/badge.svg)](https://github.com/apv022/sol/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
![Go dependencies: 0](https://img.shields.io/badge/go%20dependencies-0-brightgreen)

```
home server (behind CGNAT, no open ports)           your laptop, anywhere
┌──────────────────────────────────────┐            ┌───────────────────────────┐
│ Navidrome :4533 ─┐                   │    Tor     │ sol open home             │
│ Jellyfin  :8096 ─┼─ sol ─ tor ───────┼────────────┼─ localhost:9000 → music   │
│ sshd      :22   ─┘   (outbound only) │  ◄──────►  │ sol send home notes.pdf   │
└──────────────────────────────────────┘  both ways └───────────────────────────┘
```

Sol is one Go binary that runs on each of your machines. Pair two of them once with a one-time
link, and they become **peers**: each can open the apps the other shares with it, and either can
send the other files, through Tor, from anywhere. Nobody else can even find them.

<!-- demo GIF: record with `vhs docs/demo.tape` and replace this comment -->

<p align="center"><img src="docs/media/panel-services.png" width="85%" alt="Sol panel: services with who can open them, and a public onion address"></p>

## Why

Self-hosting usually means fighting your router, CGNAT, dynamic DNS, TLS certificates, and VPN
configs, or handing your traffic to a tunnel company. Sol skips all of it:

- **Zero network config.** Sol only makes outbound connections. It works behind CGNAT, on hotel
  Wi-Fi, and on mobile hotspots.
- **Private by default.** A service is invisible to anyone you haven't paired: Tor client
  authorization hides the address, and the handshake drops strangers without a single byte of reply.
- **You choose what each peer sees.** A new peer can only send you files. Tick the apps it may
  open. Unpair a lost laptop and its connections close instantly, on both sides.
- **Files both ways.** `sol send laptop notes.pdf` from any machine, checked by SHA-256 on arrival.
- **Any TCP service.** HTTP, WebSockets, media streaming with seeking, SSH, game servers, databases.
- **Public too, if you want.** Flip a service to `public` and it gets its own `.onion` that anyone
  can open in Tor Browser. It can't be linked to your private node.
- **Small and auditable.** Standard-library Go, zero third-party Go dependencies, no build step for
  the UI.

## Quick start

You need `tor` installed on each machine (`sudo apt install tor`, `brew install tor`, …) and Go
1.24+ to build.

```sh
go install github.com/apv022/sol/cmd/sol@latest
sol                       # on every machine: starts the node; panel at http://127.0.0.1:8080
```

**Pair two machines** (once). On the home server, `sol invite` (or the panel's **Peers → Invite a
machine**) prints a link. On the laptop:

```sh
sol pair 'sol://pair?node=…'          # or paste it in the panel: Peers → Join with a link
```

**Share an app.** On the server:

```sh
sol service add navidrome 4533 --http --share laptop
```

(or in the panel: **Services → Add**, then tick it for the laptop on the **Peers** tab). On the
laptop, click **Open** next to it on the **Peers** tab, or:

```sh
sol open home
#   navidrome -> http://127.0.0.1:9000/
```

The tunnel belongs to the node, not the terminal: it stays open, comes back on the same port
after a restart (bookmark it), and heals by itself when the other machine reboots or changes
networks.

**Send files**, either way: `sol send home notes.pdf` (or **Send a file…** on the Peers tab).
They show up in the other machine's Files tab, marked with who sent them.

The panel walks you through it: a getting-started checklist, presets for common apps (Navidrome,
Jellyfin, Immich, Home Assistant, SSH…), a warning if the app isn't running, and a live view of
which peers are connected to what, each one cryptographically verified.

> Tor is slow to start the first time (around 30 s to bootstrap, plus up to a minute for a new
> onion to become reachable). Your onion address and Tor's state are kept between runs, so later
> starts take seconds to bootstrap and 10-60 s to become reachable again.

Try it without Tor on one LAN: set `"tor": false, "clearnet": true` in `~/.sol/config.json`.
That's for LAN and testing only, since clearnet traffic is not encrypted.

## How it works

1. Every node runs its own `tor` child process and publishes one **private onion** for itself,
   locked with Tor v3 client authorization.
2. **Pairing is mutual.** Each node has an **ed25519** identity. The link carries the inviter's
   address and an **x25519** Tor key made just for the joiner; the joiner answers with its own
   address and a key it made for the inviter, signed. The link is single use and expires in 10
   minutes.
3. Every connection, either way, begins with a **signed challenge-response handshake**: the callee
   sends a fresh nonce, the caller signs it with the service name, and the callee checks the key is
   a peer and the service is shared with it. Then it's a plain byte pipe to `127.0.0.1:<port>`.
4. **Unpairing** closes live connections on both sides and republishes the onion without the key.

Details: [docs/PEERS.md](docs/PEERS.md) · [docs/PROTOCOL.md](docs/PROTOCOL.md) ·
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) · [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md)

## Screenshots

| Panel home | Services (dark) |
|---|---|
| <img src="docs/media/panel-home.png" alt="Sol panel home: online over Tor"> | <img src="docs/media/panel-services-dark.png" alt="Sol services, dark theme"> |

## Commands

| Command | What it does |
|---|---|
| `sol` | Run the node (panel on `127.0.0.1:8080`, data in `~/.sol`). Everything below talks to it |
| `sol invite` | Print a one-time pairing link for another machine |
| `sol pair <link>` | Pair with the machine that made the link |
| `sol peers [-r]` | List peers and what each side shares (`-r` asks them now) |
| `sol services` | List this machine's services and who may open each |
| `sol service add <name> <port> [--http] [--public] [--share PEER]` | Add an app running on `127.0.0.1:<port>` (private unless `--public`) |
| `sol service remove\|public\|private <name>` | Remove a service, or change who can open it |
| `sol share <peer> <service> [--off]` | Let a peer open a service (`_files` = your shared files page) |
| `sol open <peer> [service…] [-L svc=PORT]` | Keep what a peer shares reachable on `127.0.0.1` (ports from 9000; saved, survive restarts) |
| `sol tunnels`, `sol close <peer> [service]` | List tunnels and whether the peer answers; close them |
| `sol send <peer> <file>…` | Send files to a peer |
| `sol unpair <peer>` | Remove a peer here and tell it to remove you |
| `sol name [NEW]`, `sol status` | Show or change this machine's name; show its id and addresses |
| `sol audit` | Leak checks for anonymous-public services |

`<peer>` is a name or the start of an id. Environment variables: `SOL_DATA` (data dir),
`SOL_DEBUG=1`.

## Status

Sol is **early**. It works and is tested, but it hasn't had an independent security review.

| Feature | Status |
|---|---|
| Tor onion services with client authorization | ✅ |
| Mutual peer pairing, per-peer sharing, instant unpairing on both sides | ✅ |
| Any TCP service; lasting tunnels from the panel's **Open** button or `sol open` | ✅ |
| Send files between peers, both ways, SHA-256 verified, with size limits | ✅ |
| Web panel: guided setup, invite/join, share toggles, live "connected and verified" status | ✅ |
| Public services on their own onion | ✅ |
| Shared files page for peers, streaming, SHA-256 fingerprints | ✅ |
| LAN / clearnet transport (testing) | ✅ |
| Release builds with tor bundled | 🚧 (`make build-bundled`, per platform) |
| Anonymous Public Mode (egress lock, EXIF stripping, full audit) | 🚧 partial. See the [threat model](docs/THREAT_MODEL.md) |
| I2P transport | 🗺️ stub in place ([how to add it](docs/EXTENDING.md)) |
| Gateway: open services in a normal phone browser | 🗺️ v0.2 |
| Known-hosts directory | 🗺️ |

The full roadmap is in [docs/spec.md §10](docs/spec.md#10-roadmap).

## How Sol compares

| | Sol | Tailscale | Cloudflare Tunnel / ngrok | Hand-made Tor onion |
|---|---|---|---|---|
| Account needed | No | Yes | Yes | No |
| Third party sees your traffic | No (Tor, end to end) | No (WireGuard) | Yes (TLS terminates at the provider) | No |
| Works behind CGNAT | Yes | Yes | Yes | Yes |
| Hides your IP from clients | Yes | No | From clients, not the provider | Yes |
| Per-machine access and revocation | Yes, built in | Yes | Via provider ACLs | Manual key files |
| Speed | Tor speed (fine for media, not for 4K) | Fast | Fast | Tor speed |
| Setup | One binary, a link | App + login | Account + domain | torrc, keys, restarts |

Pick Sol when you want privacy and independence more than raw speed.

## Development

```sh
make test      # unit tests + full two-node e2e over clearnet (no Tor needed)
make check     # gofmt, vet, staticcheck, tests (what CI runs)
make test-tor  # e2e over the real Tor network (needs tor; takes a few minutes)
```

Start with [AGENTS.md](AGENTS.md) for a map of the code and the rules, and
[docs/EXTENDING.md](docs/EXTENDING.md) to add a transport, provider, or audit check. See
[CONTRIBUTING.md](CONTRIBUTING.md) for how to contribute.

## License

[Apache-2.0](LICENSE). The vendored QR library in `web/vendor/` is MIT.
