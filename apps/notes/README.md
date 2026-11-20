# Notes

Markdown notes that are plain `.md` files in `<SOL_FILES>/Notes/`. Files syncs them between
devices and merges or conflicts concurrent edits; Notes never syncs anything itself. Spec context:
[design/apps/FILES.md](../../design/apps/FILES.md). Manifest: [app.json](app.json).

- Entry point: `notes.New(api, notes.Config{...})`, then `Run(ctx)` or `Start(ctx)` + `Handler()`.
  `SOL_FILES` is the synced folder, `SOL_APP_DATA` holds only the pin list.
- The index (titles, tags, links, full-text with prefix match) lives in memory and is rebuilt from
  the files at startup; inotify (Linux) or a 10 s scan keeps it fresh.
- Links are by name: `[[Name]]` matches a file name, then a `# Title`, case-insensitively;
  `[[Folder/Name]]` matches a path. `notes.move` rewrites links to a renamed note (option
  `rewrite_links`, default on; code is never touched). Moving a folder does not rewrite path links.
- Images and files go to `attachments/` next to the note (`notes.attach`, up to 3 MiB; the UI shrinks big photos).
- Pins are per device (in app data), so they do not touch the files.
- The Markdown renderer (`ui/md.js`) escapes all raw HTML; entities like `&copy;` are shown as typed.

## Try the UI without a daemon

```sh
go run ./apps/notes/cmd/notes-dev      # http://127.0.0.1:8788/, seeded temp folder (printed)
```

Edit a file in the printed folder with any editor and the open note updates (or shows a banner if
you have unsaved changes).

## Tests

```sh
go test -race ./apps/notes/     # Go tests
# Renderer tests: `python3 -m http.server` in the repo root, open /apps/notes/test/index.html
```
