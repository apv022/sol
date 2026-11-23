package app.sol;

import java.time.Instant;
import java.time.LocalDate;
import java.time.LocalDateTime;
import java.time.ZoneId;
import java.time.ZoneOffset;
import java.time.format.DateTimeFormatter;
import java.util.ArrayList;
import java.util.List;

/**
 * The subset of iCalendar (RFC 5545) that the Calendar app's `calendar.export` writes, read
 * into events for the phone's calendar provider (CalendarSyncer).
 *
 * Calendar exports one VEVENT per event (UID "<id>@sol") with DTSTART/DTEND (a date for all-day
 * events, a TZID wall time, or UTC), SUMMARY, LOCATION, DESCRIPTION, RRULE, EXDATE and VALARM,
 * and one more VEVENT with the same UID and a RECURRENCE-ID per changed occurrence. Lines are
 * unfolded first; anything else (VALARM, X- properties, attendees) is ignored. Times become
 * epoch milliseconds; a date is UTC midnight, as CalendarContract wants for all-day events.
 */
final class Ics {
    private Ics() { }

    /** One VEVENT. */
    static final class Event {
        /** The Sol event id (the UID without "@sol"). */
        String id;
        /** The occurrence this VEVENT overrides, in Sol's form (20261130T083000Z or 20261130); null for a whole event. */
        String rid;
        String title = "", location = "", notes = "";
        boolean allDay;
        /** IANA zone of DTSTART (UTC for all-day and UTC times, the device's for floating times). */
        String zone;
        long start, end;
        /** RRULE value, or null. */
        String rrule;
        /** EXDATEs in Sol's form, UTC (20261207T083000Z) or dates for all-day events. */
        final List<String> exdates = new ArrayList<>();
        /** The VEVENT's lines without DTSTAMP: changes when the event changes. */
        final StringBuilder text = new StringBuilder();
        private boolean hasEnd;
        private long duration = -1;
    }

    private static final DateTimeFormatter UTC_FORM = DateTimeFormatter.ofPattern("yyyyMMdd'T'HHmmss'Z'").withZone(ZoneOffset.UTC);
    private static final DateTimeFormatter DATE_FORM = DateTimeFormatter.ofPattern("yyyyMMdd").withZone(ZoneOffset.UTC);
    private static final DateTimeFormatter LOCAL_FORM = DateTimeFormatter.ofPattern("yyyyMMdd'T'HHmmss");

    /** Parses ics text; VEVENTs without a UID or DTSTART are skipped. */
    static List<Event> parse(String ics) {
        List<Event> out = new ArrayList<>();
        Event ev = null;
        int nested = 0; // inside a VALARM (or anything else) within the VEVENT
        for (String line : unfold(ics)) {
            int colon = valueColon(line);
            if (colon < 0) continue;
            String head = line.substring(0, colon), value = line.substring(colon + 1);
            String[] parts = head.split(";");
            String name = parts[0].toUpperCase(java.util.Locale.ROOT);
            if (name.equals("BEGIN")) {
                if (value.equalsIgnoreCase("VEVENT")) { ev = new Event(); nested = 0; }
                else if (ev != null) nested++;
                continue;
            }
            if (name.equals("END")) {
                if (value.equalsIgnoreCase("VEVENT")) {
                    if (ev != null && ev.id != null && ev.zone != null) { finish(ev); out.add(ev); }
                    ev = null;
                } else if (ev != null && nested > 0) nested--;
                continue;
            }
            if (ev == null || nested > 0) continue;
            if (!name.equals("DTSTAMP")) ev.text.append(line).append('\n');
            String tzid = param(parts, "TZID");
            boolean date = "DATE".equalsIgnoreCase(param(parts, "VALUE")) || value.length() == 8;
            try {
                switch (name) {
                    case "UID": ev.id = value.endsWith("@sol") ? value.substring(0, value.length() - 4) : value; break;
                    case "SUMMARY": ev.title = text(value); break;
                    case "LOCATION": ev.location = text(value); break;
                    case "DESCRIPTION": ev.notes = text(value); break;
                    case "RRULE": ev.rrule = value; break;
                    case "DTSTART":
                        ev.allDay = date;
                        ev.start = millis(value, tzid, date);
                        ev.zone = date || value.endsWith("Z") ? "UTC" : tzid != null ? tzid : ZoneId.systemDefault().getId();
                        break;
                    case "DTEND": ev.end = millis(value, tzid, date); ev.hasEnd = true; break;
                    case "DURATION": ev.duration = duration(value); break;
                    case "RECURRENCE-ID": ev.rid = solTime(millis(value, tzid, date), date); break;
                    case "EXDATE":
                        for (String v : value.split(",")) {
                            v = v.trim();
                            if (!v.isEmpty()) ev.exdates.add(solTime(millis(v, tzid, v.length() == 8), v.length() == 8));
                        }
                        break;
                    default: break;
                }
            } catch (RuntimeException e) {
                // A value this parser does not understand: the event keeps its other fields.
            }
        }
        return out;
    }

