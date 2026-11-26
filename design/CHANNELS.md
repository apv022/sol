# App channels: installing and updating apps over Sol

Status: agreed direction (APV, 2026-11-26: "Sol-native channel"). First version built
2026-11-26 (daemon, CLI, actions; desktop installs): see "As built" at the end for what it settles
and what is left (the UI, APK installs by the Android shell).

Today an app is installed by hand (`sol app install <dir>`) and an Android app that connects to
Sol (Raypass) is installed and updated outside Sol. Every update means downloading again, and
on Android an APK signed with a different key can't even replace the old one. A **channel** lets
a developer publish releases from their own Sol node, and lets every node that follows it learn
of a new release the moment it is published (pushed, never polled), verify it, and install it.
No app store server and no account: the channel is a key, and the releases are signed by it.

## The pieces

- **Channel key.** An ed25519 key the developer makes once (`sol channel new raypass`). Its
  public key is the channel's identity. It is not the developer's person key: it can live in CI,
  and losing it means making a new channel, not a new identity. Kept like an agent key (0600).
- **Release record** (signed by the channel key, encoded like IDENTITY.md objects):
  ```
  lp("sol app-release v1") | lp(C_pub) | u64(seq) | lp(app_id) | lp(version) | u64(time)
    | lp(notes) | list of artifact: lp(platform) | lp(kind) | lp(sha256) | u64(size) | list of lp(mirror_url)
  ```
  `platform` is `linux-amd64`, `darwin-arm64`, `windows-amd64`, `android` …; `kind` is
  `sol-app` (a tar of a Sol app directory: app.json + executable + UI) or `apk`. `mirror_url`s
  are optional HTTPS copies (a GitHub release asset, say): bytes from anywhere are fine because
  the hash is signed. The highest `seq` wins; a record with a lower or equal `seq` is ignored.
- **Publishing.** The developer's node serves the channel as a **public onion service** (ADMIN.md
  §Public onion services, built in this round): `GET /channel` (the latest records), `GET
  /channel/stream` (server-sent events: one per new record, so followers are pushed) and `GET
  /blob/{sha256}` (artifact bytes). `sol channel publish --key K --app raypass --version 0.2.0
  --artifact android=raypass.apk …` signs the record, stores the blobs and announces it. CI can
  sign with the key and push the record and blobs to the developer's node over its admin API, or
  only to the mirrors.
- **Following.** A channel link is `sol://channel?k=<C_pub>&o=<onion>` (also a QR). `sol do
  sol.apps.follow --link …` records it in an internal feed, so every device of mine follows the
  same channels. One device of mine (an anchor if I have one, else any) keeps the stream open
  over Tor with backoff on failure, and writes new records into that feed, so my other devices
  learn of them over sessions without each holding a Tor stream. Releases also travel from
  follower to follower: a node that has a record and its blobs serves them to its own devices.
- **Trust.** Trust on first use, per app: installing an app from a channel pins the channel key
  in the app's binding. An update must be signed by that key, carry a higher `seq`, and match
  the hash it names. For an `apk`, the APK's signing certificate must also equal the one Sol
  bound the app to when it connected (API.md §External Android apps), so a channel can't replace
  an app with someone else's build. Switching a channel or key is an explicit reinstall.
- **Installing and updating.**
  - Desktop `sol-app`: the daemon downloads (onion or mirror), checks the hash, unpacks into a
    staging directory, validates the manifest (same id), swaps it in, restarts the app, and keeps
    the previous version for one-step rollback (`sol do sol.apps.rollback --id raypass`).
  - Android `apk`: the shell downloads and checks it, then hands it to `PackageInstaller`. Sol
    needs `REQUEST_INSTALL_PACKAGES`; the first install asks the person; later updates of apps Sol
    installed can skip the prompt on Android 12+ (`USER_ACTION_NOT_REQUIRED`), otherwise one tap.
  - Policy per app: `auto` (default for desktop apps), `ask` (default for Android apps: an Inbox
    item "Raypass 0.2.0 is ready", with Update), or `off`. Updates never run while the app is
    in the middle of a call it reported as busy.
- **Agent first.** `sol.apps.follow`, `sol.apps.channels`, `sol.apps.available` (from followed
  channels), `sol.apps.install --id`, `sol.apps.updates`, `sol.apps.update --id`, `sol.apps.auto
  --id --policy auto|ask|off`, `sol.apps.rollback --id`; installing and updating need approval
  when an agent asks.
- **UI.** The Apps page gains "Get more apps": the apps of followed channels with Install, and
  an Updates section; Settings › Apps shows each app's channel, version, policy and Rollback.

## As built (first version)

Code: `internal/identity/release.go` (the record), `internal/channels` (key files, links, the
artifact store, the HTTP surface and its stream, safe unpacking), `internal/daemon/channels.go`
(hosting, following, installing), `cmd/sol/channels.go` (`sol channel`). Admin endpoints:
ADMIN.md §App channels. What the first version settles, and where it differs from the text above:

