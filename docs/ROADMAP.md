# Roadmap

Sol becomes the place your people, your devices and their apps live, with a small app API that
anyone can build a dapp on. The plumbing is ahead of the product, so the work now goes to the
product. **There are no dates.** A phase ends when its exit criteria pass, and Sol launches when
the whole plan below is built (marketing stays paused until then).

This file is the working plan. The design it builds on is already settled in
[APPS.md](APPS.md) (four primitives, apps outside the binary) and [NETWORK.md](NETWORK.md)
(trust layers, bigger nodes help but are never required). It doesn't reopen those decisions.

## Commitments

1. **It just works.** Install to a first connected device in under 3 minutes with no terminal.
   Opening a friend's app feels instant once warm.
2. **One surface.** One Sol app on desktop and Android, organised around **People, Devices and
   Apps**. Ports, onions, tunnels and transports move under Advanced. `sol://` links open
   anything.
3. **Prove the dapp layer by shipping dapps on it.** Sol Chat, your own agent and agent-native
   self-hosted apps, built only on the public API.

"Can't stop using it" means habit from real value (presence, instant sharing, things arriving),
never streaks, badges or engagement notifications.

## The product bar

Checked by the e2e suite and a nightly real-Tor bench; a release that misses a row doesn't ship.
These are targets to validate against a first baseline (P0-5), not measurements yet.

| Moment | Target | Checked by |
|---|---|---|
| Install to first paired device | < 3 min, no terminal (desktop and Android) | scripted fresh-VM run per OS |
| Pair (QR or link) | p50 < 15 s to connected | nightly real-Tor bench |
| Open a friend's app, cold, over Tor | p50 < 5 s, p95 < 15 s | nightly real-Tor bench |
| Open it again, warm or direct | < 500 ms; same LAN < 100 ms | e2e + bench |
| Reboot or network change | sessions heal within 30 s, 99% of the time | faultnet e2e |
| 1 GB file between direct peers | near line speed; resumable over Tor | bench |
| Default UI | no ports, onion addresses or transport names visible | UI review checklist |

No telemetry. Measurement comes from the bench, an opt-in `sol doctor` report, and a small
private tester group.

## The user's model

| Today's concept | What the user sees |
|---|---|
| Peer (a machine) | a person (contact) or one of my devices |
| Service + share list | an app, with "Who can open this?" |
| Tunnel on 127.0.0.1:9000 | tap the app, it opens; no port shown |
| `_inbox`, `_files` | the Inbox: files, messages, access and approval requests |
| Transport / path | one status dot (fast, private, offline); detail in Advanced |
| Pairing link | Add a person / Add my device (QR, link, or nearby) |

**Identity.** A person key (ed25519) signs a device certificate for each of the person's nodes.
"Add my device" links a node under your person key with full trust; adding a person pairs once
and their other devices follow through the signed device list. Recovery is a 24-word backup;
a lost device is revoked from any other device. Each device keeps its own node key, onion and
client-auth key, and the person layer sits above the transport. It does change what the
handshake and list reply carry, so it ships as **protocol version 2** (AGENTS.md rule 6:
version 1 stays, negotiated as today).

**Opening anything.** Home is a presence strip, app tiles (mine and shared with me), then the
Inbox. A tile opens `http://<app>.<person>.sol.localhost:<port>/`, one browser origin per app,
with lazy, pre-warmed tunnels. The router gets its own Host check and CSRF rules, as strict as
`api.guard` (rule 4). `sol://<person>/<app>/<path>` opens the app or offers **Ask for access**.

**One UI codebase, three shells.** `web/` becomes the Sol app: a desktop shell (Wails: tray,
`sol://`, autostart, notifications), sol-mobile's WebView, and a PWA. Preact + HTM is vendored
like `qrcode.js`, so there is still no build step. The CLI stays and calls the same API.

## Agents and agent-native apps

Your agent and your self-hosted apps are separate projects and Sol's first dapps. To Sol they
are ordinary apps; Sol adds agents as scoped principals, approvals in the Inbox, and an MCP
bridge. The target loop: from another country, you message your agent in Sol Chat, it works on
your homeserver through your apps, and every change shows up in Sol.

- `Sol-Person-Id` and `Sol-Agent-Id` headers tell an app who is asking.
- An **agent certificate** is your person key signing an agent key with scopes (apps, people)
  and an expiry, revocable from any device.
- Apps and agents post **approval requests** to the Inbox (same item type as Ask for access).
- **`sol-mcp`** exposes Sol's operations (people, apps, send, Inbox) as MCP tools, limited by the
  agent certificate. It is a separate program that talks to the control socket, not part of the
  `sol` binary, so the audited core doesn't grow.
- Sol never runs an agent, holds a model key, or exposes an app's tools. It carries and
  authenticates calls.

Checklist for agent-native apps: listen on 127.0.0.1 only and ship an `app.json`; CLI-first with
`--json` and `--dry-run`, same operations over HTTP; decide by identity, never by message text
(only your person key or your agent certificate gives instructions); keep a signed append-only
activity log; sign with the app key, never the node key; ask for approval before anything
destructive.

