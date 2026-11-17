# Apps on Sol

> **Design, not built.** Nothing on this page exists in the code yet unless it says so. It is
> the contract apps will build against, written down before the code so the core stays small.
> When a piece ships, its section moves into [ARCHITECTURE.md](ARCHITECTURE.md) and
> [PROTOCOL.md](PROTOCOL.md) and this page says so.

Sol is the network and identity layer. Apps are built on top of it. Sol gets two nodes talking,
proves who is on each end, and enforces who may reach what. Each app decides what its data is and
how its users interact: a music app streams tracks, a social app serves feeds, a market takes
orders. Sol never learns what any of that means.

```
┌───────────────────────────────────────────────────────────────┐
│  Apps        music · social · market · anything else          │  data, logic, storage,
│              (separate programs, separate repos)               │  payments, moderation
├───────────────────────────────────────────────────────────────┤
│  App API     who is calling · dial a peer's app · register     │  ← this page
│              · sign as this node's app                         │
├───────────────────────────────────────────────────────────────┤
│  Sol core    identity · pairing · shares · handshake           │  what exists today
│              · Tor / WireGuard / TLS direct / I2P (later)      │
└───────────────────────────────────────────────────────────────┘
```

The design rule from [spec.md](spec.md): **keep the core thin, boring and well audited.** An app
gets only what a peer has been shared, a buggy app is a hole in that app and not in the node, and
nothing an app does can widen what Sol exposes.

## Who owns what

| Sol core | Apps |
|---|---|
| Node identity (ed25519 keys) | What the data is: tracks, posts, listings, orders |
| Pairing and the peer list | How their clients talk: streaming, feeds, order flow |
| Transports: Tor, WireGuard direct, TLS direct, I2P later | Their own storage, caching, replication |
| Getting through NAT and CGNAT, outbound only | Payments and wallets |
| Shares: which peer may open which app | Reputation, moderation, content policy |
| Authenticating every connection | Authenticating strangers who reach a public app |
| The four app primitives below | Their own UI |

Two consequences:

- **"Bigger nodes" are not a Sol feature.** A big node is an ordinary node running an app with
  more uptime, disk and bandwidth: a pinning node, a mirror, a mailbox, a seed. The app decides
  what it does for others. Sol treats it like any other peer.
- **Apps don't ship inside the `sol` binary.** They are separate programs, usually in separate
  repositories. Sol never links an app's code, holds an app's wallet, or parses an app's data.

## Peers and strangers

A Sol **peer** is a machine you paired with, a trust relationship (see [PEERS.md](PEERS.md)).
Apps also want to reach people you haven't paired with: buyers in a market, followers of a public
feed. Sol supports both, and the app chooses per service:

| Who connects | How Sol carries it | Who authenticates them |
|---|---|---|
| A paired peer | The node's private onion (or direct mode), signed handshake, share list | **Sol.** The app is told which peer it is |
| Anyone | The app's own public onion (`--public`), no handshake | **The app**, with its own signatures or accounts |

Pairing doesn't scale to audiences, and that is deliberate: every peer is a Tor client-auth key
on your onion, Tor limits how many fit in one descriptor (on the order of a few hundred), and a
peer is someone you trust. Public content therefore goes on a public service and carries its own
signatures, so anyone can verify it and anyone can mirror it. See [NETWORK.md](NETWORK.md).

## Three connection patterns

Every app we have in mind uses one or more of these. Sol doesn't implement them; it provides what
each one needs.

| Pattern | Shape | Examples | Sol provides |
|---|---|---|---|
| **Direct** | Open a stream to a peer's app and pull data | Music, file sharing, video between friends | Dial a peer's app, tell the app who is calling |
| **Subscribe** | Fetch a signed, append-only feed every few minutes; mirrors may carry it | Social posts, channel updates, store catalogues | Public services, per-app signing keys |
| **Transact** | Exchange signed messages between two parties, delivered when the other side is online | Market orders, DMs, invitations | Dial, who is calling, signing; retries are the app's |

