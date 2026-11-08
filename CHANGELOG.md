# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/),
and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

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

### Added
- Send files between peers, both ways: `sol send <peer> <file>` or the panel's Send a file. The
  sender checks the receiver's SHA-256; per-file (4 GiB) and total (20 GiB) limits; received files
  are labelled with the sender and kept off your shared-files page.
- CLI talks to the running node over a local socket: `sol invite`, `pair`, `peers`, `share`,
  `open`, `send`, `unpair`, `name`, `status`.
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
