# Contributing

Thanks for helping. Sol is small on purpose, so the bar is: **simple, tested, documented.**

## Before you start

- For anything bigger than a bug fix, open an issue first so we can agree on the approach.
- Read [AGENTS.md](AGENTS.md) (the rules, short) and the design docs it points to.

## Making a change

1. Fork, branch from `main`.
2. Write the code **and a test**. Apps test against `memsol`; the daemon has e2e tests with several
   nodes in one process (`internal/e2e`); browser JS is tested in a browser (`make browser-test`).
3. Run `make check` (gofmt, vet, tests with `-race`). CI runs it on Linux, macOS and Windows.
4. Update the docs your change affects and `CHANGELOG.md`.
5. Open a PR describing *what* and *why*.

## Style

- No third-party Go dependencies (the one exception is `wireguard-go`), no Node.js, no build step
  for UIs.
- Every user-visible operation is an action reachable from `sol do`.
- Godoc on exported identifiers, a package doc in every package. Comments explain *why*.
- Never log secrets.

## Security issues

Please **don't** open a public issue. See [SECURITY.md](SECURITY.md).
