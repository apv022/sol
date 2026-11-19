# Files: the synced folder

Status: spec for ticket A2. Files is an app (it uses only [API.md](../API.md)); every other
lens app builds on the folder it keeps.

## What the person sees

A folder on disk, `~/SolDrive` by default (configurable; `SolDrive` in app storage on Android), that is the same on all
their devices, like OneDrive or Nextcloud. Any program can use it. The Files UI browses it,
uploads, downloads, shares subfolders with people, and shows folders people shared with them.

## Model

- **The tree feed.** Files keeps one feed, `tree`, owned by me. Each entry is one change:
  ```json
  {"op": "put", "path": "Notes/ideas.md", "blob": "sha256:…", "size": 812,
   "mtime": 1795121162000, "base": "sha256:<blob this edit started from, or empty>"}
  {"op": "delete", "path": "Notes/old.md", "base": "sha256:…"}
  {"op": "move", "path": "Notes/a.md", "to": "Archive/a.md"}
  ```
  The current tree is the fold of all entries (all my devices' chains) in `pos` order, with a
  last-writer-wins map per path keyed by `(mtime, device)`. Folders are implied by paths; an empty
  folder is `{"op": "mkdir"}`.
- **Content** is in blobs. Writing a file = put the blob, append a `put` entry.
- **Conflicts.** A `put` whose `base` is not the current blob of that path on the receiving device
  means two devices edited the same version. Both survive: the later one keeps the name, the
  other is written as `ideas (conflict from laptop 2026-11-19 2103).md` and a notice goes to the
  Inbox. Never silently lose an edit.
- **Paths** are `/`-separated, NFC-normalised, relative, no `..`, no leading `/`. Names invalid on
  Windows (`CON`, `aux.txt`, trailing dot/space, `<>:"\|?*`) are refused at write time on every OS,
  so the folder works everywhere.

## Disk ↔ feed

- **Watching.** On Linux and Android use inotify through `syscall` (stdlib); elsewhere a full scan
  every 10 s plus on demand. A scan compares size and mtime, hashes only changed files.
- **Local change** → hash → put blob → append entry. Batch bursts (editor saves) with a 300 ms
  debounce per path.
- **Remote change** → fetch blob (if the folder is kept on this device) → write to a temp file in
  the same directory → atomic rename. Set mtime from the entry. Suppress the watcher echo.
- Ignored: `.sol/` (Files' own state lives in `SOL_APP_DATA`, not in the folder), editor temp
  files (`*.swp`, `~$*`, `.#*`), and patterns in `.solignore` at the folder root.

## Keep on device vs online only

Each top-level folder (and any shared folder) is either **kept** (files on disk, synced) or
**online only** (the tree is known, contents fetched when opened or streamed). Defaults: my own
folders kept on desktop, online only on Android except ones I pin; folders shared with me online
only. Music and Movies stream online-only files by blob hash without writing them to disk.

## Sharing

- Sharing a subfolder with a person, read or write, creates a feed `share.<id>` holding only that
  subtree's entries (paths relative to the shared folder), shared through `POST /v1/shares`. My
  tree entries under that folder are mirrored into it.
- A folder shared with me appears under `Shared/<petname>/<folder>/`. If they granted write, my
  changes there go into my own `share.<id>` feed, which they read as co-author (API.md §5) and
  fold into theirs with the same conflict rule.
- Unsharing stops future updates; files already copied stay where they are.

## Actions (everything the UI can do)

`files.list {path}`, `files.stat {path}`, `files.read {path}` (returns blob hash and a local path
if kept), `files.write {path, blob | text}`, `files.mkdir {path}`, `files.move {from, to}`,
`files.copy {from, to}`, `files.delete {paths}` (destructive, approval for agents), `files.search
{query}` (names; content search is a later addition), `files.share {path, people, mode}`,
`files.unshare {path, people}`, `files.shared` (with me and by me), `files.keep {path, keep}`,
`files.conflicts`, `files.status` (sync state per device).

## UI

Fast list and grid views, breadcrumbs, drag and drop upload, multi-select, preview for images,
audio, video, PDF (browser built-in) and text, a share sheet with people, a "Shared with me"
section, sync status per device, and conflicts listed with "keep this / keep that / keep both".
Phone layout first-class. Uses `ui/kit` and `sol.js` only.

## Tests

Against memsol: two devices of one person converge after concurrent edits (with a conflict copy),
offline edits merge after reconnect, a share to Bob shows only that subtree and Bob's write-share
edits come back, online-only files stream by hash with Range, invalid names are refused, watcher
echo doesn't loop, and every action works from the CLI path with `--dry-run`.
