# From peers to a network

> **Design, mostly not built.** Pairing, public services, the signed service record and direct
> mode exist today. Introductions, Nostr announcements, community directories and the app layer
> are plans. Each section says which is which.

Sol starts as a way to connect your own machines ([PEERS.md](PEERS.md)). This page is how many
such nodes become a network that apps run on ([APPS.md](APPS.md)), without anyone becoming its
centre. **Everyone is a node. Some nodes are bigger than others, but nothing forces you to depend
on them, and your data stays on your node.**

## Three kinds of reach

| Reach | Who | Carried by | Authenticated by | State |
|---|---|---|---|---|
| **Paired** | People and machines you trust | The node's private onion, or direct mode (WireGuard, TLS) | Sol's signed handshake and share list | Works |
| **Public, signed** | Anyone | A service's own public onion; mirrors and relays may copy it | Signatures on the content (app keys, the service record) | Public onions and records work; app keys are planned |
| **Anonymous** | Anyone, with the origin hidden | Anonymous Public Mode (spec.md 7.6) | Signatures on the content | Partial (audit checks pending) |

Paired reach is small on purpose. A peer is a trust decision, and every peer costs a Tor
client-auth slot on your onion, which Tor caps at a few hundred per descriptor. Anything meant for
an audience goes public and carries signatures, so it can be verified without contacting you and
mirrored without your permission being needed for each copy.

## Transports

| Transport | Good for | Cost | State |
|---|---|---|---|
| Tor | Reach: works behind CGNAT, hotel Wi-Fi, mobile data, outbound only | Slow for big files; first contact can take a while | Works |
| WireGuard direct | Speed, when two peers can reach each other or punch through NAT | Shows your IP to your peers; opt-in | Works |
| TLS direct | Speed where UDP is blocked | Shows your IP to your peers | Works |
| I2P | Publishing to a second network; peer-to-peer swarming for people with a reachable router | Routers need an hour or more of uptime to perform; weak behind CGNAT | Stub (`transport/i2p.go`) |

Apps don't choose transports. They dial a peer's app, and Sol uses the fastest path that works
(see the panel's "which path" view). The rule of thumb: **Tor for reach, WireGuard for speed, I2P
later as an option.** Before betting an app on I2P throughput, measure it on a router that has
been up for a day or two.

## Discovery

Sol is a client, not a directory. Finding people and services is built from layers that can all
coexist. None is required, and none can take over another's job.

| Layer | How | Scales | Spam resistance | State |
|---|---|---|---|---|
| **1. Pairing** | A one-time link from someone you know | Small | Total: you chose them | Works |
| **2. Introductions** | A peer offers to introduce you to one of its peers; both sides must accept | Grows along trust | High | Planned |
| **3. Announcements** | A node or app publishes a signed announcement to Nostr relays (or a DHT); anyone can query | Large | Low on its own; needs signatures plus reputation or vouching | Planned (spec.md 3.1: Nostr carries signed records) |
| **4. Directories** | Index sites anyone can run; Sol ships a default list and users add their own | Large | Whatever the directory's policy is | Planned (spec.md 9) |

### Introductions (planned)

A introduces B to C. A sends B a signed note: "C's node id, C's name, and a fresh pairing link
C made for you". B pairs with C as usual, so the pairing still runs Sol's handshake end to end,
and A learns nothing more than that it made the introduction. Rules:

- C must agree first: A asks C for a link meant for B, and C can refuse.
- No transitive trust. Being introduced gives B nothing on C's node beyond what any new peer
  gets (`_inbox` only), until C shares more.
- It needs no protocol change: the note is delivered to B's `_inbox` or through a future
  built-in, and the link is an ordinary pairing link.

### Announcements (planned)

What already exists: a public service with `record: true` serves a signed record at
`/.well-known/sol.json` (`internal/record`), and a checker can confirm it with `record.Verify`
and `Record.Reaches`. An announcement is that record, published as a Nostr event, plus a short
description. The event's signature is a Nostr key; the record inside carries the service's own
signature, so the relay can't change either.

- Relays are dumb and replaceable. A node publishes to several, and a reader queries several.
- An app announces **app** services (a store, a public feed), never the node's private onion.
- The private node is never announced. Its peers already know where it is (onions are stable),
  and spec.md 3.1 keeps Nostr out of finding paired peers.

**The main risk is spam and abuse.** Announcements are cheap to fake in bulk. Readers should rank
by things that are expensive to fake: vouches from peers you have, a record that has stayed
reachable for a long time, app-level reputation (a market's completed orders).

### Directories (planned)

spec.md section 9 has the design: opt-in listings, proof of control through the record, published
criteria and a report path, no crawling. Any number can exist. Sol can ship a default list, and a
user can add or remove directories like relays.

## Bigger nodes

Some nodes will have more uptime, disk and bandwidth: a home server that never sleeps, a rented
machine, a community box. They help in ways that **apps** define, not Sol:

| Role | Who defines it | Example |
|---|---|---|
| Mirror / pin | The app | A social app copies a friend's public feed so followers can fetch it while the friend's laptop is off |
| Mailbox | The app | A market app holds orders for a seller who is offline, encrypted to the seller |
| Seed | The app | A music app keeps popular pieces available between friends |
| Gateway | Sol (v0.2, spec.md 7.4) | Lets a plain browser reach a node's services, for people with nothing installed |
| Directory / relay | Their operators | Index and announcement layers above |

The guardrail: **a bigger node can only help, never be required.** Every app must still work,
more slowly, between two small nodes with no one else around. If an app stops working when its
biggest node disappears, that node has become a server, and the design has failed.

## The app layer

Apps are where users actually meet. The contract is [APPS.md](APPS.md). In short:

- A **paired** peer opening your app is authenticated by Sol, and the app is told who it is.
- A **stranger** reaching your app's public onion is the app's to authenticate.
- An app can dial its counterpart on a peer's node, sign with its own key, and nothing else.

## Launch order

From spec.md 3.1 and the app proposal, unchanged in spirit:

1. **Self-hosting first:** "Tailscale without someone else's cloud." Finish onboarding, get real
   users. The network starts as many small groups of paired machines.
2. **The app API** (APPS.md build order, steps 1 to 3).
3. **The first app between friends** (music, [apps/aries.md](apps/aries.md)). It needs pairing
   only, no open discovery, and proves the app model.
4. **Signed public content:** app keys, then the social app's feeds
   ([apps/social.md](apps/social.md)) and Nostr announcements.
5. **The market** ([apps/market.md](apps/market.md)), once signed content and announcements work.
   It can start earlier by publishing listings to Nostr directly.
6. **Introductions and directories**, as the number of nodes makes them worth it.

## Open questions

1. Do introductions need a built-in service, or is a note in `_inbox` enough?
2. Which Nostr event kind carries a Sol service announcement? Reuse an existing one or define
   one (and propose it as a NIP)?
3. Should Sol itself publish announcements, or only apps? Default: only apps; the node stays
   quiet.
4. Name: "Sol" reads as Solana to crypto audiences (spec.md open question 6). Write "Sol
   Protocol" in launch posts; rename, if ever, before there are many users.
