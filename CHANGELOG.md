# Changelog

## v0.2.0-beta.4 (beta, unreleased)

- **Your name and picture.** Settings has a profile card: change your name or picture, and your
  devices and contacts see it. Anonymous contacts get your name but never your picture. A phone
  passes the change to the device that set Sol up, so that one must be online.
- **Nearby: devices on the same network find each other.** Your devices and your buddies'
  devices on one Wi-Fi or LAN now connect directly even when their addresses changed while they
  were apart, or the internet is down: each sends a small private beacon that only they can
  recognise (nothing in it identifies you to anyone else; anonymous contacts never see it).
  `sol do sol.nearby --state off` (or `sol transports disable nearby`) turns it off; `sol
  transports list` shows it.
- **Public onion services in the Sharing page:** host a local port on Tor, copy its address,
  remove it.
- The "It is a website" switch in "Share a service" was squashed under its label.
- Switching themes quickly could leave the previous theme showing, or put back an earlier
  setting.

## v0.2.0-beta.3 (beta)

- `sol start` printed `UI: http://localhost:0/` when the UI hadn't opened yet; it now waits for it.
- A setup that found port 7777 taken (often by the old proof of concept) used a new random UI port
  on every start, breaking bookmarks and installed web apps. Sol now takes 7777 when it's free,
  and otherwise keeps the port it got.
- `sol autostart on` replaces the old proof of concept's start-at-login entry (it stops it first)
  instead of refusing with "set up for another data directory".
- Port sharing: a person could miss a service you shared right after pairing. Two offers in
  flight could arrive out of order and the older (without the service) won; offers are now
  numbered and an older one never replaces a newer one, and a failed push is retried on the
  current connection.
- **Files: Trash and version history.** A delete anywhere (the Files app, `sol do files.delete`,
  `rm` in a terminal, on any of your devices) now goes to a Trash that every device shows the
  same: restore it (folders too), or delete it forever. Earlier contents of a file (edits, syncs
  from another device, merges) are kept as versions you can download or restore. Kept 30 days and
  10 versions per file by default (`sol do files.trash_settings`). "Delete forever", emptying the
  Trash and expiry free the disk space on every device, the always-on one included.
- **Connected Android apps in the Apps grid.** Raypass and other apps that connected to Sol on
  your phone show next to Files and Notes with their own icon, and open when you tap them.
- **Public onion services.** `sol do sol.onions.host --name blog --port 8080` publishes a local
  port at a fixed `.onion` address anyone can reach (Tor must be on); `sol.onions.remove` takes it
  down and deletes the address for good. Sol's own ports can't be published.
- **Notes:** a tap below the text of a short note starts editing (on phones it did nothing). A
  note started with "New note" is no longer saved as `Untitled.md`: leaving it names the file
  after its first heading. `sol do notes.tidy_names` renames the `Untitled*.md` notes you already
  have. The Notes app no longer checks for changes every 3 seconds; changes are pushed to it.
- **App icons** have readable contrast (the white glyph on the lime and cyan icons was barely
  visible), a bolder glyph and one distinct colour per app.
- **Security: approvals no longer keep secrets on disk** (#4). When an agent's call needed your
  approval, its arguments and the action's output stayed in `inbox.json`. Now only a digest and
  the outcome are kept; the output is handed to the waiting app or agent once, from memory. Apps
  can mark arguments `"secret"`, shown as `••••` when you approve.
- Agents: Sol's own actions (turning a transport off, hosting an onion) now ask for your approval
  in the Inbox when an agent calls them, like every app's actions; before, agents skipped it.
- Sol starts when port 7777 is taken by another program (on a free port for that run) instead of
  stopping.
- Sharing: a device of a contact's that was behind could take back a share their other device
  had just made; share announcements are now versioned.

## v0.2.0-beta.2 (beta)

- **Android: folders are online only by default, as designed.** Files never learned it was
  running on a phone, so phones downloaded every folder. Now a phone keeps only `Notes/` (Notes
  reads it from disk) plus folders you choose with "Keep on this device"; everything else is
  fetched when you open it. Files already downloaded stay until you set that folder to "Online
  only".
- Test reliability on macOS and Windows CI runners; sol-mobile README images and a note that
  Android pull requests go to apv022/sol.

## v0.2.0-beta.1 (beta)

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
- Other Android apps can become Sol apps: they ask to connect (`app.sol.action.CONNECT`), you
  approve their permissions on a native screen tied to their signing certificate, and they get
  their own revocable key for the app API (Settings, Connected apps).

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
