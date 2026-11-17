# Aries: music between friends

> **App design, not part of Sol.** Aries (short for Audiophile Adventures; a working name) would
> be a separate program in its own repository, built on the app API in [../APPS.md](../APPS.md).
> It is written down here to check that the API is enough. Pattern: **direct**.

Friends pair their Sol nodes, share Aries with each other, and see each other's music libraries
in one place, including lossless FLAC. Nobody uploads anything to a service, and no one outside
the group can see what anyone has.

## What it needs from Sol

| Need | Primitive | Why |
|---|---|---|
| Know which friend is browsing | 1. Who is calling | Per-friend library visibility, per-friend stats |
| Fetch friends' libraries and tracks | 2. Dial a peer's app | The Aries on your node talks to the Aries on theirs |
| Install and consent | 3. Manifest | `wants: { peer_identity, dial }`, `suggest_share: ask`, never public |
| Signing | not needed | Every exchange is between paired peers, already authenticated by Sol |

So Aries needs build steps 1 to 3 of APPS.md and nothing else.

## How it works

```
your node                                        friend's node
┌──────────────────────────────┐                  ┌──────────────────────────────┐
│ Aries UI (127.0.0.1)         │                  │ Aries                        │
│   │                          │   Sol: Tor or    │   ├─ library index           │
│ Aries ── apps/aries.sock ────┼── WireGuard ────►│   ├─ track pieces (hashed)   │
│   ├─ merged library          │   (signed,       │   └─ transcoder (Opus)       │
│   └─ local cache             │    shared only)  │      reads ~/Music, read-only│
└──────────────────────────────┘                  └──────────────────────────────┘
```

1. **Library index.** Each Aries serves a compact index of its library: artist, album, track,
   duration, format, size, and a hash per track. Your Aries fetches the index from every friend
   that shares Aries (`peers`, then `dial`) and merges it.
2. **Playback.** Tracks move in fixed-size pieces, each with its own hash in the index, so a
   piece that arrives is the piece that was sent.
   - **Progressive download with a buffer:** fetch pieces ahead of playback, start once a few
     seconds are buffered, cache them on disk. Replays are free.
   - **Prefetch:** while one track plays, fetch the next in the queue.
   - **Transcoded fallback:** when the path is slow, the friend's Aries transcodes to Opus at
     about 128 to 192 kbit/s, which Tor handles well. The FLAC stays available as a download.
3. **Speed.** FLAC runs at roughly 800 to 1,000 kbit/s, which is borderline over a slow Tor
   circuit. Sol uses WireGuard direct mode whenever both sides have it on, which makes FLAC
   streaming easy. Aries just reports which path it got (from `peers`) and picks FLAC or Opus.
4. **Several friends with the same album.** Pieces are verified by hash, so Aries can fetch them
   from several friends at once, torrent-style. This stays **among paired peers only**: there is
   no public swarm, so no stranger sees your IP or what you are fetching.

## Existing pieces

- **Subsonic API.** Exposing the merged library over the Subsonic API (spec.md v1.0 lists it)
  lets existing music apps on phones and desktops act as Aries clients.
- **Navidrome and similar servers** already work on Sol today as plain services: `sol service add
  navidrome 4533 --http --share friend`. Aries is what you get when the app knows who is
  listening and can merge many friends' libraries.
- **Rip Galore** (spec.md v1.0): CD ripping with AccurateRip and MusicBrainz straight into a
  node's library.

## Boundaries

- **Read-only on the music folder.** Aries reads the library; it never writes to it or deletes
  from it. Tier 1 isolation (APPS.md) should enforce that.
- **Friends only in the first version.** No public library, no open discovery of libraries.
- **Legal.** People will share albums they don't have the right to share. Aries should be framed
  and built as "your own library, shared with people you know", kept separate from Sol itself,
  and released by whoever maintains it with that exposure in mind. Open discovery of libraries
  would make it a public file-sharing network, which is a different legal and moderation problem.

## Open questions

1. Piece size and index format: invent a small one, or reuse BitTorrent v2's piece hashing?
2. Cache size and eviction defaults.
3. Does Aries need offline-friend availability (a friend's always-on node seeding the group's
   most-played pieces), or is that a later version?
