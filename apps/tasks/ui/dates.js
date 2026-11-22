// Dates in Tasks are wall-clock strings ("2026-11-21", "09:00"), never instants. This module is
// the twin of dates.go: parseQuick turns "call mom tomorrow 9am #family" into fields (for the live
// preview while typing), and both implementations pass test/../testdata/dates.json. formatDue
// writes a due date the way Reminders does.

const pad = (n) => String(n).padStart(2, '0');

/** A Date as "YYYY-MM-DD" in local time. */
export const ymd = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;

/** "YYYY-MM-DD" as a local-midnight Date, or null when it is not a real date. */
export function parseYMD(s) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s || '');
  if (!m) return null;
  const d = new Date(+m[1], +m[2] - 1, +m[3]);
  return d.getMonth() === +m[2] - 1 && d.getDate() === +m[3] ? d : null;
}

const midnight = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate());
const addDays = (d, n) => new Date(d.getFullYear(), d.getMonth(), d.getDate() + n);
/** Adds months, clamping to the end of a shorter month (Jan 31 + 1 month = Feb 28). */
export function addMonths(d, n) {
  const first = new Date(d.getFullYear(), d.getMonth() + n, 1);
  const last = new Date(first.getFullYear(), first.getMonth() + 1, 0).getDate();
  return new Date(first.getFullYear(), first.getMonth(), Math.min(d.getDate(), last));
}

const WEEKDAYS = ['sunday', 'monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday'];
const MONTHS = ['january', 'february', 'march', 'april', 'may', 'june', 'july', 'august', 'september', 'october', 'november', 'december'];
const weekdayIndex = (s) => { s = s.toLowerCase(); return Math.max(0, WEEKDAYS.findIndex((n) => n.startsWith(s.slice(0, 3)))); };
const monthIndex = (s) => MONTHS.findIndex((n) => n.startsWith(s.toLowerCase().slice(0, 3))) + 1;