The protocol has no push (PEERS.md), so subscribe is **pull**. A follower's app polls; a mirror
polls and serves what it fetched. That works the same over Tor, WireGuard and a plain mirror, and
needs no server that stays connected to every follower.

## The four primitives

These are the whole app API. Everything else is the app's business.

### 1. Who is calling (inbound identity)

**Today:** `node.handleConnect` checks the caller's key and the share list, then pipes raw bytes
to `127.0.0.1:<port>`. The verified key stops at Sol, so the app can't tell peers apart.
(`_inbox` and `_files` can, because Sol serves them itself through `serveHTTPConn`.)

**Plan:** the app opts in, in its manifest (primitive 3), and Sol hands the identity over on the
local connection:

- **`http` apps:** Sol serves the stream through a small reverse proxy on the callee side (like
  `serveHTTPConn` with `internal/proxy`), removes any incoming `Sol-*` headers, and adds:

  ```
  Sol-Peer-Id:   <peer node id>      the ed25519 identity, base32 (identity.NodeID)
  Sol-Peer-Name: <peer's node name>  for display only; never use it for decisions
  Sol-Transport: tor | wg | direct | clearnet
  ```

- **`tcp` apps:** Sol writes a [PROXY protocol v2](https://www.haproxy.org/download/1.8/doc/proxy-protocol.txt)
  header before the first byte, with the peer's node ID in a custom TLV (type `0xE0`) and a
  loopback address pair. Many servers already parse PROXY v2, and the app can ignore the
  addresses. An app that didn't opt in gets the plain pipe it gets today, so existing services
  (Navidrome, Jellyfin, sshd) don't break.

Rules:

- The headers are only trustworthy because the app listens on loopback and Sol strips whatever
  the caller sent. The app must listen only on `127.0.0.1`. Sol's audit gets a check for that.
- Public services carry **no** identity: the caller is a stranger and Sol didn't authenticate it.
  Sol strips the headers there too, so a stranger can't claim to be a peer.
- Peer credentials (Tor keys, `access_*`) never reach an app. Rule 11 in AGENTS.md stands.

### 2. Dial a peer's app (outbound)

**Today:** `control.OpDial` pipes a local process to a peer's service. But the control socket is
the whole node: it can also pair, unpair, share, change settings and stop the node. An app must
never hold it.

**Plan:** each installed app gets its **own socket**, `apps/<name>.sock` (`0600`, in a `0700`
directory), with a short list of ops, all scoped to that app:

| Op | Does | Scope |
|---|---|---|
| `peers` | Lists peers that share this app with us: id, name, reachable | Only peers whose last `list` included this app |
| `dial` | Opens a stream to a peer's service of the same name; then a raw pipe | Same service name only; the peer's share list still decides |
| `sign` | Signs bytes with this app's key (primitive 4) | This app's key only |
| `whoami` | This node's id and name, and this app's public key | |

There is no op to pair, unpair, change shares, read config, or reach another app. An app that
wants a new peer asks the user to pair in Sol, and an app that wants more peers to open it asks
the user to share it. Both stay human decisions in the panel or CLI.

The wire format is the control socket's: one JSON line each way, then raw bytes for `dial`. It
reuses `internal/control`, with a smaller op table.

### 3. Register an app (manifest and consent)

**Today:** `sol service add <name> <port>` registers any local service. That stays as it is for
plain services.

**Plan:** an app ships an `app.json` manifest. `sol app install <dir>` (and a panel screen) reads
it, shows the user what the app asks for, and registers it only on approval:

```json
{
  "manifest": 1,
  "name": "aries",
  "version": "0.1.0",
  "description": "Listen to your friends' music libraries",
  "service": { "port": 4600, "protocol": "http" },
  "wants": {
    "peer_identity": true,
    "dial": true,
    "sign": false,
    "public": false
  },
  "suggest_share": "ask"
}
```

| Field | Meaning |
|---|---|
| `name` | Service name, `[a-z0-9-]{1,32}`, same rules as `config.Service`. Names starting `_` stay reserved for built-ins |
| `service` | Where the app listens on loopback and how Sol carries it (`tcp` or `http`) |
| `wants.peer_identity` | Primitive 1 on inbound peer streams |
| `wants.dial` / `wants.sign` | Create `apps/<name>.sock` with those ops |
| `wants.public` | The app may also be published on its own public onion. Off unless the user agrees |
| `suggest_share` | `none` or `ask`. **Never** automatic: new peers still get `_inbox` only (rule 11) |

The manifest is a request, not a grant. Sol stores what the user approved in `config.json` (a new
`apps` section, through `config.WriteFileAtomic`). An update that asks for more triggers a new
prompt. Removing the app removes its service, its shares, its socket and its key.

### 4. Sign as this node's app

Apps need to sign things: feed entries, listings, orders. They must not sign with the node key,
because then any signature an app makes is the node's, and a public app would link to the private
node.

**Plan:** Sol keeps one ed25519 key per app (`apps/<name>.key`, `0600`), made at install, and
signs on the app's behalf through the `sign` op. Messages are domain-separated so an app signature
can never be confused with a handshake or a record:

```
"sol-app-sig-v1\x00" || app name || "\x00" || message
```

By default the app key is **not linked** to the node: a public app's posts can't be tied to your
private node, the same rule public services follow today (each has its own onion key). For paired
peers who should know that "this app key is my friend's node", Sol can issue a **delegation**:
the node key signs `(app name, app public key)`, and Sol gives it only to peers over the
authenticated `list` reply. Strangers never see it.

An app that has its own identity system (a Nostr key, a Monero wallet) keeps it. The app key is
for apps that want one, not a replacement.

## Isolation

What Sol can enforce depends on how the app runs. Be honest about which tier an app is in.

| Tier | How the app runs | What Sol enforces | State |
|---|---|---|---|
| **0. Exposed** | You run it, like any service today | Network edge only: who reaches it, as whom. The app has your user's full access to the machine | Works today (without the API) |
| **1. Supervised** | Sol starts it in a container or OS sandbox (`runtime.Provider`; the Docker stub exists) | Plus filesystem and network limits: its own data folder, loopback only, no other ports | Planned |
| **2. Sandboxed** | WebAssembly with capabilities granted from the manifest | Plus no syscalls beyond what Sol hands it | Planned, v1.0 in spec.md, with wazero (see below) |

### Why WebAssembly, and why wazero

**Where app code runs.** An app has two halves. Its **client side** (the UI) is a web page, and
the browser already sandboxes it. Its **server side** (the backend on your node) is what needs a
sandbox, because it runs on your machine next to Sol and your files.

**Why not just OS sandboxes.** Linux has namespaces, seccomp and Landlock; macOS and Windows have
different, weaker or deprecated mechanisms; Android allows almost none of it for a process the
app starts. Tier 1 is still worth having, but it would be a different sandbox on every platform,
each with its own holes. WebAssembly is one sandbox that behaves the same on every platform Sol
ships on, phones included: a module can touch nothing except the functions Sol hands it.

**What it costs.** App backends must be compiled to WebAssembly (Go, Rust, C, Zig and others can
target it). Heavy native programs don't fit: a Monero wallet daemon or an audio transcoder stays a
separate program the user runs (tier 0 or 1), and the WebAssembly part of the app talks to it. So
WebAssembly is for apps from people you don't fully trust, not a rule for every app.

**When.** Not needed for the first apps, which come from the project itself and can run at tier
0. It must exist before Sol offers to install other people's apps.

**Decision: [wazero](https://wazero.io), not wasmtime.**

| | wazero | wasmtime |
|---|---|---|
| Language | Pure Go | Rust, used from Go through cgo |
| Sol's builds | Unchanged: `go build` cross-compiles every target, Android included | Needs a C toolchain and a prebuilt Rust library per target; cross-compiling gets much harder |
| Its own dependencies | None | The Rust library and the cgo wrapper |
| Speed | Fast enough for app backends (an ahead-of-time compiler on amd64 and arm64, an interpreter elsewhere) | Faster |
| Standards | Core WebAssembly and WASI preview 1 | Also the newer component model and WASI 0.2 |

wazero is accepted as Sol's second dependency, on the same reasoning as `wireguard-go`: writing a
WebAssembly sandbox ourselves is the riskier choice. The features wasmtime adds over it aren't
needed, because apps get no raw sockets or files anyway: Sol gives them the four primitives as
host functions and nothing more. Revisit only if app backends turn out to be too slow on it.

Tier 0 is enough to build and test every app. Tiers 1 and 2 are what make "pull and run an app
you don't fully trust" safe, and they must exist before Sol offers to install apps from anyone
else.

## Distribution

"Pull an app and run it through Sol" turns Sol into an app store, which is the largest new attack
surface on this page. So:

- **No registry in the core.** `sol app install` takes a local folder or archive. Finding apps is
  a separate concern (a directory, a Nostr announcement, a friend's link).
- **Signed manifests.** An app archive carries its publisher's signature. Sol shows who signed
  it and, for an update, refuses a different signer unless the user agrees.
- **Untrusted apps wait for isolation.** Until tier 1 exists, the panel says plainly that an app
  runs with your user's access.

## What does not change

- **The handshake.** An app is a service name; peers open it with `op: connect`, as any service
  today. No protocol version bump is needed for any of this.
- **The pairing link and the peer record.**
- **Rule 11.** A peer reaches only what it is shared, and new peers get `_inbox` only.
- **The panel's safety rules.** App UIs are their own pages on their own ports, never served from
  the panel origin.

## Build order

Each step is useful alone and has an e2e test in `internal/e2e` (two or three clearnet nodes in
one process, as the existing tests do).

1. **Inbound identity for `http` services** (primitive 1). Test: two peers open the same app and
   the app sees two different `Sol-Peer-Id` values; a public caller sending a forged header gets
   it stripped.
2. **The app socket with `peers` and `dial`** (primitive 2). Test: an app on A dials B's same app;
   it can't dial B's other services; it has no op that changes config.
3. **Manifests and `sol app install`** (primitive 3), tier 0. Test: nothing is shared after
   install; removal cleans up socket, service, shares and key.
4. **App keys and `sign`, then delegations** (primitive 4). Test: domain separation; a
   delegation is in `list` for peers and absent on public services.
5. **PROXY v2 for `tcp` apps.**
6. **Tier 1 isolation**, then signed archives, then tier 2.

The first real app ([apps/aries.md](apps/aries.md)) needs steps 1 to 3. The social app and the
market need step 4.

## Example apps

These are designs for apps that live **outside** this repository. They are here to check that the
API above is enough, not as part of Sol.

- [apps/aries.md](apps/aries.md): music between friends (direct). Working name.
- [apps/social.md](apps/social.md): posts and feeds (subscribe), built on Nostr.
- [apps/market.md](apps/market.md): a peer-to-peer market (transact), with Monero.

## Open questions

1. Header names: `Sol-Peer-Id` or a `Forwarded`-style extension? Pick one before step 1 ships;
   it becomes part of the contract.
2. Should an app be told when a peer stops sharing with us, or just see `dial` fail?
3. Can two apps cooperate (the market reusing the social app's feed), and if so, through Sol or
   directly? Default: directly, outside Sol.
4. ~~WebAssembly runtime~~: decided, wazero (see Isolation).
5. Do apps need a per-app quota on the node (disk, bandwidth), or is that the app's job?
