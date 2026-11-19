# Sol: architecture

Status: draft for discussion. Read [VISION.md](VISION.md) first. Where this reuses the current
proof of concept (the code in this repo today), it says so.

## The pieces

```
 ┌──────────────────────────────────────────────────────────────────────┐
 │  Shells: desktop app · Android app · browser                          │  one UI
 ├──────────────────────────────────────────────────────────────────────┤
 │  Apps: files · notes · tasks · calendar · music · movies ·            │  separate
 │        chat · agent · (anyone's app)                                  │  processes
 │        each = backend binary + web UI + action schema + CLI           │
 ├──────────────────── app API (local socket) ──────────────────────────┤
 │  Sol daemon                                                           │
 │   ├─ Identity   person key → device keys → app/agent keys            │
 │   ├─ People     contacts, devices, what each one shares               │
 │   ├─ Sync       per-app signed logs, pushed live, caught up offline   │
 │   ├─ Blobs      content-addressed, chunked, fetched from any holder   │
 │   ├─ Actions    registry of every app's actions, routing, approvals   │
 │   ├─ Router     opens app UIs, one browser origin per app             │
 │   └─ Reach      sessions to peers over the best path                  │
 ├──────────────────────────────────────────────────────────────────────┤
 │  Transports: LAN · WireGuard direct + hole punching · relay through   │  reused from
 │  your own devices · Tor                                               │  the PoC
 └──────────────────────────────────────────────────────────────────────┘
```

**The Sol daemon** is one Go binary on every device. It is the only thing that touches the network,
keys or peers. It stays small and audited.

**Apps** are separate binaries that talk to the daemon over a local socket. An app never opens a
network connection of its own, never holds the person key, and never knows which transport is in
use. A bug in an app can't take the network or the identity down with it.

**Shells** show apps. The UI is web technology served locally, so the same UI runs in the desktop
app, inside the Android app, and in a browser.

## Identity: keys in layers

A single key that does everything is a single point of failure: if it leaks, you're done. So
identity is a hierarchy, built in from day one because changing it later means everyone re-pairs.

| Key | Lives | Signs | If it leaks |
|---|---|---|---|
| **Person (root)** | offline: the 24-word backup, optionally one trusted device | device certificates, revocations | rotate to a new root; your contacts confirm once |
| **Device** | each device, `0600` | sessions, app and agent certificates, sync log entries for that device | revoke that device from any other; the rest keep working |
| **App / agent** | on the device, per app | that app's data and actions only, within its scopes, with an expiry | revoke that one key; nothing else is affected |

Contacts trust your person key once; your devices, apps and agents are proven by a chain of
certificates back to it. The root signs rarely, so it can stay offline.

## People and sharing

- **Contacts** are people, not machines. Pairing once with a person covers all their devices,
  because their signed device list travels with them.
- **Your own devices** trust each other fully and share your contacts and app data.
- **Sharing** is one model for every app: "this person (or group) can read / write this thing".
  The daemon enforces it; apps only ask "who is calling?" and get a verified answer.
- **Discovery** widens slowly from real trust: a QR code or link; nearby on the LAN;
  introductions through a contact (the other side must agree); later, optional public listings
  for things you choose to make public. There is no global list of people.

## Data: local first

Every app keeps a **local copy** of its data on each device that uses it. The UI reads the local
copy, so it is instant and works offline. The network only moves changes.

- **Sync logs.** Each app gets signed, append-only logs: one per device per app ("feeds"),
  readable by whoever it's shared with. The daemon pushes new entries to subscribed peers the
  moment they're written and catches peers up from a sequence number when they come back.
  This replaces polling everywhere, including the PoC's "Check again" button.
- **Each app's data in the model that fits it.** Files is a synced folder on disk (see
  `apps/FILES.md`); Music and Movies read media from it. Notes, Tasks and Calendar keep records in
  feeds and merge with CRDTs from the SDK (each edit is a feed entry), so concurrent edits on two
  devices or by two people merge per item, live. Rich text uses the SDK's block-document CRDT.
  Every app exports and imports a standard format, so data is never locked in.
- **Blobs.** Files, songs and movies are stored by SHA-256 hash, in chunks, so they can be streamed
  with seeking and fetched from any device of anyone who already has them and shares them with
  you. The PoC's blob store is the starting point.

## Availability: who is on when you're off

Every device is equal in the protocol. **Always-on is a role, not a requirement.** A person can mark
one or more devices (a home server, a Pi, a VPS, a friend's node that agreed) as **anchors**.
Anchors keep a copy of what you chose to make available, hold incoming messages until your other
devices come back, and answer for you while your laptop sleeps. With no anchor, Sol still works:
you're just offline when all your devices are.

There is no IPFS and no global storage network: availability comes from your devices and people
who agreed to help.

## Actions: agent native by construction

Each app declares its **actions** in a schema: name, typed inputs and outputs, description,
whether it changes something, whether it can be undone, whether it needs approval.

From that one schema the SDK generates:

- the **CLI** (`music play`, `tasks add`), with `--json`, `--dry-run` and full `--help`;
- the **local HTTP API** the app's own UI calls;
- the **reference docs**.

So the UI, the CLI and an agent are three clients of the same code. Nothing exists only as a
button. The daemon routes actions between apps and devices, checks the caller's key and scopes,
records every change in a signed activity log, and turns "needs approval" into an Inbox item
on your phone.

**The agent** is a first-party app: it holds an agent key with scopes and an expiry, calls other
apps' actions through the daemon, and works with any model (local or an API key you give it).
The daemon itself never runs a model.

## Reach

Reused from the PoC, which already works: LAN, WireGuard direct with hole punching, TLS direct,
and Tor. New: relaying through one of your own reachable devices when two peers can't connect
directly, before falling back to Tor. Each peer connection is one long-lived session that carries
sync, blobs, actions and app streams together, and heals itself after sleep or a network change.

Any local service (an app you didn't write) can still be shared to a peer as today, which is what
makes Sol a Tailscale replacement on day one.

## The SDK and the apps

- **Go SDK** for app backends: identity, feeds, blobs, actions, sharing, and the code generator for
  CLI, API and docs.
- **UI kit**: one design system and component set shared by every app, so the suite looks like one
  product and each app stays small.
- Each app is one repo (or one folder in an apps monorepo): a Go backend, a UI, an action schema.
  No dependencies beyond the stdlib; UIs use plain JS and the UI kit.

## Build order

1. **Specs** for identity, the wire protocol, sync, blobs, actions and the app API.
2. **Sol daemon**: identity, people, sync, blobs, actions, reach (porting the PoC transports).
3. **SDK + UI kit**, proven by a first app.
4. **The Sol app**: people, devices, apps, Inbox.
5. **Apps**, in an order that exercises the platform: files (the synced folder), notes, music
   (streaming from peers), the agent, then tasks, calendar, movies.

## Open decisions

1. **Settled:** v1 uses the SDK's block CRDT (blocks merge, same-block edits last-writer-wins);
   a character-level text CRDT can replace it later behind the same SDK API.
2. **UI stack:** Preact + HTM with no build step (very light, harder for a large design system), or
   Svelte or Solid with a build step (still light, nicer for a component kit).
3. **Desktop shell:** Wails (Go + the system webview) or Tauri (Rust).
4. **App packaging:** a plain binary per app that the daemon supervises, or WASM for untrusted
   third-party apps later.
5. **Movie streaming over slow paths:** stream the original file only, or transcode on the anchor
   (heavier, but needed for Tor and mobile).
