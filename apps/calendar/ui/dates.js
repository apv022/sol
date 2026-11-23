// Date and grid helpers for the Calendar UI: pure functions, no DOM, tested in a browser
// (test/dates.test.js). Days are local Date objects at midnight; all-day events come as
// 'YYYY-MM-DD' with an exclusive end, timed ones as RFC 3339 instants already in the viewer's
// zone (the app returns them that way), so the UI positions by the local clock and never does
// zone math except to show an event's own zone in the editor (wallIn).

/** 'YYYY-MM-DD' of a local date. */
export const ymd = (d) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;

/** A local midnight from 'YYYY-MM-DD' (null when it is not one). */
export function parseYMD(s) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(s || ''));
  if (!m) return null;
  const d = new Date(+m[1], +m[2] - 1, +m[3]);
  return d.getMonth() === +m[2] - 1 ? d : null;
}

/** Midnight of the day of d. */
export const startOfDay = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate());
/** d plus n calendar days (DST-safe: works on the date, not on milliseconds). */
export const addDays = (d, n) => new Date(d.getFullYear(), d.getMonth(), d.getDate() + n, d.getHours(), d.getMinutes());
/** The same day n months later, clamped to the month's end (Jan 31 + 1 = Feb 28). */
export function addMonths(d, n) {
  const t = new Date(d.getFullYear(), d.getMonth() + n, 1);
  const last = new Date(t.getFullYear(), t.getMonth() + 1, 0).getDate();
  return new Date(t.getFullYear(), t.getMonth(), Math.min(d.getDate(), last));
}
export const sameDay = (a, b) => a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
/** Whole days from a to b (local dates). */
export const daysBetween = (a, b) => Math.round((startOfDay(b) - startOfDay(a)) / 864e5);

/** The first day of the week holding d; weekStart 1 is Monday, 0 Sunday. */
export function startOfWeek(d, weekStart = 1) {
  const s = startOfDay(d);
  return addDays(s, -((s.getDay() - weekStart + 7) % 7));
}

/** The days a month view shows: whole weeks from the week of the 1st to the week of the last day. */
export function monthGrid(year, month, weekStart = 1) {
  const first = new Date(year, month, 1), last = new Date(year, month + 1, 0);
  const from = startOfWeek(first, weekStart);
  const n = daysBetween(from, last) + 1;
  const weeks = Math.ceil(n / 7);
  return Array.from({ length: weeks * 7 }, (_, i) => addDays(from, i));
}

/** The range [from, to) of local midnights a view shows around day d. */
export function viewRange(view, d, weekStart = 1) {
  switch (view) {
    case 'day': return { from: startOfDay(d), to: addDays(startOfDay(d), 1) };
    case 'week': { const f = startOfWeek(d, weekStart); return { from: f, to: addDays(f, 7) }; }
    case 'month': { const g = monthGrid(d.getFullYear(), d.getMonth(), weekStart); return { from: g[0], to: addDays(g[g.length - 1], 1) }; }
    default: return { from: startOfDay(d), to: addDays(startOfDay(d), 42) };
  }
}

/** Moves the focused day one view step back (-1) or forward (1). */
export function step(view, d, dir) {
  if (view === 'month') return addMonths(new Date(d.getFullYear(), d.getMonth(), 1), dir);
  if (view === 'week') return addDays(d, 7 * dir);
  if (view === 'list') return addDays(d, 7 * dir);
  return addDays(d, dir);
}

/** Local start and end Dates of an occurrence (all-day end exclusive, at midnight). */
export function occTimes(o) {
  if (o.all_day) return { start: parseYMD(o.start), end: parseYMD(o.end) || addDays(parseYMD(o.start), 1) };
  return { start: new Date(o.start), end: new Date(o.end) };
}

/** Whether an occurrence touches the local day d. A timed event ending at midnight does not touch the next day. */
export function onDay(o, d) {
  const { start, end } = occTimes(o);
  const ds = startOfDay(d), de = addDays(ds, 1);
  if (o.all_day) return start < de && end > ds;
  return start < de && (end > ds || (+start === +end && start >= ds));
}

/** Whether an occurrence goes in the all-day row of a time grid: all-day ones and timed ones of 24 hours or more. */
export const inAllDayRow = (o) => o.all_day || (new Date(o.end) - new Date(o.start)) >= 864e5;

/** Minutes since local midnight. */
export const minutesOf = (d) => d.getHours() * 60 + d.getMinutes();

