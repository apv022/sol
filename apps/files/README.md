# Files

The synced folder (`~/SolDrive`): the same on all your devices, shared with people you choose.
Spec: [design/apps/FILES.md](../../design/apps/FILES.md). Manifest: [app.json](app.json).

- Entry point: `files.New(api, files.Config{...})`, then `Run(ctx)` (serves the UI,
  `/raw` and `/_sol/actions/*`) or `Start(ctx)` + `Handler()`.
- `SOL_FILES` is the folder, `SOL_APP_DATA` holds Files' own state (index, keep choices).
- The UI (`ui/`) uses only `ui/kit` and `sol.js` and calls only actions and events.

## Try the UI without a daemon

```sh
go run ./apps/files/cmd/files-dev            # Alice (laptop + phone) and Bob, in memory
```

It prints two URLs: Alice's UI (default `http://127.0.0.1:8787/`) and Bob's (next port). Alice's
folder is a temp directory (printed) seeded with a few files, and `Photos` is already shared with
Bob. Drop files into the browser or the folder and watch the other side follow. Everything is
reset when you stop it (`-keep` keeps the temp folders).

## Tests

```sh
go test -race ./apps/files/
```

They run against `memsol` with real temp folders, a real inotify watcher on Linux and the scan
fallback elsewhere.
