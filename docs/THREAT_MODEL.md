# Threat model

Sol's anonymity and encryption come from Tor. Sol adds device-level access control on top and
tries hard not to leak. It does not invent new cryptography or a new anonymity network. This page
says plainly what is and isn't protected in the current build. The longer design discussion is in
[spec.md §7](spec.md#7-security-model).

## What is protected

| Threat | Protection | Where |
|---|---|---|
| A stranger finds the node's onion address | Tor v3 client authorization hides the onion from anyone without a paired device's key. If they get past that, the sol/1 handshake drops unsigned or unknown keys without sending a byte | `transport/tor.go`, `protocol`, `node.handleConn` |
| Port scanners and HTTP probes | The client must speak first with magic bytes; anything else gets no reply | `protocol.ReadRequest` |
| A replayed handshake | Every connection signs a fresh 32-byte nonce from the node | `protocol` |
| A man in the middle changing the target service | The signature covers op, service, name, secret, node key, and nonce | `protocol.signedMessage` |
| A stolen or lost device | Remove it in the panel. Its live connections close at once, and its Tor key is removed from the onion | `node` hooks, `auth.Devices.Remove` |
| A leaked pairing link | Single use, expires in 10 minutes, and the secret is compared by hash | `pairing` |
| Knowing who is really connected | The panel's "Connected and verified" status lists only streams that passed the handshake with that device's paired key | `node.Devices` |
| Malicious web pages attacking the panel | Loopback-only bind, Host header check (DNS rebinding), CSRF token, strict CSP, HTML escaping | `api.guard`, `web/app.js` |
| Linking a public site to your private node | Each public service gets its own onion key | `node.publishService` |
| Network observers | Tor encrypts end to end. Onion services use no exit nodes | Tor |
| Tampered bundled tor binary | A pinned SHA-256 is checked before extraction | `runtime/bundled.go` |

## What is NOT protected (yet, or ever)

- **Clearnet transport.** It's unencrypted and shows your LAN IP. Use it only on a trusted LAN or
  for testing. Devices are still authenticated, but traffic can be read.
- **A compromised node machine.** Anyone with your user account has `node.key` and every service.
- **A compromised service.** Services run as normal local processes. Sandboxing is v1.0.
- **Global adversaries** who can watch both ends of Tor and correlate traffic timing.
- **Uptime correlation.** A public onion that goes down whenever your home internet does can be
  linked to you. Replication is on the roadmap.
- **Anonymous Public Mode** is partial in this build. Sol refuses to publish such services on
  clearnet, strips identifying HTTP headers for `http` services, and `sol audit` checks clearnet
  is off. The egress lock (stopping the *service itself* from leaking your IP through its own
  outbound requests), EXIF stripping, and vanguards are **not implemented**, and `sol audit`
  reports them as `skip`. Don't rely on this mode for real anonymity yet.
- **Gateways** (v0.2) will see the traffic they proxy. Run your own.

## Reporting a vulnerability

See [SECURITY.md](../SECURITY.md).
