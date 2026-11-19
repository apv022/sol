# Build plan

The working plan for the rebuild. Agents: read [AGENTS.md](../AGENTS.md), then this file, then
the spec files your ticket names. Update your ticket's status here when you finish.

## Decisions (settled)

| Question | Decision | Why |
|---|---|---|
| Language | Go for the daemon, the SDK and every app backend | one toolchain, static binaries for Linux/Android/macOS/Windows, the PoC is Go |
| Dependencies | stdlib only, except `wireguard-go` in the daemon (direct mode) | slim; each exception needs a line in AGENTS.md |
| Repo | one monorepo, one Go module | apps, SDK and daemon change together while the API settles |
| App UIs | web: plain JS modules + the shared UI kit, no build step, no framework | runs identically in the Android WebView, desktop shell and browsers; nothing to install |
| App packaging | each app is its own process talking to the daemon over the app API. First-party apps are compiled into the one `sol` binary (busybox style, `sol app run files`), still one process each | one binary to ship (Android can only exec what's in the APK), third-party apps are any executable with an `app.json` |
| Peer wire | protocol v2: the PoC handshake extended with person/device certificates, then HTTP/2 (stdlib, unencrypted h2 over the already authenticated stream) as the multiplexer | streams, flow control and headers without a hand-written mux |
| Collaboration | each writer has their own feed; apps merge with CRDTs from the SDK. v1 CRDTs: LWW map, ordered list, and a block-level text CRDT (paragraphs merge, same-paragraph conflicts resolve last-writer-wins) | small and good enough; character-level text CRDT is a later upgrade behind the same SDK API |
| Android | the PoC's Gradle-free build (javac + d8, `~/sol-mobile/build.sh`) packaging the `sol` binary as a native lib, plus a WebView that opens the Sol app and app origins | proven on a Pixel |
| Desktop shell | none at first: the Sol app opens in the browser as an installable PWA. A native shell comes later | slimmest path; no decision on Wails/Tauri needed yet |
| Agent | first-party app `agent` in Go: providers over plain HTTP (Anthropic, OpenAI-compatible incl. Ollama), tools = Sol actions + coding tools, sub-agents and loops, CLI/TUI + chat UI | |

## Layout

```
cmd/sol/            the one binary: daemon, CLI (`sol do`, `sol mcp`, `sol app`), builtin apps
internal/           daemon only (identity, wire, feeds, blobs, apps, api, transport, ...)
sdk/go/sol/         Go client for the app API + memsol (in-memory fake) + CRDTs
sdk/js/             sol.js (browser client), crdt.js
ui/kit/             tokens.css, components (plain JS custom elements), icons
apps/files/         Files: folders, sync, sharing, upload/download
apps/docs/          Docs: notes and rich text documents, collaborative
apps/music/         Music: library indexer (anchor) + player, peers' libraries
apps/home/          the Sol app: people, devices, apps, inbox, settings (admin API)
apps/agent/         the agent
android/            Android shell (from ~/sol-mobile)
design/             vision, architecture, specs, this plan
```

Rules of the layout: `apps/*` import only `sdk/go/...` and the stdlib, never `internal/`. The
daemon never imports `apps/` except `cmd/sol` wiring builtin apps.

## The loop

Two lanes run at once: **Core** (one Opus agent: protocol, crypto, concurrency) and **Apps**
(one Sonnet agent: SDK, UI, apps). Apps are built against `memsol`, so they never wait for the
daemon; when a core ticket lands, the next app ticket also runs against the real daemon.
Each ticket ends with `make check` green and its own tests. The coordinator reviews, runs the
tests, updates this file and starts the next ticket.

Status: `todo` · `doing` · `done`.

### Core lane

| ID | Ticket | Spec | Status |
|---|---|---|---|
| C1 | Repo skeleton (go.mod, Makefile, `make check`), port from `~/solbak`: transports (tor, wg, stun, direct, clearnet), faultnet, runtime, config atomic writes, netutil, blob store. Keep their tests passing | AGENTS.md | todo |
| C2 | Identity: person/device/app keys, certificates, revocations, 24-word backup | design/IDENTITY.md (write first) | todo |
| C3 | Wire v2: handshake with certificates, pairing a person, adding my device, h2 session per peer, liveness + healing (port PoC logic) | design/PROTOCOL.md (write first) | todo |
| C4 | Feeds: store, signing, append/read, replication to own devices and sharers, catch-up, events stream | API.md §3, §5 | todo |
| C5 | Blobs over sessions: chunked fetch, Range, multi-source, pin, permission rule | API.md §4 | todo |
| C6 | App host: manifest, install, supervise, app tokens, app socket routing, per-app origins, `/_sol/` bridge | API.md §1, §9, §10 | todo |
| C7 | Actions registry, `sol do`, `sol mcp`, activity log, Inbox + approvals, dial | API.md §6–8 | todo |
| C8 | Admin API for the Sol app: people, pairing, devices, port sharing (services), settings | design/ADMIN.md | todo |

### Apps lane

| ID | Ticket | Status |
|---|---|---|
| A1 | `sdk/go/sol` client + `memsol` fake (whole API.md) + CRDTs (LWW map, list, block text) with tests; `sdk/js/sol.js` + `crdt.js`; `ui/kit` (tokens, base components, dark/light) | todo |
| A2 | Files app: tree as a feed (LWW map of paths → blob), upload/download, folders, share a folder, peers' shared folders, actions + UI | todo |
| A3 | Docs app: notes and rich text (slim editor: headings, lists, bold/italic, links, checklists, code), block CRDT sync, collaborators, actions + UI | todo |
| A4 | Music app: indexer (tags from MP3/FLAC/Ogg/M4A, cover art) on anchors, library feed, player UI with queue, play from peers' libraries | todo |
| A5 | Sol app (`apps/home`): people, devices, apps grid, Inbox, pairing with QR, settings | todo |
| A6 | Agent: providers, tool loop, Sol actions as tools, coding tools, sub-agents, loops/schedules, approvals, CLI/TUI + chat UI | todo |
| A7 | Android shell: build `sol` for arm64/x86_64, WebView on the Sol app, notifications, share target; test on the emulator | todo |

Order: A1 → A2 → A3 → A4 while C1 → C7 run; A5 after C8; A6 after C7; A7 after C6.
