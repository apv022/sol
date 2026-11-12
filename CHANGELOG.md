# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/),
and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Fixed
- **Slow "online" after starting.** A node could report "your address isn't reachable yet" for
  five minutes or more although peers could already reach it: its self-check fetched the
  descriptor from one directory picked at random, and a slow one stalled the check until tor was
  restarted. The node is now online as soon as tor confirms a directory accepted the descriptor
  (about ten seconds), and a single failed check no longer marks it unreachable.
- Panel buttons marked hidden were still shown (for example "Reconnect to Tor" while online).
- The directory page put values from `hosts.json` into the page unescaped.

### Changed
- **Every machine is now a node, and nodes pair as peers** ([docs/PEERS.md](docs/PEERS.md)).
  Pairing is mutual: both sides can reach each other, each chooses what it shares with the other,
  and either can unpair (the other side is told, and live connections close on both).
  `sol connect` and `~/.sol-client` are gone; re-pair your machines once with `sol invite` /
  `sol pair`. The handshake, pairing link, and config format changed in place (pre-release).
- New peers can only send you files; services are shared per peer (panel Peers tab, or
  `sol share`). The shared-files page is a share like any other.
- Pairing links and peer records keep addresses and access keys per transport, so I2P, a direct
  path, or a new protocol plug in without changing pairing or the handshake.

### Changed
- **`sol` now starts the node in the background and returns.** `sol stop`, `sol restart` and
  `sol logs [-f]` manage it; `sol run` is the old foreground behaviour, and what `sol autostart`
  uses. Running `sol` again shows where the running node is.

### Added
- **Settings without editing config.json**: `sol config` lists every setting with what it does,
  `sol config <key> <value>` changes one, and the panel has a Settings screen for the same list
  (Tor, direct mode and its port, forwarded addresses, STUN servers, LAN transport, ports, helping
  Tor) with a Restart button for the ones read at start-up. Bad values are refused and change
  nothing.
- **`sol site add <name> <folder>`**: serve a folder as a website on its own `.onion`, with no web
  server behind it (also in the panel: Add a service → Website). Sol serves the files itself and
  never lists folders, serves dotfiles (`.git`, `.env`), follows links out of the folder, or sends
  file dates. `--private` shares it with chosen peers only, over their verified connection.
- **Signed service records** (opt-in, `--record`): a public web service or site answers
  `/.well-known/sol.json` with its addresses signed by a key of its own (never the node key), so
  a directory or a visitor can check which addresses are the same site. It shows the site runs Sol.
- **Help the Tor network** (opt-in): `sol snowflake on`, or the switch on the panel's home screen,
  runs the official `snowflake-proxy` so people in censored countries can reach Tor through this
  machine. No open port needed; it is not a relay. Off by default, because it contacts the Tor
  Project and public STUN servers from your IP. Needs `snowflake-proxy` installed.
- A new logo and favicon.
- **Panel redesign**: a quiet terminal look with a sidebar of machines (or numbered tabs on top;
  keys 1-5 either way), peers and tunnels as tables, and a log view. **Settings** choose the theme
  (dark, light, system, green, amber), an accent colour, the layout and the text size, per browser. Your own
  CSS in `panel.css` in the data folder is loaded on top. The panel can be installed as an app
  window from browsers that support it.
- **Open a peer's app from the panel.** Every service a peer shares with you has an **Open**
  button; `sol open` does the same. The node now owns tunnels: they stay open without a terminal,
  are saved and reopen on the same port after a restart, survive the peer going offline (each
  connection dials fresh), and show their status and transport. `sol tunnels` and
  `sol close <peer> [service]` manage them. `http` tunnels check the Host header against DNS
  rebinding.
- **Direct mode** (opt-in, `"direct": true` under `transports`): peers connect straight to each
  other over **WireGuard** (the official wireguard-go, in userspace: no root), with NAT hole
  punching (public address learned on the WireGuard socket, endpoints exchanged over Tor, punch
  packets, a single handshake initiator), so two homes behind ordinary routers connect directly.
  The public address comes from your own paired peers (**peer reflection**): no third-party
  server is contacted unless you choose one under `stun` (presets `cloudflare`, `google`,
  `mozilla`, `twilio`, or any `host:port`). TLS 1.3 pinned to
  the node key covers networks that block UDP, and Tor everything else. Addresses refresh themselves after a network change;
  the panel shows "direct" or "via Tor" per connection. Public services are never direct. Peers
  may announce direct addresses only for their own key, as plain IPs; the port stays silent to
  anyone who doesn't already know the node's ID. `direct_advertise` (for a forwarded port) takes
  IP:port entries; if none is usable, direct mode stays off.
- `sol autostart on|off|status`: the node starts when you log in (systemd user unit on Linux,
  LaunchAgent on macOS; Windows not yet). The panel shows whether it's on.
- Send files between peers, both ways: `sol send <peer> <file>` or the panel's Send a file. The
  sender checks the receiver's SHA-256; per-file (4 GiB) and total (20 GiB) limits; received files
  are labelled with the sender and kept off your shared-files page.
- CLI talks to the running node over a local socket: `sol invite`, `pair`, `peers`, `share`,
  `open`, `send`, `unpair`, `name`, `status`, and `sol services` / `sol service add|remove|public|private`
  (with `--share PEER` to add and share in one step), so a node can be managed without the panel.
- Panel: Peers tab with invite and join, per-peer share toggles, send a file, unpair; node name.
- Redesigned panel for non-technical users: status hero with Tor progress, getting-started
  checklist, add-service presets, plain-language access choices, a pairing wizard that detects
  the new device, drag-and-drop files with names, readable activity log, dark mode.
- Live "connected and verified" status per peer and service, and a `connect` event.
- Self-healing Tor: restarts automatically when the network changes (VPN, Wi-Fi), and "Online"
  now means the node's address was verified reachable by a self-check. Reconnect button in the panel.
- Shared files page: paired devices open the "shared files" link from `sol connect` to browse
  and download the node's files by name, with SHA-256 fingerprints shown.
- Service health check: the panel warns when nothing listens on a service's port.
- `sol pair`/`sol connect` wait for Tor with progress, retry while an onion publishes, and explain
  failures in plain English.
- Node mode: Tor onion service with v3 client authorization, managed through tor's control port.
- `sol/1` handshake: one signed challenge-response per connection, then a raw TCP pipe.
- Pairing with single-use, 10-minute links and QR codes; per-device instant revocation.
- `sol pair` / `sol connect` client with automatic local port mapping.
- Public and anonymous-public services, each on its own onion; header stripping for `http` services.
- Content-addressed file store with range streaming (`_files` service).
- Control panel (loopback only, Host check, CSRF, CSP).
- `sol audit` with a pluggable check registry.
- Clearnet transport for LAN and tests; in-process end-to-end test suite.
- Stubs with documented seams: I2P transport, Docker provider, gateway, directory checker.
