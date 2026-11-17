# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/),
and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Docs
- Design for an app platform on Sol ([docs/APPS.md](docs/APPS.md)): Sol stays the network and
  identity layer, apps run on top through four primitives (who is calling, dial a peer's app,
  register with consent, per-app signing keys). Not built yet.
- [docs/NETWORK.md](docs/NETWORK.md): how paired nodes grow into a network (paired versus public
  reach, transports, discovery layers, bigger nodes).
- Example app designs in `docs/apps/`: music between friends, social feeds on Nostr, a market.

## [0.1.0-alpha.6] - 2026-11-15

### Fixed
- Direct mode now works on Android: when the OS denies listing network interfaces (Android 11+
  blocks netlink), a node advertises its outbound IPv4/IPv6 address for WireGuard and TLS direct
  instead of no direct address at all.

## [0.1.0-alpha.5] - 2026-11-15

Pre-release. First build with the versioned handshake (protocol version 1): nodes from before it
can't talk to it, so update every machine and re-pair once.

### Added
- **Versioned handshake (protocol version 1, locked for v0.1.0).** Both hellos carry the sender's
  supported version range and the nodes use the highest version both speak. The negotiated
  version and both ranges are signed, so a man in the middle can't force a downgrade. A peer that
  speaks no common version shows as incompatible in the panel and `sol peers` ("alpha runs an
  older Sol; update Sol on alpha"), is logged once, fails dials at once instead of retrying, and
  is checked back only at the slowest redial pace. The callee still sends nothing to a caller it
  refuses: an incompatible caller gets only the 8-byte hello prefix every Sol caller gets.
- Pairing links carry a format version (`v=1`); a link from a newer Sol says "update Sol" instead
  of failing to parse.
- **Edit a service in place** (panel: Edit on each service; `sol service edit <name> [--port N]
  [--http|--tcp] [--rename NEW]`, `sol site edit <name> [--folder DIR] [--record|--no-record]`;
  `PATCH /api/services/{name}`). New connections use the new port at once; open streams keep
  going. A public service keeps its address, also when renamed (its onion and record keys move
  with it). A rename carries every peer's share along; peers see the new name after a refresh and
  reopen it there. Refused edits change nothing.
- **Shared files are now fully editable** (panel Files screen, `sol files [add|rename|rm]`, API
  `PUT /api/files/{ref}/name`, `DELETE /api/files/{ref}`, `DELETE /api/inbox/{hash}`): add,
  rename, replace (same name, new content) and delete; delete files peers sent too. The same bytes
  can be shared under several names; they leave the disk only with their last name (or inbox
  entry). Peers see changes on their next look at the shared files page.
- `sol network-changed [reason]` tells the running node that the network changed (the Android app
  runs it on every default-network change), so it re-checks its connections at once.
- `internal/faultnet`, a fault-injection test kit (scripted fake tor, TCP/SOCKS5 proxies that add latency or blackhole, reset or refuse connections), and tests that use it for the Tor supervisor and peer liveness. These have only been run on Linux so far.
- `SOL_BIN_TOR` (and `SOL_BIN_<NAME>` for any helper program) names the program's file directly,
  for platforms without a usable PATH. The Android app
  ([sol-mobile](https://github.com/apv022/sol-mobile)) uses it.
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

### Changed
- The handshake's magic is now `SOLH` with a version range, replacing the unversioned `SOL1`:
  nodes from before this change can't talk to nodes after it (update both; pairings are kept).
  Pairing links without `v` are refused.
- Panel polish: the leak check's "skip" badges were invisible (a class clash with the skip link), the
  sidebar no longer squashes in short landscape windows, the tabs-on-top layout keeps Appearance on the
  first row, stopped peers show a visible hollow dot, and the system bars on phones follow the bar colour.
- Settings can no longer leave the node unusable: turning off the last transport, an invalid port,
  or a port used twice is refused with a plain message next to the control.
- `Tor.WaitReady` keeps waiting after tor is marked failed (the supervisor keeps retrying) until
  tor is ready, the context ends or tor is stopped, instead of returning an error early.
- **Panel redesign.** Every peer, open tunnel and connection shows the path it takes (Tor,
  WireGuard direct, TLS direct, LAN) in plain words, and why direct mode isn't carrying it and
  what fixes it. Tor's start-up shows its real phase, progress, time spent and retries instead of
  a spinner; Reconnect appears only when Tor is stuck. Settings has a "How this node connects"
  section that explains each transport and, before a change, shows whether it needs a restart and
  which peers become unreachable (`GET /api/settings/{key}/impact`), asks before risky changes,
  and shows the node's real running state (`running`/`pending` in `/api/settings`). Ports, STUN
  and forwarded addresses moved under Advanced. New phone layout (tab bar, bottom sheets, 44 px
  targets) and Android app support (`/#pair=` links, `SolAndroid` user agent).
- The shared files page now looks like the panel: it inlines the panel's design tokens (`web.Tokens`, delimited in `web/style.css`), follows light or dark, and has a phone-friendly layout with large buttons and an empty state. Still inline CSS only, no script, same CSP.
- **Every machine is now a node, and nodes pair as peers** ([docs/PEERS.md](docs/PEERS.md)).
  Pairing is mutual: both sides can reach each other, each chooses what it shares with the other,
  and either can unpair (the other side is told, and live connections close on both).
  `sol connect` and `~/.sol-client` are gone; re-pair your machines once with `sol invite` /
  `sol pair`. The handshake, pairing link, and config format changed in place (pre-release).
- New peers can only send you files; services are shared per peer (panel Peers tab, or
  `sol share`). The shared-files page is a share like any other.
- Pairing links and peer records keep addresses and access keys per transport, so I2P, a direct
  path, or a new protocol plug in without changing pairing or the handshake.
- **`sol` now starts the node in the background and returns.** `sol stop`, `sol restart` and
  `sol logs [-f]` manage it; `sol run` is the old foreground behaviour, and what `sol autostart`
  uses. Running `sol` again shows where the running node is.

### Fixed
- A service renamed while a peer was opening it no longer cuts that new stream.
- **Direct connections stay direct on slower machines.** Opening a stream over a live
  WireGuard tunnel sent a punch, which made an initiating peer reset its session, and the TCP
  connect only got what the handshake left of its time budget. Both made dials fall back to
  Tor or clearnet for no reason (and failed the macOS CI). A live tunnel is no longer punched,
  and the stream gets its own budget.
- **Apps that listen on `localhost` over IPv6 now work.** A service whose app binds only `[::1]`
  (many Node and Bun servers, e.g. opencode) gave peers an empty reply and showed as not
  running; Sol now tries `127.0.0.1` and then `[::1]`, both loopback only. The node's log names
  the peer and both addresses when nothing answers.
- A crash in direct mode when a WireGuard stream was reset before it was set up (macOS CI), and
  such streams are now always tied to their peer so they are cut when the tunnel dies.
- The shared files page now uses the panel's own stylesheet base (header with the node name,
  rows, buttons), follows the visitor's light or dark setting, applies the owner's `panel.css`,
  and uses a hash-based CSP instead of `'unsafe-inline'` styles.
- **Security updates:** builds use Go 1.27.4 (fixes GO-2026-6859 in `net/http`), and
  `golang.org/x/crypto` and `golang.org/x/net` are updated past every open advisory. Building
  from source now needs Go 1.26+. CI runs `govulncheck` on every push and weekly, and a release
  is refused if it finds anything.
- `install.sh` installs the newest pre-release while no stable release exists, instead of
  failing with a broken download URL.
- **A WireGuard tunnel that dies no longer leaves its streams hanging.** Streams were cut only
  when the tunnel went silent while marked up; one that stopped counting as up another way (a
  session reset by a peer's punch) and then died kept them open until the node's probes gave up.
  Now any stream on a tunnel not heard from for the silence limit is cut, and a tunnel that just
  came up is no longer punched (which made the initiating peer reset it). Tor's retry status
  reports the exact backoff.
- **First contact over Tor is fast again.** Reaching a new onion often took minutes: tor's lookup
  of the onion's descriptor from one directory sometimes hangs, and tor waited 120 s
  (`SocksTimeout`) before giving up, while further dials only joined the hung lookup. `Tor.Dial`
  now asks tor to fetch from another directory (`HSFETCH`) when no descriptor has arrived after 8 s,
  the torrc sets `SocksTimeout 45` and `ExtendedErrors`, and a failed dial is retried after 5 s
  instead of 20 s. Measured over 6 new onions each: median 89 s → 17 s from publish to first
  connection. A joining node publishes the key it issues to the inviter before the pairing dial,
  so pairing no longer re-publishes its onion right when the inviter first dials it.
- SOCKS extended error codes from F1 on were named off by one (F5 said "missing client
  authorization" for "wrong client authorization").
- A peer probe cut mid-handshake (or whose held bytes never arrive) no longer logs "handshake failed:
  malformed request"; truncated or timed-out handshakes are a debug-level "connection closed during
  handshake". Genuinely malformed bytes are still rejected and logged as before.
- A peer with a working tunnel no longer shows "Not connected": the panel's peer state is now connected,
  reachable-but-idle, unreachable or unknown, matching its tunnels and link.
- **Peer connections no longer hang on a dead path.** A stream whose path died silently (phone
  changed networks or slept, NAT dropped the mapping, WireGuard UDP blocked) is now detected by
  path probes, TCP keepalive and WireGuard silence, closed, and the peer redialled with backoff
  over the next path. A failed direct path returns only after it has been stable for two minutes,
  so it no longer flaps with Tor. `NetworkChanged` and waking from sleep re-check every peer at
  once. `Node.PeerLink` reports the path, fallback reason and direct-path state.
- **Tor recovers by itself.** A tor that stalled while bootstrapping, froze, lost its control port,
  or failed to start is now replaced automatically with backoff (and a stale directory cache is
  cleared after repeated stalls), so "Reconnect" is no longer needed. Status reports the phase,
  bootstrap percentage, what it waits for, the attempt and the next retry. A new
  `NetworkChanged` hook (debounced) lets the OS report network changes.
- **Slow "online" after starting.** A node could report "your address isn't reachable yet" for
  five minutes or more although peers could already reach it: its self-check fetched the
  descriptor from one directory picked at random, and a slow one stalled the check until tor was
  restarted. The node is now online as soon as tor confirms a directory accepted the descriptor
  (about ten seconds), and a single failed check no longer marks it unreachable.
- Panel buttons marked hidden were still shown (for example "Reconnect to Tor" while online).
- The directory page put values from `hosts.json` into the page unescaped.

[Unreleased]: https://github.com/apv022/sol/compare/v0.1.0-alpha.6...HEAD
[0.1.0-alpha.6]: https://github.com/apv022/sol/releases/tag/v0.1.0-alpha.6
[0.1.0-alpha.5]: https://github.com/apv022/sol/releases/tag/v0.1.0-alpha.5
