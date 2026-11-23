# Apps everywhere: home screens, installable web apps, other apps

Status: plan, for APV to agree before building (tickets M1–M5 in PLAN.md).

Today every app is reachable only through the Sol app: on Android only inside Sol's WebView,
on desktop at `http://<app>.localhost:7777/` (bookmarkable, but a browser tab). The goal: each
app feels like its own app (its own icon on the home screen or dock, its own window, its own
entry in recents) while staying one Sol underneath, and other apps on the device can work
with Sol's data without going through Sol's UI.

Nothing here changes the rule that apps use only the app API (AGENTS.md rule 2). It's about
how people and other programs *reach* the apps.

## What is fixed, and why it matters

- **One stable address per app.** Each app has its own origin, `http://<app>.localhost:<port>/`,
  and the port is chosen once at setup and kept in config.json (7777 by default). Everything
  below (bookmarks, installed web apps, shortcuts) depends on it never changing, so a port clash
  at start must keep refusing to move silently: it must say so.
- **`localhost` is a secure context**, so browsers allow service workers, installable web apps
  and the clipboard on these origins without HTTPS.
- **Sign-in per origin already works** (ADMIN.md "Who may use it"): a browser of the computer's
  own user is let in; anything else gets a one-time code through `/_sol/open`.

## Desktop

### M1. Every app installable as a web app (PWA)

Status: done. Icons are pre-rendered once at dev time (`make icons`, cmd/iconrender, headless Firefox) and
embedded, not rendered by the daemon; the manifest uses fixed theme colours, not the active theme.

Each app origin serves, from the daemon (so apps do nothing, like `/_sol/theme.css`):

- `/_sol/manifest.webmanifest`: `name`, `short_name` and `description` from app.json,
  `start_url: "/"`, `scope: "/"`, `display: "standalone"`, `theme_color`/`background_color`
  from the active theme, `icons` from the app's icon (SVG, plus 192 and 512 px PNGs rendered
  once by the daemon from it, cached under `<dir>/icons/`; Chromium wants PNGs);
- `/_sol/sw.js`: a minimal service worker. It caches the app shell (index.html, JS, CSS, kit),
  so an installed app opens instantly, and shows a "Sol isn't running" page with a Start
  button hint when the daemon is down, instead of the browser's error page. It never caches
  API responses (data is local and pushed anyway).

The kit's `sol.js` adds the `<link rel="manifest">` and registers the worker, so every app,
first-party or not, is installable with no work. The Sol app itself gets the same treatment.

Result: in Chrome, Edge or Brave, "Install Files" gives a Files window with its own icon in the
dock and app menu. Firefox can't install web apps; bookmarks keep working there.

### M2. App menu entries per app (Linux, then macOS/Windows)

Status: Linux done (`sol open <app>`, `sol-app-<id>.desktop` entries kept in step by `sol run`); macOS and
Windows still say not_implemented.

`sol launcher on` already adds "Sol" to the app menu. Extend it: one entry per app with a UI
("Sol Files", "Sol Calendar"…) running `sol open files`, which opens that app's origin signed
in. New apps add their entry when installed and remove it when removed. `sol open <app>` is
new and useful on its own (`sol open calendar`).

## Android

The phone has two ways to make an app feel separate. Recommendation: do the first now, offer
the second where it's free.

### M3. Home screen shortcuts and per-app windows (native, recommended)

- **Add to home screen.** Each app's page in the Sol app (and a long-press in the Apps list)
  gets "Add to home screen", which uses Android's pinned shortcuts
  (`ShortcutManager.requestPinShortcut`): an icon named "Files" (the app's icon, rendered by the
  daemon as above) that opens Sol straight into Files, full screen, without Sol's tab bar.
- **App shortcuts on Sol's icon.** Long-pressing the Sol icon lists Files, Notes, Tasks,
  Calendar (dynamic shortcuts, refreshed when apps change).
- **Their own entry in recents.** Each app opens in its own task (one activity per app id,
  `documentLaunchMode`), so switching between Files and Calendar works like switching apps,
  and Back from Files' root goes to the home screen, not to Sol.
- It's all one APK and one node: no second sign-in, works offline, nothing to install.

