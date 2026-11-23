package app.sol;

import android.accounts.Account;
import android.content.ContentProviderClient;
import android.content.ContentProviderOperation;
import android.content.ContentUris;
import android.content.ContentValues;
import android.content.Context;
import android.content.SyncResult;
import android.database.Cursor;
import android.net.Uri;
import android.provider.CalendarContract.Calendars;
import android.provider.CalendarContract.Events;
import android.provider.CalendarContract.Reminders;
import android.util.Log;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.time.Instant;
import java.time.ZoneOffset;
import java.time.format.DateTimeFormatter;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;

/**
 * One sync of the "app.sol" account with the node: first the rows another calendar app changed
 * go to Sol (upload), then Sol's calendars and events are written into the provider (download).
 *
 * Mapping: a Sol calendar is a Calendars row (_SYNC_ID = calendar id); a Sol event is an Events
 * row (_SYNC_ID = event id) with DTSTART and DTEND, or DURATION, RRULE and EXDATE when it
 * repeats; a changed occurrence is an exception row (_SYNC_ID = "<id>@<occurrence>",
 * ORIGINAL_SYNC_ID, ORIGINAL_INSTANCE_TIME). All-day events are at UTC midnight in zone UTC, as
 * CalendarContract requires. Events come from `calendar.export` (.ics: the events themselves
 * with their rules, not expanded occurrences). SYNC_DATA1 is a hash of the exported VEVENT, so
 * unchanged events are not rewritten; SYNC_DATA2 is what was written (JSON), so an upload sends
 * only the fields the person changed.
 *
 * No Reminders rows are written: Sol's alerts already reach this phone as Inbox notifications,
 * and the calendar app would remind a second time. Reminders a person adds in a calendar app
 * become the event's Sol alerts and the rows are removed, so each alert fires once, from Sol.
 *
 * Conflicts: the local change is pushed first, then Sol's state is written back (last writer
 * wins; Sol's CRDT merges concurrent edits from other devices).
 */
final class CalendarSyncer {
    private static final String TAG = "Sol";
    private static final DateTimeFormatter DAY = DateTimeFormatter.ofPattern("yyyy-MM-dd").withZone(ZoneOffset.UTC);

    private final Context ctx;
    private final Account acc;
    private final ContentProviderClient cp;
    private final SyncResult result;

    CalendarSyncer(Context ctx, Account acc, ContentProviderClient cp, SyncResult result) {
        this.ctx = ctx;
        this.acc = acc;
        this.cp = cp;
        this.result = result;
    }

    /** A provider URI that writes as this account's sync adapter (no DIRTY marks, SYNC_* columns writable). */
    static Uri asAdapter(Uri u, Account acc) {
        return u.buildUpon().appendQueryParameter(android.provider.CalendarContract.CALLER_IS_SYNCADAPTER, "true")
                .appendQueryParameter(Calendars.ACCOUNT_NAME, acc.name)
                .appendQueryParameter(Calendars.ACCOUNT_TYPE, acc.type).build();
    }

    private Uri ad(Uri u) { return asAdapter(u, acc); }

    void run() throws Exception {
        Map<Long, Cal> local = localCalendars();
        upload(local);
        download(local);
    }

    // ---- calendars ----

    /** A Calendars row of the account. */
    private static final class Cal {
        long rowId;
        String solId;
        int access;
    }

    private Map<Long, Cal> localCalendars() throws Exception {
        Map<Long, Cal> out = new HashMap<>();
        try (Cursor c = cp.query(ad(Calendars.CONTENT_URI), new String[]{Calendars._ID, Calendars._SYNC_ID, Calendars.CALENDAR_ACCESS_LEVEL},
                Calendars.ACCOUNT_NAME + "=? AND " + Calendars.ACCOUNT_TYPE + "=?", new String[]{acc.name, acc.type}, null)) {
            while (c != null && c.moveToNext()) {
                Cal k = new Cal();
                k.rowId = c.getLong(0);
                k.solId = c.getString(1);
                k.access = c.getInt(2);
                out.put(k.rowId, k);
            }
        }
        return out;
    }

    // ---- upload: rows changed in a calendar app ----