/**
 * Lays out the timed occurrences of one day column, Apple style: clusters of overlapping events
 * share columns, each event takes the first free column and widens into free columns to its
 * right. Returns [{o, top, height, left, width}] in minutes and fractions of the column width.
 * min is the shortest drawn length in minutes (so 5-minute events stay tappable).
 */
export function layoutDay(occs, day, min = 20) {
  const ds = startOfDay(day), de = addDays(ds, 1);
  const items = occs.map((o) => {
    const { start, end } = occTimes(o);
    const s = Math.max(0, (Math.max(start, ds) - ds) / 6e4);
    const e = Math.min(24 * 60, Math.max(s + min, (Math.min(end, de) - ds) / 6e4));
    return { o, s, e };
  }).sort((a, b) => a.s - b.s || b.e - a.e);
  const out = [];
  let cluster = [], cols = [], clusterEnd = -1;
  const flush = () => {
    const n = cols.length;
    for (const it of cluster) {
      let span = 1;
      for (let c = it.col + 1; c < n; c++) {
        if (cluster.some((x) => x.col === c && x.s < it.e && x.e > it.s)) break;
        span++;
      }
      out.push({ o: it.o, top: it.s, height: it.e - it.s, left: it.col / n, width: span / n });
    }
    cluster = []; cols = []; clusterEnd = -1;
  };
  for (const it of items) {
    if (cluster.length && it.s >= clusterEnd) flush();
    let c = cols.findIndex((end) => end <= it.s);
    if (c < 0) { c = cols.length; cols.push(it.e); } else cols[c] = it.e;
    it.col = c;
    cluster.push(it);
    clusterEnd = Math.max(clusterEnd, it.e);
  }
  if (cluster.length) flush();
  return out;
}

/**
 * Lays out the all-day row of a range of days: each occurrence spans columns [c0, c1) and gets
 * the first lane that is free across its span. Returns [{o, c0, c1, lane}] (1-based columns).
 */
export function layoutAllDay(occs, from, days) {
  const lanes = [];
  const out = [];
  const items = occs.map((o) => {
    const { start, end } = occTimes(o);
    const s = Math.max(0, daysBetween(from, start));
    let e = Math.min(days, o.all_day ? daysBetween(from, end) : daysBetween(from, end) + (+startOfDay(end) !== +end ? 1 : 0));
    if (e <= s) e = s + 1;
    return { o, s, e };
  }).filter((x) => x.s < days && x.e > 0).sort((a, b) => a.s - b.s || (b.e - b.s) - (a.e - a.s));
  for (const it of items) {
    let lane = lanes.findIndex((end) => end <= it.s);
    if (lane < 0) { lane = lanes.length; lanes.push(it.e); } else lanes[lane] = it.e;
    out.push({ o: it.o, c0: it.s + 1, c1: it.e + 1, lane: lane + 1 });
  }
  return { items: out, lanes: lanes.length };
}

/** Groups occurrences by the local days they touch, for the agenda: [{day, items}] in order, empty days left out. */
export function agendaDays(occs, from, to) {
  const out = [];
  for (let d = startOfDay(from); d < to; d = addDays(d, 1)) {
    const items = occs.filter((o) => onDay(o, d));
    if (items.length) out.push({ day: d, items });
  }
  return out;
}

// ---------- words ----------

const locale = undefined; // the browser's
const fmt = (opts) => new Intl.DateTimeFormat(locale, opts);
export const fmtTime = (d) => fmt({ hour: 'numeric', minute: '2-digit' }).format(d);
export const fmtHour = (h) => fmt({ hour: 'numeric' }).format(new Date(2000, 0, 1, h));
export const fmtMonthYear = (d) => fmt({ month: 'long', year: 'numeric' }).format(d);
export const fmtDayLong = (d) => fmt({ weekday: 'long', month: 'long', day: 'numeric', year: 'numeric' }).format(d);
export const fmtDayShort = (d) => fmt({ weekday: 'short', month: 'short', day: 'numeric' }).format(d);
export const fmtWeekday = (d, width = 'short') => fmt({ weekday: width }).format(d);

