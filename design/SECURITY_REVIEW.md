# Security review of Sol's core

Date: 2026-11-20. Reviewed at commit `6fbc495` plus the fixes below.

## Scope

The daemon's core, in this order: the handshake and sessions (`internal/wire`, `internal/session`,
`internal/pairing`, `internal/identity/toraccess.go`, the direct TLS transport's pinning); feeds,
replication and blobs (`internal/feeds`, `internal/replicate`, `internal/blobnet`); the app API
and app hosting (`internal/appapi`, `internal/apphost`); the admin API and browser sign-in
(`internal/daemon/browser.go`, `peercred*.go`, `admin.go`); port sharing
(`internal/daemon/services.go`). Apps (`apps/`) only where the core's rules depend on them;
Android (`android/`) not at all.

Threat model: (a) a stranger on the network or Tor who knows a device's address; (b) a paired
contact trying to reach what isn't shared with them; (c) a malicious or buggy app on my own
device; (d) another OS user on the same computer (or another Android app); (e) a malicious
website open in my browser; (f) a stolen device after it was removed from my device list.

## Method

Narrow reading of each area against design/IDENTITY.md, PROTOCOL.md, API.md and ADMIN.md, writing
a concrete attack for each suspected issue and checking it against the code. Every fixed finding
has a regression test that fails without the fix. Checks run: `make check`, `make browser-test`,
`SOL_UI_E2E=1 make ui-e2e`.

## Findings

| # | Severity | Threat | Finding | Status | Test |
|---|---|---|---|---|---|
| F1 | High | b | Co-author by claim: any contact could read any feed of mine | fixed | `TestViews`, `TestBlobPermission`, `TestFeedReplication`, `soltest.WriteShareCoAuthors` |
| F2 | High | d, c | App and peer origins served everything without sign-in | fixed | `TestAppOriginGate` |
| F3 | Medium | e | No CSRF check on app and peer origins outside the bridge | fixed | `TestAppOriginCSRF` |
| F4 | Medium | c | Any app could run any other app's write and destructive actions as me | fixed | `TestCrossAppActionNeedsApproval` |
| F5 | Low | d | Same-user lookup matched non-established socket rows; IPv6 parse assumed little-endian | fixed | `TestParseProcNetUID` |
| F6 | Medium | d | Windows: app sockets are loopback TCP, so other users can send forged `Sol-Self` | open | |
| F7 | Medium | b | Sharing Sol's own UI port as a service would sign the contact in as me | fixed | `TestCannotShareOwnPort` |
| F8 | Info | b | `/p/dial` has no sharing check, unlike what PROTOCOL.md said | accepted, doc fixed | |
| F9 | Low | b, c | Dial path not validated: `/_sol/` reachable, a bad path panicked the handler | fixed | `TestPeerDialPath` |
| F10 | Medium | d | A stolen session cookie (cookies ignore ports) worked from any OS user | fixed (same-user sessions) | `TestSameUserSessionBound` |

### F1. Co-author by claim (fixed)

Attack: Mallory, a contact, answers my `/p/sync` pull with
`{"t":"feed","feed":"<mallory>/files/tree","readable":true,"mode":"write"}`. My daemon recorded
that as an incoming write share, and `Shares.ReadMode`/`ReadableBy` then made my own
`files/tree` readable to Mallory as its "co-author". She pulled my whole Files tree, then every
blob it references (`MayFetch` follows `ReadMode`), and `sharesApp` let her call write actions
and open the UI of every app she named a feed of. Even an honest write share had the same
effect: write-sharing a feed called `tree` with someone exposed *their* `tree`.

Fix: a contact's announcement grants nothing. A feed of mine is readable by a contact only if I
share it (my own `sol/shares` records); `ModeCoauthor` now only labels a feed I share back to
someone who write-shared the same name with me. Apps consent by sharing their co-author feed back:
Files does it before its first write into a folder shared with it for writing
(`apps/files/write.go` `shareBack`), memsol follows the same rule, and API.md §5 says so.

### F2. App origins without sign-in (fixed)

Attack: another OS user (or any other Android app) runs
`curl -H 'Host: files.localhost:<port>' -H 'Origin: http://files.localhost:<port>' -d '{"action":"files.delete",...}' http://127.0.0.1:<port>/_sol/v1/actions/call`.
The router checked only the Host shape; the bridge checked only Origin, which a non-browser sets
freely; the app saw `Sol-Self: 1`. So the root's login screen protected only the Sol app, while
every app's data and actions, and every contact's apps through `<app>.<petname>.localhost`, were
open to the whole machine.

