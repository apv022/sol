# Threat model

Sol's anonymity and encryption come from Tor. Sol adds peer-level access control on top and
tries hard not to leak. It does not invent new cryptography or a new anonymity network. This page
says plainly what is and isn't protected in the current build. The longer design discussion is in
[spec.md §7](spec.md#7-security-model).

## What is protected

| Threat | Protection | Where |
|---|---|---|
| A stranger finds the node's onion address | Tor v3 client authorization hides the onion from anyone without a key issued to one of its peers. If they get past that, the handshake drops unsigned or unknown keys without sending a byte | `transport/tor.go`, `protocol`, `node.handleConn` |
| Port scanners and HTTP probes | The client must speak first with magic bytes; anything else gets no reply | `protocol.ReadRequest` |
| A replayed handshake | Every connection signs a fresh 32-byte nonce from the node | `protocol` |
| A man in the middle changing the target service, or swapping in their own address or key while pairing | The signature covers op, service, secret, the joiner's whole card, the callee's key, and the nonce | `protocol.signedMessage` |
| A peer opening more than you meant to share | Each peer reaches only the services in its own share list. New peers get the inbox only. Unsharing closes its open connections to that service | `node.handleConnect`, `Node.SetShare` |
| A stolen or lost machine | Unpair it on any of your other machines. Its live connections close at once, in both directions, and its Tor key is removed from the onion | `node` hooks, `peers.Peers.Remove` |
| A peer filling your disk or sending something nasty | Received files are capped (4 GiB each, 20 GiB in total), stored by hash, named safely, kept off your shared-files page, and served with a `sandbox` CSP. Browsers can't be tricked into sending (PUT only, `Origin` refused) | `node/inbox.go`, `blob` |
| A file corrupted or swapped in transit | The sender hashes while it sends and checks the receiver's SHA-256 | `Node.SendFile` |
| Other local users driving your node | The CLI talks to the node over a `0600` Unix socket in the `0700` data directory; it's never on the network | `control`, `node/ctlserver.go` |
| A leaked pairing link | Single use, expires in 10 minutes, and the secret is compared by hash | `pairing` |
| Knowing who is really connected | The panel's "Connected and verified" status lists only streams that passed the handshake with that peer's key | `node.Peers` |
| Malicious web pages reading a peer's app through a tunnel | Tunnels bind `127.0.0.1`; `http` tunnels accept only a `127.0.0.1`/`localhost` Host header (DNS rebinding) and add no forwarding headers | `node/tunnels.go` |
| Malicious web pages attacking the panel | Loopback-only bind, Host header check (DNS rebinding), CSRF token, strict CSP, HTML escaping | `api.guard`, `web/app.js` |
| Linking a public site to your private node | Each public service gets its own onion key | `node.publishService` |
| Network observers | Tor encrypts end to end. Onion services use no exit nodes. Direct mode uses TLS 1.3 | Tor, `transport/direct.go` |
| A man in the middle on a direct connection (hostile Wi-Fi, a stale address now used by someone else) | The direct address names the node key; the dialer accepts only a TLS certificate for exactly that key, then the signed handshake runs inside | `transport/direct.go` |
| Tampered bundled tor binary | A pinned SHA-256 is checked before extraction | `runtime/bundled.go` |

## What is NOT protected (yet, or ever)

- **Direct mode shows your IP addresses** (LAN, IPv6, tailnet) to your peers. It's off by
  default; turn it on only for peers you'd trust with your IP. Public and anonymous services are
  never published on it. The port answers only dialers that already know your node ID (the
  preamble carries a hash of it), so scanners get silence and can't learn your key. Someone who
  has your node ID (from a pairing link) can still tell the port is a Sol node.
- **Direct addresses are dialled as announced.** A peer can announce a direct address only for
  its own key, but it chooses the host:ports, so a malicious peer could make your node send a
  short TLS hello to up to 8 addresses on your network, at most once a minute (backing off).
- **Clearnet transport.** It's unencrypted and shows your LAN IP. Use it only on a trusted LAN or
  for testing. Peers are still authenticated, but traffic can be read.
- **Your peers know your onion.** Pairing is mutual, so each peer can reach your node's address
  (with its own key) and can tell when you're online. Pair only with machines you trust; unpair
  removes that.
- **Addresses a peer gives you are dialled as given.** A peer's card can name any address on any
  enabled transport. With clearnet enabled, a malicious peer could make your node dial a LAN
  address. Keep clearnet off outside testing.
- **Tunnels are local ports.** Any program running on this machine (any user, on a shared
  computer) can connect to `127.0.0.1:<port>` and use a peer's service with your access. A web
  page can still *send* blind requests to a `tcp` tunnel, as it can to any local port. Close
  tunnels you don't use.
- **A compromised node machine.** Anyone with your user account has `node.key`, every service, and
  the access keys for every peer (they are in `config.json`). Unpair it from your other machines.
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