## Technology decisions

Rule: no new network and no new server before the existing ones are fully used. Anything apps
need beyond the four primitives goes in an SDK outside the binary.

| Concern | Choice | Rejected |
|---|---|---|
| Core | Go, stdlib-first, one dependency (wireguard-go) | a rewrite |
| Reach | bundled C tor child process, 0.4.9+ | system tor as the default |
| Tor on Android, later | evaluate embedding Arti (onion services + client auth) | switching desktop now |
| Speed | WireGuard direct + punching; add UPnP / NAT-PMP / PCP; relay through your own devices | public DERP-style relays |
| People | ed25519 person key, device certs, 24-word backup | accounts; a Nostr key as root (cross-sign instead) |
| Names to endpoints | Pkarr on the Mainline DHT, opt-in, see Privacy below | chain naming; our own DHT |
| Announcements | Nostr events carrying the signed service record; propose a NIP | a Sol-run index |
| App data (SDK) | signed append-only feeds; Automerge; content-addressed blobs | IPFS dependency; a blockchain |
| App runtime | A: web apps (bundles, one origin each). B: process apps (manifest + app socket). C: WASM via wazero | apps inside the binary |
| UI | Preact + HTM, vendored, no build step | React + bundler; Flutter |
| Desktop shell | Wails v2 | Electron |
| Payments | app-level only (e.g. Monero in the market app) | wallets in core |
| I2P | i2pd through the runtime provider, for publishing | I2P for speed |

### Privacy guardrails for discovery

- **Nearby (mDNS)** is off by default and on only while an "Add" screen is open, so a node
  doesn't announce itself on every network it joins. It shows a device name only; pairing
  still needs the one-time token.
- **Pkarr** records are opt-in. They hold only what a peer who already knows the key needs, are
  encrypted to paired people where possible, and never publish a direct IP by default: a
  public, long-lived key next to an IP links you to your location.
- Never announced: a node's private onion, the device list (paired people only), or any
  person-to-device link on a public app.
- Names are petnames, stored locally; fingerprints only in Advanced and on the pairing screen.
- Each guardrail lands in [THREAT_MODEL.md](THREAT_MODEL.md) with its feature.

## Phases

Each ticket is one or a few working sessions, lands with its own e2e case, and keeps
`make check` green. "After" lists the tickets that must land first. Status: `todo`, `doing`,
`done`.

### Phase 0: Just works

Exit: fresh-VM install to a paired device in under 3 minutes on Linux, macOS and Windows, and a
baseline number for every product-bar row.

| ID | Ticket | Touches | After | Status |
|---|---|---|---|---|
| P0-1 | Fetch, verify and pin tor 0.4.9+ for linux/darwin/windows × amd64/arm64 | `scripts/fetch-binaries.sh`, `internal/runtime/bin` | | todo |
| P0-2 | Bundled provider is the default, native the fallback | `internal/runtime` | P0-1 | todo |
| P0-3 | Windows autostart | `internal/autostart` | | todo |
| P0-4 | Signing and notarization; Homebrew, AUR, Flatpak, winget | `.goreleaser.yml`, `release.yml` | P0-1 | todo |
| P0-5 | Bench that outputs every product-bar metric as JSON, nightly in CI | new `cmd/solbench`, CI | | todo |
| P0-6 | Warm paths: circuits to online peers, descriptor freshness, tuned backoff | `transport/tor*.go`, `node` | P0-5 | todo |
| P0-7 | `sol doctor`: a redacted report users can paste | `cmd/sol`, `internal/node` | | todo |
| P0-8 | Signed release manifest and an in-app update check | new `internal/update` | P0-4 | todo |
| P0-9 | Security self-review of handshake, pairing, `api.guard`, files, sites; fix findings | core | | todo |

### Phase 1: One surface

Exit: 10 testers use it at least 4 days a week for 2 weeks, and 3 people new to Sol finish the
first-3-minutes flow without help. Testers are recruited privately at the start of the phase.

