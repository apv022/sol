# Social: posts and feeds on your own node

> **App design, not part of Sol.** A separate program built on [../APPS.md](../APPS.md). It is
> written down here to check that the API is enough. Pattern: **subscribe**, plus **direct** for
> friends-only posts.

spec.md lists "a new social network or identity system" as a non-goal: Nostr already is one. So
this app doesn't invent a protocol. **Your node becomes your own Nostr relay**, reachable over Tor
without a server, and Sol carries the friends-only part.

## Two audiences

| Posts for | Where they live | Who can read | How followers get them |
|---|---|---|---|
| **Everyone** | Your relay, on the app's public onion | Anyone, with a Nostr client that can use Tor | They poll your relay; any public relay or bigger node can mirror your events |
| **Friends** | Your relay, on the app's private service | Paired peers you shared the app with | Their app dials yours through Sol (primitive 2), and your app checks who is asking (primitive 1) |

Public posts are standard Nostr events (NIP-01), signed with your Nostr key. That gives
everything the public side needs for free:

- **Verifiable anywhere.** Mirrors can't change an event without breaking its signature.
- **Mirrorable without permission.** Followers' relays and public relays can carry your posts
  while your machine is off.
- **Existing clients work.** Anyone with a Nostr client can follow you. Your relay list (NIP-65)
  names your onion relay alongside any public ones.

Friends-only posts are the part Nostr is weak at, and Sol is good at. They never leave the paired
group: they are served only to peers Sol has authenticated, and the app decides per friend.

## What it needs from Sol

| Need | Primitive |
|---|---|
| Know which friend is reading friends-only posts | 1. Who is calling |
| Fetch friends' friends-only feeds | 2. Dial a peer's app |
| Install, private service plus an optional public one | 3. Manifest, `wants.public: true` (the user agrees) |
| Signing | Not Sol's: the user's **Nostr key** signs posts. It stays in the app, or in a NIP-46 remote signer |

Keeping the Nostr key separate from Sol's node key is deliberate. Your public social identity
must not be linkable to your private node. Sol's per-app key (primitive 4) isn't needed here.

## Feeds are pull

Sol has no push, and doesn't need it:

- A follower's app asks your relay for events since the last one it saw, every few minutes.
- A mirror does the same and serves what it fetched.
- Replies and mentions arrive the same way: the replier's relay has them, and your app polls the
  relays of people you follow (or of people who mention you, found through public relays).

Over Tor, polling a handful of relays every few minutes is cheap. For thousands of followers, the
load spreads onto mirrors and public relays, not onto your home node.

## Bigger nodes

A friend's always-on node can mirror your public events and your friends-only events (for the
same friends you chose). It then serves them when your laptop is off. That's an app feature: the
mirror is an ordinary peer running the same app, sharing it with the same group.

## Open questions

1. Friends-only posts: encrypt them to each friend (so a mirror can carry them blind), or serve
   them only from nodes the friends are paired with?
2. Which Nostr relay implementation to embed or run as the app's backend?
3. Moderation for a public relay on your node: who may post to it? Default: only you and the
   people you follow.