**Status (built):** `AppActivity` shows one app per task (`sol-app://<id>`,
`documentLaunchMode="intoExisting"`, task label and icon are the app's; Back at the app's start
moves the task back, so it stays in recents). Launcher shortcuts go through `ShortcutActivity`,
a windowless trampoline, because the launcher starts shortcuts with `CLEAR_TASK`, which would
restart an open app. Dynamic shortcuts (Files, Notes, Tasks, Calendar first, at most 4) come
from `libsol.so apps --json` when the node comes up and on every `app` admin event, and are only
rewritten when the list, a name or an icon changed. Icons are the node's
`/_sol/icon-192.png` as an adaptive bitmap, with a drawn fallback (a colour from the id and the
first letter). The page asks for a pinned icon with `SolAndroid.addToHomeScreen(id, name)`
(local pages only); the Sol app's Apps page shows "Add <app> to home screen" in the Android app.
The WebView setup is shared with the Sol app (`SolWeb`).

### Web apps on Android (optional, nearly free after M1)

Chrome on Android can reach the node on `localhost` too, and with M1's manifest it can install
an app as a real web app (a WebAPK). The only missing piece is sign-in: Android hides which app
owns a connection, so Chrome needs a one-time code. "Open in browser" in the Sol app would hand
Chrome a login link. Worth doing only if APV prefers Chrome's install over M3's shortcuts.

## Other apps on the device

What already works: sharing to Sol (share sheet → SolDrive), and SolDrive as "Sol" in the
system Files app and every file picker (DocumentsProvider).

### M4. System integrations, per app

The most useful kind of modularity is your Sol data showing up where you already look:

- **Calendar → the phone's calendars.** A sync adapter exposes Sol calendars to Android's
  CalendarProvider, so Google Calendar, Etar or the launcher widget show (and can edit) Sol
  events, and alerts use the system's reminders. On desktop the same idea is a read-only
  `.ics` subscription URL per calendar for Thunderbird/GNOME Calendar (local only).
  *Desktop status: built (`calendar.subscription`, files in the app data dir).*
- **Tasks → none standard on Android.** Skip; Tasks gets a home-screen shortcut (M3).
- **Notifications per app.** Inbox items become notifications on their own channel per app
  ("Calendar alerts", "Files: someone sent you files"), with Approve/Deny buttons for approvals.
  *Desktop status: built* (daemon posts via the OS notifier, never a push service; `sol notifications`,
  `SOL_NOTIFY`; see ADMIN.md). Android channels: not yet.

  **Status (built, local only).** No push service of any kind (no Firebase/FCM, no Google Play
  services, no vendor push): the foreground service runs `libsol.so events --json` (the admin
  event stream, `sol events`) as a child while the node serves and notifies from what this
  phone's own node says. Items that arrive while the phone is offline are notified when it
  reconnects and syncs. Each open inbox item is notified once (ids kept, capped at 500), on a
  channel per app named after it (created on first use; Sol's own items and apps without a UI
  use "Sol"), so turning an app off is Android's own per-channel switch. Approvals get Approve
  and Deny, notices Dismiss (each runs `libsol.so inbox approve|deny|dismiss <id>`); an item
  answered anywhere else cancels its notification. Tapping an approval opens Sol's Inbox, a
  notice opens its app in the app's own task. On every node start `inbox --json` is read too,
  so items from while the stream was down are not missed (items older than 7 days are not
  notified then).

### M5. Third-party Android apps talking to Sol (the app API on Android)

An Android app that wants to be a Sol app (say a music player) should use the same app API as
everyone else. Proposed handshake:

1. It sends an intent `app.sol.action.CONNECT` with its app id, name and the scopes it wants.
2. Sol shows an approval screen ("Music wants to read your Music folder and use People"),
   like installing an app on desktop.
3. On approval Sol registers it as an app (manifest from the request, no `exec`) and returns
   its app token and the API URL (`http://127.0.0.1:<api port>`); revocable in Settings.
4. The app calls the HTTP API with its token like any app. Its events stream works the same.

Two constraints shape this:
- Android won't run programs from an app's writable storage, so the desktop way of installing
  an app (a folder with an executable) can't work on the phone. On Android, Sol apps are either
  built in, web-only, separate APKs that connect this way, or run on another of your devices.
- Any app on the phone can open loopback connections, so the token is the only gate. That is
  already the rule for the API; tokens stay per app and scoped.

## Order

1. **M1** (web app manifests + service worker, daemon + sol.js): unlocks desktop install and
   is reused by everything else.
2. **M3** (Android shortcuts and per-app tasks).
3. **M2** (`sol open <app>`, per-app menu entries).
4. **M4** Calendar sync adapter + notifications per app.
5. **M5** when a real third-party Android app needs it (likely Music); design it with that app.