    private static final String[] EV_COLS = {
            Events._ID, Events._SYNC_ID, Events.CALENDAR_ID, Events.DELETED, Events.DTSTART, Events.DTEND,
            Events.DURATION, Events.ALL_DAY, Events.EVENT_TIMEZONE, Events.RRULE, Events.EXDATE, Events.TITLE,
            Events.EVENT_LOCATION, Events.DESCRIPTION, Events.ORIGINAL_ID, Events.ORIGINAL_SYNC_ID,
            Events.ORIGINAL_INSTANCE_TIME, Events.ORIGINAL_ALL_DAY, Events.STATUS, Events.SYNC_DATA2,
    };

    /** An Events row as a calendar app left it. */
    private static final class Row {
        long id, calId, start, end, origId, origTime;
        boolean deleted, allDay, origAllDay, canceled;
        String syncId, zone, rrule, exdate, title, location, notes, origSyncId, written;

        boolean exception() { return origId > 0 || origSyncId != null; }
    }

    private void upload(Map<Long, Cal> cals) throws Exception {
        List<Row> rows = new ArrayList<>();
        try (Cursor c = cp.query(ad(Events.CONTENT_URI), EV_COLS,
                Events.ACCOUNT_TYPE + "=? AND (" + Events.DIRTY + "=1 OR " + Events.DELETED + "=1)", new String[]{acc.type}, null)) {
            while (c != null && c.moveToNext()) rows.add(row(c));
        }
        if (rows.isEmpty()) return;
        // Whole events first: an exception of a new event needs the event's Sol id.
        rows.sort((a, b) -> Boolean.compare(a.exception(), b.exception()));
        for (Row r : rows) {
            Cal cal = cals.get(r.calId);
            try {
                if (cal == null || cal.solId == null) { dropRow(r); continue; }
                if (cal.access < Calendars.CAL_ACCESS_CONTRIBUTOR) { revert(r); continue; } // read-only share
                if (r.exception()) uploadException(r);
                else uploadEvent(r, cal);
            } catch (CliError e) {
                Log.w(TAG, "calendar sync: " + e.getMessage());
                result.stats.numIoExceptions++;
            }
        }
    }

    private Row row(Cursor c) {
        Row r = new Row();
        r.id = c.getLong(0);
        r.syncId = c.getString(1);
        r.calId = c.getLong(2);
        r.deleted = c.getInt(3) == 1;
        r.start = c.getLong(4);
        r.allDay = c.getInt(7) == 1;
        r.zone = c.getString(8);
        r.rrule = c.getString(9);
        r.exdate = c.getString(10);
        r.title = nz(c.getString(11));
        r.location = nz(c.getString(12));
        r.notes = nz(c.getString(13));
        r.origId = c.isNull(14) ? 0 : c.getLong(14);
        r.origSyncId = c.getString(15);
        r.origTime = c.isNull(16) ? 0 : c.getLong(16);
        r.origAllDay = c.getInt(17) == 1;
        r.canceled = !c.isNull(18) && c.getInt(18) == Events.STATUS_CANCELED;
        r.written = c.getString(19);
        if (!c.isNull(5)) r.end = c.getLong(5);
        else {
            long d = 0;
            try { if (c.getString(6) != null) d = Ics.duration(c.getString(6)); } catch (RuntimeException ignored) { }
            r.end = r.start + d;
        }
        return r;
    }

    private void uploadEvent(Row r, Cal cal) throws Exception {
        if (r.deleted) {
            if (r.syncId != null) call("calendar.delete", new JSONObject().put("id", r.syncId).put("span", "all"), true);
            dropRow(r);
            return;
        }
        JSONObject before = null;
        if (r.written != null) { try { before = new JSONObject(r.written); } catch (JSONException ignored) { } }
        JSONObject now = written(r.title, r.location, r.notes, r.start, r.end, r.zone, r.allDay, r.rrule, r.exdate);
        JSONObject args = new JSONObject();
        if (r.syncId == null) {
            args.put("calendar", cal.solId);
            fields(args, r, null, now);
        } else {
            args.put("id", r.syncId);
            if (r.rrule != null || (before != null && !before.optString("r").isEmpty())) args.put("span", "all");
            fields(args, r, before, now);
        }
        List<Integer> alerts = takeReminders(r.id);
        if (!alerts.isEmpty()) args.put("alerts", new JSONArray(alerts));
        String newId = r.syncId;
        if (r.syncId == null) {
            JSONObject out = call("calendar.add", args, false);
            newId = out.getJSONObject("event").getString("id");
        } else if (args.length() > (args.has("span") ? 2 : 1)) {
            if (call("calendar.update", args, true) == null) { dropRow(r); return; } // gone from Sol
        }
        // Occurrences removed in the calendar app (EXDATE added to the series).
        if (newId != null && r.syncId != null && before != null) {
            Set<String> had = new HashSet<>(splitExdates(before.optString("x"), r.allDay));
            for (String x : splitExdates(r.exdate, r.allDay)) {
                if (!had.contains(x)) call("calendar.delete", new JSONObject().put("id", r.syncId + "@" + x).put("span", "this"), true);
            }
        }
        markClean(r.id, newId);
    }

