# Calendar: events, invites, alerts

Status: built (ticket A9). Not yet built: drag to move and resize in the time grid, a search field and an invite picker in the UI (`calendar.search` and `calendar.invite` work from `sol do`), VTIMEZONE blocks in exports (readers resolve the IANA TZIDs). Calendar is an app (it uses only [API.md](../API.md)). It should feel
like Apple Calendar: a calm week grid, colours per calendar, quick to read and quick to change.
It stands alone (rule 13): no Tasks overlay, its own date helpers, `.ics` in and out.

## Model: one feed per calendar

A **calendar** is one feed, `cal.<id>` (`<id>` is 8 random hex chars), owned by the person who
created it. Each entry is a batch of `crdt.Map` ops (last writer wins per key, HLC stamps, device
id as the tiebreak), appended as one entry so a change lands everywhere at once:

```json
{"ops": [{"key": "e.3fa91c20aa01.title", "value": "Standup", "stamp": {"w": 1795121162000, "c": 0, "d": "<device>"}}]}
```

| Key | Value |
|---|---|
| `c.owner` | person id; marks an owner feed (co-author feeds never carry it) |
| `c.name`, `c.color`, `c.order`, `c.deleted` | the calendar (colour is a palette name) |
| `g.<person>` | `read` or `write`: who the owner shared the calendar with (owner only) |
| `e.<id>.title` | an event exists once it has a title; `<id>` is 12 hex chars |
| `e.<id>.start`, `e.<id>.end` | RFC 3339 with offset for timed events; `YYYY-MM-DD` for all-day (end exclusive, as in iCalendar) |
| `e.<id>.tz` | IANA zone the wall time belongs to (`Europe/Berlin`); empty for all-day |
| `e.<id>.location`, `notes`, `color`, `uid` | text; colour overrides the calendar's; `uid` is the iCalendar UID |
| `e.<id>.alerts` | JSON array of minutes before the start (`[10, 1440]`) |
| `e.<id>.rrule` | recurrence rule text, the RFC 5545 subset below |
| `e.<id>.x.<rid>` | `true`: that occurrence is excluded (EXDATE). One key per date, so two devices excluding two dates both win |
| `e.<id>.of`, `e.<id>.rid` | an **override**: this event replaces occurrence `rid` of event `of` |
| `e.<id>.a.<person>` | `invited`: a Sol contact invited to the event |
| `e.<id>.created`, `e.<id>.deleted` | creation time (ms), tombstone |

Concurrent edits merge per field: one device renames an event while another moves it, and both
changes survive. A move writes `start` and `end` together; when concurrent writes leave `end`
not after `start`, the fold keeps the start and uses the default duration (1 hour, 1 day).

An event belongs to the calendar whose feed holds it; **moving** to another calendar copies its
fields (same id) and tombstones the original, like Tasks.

`rid` (a recurrence id) is the original start of an occurrence: `20261124T180000Z` (UTC) for timed
events, `20261124` for all-day ones. It names occurrences in keys, in actions (`at`) and in the UI
(`<event id>@<rid>`).

## Time zones

Times are instants plus the zone they were made in. Recurrence is expanded in **wall time of the
event's zone** (`time.LoadLocation`, with `time/tzdata` embedded so Android and Windows resolve
names too), so a 09:00 standup in Vancouver stays at 09:00 local across DST, and moves for
someone looking from Berlin during the weeks the two DST calendars disagree. A wall time that
does not exist (the spring-forward gap) is shifted forward by the gap, the RFC 5545 rule; a
repeated hour takes the first. All-day events are dates, the same day everywhere.

Actions take `start` as RFC 3339, as `YYYY-MM-DDTHH:MM` (wall time in `zone`, default the device
zone) or as `YYYY-MM-DD` (all day). Reads take `zone` and return times in it, so the UI never does
zone math: it asks in `Intl`'s zone and positions by the local clock.

## Recurrence

RRULE subset: `FREQ` (`DAILY`, `WEEKLY`, `MONTHLY`, `YEARLY`), `INTERVAL`, `BYDAY` (with ordinals
for monthly and yearly: `2TU`, `-1FR`), `BYMONTHDAY` (negative counts from month end),
`BYMONTH`, `COUNT`, `UNTIL`, `WKST`. Invalid dates are skipped, as RFC 5545 says (monthly on the
31st skips short months; yearly on Feb 29 happens in leap years). The first occurrence is always
`start`. Expansion is lazy and windowed: only occurrences in the asked range are computed, with a
hard cap so a hostile rule cannot loop.

Editing an occurrence asks for a span:

- **this event**: writes an override event (id derived from the master id and `rid`, so two devices
  editing the same occurrence converge on one override). Deleting only this one writes `x.<rid>`.
- **this and following**: ends the master the second before `rid` (`UNTIL`; a `COUNT` becomes the
  matching `UNTIL`) and starts a new series at this occurrence with the change (id derived from
  the master id and `rid`). Exceptions after the split move to the new series. Deleting ends the
  series.
- **all events**: edits the master; a time shift moves every exception's `rid` by the same amount.

## Sync

Like Tasks: the app folds every feed it can read, reading each from `pos` 1 at start, then follows
the `events` stream (no polling). Every action first reads what the stream has not delivered.
State is in memory and rebuilt from the feeds (a person's calendars are small; occurrences are
computed per request).

