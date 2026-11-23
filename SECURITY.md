# Security policy

Sol is early software. It has had one internal review ([design/SECURITY_REVIEW.md](design/SECURITY_REVIEW.md))
and no independent one. How identity, pairing and the wire protocol work is in
[design/IDENTITY.md](design/IDENTITY.md) and [design/PROTOCOL.md](design/PROTOCOL.md); who may use the
local UI and APIs is in [design/ADMIN.md](design/ADMIN.md) ("Who may use it") and
[design/API.md](design/API.md) §10.

## Supported versions

Sol is pre-1.0: only the latest release gets fixes. Check against it (`sol version`) before reporting.

## Where a second pair of eyes helps most

1. **Identity and pairing** (`internal/identity`, `internal/pairing`, `internal/wire`,
   `internal/session`): person/device/app/agent certificates, one-time pair and join links, the
   handshake, restore from the 24 words, and whether anyone on the path can swap in their own key.
2. **Anonymous contacts** (`internal/session`, `internal/daemon/privacy.go`): that an anonymous
   contact only ever learns onion addresses and is refused on every other transport.
3. **Local access** (`internal/daemon/browser.go`, `internal/daemon/peercred*.go`,
   `internal/apphost/ui.go`, `internal/daemon/setup.go`): same-user auto-login, login codes, app
   origins, the setup page and its token, and Android's loopback exposure to other apps.
4. **Sharing and blobs** (`internal/feeds`, `internal/replicate`, `internal/blobnet`): that a
   person can read only the feeds shared with them and fetch only blobs those feeds reference.

Findings of any size are welcome, including "this is fine, and here is why".

## Reporting a vulnerability

Please report privately with GitHub's **"Report a vulnerability"** button on the repository's
Security tab. Include what an attacker can do and under which setup (transports, anonymous or not,
desktop or Android), steps to reproduce, and `sol version` and the OS.

You should get a reply within a week. Please give a reasonable time to fix the issue before
disclosing it publicly. You'll be credited unless you'd rather not be.

## Scope

In scope: everything in this repository and apv022/sol-mobile, and anything that leaks a person's
IP address to an anonymous contact, their keys, or their data to someone they didn't share with.

Out of scope: vulnerabilities in Tor or WireGuard themselves (report those upstream), and attacks
that need control of the device's own user account.