    private static void finish(Event ev) {
        if (ev.hasEnd) return;
        if (ev.duration >= 0) ev.end = ev.start + ev.duration;
        else ev.end = ev.start + (ev.allDay ? 86_400_000L : 0); // RFC 5545: no DTEND is one day / zero length
    }

    /** Lines with folding (CRLF + space or tab) undone. */
    static List<String> unfold(String ics) {
        List<String> out = new ArrayList<>();
        StringBuilder cur = null;
        for (String raw : ics.split("\r?\n")) {
            if (!raw.isEmpty() && (raw.charAt(0) == ' ' || raw.charAt(0) == '\t') && cur != null) {
                cur.append(raw, 1, raw.length());
                continue;
            }
            if (cur != null) out.add(cur.toString());
            cur = new StringBuilder(raw);
        }
        if (cur != null && cur.length() > 0) out.add(cur.toString());
        return out;
    }

    /** The colon that ends the property name and parameters (not one inside a quoted parameter). */
    private static int valueColon(String line) {
        boolean quoted = false;
        for (int i = 0; i < line.length(); i++) {
            char c = line.charAt(i);
            if (c == '"') quoted = !quoted;
            else if (c == ':' && !quoted) return i;
        }
        return -1;
    }

    private static String param(String[] parts, String name) {
        for (int i = 1; i < parts.length; i++) {
            int eq = parts[i].indexOf('=');
            if (eq > 0 && parts[i].substring(0, eq).equalsIgnoreCase(name)) {
                String v = parts[i].substring(eq + 1);
                return v.startsWith("\"") && v.endsWith("\"") && v.length() >= 2 ? v.substring(1, v.length() - 1) : v;
            }
        }
        return null;
    }

    /** A TEXT value unescaped (\n, \, \; \\). */
    static String text(String v) {
        StringBuilder b = new StringBuilder(v.length());
        for (int i = 0; i < v.length(); i++) {
            char c = v.charAt(i);
            if (c == '\\' && i + 1 < v.length()) {
                char n = v.charAt(++i);
                b.append(n == 'n' || n == 'N' ? '\n' : n);
            } else b.append(c);
        }
        return b.toString();
    }

    /** A DATE or DATE-TIME as epoch ms: a date is UTC midnight; a time is UTC (Z), in tzid, or floating (device zone). */
    static long millis(String v, String tzid, boolean date) {
        if (date) return LocalDate.parse(v.substring(0, 8), DateTimeFormatter.BASIC_ISO_DATE).atStartOfDay(ZoneOffset.UTC).toInstant().toEpochMilli();
        boolean utc = v.endsWith("Z");
        LocalDateTime t = LocalDateTime.parse(utc ? v.substring(0, v.length() - 1) : v, LOCAL_FORM);
        ZoneId z = utc ? ZoneOffset.UTC : zone(tzid);
        return t.atZone(z).toInstant().toEpochMilli();
    }

    /** The zone named tzid, or the device's when it is missing or unknown. */
    static ZoneId zone(String tzid) {
        if (tzid != null) {
            try { return ZoneId.of(tzid); } catch (RuntimeException ignored) { }
        }
        return ZoneId.systemDefault();
    }

    /** Sol's occurrence form: 20261130T083000Z, or 20261130 for an all-day event. */
    static String solTime(long ms, boolean date) {
        return (date ? DATE_FORM : UTC_FORM).format(Instant.ofEpochMilli(ms));
    }

    /** CalendarContract's EXDATE form: UTC times, comma-separated. */
    static String androidTime(long ms) { return UTC_FORM.format(Instant.ofEpochMilli(ms)); }

    /**
     * An RFC 5545 / RFC 2445 duration (P1D, PT1H30M, P3600S, -PT10M) in ms; Android's own writers
     * use P<seconds>S and P<days>D. Throws on anything else.
     */
    static long duration(String v) {
        int i = 0;
        boolean neg = false;
        if (v.startsWith("-")) { neg = true; i = 1; } else if (v.startsWith("+")) i = 1;
        if (i >= v.length() || Character.toUpperCase(v.charAt(i)) != 'P') throw new IllegalArgumentException("bad duration");
        long total = 0, n = 0;
        boolean digits = false;
        for (i++; i < v.length(); i++) {
            char c = Character.toUpperCase(v.charAt(i));
            if (c >= '0' && c <= '9') { n = n * 10 + (c - '0'); digits = true; continue; }
            if (c == 'T') continue;
            if (!digits) throw new IllegalArgumentException("bad duration");
            switch (c) {
                case 'W': total += n * 7 * 86_400_000L; break;
                case 'D': total += n * 86_400_000L; break;
                case 'H': total += n * 3_600_000L; break;
                case 'M': total += n * 60_000L; break;
                case 'S': total += n * 1000L; break;
                default: throw new IllegalArgumentException("bad duration");
            }
            n = 0;
            digits = false;
        }
        if (digits) throw new IllegalArgumentException("bad duration");
        return neg ? -total : total;
    }
}
