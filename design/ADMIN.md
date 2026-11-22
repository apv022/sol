# Admin API and the Sol app

Status: spec for tickets C8 (daemon) and A5 (UI). The Sol app manages Sol itself (rule 1:
"managing Sol itself" is core), so it is not a separate app process: the daemon serves it at the
UI root, `http://localhost:<port>/`, from `ui/home/` (embedded), and its page calls the admin API
on the same origin. The CLI uses the same API over the API socket with `admin.token`.

## Who may use it

Anyone who can load `http://localhost:<port>/` in a browser could otherwise manage your Sol, and
other users on the same computer can reach loopback. So the browser needs a session:

- `sol open` (and the Android shell, and the desktop autostart) asks the daemon for a one-time
  login code (`POST /admin/v1/login-code`, admin token required), then opens
  `http://localhost:<port>/login?code=<code>`. The code is 32 random bytes, valid once for 60
  seconds; redeeming it sets `sol_session` (HttpOnly, SameSite=Strict, Path=/, 30 days, renewed
  on use) and redirects to `/`.
- Every admin request from the browser needs that cookie **and** an `Origin` equal to the UI
  root origin **and** `Host` equal to `localhost:<port>` or `127.0.0.1:<port>`. App origins
  (`<app>.localhost`) never get the cookie (it is host-only) and are refused.
- **Same OS user, no code.** A browser request (page load or admin call) with no live session is
  signed in transparently when the TCP connection it arrived on belongs to the daemon's own OS
  user. The daemon maps the client end of the loopback connection to its owner: Linux and
  Android read `/proc/net/tcp` and `/proc/net/tcp6` (the row whose local address is the
  request's remote address:port, whose remote is the daemon's listener, and take its uid);
  Windows uses `GetExtendedTcpTable` (`TCP_TABLE_OWNER_PID_ALL`) and compares the owning
  process token's user SID with the daemon's; macOS is not supported yet and uses the login
  screen. Why this is safe: the kernel, not the peer, says who owns a socket; both ends must be
  loopback; the lookup runs on every request and nothing is cached, so a recycled port or a
  different user never inherits an earlier answer; any error or missing row means "not the same
  user". The Host/Origin rules above apply first (a cross-site navigation or an app origin never
  triggers it), and the session is the ordinary cookie (revocable in Settings; logging out
  signs a same-user browser back in on its next page load). Other users on the machine and
  other Android apps (other uids) get the login screen. Anything running as the same user can
  already read the admin token, so this grants it nothing new; the one thing to know is that a
  same-user tunnel (`ssh -L`, a reverse proxy) forwards its remote peers as that user.
- **App and peer origins** (`<app>.localhost`, `<app>.<petname>.localhost`) are signed in too,
  since apps trust `Sol-Self: 1` and other users and other Android apps can reach loopback. The
  daemon's own OS user is let in by the connection's owner (looked up per request). Any other
  browser needs the origin's `sol_app` cookie (HttpOnly, SameSite=Strict, host-only): a page load
  without it goes to `GET /_sol/open?to=<app>[.<petname>]&next=<path>` on the root, which, with a
  live session and only for a top-level navigation, mints a one-time code (60 s) for exactly that
  origin and sends the browser to `/_sol/login?code=` there. The cookie is an HMAC of the origin
  keyed by the session, so it works on that origin only and ends with the session. On every app
  origin a request that changes something needs the origin's own `Origin`, and a cross-site GET
  must be a navigation (CSRF; `apphost.FetchAllowed`).