/** The toolbar title of a view. */
export function viewTitle(view, d, weekStart = 1) {
  if (view === 'day') return fmtDayLong(d);
  if (view === 'week') {
    const s = startOfWeek(d, weekStart), e = addDays(s, 6);
    if (s.getMonth() === e.getMonth()) return fmtMonthYear(s);
    const m = fmt({ month: 'short' });
    return s.getFullYear() === e.getFullYear() ? `${m.format(s)} – ${m.format(e)} ${e.getFullYear()}` : `${m.format(s)} ${s.getFullYear()} – ${m.format(e)} ${e.getFullYear()}`;
  }
  return fmtMonthYear(d);
}

/** An agenda day heading: Today, Tomorrow, Yesterday, or the weekday and date. */
export function dayLabel(d, today) {
  const n = daysBetween(today, d);
  if (n === 0) return 'Today';
  if (n === 1) return 'Tomorrow';
  if (n === -1) return 'Yesterday';
  return d.getFullYear() === today.getFullYear() ? fmt({ weekday: 'long', month: 'short', day: 'numeric' }).format(d) : fmt({ weekday: 'short', month: 'short', day: 'numeric', year: 'numeric' }).format(d);
}

/** "9:00 – 10:30", "all-day", or for a day inside a longer event "until 11:00" / "from 22:00". */
export function timeLabel(o, day) {
  if (o.all_day) return 'all-day';
  const { start, end } = occTimes(o);
  const startsToday = !day || sameDay(start, day);
  const endsToday = !day || sameDay(end, day) || (+end === +addDays(startOfDay(day), 1));
  if (startsToday && endsToday) return +start === +end ? fmtTime(start) : `${fmtTime(start)} – ${fmtTime(end)}`;
  if (startsToday) return 'from ' + fmtTime(start);
  if (endsToday) return 'until ' + fmtTime(end);
  return 'all-day';
}

export const ALERTS = [
  [0, 'At time of event'], [5, '5 minutes before'], [10, '10 minutes before'], [15, '15 minutes before'], [30, '30 minutes before'],
  [60, '1 hour before'], [120, '2 hours before'], [1440, '1 day before'], [2880, '2 days before'], [10080, '1 week before'],
];
export const ALERTS_ALL_DAY = [[0, 'On day of event (9 AM)'], [1440, '1 day before (9 AM)'], [2880, '2 days before (9 AM)'], [10080, '1 week before']];
/** Words for an alert of m minutes. */
export function alertLabel(m, allDay) {
  const hit = (allDay ? ALERTS_ALL_DAY : ALERTS).find(([v]) => v === m);
  if (hit) return hit[1];
  if (m % 1440 === 0) return `${m / 1440} days before`;
  if (m % 60 === 0) return `${m / 60} hours before`;
  return `${m} minutes before`;
}

export const REPEATS = [['', 'Never'], ['daily', 'Every Day'], ['weekdays', 'Every Weekday'], ['weekly', 'Every Week'], ['biweekly', 'Every 2 Weeks'], ['monthly', 'Every Month'], ['yearly', 'Every Year']];
export const repeatLabel = (r) => (REPEATS.find(([v]) => v === r) || [r, 'Custom'])[1];

// ---------- zones ----------

/** The browser's IANA zone. */
export const deviceZone = () => { try { return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'; } catch { return 'UTC'; } };

/** The wall date and time of instant d in zone: {date: 'YYYY-MM-DD', time: 'HH:MM'}. */
export function wallIn(d, zone) {
  let parts;
  try { parts = new Intl.DateTimeFormat('en-US', { timeZone: zone || undefined, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).formatToParts(d); } catch { parts = null; }
  if (!parts) return { date: ymd(d), time: `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}` };
  const p = Object.fromEntries(parts.map((x) => [x.type, x.value]));
  const hh = p.hour === '24' ? '00' : p.hour;
  return { date: `${p.year}-${p.month}-${p.day}`, time: `${hh}:${p.minute}` };
}

/** Every zone name the browser knows, with the ones given first (deduplicated). */
export function zoneList(...first) {
  let all = [];
  try { all = Intl.supportedValuesOf ? Intl.supportedValuesOf('timeZone') : []; } catch { all = []; }
  return [...new Set([...first.filter(Boolean), ...all, 'UTC'])];
}

/** 'HH:MM' rounded down to step minutes, from minutes since midnight. */
export function hhmm(min, stepMin = 1) {
  const m = Math.max(0, Math.min(24 * 60 - stepMin, Math.floor(min / stepMin) * stepMin));
  return `${String(Math.floor(m / 60)).padStart(2, '0')}:${String(m % 60).padStart(2, '0')}`;
}
