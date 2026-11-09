# Peers

Every machine running Sol is a **node**. Two nodes pair once and become **peers** of each other:
both can reach the other, each decides what it shares with the other, and either can send the
other files. There is no separate client program anymore.

This replaces the earlier node/device split (`sol` vs `sol pair`/`sol connect`). Sol is
pre-release with no outside users, so the handshake and on-disk format changed in place; re-pair
your machines once.

## What a peer is

A peer record (in `config.json`, `peers`) holds:

| Field | Meaning |
|---|---|
| `id`, `node_id`, `pubkey` | The peer's ed25519 identity. `id` is a short fingerprint for the UI |
| `name` | What the peer calls itself (its node name) |
| `addrs` | How to reach the peer, keyed by transport: `{"tor": "x.onion", "clearnet": "host:port"}` |
| `access_in` | Credential **we** gave the peer, keyed by transport. For Tor: the x25519 public key we authorize on our onion |
| `access_out` | Credential the peer gave **us**, keyed by transport. For Tor: the x25519 private key we install in our tor |
| `shares` | Services we let this peer open. New peers get `_inbox` only |
| `services` | What the peer shares with us (last list we fetched) |

Everything transport-specific is a map keyed by transport name. Adding I2P, a direct WireGuard
path, or our own transport adds keys to those maps; the record, the pairing link, and the
handshake don't change shape.

## Pairing

```
A (inviter)                                         B (joiner)
  panel/CLI: invite ─► link with A's addrs, a one-time secret,
                       and access_out for B (per transport)
                                       link ─────►  sol pair <link>
                                                    install A's access, issue access for A,
                                                    authorize it on B's own publication
  ◄──────────── op "pair": secret + B's card {name, addrs, access} (signed by B)
  burn secret, save peer B (access_in from the token,
  addrs + access_out from the card), install B's access
  ─────────────► ok + A's peer info {name, services A shares}
                                                    save peer A
```

The card is covered by B's handshake signature, so nobody on the path can swap in their own
address or key. B needs its own node published before pairing so its card has an address; a node
that publishes nothing (a phone, later) pairs with an empty `addrs` and is reachable only when it
dials out.

## Connections

Every connection, in either direction, is the same handshake as before: the caller speaks first,
the callee sends its key and a nonce, the caller signs. The callee then checks:

1. the caller's key belongs to a peer, and
2. the requested service is in that peer's `shares` (built-ins included).

Otherwise it closes without sending a byte. `op` is one of `connect`, `pair`, `list`, `unpair`.

## Built-in services

| Name | What | Shared by default |
|---|---|---|
| `_inbox` | Receives files: `PUT /files/{name}`, answers with the SHA-256 | yes |
| `_files` | The shared-files page (files the owner added in the panel) | no |

Received files go to the receiver's own files list marked "from <peer>". They are **not** shown
on `_files` to other peers until the owner shares them. Limits: 4 GiB per file, 20 GiB total
from peers. The sender hashes while it streams and checks the receiver's answer, so a file
that arrives is the file that was sent.

## Running it

`sol` is one long-running process (the node). Other `sol` commands talk to it over a Unix socket
in the data directory (`control.sock`, `0600`, so only your user can use it):

```
sol                          run the node (panel on 127.0.0.1:8080)
sol invite                   print a pairing link
sol pair <link>              pair with the node that made the link
sol peers                    list peers and what they share
sol share <peer> <service>   let a peer open one of your services (--off to stop)
sol open <peer> [service]    keep a peer's services on 127.0.0.1 (ports from 9000)
sol tunnels                  list tunnels; sol close <peer> [service] closes them
sol send <peer> <file>       send a file to a peer's inbox
sol unpair <peer>            remove a peer here, and tell it to remove you
```

`<peer>` is a name or the start of an id.

## Tunnels

A tunnel makes one of a peer's services reachable on this machine at `127.0.0.1:<port>`. The
node owns it (the panel's **Open** button and `sol open` both ask the node), so nothing has to
stay running in a terminal:

- It is saved in `config.json` (`tunnels`) and reopens on the **same port** when the node starts,
  so bookmarks keep working. Automatic ports start at 9000.
- The listener stays up while the peer is unreachable. Each new connection dials the peer fresh,
  with the usual retries, so a reboot or a network change on either side heals by itself. The
  panel and `sol tunnels` show the last error and which transport carried the last connection.
- `http` services go through a small reverse proxy that accepts only `Host: 127.0.0.1:<port>`
  or `localhost:<port>`. That stops a web page from using DNS rebinding to read the peer's app
  through your loopback port. It adds no `X-Forwarded-*` headers. `tcp` services are a raw pipe.
- Tunnels listen on `127.0.0.1` only. Any program on this machine can use them, like any local port.
- Unpairing closes and forgets the peer's tunnels. If the peer stops sharing the service, the
  tunnel stays but every connection is refused; close it with **Close** or `sol close`.

## Direct mode

Set `"direct": true` under `transports` in `config.json` (and allow UDP and TCP port 7701 on
your firewall), then restart Sol. Peers then connect over WireGuard, punching through home
routers when they can, or over TLS where UDP is blocked. Behind a router you can forward the port and list it in
`direct_advertise`: IP:port only (no names; peers never dial names), no loopback, link-local or
multicast addresses. An unusable entry is ignored with a warning, and if none is left, direct
mode stays off rather than advertising all your addresses. Two nodes on one machine: set both `direct_listen` to
`127.0.0.1:<port>`. Peers then connect straight to this machine whenever they can
(same Wi-Fi, IPv6, a shared tailnet), and also across home routers (NAT hole punching), which is much faster than Tor; they use Tor when
they can't (for example behind a mobile carrier's symmetric NAT). The panel shows which path each
connection uses ("direct (WireGuard)", "direct (TLS)" or "via Tor"). Your peers see
your IP addresses, so it's opt-in. Details: [PROTOCOL.md](PROTOCOL.md#direct-mode-transport).

## Unpairing

`unpair` removes the peer locally first (its connections close, its Tor key leaves our onion),
then tries to tell the other side with a signed `unpair` op so it removes us too. If the other
side is offline, it keeps us in its list, but it can no longer reach us, and we no longer accept
it.

## Extension seams

- **New transport (I2P, direct WireGuard, our own):** implement `transport.Transport`; if it can
  restrict who reaches a publication, also implement `transport.AccessIssuer` (issue a
  credential) and `transport.ClientAuthorizer` (install one). Its addresses and credentials flow
  through `addrs`/`access_*`, links and cards automatically.
- **Discovery/rendezvous (Nostr, a DHT):** peers' `addrs` are just remembered values. A future
  discovery channel can update them (signed by the peer's ed25519 key) without touching pairing
  or the handshake.
