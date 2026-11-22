# Tasks

Lists and tasks that sync between your devices and can be shared with people. Spec:
[design/apps/TASKS.md](../../design/apps/TASKS.md). Manifest: [app.json](app.json).

- Entry point: `tasks.New(api, tasks.Config{...})`, then `Run(ctx)` or `Start(ctx)` + `Handler()`.
  `SOL_APP_DATA` holds only `fired.json` (reminders already delivered) and `hidden.json` (shared
  lists you left).
- Data is not files: each list is a feed (`list.<id>`) of CRDT map ops, one LWW register per task
  field, so edits from two devices merge field by field. Sharing a list uses the daemon's feed
  sharing; a write share makes the other person a co-author whose feed the owner folds in and
  relays (API.md section 5).
- Reminders: a task with a due date and time posts an Inbox notice when it comes due, from one
  timer for the earliest unfired reminder (no polling).
- Quick add understands `tomorrow 9am`, `next fri`, `in 3 days`, `every monday`, `nov 25`,
  `#list` and a trailing `!`. The Go parser (`dates.go`) and the UI's (`ui/dates.js`) share
  `testdata/dates.json`.
- Export and import: `sol do tasks.export --list Groceries`, `sol do tasks.import --list
  Groceries --markdown "$(cat groceries.md)"`; the format is `- [ ] Milk (due 2026-11-21 09:00, repeats weekly, flagged)`.
- Everything the UI does is an action: `sol do --help tasks`.

## Try the UI without a daemon

```sh
go run ./apps/tasks/cmd/tasks-dev     # Alice http://127.0.0.1:8789/, Bob http://127.0.0.1:8790/
```

Alice and Bob are paired and share a "Groceries" list; edit as either and watch the other follow.
Add `?theme=dark` to force dark mode.

## Tests

```sh
go test -race ./apps/tasks/      # merge, recurrence, dates table, sharing, reminders, Markdown, every action
go run ./cmd/browsertest apps/tasks/test   # ui/dates.js against the same table
```
