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
  means two devices edited the same version. For text files (UTF-8, under 4 MiB) the receiving device
  first tries a three-way merge (base, mine, theirs; line-based, like git): if the edits don't
  overlap, the merged file is written and appended as a new `put`. Otherwise both survive: the
  later one keeps the name, the other is written as `ideas (conflict from laptop 2026-11-19 2103).md` and a notice goes to the
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
- **Empty folders** (`mkdir` entries) have an index too: the set of explicit folders this device
  has on disk in sync with the tree. A tree folder not in it is new and is created; one in it that
  is missing from disk was removed or renamed here, so the watcher or the next scan publishes a
  `delete` (for it and every explicit folder below it) instead of creating it again.
- Ignored: `.sol/` (Files' own state lives in `SOL_APP_DATA`, not in the folder), editor temp
  files (`*.swp`, `~$*`, `.#*`), and patterns in `.solignore` at the folder root.

## Keep on device vs online only

Each top-level folder (and any shared folder) is either **kept** (files on disk, synced) or
**online only** (the tree is known, contents fetched when opened or streamed). Defaults: my own
folders kept on desktop, online only on Android except ones I pin and `Notes/` (Notes reads its
files from disk); folders shared with me online only. Music and Movies stream online-only files by blob hash without writing them to disk.

## Sharing

- Sharing a subfolder with a person, read or write, creates a feed `share.<id>` holding only that
  subtree's entries (paths relative to the shared folder), shared through `POST /v1/shares`. My
  tree entries under that folder are mirrored into it.
- A folder shared with me appears under `Shared/<petname>/<folder>/`. If they granted write, my
  changes there go into my own `share.<id>` feed, which they read as co-author (API.md §5) and
  fold into theirs with the same conflict rule. Before the first such change Files shares that
  feed back to the owner (`read`): a write share alone never exposes a feed of mine.
- Unsharing stops future updates; files already copied stay where they are.

## Send

"Send to a person" is the AirDrop-style quick send for files (not folders: those are shared). The
sender picks files and a contact (and an optional note); nothing arrives unasked.

- **Sender.** `files.send {paths, person, note?}` pins the files' blobs (they are already in the
  tree, so nothing is uploaded again), shares my feed `send.<recipient>` read-only with the person
  (once; sharing is idempotent) and appends one entry `{id, items: [{name, size, blob, type}], note,
  time}`. The blob permission rule (API.md §4) then lets the recipient fetch exactly those blobs
  and nothing else. Refused: an empty list, a folder ("share the folder instead"), a file that is
  not mine (under Shared), more than 100 files, myself.
- **Recipient.** Files folds every `send.<me>` feed it can see. For each send that is neither
  answered nor already asked about it files one Inbox approval: "<Name> sent you <file | N files>
  (<size>)", the names and note in the body, Action `files.receive {send: "<sender>/<id>", accept:
  true}`. Only the person's first device (lowest device id, the rule of the conflict notices) files
  it, and the answer lives in a feed every device reads, so any device may answer. `asked` (send
  ref → inbox id) is kept in the app's state so a restart does not file it again.
- **Answer.** Approve (or Save in the Shared view) fetches the blobs with `GET
  /v1/blobs/{hash}?from=<sender>` and writes the files through the normal tree path into
  `Received/<sender's name>/`, created if needed, so they sync to my other devices and show in the
  tree. A name that is taken gets " (2)", " (3)" before the extension; nothing is replaced. Deny
  (or Decline) saves nothing. Either way the recipient appends `{send, status: saved|declined,
  time, dir}` to its own `recv.<sender>`, shared read-only with the sender, who sees the status.
  Answering twice (Files, then the Inbox item) is a no-op that returns the first answer.
- **Reading.** `files.sent` lists my sends with per-item status (`sent`, `saved`, `declined`) and
  `files.received` lists incoming ones (`pending`, `saved`, `declined`). Entries of both feeds are
  untrusted: ids, hashes, sizes and names are validated, names made valid for every platform, and
  the first entry for an id wins.
- **Effects.** `files.send` is `write` with approval for agents (it transmits data to a person);
  `files.receive` is `write`. Anonymous contacts get no special case.

## Trash and versions

Nothing a person (or a program, or a sync bug) does to a file is final at once: a deleted file goes
to **Trash** and a replaced content becomes an earlier **version**, on every device, for 30 days.

