# Tasks: lists and reminders

Status: spec for ticket A8. Tasks is an app (it uses only [API.md](../API.md)). It should feel
like Apple Reminders: fast, calm, one job.

## Model: one feed per list

A **list** is one feed, `list.<id>` (`<id>` is 8 random hex chars), owned by the person who
created it. Everything about the list and its tasks lives in that feed; each entry is a batch of
CRDT map ops:

```json
{"ops": [{"key": "t.3fa91c20.title", "value": "Buy milk", "stamp": {"w": 1795121162000, "c": 0, "d": "<device>"}}, ...]}
```

The list's state is one `crdt.Map` (last writer wins per key, HLC stamps with the device id as
the tiebreak), so two devices editing different fields of one task, or the same field, merge
without losing anything but the older write of that same field.

| Key | Value |
|---|---|
| `l.owner` | person id. Marks a feed as an owner feed (a co-author feed never has it) |
| `l.name`, `l.color`, `l.icon`, `l.order`, `l.deleted` | the list itself (colour and icon are names) |
| `g.<person>` | `"read"` or `"write"`: who the owner shared the list with (owner-only keys) |
| `t.<id>.title` | task text. A task exists once it has a title |
| `t.<id>.notes` | free text |
| `t.<id>.done` | completion time in ms, `0` = open (one field, so it cannot disagree with itself) |
| `t.<id>.due`, `t.<id>.time` | `YYYY-MM-DD` and optional `HH:MM`, wall-clock time of the device |
| `t.<id>.flag` | bool |
| `t.<id>.repeat` | `daily`, `weekly`, `monthly`, `yearly` or empty |
| `t.<id>.parent` | id of the parent task (subtasks are one level deep) |
| `t.<id>.order` | float; between two neighbours = their midpoint (fractional index) |
| `t.<id>.deleted`, `t.<id>.created` | tombstone, creation time |

A task belongs to the list whose feed holds it, so **moving** a task to another list writes its
fields into the target feed (same id) and tombstones it in the source: there is no `list` field,
because the feed is the unit of sharing.

**Order** uses fractional indices rather than the SDK's RGA list: tasks are reordered rarely, and
a per-task LWW float keeps one reorder from touching the others.

**Recurrence.** Completing a recurring task marks it done and creates the next occurrence in the
same entry. The next task's id is a hash of the old id and the new due date, so two devices that
complete the same task at once create the *same* task and converge. The next due date is the first
occurrence after both the old due date and today (monthly clamps to month end).

## Sync

The app folds every feed it can read. Start: `Feeds("")`, read each from `pos` 1. Live: the
`events` stream (`entry` and `feed` events), never polling. Every action first reads whatever the
stream has not delivered yet, so what it returns is current. State is in memory, rebuilt from the
feeds at start (a person's lists are small).

## Sharing

`tasks.share` appends `g.<person>` to the list (owner only) and calls `POST /v1/shares` on the
list feed. A write share makes the other person's feed `list.<id>` a **co-author feed** (API.md
§5):

- The recipient sees the list once the owner's feed has a `g.<me>` grant. Before the recipient's
  first write the app shares its co-author feed back to the owner (`read`); until then the owner
  sees nothing of it. Read-only recipients never write.
- The owner folds co-author feeds only from people it holds a write grant for, and ignores the
  owner-only keys (`g.*`, `l.owner`) in them. Co-author ops the owner has not yet written are
  **relayed** into the owner's feed, so every collaborator sees every other through the owner.
  Two owner devices may relay the same op twice; ops are idempotent.

## Reminders

A task with a due date and time posts an Inbox notice (`kind: notice`, `to: self`) when that
moment arrives. The app keeps one timer for the earliest unfired reminder and re-arms it after
every change. Fired reminders are remembered (`fired.json`), so a restart does not repeat them;
reminders missed by more than 15 minutes while the app was off are dropped silently. Tasks with a
date but no time do not notify.

## Quick add and dates

`tomorrow 9am`, `next fri`, `in 3 days`, `every monday`, `nov 25`, `2026-12-01 at 14:30`,
`#groceries`, trailing `!` (flag). The same rules live in Go (`dates.go`, for `tasks.add --quick`)
and in `ui/dates.js` (live preview), and both pass `testdata/dates.json`. A weekday means the next
such day after today; `at N` without am/pm is am for 7 to 11, noon for 12 and pm for 1 to 6; a time
alone means today, or tomorrow when it has passed.

## Markdown

```
# Groceries
- [ ] Milk (due 2026-11-21 09:00, repeats weekly, flagged)
  > notes, one quoted line each
  - [x] subtask
```

`tasks.export` writes one list, `tasks.import` reads one (into a named or new list).

## Actions

`tasks.list_lists`, `create_list`, `update_list`, `delete_list` (destructive), `tasks.list`
(a list or a smart list: today, scheduled, flagged, all, completed), `tasks.get`, `tasks.add`,
`tasks.update`, `tasks.complete`, `tasks.delete` (destructive), `tasks.move`, `tasks.search`,
`tasks.today`, `tasks.share`, `tasks.export`, `tasks.import`, `tasks.snapshot` (everything, for
the UI). Writes take `--dry-run`. Lists are named by id or by name.

## UI

Desktop: glass sidebar with smart-list tiles (Today, Scheduled, All, Flagged, Completed, each
with a count) and the lists; the task list with round checkboxes, inline editing, a quick-add bar
and a details panel. Phone: a lists screen, then a list screen with swipe actions and quick add at
the bottom; details in a sheet. Rendering is from local state at once (a cached snapshot paints
before the first answer); every value is escaped; tokens only.