- **Same-user sessions are bound to that user.** A session started by same-user auto-login is
  honoured only on connections of the daemon's OS user: cookies ignore ports, so a page served on
  any other localhost port receives the cookie, and a copy must be worthless to anyone else.
  Sessions from login codes (another uid's browser, Android) are bearer sessions.
- A port Sol itself serves (the UI, the API on TCP) cannot be shared as a service: the daemon
  dials shared ports itself, so the contact would arrive as this OS user.
- Without a session the page shows "This browser isn't signed in to Sol. Open Sol from your
  app menu, or run `sol open`." and nothing else.
- Plain `sol` on a fresh machine sets Sol up (person = the OS account's full name, device =
  host name), starts it in the background and opens the app signed in with a login code (it
  only prints the URL over SSH or without a display). `sol autostart on` and that first run
  add a "Sol" app-menu entry on Linux (`~/.local/share/applications/sol.desktop` running
  `sol open`); `sol autostart off` or `sol launcher off` removes it.
- `POST /admin/v1/logout` clears it and sets `sol_signed_out=1` (HttpOnly, SameSite=Strict, ten
  years), which turns same-OS-user auto-login off for that browser: signing out must not undo
  itself. The locked screen then offers **Sign in**: `POST /_sol/signin` (same Host and `Origin`
  checks, and the connection must still belong to the daemon's OS user) starts a session and
  clears the marker. Other browsers need a login code as before. The Android app has no Sign out.
  Sessions are listed and revocable in Settings; a session's `client` is a friendly name made
  from its User-Agent (`Firefox on Linux`, `Sol app on Android` for the `SolAndroid/` token).

## Endpoints (`/admin/v1/…`, JSON)

| Endpoint | Does |
|---|---|
| `GET status` | me (person, device), endpoints, transports and their state, `sessions` (browser), `peer_sessions` (live device sessions), `storage` (`{used, free, total, measured}`: `used` is what the synced folder (SolDrive) holds, measured in the background at start and refreshed at most once a minute when status is asked, `measured` false until the first measurement ends; `free` is what the device has left for this user (`Bavail`), `total` the disk) |
| `GET people` | contacts: id, name, petname, `anonymous`, devices (online, anchor), what they share with me, what I share with them |
| `PATCH people/{id}` `{petname?, anonymous?}` | rename locally; `anonymous` true makes them Tor-only both ways (PROTOCOL.md §Anonymous contacts), false a buddy |
| `DELETE people/{id}` | unpair (both sides drop the contact; their copies of shared data stay with them) |
| `GET devices` | my devices: id, name, online, admin, anchor, last seen, this one |
| `PATCH devices/{id}` `{name, anchor, admin}` | change (admin devices only; admin needs P) |
| `DELETE devices/{id}` | remove from my device list (admin devices only) |
| `POST invite` `{join, anonymous}` | a one-time link `{link, expires}` (pair a person, or join a device of mine); `anonymous` makes a pair link with only the onion, whose redeemer becomes an anonymous contact (an error while there is no onion yet; never with `join`) |
| `POST pair` `{link}` | redeem a link |
| `GET apps` | installed apps: id, name, version, running, url, icon, description, scopes |
| `POST apps/install` `{dir}` · `POST apps/{id}/remove` · `POST apps/{id}/restart` | manage apps |
| `GET services` | shared local services (port sharing): name, port, http, people |
| `POST services` `{name, port, http, people}` · `PATCH services/{name}` · `DELETE services/{name}` | share a local port with people |
| `GET tunnels` · `POST tunnels` `{person, service, port?}` · `DELETE tunnels/{id}` | open a contact's service on a local port (`127.0.0.1:<port>`) |
| `GET inbox` · `POST inbox/{id}/approve` · `POST inbox/{id}/deny` · `POST inbox/{id}/dismiss` | the Inbox for all apps (dismiss is for notices) |
| `GET theme` · `POST theme` `{name}` · `POST theme/settings` `{…}` · `GET themes` | appearance |
| `GET backup` (pending words, if any) · `POST backup/confirm` | the 24 words |
| `GET agents` · `POST agents/{name}/revoke` | agent keys issued on this device |
| `POST agents/new` `{name, scopes, days, person?}` | makes an agent key for the Sol app (browser or CLI): the daemon makes the key pair, signs the certificate, saves the key file as `<dir>/agents/<name>.key` (0600, where `sol agent new` keeps it) and answers `{agent, key_file, path}`; `key_file` is the key file's text, returned this once only (never logged, never kept by the UI). `GET actions` lists the scopes to offer |
| `GET wallpapers` · `POST wallpapers` (raw image body, `Content-Type` png, jpeg or webp, at most 15 MB) · `DELETE wallpapers/{id}` | the person's own wallpapers (below) |
| `POST network-changed` | re-dial now |
| `GET transports` · `POST transports/{name}` `{enabled}` | the transports (`wg`, `direct`, `tor`, and any other configured): `{name, enabled, state: up\|starting\|stopped\|failed, detail, listen}`; turning one on or off applies now and is kept in `config.json` (the last one stays on). Also `sol transports` and the `sol.transports.*` actions |
| `GET sessions` · `DELETE sessions/{id}` | browser sessions (list, revoke) |
| `GET events` | SSE: `status`, `person`, `device`, `session`, `app`, `inbox`, `service`, `tunnel`, `theme` with `Last-Event-ID` resume. The page never polls |

Errors use the API.md error shape. Every change is written to the activity log as `sol.*`.

Settled in C8: `POST login-code` (admin token only) answers `{code, url, expires}`; `POST
apps/install` and `POST agents` (issuing a key for a public key the CLI made) are CLI-only, since they name files on this
computer; the browser makes keys with `POST agents/new`, where the daemon makes the pair itself. A browser GET without `Origin` (EventSource and same-origin fetch omit it) is accepted
unless `Sec-Fetch-Site` names another site; anything else needs `Origin` equal to the UI root.
`PATCH devices/{id}` with a changed `admin` answers `not_implemented` until handing over P
exists. Event data is the same object the matching GET returns (`{"id"|"name": …, "removed":
true}` once gone; `session` adds `up`). Times are RFC 3339 strings everywhere (never unix numbers). Shapes the table leaves open, settled
in A5b (the daemon and `home-dev` both follow them, and `make ui-e2e` checks the app against a real
daemon): `GET themes` is `{active, settings, themes:[{name, title, author, supports, builtin}]}`
(`name` is the lowercase id); agents are `{name, id, scopes, issued, expires}`; browser sessions
are `{id, client, created, last_used, expires, current}`; `peer_sessions` are `{id, person, device,
peer, relation, since, path, paths}`, where `path` is `{transport, inbound, reason}` (how the
session was reached and why; `transport` is empty and `inbound` true when the peer dialed) and
`paths` lists the paths to that device in trouble, `{transport, state: backoff|probation,
failures, last_error, failed_at, next_probe}` (C3b, for the Advanced view); `people[].last_seen` is optional (absent means unknown); `POST invite`
answers `{link, expires}`.

## Wallpapers

Built-in wallpapers (`dusk`, `aurora`, `ember`, `slate`) are drawn in CSS. The person's own images
are `<files>/.sol/wallpapers/<id>.<ext>` (png, jpeg or webp, at most 15 MB, written atomically,
`0600`; the daemon checks the magic bytes, not just the header). `POST wallpapers` takes the raw
image and answers `{id, type, size, added, url, setting}`; the setting `wallpaper` is
`custom:<id>`; deleting the chosen one resets the setting. The UI root serves an image at
`/wallpapers/<id>` to a session only, with its real type and `X-Content-Type-Options: nosniff`;
the page sets it through the CSSOM (`--wp-image`), as inline styles are forbidden. `accent` is
any `#rrggbb`: Settings offers swatches and a colour picker. The UI root links the active
theme's stylesheet (`/_sol/theme.css`) and swaps it live on the `theme` event.

## Setup and removing Sol from a device

A data directory with no identity runs in **setup mode** (internal/daemon/setup.go): `sol run`
serves `ui/home/setup.html` on the UI address instead of the daemon, with three choices:
**Start new** (name, device name), **Add to my Sol** (a `sol://join` link from another device's
Devices, Add my device) and **Restore from backup words** (24 words, name, device name, and
optionally a link from a device the person still has, so the restored identity is recognised
again). Plain `sol` on a fresh desktop with a display prepares the config, starts setup mode
and opens the page; over SSH or with `SOL_AUTO_INIT=1` it still sets up a new Sol with defaults.

The setup API (`GET /setup/v1/state`, `POST /setup/v1/new|join|restore|stop`) answers only
requests carrying the setup token (header `Sol-Setup`), a random secret in the URL `sol` opens
and in `<dir>/setup.url` (0600, for `sol open`); POSTs from a browser must carry the UI origin's
own `Origin`. After a successful choice the server stops and the same process starts the
daemon on the same address; the page waits for `/admin/v1/status` to answer and opens the app.

`POST /admin/v1/reset {"confirm": "<my name>"}` (Settings, This device, **Remove Sol from this
device**; `sol reset --confirm NAME`) stops the daemon; the process moves everything in the data
directory except `config.json`, the logs and earlier archives into `<dir>/removed-<UTC time>/`
(moving it back undoes it; the synced folder is never touched) and enters setup mode with the
token the reply carried (`{"setup": "/?setup=<token>"}`). On Android the Settings button calls
`SolAndroid.reset()`; the shell confirms natively, erases the node's data and SolDrive, and
shows its own welcome screen (Start new, Add to my Sol, Restore from backup words).

## Port sharing (services and tunnels)

The PoC's proven feature, on the new wire: a service is a local TCP port (`127.0.0.1:<port>`)
shared with chosen people. A contact opens it with a tunnel: their daemon listens on a local
port and forwards each connection as a session stream to `GET /p/service/{name}` (PROTOCOL.md),
which the sharer's daemon connects to the local port after checking the share. Port the
PoC's tunnel lifecycle (`~/solbak/internal/node/tunnels.go`): lasting tunnels, reconnect, per-
tunnel status. HTTP services can also be opened in the browser as `http://<service>.<petname>.localhost:<port>/`
(the same router as peer app UIs, C6b).

## The Sol app (UI)

Per design/UI.md: a plain sidebar app, not a desktop OS.

- **Layout:** a sidebar (the logo, the installed apps, then People, Devices, Inbox, Sharing,
  Settings) on desktop; a tab bar (Home, Apps, People, Inbox, Settings) on phones. Every app's
  shell gets a way back (Home and the other apps) from `GET /_sol/nav` (API.md §10).
- **Home:** the wallpaper, a greeting and the date, widgets (people online, my devices, the
  Sol folder's size and the device's free space), the apps, and the Inbox preview. Tapping an
  app opens its origin.
- **People:** contacts with presence; a person's page shows their devices, what they share with
  me (open it), what I share with them and an **Anonymous** switch (turning it on warns that an
  IP address they already saw can't be taken back); **Add a person** shows a QR code
  (`<sol-qr>`) and the link with Copy and Share (an **Anonymous** switch makes it an onion-only
  link), or pastes a link.
- **Devices:** my devices; **Add my device** shows a join QR code ("Scan with your phone's
  camera"), rename, mark as anchor, remove.
- **Inbox:** notices and approvals with Approve / Deny.
- **Sharing:** services I share (port sharing) and tunnels I opened; add a service.
- **Settings:** appearance (theme, accent, density, corners, glass, font, wallpaper), backup words
  (show once, confirm), agents (list, revoke, **New agent key**: name, scopes picked from the installed apps' actions, expiry; the key is shown once with Copy and the exact `sol do` commands), browser sessions, advanced (endpoints,
  transports with on/off switches, sessions, network re-check).
- Phone: tab bar Home · People · Inbox · Settings. Desktop: sidebar.
- Everything updates live from `GET events`; everything a button does is also a `sol` CLI command.