Fix: `apphost.Config.Gate` runs before anything on an app or peer origin (`Daemon.appGate`). A
connection of the daemon's own OS user is let in (looked up per request, as for the root). Any
other browser needs the origin's `sol_app` cookie, obtained only through a one-time code: an
unsigned page load is redirected to `/_sol/open` on the root, which requires a live root session
and a top-level navigation, mints a 60-second code for exactly that origin, and redirects to
`/_sol/login?code=` there. The cookie is `HMAC(session token hash, origin)`: it works on one origin,
needs no stored state, and dies with the session (logout or revocation signs out every origin).

### F3. CSRF on app origins (fixed)

Attack: a website (or a contact's page at `files.bob.localhost`, or another app's page) submits
`<form method=post action="http://files.localhost:<port>/some-endpoint">`. Only the `/_sol/`
bridge checked Origin; everything else was proxied to the app with `Sol-Self: 1`. Same-user
sign-in makes this worse on its own, because it signs in my browser whoever drives it.

Fix: `apphost.FetchAllowed`, applied to every app and peer origin before the gate. A non-GET
needs the origin's own `Origin` (or `Sec-Fetch-Site: same-origin`); a GET from another site must
be a top-level navigation (`Sec-Fetch-Mode: navigate`). Every other `*.localhost` origin counts as
another site, and the bridge keeps its own same-origin check.

### F4. Cross-app actions as me (fixed)

Attack: an app with the `actions` scope (its UI in the browser, or a buggy backend) calls
`POST /v1/actions/call {"action":"files.delete","args":{"paths":["/"]}}`. The daemon ran it as me
(`Sol-Self: 1`); the `agents` approval policy did not apply, because apps are not agents.

Fix: an app calling another app's `write` or `destructive` action gets an Inbox approval
(`appapi.Server.run`, and memsol), whatever the action's own policy; `read` actions and an app's
own actions are unchanged. API.md §6 documents it. Residual: an app *process* running as my OS
user can still read `identity.key` and reach other apps' sockets directly (see Residual risks).

### F5. Same-user lookup details (fixed)

`parseProcNetUID` took the first row matching the 4-tuple in any state. A `TIME_WAIT` row of an
earlier connection reports uid 0, which matches a daemon running as root. Now only `ESTABLISHED`
rows count (Linux `st` 01, Windows `MIB_TCP_STATE_ESTAB`), and address words are byte-swapped only
on little-endian hosts. Port reuse is otherwise safe: the lookup runs per request against the live
table and a live 4-tuple is unique.

### F6. Windows app sockets (open)

On Windows an app serves on `127.0.0.1:<port>` (`apphost.listenAddr`), and apps trust the
`Sol-*` headers on whatever reaches them. Another OS user can connect to that port directly and
send `Sol-Self: 1`, bypassing the daemon. On Linux, macOS and Android the socket is a unix socket
in a `0700` directory. Proposed fix (an API change, so API.md first): either AF_UNIX sockets on
Windows too (supported since Windows 10 1803; needs testing on Windows), or a per-app secret
`SOL_APP_KEY` the daemon sends as a header and `sol.Serve` checks in constant time.

### F7. Sharing Sol's own port (fixed)

The daemon dials a shared service's port itself, so a contact's stream into the UI port arrives
as the daemon's OS user and same-user sign-in lets it in, with full admin. `checkService` and
`peerService` refuse Sol's own UI port and the TCP API port.

### F8. Dial without sharing (accepted)

`/p/dial` lets any contact open a stream to any app, while PROTOCOL.md said "per sharing". A
stream (a call, a terminal) has no feed to share, and the app gets the verified caller headers,
so this is kept as the rule. PROTOCOL.md and API.md §7 now say so.

### F9. Dial path (fixed)

`dialApp` built `http://app<path>` and ignored `http.NewRequest`'s error: a path with a control
character gave a nil request and a panic (recovered by net/http, but a contact could trigger it
at will), and `/_sol/...` paths reached the app's reserved daemon endpoints as upgrade requests.
Paths are now parsed, must be plain absolute paths, and may not be under `/_sol/`.

### F10. Session cookies ignore ports (fixed for same-user sessions)

Cookies are not scoped by port, and every `localhost:<port>` is the same site, so a page served
by another OS user's server on some other localhost port receives `sol_session` from my browser
(SameSite=Strict does not help: it is same-site). Replayed with the right Host and Origin from
`curl`, it was a full admin session. Sessions started by same-user auto-login are now bound to
that user: they are honoured only on connections of the daemon's own uid, and they never mint app
codes (that user is let in on app origins without one). Sessions from login codes remain bearer
sessions, since those are exactly the browsers of another uid (Android, macOS); see Residual
risks.

## Reviewed and found sound

- **Handshake** (`internal/wire`): T covers both device keys, both nonces, the negotiated
  versions, op, secret, the caller's bundle hash and the op's extra field (join name, recover
  R_pub); the callee also signs its bundle. Request JSON must be canonical with no unknown
  fields; field sizes are checked per op; the caller checks `D_b` before signing and the person
  after. Every callee rejection is a silent close. Replay is impossible across connections
  (fresh nonces on both sides).
