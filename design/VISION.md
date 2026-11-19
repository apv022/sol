# Sol: vision

Status: draft for discussion. Nothing here is built unless it says so.

## Two problems

**1. Peer-to-peer is garbage today.** Every p2p app brings its own networking, identity,
discovery and sync, and most of them fail at the basics: they can't get through NAT, they lose
your identity on reinstall, nobody can find anybody, and nothing works when the other side is
asleep. So people give up and use someone's cloud.

**2. Self-hosted apps are heavy.** Each one is a stack of services, databases and containers that
has to be set up, upgraded and babysat. They are built for a browser and a human; an agent has
to scrape its way through them. And the light ones are usually ugly.

## The answer

**Sol is one p2p layer that apps are built on.** You set Sol up once: your identity, your devices,
your people. From then on every app built on it is p2p for free. It reaches your devices and
your friends' devices anywhere, without opening a port, and it syncs data between them. No app
writes networking, login, discovery or sync code again.

**On top of it, a suite of slim apps** that replace the self-hosted stack: files (a synced folder
like OneDrive or Nextcloud), notes (slim rich text, live co-editing), tasks, calendar, music, movies, chat, and an agent. Each one is a good app alone and
better with peers: your music player plays your library, and John's if he shares it with you.

Self-hosting falls out of this for free. "My server" is just one of my devices that happens to
stay on.

## Principles

1. **Slim.** Few dependencies, small binaries, low memory. A Raspberry Pi runs the whole suite.
   Every dependency needs a reason written down.
2. **Snappy.** Apps read from a local copy of their data, so the UI never waits on the network.
   Budgets: first paint under 100 ms, interactions under 50 ms, no spinner for data you already
   have.
3. **Beautiful.** Light is not an excuse for ugly. One design system across every app, so the
   suite feels like one product.
4. **Agent native.** Anything you can do, an agent can do. Every app is defined by its actions;
   the UI, the CLI and the agent all call the same actions. Every CLI is documented, has
   `--json` and `--dry-run`, and is the first interface built, not the last.
5. **Just works, heals itself.** No ports, no config files to start. Connections and sync recover
   on their own after sleep, reboot or a network change.
6. **You own it.** Your keys, your devices, your data. No accounts, no company servers in the
   path. FOSS.
7. **Private by default.** Only the people you share something with can reach it. Nothing is
   public unless you publish it.

## What success looks like

- **If nobody else adopts it:** you have a private network that replaces Tailscale, plus a set of
  fast, good-looking apps that run your workflow and that your agent can drive end to end.
- **If people adopt it:** the same thing becomes everyone's own network and their way into a p2p
  web: apps that talk to friends' apps directly, with no platform in between.

The design has to make the first outcome excellent on its own, and leave nothing in the way of
the second.

## Non-goals

- No blockchain, no token, no accounts.
- No global directory of people. You find people through people.
- No server OS or distribution. Sol runs on the OS you have.
- No hosting other people's apps you didn't write as a catalog of containers. Sol's apps replace
  them; any other local app can still be shared over the network like today.
- No feature that only works through a server we run.
