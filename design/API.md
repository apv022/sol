# Sol app API, v1

Status: spec, being built. This is the contract between the Sol daemon and every app. Apps are
written against this file, not against the daemon's code. If the daemon and this file disagree,
the daemon is wrong.

## How an app talks to Sol

An app is any program the daemon starts (or that a developer starts by hand) with two
environment variables:

| Variable | Meaning |
|---|---|
| `SOL_API` | where the daemon listens: `unix:///path/to/api.sock` (Linux, macOS, Android) or `http://127.0.0.1:PORT` (Windows, tests) |
| `SOL_APP_TOKEN` | this app's secret. Sent as `Authorization: Bearer <token>` on every call. Identifies the app and its scopes |

The daemon also gives the app its own socket to serve on:

| Variable | Meaning |
|---|---|
| `SOL_APP_LISTEN` | where the app must serve HTTP: `unix:///...` or `127.0.0.1:PORT`. The daemon sends the app its UI requests, action calls and incoming streams here |
| `SOL_APP_DATA` | a private directory for the app's own files and local database (`0700`) |

Everything is HTTP/1.1 with JSON bodies, except blob bodies (raw bytes) and event streams
(`text/event-stream`). Errors are `{"error": {"code": "...", "message": "..."}}` with a matching
HTTP status. Codes: `bad_request`, `unauthorized`, `forbidden`, `not_found`, `conflict`,
`unavailable` (peer offline, try later), `not_implemented`.

Go apps use `sdk/go/sol` and JS UIs use `sdk/js/sol.js`; nobody needs to write raw HTTP. The SDK
also ships `memsol`, an in-memory fake of this whole API, so apps and their tests run without a
daemon.

## Names

| Thing | Format | Example |
|---|---|---|
| Person ID | base32 of the person public key (52 chars, lowercase) | `vg3bq57r…` |
| Device ID | base32 of the device public key | |
| App ID | `[a-z][a-z0-9-]{1,31}` | `files` |
| Feed name | `[a-z0-9][a-z0-9._-]{0,63}`, scoped to (owner person, app) | `tree`, `doc.4f2a` |
| Feed address | `<person>/<app>/<feed>` | `vg3b…/docs/doc.4f2a` |
| Blob hash | `sha256:` + 64 hex | `sha256:9f86…` |
| Action | `<app>.<verb>` | `files.upload` |

A **feed** belongs to one person and one app. Every device of that person may append to it. An app
can only write its own app's feeds, and read its own plus those of apps listed in its manifest's
`reads` (§9), within what is shared with me.

## 1. Identity: who am I, who is calling

### `GET /v1/whoami`
```json
{"person": {"id": "vg3b…", "name": "APV"},
 "device": {"id": "k3v5…", "name": "laptop", "anchor": false},
 "app": {"id": "files", "scopes": ["feeds", "blobs", "people", "dial", "actions"]}}
```

### Incoming requests carry the caller
Every request the daemon forwards to an app (UI, action call, stream) has:

| Header | Meaning |
|---|---|
| `Sol-Person` | verified person ID of the caller (`self` traffic carries your own ID) |
| `Sol-Device` | verified device ID |
| `Sol-Agent` | present when the caller is an agent key: its ID. Absent for humans |
| `Sol-Self` | `1` when the caller is one of your own devices or your own agent |

Apps decide by these headers, never by anything in the request body.

## 2. People: contacts and devices

### `GET /v1/people`
```json
[{"id": "…", "name": "John", "petname": "john", "online": true,
  "devices": [{"id": "…", "name": "john-pc", "online": true, "anchor": true}],
  "shares_with_me": ["music/library"], "i_share": ["files/shared"]}]
```
`GET /v1/devices` lists my own devices in the same device shape.

Pairing, removing people and naming them are done by the person in the Sol app (admin API), not
by apps. Apps see the result.

## 3. Feeds: synced, signed logs

A feed is an append-only list of entries. The daemon signs each entry with the device's app key,
stores it, pushes it to my other devices and to everyone the feed is shared with, and catches
offline devices up later. Apps build their state by reading feeds (and CRDTs on top of them, in the
SDK). **The daemon never interprets an entry's body.**