    /** The fields of args that differ from what was last written (all of them for a new event). */
    private void fields(JSONObject args, Row r, JSONObject before, JSONObject now) throws JSONException {
        boolean all = before == null || before.optBoolean("a") != r.allDay;
        if (all || !before.optString("t").equals(r.title)) args.put("title", r.title);
        if (all || !before.optString("l").equals(r.location)) args.put("location", r.location);
        if (all || !before.optString("d").equals(r.notes)) args.put("notes", r.notes);
        boolean when = all || before.optLong("s") != r.start || before.optLong("e") != r.end
                || !before.optString("z").equals(nz(r.zone));
        if (when) {
            args.put("all_day", r.allDay);
            if (r.allDay) {
                args.put("start", DAY.format(Instant.ofEpochMilli(r.start)));
                args.put("end", DAY.format(Instant.ofEpochMilli(Math.max(r.end, r.start + 86_400_000L))));
            } else {
                java.time.ZoneId z = Ics.zone(r.zone);
                args.put("start", Instant.ofEpochMilli(r.start).atZone(z).format(DateTimeFormatter.ISO_OFFSET_DATE_TIME));
                args.put("end", Instant.ofEpochMilli(Math.max(r.end, r.start)).atZone(z).format(DateTimeFormatter.ISO_OFFSET_DATE_TIME));
                args.put("zone", z.getId());
            }
        }
        if (all ? r.rrule != null : !before.optString("r").equals(nz(r.rrule))) args.put("repeat", nz(r.rrule));
    }

    private void uploadException(Row r) throws Exception {
        String master = r.origSyncId;
        if (master == null && r.origId > 0) master = syncIdOf(r.origId);
        if (master == null || r.origTime == 0) { revert(r); return; } // its event never reached Sol
        String occ = master + "@" + Ics.solTime(r.origTime, r.origAllDay);
        if (r.deleted || r.canceled) {
            call("calendar.delete", new JSONObject().put("id", occ).put("span", "this"), true);
            if (r.deleted) { dropRow(r); return; }
            markClean(r.id, occ);
            return;
        }
        JSONObject args = new JSONObject().put("id", occ).put("span", "this");
        JSONObject before = null;
        if (r.written != null) { try { before = new JSONObject(r.written); } catch (JSONException ignored) { } }
        Row plain = r;
        plain.rrule = null; // an occurrence has no rule of its own
        fields(args, plain, before, null);
        List<Integer> alerts = takeReminders(r.id);
        if (!alerts.isEmpty()) args.put("alerts", new JSONArray(alerts));
        if (args.length() > 2 && call("calendar.update", args, true) == null) { dropRow(r); return; }
        markClean(r.id, occ);
    }

    private String syncIdOf(long rowId) throws Exception {
        try (Cursor c = cp.query(ad(ContentUris.withAppendedId(Events.CONTENT_URI, rowId)), new String[]{Events._SYNC_ID}, null, null, null)) {
            return c != null && c.moveToFirst() ? c.getString(0) : null;
        }
    }

    /** Minutes of the row's alert reminders, which are then removed (Sol's alerts notify instead). */
    private List<Integer> takeReminders(long eventRow) throws Exception {
        List<Integer> out = new ArrayList<>();
        try (Cursor c = cp.query(ad(Reminders.CONTENT_URI), new String[]{Reminders.MINUTES, Reminders.METHOD},
                Reminders.EVENT_ID + "=?", new String[]{String.valueOf(eventRow)}, null)) {
            while (c != null && c.moveToNext()) {
                int m = c.getInt(0), method = c.getInt(1);
                if (m < 0) m = 10; // REMINDER_MINUTES_DEFAULT
                if ((method == Reminders.METHOD_ALERT || method == Reminders.METHOD_DEFAULT) && !out.contains(m)) out.add(m);
            }
        }
        if (!out.isEmpty()) cp.delete(ad(Reminders.CONTENT_URI), Reminders.EVENT_ID + "=?", new String[]{String.valueOf(eventRow)});
        return out;
    }

