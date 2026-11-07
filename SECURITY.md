# Security policy

Sol is early software that has not had an independent security review. What it does and does not
protect against is in [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md).

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
revocation (`internal/node`, `internal/auth`), the panel (`internal/api`, `web/`), Tor
integration (`internal/transport`), and anything that leaks a user's IP address or keys.

Out of scope: vulnerabilities in Tor itself (report those to the Tor Project), and attacks that
need control of the node machine's user account.
