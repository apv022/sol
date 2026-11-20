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
- Without a session the page shows "Open Sol from the app or run `sol open`" and nothing else.
- `POST /admin/v1/logout` clears it. Sessions are listed and revocable in Settings.

## Endpoints (`/admin/v1/…`, JSON)

| Endpoint | Does |
|---|---|
| `GET status` | me (person, device), endpoints, transports and their state, `sessions` (browser), `peer_sessions` (live device sessions), `storage` (`{used, total}` of the synced folder's disk) |
| `GET people` | contacts: id, name, petname, devices (online, anchor), what they share with me, what I share with them |
| `PATCH people/{id}` `{petname}` | rename locally |
| `DELETE people/{id}` | unpair (both sides drop the contact; their copies of shared data stay with them) |
| `GET devices` | my devices: id, name, online, admin, anchor, last seen, this one |
| `PATCH devices/{id}` `{name, anchor, admin}` | change (admin devices only; admin needs P) |
| `DELETE devices/{id}` | remove from my device list (admin devices only) |
| `POST invite` `{join}` | a one-time link `{link, expires}` (pair a person, or join a device of mine) |
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
| `POST network-changed` | re-dial now |
| `GET sessions` · `DELETE sessions/{id}` | browser sessions (list, revoke) |
| `GET events` | SSE: `status`, `person`, `device`, `session`, `app`, `inbox`, `service`, `tunnel`, `theme` with `Last-Event-ID` resume. The page never polls |

Errors use the API.md error shape. Every change is written to the activity log as `sol.*`.

## Port sharing (services and tunnels)

The PoC's proven feature, on the new wire: a service is a local TCP port (`127.0.0.1:<port>`)
shared with chosen people. A contact opens it with a tunnel: their daemon listens on a local
port and forwards each connection as a session stream to `GET /p/service/{name}` (PROTOCOL.md),
which the sharer's daemon connects to the local port after checking the share. Port the
PoC's tunnel lifecycle (`~/solbak/internal/node/tunnels.go`): lasting tunnels, reconnect, per-
tunnel status. HTTP services can also be opened in the browser as `http://<service>.<petname>.localhost:<port>/`
(the same router as peer app UIs, C6b).

## The Sol app (UI)

Per design/UI.md: the home screen in the spirit of umbrelOS.

- **Home:** dusk wallpaper, greeting and clock, glass widgets (people online, my devices,
  storage), the app grid (squircle icons from each app's manifest), the Inbox preview, and a
  dock on desktop. Tapping an app opens its origin.
- **People:** contacts with presence; a person's page shows their devices, what they share with
  me (open it) and what I share with them; **Add a person** shows a QR code (`<sol-qr>`) and the
  link with Copy and Share, or pastes a link.
- **Devices:** my devices; **Add my device** shows a join QR code ("Scan with your phone's
  camera"), rename, mark as anchor, remove.
- **Inbox:** notices and approvals with Approve / Deny.
- **Sharing:** services I share (port sharing) and tunnels I opened; add a service.
- **Settings:** appearance (theme, accent, density, corners, glass, font, wallpaper), backup words
  (show once, confirm), agents (list, revoke), browser sessions, advanced (endpoints,
  transports, sessions, network re-check).
- Phone: tab bar Home · People · Inbox · Settings. Desktop: sidebar.
- Everything updates live from `GET events`; everything a button does is also a `sol` CLI command.