| ID | Ticket | Touches | After | Status |
|---|---|---|---|---|
| P1-0 | Spike: `*.sol.localhost` origins on Chrome, Firefox, Safari, Android WebView; fallback is a port per app | throwaway | | todo |
| P1-1 | Spec, then code: person key, device certificate, domain-separated signatures; protocol v2 in PROTOCOL.md | `internal/identity`, `docs/PROTOCOL.md` | | todo |
| P1-2 | Signed device list in the list reply; group peers by person; config migration | `protocol`, `peers`, `config` | P1-1 | todo |
| P1-3 | "Add my device": full trust between your devices, contacts and apps synced | `pairing`, `node` | P1-2 | todo |
| P1-4 | 24-word backup, restore, revoke a device | `identity`, `cmd/sol` | P1-1 | todo |
| P1-5 | Per-app origin router with its own Host/CSRF guard: `<app>.<person>.sol.localhost` to a lazy tunnel | `node`, new `internal/router` | P1-0 | todo |
| P1-6 | API v2 (people, devices, apps, inbox) plus an SSE event stream | `internal/api`, `internal/events` | P1-2 | todo |
| P1-7 | UI rebuilt in Preact + HTM: Home, People, Apps, Inbox, Advanced | `web/` | P1-6 | todo |
| P1-8 | `sol://` parsing and resolution; access-request item in `_inbox` | `node/inbox.go`, `cmd/sol` | P1-5 | todo |
| P1-9 | QR pairing screens; nearby discovery over mDNS (stdlib, off by default) | `pairing`, new `internal/nearby` | | todo |
| P1-10 | Desktop shell (Wails v2): tray, `sol://` handler, notifications | new repo `sol-desktop` | P1-7, P1-8 | todo |
| P1-11 | sol-mobile on the shared UI; share target and intent filter | `sol-mobile` | P1-7, P1-8 | todo |
| P1-12 | Tester group: 10 people and a private feedback channel | | | todo |
| P1-13 | `sol-mcp` (local): Sol's operations as MCP tools over the control socket | new repo or `cmd/sol-mcp` | P1-6 | todo |

### Phase 2: App platform and the first dapps

Exit: someone outside the project ships a working web app from the docs in a day, and for a week
you manage your homeserver from another network by messaging your agent in Sol Chat, with every
change visible in Sol.

| ID | Ticket | Touches | After | Status |
|---|---|---|---|---|
| P2-1 | Primitive 1: `Sol-Peer-Id` plus `Sol-Person-Id` on http apps | `node`, `internal/proxy` | P1-2 | todo |
| P2-2 | Primitive 2: per-app socket (peers, dial, whoami) | `control`, new `internal/apps` | P2-1 | todo |
| P2-3 | Primitive 3: `app.json`, `sol app install`, consent screen | `config`, `cmd/sol`, `web/` | P2-2 | todo |
| P2-4 | Primitive 4: app keys, sign, delegations | `identity`, `internal/apps` | P2-3 | todo |
| P2-5 | Web-app tier: content-addressed bundles, one origin each, `/_sol/` mirroring the app socket behind an origin check and a per-app token | `internal/apps`, `internal/blob` | P2-3, P1-5 | todo |
| P2-6 | `sol.js` SDK, template repo, `sol app dev` with hot reload | new repo `sol-js` | P2-5 | todo |
| P2-7 | Sol Chat as a web app on the public API, agents as contacts | new repo `sol-apps` | P2-6 | todo |
| P2-8 | Your agent and first self-hosted app on Sol: `app.json`, identity headers, activity log | agent and app repos | P2-2 | todo |
| P2-9 | Developer guide and an outside-developer trial | `docs/` | P2-6 | todo |
| P2-10 | Agent certificates: scoped, expiring, revocable; `Sol-Agent-Id` | `internal/identity`, `internal/proxy` | P1-1, P2-1 | todo |
| P2-11 | Approval requests in the Inbox, postable by apps and agents | `node/inbox.go`, `internal/apps` | P1-8, P2-2 | todo |
| P2-12 | `sol-mcp` scoped by the agent certificate | `sol-mcp`, `internal/apps` | P1-13, P2-10 | todo |

### Phase 3: The network grows

Exit: a pair behind symmetric NAT gets a non-Tor path through a relay, and 20% of new pairings
come from introductions.

- Pkarr endpoint records (opt-in, guardrails above); UPnP / NAT-PMP / PCP; relay through your
  own reachable devices.
- Introductions: A introduces B to C, C must agree, nothing transitive.
- The data SDK: signed feeds, Automerge sync, blob replication ("pin to my other devices").

### Phase 4: Public layer

Exit: an independent review of core plus the app API is done with highs fixed, and public apps
are discoverable without the directory.

- Nostr announcements and a NIP draft; the opt-in directory (spec 9).
- The social app (feeds on Nostr); the HTTPS gateway (spec 7.4).
- I2P publishing; full Anonymous Public Mode; an Arti spike on Android.
- Independent security review.

### v1.0, then launch

The market app, the WASM tier for untrusted apps (wazero), Aries, a stable protocol and API, the
1.0 freeze. Then the launch: Show HN, r/selfhosted, a developer launch.

## Risks

| Risk | Mitigation |
|---|---|
| Tor cold start makes Sol feel broken | measure first (P0-5), pre-warm, push direct paths, honest progress in the UI |
| The person layer complicates the audited core | protocol v2 beside v1, spec before code, e2e for revoke and restore |
| `*.localhost` origins differ across browsers and WebViews | P1-0 spike first; fall back to a port per app |
| Solo scope is large | exit criteria over dates; cut Phase 4 items before cutting the product bar |
| "Sol" reads as Solana to crypto audiences | decide on naming ("Sol Protocol" or a rename) before launch |

## Open decisions

- Pkarr: speak BEP44 directly in Go (preferred, no relay to trust) or go through a relay.
- Which Nostr event kind carries service announcements (NETWORK.md open question 2).
