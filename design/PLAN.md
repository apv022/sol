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
| Peer wire | protocol v2 (design/PROTOCOL.md): mutual handshake with identity bundles, then a small stream mux with HTTP/1.1 per stream, opened by either side | HTTP/2 only lets one side make requests; replication and blob fetches need both directions |
| User data | each app uses the model that fits its job: Files is a synced folder (design/apps/FILES.md); Music and Movies read media from it; Notes are Markdown files there; Tasks and Calendar keep records in synced feeds with CRDTs; every app exports/imports a standard format | per-item merging and live sync where data is structured; plain files where data is naturally files |
| Conflicts | Files: concurrent edits to one file keep a conflict copy (like Syncthing). Feed-based apps merge with CRDTs, so they never conflict | each app gets the best merge its data allows |
| Agent interface | the CLI (`sol do`), no MCP | agents drive shells well, schemas stay out of the context window; an MCP adapter can be added later from the same schemas |
| Android | the PoC's Gradle-free build (javac + d8, `~/sol-mobile/build.sh`) packaging the `sol` binary as a native lib, plus a WebView that opens the Sol app and app origins | proven on a Pixel |
| Desktop shell | none at first: the Sol app opens in the browser as an installable PWA. A native shell comes later | slimmest path; no decision on Wails/Tauri needed yet |
| Agent | first-party app `agent` in Go: providers over plain HTTP (Anthropic, OpenAI-compatible incl. Ollama), tools = Sol actions + coding tools, sub-agents and loops, CLI/TUI + chat UI | |

## Layout

```
cmd/sol/            the one binary: daemon, CLI (`sol do`, `sol app`), builtin apps
internal/           daemon only (identity, wire, feeds, blobs, apps, api, transport, ...)
sdk/go/sol/         Go client for the app API + memsol (in-memory fake) + CRDTs
sdk/js/             sol.js (browser client)
ui/kit/             tokens.css, components (plain JS custom elements), icons
apps/files/         Files: folders, sync, sharing, upload/download
apps/notes/         Notes: Markdown notes over the synced folder
apps/tasks/         Tasks: tasks and lists with dates
apps/calendar/      Calendar: events, invites
apps/movies/        Movies: video library and player
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
| C1 | Repo skeleton (go.mod, Makefile, `make check`), port from `~/solbak`: transports (tor, wg, stun, direct, clearnet), faultnet, runtime, config atomic writes, netutil, blob store. Keep their tests passing | AGENTS.md | done |
| C2 | Identity: person/device/app keys, certificates, revocations, 24-word backup | design/IDENTITY.md (write first) | done |
| C3 | Wire v2: handshake with certificates, pairing a person, adding my device, h2 session per peer, liveness + healing (port PoC logic) | design/PROTOCOL.md (write first) | done |
| C3b | Wire follow-ups from PROTOCOL.md "Settled in C3": 1 MiB bundle frames, join `name`, `recover` op, push bundle changes, persist the identity cache; then port the PoC's transport ranking, probation, hedged dialing, sleep/network-change detection | design/PROTOCOL.md | todo |
| C4 | Feeds: store, signing, append/read, replication to own devices and sharers, catch-up, events stream | API.md §3, §5 | done |
| C5 | Blobs over sessions: chunked fetch, Range, multi-source, pin, permission rule | API.md §4 | done |
| C6 | App host: manifest, install, supervise, app tokens, app socket routing, per-app origins, `/_sol/` bridge | API.md §1, §9, §10 | done |
| C7 | `sol do <app>.<action>` (args from flags/JSON, `--json`, `--dry-run`, generated `--help` from schemas), agent keys (`sol agent new/list/revoke`, certs per IDENTITY.md, `--as-agent`/`SOL_AGENT_KEY`), Inbox approvals end to end, `theme` events pushed (live swap), `sol backup words/confirm` + `sol restore`, join device names, an SDK method for GET /v1/inbox | API.md §6–8, IDENTITY.md | done |
| C6b | C6 follow-ups: peer app UIs (`<app>.<petname>.localhost`), my devices learn contacts' endpoints, Tor client auth, offline scenarios in the conformance suite, drop CSP `style-src 'unsafe-inline'` (apps use classes/CSSOM, not inline style attributes) | API.md §10, PROTOCOL.md | todo |
| C7b | C7 follow-ups: theme file watching on macOS/Windows (scan fallback), themes sync between devices (Files ignores `.sol/`: sync `.sol/themes` or move themes to an internal feed), `theme.*` actions, re-add contacts after restore, agent list across my devices, `sol do` human output picks sensible default columns per action (manifest hint) | todo |
| C8 | Admin API for the Sol app: people, pairing, devices, port sharing (services), settings | design/ADMIN.md | todo |

### Apps lane

| ID | Ticket | Status |
|---|---|---|
| A1 | `sdk/go/sol` client + `memsol` fake (whole API.md) + CRDTs (LWW map, list, block text) with tests; `sdk/js/sol.js` + `crdt.js`; `ui/kit` (tokens, base components, dark/light) | done |
| A1c | UI kit v2 to design/UI.md: tokens, Inter fallback font vendored, Apple-style components (shell, grouped lists, sheets, menus, segmented, toggles, squircle icons, widgets, dock), demo page; theming per UI.md "Themes" (token contract doc, 4 built-in themes, theme file loading + live swap in kit/sol.js, settings tokens: accent derivation, density, radius, glass, font); then restyle Files to it | done |
| A2 | Files: the synced folder (design/apps/FILES.md): watch + scan, tree feed, blobs, conflict copies, keep-on-device vs online-only folders, share a subfolder, peers' shared folders, UI + actions | done |
| A3 | Notes: Markdown editor over `.md` files in the synced folder (`Notes/`): live preview, `[[wiki links]]` + backlinks, tags, fast full-text search, checklists, images from the folder; phone-first; actions + UI | done |
| A2b | Files: three-way merge for text files (line-based diff3 against the entry's `base`, stdlib Myers diff); conflict copy only for overlapping edits | done |
| A4 | Music: index audio files (tags from MP3/FLAC/Ogg/M4A, cover art) in your folder and peers' shared music folders, player UI with queue, streaming with seek | parked |
| A5 | Sol app (`apps/home`): people, devices, apps grid, Inbox, pairing with QR, settings | todo |
| A6 | Agent: providers, tool loop, Sol actions as tools, coding tools, sub-agents, loops/schedules, approvals, CLI/TUI + chat UI | todo |
| A7 | Android shell: build `sol` for arm64/x86_64, WebView on the Sol app, notifications, share target; test on the emulator | done |
| A7c | Android follow-ups: test pair/error/paste paths and "Start new" on the emulator; `sol run` on a joining device prints an empty name until joined (show "joining…"); shorten join/pair links (≈600 chars today; QR needs short) e.g. carry only the device's endpoint record digest + one address and fetch the rest after the handshake; test arm64 build on a real Pixel | todo |
| A8 | Tasks: tasks and lists as feed records (LWW map per task), due dates, today/upcoming, reminders via Inbox, shared lists; export/import Markdown checklists | todo |
| A9 | Calendar: events as feed records, month/week/agenda, invites to contacts; export/import `.ics` | todo |
| A10 | Movies: video library, posters, resume, streaming to peers (transcoding decision first) | todo |

Order: A1 → A2 → A3 → A4 while C1 → C7 run; A5 after C8; A6 after C7; A7 after C6; then A8–A10.