    /** Clean, with Sol's id; SYNC_DATA1 cleared so the download writes Sol's version back. */
    private void markClean(long rowId, String syncId) throws Exception {
        ContentValues v = new ContentValues();
        v.put(Events.DIRTY, 0);
        v.put(Events._SYNC_ID, syncId);
        v.putNull(Events.SYNC_DATA1);
        cp.update(ad(ContentUris.withAppendedId(Events.CONTENT_URI, rowId)), v, null, null);
    }

    /** A change that cannot reach Sol (a read-only share): Sol's version is written back. */
    private void revert(Row r) throws Exception {
        if (r.syncId == null) { dropRow(r); return; }
        ContentValues v = new ContentValues();
        v.put(Events.DIRTY, 0);
        v.put(Events.DELETED, 0);
        v.putNull(Events.SYNC_DATA1);
        cp.update(ad(ContentUris.withAppendedId(Events.CONTENT_URI, r.id)), v, null, null);
    }

    private void dropRow(Row r) throws Exception {
        cp.delete(ad(ContentUris.withAppendedId(Events.CONTENT_URI, r.id)), null, null);
        result.stats.numDeletes++;
    }

    // ---- download: Sol's calendars and events ----

    private void download(Map<Long, Cal> local) throws Exception {
        JSONArray list = call("calendar.list_calendars", new JSONObject(), false).getJSONArray("calendars");
        Map<String, Long> bySol = new HashMap<>();
        for (Cal k : local.values()) if (k.solId != null) bySol.put(k.solId, k.rowId);
        Set<String> keep = new HashSet<>();
        for (int i = 0; i < list.length(); i++) {
            JSONObject cal = list.getJSONObject(i);
            String id = cal.getString("id");
            keep.add(id);
            String mode = cal.optString("mode");
            ContentValues v = new ContentValues();
            String name = cal.optString("name", id);
            String owner = cal.optString("owner_name", "");
            v.put(Calendars.CALENDAR_DISPLAY_NAME, "owner".equals(mode) || owner.isEmpty() ? name : name + " (" + owner + ")");
            v.put(Calendars.CALENDAR_COLOR, CalendarSync.color(cal.optString("color")));
            v.put(Calendars.CALENDAR_ACCESS_LEVEL, "read".equals(mode) ? Calendars.CAL_ACCESS_READ : Calendars.CAL_ACCESS_OWNER);
            Long row = bySol.get(id);
            if (row == null) {
                v.put(Calendars.ACCOUNT_NAME, acc.name);
                v.put(Calendars.ACCOUNT_TYPE, acc.type);
                v.put(Calendars._SYNC_ID, id);
                v.put(Calendars.NAME, id);
                v.put(Calendars.OWNER_ACCOUNT, acc.name);
                v.put(Calendars.VISIBLE, 1);
                v.put(Calendars.SYNC_EVENTS, 1);
                v.put(Calendars.CALENDAR_TIME_ZONE, java.util.TimeZone.getDefault().getID());
                v.put(Calendars.ALLOWED_REMINDERS, String.valueOf(Reminders.METHOD_ALERT));
                v.put(Calendars.ALLOWED_AVAILABILITY, Events.AVAILABILITY_BUSY + "," + Events.AVAILABILITY_FREE);
                v.put(Calendars.CAN_ORGANIZER_RESPOND, 0);
                Uri u = cp.insert(ad(Calendars.CONTENT_URI), v);
                if (u == null) continue;
                row = ContentUris.parseId(u);
            } else {
                cp.update(ad(ContentUris.withAppendedId(Calendars.CONTENT_URI, row)), v, null, null);
            }
            syncEvents(row, id);
        }
        for (Cal k : local.values()) {
            if (k.solId == null || !keep.contains(k.solId)) cp.delete(ad(ContentUris.withAppendedId(Calendars.CONTENT_URI, k.rowId)), null, null);
        }
    }