Entry:
```json
{"feed": "vg3b…/docs/doc.4f2a", "seq": 42, "device": "k3v5…",
 "time": "2026-11-19T21:03:11Z", "body": {…any JSON, max 64 KiB…},
 "hash": "sha256:…", "prev": "sha256:…"}
```
Under the hood each device of the owner keeps its own chain in the feed, so two devices
appending while offline never conflict (PROTOCOL.md §Feeds). `seq` counts from 1 per
(feed, device); `prev` chains to that device's previous entry, so a gap or a rewrite is
detectable; `hash` covers the signed entry. Apps don't usually care about `seq`: they read with
`pos`, a cursor that increases by one for every entry this device stores for that feed, in
arrival order. An entry returned by the API carries both `seq` and `pos`. Bodies over 64 KiB go in
a blob and the entry points at it.

### `POST /v1/feeds/{feed}/append`
Body: `{"body": {…}}`. `{feed}` is the feed name (own feeds only). Returns the stored entry.

### `GET /v1/feeds/{address}?from=POS&limit=N`
Reads entries with `pos >= from` (default 1, limit default 500, max 5000), in `pos` order. Works for my feeds and
for feeds shared with me (served from the local replica; never blocks on the network).

### `GET /v1/feeds?owner=PERSON`
Lists the feed addresses for this app that I can read, with their latest `pos`.

### `GET /v1/events?feeds=ADDR,ADDR&people=1&inbox=1`
Server-sent events. Event types:

| Event | Data |
|---|---|
| `entry` | a new entry in a subscribed feed (also catch-up entries after reconnect) |
| `feed` | a feed became readable or stopped being readable to me |
| `person` | a contact changed (online, devices, shares) |
| `inbox` | an Inbox item for this app changed |

Every event has an `id`; reconnecting with `Last-Event-ID` resumes without gaps. **There is no
polling anywhere in Sol.**

## 4. Blobs: files by hash

Blobs are immutable bytes addressed by SHA-256. The daemon stores them in chunks and fetches
missing ones from whichever of the owner's (or any sharer's) devices is online.

### `PUT /v1/blobs`
Body: raw bytes. Header `Sol-Name` (optional) is a hint shown in storage views. Returns
`{"hash": "sha256:…", "size": 123}`. Uploading the same bytes twice is free.

### `GET /v1/blobs/{hash}?from=PERSON`
Streams the blob. Supports `Range` (so music and video can seek). If the blob isn't local, the
daemon fetches it from `from` (or any person who shared a feed referencing it) and caches it.
`HEAD` returns size and whether it is local.

### `POST /v1/blobs/pin` / `POST /v1/blobs/unpin`
Body `{"hashes": [...]}`. A pinned blob is kept and copied to my anchors. Unpinned blobs that no
feed of mine references may be garbage collected.

**Who can fetch a blob:** a person can fetch a blob from me only if it is referenced (by hash,
anywhere in the body) by an entry of a feed I share with them. That is the whole blob permission
rule; there is no separate blob ACL.

## 5. Sharing

### `POST /v1/shares`
```json
{"feed": "tree", "people": ["<person id>"], "mode": "read"}
```
`mode` is `read` or `write`. Write means: the person's own feed with the same name is pulled in
and offered to this app as a co-author (collaborative apps merge the two). Sol never merges.

### `DELETE /v1/shares` (same body) and `GET /v1/shares?feed=NAME`

Sharing an app as a whole, or making a feed public, is the person's decision in the Sol app; apps
can request it through an Inbox approval.

## 6. Actions: everything a person can do, an agent can do

Each app declares its actions in its manifest (section 9). The daemon keeps a registry of every
installed app's actions and routes calls.

### Calling: `POST /v1/actions/call`
```json
{"action": "files.upload", "args": {"path": "/notes.txt", "blob": "sha256:…"},
 "person": "<optional: run on this person's app>", "dry_run": false}
```
Returns the action's output, or `{"approval": "<inbox item id>"}` when the action needs approval.

### Listing: `GET /v1/actions?app=files`
Returns the manifests' action entries with JSON Schemas, for UIs, the CLI and agents.

### Being called
The daemon sends `POST /_sol/actions/{name}` to the app's own socket with the args as the body and
the caller headers from section 1. With `Sol-Dry-Run: 1` the app must describe what it would do
and change nothing.

### The same actions everywhere
- **CLI:** `sol do files.upload --path /notes.txt --blob sha256:… [--json] [--dry-run]` and
  `sol do --help files` (generated from the schemas).
