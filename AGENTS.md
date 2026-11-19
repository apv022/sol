# AGENTS.md

Rules for anyone (human or agent) building Sol. Short on purpose; read it fully.

Start here: [design/VISION.md](design/VISION.md) (why), [design/ARCHITECTURE.md](design/ARCHITECTURE.md)
(how), [design/API.md](design/API.md) (the app contract), [design/PLAN.md](design/PLAN.md)
(tickets). The old proof of concept is in `~/solbak` (read-only reference; port code from it,
never edit it).

## Commands

```sh
make check   # gofmt + go vet + tests with -race. Must pass before you report a ticket done
make build   # bin/sol
```

## Rules

1. **Purity.** The daemon does only what needs keys or the network, what two or more apps
   would otherwise each build, or managing Sol itself. Everything with its own noun (file,
   song, document) is an app.
2. **Apps use only the app API.** Code under `apps/` imports the stdlib and `sdk/go/...`, never
   `internal/`. If an app needs something the API lacks, change `design/API.md` first.
3. **No third-party Go dependencies.** The one exception is `wireguard-go` in the daemon. No
   npm, no build step for UIs: plain JS modules and `ui/kit`.
4. **Agent first.** Every user-visible operation is an action in the app's manifest, reachable
   from `sol do` with `--json`, `--dry-run` and generated `--help`. The CLI is the agent
   interface (no MCP). A button that isn't an action is a bug.
5. **No polling.** State changes are pushed (events stream, feed replication).
6. **Never log secrets** (keys, tokens, pairing secrets, request bodies). Secret files `0600`,
   secret dirs `0700`, writes atomic.
7. **Never send bytes to a rejected connection.** Close and log.
8. **Honest stubs.** Unbuilt behaviour returns `not_implemented`, never fake success.
9. **Tests with every change.** Apps test against `memsol`; the daemon has e2e tests with
   several nodes in one process over the clearnet transport.
10. **Godoc on every exported identifier**, a package comment saying the package's job and
    invariants. Comment the why.
11. **Snappy and light UIs.** Read from local state, render immediately, no spinners for local
    data. Use the `ui/kit` tokens; escape every value that came from data before it reaches
    the DOM.
12. **Linux and Android first,** but nothing Linux-only without a fallback: macOS and Windows
    must build and pass tests.
13. **Everything is a file.** User data lives as plain-format files in the synced folder
   (Markdown, `.ics`, audio, video). Apps are lenses over those files, not owners of hidden
   databases. App-private state (indexes, caches) goes in `SOL_APP_DATA` and can be rebuilt.