## Sharing a calendar

`calendar.share` writes `g.<person>` and calls `POST /v1/shares` on the calendar feed. A write
share makes the person's own `cal.<id>` a **co-author feed** (API.md §5): before their first write
their app shares it back (`read`); the owner folds it only for people it granted write, ignores
owner-only keys in it (`g.*`, `c.owner`, `c.deleted`), and relays co-author ops into its own feed
so every collaborator sees every other.

## Invites

Inviting is per event and needs no shared calendar:

1. The organiser writes `e.<id>.a.<person>` and keeps a feed **`inv.<person id>`** (shared `read`
   with that person) holding one key per invited event, `i.<cal>.<event>` = a JSON snapshot of the
   event (title, times, zone, location, notes, rule, exceptions, overrides, attendees), or
   `{"cancelled": true}`. Any of the organiser's devices rewrites a snapshot when the event
   changes; equal snapshots are not rewritten.
2. The invitee's app reads `inv.<me>` feeds of contacts (an `inv.` feed naming someone else is
   ignored), shows the events in an **Invitations** calendar, and posts an Inbox approval once per
   invite: *"Alice invited you to Standup"*, whose action is `calendar.respond` with `accepted`.
   Denying it answers `declined` (the app watches `inbox` events).
3. The answer flows back through the invitee's **own** feed, **`rsvp.<organiser id>`**
   (shared `read` with the organiser): `r.<cal>.<event>` = `accepted`, `declined` or `tentative`.
   The organiser folds it into the attendee list, only for people the event actually invites, and
   gets an Inbox notice when an answer changes.

Removing an invitee writes the `cancelled` snapshot; deleting an event cancels it for everyone.

## Alerts

`alerts` are minutes before the start of each occurrence. The app keeps **one timer** (an
injectable clock in tests) for the earliest unfired alert within the next 8 days, and re-arms
after every change or when that horizon is reached; nothing polls. A firing posts an Inbox notice
to myself (`to: self`): title the event, body the time and place. Fired alerts are remembered
(`fired.json`, per device); an alert missed by more than 15 minutes while the app was off is
dropped. Accepted invitations alert too; declined ones and hidden calendars still alert only if
the calendar is shown (hiding is a view choice, and an alert for a hidden calendar would surprise).

## Import and export

`.ics` (RFC 5545) `VCALENDAR`/`VEVENT`: `UID`, `SUMMARY`, `DTSTART`/`DTEND`/`DURATION` (with
`TZID`, UTC `Z`, floating, or `VALUE=DATE`), `LOCATION`, `DESCRIPTION`, `RRULE`, `EXDATE`,
`RECURRENCE-ID` (overrides), `VALARM` (`TRIGGER` before start), `STATUS:CANCELLED`. Lines are
unfolded on read and folded at 75 octets on write; text is escaped (`\\ \; \, \n`). Unknown
`TZID`s that are Windows names (Outlook) map through a small table; others fall back to the
device zone. Export writes one calendar, or every calendar as one file (each `VEVENT` carries
`X-SOL-CALENDAR`); a round trip keeps everything above. Import reuses events by UID (an import of
the same file twice changes nothing).

**Subscription files (desktop).** The app keeps a read-only `.ics` per calendar, plus `all.ics`,
in `<app data dir>/subscribe/` (not the synced folder, so devices never write the same file), so
Thunderbird, GNOME Calendar or Evolution can subscribe to a local `file://` URL. They are rewritten
atomically, after a short settle timer, whenever a calendar changes (no polling) and use the
export code above. `calendar.subscription` returns the paths and URLs; the UI shows them in
"Subscribe from another app" with Copy buttons, hidden in the Android shell.

## Actions

`calendar.list_calendars`, `create_calendar`, `update_calendar`, `delete_calendar` (destructive),
`calendar.agenda` (`from`, `to`, `zone`, `calendars`), `calendar.get`, `calendar.add`,
`calendar.update` (`span`: `this`, `following`, `all`, with `at` = the occurrence),
`calendar.move` (new start keeping the duration, and/or another calendar), `calendar.delete`
(destructive, approval for agents), `calendar.search`, `calendar.invite`, `calendar.invitations`,
`calendar.respond`, `calendar.share`, `calendar.export`, `calendar.import`, `calendar.subscription`, `calendar.snapshot`
(calendars, invitations and the occurrences of a range, for the UI). Every write takes a dry run;
calendars are named by id or name; table output has `columns`.

## UI

Apple Calendar on the kit. Desktop: a glass sidebar with the calendars (coloured check toggles,
grouped as mine, shared with me, invitations), a mini month and "Add Calendar"; a toolbar with ‹ ›,
Today, the title (`November 2026`), the Day/Week/Month segmented control, search and +. The week
and day views are a time grid (hour lines, a red now line, an all-day row) where overlapping
events split the width (clusters of overlapping events share columns, each event widens into
free columns to its right); drag to move, drag the bottom edge to resize, click an empty slot to
add. The month grid shows up to three events per day and "n more". Phone: the month (dots) with
the chosen day's agenda under it, a day view, and a bottom sheet to see and edit an event.
Everything paints from a cached snapshot at once and refreshes on feed events; calendar colours
are palette names mapped to tokens in `app.css`; every value is escaped.
