# Changelog

## v0.2.0-beta.1 (pre-release)

The first public release of the rebuilt Sol. It replaces the earlier alpha (v0.1.0-alpha.6, a
Tor-only proof of concept for connecting your own machines) and is **not compatible** with it:
the wire protocol is new (v2), and identities, pairing and data are different. Install fresh.

Tested by its author on Linux, an Android emulator and an Android phone. No iOS. macOS and
Windows builds exist and pass the test suite but are lightly used. No independent security review.

### Core

- One identity per person with any number of devices: a person key, device keys and certificates,
  revocations, and a 24-word backup that restores who you are on a fresh device.
- Several transports at once with path ranking: WireGuard direct (NAT hole punching), TLS direct
  and Tor (bundled). A mutual handshake with identity bundles, then a stream mux where either side
  can open streams; liveness and self-healing of peer links.
- Pairing by link or QR code, for people and for your own devices ("Add to my Sol").
- Anonymous contacts: Tor only in both directions, never learning your address.
- Feeds (signed, replicated append-only logs with CRDTs) and blobs (chunked, range requests,
  multi-source) shared with your devices and the people you choose.
- Port sharing: share a local service with a person, open theirs on a local port.
- Setup, restore and removal in the browser. No accounts and no servers of Sol's own.

### Apps

- **Files**: a synced folder (`~/SolDrive`) with a watcher, keep-on-device or online-only folders,
  conflict copies and three-way merge for text files, sharing folders with people (read or
  write), send files to a person (one approval in their Inbox, saved under `Received/<name>/`),
  and a swipeable viewer for images, video, audio and PDF.
- **Notes**: Markdown files with live preview, `[[wiki links]]`, backlinks, tags, full-text
  search, checklists and images.
- **Tasks**: lists, due dates, repeats, flags, subtasks, quick add, reminders, shared lists,
  Markdown checklist import and export.
- **Calendar**: day, week, month and list views, time zones, repeating events, alerts, shared
  calendars, invites, `.ics` import and export.
- One design system across apps (light, dark, accent, density, wallpaper, themes), a Sol home
  with People, Devices, Inbox, Sharing and Settings, and every app installable as a web app.

### Android

- The full node in the APK (`arm64-v8a`; an `x86_64` build for the emulator), Tor included.
- Setup, add to my Sol by QR code, restore from backup words; the synced folder appears as "Sol"
  in the system Files app and every file picker; share text and files to Sol.
- Apps as their own tasks with launcher shortcuts and "Add to home screen".
- Local notifications (no push service, no Google Play services) with Approve and Deny.
- Optional: your Sol calendars in the phone's calendar apps (Etar, Fossify Calendar, any app that
  reads Android's calendar storage), both ways, through a local account and sync adapter; no
  Google account. Off until you turn it on in Calendar's menu.
- Edge-to-edge UI, PDFs in the system viewer, reconnection on network changes.

### Desktop

- `sol` starts in the background and returns; `sol stop`, `restart`, `logs`, `autostart`,
  `version`; the first run sets itself up and opens Sol signed in.
- `sol open <app>`, app-menu entries per app on Linux, installable web apps.
- Local desktop notifications for Inbox items (`notify-send`, `osascript`, Windows toast) and a
  read-only `.ics` subscription file per calendar for desktop calendar apps.
- `install.sh` for Linux and macOS (checksum verified, restarts a running Sol after an upgrade).

### Agents

- Every action of every app is scriptable: `sol do <app>.<action>` with `--json`, `--dry-run` and
  generated `--help`.
- Scoped agent keys (`sol agent new|list|revoke|renew`, also in Settings) and Inbox approvals for
  destructive actions and for sending files.
- `sol events` streams Inbox and app events; no polling anywhere.
