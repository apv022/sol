# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/),
and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
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
