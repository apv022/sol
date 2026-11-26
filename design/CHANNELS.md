# App channels: installing and updating apps over Sol

Status: design, agreed direction (APV, 2026-11-26: "Sol-native channel"), not built yet.

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

## Why not GitHub releases checked daily

Simpler, but it polls (AGENTS.md rule 5), depends on one company, and every node asks GitHub
about every app it has. Mirrors keep GitHub useful as a byte source without making it the
authority: the signature, not the host, says what a release is.

## Open questions

- A followed channel's onion is down for a long time: followers still learn of releases from
  each other only if they share a contact path. Good enough to start.
- Revoking a leaked channel key: a signed "channel moved" record naming the new key, honoured
  only if signed by the old key before the leak is noticed. Not in the first version.