    private void syncEvents(long calRow, String calId) throws Exception {
        String ics = call("calendar.export", new JSONObject().put("calendar", calId), false).optString("ics", "");
        List<Ics.Event> evs = Ics.parse(ics);
        // Rows now in the provider: sync id -> {row id, hash, dirty}.
        Map<String, Object[]> have = new HashMap<>();
        List<Long> strays = new ArrayList<>();
        try (Cursor c = cp.query(ad(Events.CONTENT_URI), new String[]{Events._ID, Events._SYNC_ID, Events.SYNC_DATA1, Events.DIRTY},
                Events.CALENDAR_ID + "=?", new String[]{String.valueOf(calRow)}, null)) {
            while (c != null && c.moveToNext()) {
                if (c.getString(1) == null) { if (c.getInt(3) != 1) strays.add(c.getLong(0)); continue; }
                have.put(c.getString(1), new Object[]{c.getLong(0), c.getString(2), c.getInt(3) == 1});
            }
        }
        Set<String> seen = new HashSet<>();
        Map<String, Ics.Event> masters = new HashMap<>();
        for (Ics.Event e : evs) if (e.rid == null) masters.put(e.id, e);
        ArrayList<ContentProviderOperation> ops = new ArrayList<>(), later = new ArrayList<>();
        for (Ics.Event e : evs) {
            String sid = e.rid == null ? e.id : e.id + "@" + e.rid;
            if (!seen.add(sid)) continue;
            String hash = hash(e.text.toString());
            Object[] h = have.get(sid);
            if (h != null && ((Boolean) h[2] || hash.equals(h[1]))) continue; // unchanged, or a local edit not yet sent
            ContentValues v = values(e, calRow, sid, hash, masters.get(e.id));
            Uri target = h == null ? ad(Events.CONTENT_URI) : ad(ContentUris.withAppendedId(Events.CONTENT_URI, (Long) h[0]));
            ContentProviderOperation op = h == null ? ContentProviderOperation.newInsert(target).withValues(v).build()
                    : ContentProviderOperation.newUpdate(target).withValues(v).build();
            (e.rid == null ? ops : later).add(op);
            if (h == null) result.stats.numInserts++; else result.stats.numUpdates++;
        }
        for (Map.Entry<String, Object[]> en : have.entrySet()) {
            if (seen.contains(en.getKey()) || (Boolean) en.getValue()[2]) continue;
            ops.add(ContentProviderOperation.newDelete(ad(ContentUris.withAppendedId(Events.CONTENT_URI, (Long) en.getValue()[0]))).build());
            result.stats.numDeletes++;
        }
        for (long s : strays) ops.add(ContentProviderOperation.newDelete(ad(ContentUris.withAppendedId(Events.CONTENT_URI, s))).build());
        // Events before their exceptions: the provider links ORIGINAL_SYNC_ID to an existing row.
        if (!ops.isEmpty()) cp.applyBatch(ops);
        if (!later.isEmpty()) cp.applyBatch(later);
    }