**Where it lives: in the tree feed, folded like the tree.** There is no Trash folder on disk and no
second feed. A `delete` entry already names the path and the blob it removes (`base`), so the fold
turns every head a delete removes into a trash record (path, blob, size, when, which device, which
person), and every head a `put` supersedes (an edit on disk or through `files.write`, a sync from
another device, a three-way merge's `also`, "keep this / keep that") into a version of that path.
Because every device folds the same entries, every device and the anchor show the same Trash and
the same versions without anything extra to sync, and record ids are derived from the entry that
made them (device, seq, path, blob), so they are the same everywhere too. Why not a `.Trash/`
folder: it would sync as ordinary files (a second copy of everything, visible to every program,
deleted by the same `rm -rf` that caused the loss) and could not say who deleted what from where.
Why not a feed of its own: the tree feed is the one that already carries the facts, in causal
order, and a second feed could disagree with it.

The fields a delete entry gains: `trash` (a random id grouping the entries of one deletion, so a
folder is one row of Trash that restores as a tree), `in` (the path the person deleted: the row's
name) and `why` (`disk` when the watcher or a scan noticed it, `move` when the content lives on at
another path, which is neither Trash nor a version: a move across shares, "keep both"). Every
entry gains `at`, when it was made, because `mtime` is the file's own time and may be years old.
Two ops are new: `{"op": "purge", "ids": [...]}` forgets trash rows, trash items or versions (a
restore, "delete forever", "empty trash", an expiry), and `{"op": "trash-settings", "days": 30,
"versions": 10}` sets the retention for every device (the newest wins as a whole record).

- **Deletions on disk count.** `rm` (or any program) removing a synced file is noticed by the
  watcher or the next scan and published as a delete with `why: disk`; a folder removed with
  everything in it is published as one deletion of its top folder. If the synced folder itself is
  missing (an unmounted disk) nothing is published at all. A file that vanished on disk while its
  content lives on at another path (a rename by another program, seen as a delete and a put) is
  not listed in Trash.
- **Folders shared with me** keep nothing on my side: deletions and edits there go to their
  owner's Trash and versions (a co-author's deletion lands in mine as one row, named after them; a
  co-author cannot call a deletion a "move" to keep it out of my Trash, nor add rows to mine).
- **Restore** writes the files back as ordinary `put` (and `mkdir`) entries, parent folders
  implied, then purges the rows. A taken name gets `name (restored).ext` (`(restored 2)`, …); `to`
  restores into another folder. One item of a deleted folder can be restored alone.
- **Versions** are kept per path (they follow a move), newest first, one entry per content.
  Restoring a version writes it as a `put` whose `base` is the current content, which therefore
  becomes a version itself; the restored one leaves the list.
- **Retention.** Rows and versions older than `days` (default 30, 1–3650) are hidden at once on
  every device (the window is applied when listing, so devices agree without talking), and at most
  `versions` (default 10, 0–100) are listed per file. My first device (lowest id) then appends one
  purge for what expired, from a single timer set to the next expiry; nothing polls. The UI also
  sets one timer to the `next_expiry` the listing returns.
