// Tests for ui/dates.js: the date arithmetic and the grid layouts the views draw from.
import { test, assert } from '../../../ui/kit/testing.js';
import {
  ymd, parseYMD, addDays, addMonths, startOfWeek, monthGrid, viewRange, step, onDay, inAllDayRow, layoutDay, layoutAllDay,
  agendaDays, dayLabel, timeLabel, wallIn, hhmm, alertLabel, repeatLabel, daysBetween,
} from '../ui/dates.js';

const D = (s) => parseYMD(s);
const at = (s) => { const [d, t] = s.split('T'); const [y, m, dd] = d.split('-').map(Number); const [h, mi] = t.split(':').map(Number); return new Date(y, m - 1, dd, h, mi); };
const timed = (id, s, e) => ({ id, all_day: false, start: at(s).toISOString(), end: at(e).toISOString() });
const allDay = (id, s, e) => ({ id, all_day: true, start: s, end: e });

test('dates and months', () => {
  assert.equal(ymd(D('2026-11-22')), '2026-11-22');
  assert.equal(parseYMD('2026-02-30'), null);
  assert.equal(parseYMD('nope'), null);
  assert.equal(ymd(addDays(D('2026-12-31'), 1)), '2027-01-01');
  assert.equal(ymd(addMonths(D('2026-01-31'), 1)), '2026-02-28');
  assert.equal(ymd(addMonths(D('2028-01-31'), 1)), '2028-02-29');
  assert.equal(ymd(startOfWeek(D('2026-11-22'), 1)), '2026-11-16');
  assert.equal(ymd(startOfWeek(D('2026-11-22'), 0)), '2026-11-22');
  assert.equal(daysBetween(D('2026-03-01'), D('2026-04-01')), 31, 'across a DST change');
});

test('month grid is whole weeks around the month', () => {
  const g = monthGrid(2026, 10, 1); // November 2026
  assert.equal(g.length % 7, 0);
  assert.equal(ymd(g[0]), '2026-10-26');
  assert.equal(ymd(g[g.length - 1]), '2026-12-06');
  assert.equal(monthGrid(2027, 1, 1).length, 28, 'Feb 2027 starts on a Monday and fills four weeks');
});

test('view ranges and steps', () => {
  const d = D('2026-11-22');
  assert.equal(ymd(viewRange('week', d).from), '2026-11-16');
  assert.equal(ymd(viewRange('week', d).to), '2026-11-23');
  assert.equal(ymd(viewRange('day', d).to), '2026-11-23');
  assert.equal(ymd(viewRange('month', d).from), '2026-10-26');
  assert.equal(ymd(step('month', D('2026-01-31'), 1)), '2026-02-01');
  assert.equal(ymd(step('week', d, -1)), '2026-11-15');
});

test('which days an occurrence touches', () => {
  const trip = allDay('t', '2026-11-20', '2026-11-23');
  assert.ok(onDay(trip, D('2026-11-20')) && onDay(trip, D('2026-11-22')));
  assert.ok(!onDay(trip, D('2026-11-23')), 'all-day end is exclusive');
  const late = timed('l', '2026-11-20T22:00', '2026-11-21T01:00');
  assert.ok(onDay(late, D('2026-11-20')) && onDay(late, D('2026-11-21')));
  const toMidnight = timed('m', '2026-11-20T22:00', '2026-11-21T00:00');
  assert.ok(!onDay(toMidnight, D('2026-11-21')), 'ending at midnight does not touch the next day');
  assert.ok(inAllDayRow(trip) && !inAllDayRow(late) && inAllDayRow(timed('x', '2026-11-20T09:00', '2026-11-21T09:00')));
});

test('day layout splits overlapping events and widens into free columns', () => {
  const day = D('2026-11-20');
  const l = layoutDay([
    timed('a', '2026-11-20T09:00', '2026-11-20T10:00'),
    timed('b', '2026-11-20T09:30', '2026-11-20T11:00'),
    timed('c', '2026-11-20T10:00', '2026-11-20T10:30'),
    timed('d', '2026-11-20T14:00', '2026-11-20T15:00'),
  ], day);
  const by = Object.fromEntries(l.map((x) => [x.o.id, x]));
  assert.equal(by.a.top, 540);
  assert.equal(by.a.height, 60);
  assert.equal(by.a.width, 0.5);
  assert.equal(by.b.left, 0.5);
  assert.equal(by.c.left, 0, 'c takes the column a freed');
  assert.equal(by.d.width, 1, 'a lone event is full width');
  const short = layoutDay([timed('s', '2026-11-20T09:00', '2026-11-20T09:05')], day)[0];
  assert.equal(short.height, 20, 'short events stay tappable');
  const night = layoutDay([timed('n', '2026-11-19T23:00', '2026-11-20T02:00')], day)[0];
  assert.equal(night.top, 0);
  assert.equal(night.height, 120, 'clipped to the day');
});

test('all-day row lanes', () => {
  const from = D('2026-11-16');
  const r = layoutAllDay([allDay('a', '2026-11-15', '2026-11-18'), allDay('b', '2026-11-17', '2026-11-19'), allDay('c', '2026-11-20', '2026-11-30')], from, 7);
  const by = Object.fromEntries(r.items.map((x) => [x.o.id, x]));
  assert.deepEqual([by.a.c0, by.a.c1, by.a.lane], [1, 3, 1]);
  assert.deepEqual([by.b.c0, by.b.c1, by.b.lane], [2, 4, 2]);
  assert.deepEqual([by.c.c0, by.c.c1, by.c.lane], [5, 8, 1]);
  assert.equal(r.lanes, 2);
});

test('agenda groups by day', () => {
  const g = agendaDays([allDay('a', '2026-11-20', '2026-11-22'), timed('b', '2026-11-21T09:00', '2026-11-21T10:00')], D('2026-11-19'), D('2026-11-25'));
  assert.deepEqual(g.map((x) => [ymd(x.day), x.items.map((o) => o.id).join('')]), [['2026-11-20', 'a'], ['2026-11-21', 'ab']]);
});

test('words', () => {
  const today = D('2026-11-22');
  assert.equal(dayLabel(today, today), 'Today');
  assert.equal(dayLabel(D('2026-11-23'), today), 'Tomorrow');
  assert.equal(timeLabel(allDay('a', '2026-11-22', '2026-11-23')), 'all-day');
  assert.ok(timeLabel(timed('l', '2026-11-20T22:00', '2026-11-21T01:00'), D('2026-11-21')).startsWith('until'));
  assert.equal(alertLabel(10), '10 minutes before');
  assert.equal(alertLabel(180), '3 hours before');
  assert.equal(repeatLabel('weekdays'), 'Every Weekday');
  assert.equal(repeatLabel('custom'), 'Custom');
  assert.equal(hhmm(9 * 60 + 47, 15), '09:45');
});

test('wall time in a zone', () => {
  const t = new Date(Date.UTC(2026, 10, 24, 17, 30));
  assert.deepEqual(wallIn(t, 'UTC'), { date: '2026-11-24', time: '17:30' });
  assert.deepEqual(wallIn(t, 'Asia/Tokyo'), { date: '2026-11-25', time: '02:30' });
  assert.deepEqual(wallIn(new Date(Date.UTC(2026, 6, 1, 16, 0)), 'America/Los_Angeles'), { date: '2026-07-01', time: '09:00' });
});