- **Encoding.** `lp(sha256)` is the 32 raw digest bytes (shown as hex everywhere else). `time` is
  unix seconds. Bounds: notes 8 KiB, 8 artifacts (one per platform and kind), 4 mirrors each
  (`https://` only, no user info, 512 bytes), version 1-64 of `[0-9A-Za-z.+_-]`, a record 40 KiB
  in all, so it fits a feed entry base64-encoded.
- **Ordering is per channel and app.** "The highest seq wins" is per `(C_pub, app_id)`: one channel
  can carry several apps. A different record at an equal seq is refused (the first seen stays).
  `sol channel publish` picks `max(prev+1, now in unix seconds)`, so a record signed offline in CI
  (`--out`, no daemon to ask for prev) still moves forward.
- **The key never reaches a daemon.** `sol channel new` writes `<dir>/channel-keys/<name>.key`
  (0600, never overwritten); `sol channel publish` signs in the CLI and hands the record and the
  artifacts to the node (`POST /admin/v1/channels/blobs`, `/admin/v1/channels/hosted`). CI signs
  with `--out FILE` and hands over with `sol channel import FILE --blob ...`, or uploads the
  artifacts only to mirrors. Hosting needs Tor on.
- **The wire.** The channel's onion is a public onion `pub-channel-<name>` on port 80 (public
  service names starting `channel-` are therefore refused). `GET /channel` is
  `{"channel": ID, "records": [base64 wire form, ...]}`, the latest per app. `GET /channel/stream`
  is server-sent events, `event: release` / `data: <base64>` for every current record first, then a
  `: ready` comment, then each new record as it is published, and `: keepalive` every 30 s.
  `GET /blob/{sha256 hex}` serves the bytes.
- **Following.** My internal feed `sol/channels` holds `follow`/`unfollow` entries (last writer
  wins, in the feeds' order) and `release` entries (the record, base64). Every device re-verifies
  a replicated record before using it. The stream holder is the lowest-ID anchor among my devices
  that are up as this device sees them, else the lowest-ID device that is up; it is re-elected on
  every session, device and transport change. Two devices that cannot see each other may both
  hold it, which is harmless: a record already held is not written again. A broken stream is
  retried with jittered exponential backoff (1 s to 5 min). Unfollowing keeps the installed apps
  and their pins.
- **Not built: devices serving blobs to each other.** Records reach my devices over the feed, but
  each device downloads an artifact itself (the onion, then the mirrors). Relaying blobs between my
  devices over sessions is a follow-up.
- **Installing (desktop).** The artifact is the `sol-app` for `GOOS-GOARCH`. A tar may be gzipped
  and may hold one top-level directory. Unpacking refuses absolute names, `..`, backslashes,
  symlinks, hard links, devices and duplicates, and is bounded at 1 GiB and 20 000 entries. The
  manifest's `id` and `version` must equal the record's, and its `exec` must be a file of its
  own (`builtin:` and `external` are refused). An app already installed by other means is never
  replaced: remove it first. One automatic attempt per release; a failure is logged and waits for
  the next release or a manual `sol.apps.update`.
- **Rollback** keeps one previous version (`<dir>/channels/prev/<id>/`); rolling back twice goes
  forward again. The pinned seq stays at the newest installed, so the release rolled back from is
  not reinstalled automatically, only a newer one.
- **Ask** files an Inbox approval of `sol.apps.update --id <app>` (title "<Name> <version> is
  ready", the notes as body), once per release.
- **Android, not built: the install.** For an Android app connected to Sol (an external app) the
  daemon pins the one followed channel that offers an APK for that app id (trust on first use,
  policy `ask`), lists newer releases in `sol.apps.updates` and files a notice "<Name> <version>
  is available" once per release. `sol.apps.install` and `sol.apps.update` of an APK answer
  `not_implemented`. The record carries no package name, so the match is by app id; the Android
  shell, when it installs, must download the APK (onion or mirror), check its SHA-256 against the
  record, check that its package and signing certificate equal the app's binding (API.md
  §External Android apps), hand it to `PackageInstaller`, and then report the installed release
  to the daemon so the pinned seq moves (an admin endpoint to add with it).
- **Not built: "busy".** The app API has no way for an app to report a call in progress, so an
  update does not wait for one yet.

## Why not GitHub releases checked daily

Simpler, but it polls (AGENTS.md rule 5), depends on one company, and every node asks GitHub
about every app it has. Mirrors keep GitHub useful as a byte source without making it the
authority: the signature, not the host, says what a release is.

## Open questions

- A followed channel's onion is down for a long time: followers still learn of releases from
  each other only if they share a contact path. Good enough to start.
- Revoking a leaked channel key: a signed "channel moved" record naming the new key, honoured
  only if signed by the old key before the leak is noticed. Not in the first version.