- **Silent to strangers**: the direct transport sends nothing until a preamble derived from the
  device key matches (`subtle.ConstantTimeCompare`), clearnet refuses non-loopback, and the onion
  serves its descriptor only to client-auth keys. A Sol hello reveals `D_b`, which the TLS
  certificate reveals too; that needs the preamble first.
- **Link secrets** (`internal/pairing`): 32 random bytes, 10 minutes, burned on first
  presentation whatever the outcome, looked up by SHA-256 (no timing on the secret).
  Join requires an admin device; pair refuses my own person.
- **Recover**: needs R's signature over T; answers only for me or a contact, with public objects.
- **Bundle pushes**: a contact may push only its own person's bundles; only my own devices may
  push mine or relay others; each verifies on its own; the cache refuses rollback (`ErrRollback`)
  and equivocation (`ErrConflict`), and an older list from a device the newest list dropped is
  refused, so a removed device (f) cannot come back with an old bundle anywhere the newer list
  has reached.
- **Tor client auth**: keys are X25519 of the two device keys hashed with both public keys; no
  secret travels; low-order results are refused by `crypto/ecdh`.
- **Replication**: the callee filters every pass by `ReadableBy`; the caller accepts from a
  contact only that contact's own feeds announced in the same stream; entries need a valid
  signature, canonical body (at most 64 KiB), an exact `seq`/`prev` chain, and a device in the
  owner's current list (entries of removed devices are refused outright, stricter than the spec).
  Internal feeds are never served to contacts. Unsharing ends live streams at once.
- **Blobs**: `403` is decided before any lookup; the rule follows `ReadMode` (fixed by F1); a
  fetched blob is verified by SHA-256 before it is complete.
- **App API**: tokens by SHA-256 lookup; every endpoint behind a scope; an app writes only its own
  feeds and reads its own plus `reads`, within sharing; inbox and events are per app; the bridge
  can only reach `/v1/`; peer UI proxies strip `Cookie`, `Authorization` and every inbound
  `Sol-*` header and set their own; the peer side never passes `/_sol/`.
- **Admin**: Host pinned to `localhost:<port>`/`127.0.0.1:<port>` (DNS rebinding), Origin or
  `Sec-Fetch-Site` checked before the cookie, cookie HttpOnly + SameSite=Strict + host-only,
  login codes single use for 60 s and only from the admin token, sessions stored as hashes,
  `admin.token` and every secret file `0600` and written atomically, run directory `0700`, API
  socket `0600`. No secret, code, token or request body is logged (checked by grep over every
  `log.*` call and the Tor control path).
- **Port sharing**: a stream is opened only to a service shared with that person (same answer for
  "no such service"); the port is the sharer's configuration, never the peer's; tunnels listen on
  `127.0.0.1` only and HTTP tunnels check Host.

## Residual risks

- **Same-user processes.** Anything running as my OS user (including an installed native app's
  process) can read `identity.key` and `admin.token` and connect to other apps' sockets. App
  isolation (F4, the gate) holds for app UIs in the browser and for honest-but-buggy apps, not
  for hostile executables. OS sandboxing is the only real fix.
- **Bearer sessions from login codes** (Android browser, macOS) can be stolen by a hostile page
  on another localhost port (F10) and replayed by another user. Mitigation would be binding them
  to something the attacker's server can't see; none exists on plain http. They expire after 30
  days unused and are revocable in Settings.
- **Windows app sockets** (F6), open.
- **Disk and CPU from contacts.** A contact can fill my disk with entries in its own feeds, or with
  a blob claimed larger than it is (verified only when complete); can open many mux streams (the
  accept backlog is bounded, total streams are not). Quotas per contact are not built.
- **Removal reach.** A removed device (f) is refused wherever the newer device list has arrived
  (pushed at once to live sessions); a contact offline since then still accepts it until they
  learn the list, at their next session with any of my devices.
- **Cookie tossing.** A contact's HTTP service at `<svc>.<petname>.localhost` can set a cookie
  for `localhost` and sign my browser out (a nuisance, not access).
- **Petname collisions.** A contact who names themselves like another contact may win
  `<app>.<petname>.localhost`; their page still can't reach my data (F2, F3).
- **Approval text.** An app can post an approval with any title for an action of any app; the
  Sol app must show the action and its arguments, not only the title.
- **Blob hashes are capabilities across apps.** Any app with the `blobs` scope can read any blob
  whose hash it knows and learn whether I hold it, and the activity log (action names, argument
  digests) is readable by every app with the `actions` scope.
- **Flaky tests seen during this review** (also on the unchanged tree, under load):
  `TestRestartRedials` and `TestLastingTunnelAndUnpair` time out waiting for a session when the
  whole e2e package runs alongside heavy load; both pass alone.
