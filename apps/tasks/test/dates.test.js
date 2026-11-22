// Tests for ui/dates.js: the quick-add parser must agree with the Go parser on the shared table
// (testdata/dates.json), and the formatters write dates the way the list shows them.
import { test, assert } from '../../../ui/kit/testing.js';
import { parseQuick, formatDue, formatDay, formatTime, formatLong, describeParsed, ymd, parseYMD, addMonths } from '../ui/dates.js';

const at = (s) => { const [d, t] = s.split('T'); const [y, m, dd] = d.split('-').map(Number); const [h, mi] = t.split(':').map(Number); return new Date(y, m - 1, dd, h, mi); };

test('quick-add table shared with the Go parser', async () => {
  const tab = await (await fetch('../testdata/dates.json')).json();
  const now = at(tab.now);
  assert.ok(tab.cases.length > 40, 'table loaded');
  const bad = [];
  for (const c of tab.cases) {
    const got = parseQuick(c.in, now);
    const want = { title: c.title, due: c.due, time: c.time, repeat: c.repeat, flag: c.flag, list: c.list };
    if (JSON.stringify(got) !== JSON.stringify(want)) bad.push(`${c.in}: got ${JSON.stringify(got)} want ${JSON.stringify(want)}`);
  }
  assert.deepEqual(bad, []);
});

test('formatting a due date', () => {
  const now = at('2026-11-18T10:30');
  assert.equal(formatDay('2026-11-18', now), 'Today');
  assert.equal(formatDay('2026-11-19', now), 'Tomorrow');
  assert.equal(formatDay('2026-11-17', now), 'Yesterday');
  assert.equal(formatDay('2026-11-22', now), 'Sunday');
  assert.equal(formatDay('2026-11-25', now), 'Nov 25');
  assert.equal(formatDay('2027-01-09', now), 'Jan 9, 2027');
  assert.equal(formatDay('garbage', now), '');
  assert.deepEqual(formatDue('2026-11-19', '09:00', now), { text: 'Tomorrow, 9:00 AM', overdue: false });
  assert.deepEqual(formatDue('2026-11-18', '', now), { text: 'Today', overdue: false });
  assert.deepEqual(formatDue('2026-11-18', '09:00', now), { text: 'Today, 9:00 AM', overdue: true });
  assert.deepEqual(formatDue('2026-11-18', '23:05', now), { text: 'Today, 11:05 PM', overdue: false });
  assert.deepEqual(formatDue('2026-11-10', '', now), { text: 'Nov 10', overdue: true });
  assert.deepEqual(formatDue('', '', now), { text: '', overdue: false });
});

test('times and dates', () => {
  assert.equal(formatTime('00:15'), '12:15 AM');
  assert.equal(formatTime('12:00'), '12:00 PM');
  assert.equal(formatTime('13:30'), '1:30 PM');
  assert.equal(formatTime(''), '');
  assert.equal(formatLong('2026-11-21'), 'Sat, Nov 21, 2026');
  assert.equal(ymd(new Date(2026, 0, 5)), '2026-01-05');
  assert.equal(parseYMD('2026-02-30'), null);
  assert.equal(ymd(addMonths(new Date(2026, 0, 31), 1)), '2026-02-28');
});

test('what the quick-add bar says it understood', () => {
  const now = at('2026-11-18T10:30');
  assert.equal(describeParsed(parseQuick('milk', now), now), '');
  assert.equal(describeParsed(parseQuick('milk every monday 9am !', now), now), 'Monday, 9:00 AM · Weekly · Flagged');
});
