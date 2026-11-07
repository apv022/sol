# Contributing

Thanks for helping. Sol is small on purpose, so the bar is: **simple, tested, documented.**

## Before you start

- For anything bigger than a bug fix, open an issue first so we can agree on the approach.
- Read [AGENTS.md](AGENTS.md) (rules and code map) and [docs/EXTENDING.md](docs/EXTENDING.md)
  (where new features plug in).

## Making a change

1. Fork, branch from `main`.
2. Write the code **and a test**. Behaviour changes usually belong in `internal/e2e`.
3. Run `make check`. CI runs the same thing on Linux, macOS, and Windows.
4. Update the docs your change affects (listed at the bottom of AGENTS.md) and `CHANGELOG.md`.
5. Open a PR describing *what* and *why*.

## Style

- `gofmt`, standard Go idioms, standard library first. New third-party dependencies need a good
  reason in the PR.
- Godoc on exported identifiers, a package doc in every package. Comments explain *why*.
- Errors are wrapped with context (`fmt.Errorf("doing x: %w", err)`) and returned, not just logged.

## Security issues

Please **don't** open a public issue. See [SECURITY.md](SECURITY.md).
