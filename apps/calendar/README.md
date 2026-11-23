# Calendar

Calendars of events with time zones, repeats, alerts, sharing and invitations. Spec:
[design/apps/CALENDAR.md](../../design/apps/CALENDAR.md). Manifest: [app.json](app.json).

- Entry point: `calendar.New(api, calendar.Config{...})`, then `Run(ctx)` or `Start(ctx)` + `Handler()`.
  `SOL_APP_DATA` holds only per-device state: `fired.json` (alerts already delivered), `hidden.json`
  (calendars hidden on this device), `left.json` (shared calendars you left), `asked.json` and
  `told.json` (invitation notices already posted).
- Data is not files: each calendar is a feed (`cal.<id>`) of CRDT map ops, one LWW register per
  event field, so edits from two devices merge field by field. A write share makes the other
  person a co-author whose feed the owner folds in (only with the grant) and relays. Invitations
  travel in `inv.<person>` (event snapshots) and `rsvp.<organiser>` (answers).
- Times: a timed event is an instant plus its IANA zone; repeats expand in that zone's wall time
  (`rrule.go`, `times.go`), so 09:00 stays 09:00 across DST. All-day events are dates.
- Repeats: `daily`, `weekdays`, `weekly`, `biweekly`, `monthly`, `yearly` or an RRULE (FREQ,
  INTERVAL, BYDAY with ordinals, BYMONTHDAY, BYMONTH, COUNT, UNTIL, WKST). Edits and deletes of one
  occurrence take `span`: `this`, `following` or `all`.
- Alerts post an Inbox notice from one timer for the earliest unfired alert (no polling).
- Export and import (RFC 5545): `sol do calendar.export --calendar Work > work.ics`,
  `sol do calendar.import --ics "$(cat work.ics)"`. Import matches events by UID, so importing a
  file twice changes nothing.
- Everything the UI does is an action: `sol do --help calendar`.

## Try the UI without a daemon

```sh
go run ./apps/calendar/cmd/calendar-dev   # Alice http://127.0.0.1:8791/, Bob http://127.0.0.1:8792/
```

Alice shares "Family" with Bob for editing, and Bob invites Alice to a party. Add `?theme=dark` to
force dark mode; `#/week/2026-11-24` opens a view on a day.

## Tests

```sh
go test -race ./apps/calendar/                # merge, DST, spans, .ics round trip, alerts, sharing, invites, actions
go run ./cmd/browsertest apps/calendar/test   # ui/dates.js: grids, layouts, words
```