- **Blobs.** A trashed or earlier content stays fetchable from any of my devices and the anchor
  with nothing extra: the entries that put and deleted it reference its hash, and the daemon keeps
  a blob a feed of mine references (API.md §4). Every purge (a restore, "delete forever", "empty
  trash", an expiry, a version delete, the per-file cap) ends with a `{"op": "release", "release":
  [...]}` entry naming every blob the tree feed references that nothing uses any more: no head of
  a file (nor the base a conflict merge needs), no Trash item, no kept version. Content is
  addressed by hash, so bytes another file, item or version still holds are never released. Every
  device of mine and the anchor then delete the bytes. My share feeds release, at the same moment,
  what is no longer in the shared folder, so the people it is shared with can no longer fetch it.
  The versions beyond the cap (hidden at once everywhere) are purged by my first device, like
  expiry. If a release crosses a new use of the same bytes on another device (put while offline,
  stamped before the release), whichever device sees both appends `{"op": "keep", "keep": [...]}`
  naming them again, within the daemon's grace before it deletes released bytes.

## Actions (everything the UI can do)

`files.list {path}`, `files.stat {path}`, `files.read {path}` (returns blob hash and a local path
if kept), `files.write {path, blob | text}`, `files.mkdir {path}`, `files.move {from, to}`,
`files.copy {from, to}`, `files.delete {paths}` (destructive, approval for agents), `files.search
{query}` (names; content search is a later addition), `files.share {path, people, mode}`,
`files.unshare {path, people}`, `files.shared` (with me and by me), `files.keep {path, keep}`,
`files.conflicts`, `files.resolve {path, keep}`, `files.status` (sync state per device),
`files.sync` (rescan and fetch now; answers with the status), `files.send {paths, person, note}`,
`files.sent`, `files.received`, `files.receive {send, accept}` (see Send), `files.trash_list`,
`files.trash_restore {ids, to?}`, `files.trash_purge {ids}` and `files.trash_empty` (destructive,
approval for agents), `files.trash_settings {days?, versions?}` (no arguments shows them),
`files.versions {path}`, `files.version_restore {path, id}`, `files.version_delete {path, ids}`
(destructive, approval for agents) (see Trash and versions). `files.delete` answers the `trash` ids
of what it deleted. `files.list Shared` always
answers, empty when nobody shares with me. `files.list` also returns `here`
(the folder itself) and an `items` count per folder, so the UI needs one call per folder.

## UI

Fast list and grid views (painted from the last listing kept in the page, then the live one),
breadcrumbs, upload (+ menu, drag and drop, the Android file chooser), multi-select with a
selection bar and long-press/right-click menus, Move and Copy through a folder chooser, delete
with an Undo toast (it restores from Trash, folders too; folders ask first), a Trash view (name,
original folder, when, from which device, size, when it is gone; Restore, Delete forever, Empty
trash), Versions in the file menu and the viewer (each earlier content with Download, Restore and
Delete), a share sheet with people,
a "Shared" section (which also carries incoming sends with Save / Decline and a short "Sent" list), "Send to…" in the file menu, selection bar and viewer (contact picker with online state, optional note), keep on device / online only per folder, sync status with "Sync now", and
conflicts listed with "keep this / keep that / keep both". Phone layout first-class. Uses `ui/kit`
and `sol.js` only.

**The viewer** (`ui/viewer.js`) is one full-screen layer outside the re-rendered list (a live
update never reloads it) that moves through the files of the folder or search it was opened from:
swipe on touch (follows the finger; commits past a quarter of the width or on a flick), arrow keys
and chevrons on desktop, Escape or Back closes. It shows name, size and "3 / 12", with Download,
Share (the system share sheet, where the browser has one), Rename and Delete. Only the current file
and its neighbours are in the page; neighbour images preload, heavier kinds load when shown.
Images fit the screen (double-tap or pinch to zoom), video and audio use native controls, text and
code show the first 200 KB (a Range request) in monospace. PDFs use the browser's own viewer in a
frame on desktop; on Android (`SolAndroid.open`) a card with Open hands the file to the system
viewer once, never a frame (the WebView would turn it into a download). Other kinds get a card
with Download (and Open on Android). `#/<path to a file>` opens its folder with the file in the
viewer. `/raw?path=…&v=<blob>` is cacheable for good, so thumbnails and previews come from the
browser cache after the first view.

## Tests

Against memsol: two devices of one person converge after concurrent edits (with a conflict copy),
offline edits merge after reconnect, a share to Bob shows only that subtree and Bob's write-share
edits come back, online-only files stream by hash with Range, invalid names are refused, watcher
echo doesn't loop, empty and full folders renamed or removed on disk stay gone on both devices
(watcher and scan), and every action works from the CLI path with `--dry-run`. Send: two files from Alice to Bob give Bob exactly one approval, approving saves them under
`Received/alice/` with the right bytes and Alice sees "saved"; deny shows "declined"; a second
device does not file a second approval; a third contact cannot read the feed or the blobs; folders,
empty and oversized lists are refused; name clashes get " (2)"; dry runs send nothing. Trash and versions: a delete by action and an `rm` on disk (a
whole folder, one row) show on the other device with path, device and size, and restore there
(also on a taken name, into another folder, one item of a folder); purge and empty reach every
device; retention hides old rows at once and the first device alone appends the purge (fake
clock); settings set on one device hold on the other; versions come from an overwrite, an edit on
another device, a merge and a conflict choice, restore and delete everywhere and honour the cap;
dry runs change nothing; purge, empty, version delete and the cap free the bytes (never ones a
live file, Trash item or version shares), share feeds release them too, and a crossing release
is answered with a keep (`release_test.go`; e2e: a purged file's blob leaves both of my devices'
disks and the contact can no longer fetch it). The viewer's
step, swipe and slide rules, the send rules (`ui/send.js`) and the Trash wording and expiry timer
(`ui/trash.js`) run in a browser:
`apps/files/test/index.html` (`make browser-test`).