- **Agents:** use the same CLI with their agent key (`SOL_AGENT_KEY` or `--as-agent`); the daemon
  enforces the agent certificate's scopes, expiry and approvals. There is no separate agent protocol.
- **UI:** the app's own UI calls its actions through `sol.js` like any other client.

The daemon writes every action that changes something to a signed activity log (who, which key,
args digest, result), readable through `GET /v1/activity`.

## 7. Dial: live streams to a peer's app

For things that aren't data sync: streaming playback negotiation, calls, terminals.

### `POST /v1/dial`
```json
{"person": "…", "app": "music", "device": "<optional>", "path": "/stream"}
```
The daemon upgrades the HTTP connection (`101 Switching Protocols`, `Upgrade: sol-stream`) and
it becomes a raw byte stream to the target app. The target app receives it as an upgrade request
on its socket at `path`, with caller headers.

## 8. Inbox: notices and approvals

### `POST /v1/inbox`
```json
{"kind": "notice", "title": "Album added", "body": "…", "to": "self"}
{"kind": "approval", "title": "Delete 3 files?", "action": {"action": "files.delete", "args": {…}}}
```
`GET /v1/inbox` lists this app's items (and their answers) newest first.

Notices appear in the Sol app (and as an OS notification when `to` is `self` and the person allows
it). An approval is answered by the person on any of their devices; on approval the daemon runs the
stored action as the requesting app. Events of type `inbox` report the answer.

## 9. The manifest: `app.json`

Every app ships one. Installing an app is: put the executable and `app.json` in place and run
`sol app install <dir>` (or drop it in the apps directory). Removing it is the reverse. That is
the whole "plug and play" contract.

```json
{
  "id": "files",
  "name": "Files",
  "version": "0.1.0",
  "description": "Your files on all your devices, shared with people you choose.",
  "exec": "sol-files",
  "ui": "/",
  "icon": "icon.svg",
  "scopes": ["feeds", "blobs", "people", "actions"],
  "runs_on": ["any"],
  "actions": [
    {"name": "files.list", "description": "List a folder.",
     "input": {"type": "object", "properties": {"path": {"type": "string"}}},
     "effect": "read"},
    {"name": "files.delete", "description": "Delete files or folders.",
     "input": {"type": "object", "required": ["paths"],
               "properties": {"paths": {"type": "array", "items": {"type": "string"}}}},
     "effect": "destructive", "approval": "agents"}
  ]
}
```

| Field | Meaning |
|---|---|
| `exec` | program to start, relative to the app directory. First-party apps are built into the `sol` binary and use `"exec": "builtin:files"` |
| `ui` | path the app serves its UI on; omit for headless apps |
| `scopes` | API areas the app may use; the person sees them when installing |
| `runs_on` | `any`, or `anchor` for apps that only run on always-on devices (e.g. a music indexer) |
| `folders` | lens apps: subfolders of the synced folder the app may use on disk, with mode, e.g. `[{"path": "Music", "mode": "read"}]` or `[{"path": "*", "mode": "write"}]`. The daemon passes the folder root as `SOL_FILES` |
| `reads` | other apps whose feeds this app may read, e.g. `["files"]` (Music reads the Files tree to stream peers' music by blob hash) |
| `effect` | `read`, `write` or `destructive` |
| `approval` | `never` (default for read), `agents` (agents need approval), `always` |

## 10. Opening an app's UI

The daemon serves each app's UI at its own origin, `http://<app>.localhost:<port>/`, so apps
can't read each other's cookies or storage. Requests to that origin are passed to the app's
socket with caller headers (always `Sol-Self: 1`; the UI is mine). The path `/_sol/` on that
origin is reserved: it is the bridge `sol.js` uses to call this API with the app's own token,
after the daemon checks the `Host` and `Origin` headers.

`/_sol/theme.css` on every app origin is the active theme (design/UI.md "Themes"); a
`theme` event on the events stream tells open UIs to swap it live. `sol.js` and `kit.css` handle
this, so apps do nothing.

A peer's app is opened locally the same way: `http://<app>.<petname>.localhost:<port>/` is
proxied to that person's app over Sol.

## 11. What this API deliberately does not have

- No raw network access, no sockets to arbitrary hosts: apps reach people, not addresses.
- No access to keys, ever. Apps ask the daemon to sign (entries, actions).
- No merging, no schema for entry bodies: that is the app's job, helped by the SDK.
- No polling endpoints.
