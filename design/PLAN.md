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
| C3b | Wire follow-ups from PROTOCOL.md "Settled in C3": 1 MiB bundle frames, join `name`, `recover` op, push bundle changes, persist the identity cache; then port the PoC's transport ranking, probation, hedged dialing, sleep/network-change detection | design/PROTOCOL.md | done |
| C4 | Feeds: store, signing, append/read, replication to own devices and sharers, catch-up, events stream | API.md §3, §5 | done |
| C5 | Blobs over sessions: chunked fetch, Range, multi-source, pin, permission rule | API.md §4 | done |
| C6 | App host: manifest, install, supervise, app tokens, app socket routing, per-app origins, `/_sol/` bridge | API.md §1, §9, §10 | done |
| C7 | `sol do <app>.<action>` (args from flags/JSON, `--json`, `--dry-run`, generated `--help` from schemas), agent keys (`sol agent new/list/revoke`, certs per IDENTITY.md, `--as-agent`/`SOL_AGENT_KEY`), Inbox approvals end to end, `theme` events pushed (live swap), `sol backup words/confirm` + `sol restore`, join device names, an SDK method for GET /v1/inbox | API.md §6–8, IDENTITY.md | done |
| C6b | C6 follow-ups: ~~peer app UIs (`<app>.<petname>.localhost`)~~ (done in C8; the `/_sol/` bridge on a peer origin is still `not_implemented`), ~~my devices learn contacts' endpoints~~ (done in C3b: bundle relay), Tor client auth (derived per-device keys, PROTOCOL.md §Tor client authorization), offline scenarios in the conformance suite (real daemons behind a network gate), drop CSP `style-src 'unsafe-inline'` (classes, CSSOM, sol.js `data-var-*`; browser suites run under the style policy), v3 pairing links ≤ 200 chars | API.md §10, PROTOCOL.md | done |
| C7b | C7 follow-ups: theme file watching on macOS/Windows (scan fallback), themes sync between devices (Files ignores `.sol/`: sync `.sol/themes` or move themes to an internal feed), `theme.*` actions, re-add contacts after restore, agent list across my devices, `sol do` human output picks sensible default columns per action (manifest hint) | done, except contacts after a restore: they live only in my own devices' `sol/contacts` feed, so a restore from the words cannot learn them (it starts a fresh one-device list, even surviving devices are not re-adopted); re-pair each contact (tested) | done |
| C9 | Background mode and autostart: `sol`/`sol start` daemonizes and returns (like the PoC), `sol stop`, `sol restart`, `sol logs [-f] [-n N]`, `sol autostart on|off` (port ~/solbak/internal/autostart: systemd user unit, launchd, Windows later); default data dir away from the PoC's `~/.sol` until replaced; `sol open` after start | done |
| C9b | Zero-friction start: plain `sol` on a fresh machine auto-inits (name from the OS account, default dirs), starts in the background, opens the Sol app logged in, returns the terminal; same-OS-user auto-login at `localhost:<port>` (map the browser's TCP connection to its owning UID: /proc/net/tcp(6) on Linux/Android, GetExtendedTcpTable on Windows, best effort on macOS; other users still need a login code); a desktop launcher entry "Sol" (Linux .desktop, macOS app stub later) that runs `sol open`; clearer gate copy ("This browser isn't signed in to Sol. Open Sol from your app menu, or run `sol open`."); update ADMIN.md "Who may use it" | done |
| C10 | BEFORE the test release: default transports. New setups (init + first run) enable WireGuard direct (hole punching), TLS direct and Tor together (path ranking already picks the best); `--transport` becomes repeatable / a list; `sol transports list|enable|disable` for existing setups + admin endpoint + Sol app Advanced toggle. Bundle tor 0.4.9.16 (Tor Browser 16.0.1 expert bundles, signed by EF6E286DDA85EA2A4BA7DE684E2C6E8793298290; Linux/macOS need their shared libs extracted next to tor) via internal/runtime's bundled provider, native tor as fallback; Android uses its own tor build. e2e: two daemons on different simulated networks (faultnet) connect via wg; with wg blocked they fall back to Tor (fake tor); real-Tor test behind SOL_TEST_TOR=1. Per-person privacy: my devices and buddies (the default) use WireGuard direct first, then TLS direct, Tor last; an **anonymous** contact is Tor-only both ways: anonymous invites (`sol invite --anonymous`, a toggle in Add a person) carry only the onion, endpoint records sent to them contain only onion entries (never wg/direct/LAN addresses, also not via my devices' relaying), their direct dials are refused, `sol people set <name> --anonymous|--buddy` + the person page toggle (switching to anonymous warns that the IP already seen can't be taken back); document in PROTOCOL.md and IDENTITY.md; e2e asserts no non-onion address ever reaches an anonymous contact | done |
| C11 | From the security review: fix F6 (Windows: app sockets are loopback TCP, other OS users can forge Sol-Self — use per-app random-path named pipes or a per-request HMAC from the daemon, see SECURITY_REVIEW.md); fix the e2e tests that time out under load (TestRestartRedials, TestLastingTunnelAndUnpair: wait on events/conditions, not timing); run the whole suite with -count=3 to prove no flakes; also: right after pairing both sides may dial and a request riding the session the tie-break closes fails once with 502 (TestPeerAppUI saw it 4/20): retry a peer proxy request once on a closed session | todo |
| C10b | C10 leftovers: log the bundled tor's version at start; anonymous links carry a flag so the redeeming side also treats me as anonymous (Tor-only both ways) | done: every provider (platform, bundled, native) logs `starting tor` with path and `--version` once; v3 links gain a flags field (tag 6, anonymous bit; unknown fields/flags refused as "newer Sol", no version bump); the redeemer adds the inviter as anonymous and sends its onion-only bundle in the handshake; e2e `TestAnonymousBothWays` |
| C8 | Admin API per design/ADMIN.md: browser login (one-time code, cookie, Origin/Host), all endpoints + SSE events, port sharing services + tunnels (port PoC tunnels to `/p/service`), `sol open`, serve `ui/home/` at the UI root | design/ADMIN.md | done |

### Apps lane

| ID | Ticket | Status |
|---|---|---|
| A1 | `sdk/go/sol` client + `memsol` fake (whole API.md) + CRDTs (LWW map, list, block text) with tests; `sdk/js/sol.js` + `crdt.js`; `ui/kit` (tokens, base components, dark/light) | done |
| A1c | UI kit v2 to design/UI.md: tokens, Inter fallback font vendored, Apple-style components (shell, grouped lists, sheets, menus, segmented, toggles, squircle icons, widgets, dock), demo page; theming per UI.md "Themes" (token contract doc, 4 built-in themes, theme file loading + live swap in kit/sol.js, settings tokens: accent derivation, density, radius, glass, font); then restyle Files to it | done |
| A2 | Files: the synced folder (design/apps/FILES.md): watch + scan, tree feed, blobs, conflict copies, keep-on-device vs online-only folders, share a subfolder, peers' shared folders, UI + actions | done |
| A3 | Notes: Markdown editor over `.md` files in the synced folder (`Notes/`): live preview, `[[wiki links]]` + backlinks, tags, fast full-text search, checklists, images from the folder; phone-first; actions + UI | done |
| A2b | Files: three-way merge for text files (line-based diff3 against the entry's `base`, stdlib Myers diff); conflict copy only for overlapping edits | done |
| A4 | Music: index audio files (tags from MP3/FLAC/Ogg/M4A, cover art) in your folder and peers' shared music folders, player UI with queue, streaming with seek | parked |
| A5 | The Sol app UI in `ui/home/` per design/ADMIN.md + UI.md: home (wallpaper, widgets, app grid, dock), people (QR pairing), devices (join QR), inbox, sharing, settings; dev runner with a fake admin API | done |
| A5b | Sol app ↔ real daemon integration: the UI was built on the fake admin API and mismatches the real one (no name in greeting, "My devices 0", "No apps installed yet" while Files/Notes run, no storage widget, `?` avatar). Align JSON shapes (prefer fixing the daemon to ADMIN.md + home-dev shapes, RFC 3339 vs unix times), then add an automated real-daemon UI test: start a real daemon, log in via login code, load the page in headless Firefox and assert rendered data (e.g. the page POSTs a self-check to a test hook), part of `make browser-test` or a new `make ui-e2e`; fix the phone widget row overflow | done |
| A6 | Agent: ON HOLD until APV defines its purpose (design doc first, with APV). Direction so far: like Hermes, it creates and refines its own skills and scripts (reusable, readable, editable, shareable), and it should be purposeful rather than a general chat bot. Builds on `sol do`, agent keys and Inbox approvals (done) | parked |
| A7 | Android shell: build `sol` for arm64/x86_64, WebView on the Sol app, notifications, share target; test on the emulator | done |
| A7c | Android follow-ups: test pair/error/paste paths and "Start new" on the emulator; `sol run` on a joining device prints an empty name until joined (show "joining…"); ~~shorten join/pair links~~ (done in C6b: v3 links, ≤ 200 chars); test arm64 build on a real Pixel | done |
| A7d | Android leftovers: the daemon's netlink network watch is blocked on Android (retries every 5 s): fall back to the Java network callback only (`sol network-changed`) and stop retrying; test on a real Pixel (arm64 build) | done on the emulator: a permission error (EACCES) from the netlink bind or from listing the interfaces stops netwatch's scan, Tor's address poll and the direct transport's interface listing for good, logged once; the Java callback plus the clock-jump detector remain. Emulator: SELinux bind denials went from ~2 per 5 s to 2 at start; airplane mode off ran `network-changed` (sessions checked, tor restarted). Still to check on a real Pixel |
| A8 | Tasks: tasks and lists as feed records (LWW map per task), due dates, today/upcoming, reminders via Inbox, shared lists; export/import Markdown checklists | done |
| A9 | Calendar: events as feed records, month/week/agenda, invites to contacts; export/import `.ics` | done: calendars (one feed each, CRDT map), timed/all-day events with zones, RRULE subset with this/following/all edits, alerts via Inbox, read/write sharing, invites + RSVP, `.ics` export/import (idempotent by UID), day/week/month/list UI. Not yet: drag to move/resize in the grid, a search field and an invite picker in the UI (the actions exist), VTIMEZONE blocks in exports |
| D1 | Dogfood round 1 (APV's Android test): Android edge to edge (insets as --sol-inset-*, no black bars), restore from backup words on the welcome screen, PDFs open once in the system viewer (cache + OpenProvider, `SolAndroid.open`), SolDrive as "Sol" in the system Files app (DocumentsProvider), original logo everywhere; Sol app: wallpaper setting + custom wallpapers, custom accent picker, themes actually apply (dark tints, mono font, live swap), honest storage (Sol folder size + free space), friendly session names, sign out stays signed out (hidden in the Android app), agent keys from the UI; sidebar navigation (no dock), phone tab bar with Apps, compact headers, `/_sol/nav` + a way back to Sol in every app; Files: full-screen swipeable viewer (PDF framed on desktop, native on Android), folder chooser for move/copy, undo delete, `files.sync`, empty-folder rename/delete no longer resurrected; pairing persists the peer's bundle (a restart right after pairing redials); `install.sh` (curl \| sh, PATH setup) + README install docs | done. Left: Android Share bridge, kit toast actions, monochrome launcher icon, device test of Files upload/swipe on a real phone |
| D2 | Dogfood round 2: setup in the browser (Start new / Add to my Sol / Restore, also after a removal), Remove Sol from this device (desktop: identity archived, synced folder kept; Android: native confirm, erase, welcome), builtins added/refreshed on upgrade, Android multi-file upload (ClipData), `sol reset` | done. Left: `sol do` right after start can hit an app still booting ("app is not answering"); Android restore screen lacks the optional link from a surviving device |
| M1 | Apps installable as web apps: daemon serves `/_sol/manifest.webmanifest` (from app.json + theme, PNG icons rendered from the SVG) and `/_sol/sw.js` (app-shell cache, "Sol isn't running" page) on every app origin; sol.js links and registers them (design/APPS_EVERYWHERE.md) | done: manifest, icons (192/512 PNG + SVG, pre-rendered by `make icons` / cmd/iconrender with headless Firefox, embedded per app), sw.js; served without the sign-in gate (exactly those GET paths); Firefox registers the worker |
| M2 | `sol open <app>`; per-app app-menu entries kept in step with installs | done: `sol open [--print] <app>`; `sol launcher on` writes `sol-app-<id>.desktop` per app (Linux), `sol run` refreshes them via `daemon.Options.OnApps` when apps change |
| M3 | Android: "Add to home screen" pinned shortcuts per app, dynamic shortcuts on Sol's icon, one task per app (own recents entry, Back leaves) | done (AppActivity, Shortcuts, SolWeb; bridge `SolAndroid.addToHomeScreen`) |
| M4 | Calendar sync adapter (Sol calendars in the phone's calendar apps), desktop `.ics` subscription URL, notification channel per app with Approve/Deny | notifications done (local only: `sol events` + NodeService, no push service); Android calendar sync adapter done (AOSP only, two-way, change-driven; the in-app switch still to place); `.ics` URL todo |
| M5 | Third-party Android apps: `app.sol.action.CONNECT` intent → approval → app token + API URL | todo (design with its first real app) |
| A10 | Movies: video library, posters, resume, streaming to peers (transcoding decision first) | todo |

Order: A1 → A2 → A3 → A4 while C1 → C7 run; A5 after C8; A6 after C7; A7 after C6; then A8–A10.
