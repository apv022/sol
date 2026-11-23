# Sol for Android

**Your own private network of devices and people, in your pocket.** Sol for Android runs the
full [Sol](https://github.com/apv022/sol) node on your phone: Files, Notes, Tasks and Calendar
that sync directly with your other devices and the people you add, with no account and no server
in the middle. The phone can also share its folder, send files to a person, and approve what an
agent asks for, from a notification.

<p align="center">
  <img src="shots/m3-longpress.png" alt="Long-press Sol's icon for Files, Notes, Tasks and Calendar" width="240">
  <img src="shots/m4-approve-deny.png" alt="An approval with Approve and Deny buttons in the notification shade" width="240">
</p>

> **v0.2.0-beta.1 is a pre-release.** Tested by its author on an Android emulator and a phone. There is
> no iOS app. Expect rough edges, and [report what breaks](https://github.com/apv022/sol-mobile/issues).

## Install

1. Download the APK from the [latest release](https://github.com/apv022/sol-mobile/releases)
   (pre-releases are on the same page). The release also has a `SHA256SUMS` file.
2. Open the file on the phone. When Android asks, allow installing apps from that source (your
   browser or file manager).

Needs Android 8.0 or newer and a 64-bit ARM phone. Android may warn about an unknown developer
because the app is not from a store. Updates install over the old version and keep your data, as
long as they are signed with the same key. An earlier alpha (a Tor-only proof of concept, signed
with a debug key) must be uninstalled first.

## First run

Open Sol and choose:

- **Start new** if this is your first Sol device. Give yourself a name.
- **Add to my Sol** to make this phone another device of the Sol you already have. On your other
  device open Devices, Add my device, and scan the QR code with the phone's camera (tap the
  `sol://join` link it shows), or paste the link into the field.
- **Restore from backup words** if you lost your devices and have your 24 words. Your identity
  comes back and your contacts keep trusting you; files sync back from your devices that are
  still online. (The words restore your identity, not your files.)

To add a person, open People, Add a person and let them scan your QR code, or scan theirs.

## Using it

- **Apps on the home screen.** Long-press Sol's icon for Files, Notes, Tasks and Calendar, or
  open the Apps page in Sol and add any app to the home screen. Each app opens full screen in
  its own entry in the recent apps list.
- **Notifications.** Things that need you (a file someone sent, an approval an agent is waiting
  for) arrive as notifications with Approve and Deny buttons, one channel per app so you can mute
  an app in Android's settings. They are generated on the phone: Sol uses no push service, no
  Firebase and no Google Play services, so nothing is sent to a third party. The flip side:
  you are notified while the Sol node is running and the phone has a connection, and anything
  that happened while it was offline is notified once it reconnects.
- **Your calendars in the phone's calendar apps (optional).** In Calendar's menu, "Show in phone's
  calendar apps" puts your Sol calendars into Android's own calendar storage, so Etar, Fossify
  Calendar or any calendar app and widget shows them, and edits made there come back to Sol. It
  uses a local "Sol" account, not a Google account, and asks for the calendar permission only
  when you turn it on. Reminders stay Sol's own notifications, so nothing alerts twice.
- **The "Sol" folder in Files.** Your synced folder shows up as "Sol" in Android's Files app and
  in every app's file picker, so other apps can open and save into it.
- **Share to Sol.** Share text or files from any app to Sol; they land in
  `Inbox-from-share` in your folder.
- **PDFs and other files** open in the phone's own viewer.

### Battery and the foreground service

Sol keeps its node running behind a persistent "Sol is running" notification (an Android
foreground service), and restarts it if it dies and when the network changes. That keeps your
devices reachable and syncing, and it costs some battery, mostly while a Tor connection is open.
If Android stops Sol in the background, open the app again and exempt Sol from battery
optimisation in Android's settings. There is **no VPN permission**: Sol does not take the phone's
one VPN slot.

### Permissions

Internet; foreground service and notifications (the running notification and the Inbox
notifications above); network state (to notice Wi-Fi/cellular switches); calendar, only if you
turn on calendar sync.

### Limits

arm64 only, no 32-bit build; not on Google Play or F-Droid (the APK is the only way to install
it); no iOS. More about Sol itself: [apv022/sol](https://github.com/apv022/sol#readme).

## Building

This repository is the Android shell of [apv022/sol](https://github.com/apv022/sol). It is kept
in the monorepo's `android/` folder and published here with `git subtree`; the Go node, the web
apps and the design docs live there. The rest of this page is for developers. Commands are
written for the monorepo (`android/build.sh`); in this repository the script is `./build.sh`, and
`SOL_SRC` points it at a checkout of apv022/sol to build the node from.


A thin Android shell around the same Go node (`sol`) that runs on a laptop. The APK ships the node as
`lib/<abi>/libsol.so` and Tor as `lib/<abi>/libtor.so`, a foreground service keeps the node running,
and one activity shows Sol's own UI in a WebView (`http://localhost:7777/`, apps at
`http://<app>.localhost:7777/`). There is no Gradle: the Java is built with `javac` + `d8`, packed
with `aapt2` and signed with `apksigner`.

### Build

You need Go, bash, zip, gpg, curl and a toolchain directory with JDK 17 and the Android SDK
(build-tools 35.0.0, android-35). The default is `~/sol-mobile/.toolchain`; point `TC=/path`
elsewhere to use your own.

```sh
android/build.sh                  # build/sol.apk: arm64-v8a (phones) + x86_64 (emulator), debug key
ABIS=arm64-v8a android/build.sh   # phones only
ABIS=x86_64 android/build.sh      # emulator only (a static linux/amd64 node)
SOL_SRC=/path/to/copy android/build.sh   # build the node from another checkout of this repo
```

The only download is Tor: the Tor Project's `tor-expert-bundle-android-<arch>-16.0.2.tar.gz` from
dist.torproject.org, fetched once per ABI into `$TC/dl`. Every build checks it again: the bundle's
own signature and the signed `sha256sums-signed-build.txt` must both be by the Tor Browser signing
key `EF6E286DDA85EA2A4BA7DE684E2C6E8793298290` (gpg runs in a throwaway `GNUPGHOME`), and the
bundle's hash must match the list. Any mismatch stops the build.

#### Release build

```sh
export SOL_KEYSTORE=~/.config/sol-release/sol-release.p12   # PKCS12 or JKS
export SOL_KEY_ALIAS=sol-release
export SOL_KEYSTORE_PASS="$(cat ~/.config/sol-release/keystore.pass)"  # env only, never argv
SOL_VERSION=0.1.0 RELEASE=1 android/build.sh                # -> build/sol-release.apk
```

`RELEASE=1` builds arm64-v8a only, writes `android:debuggable="false"` into the manifest and signs
with your key (the password reaches `apksigner` through the environment, so it never shows in `ps`
or a log). It stops before building if any of the three variables is missing or the keystore does
not exist. Check the result with `apksigner verify --print-certs build/sol-release.apk`.

The debug build signs with a throwaway key kept in the toolchain directory. Android only upgrades
an app in place when the new APK has the same signing key, so a phone runs either debug builds or
release builds; switching means uninstalling first, which deletes the phone's Sol identity.

### Install on a phone

1. On the phone: Settings, About phone, tap Build number seven times; then Developer options,
   USB debugging on.
2. Connect it by USB, accept the prompt on the phone, and check it shows in `adb devices`.
3. Install (the same command updates an installed Sol and keeps its data):

   ```sh
   adb install -r android/build/sol-release.apk   # or build/sol.apk for a debug build
   ```

Open Sol and choose **Start new** (a new person on this phone), **Add to my Sol** (this phone
becomes another device of the person you already are on your laptop) or **Restore from backup
words** (the 24 words from `sol backup words` or Settings: your identity comes back under the same
person ID, so contacts keep trusting you; files come back by syncing from your devices that are
still online). The words go to `libsol.so restore` as an argument only; they are never stored or
logged, and wrong words leave nothing behind.

#### Join your laptop

1. On the laptop open Sol (`sol open`), go to **Add my device**, and keep the QR code on screen.
2. On the phone choose **Add to my Sol** and give the phone a name (the model is the default).
3. Scan the QR code with the phone's camera app and tap the `sol://join` link it shows. Sol opens
   and joins at once. Without a camera, copy the link to the phone and paste it into the field.
4. When it says "Joined", the Sol app opens. Files and notes now sync with the laptop. While a
   phone waits for its link, the notification says "Joining…".

If you picked the wrong option, **Start new instead** on that screen deletes the half-made identity
(nothing has been shared yet) and goes back to the welcome screen.

To pair with *another person*, they show **Add a person**; scanning their QR (a `sol://pair` link)
asks "Pair with this person?" and, on Pair, makes you contacts. A link that is not a Sol link, has
expired or was already used shows what to do instead; nothing half-done is left behind.

#### Update

Build again and run `adb install -r` with the same kind of APK (release over release, debug over
debug). The node restarts with the app and keeps its identity, contacts and files.

### How it works

- **Node**: `NodeService` runs `libsol.so init` once (the default transports, WireGuard, TLS direct
  and Tor; the UI on 127.0.0.1:7777; files in `SolDrive`), then `libsol.so run` forever,
  restarted with a growing delay if it dies. Data lives in the app's private storage
  (`allowBackup=false`, so the device key never reaches a cloud backup).
- **Tor**: the node runs the APK's own tor; `SOL_TOR_BIN` points it at `libtor.so` in the
  native library directory (the only place an app may execute files from).
- **Login**: the daemon's same-user sign-in needs `/proc/net/tcp`, which Android's SELinux policy
  hides from apps, so the WebView signs in to the Sol app with a one-time code from
  `libsol.so open --print`, and keeps that session across restarts while the node accepts it.
  App origins (Files, Notes) are signed in from that session on first use: the activity makes
  the `/_sol/open` → `/_sol/login?code=` requests itself and stores the origin's `sol_app`
  cookie, because Chromium treats `files.localhost` and `localhost` as different sites and drops
  a SameSite=Strict cookie set through that redirect chain.
- **Network changes**: a new default network (Wi-Fi back, Wi-Fi to cellular) runs
  `libsol.so network-changed`, so peers are redialled and Tor restarted at once. The node's own
  netlink watch is denied to apps.
- **Share**: Sol is a share target for text and files (one or many). They are copied into
  `SolDrive/Inbox-from-share` and Files opens.
- **Edge to edge**: the WebView fills the screen under transparent status and navigation bars.
  Android's WebView does not report the bars as `env(safe-area-inset-*)`, so the activity sets
  `--sol-inset-top/-bottom/-left/-right` (CSS px) on `<html>` on every page commit, page load and
  inset change, for every local origin; `ui/kit/tokens.css` folds them into `--safe-*`. Only the
  keyboard pads the view natively. Bar icons are dark in light mode and light in dark mode.
- **Downloads**: a download the user asked for (`download=1`, or `Content-Disposition:
  attachment`) is saved to the Downloads folder under the file's own name. Anything else reached
  the download listener only because the WebView cannot show it inline (a PDF preview in Files):
  it is fetched once into `cache/open/` (keyed by URL, length, ETag and Last-Modified; reused when
  unchanged, pruned after a day) and opened in the system viewer through `OpenProvider`
  (`content://app.sol.open/...`, read-only, per-intent grant). The same URL opens once per 4 s.
- **Page bridge** `window.SolAndroid` (answers only while the top page is a local Sol origin):
  - `SolAndroid.isApp()` → `true` inside the Android app (`window.SolAndroid?.isApp` is truthy),
    so a page can hide what makes no sense here, such as Sign out (the app signs itself in again).
  - `SolAndroid.open(url, mime, name)` opens a file of this node in the system viewer as above;
    `url` may be relative to the page and must resolve to a local Sol origin; `mime`/`name` may be
    empty. Returns false when refused. Files can call it for PDFs instead of an `<iframe>`.
  - `SolAndroid.addToHomeScreen(appId, name)` asks the launcher to pin the app (`files`,
    "Files") to the home screen with its icon; the launcher shows its own confirmation. Returns
    false when refused (not a local page, a bad id, or a launcher without pinned shortcuts).
    Guard it with `window.SolAndroid?.addToHomeScreen`; the Apps page (`#/apps`) does.
  - `SolAndroid.calendarSync(on)` is the "Show in phone's calendar apps" switch: `true` turns
    calendar sync on (Android asks for the calendar permissions the first time), `false` turns it
    off and removes Sol's calendars from the phone. It returns at once whether the request was
    taken; the outcome arrives as a `sol-calendar-sync` event on `window` (`detail.on`).
    `SolAndroid.calendarSyncEnabled()` says whether it is on. Guard both with
    `window.SolAndroid?.calendarSync`. The Calendar app shows it in its import/export menu ("Show in phone’s calendar apps");
    debug builds also take `adb shell am start -n app.sol/.MainActivity --ez calendar_sync
    true|false`.
- **Apps in their own tasks**: `AppActivity` shows one app full screen (`sol-app://<id>`,
  `documentLaunchMode="intoExisting"`): its own entry in recents with the app's name and icon, no
  Sol tab bar, and Back at the app's start goes to the home screen. It shares the window code
  with the Sol app (`SolWeb`: edge to edge, sign-in, downloads, file picker, the bridge) and
  starts the node if it is not running. Before Sol is set up it opens the Sol app instead.
- **Launcher shortcuts**: long-pressing Sol's icon lists the node's apps (Files, Notes, Tasks,
  Calendar first; from `libsol.so apps --json`, refreshed when the node starts and when apps
  change), and any app can be pinned to the home screen (`addToHomeScreen`). Both go through
  `ShortcutActivity`, a windowless trampoline: the launcher starts shortcuts with `CLEAR_TASK`,
  which would otherwise restart an app that is already open. Icons are the node's
  `http://<app>.localhost:7777/_sol/icon-192.png` (cached in `files/icons/`), or a drawn square
  in a colour from the id with the first letter when the node has none.
- **Notifications** (local only, no push service: no Firebase, no Google Play services): while
  the node serves, `NodeService` runs `libsol.so events --json` (the admin event stream; restarted
  if it ends) and turns each new inbox item into a notification on a channel per app, named after
  it ("Sol" for Sol's own items), so each app can be turned off in Android's settings. Approvals
  have Approve and Deny, notices Dismiss (`InboxReceiver` runs `libsol.so inbox approve|deny|dismiss
  <id>`; a failure stays on the notification). An item answered anywhere else (another device, the
  web UI) cancels its notification; ids already notified are kept (the last 500), so nothing
  notifies twice. Tapping an approval opens Sol's Inbox, a notice its app. Things that happened
  while the phone was offline are notified once it reconnects and syncs. Android 13+ asks for the
  notification permission when the Sol app opens.
- **Calendars in the phone's calendar apps** (off until turned on; AOSP framework only: no Google
  account, no Play services, no push service). `CalendarSync` adds a local account of type
  `app.sol` named after the person (no password, nothing stored in it) and a sync adapter for
  `com.android.calendar`, so any app that reads Android's CalendarProvider (Google Calendar,
  Etar, Fossify, widgets) shows Sol's calendars and can edit them. `CalendarSyncer` does one sync:
  - *Upload first*: rows of the account that another app created, changed or deleted (`DIRTY` /
    `DELETED`) go to Sol with `calendar.add` / `calendar.update` / `calendar.delete` (JSON on
    stdin, `do <action> --args -`, so event text never reaches argv or the log), only the fields
    that changed (`SYNC_DATA2` holds what Sol last wrote). A changed occurrence is
    `update <id>@<occurrence> span this`, a cancelled one or a new `EXDATE` is `delete ... span
    this`, a new event gets Sol's id as `_SYNC_ID`. Changes to a read-only share are undone.
    Last writer wins; Sol's CRDT merges edits made on other devices.
  - *Then download*: `calendar.list_calendars` gives one Calendars row per Sol calendar
    (`_SYNC_ID` = calendar id, colour from the palette name, `OWNER` access for own and
    writable calendars, `READ` for read-only shares; a share is named "Name (Owner)").
    `calendar.export` per calendar gives the events with their rules (`.ics`, parsed by `Ics`):
    one Events row per event (`DTSTART`/`DTEND`, or `DURATION` + `RRULE` + `EXDATE` when it
    repeats; all-day at UTC midnight in zone UTC; `EVENT_TIMEZONE` the event's IANA zone) and
    one exception row per changed occurrence (`ORIGINAL_SYNC_ID`, `ORIGINAL_INSTANCE_TIME`).
    `SYNC_DATA1` hashes the exported VEVENT, so unchanged events are not rewritten.
  - *Alerts*: no Reminders rows are written. Sol's alerts already reach this phone as Inbox
    notifications (Notifier), so a calendar app's reminder would alert twice. A reminder a person
    adds in a calendar app becomes the event's Sol alert and the row is removed, so each alert
    fires once, from Sol.
  - *When* (no timer: the daily periodic sync Android adds is removed): when the node starts
    serving; when the Calendar app's own event stream (`/_sol/v1/events` on
    `calendar.localhost`, signed in like a page with a `sol_app` cookie made from a one-time
    code and kept in `files/calendar-session`, 0600; it shows in Settings as "Calendar sync on
    Android") reports an entry or feed change (debounced 1.5 s; the admin stream has no feed
    events); and when another app leaves a row dirty (a ContentObserver while the node runs asks
    for an expedited sync at once, because JobScheduler can hold Android's own local-change sync
    for minutes). With the node down nothing syncs; it catches up when the node starts.
  - *Off*: removes the account (and so every calendar row it owns) and the stream session; Sol's
    data is untouched. Removing the account in Android's settings turns it off too, and "Remove
    Sol from this phone" turns it off first. Not synced yet: invitations from other people's
    calendars (`calendar.invitations`) and per-event colours.
- **User-Agent**: the WebView's default UA plus ` SolAndroid/<versionName>`, so the node can name
  the session "Sol app on Android".
- **Sol in the Files app**: `DriveProvider` (a `DocumentsProvider`, authority `app.sol.documents`)
  shows `SolDrive` as "Sol" in the system Files app and every file picker: browse, open, create,
  rename, delete and write. The node's `.sol` folder is hidden and unreachable. The node watches
  the folder, so changes made there sync.

### Test on the emulator

```sh
TC=~/sol-mobile/.toolchain
ANDROID_SDK_ROOT=$TC/sdk ANDROID_AVD_HOME=$TC/avd \
  $TC/sdk/emulator/emulator -avd sol -no-window -no-audio -gpu swangle_indirect &
ABIS=x86_64 android/build.sh && adb install -r android/build/sol.apk
adb shell am start -n app.sol/.MainActivity --es name Ana    # debug builds only: skip the name form
adb shell am start -p app.sol -a android.intent.action.VIEW -d 'sol://pair?...'   # a scanned link
adb emu kill
```

Use `-gpu swangle_indirect`: with `swiftshader_indirect` the emulator's host render thread
segfaults as soon as the Sol app's page draws. A debug build accepts `--es name X` so scripts can
skip the form. Screenshots of the tested flows are in `android/shots/`.

Calendar sync: grant `READ_CALENDAR` and `WRITE_CALENDAR` with `adb shell pm grant app.sol ...`
(to skip the dialog), turn it on with `adb shell am start -n app.sol/.MainActivity --ez
calendar_sync true`, and look with `adb shell content query --uri
content://com.android.calendar/events`. A `content insert|update|delete` on that URI (without
the sync-adapter parameter) acts like an edit in a calendar app and reaches Sol within seconds
(`sol do calendar.agenda` via `run-as app.sol`). `--ez calendar_sync false` removes it all.

### For Android developers: make your app a Sol app

Any Android app can use the Sol app API (design/API.md) after the person approves it once. No
library, no Play services: one intent and plain HTTP.

```java
Intent i = new Intent("app.sol.action.CONNECT")
        .putExtra("app_id", "music")                       // [a-z][a-z0-9-]{1,31}
        .putExtra("name", "Music")
        .putExtra("description", "Plays the music in your Sol.")
        .putExtra("scopes", new String[]{"feeds", "blobs", "people"});
// or .putExtra("manifest", "{\"id\":\"music\",...}")   a whole app.json (no exec, ui or folders)
startActivityForResult(i, CONNECT);                         // not startActivity: Sol must see who you are

@Override protected void onActivityResult(int req, int res, Intent data) {
    if (res == RESULT_OK) {
        String token = data.getStringExtra("token");   // keep it in your private storage; shown once
        String api = data.getStringExtra("api");       // http://127.0.0.1:<port>
        String id = data.getStringExtra("app_id");
    } else if (data != null) {
        String error = data.getStringExtra("error");   // denied, not_for_result, not_set_up, bad_request, failed
    }
}
```

Then call `GET <api>/v1/whoami` (and the rest of the API) with `Authorization: Bearer <token>`.
Your app needs `INTERNET` and cleartext HTTP to `127.0.0.1` (a network security config with a
`127.0.0.1` domain, or `usesCleartextTraffic`); on Android 11+ add a `<queries>` entry for the
`app.sol.action.CONNECT` intent if you check whether Sol is installed first.

- **Who you are** comes from Android, not your extras: Sol shows your label and icon from
  PackageManager and binds the id to your package and the SHA-256 of your original signing
  certificate. The same id from another package or certificate is refused.
- **Connecting again** shows the approval again and gives you a new token; the old one stops
  working. A `401` means the person removed you (Settings > Connected apps, or
  `sol app remove <id>`): connect again.
- **Not available to you:** `folders` (use blobs), a UI served by Sol, and being called: actions
  in your manifest answer `not_implemented`, because Sol cannot reach into your app.
- **Inside Sol** (`ConnectActivity`): the approval runs `libsol.so app connect` with the request
  on stdin (`NodeService.cli2In`), never in argv or logcat; the node stores only the token's
  hash and serves `/v1/` on a loopback port kept in `files/sol/external-api.addr`.

Test it with the client in `android/testclient/` (never shipped): `android/testclient/build.sh`
(after one `android/build.sh`), `adb install -r android/testclient/build/sol-testclient.apk`,
open "Sol test client", tap Connect to Sol, Allow; it calls `/v1/whoami` and shows the answer.
Revoke with Settings > Connected apps and tap Call whoami again: `401`.