    private ContentValues values(Ics.Event e, long calRow, String sid, String hash, Ics.Event master) throws JSONException {
        ContentValues v = new ContentValues();
        v.put(Events.CALENDAR_ID, calRow);
        v.put(Events._SYNC_ID, sid);
        v.put(Events.TITLE, e.title);
        v.put(Events.EVENT_LOCATION, e.location);
        v.put(Events.DESCRIPTION, e.notes);
        v.put(Events.ALL_DAY, e.allDay ? 1 : 0);
        v.put(Events.EVENT_TIMEZONE, e.allDay ? "UTC" : e.zone);
        v.put(Events.DTSTART, e.start);
        v.put(Events.ORGANIZER, acc.name);
        v.put(Events.HAS_ALARM, 0);
        v.put(Events.STATUS, Events.STATUS_CONFIRMED);
        v.put(Events.DIRTY, 0);
        long end = Math.max(e.end, e.start);
        String exdate = null;
        boolean repeats = e.rid == null && e.rrule != null;
        if (repeats) {
            // A repeating event has a DURATION instead of DTEND (CalendarContract.Events).
            long len = end - e.start;
            v.put(Events.DURATION, e.allDay ? "P" + Math.max(1, len / 86_400_000L) + "D" : "P" + (len / 1000) + "S");
            v.putNull(Events.DTEND);
            v.put(Events.RRULE, e.rrule);
            if (!e.exdates.isEmpty()) {
                StringBuilder b = new StringBuilder();
                for (String x : e.exdates) {
                    if (b.length() > 0) b.append(',');
                    b.append(Ics.androidTime(Ics.millis(x, null, x.length() == 8)));
                }
                exdate = b.toString();
            }
            v.put(Events.EXDATE, exdate);
        } else {
            v.put(Events.DTEND, end);
            v.putNull(Events.DURATION);
            v.putNull(Events.RRULE);
            v.putNull(Events.EXDATE);
        }
        if (e.rid != null) {
            v.put(Events.ORIGINAL_SYNC_ID, e.id);
            v.put(Events.ORIGINAL_INSTANCE_TIME, Ics.millis(e.rid, null, e.rid.length() == 8));
            v.put(Events.ORIGINAL_ALL_DAY, master != null ? (master.allDay ? 1 : 0) : (e.rid.length() == 8 ? 1 : 0));
        }
        v.put(Events.SYNC_DATA1, hash);
        v.put(Events.SYNC_DATA2, written(e.title, e.location, e.notes, e.start, end, e.allDay ? "UTC" : e.zone, e.allDay,
                repeats ? e.rrule : null, exdate).toString());
        return v;
    }

    /** What a row held when Sol wrote it, to tell the person's changes from Sol's. */
    private static JSONObject written(String title, String loc, String notes, long start, long end, String zone,
                                      boolean allDay, String rrule, String exdate) throws JSONException {
        return new JSONObject().put("t", nz(title)).put("l", nz(loc)).put("d", nz(notes)).put("s", start).put("e", end)
                .put("z", nz(zone)).put("a", allDay).put("r", nz(rrule)).put("x", nz(exdate));
    }

    /** EXDATE values (CalendarContract form, possibly "TZID;..." prefixed) in Sol's occurrence form. */
    private static List<String> splitExdates(String exdate, boolean allDay) {
        List<String> out = new ArrayList<>();
        if (exdate == null || exdate.isEmpty()) return out;
        String tz = null, v = exdate;
        int semi = v.indexOf(';');
        if (semi >= 0) { tz = v.substring(0, semi); v = v.substring(semi + 1); }
        for (String x : v.split(",")) {
            x = x.trim();
            if (x.isEmpty()) continue;
            try { out.add(Ics.solTime(Ics.millis(x, tz, x.length() == 8), allDay)); } catch (RuntimeException ignored) { }
        }
        return out;
    }

    // ---- the CLI ----

    /** A failed Calendar action; the message names the action and code only (no event text). */
    private static final class CliError extends Exception {
        CliError(String m) { super(m); }
    }

    /**
     * Runs a Calendar action and returns its JSON output. With missingOk, an event Sol no longer
     * has (not_found) returns null instead of failing.
     */
    private JSONObject call(String action, JSONObject args, boolean missingOk) throws Exception {
        String[] r = CalendarSync.doAction(ctx, action, args);
        if ("0".equals(r[0])) {
            String out = r[1];
            int brace = out.indexOf('{');
            return brace >= 0 ? new JSONObject(out.substring(brace)) : new JSONObject();
        }
        if (missingOk && r[1].contains("not_found")) return null;
        String code = r[1].matches("(?s).*\\b(bad_request|not_found|forbidden|conflict|unavailable|not_implemented)\\b.*")
                ? r[1].replaceAll("(?s).*\\b(bad_request|not_found|forbidden|conflict|unavailable|not_implemented)\\b.*", "$1") : "exit " + r[0];
        throw new CliError(action + " failed: " + code);
    }

    private static String hash(String s) {
        try {
            byte[] d = MessageDigest.getInstance("SHA-256").digest(s.getBytes(StandardCharsets.UTF_8));
            StringBuilder b = new StringBuilder();
            for (int i = 0; i < 12; i++) b.append(String.format("%02x", d[i]));
            return b.toString();
        } catch (java.security.NoSuchAlgorithmException e) {
            return String.valueOf(s.hashCode());
        }
    }

    private static String nz(String s) { return s == null ? "" : s; }
}
