# Security policy

Sol is early software that has not had an independent security review. What it does and does not
protect against is in [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md).

## Supported versions

Sol is pre-1.0: only the latest release (currently the pre-release `v0.1.0-alpha.6`) gets fixes.
Once `v0.1.0` is out, that is the latest stable release. Please check against it before reporting.

## Looking for review

Nobody outside the project has reviewed Sol yet. If you know Tor onion services or protocol
design, these are the parts where a second pair of eyes would help most:

1. **The pairing handshake** ([docs/PROTOCOL.md](docs/PROTOCOL.md), `internal/protocol`,
   `internal/pairing`): the one-time link, what the signature covers, and whether anyone on the
   path can swap in their own address or key.
2. **Tor client-authorization keys** (`internal/transport/tor.go`): one x25519 key per peer, how
   they are issued, stored and installed, and what a stolen `config.json` gives away.
3. **Unpairing** (`internal/node/dial.go`, `internal/peers`): that removing a peer really ends
   its access at both layers, at once, including connections that are already open.

Findings of any size are welcome, including "this is fine, and here is why". Please report
anything exploitable privately (below); everything else can be an issue or a discussion.

## Reporting a vulnerability

Please report privately with GitHub's **"Report a vulnerability"** button on the repository's
Security tab (private vulnerability reporting). Include:

- what an attacker can do, and under which setup (transport, visibility mode),
- steps to reproduce,
- the Sol version (`sol version`) and OS.

You should get a reply within a week. Please give us a reasonable time to fix the issue before
disclosing it publicly. We will credit you unless you'd rather we didn't.

## Scope

In scope: the handshake and pairing (`internal/protocol`, `internal/pairing`), access control and
removal (`internal/node`, `internal/peers`), the panel (`internal/api`, `web/`), Tor
integration (`internal/transport`), and anything that leaks a user's IP address or keys.

Out of scope: vulnerabilities in Tor itself (report those to the Tor Project), and attacks that
need control of the node machine's user account.