const WD = '(sun(?:day)?|mon(?:day)?|tue(?:s(?:day)?)?|wed(?:nesday)?|thu(?:r(?:s(?:day)?)?)?|fri(?:day)?|sat(?:urday)?)';
const MON = '(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)';
const RE = {
  tag: /(^|\s)#([\w-]+)/i,
  flag: /(^|\s)!(\s|$)/,
  every: new RegExp('\\bevery\\s+(day|week|month|year|' + WD + ')\\b', 'i'),
  iso: /\b(\d{4})-(\d{2})-(\d{2})\b/g,
  md: new RegExp('\\b(?:on\\s+)?' + MON + '\\.?\\s+(\\d{1,2})(?:st|nd|rd|th)?\\b(?:,?\\s+(\\d{4})\\b)?', 'gi'),
  dm: new RegExp('\\b(?:on\\s+)?(\\d{1,2})(?:st|nd|rd|th)?\\s+' + MON + '\\b(?:,?\\s+(\\d{4})\\b)?', 'gi'),
  slash: /\b(\d{1,2})\/(\d{1,2})(?:\/(\d{2,4}))?\b/g,
  in: /\bin\s+(\d+|an?)\s+(minute|min|hour|hr|day|week|month|year)s?\b/i,
  next: /\bnext\s+(week|month|year)\b/i,
  wd: new RegExp('\\b(?:(on|next|this|by)\\s+)?' + WD + '\\b', 'gi'),
  day: /\b(today|tonight|tomorrow|tmrw)\b/i,
  part: /\bthis\s+(morning|afternoon|evening)\b/i,
  ampm: /\b(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b/i,
  h24: /\b(?:at\s+)?(\d{1,2}):(\d{2})\b/,
  atn: /\bat\s+(\d{1,2})\b/i,
  noon: /\b(?:at\s+)?noon\b/i,
  tail: /\s+(on|at|by|due|in|for)$/i,
};

const cut = (s, m) => s.slice(0, m.index) + ' ' + s.slice(m.index + m[0].length);
const validDate = (y, m, d) => { const t = new Date(y, m - 1, d); return m >= 1 && m <= 12 && t.getMonth() === m - 1 && t.getDate() === d ? t : null; };
const yearFor = (m, d, today) => { const t = validDate(today.getFullYear(), m, d); return t && t < today ? today.getFullYear() + 1 : today.getFullYear(); };
const hour12 = (h) => (h >= 1 && h <= 6 ? h + 12 : h);

/**
 * Pulls a date, time, repetition, flag and #list out of a quick-add line; what is left is the
 * title. `now` is a Date. Never fails: a line with nothing recognised is all title.
 */
export function parseQuick(input, now = new Date()) {
  const orig = String(input).trim();
  let rest = ' ' + orig + ' ';
  const p = { title: '', due: '', time: '', repeat: '', flag: false, list: '' };
  const today = midnight(now);
  let date = null, hh = 0, mm = 0, haveTime = false, def = null;
  const setTime = (h, m) => { hh = h; mm = m; haveTime = true; };
  let m;

  if ((m = RE.tag.exec(rest))) { p.list = m[2]; rest = cut(rest, m); }
  if ((m = RE.flag.exec(rest))) { p.flag = true; rest = cut(rest, m); }
  if ((m = RE.every.exec(rest))) {
    const w = m[1].toLowerCase();
    if (w === 'day' || w === 'week' || w === 'month' || w === 'year') { p.repeat = { day: 'daily', week: 'weekly', month: 'monthly', year: 'yearly' }[w]; date = today; }
    else { p.repeat = 'weekly'; date = addDays(today, (weekdayIndex(w) - today.getDay() + 7) % 7); }
    rest = cut(rest, m);
  }
  if (!date) {
    // each matcher tries its patterns in turn and stops at the first usable match
    const tryAll = (re, f) => {
      re.lastIndex = 0;
      for (const x of [...rest.matchAll(re)]) { const d = f(x); if (d) { date = d; rest = cut(rest, x); return true; } }
      return false;
    };
    const done =
      tryAll(RE.iso, (x) => validDate(+x[1], +x[2], +x[3])) ||
      tryAll(RE.md, (x) => { const mo = monthIndex(x[1]), d = +x[2]; return validDate(x[3] ? +x[3] : yearFor(mo, d, today), mo, d); }) ||
      tryAll(RE.dm, (x) => { const mo = monthIndex(x[2]), d = +x[1]; return validDate(x[3] ? +x[3] : yearFor(mo, d, today), mo, d); }) ||
      tryAll(RE.slash, (x) => {
        let y = x[3] ? +x[3] : yearFor(+x[1], +x[2], today);
        if (x[3] && y < 100) y += 2000;
        return validDate(y, +x[1], +x[2]);
      });
    if (!done) {
      if ((m = RE.in.exec(rest))) {
        const w = m[1].toLowerCase(), n = w === 'a' || w === 'an' ? 1 : +w, u = m[2].toLowerCase();
        if (u === 'minute' || u === 'min' || u === 'hour' || u === 'hr') {
          const at = new Date(now.getTime() + n * ((u === 'hour' || u === 'hr') ? 3600e3 : 60e3));
          date = midnight(at); setTime(at.getHours(), at.getMinutes());
        } else if (u === 'day') date = addDays(today, n);
        else if (u === 'week') date = addDays(today, 7 * n);
        else if (u === 'month') date = addMonths(today, n);
        else date = addMonths(today, 12 * n);
        rest = cut(rest, m);
      } else if ((m = RE.next.exec(rest))) {
        const u = m[1].toLowerCase();
        date = u === 'week' ? addDays(today, 7) : addMonths(today, u === 'month' ? 1 : 12);
        rest = cut(rest, m);
      } else {
        let found = false;
        RE.wd.lastIndex = 0;
        for (const x of [...rest.matchAll(RE.wd)]) {
          const prefix = (x[1] || '').toLowerCase(), name = x[2].toLowerCase();
          if (!prefix && !WEEKDAYS.includes(name)) continue;
          let diff = (weekdayIndex(name) - today.getDay() + 7) % 7;
          if (diff === 0 && prefix !== 'this') diff = 7;
          date = addDays(today, diff); rest = cut(rest, x); found = true;
          break;
        }
        if (!found) {
          if ((m = RE.day.exec(rest))) {
            const w = m[1].toLowerCase();
            if (w === 'today') date = today;
            else if (w === 'tonight') { date = today; def = [20, 0]; }
            else date = addDays(today, 1);
            rest = cut(rest, m);
          } else if ((m = RE.part.exec(rest))) {
            date = today; def = { morning: [9, 0], afternoon: [15, 0], evening: [18, 0] }[m[1].toLowerCase()];
            rest = cut(rest, m);
          }
        }
      }
    }
  }
  if (!haveTime && (m = RE.ampm.exec(rest)) && +m[1] >= 1 && +m[1] <= 12 && (m[2] === undefined || +m[2] < 60)) {
    setTime((+m[1] % 12) + (m[3].toLowerCase() === 'pm' ? 12 : 0), m[2] ? +m[2] : 0);
    rest = cut(rest, m);
  }
  if (!haveTime && (m = RE.h24.exec(rest)) && +m[1] < 24 && +m[2] < 60) { setTime(+m[1], +m[2]); rest = cut(rest, m); }
  if (!haveTime && (m = RE.noon.exec(rest))) { setTime(12, 0); rest = cut(rest, m); }
  if (!haveTime && (m = RE.atn.exec(rest)) && +m[1] >= 1 && +m[1] <= 12) { setTime(hour12(+m[1]), 0); rest = cut(rest, m); }
  if (!haveTime && def) setTime(def[0], def[1]);
  if (haveTime && !date) date = hh * 60 + mm <= now.getHours() * 60 + now.getMinutes() ? addDays(today, 1) : today;

  let title = rest.split(/\s+/).filter(Boolean).join(' ');
  for (;;) { const t = title.replace(RE.tail, ''); if (t === title) break; title = t; }
  if (!title) return { title: orig, due: '', time: '', repeat: '', flag: false, list: '' };
  p.title = title;
  if (date) p.due = ymd(date);
  if (haveTime) p.time = `${pad(hh)}:${pad(mm)}`;
  return p;
}

/** "9:30 AM" for "09:30" (12-hour, like the Reminders list), "" for none. */
export function formatTime(t) {
  const m = /^(\d{1,2}):(\d{2})$/.exec(t || '');
  if (!m) return '';
  const h = +m[1];
  return `${h % 12 || 12}:${m[2]} ${h < 12 ? 'AM' : 'PM'}`;
}

const SHORT_DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
const SHORT_MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

/** The day of a due date in words: Today, Tomorrow, Yesterday, a weekday within the next week, else "Nov 25" (with the year when it is not this year). */
export function formatDay(due, now = new Date()) {
  const d = parseYMD(due);
  if (!d) return '';
  const diff = Math.round((d - midnight(now)) / 864e5);
  if (diff === 0) return 'Today';
  if (diff === 1) return 'Tomorrow';
  if (diff === -1) return 'Yesterday';
  if (diff > 1 && diff < 7) return SHORT_DAYS[d.getDay()];
  return `${SHORT_MONTHS[d.getMonth()]} ${d.getDate()}${d.getFullYear() === now.getFullYear() ? '' : ', ' + d.getFullYear()}`;
}

/** {text, overdue} for a task's due date and optional time: "Tomorrow, 9:00 AM"; overdue once the day (or, with a time, the moment) has passed. */
export function formatDue(due, time, now = new Date()) {
  const d = parseYMD(due);
  if (!d) return { text: '', overdue: false };
  const t = formatTime(time);
  const text = formatDay(due, now) + (t ? ', ' + t : '');
  const m = /^(\d{1,2}):(\d{2})$/.exec(time || '');
  const at = m ? new Date(d.getFullYear(), d.getMonth(), d.getDate(), +m[1], +m[2]) : null;
  return { text, overdue: at ? at < now : d < midnight(now) };
}

/** The longer date of a due day for the details panel: "Fri, Nov 21, 2026". */
export function formatLong(due) {
  const d = parseYMD(due);
  return d ? `${SHORT_DAYS[d.getDay()].slice(0, 3)}, ${SHORT_MONTHS[d.getMonth()]} ${d.getDate()}, ${d.getFullYear()}` : '';
}

const REPEAT_WORDS = { daily: 'Daily', weekly: 'Weekly', monthly: 'Monthly', yearly: 'Yearly' };
/** "Weekly" for "weekly", "" for none. */
export const formatRepeat = (r) => REPEAT_WORDS[r] || '';

/** What the quick-add bar shows under the field once it understands part of the line: "Tomorrow, 9:00 AM · Weekly". */
export function describeParsed(p, now = new Date()) {
  const bits = [];
  if (p.due) bits.push(formatDue(p.due, p.time, now).text);
  if (p.repeat) bits.push(formatRepeat(p.repeat));
  if (p.flag) bits.push('Flagged');
  return bits.join(' · ');
}
