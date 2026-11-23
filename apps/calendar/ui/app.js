// Calendar UI. It only calls actions (sol.call) and listens to events; calendars and events live
// in feeds the app folds. A view asks calendar.snapshot for its range in the browser's zone and
// paints at once from the last answer it has (cached per range and in localStorage), then
// refreshes when an event says a feed changed: no polling. Every API value reaches the DOM through
// html`` (escaped); there are no inline styles (CSP): positions are data-var-* custom properties,
// colours are data-color attributes app.css maps to tokens.
import { call, events, people as listPeople, html, raw, render } from '/sol.js';
import { toast, showMenu } from '/kit/kit.js';
import {
  ymd, parseYMD, startOfDay, addDays, sameDay, startOfWeek, monthGrid, viewRange, step, occTimes, onDay, inAllDayRow,
  minutesOf, layoutDay, layoutAllDay, agendaDays, fmtTime, fmtHour, fmtWeekday, fmtDayLong, fmtDayShort, viewTitle, dayLabel, timeLabel,
  ALERTS, ALERTS_ALL_DAY, REPEATS, repeatLabel, alertLabel, deviceZone, wallIn, zoneList, hhmm, daysBetween,
} from '/dates.js';

const $ = (id) => document.getElementById(id);
const $app = $('app');
const phoneQ = matchMedia('(max-width: 760px)');
const WEEK_START = 1;
const ZONE = deviceZone();
const store = {
  get(k, d) { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* optional */ } },
};
const COLORS = ['red', 'orange', 'yellow', 'green', 'teal', 'blue', 'indigo', 'purple', 'pink', 'brown', 'gray'];
const VIEWS = ['day', 'week', 'month', 'list'];

const state = {
  view: store.get('cal-view', phoneQ.matches ? 'list' : 'week'),
  day: startOfDay(new Date()),
  calendars: [], invitations: [], invitationsShown: true, occ: [], rev: -1, key: '',
};
if (!VIEWS.includes(state.view)) state.view = 'week';
const cache = new Map(); // range key → snapshot

const fail = (e) => toast(e?.message || String(e), { kind: 'error' });
const plural = (n, w) => `${n} ${w}${n === 1 ? '' : 's'}`;
const calOf = (id) => state.calendars.find((c) => c.id === id);
const writable = () => state.calendars.filter((c) => c.mode !== 'read');
const occOf = (id) => state.occ.find((o) => o.id === id) || state.invitations.find((o) => o.id === id);
const today = () => startOfDay(new Date());

// ---------- data ----------

const rangeKey = (r) => ymd(r.from) + '|' + ymd(r.to);

function adopt(snap) {
  state.calendars = snap.calendars || [];
  state.invitations = snap.invitations || [];
  state.invitationsShown = snap.invitations_shown !== false;
  state.occ = snap.occurrences || [];
  state.rev = snap.rev;
}

let asking = null, again = false;
/** Reads the snapshot of the current view's range; calls that arrive meanwhile share one more read. */
async function refresh() {
  if (asking) { again = true; return asking; }
  asking = (async () => {
    const r = viewRange(state.view, state.day, WEEK_START);
    const key = rangeKey(r);
    try {
      const snap = await call('calendar.snapshot', { from: ymd(r.from), to: ymd(r.to), zone: ZONE });
      cache.set(key, snap);
      if (cache.size > 12) cache.delete(cache.keys().next().value);
      if (key === rangeKey(viewRange(state.view, state.day, WEEK_START)) && (!state.live || snap.rev !== state.rev || key !== state.key)) {
        adopt(snap); state.key = key; state.live = true; draw();
        store.set('cal-cache', JSON.stringify({ key, snap }));
      }
    } catch (e) { fail(e); }
  })();
  await asking;
  asking = null;
  if (again) { again = false; await refresh(); }
}

/** Paints the current range from what is cached at once, then asks for fresh data. */
function show() {
  const key = rangeKey(viewRange(state.view, state.day, WEEK_START));
  const c = cache.get(key);
  if (c) { adopt(c); state.key = key; } else { state.occ = []; state.key = ''; }
  draw();
  refresh();
}

/** Runs a write action, then refreshes (dropping other cached ranges, which it may have changed). */
async function act(name, args) {
  try {
    const r = await call(name, args);
    cache.clear(); state.rev = -1;
    await refresh();
    return r;
  } catch (e) { fail(e); cache.clear(); state.rev = -1; await refresh(); return null; }
}

// ---------- drawing ----------

function draw() {
  $app.dataset.view = state.view;
  // Phones have room for a short title only: "Nov 2026", or the day.
  $('title').textContent = !phoneQ.matches ? viewTitle(state.view, state.day, WEEK_START)
    : state.view === 'day' ? fmtDayShort(state.day) : new Intl.DateTimeFormat(undefined, { month: 'short', year: 'numeric' }).format(state.day);
  for (const b of $('seg').querySelectorAll('button')) b.setAttribute('aria-pressed', String(b.dataset.view === state.view));
  $('today').setAttribute('aria-current', String(sameDay(state.day, today())));
  drawSide();
  if (state.view === 'month') drawMonth();
  else if (state.view === 'list') drawList();
  else drawGrid(state.view === 'day' ? 1 : 7);
}

const visibleOcc = () => state.occ;

function calRow(c, group) {
  return html`<div class="crow" data-item data-cal="${c.id}" data-color="${c.color}">
    <button class="cchk" type="button" role="checkbox" aria-checked="${String(c.shown !== false)}" data-toggle="${c.id}" aria-label="Show ${c.name}"><sol-icon name="check"></sol-icon></button>
    <span class="cname">${c.name}</span>
    ${group === 'shared' && c.owner_name ? html`<span class="cmeta">${c.owner_name}</span>` : ''}
    ${c.shared && c.mode === 'owner' ? html`<span class="cmeta" title="Shared"><sol-icon name="people"></sol-icon></span>` : ''}
    ${c.id === 'invitations' ? '' : html`<button class="ghost icon-btn cmore" type="button" data-calmenu="${c.id}" aria-label="${c.name}: more"><sol-icon name="more"></sol-icon></button>`}
  </div>`;
}

function calendarsView() {
  const mine = state.calendars.filter((c) => c.mode === 'owner');
  const shared = state.calendars.filter((c) => c.mode !== 'owner');
  const inv = state.invitations.length ? [{ id: 'invitations', name: 'Invitations', color: 'gray', shown: state.invitationsShown, mode: 'invite' }] : [];
  return html`
    <h2 class="group-title">My Calendars</h2>
    <div class="list cals">${mine.length ? mine.map((c) => calRow(c, 'mine')) : html`<div class="crow muted">No calendars yet</div>`}</div>
    ${shared.length ? html`<h2 class="group-title">Shared with Me</h2><div class="list cals">${shared.map((c) => calRow(c, 'shared'))}</div>` : ''}
    ${inv.length ? html`<h2 class="group-title">Invitations</h2><div class="list cals">${inv.map((c) => calRow(c, 'inv'))}</div>` : ''}`;
}

function miniView() {
  const d = state.day;
  const grid = monthGrid(d.getFullYear(), d.getMonth(), WEEK_START);
  const t = today();
  const busy = new Set();
  for (const o of state.occ) for (const g of grid) if (onDay(o, g)) busy.add(ymd(g));
  return html`<div class="mini-head"><span>${new Intl.DateTimeFormat(undefined, { month: 'long', year: 'numeric' }).format(d)}</span>
    <span><button class="ghost icon-btn" type="button" data-mini="-1" aria-label="Previous month"><sol-icon name="back"></sol-icon></button><button class="ghost icon-btn" type="button" data-mini="1" aria-label="Next month"><sol-icon name="chevron"></sol-icon></button></span></div>
    <div class="mini-grid">${grid.slice(0, 7).map((g) => html`<span class="mini-wd">${fmtWeekday(g, 'narrow')}</span>`)}
    ${grid.map((g) => html`<button type="button" class="mini-d" data-goto="${ymd(g)}" ${g.getMonth() !== d.getMonth() ? raw('data-out') : ''} ${sameDay(g, t) ? raw('data-today') : ''} ${sameDay(g, d) ? raw('aria-selected="true"') : ''} ${busy.has(ymd(g)) ? raw('data-busy') : ''}>${g.getDate()}</button>`)}</div>`;
}

function drawSide() {
  render($('cals'), calendarsView());
  render($('mini'), miniView());
}

/** The opening tag of an event button; vars become data-var-* (custom properties, see sol.js render). */
function evChip(o, extra = '', vars = {}) {
  const v = Object.entries(vars).map(([k, x]) => html` data-var-${raw(k)}="${x}"`);
  return html`<button type="button" class="ev ${raw(extra)}" data-occ="${o.id}" data-color="${o.color}"${v} ${o.status === 'invited' || o.status === 'tentative' ? raw('data-pending') : ''}>`;
}

// Month: up to three events a day and "n more" (desktop); dots and the chosen day's agenda (phone).
function drawMonth() {
  const d = state.day;
  const grid = monthGrid(d.getFullYear(), d.getMonth(), WEEK_START);
  const t = today();
  const occ = visibleOcc();
  const phone = phoneQ.matches;
  const cells = grid.map((g) => {
    const items = occ.filter((o) => onDay(o, g)).sort((a, b) => (b.all_day - a.all_day) || (occTimes(a).start - occTimes(b).start));
    const shown = items.slice(0, items.length > 3 ? 2 : 3);
    return html`<div class="mc" role="gridcell" data-day="${ymd(g)}" ${g.getMonth() !== d.getMonth() ? raw('data-out') : ''} ${sameDay(g, t) ? raw('data-today') : ''} ${phone && sameDay(g, d) ? raw('aria-selected="true"') : ''}>
      <span class="dn">${g.getDate()}</span>
      ${phone ? html`<span class="dots">${items.slice(0, 3).map((o) => html`<i data-color="${o.color}"></i>`)}</span>`
    : html`${shown.map((o) => html`${evChip(o, o.all_day ? 'bar' : 'line')}${o.all_day ? '' : html`<span class="t">${fmtTime(occTimes(o).start)}</span>`}<span class="n">${o.title}</span></button>`)}
        ${items.length > shown.length ? html`<button type="button" class="more" data-goday="${ymd(g)}">${items.length - shown.length} more</button>` : ''}`}
    </div>`;
  });
  const weeks = grid.length / 7;
  render($('body'), html`<div class="month" data-var-weeks="${weeks}">
    <div class="mh">${grid.slice(0, 7).map((g) => html`<span>${fmtWeekday(g, phone ? 'narrow' : 'short')}</span>`)}</div>
    <div class="mg" role="grid">${cells}</div>
    ${phone ? html`<div class="mday">${dayAgenda(d)}</div>` : ''}
  </div>`);
}

function agendaRow(o, day) {
  return html`${evChip(o, 'row')}<span class="bar-c"></span><span class="r-main"><span class="r-title">${o.title}</span>${o.location ? html`<span class="r-sub">${o.location}</span>` : ''}</span><span class="r-time">${timeLabel(o, day)}</span></button>`;
}

function dayAgenda(d) {
  const items = visibleOcc().filter((o) => onDay(o, d)).sort((a, b) => (b.all_day - a.all_day) || (occTimes(a).start - occTimes(b).start));
  return html`<h2 class="ag-day">${dayLabel(d, today())}</h2>${items.length ? html`<div class="list ag">${items.map((o) => agendaRow(o, d))}</div>` : html`<p class="muted ag-none">No events</p>`}`;
}

function drawList() {
  const r = viewRange('list', state.day, WEEK_START);
  const days = agendaDays(visibleOcc(), r.from, r.to);
  const t = today();
  render($('body'), html`<div class="agenda">${days.length ? days.map(({ day, items }) => html`<section class="ag-sec" data-day="${ymd(day)}">
      <h2 class="ag-day ${sameDay(day, t) ? 'is-today' : ''}">${dayLabel(day, t)}</h2>
      <div class="list ag">${items.sort((a, b) => (b.all_day - a.all_day) || (occTimes(a).start - occTimes(b).start)).map((o) => agendaRow(o, day))}</div></section>`)
    : html`<div class="empty"><h3>No events</h3><p>Nothing in the next six weeks. Tap + to add one.</p></div>`}</div>`);
}

// Day and week: a time grid with an all-day row, hour lines and a now line.
let scrolledFor = '';
function drawGrid(n) {
  const from = n === 1 ? startOfDay(state.day) : startOfWeek(state.day, WEEK_START);
  const days = Array.from({ length: n }, (_, i) => addDays(from, i));
  const t = today();
  const occ = visibleOcc();
  const top = occ.filter(inAllDayRow);
  const timed = occ.filter((o) => !inAllDayRow(o));
  const ad = layoutAllDay(top, from, n);
  const now = new Date();
  const phone = phoneQ.matches;
  render($('body'), html`<div class="grid" data-var-days="${n}" data-var-lanes="${Math.max(ad.lanes, 1)}">
    <div class="g-head"><span class="g-gut"></span>${days.map((d) => html`<button type="button" class="g-day" data-goday="${ymd(d)}" ${sameDay(d, t) ? raw('data-today') : ''}><span class="wd">${fmtWeekday(d, phone && n > 1 ? 'narrow' : 'short')}</span><span class="dn">${d.getDate()}</span></button>`)}</div>
    <div class="g-allday"><span class="g-gut">all-day</span><div class="g-adg">${days.map((d, i) => html`<span class="g-adc" data-var-c="${i + 1}" data-slot-day="${ymd(d)}"></span>`)}
      ${ad.items.map((x) => html`${evChip(x.o, 'ad', { c0: x.c0, c1: x.c1, lane: x.lane })}<span class="n">${x.o.title}</span></button>`)}</div></div>
    <div class="g-scroll" id="gscroll"><div class="g-body">
      <div class="g-hours">${Array.from({ length: 24 }, (_, h) => html`<span class="g-h" data-var-h="${h}">${h ? fmtHour(h) : ''}</span>`)}</div>
      <div class="g-cols">${days.map((d) => html`<div class="g-col" data-col="${ymd(d)}" ${sameDay(d, t) ? raw('data-today') : ''}>
        ${layoutDay(timed.filter((o) => onDay(o, d)), d).map((x) => html`${evChip(x.o, 'tm' + (x.height < 40 ? ' short' : ''), { t: x.top, len: x.height, l: x.left, w: x.width })}<span class="n">${x.o.title}</span><span class="t">${fmtTime(occTimes(x.o).start)}${x.o.location ? html` · ${x.o.location}` : ''}</span></button>`)}
        ${sameDay(d, t) ? html`<div class="now" data-var-t="${minutesOf(now)}"></div>` : ''}</div>`)}</div>
    </div></div></div>`);
  const sc = $('gscroll');
  const key = state.view + ymd(from);
  if (scrolledFor !== key) {
    scrolledFor = key;
    const hh = parseFloat(getComputedStyle(sc).getPropertyValue('--hh')) || 48;
    const first = timed.filter((o) => days.some((d) => onDay(o, d))).map((o) => minutesOf(occTimes(o).start)).sort((a, b) => a - b)[0];
    const at = Math.min(days.some((d) => sameDay(d, t)) ? minutesOf(now) - 120 : 24 * 60, (first ?? 8 * 60) - 30);
    sc.scrollTop = Math.max(0, Math.min(at, 17 * 60) * hh / 60);
  }
}

// The now line moves once a minute: a clock, not a data poll.
(function tick() {
  const n = new Date();
  setTimeout(() => {
    const line = document.querySelector('.now');
    if (line) line.style.setProperty('--t', String(minutesOf(new Date())));
    if (!sameDay(n, new Date())) draw();
    tick();
  }, 60000 - (n.getSeconds() * 1000 + n.getMilliseconds()) + 50);
})();

// ---------- navigation ----------

function go(day, view = state.view) {
  state.day = startOfDay(day);
  if (view !== state.view) { state.view = view; store.set('cal-view', view); }
  history.replaceState(null, '', `#/${state.view}/${ymd(state.day)}`);
  show();
}

/** Deep links: #/<view>[/<YYYY-MM-DD>[/open/<occurrence id>]]. */
function fromHash() {
  const [, view, day, verb, id] = location.hash.split('/');
  if (VIEWS.includes(view)) state.view = view;
  const d = parseYMD(day);
  if (d) state.day = d;
  return verb === 'open' && id ? decodeURIComponent(id) : verb === 'new' ? 'new' : '';
}

$('prev').addEventListener('click', () => go(step(state.view, state.day, -1)));
$('next').addEventListener('click', () => go(step(state.view, state.day, 1)));
$('today').addEventListener('click', () => go(today()));
$('seg').addEventListener('click', (e) => { const b = e.target.closest('[data-view]'); if (b) go(state.day, b.dataset.view); });
$('goto').addEventListener('change', (e) => { const d = parseYMD(e.target.value); if (d) go(d); });
$('goto').closest('label').addEventListener('click', (e) => { if (e.target.id !== 'goto') { e.preventDefault(); const g = $('goto'); g.value = ymd(state.day); try { g.showPicker(); } catch { g.focus(); } } });

$('side').addEventListener('click', onSideClick);
async function onSideClick(e) {
  const g = e.target.closest('[data-goto]');
  if (g) { go(parseYMD(g.dataset.goto)); return; }
  const m = e.target.closest('[data-mini]');
  if (m) { const d = new Date(state.day.getFullYear(), state.day.getMonth() + Number(m.dataset.mini), 1); go(d); return; }
  const tg = e.target.closest('[data-toggle]');
  if (tg) { const shown = tg.getAttribute('aria-checked') !== 'true'; tg.setAttribute('aria-checked', String(shown)); await act('calendar.update_calendar', { calendar: tg.dataset.toggle, shown }); return; }
  const cm = e.target.closest('[data-calmenu]');
  if (cm) { calMenu(cm, calOf(cm.dataset.calmenu)); }
}

$('body').addEventListener('click', (e) => {
  const ev = e.target.closest('[data-occ]');
  if (ev) { openEvent(ev.dataset.occ); return; }
  const gd = e.target.closest('[data-goday]');
  if (gd) { go(parseYMD(gd.dataset.goday), 'day'); return; }
  const mc = e.target.closest('.mc');
  if (mc) {
    const d = parseYMD(mc.dataset.day);
    if (phoneQ.matches) {
      if (sameDay(d, state.day)) go(d, 'day');
      else { const moved = rangeKey(viewRange('month', d, WEEK_START)) !== state.key; state.day = d; if (moved) show(); else draw(); }
      return;
    }
    if (e.detail >= 2) newEvent(atHour(d, 9)); else { state.day = d; drawSide(); }
    return;
  }
  const col = e.target.closest('.g-col');
  if (col) {
    const r = col.getBoundingClientRect();
    const min = Math.floor(((e.clientY - r.top) / r.height) * 24 * 60 / 30) * 30;
    const d = parseYMD(col.dataset.col);
    newEvent(new Date(d.getFullYear(), d.getMonth(), d.getDate(), 0, Math.min(min, 23 * 60 + 30)));
    return;
  }
  const adc = e.target.closest('[data-slot-day]');
  if (adc) newEvent(parseYMD(adc.dataset.slotDay), true);
});

const atHour = (d, h) => new Date(d.getFullYear(), d.getMonth(), d.getDate(), h);

$('add').addEventListener('click', () => {
  const n = new Date();
  const base = sameDay(state.day, today()) ? new Date(n.getFullYear(), n.getMonth(), n.getDate(), n.getHours() + 1) : atHour(state.day, 9);
  newEvent(base);
});
$('cals-btn').addEventListener('click', () => {
  const d = dialog('Calendars', html`<div class="sheet-cals">${calendarsView()}</div>
    <div class="dlg-actions"><button type="button" class="plain" data-sheet="add"><sol-icon name="plus"></sol-icon>Add Calendar</button><button type="button" class="plain" data-sheet="file"><sol-icon name="more"></sol-icon>Import / Export</button></div>`, true);
  d.box.addEventListener('click', async (e) => {
    const s = e.target.closest('[data-sheet]');
    if (s?.dataset.sheet === 'add') { d.close(); addCalendar(); return; }
    if (s?.dataset.sheet === 'file') { d.close(); fileMenu(null); return; }
    await onSideClick(e);
    if (e.target.closest('[data-toggle]')) render(d.box.querySelector('.sheet-cals'), calendarsView());
    if (e.target.closest('[data-calmenu]')) d.close();
  });
});
$('add-cal').addEventListener('click', () => addCalendar());
$('file-menu').addEventListener('click', (e) => fileMenu(e.currentTarget));

document.addEventListener('keydown', (e) => {
  const typing = /INPUT|TEXTAREA|SELECT/.test(document.activeElement?.tagName);
  if (typing || e.metaKey || e.ctrlKey || e.altKey || document.querySelector('dialog[open]')) return;
  const k = { d: 'day', w: 'week', m: 'month', l: 'list' }[e.key];
  if (k) { e.preventDefault(); go(state.day, k); }
  if (e.key === 't') go(today());
  if (e.key === 'ArrowLeft') go(step(state.view, state.day, -1));
  if (e.key === 'ArrowRight') go(step(state.view, state.day, 1));
  if (e.key === 'n') { e.preventDefault(); $('add').click(); }
});
phoneQ.addEventListener('change', () => draw());

// ---------- dialogs ----------

function dialog(heading, content, sheet = false) {
  const el = document.createElement(sheet ? 'sol-sheet' : 'sol-dialog');
  el.setAttribute('heading', heading);
  const box = document.createElement('div');
  box.className = 'dbox';
  el.append(box);
  document.body.append(el);
  render(box, content);
  el.addEventListener('close', () => el.remove());
  el.show();
  return { el, box, close: () => el.close() };
}

/** Asks for one of several answers; resolves to its value, or null when dismissed. */
function choose(title, body, options) {
  return new Promise((resolve) => {
    let done = false;
    const d = dialog(title, html`${body ? html`<p>${body}</p>` : ''}<div class="dlg-actions stack">${options.map(([v, label, kind]) => html`<button type="button" class="${kind || ''}" data-v="${v}">${label}</button>`)}<button type="button" data-v="">Cancel</button></div>`);
    const finish = (v) => { if (!done) { done = true; resolve(v); } d.close(); };
    d.box.addEventListener('click', (e) => { const b = e.target.closest('[data-v]'); if (b) finish(b.dataset.v || null); });
    d.el.addEventListener('close', () => finish(null));
  });
}

function download(name, text) {
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([text], { type: 'text/calendar' }));
  a.download = name;
  document.body.append(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 4000);
}

function pickFile(accept, onText) {
  const input = document.createElement('input');
  input.type = 'file'; input.accept = accept;
  input.addEventListener('change', async () => { const f = input.files[0]; if (f) onText(await f.text(), f.name); });
  input.click();
}

const ICS = '.ics,text/calendar';
async function exportICS(cal) {
  try {
    const r = await call('calendar.export', cal ? { calendar: cal.id } : {});
    download(r.filename, r.ics);
    toast('Exported ' + plural(r.events, 'event'));
  } catch (e) { fail(e); }
}
function importICS(cal) {
  pickFile(ICS, async (text, name) => {
    const args = { ics: text };
    if (cal) args.calendar = cal.id;
    else if (!/X-SOL-CALENDAR/.test(text) && !/X-WR-CALNAME/.test(text)) args.name = name.replace(/\.ics$/i, '');
    const r = await act('calendar.import', args);
    if (r) toast(`Imported ${plural(r.added, 'new event')}${r.updated ? `, ${r.updated} updated` : ''}${r.skipped.length ? `, ${r.skipped.length} skipped` : ''}`);
  });
}

function fileMenu(anchor) {
  const items = [
    { label: 'Import .ics…', icon: 'upload', onSelect: () => importICS(null) },
    { label: 'Export all calendars', icon: 'download', onSelect: () => exportICS(null) },
  ];
  if (anchor) showMenu(items, { anchor });
  else choose('Import or export', 'iCalendar (.ics) files work with every calendar app.', [['import', 'Import .ics…'], ['export', 'Export all calendars']]).then((v) => { if (v === 'import') importICS(null); if (v === 'export') exportICS(null); });
}

function calMenu(anchor, c) {
  if (!c) return;
  const owner = c.mode === 'owner';
  const items = [
    c.mode !== 'read' && { label: 'Edit calendar', icon: 'edit', onSelect: () => editCalendar(c) },
    owner && { label: 'Share calendar', icon: 'people', onSelect: () => shareDialog(c) },
    c.mode !== 'read' && { label: 'Import .ics into it…', icon: 'upload', onSelect: () => importICS(c) },
    { label: 'Export .ics', icon: 'download', onSelect: () => exportICS(c) },
    { separator: true },
    { label: owner ? 'Delete calendar' : 'Leave calendar', icon: 'trash', danger: true, onSelect: () => deleteCalendar(c) },
  ].filter(Boolean);
  if (phoneQ.matches) {
    choose(c.name, '', items.filter((i) => !i.separator).map((i, n) => [String(n), i.label, i.danger ? 'danger' : ''])).then((v) => { if (v !== null) items.filter((i) => !i.separator)[Number(v)].onSelect(); });
  } else showMenu(items, { anchor });
}

function calendarForm(c) {
  const color = c?.color || 'blue';
  return html`<form class="form" autocomplete="off">
    <label class="field"><span>Name</span><input name="name" required maxlength="200" value="${c?.name || ''}" placeholder="Calendar name"></label>
    <div class="swatches" role="radiogroup" aria-label="Color">${COLORS.map((k) => html`<label class="sw" data-color="${k}"><input type="radio" name="color" value="${k}" ${k === color ? raw('checked') : ''} aria-label="${k}"><span></span></label>`)}</div>
    <div class="dlg-actions"><button type="button" data-cancel>Cancel</button><button class="primary" type="submit">${c ? 'Save' : 'Add'}</button></div></form>`;
}

function calendarDialog(c) {
  return new Promise((resolve) => {
    const d = dialog(c ? 'Edit Calendar' : 'New Calendar', calendarForm(c));
    const f = d.box.querySelector('form');
    setTimeout(() => f.elements.name.focus(), 50);
    let done = false;
    const finish = (v) => { if (!done) { done = true; resolve(v); } d.close(); };
    f.addEventListener('submit', (e) => { e.preventDefault(); const name = f.elements.name.value.trim(); if (name) finish({ name, color: f.elements.color.value }); });
    d.box.querySelector('[data-cancel]').addEventListener('click', () => finish(null));
    d.el.addEventListener('close', () => finish(null));
  });
}

async function addCalendar() {
  const v = await calendarDialog(null);
  if (v) { const r = await act('calendar.create_calendar', v); if (r) toast('Added ' + r.calendar.name); }
}
async function editCalendar(c) {
  const v = await calendarDialog(c);
  if (v) await act('calendar.update_calendar', { calendar: c.id, ...v });
}
async function deleteCalendar(c) {
  const owner = c.mode === 'owner';
  const v = await choose(owner ? `Delete “${c.name}”?` : `Leave “${c.name}”?`,
    owner ? `Its ${plural(c.events, 'event')} are deleted on every device${c.shared ? ' and for everyone it is shared with' : ''}.` : 'The owner keeps it; you can be invited again.',
    [['yes', owner ? 'Delete Calendar' : 'Leave Calendar', 'danger primary']]);
  if (v) { const r = await act('calendar.delete_calendar', { calendar: c.id }); if (r) toast((owner ? 'Deleted ' : 'Left ') + c.name); }
}

async function shareDialog(c) {
  let ppl = [];
  try { ppl = await listPeople(); } catch (e) { fail(e); return; }
  const mode = (p) => (c.members || []).find((m) => m.person === p.id)?.mode || 'none';
  const body = ppl.length ? html`<p class="muted">People you share this calendar with see its events. “Can edit” lets them add and change events.</p>
    <div class="list">${ppl.map((p) => html`<div class="prow" data-item><sol-avatar name="${p.petname || p.name}"></sol-avatar><span class="pname">${p.petname || p.name}</span>
      <select data-share="${p.id}" aria-label="Access for ${p.petname || p.name}"><option value="none" ${mode(p) === 'none' ? raw('selected') : ''}>Not shared</option><option value="read" ${mode(p) === 'read' ? raw('selected') : ''}>Can view</option><option value="write" ${mode(p) === 'write' ? raw('selected') : ''}>Can edit</option></select></div>`)}</div>`
    : html`<div class="empty"><sol-icon name="people"></sol-icon><h3>No one to share with yet</h3><p>Add a person in Sol, then share calendars with them here.</p></div>`;
  const d = dialog('Share “' + c.name + '”', body);
  d.box.addEventListener('change', async (e) => {
    const s = e.target.closest('[data-share]');
    if (!s) return;
    const r = await act('calendar.share', { calendar: c.id, person: s.dataset.share, mode: s.value });
    if (r) { toast(s.value === 'none' ? 'Stopped sharing' : 'Shared'); c = calOf(c.id) || c; }
  });
}

// ---------- the event sheet ----------

function newEvent(at, allDay = false) {
  const cals = writable().filter((c) => c.shown !== false);
  const cal = cals[0] || writable()[0];
  const end = allDay ? addDays(at, 1) : new Date(+at + 3600e3);
  const o = { id: '', title: '', all_day: allDay, start: allDay ? ymd(at) : at.toISOString(), end: allDay ? ymd(end) : end.toISOString(), tz: ZONE, calendar: cal?.id || '', alerts: [], repeat: '', location: '', notes: '', mode: cal ? cal.mode : 'owner' };
  eventSheet(o, true);
}

async function openEvent(id) {
  const o = occOf(id);
  if (o) eventSheet(o, false);
}

function opts(list, cur) {
  return list.map(([v, label]) => html`<option value="${v}" ${String(v) === String(cur) ? raw('selected') : ''}>${label}</option>`);
}

function eventForm(o, isNew) {
  const ro = o.mode === 'read' || o.mode === 'invite';
  const zone = o.tz || ZONE;
  const { start, end } = occTimes(o);
  const s = o.all_day ? { date: o.start, time: '09:00' } : wallIn(start, zone);
  const lastDay = o.all_day ? ymd(addDays(end, -1)) : '';
  const e = o.all_day ? { date: lastDay, time: '10:00' } : wallIn(end, zone);
  const alerts = o.all_day ? ALERTS_ALL_DAY : ALERTS;
  const a1 = o.alerts[0] ?? '', a2 = o.alerts[1] ?? '';
  const alertOpts = (cur) => [['', 'None'], ...alerts, ...(cur !== '' && !alerts.some(([v]) => v === cur) ? [[cur, alertLabel(cur, o.all_day)]] : [])];
  const repeats = o.repeat === 'custom' ? [...REPEATS, ['custom', 'Custom (' + (o.rrule || '') + ')']] : REPEATS;
  const cals = writable();
  if (ro) {
    const { start: st, end: en } = occTimes(o);
    const when = o.all_day ? (daysBetween(st, en) > 1 ? `${fmtDayLong(st)} – ${fmtDayLong(addDays(en, -1))}` : fmtDayLong(st)) : `${fmtDayLong(st)}, ${fmtTime(st)} – ${fmtTime(en)}`;
    return html`<div class="ev-ro" data-color="${o.color}">
      <h3 class="ro-title">${o.title}</h3>
      <p class="ro-when">${when}${o.recurring ? html`<br><span class="muted">${repeatLabel(o.repeat)}</span>` : ''}</p>
      ${o.location ? html`<p class="ro-line"><sol-icon name="pin"></sol-icon>${o.location}</p>` : ''}
      <p class="ro-line"><span class="dot" data-color="${o.color}"></span>${o.calendar_name}${o.from_name ? html` · from ${o.from_name}` : ''}</p>
      ${o.alerts.length ? html`<p class="ro-line"><sol-icon name="bell"></sol-icon>${o.alerts.map((m) => alertLabel(m, o.all_day)).join(', ')}</p>` : ''}
      ${o.notes ? html`<p class="ro-notes">${o.notes}</p>` : ''}
      ${o.mode === 'invite' ? html`<div class="rsvp segmented" role="group" aria-label="Your answer">${[['accepted', 'Accept'], ['tentative', 'Maybe'], ['declined', 'Decline']].map(([v, l]) => html`<button type="button" data-rsvp="${v}" aria-pressed="${String(o.status === v)}">${l}</button>`)}</div>` : html`<p class="muted">This calendar is shared with you read-only.</p>`}
    </div>`;
  }
  return html`<form class="form ev-form" autocomplete="off" data-allday="${String(!!o.all_day)}">
    <div class="list grp">
      <input class="big" name="title" placeholder="Title" aria-label="Title" maxlength="1000" value="${o.title}" required>
      <input name="location" placeholder="Location" aria-label="Location" value="${o.location || ''}">
    </div>
    <div class="list grp">
      <label class="frow"><span>All-day</span><input type="checkbox" role="switch" name="all_day" ${o.all_day ? raw('checked') : ''}></label>
      <div class="frow"><span>Starts</span><span class="dt"><input type="date" name="sd" value="${s.date}" required aria-label="Start date"><input type="time" name="st" value="${s.time}" class="timed" aria-label="Start time" step="300"></span></div>
      <div class="frow"><span>Ends</span><span class="dt"><input type="date" name="ed" value="${e.date}" required aria-label="End date"><input type="time" name="et" value="${e.time}" class="timed" aria-label="End time" step="300"></span></div>
      <label class="frow timed"><span>Time Zone</span><select name="zone">${opts(zoneList(zone, ZONE).map((z) => [z, z.replace(/_/g, ' ')]), zone)}</select></label>
    </div>
    <div class="list grp">
      <label class="frow"><span>Repeat</span><select name="repeat">${opts(repeats, o.repeat || '')}</select></label>
      <label class="frow"><span>Calendar</span><select name="calendar">${opts(cals.map((c) => [c.id, c.name]), o.calendar)}</select></label>
    </div>
    <div class="list grp">
      <label class="frow"><span>Alert</span><select name="a1">${opts(alertOpts(a1), a1)}</select></label>
      <label class="frow"><span>Second Alert</span><select name="a2">${opts(alertOpts(a2), a2)}</select></label>
    </div>
    <div class="list grp"><textarea name="notes" rows="3" placeholder="Notes" aria-label="Notes" maxlength="20000">${o.notes || ''}</textarea></div>
    ${o.attendees?.length ? html`<div class="list grp"><div class="frow"><span>Invitees</span><span class="muted">${o.attendees.map((a) => `${a.name || 'Someone'} (${a.status})`).join(', ')}</span></div></div>` : ''}
    <div class="dlg-actions">${isNew ? '' : html`<button type="button" class="danger" data-delete>Delete Event</button>`}<span class="grow"></span><button type="button" data-cancel>Cancel</button><button class="primary" type="submit">${isNew ? 'Add' : 'Done'}</button></div>
  </form>`;
}

/** The arguments the form says, for calendar.add or calendar.update. */
function formArgs(f) {
  const allDay = f.elements.all_day.checked;
  const sd = f.elements.sd.value, ed = f.elements.ed.value || sd;
  const a = {
    title: f.elements.title.value.trim(), location: f.elements.location.value.trim(), notes: f.elements.notes.value,
    all_day: allDay, alerts: [f.elements.a1.value, f.elements.a2.value].filter((v) => v !== '').map(Number),
  };
  if (allDay) {
    a.start = sd;
    const end = parseYMD(ed) && parseYMD(ed) >= parseYMD(sd) ? parseYMD(ed) : parseYMD(sd);
    a.end = ymd(addDays(end, 1));
  } else {
    a.zone = f.elements.zone.value;
    a.start = `${sd}T${f.elements.st.value || '09:00'}`;
    a.end = `${ed}T${f.elements.et.value || f.elements.st.value || '10:00'}`;
  }
  if (f.elements.repeat.value !== 'custom') a.repeat = f.elements.repeat.value;
  return a;
}

function eventSheet(o, isNew) {
  const heading = isNew ? 'New Event' : o.mode === 'invite' ? 'Invitation' : o.mode === 'read' ? 'Event' : 'Edit Event';
  const d = dialog(heading, eventForm(o, isNew), true);
  d.el.classList.add('ev-sheet');
  const f = d.box.querySelector('form');
  d.box.addEventListener('click', async (e) => {
    const r = e.target.closest('[data-rsvp]');
    if (r) { d.close(); const res = await act('calendar.respond', { invite: o.invite, response: r.dataset.rsvp }); if (res) toast(res.status === 'accepted' ? 'Accepted' : res.status === 'declined' ? 'Declined' : 'Answered maybe'); }
  });
  if (!f) return;
  if (isNew) setTimeout(() => f.elements.title.focus(), 60);
  const syncAllDay = () => { f.dataset.allday = String(f.elements.all_day.checked); };
  f.elements.all_day.addEventListener('change', syncAllDay);
  // Moving the start keeps the length, like every calendar.
  let lastStart = { d: f.elements.sd.value, t: f.elements.st.value };
  const shift = () => {
    const was = new Date(`${lastStart.d}T${lastStart.t || '00:00'}`), now = new Date(`${f.elements.sd.value}T${f.elements.st.value || '00:00'}`);
    const end = new Date(`${f.elements.ed.value}T${f.elements.et.value || '00:00'}`);
    if (!isNaN(was) && !isNaN(now) && !isNaN(end)) {
      const ne = new Date(+end + (now - was));
      f.elements.ed.value = ymd(ne); f.elements.et.value = hhmm(minutesOf(ne));
    }
    lastStart = { d: f.elements.sd.value, t: f.elements.st.value };
  };
  f.elements.sd.addEventListener('change', shift);
  f.elements.st.addEventListener('change', shift);
  d.box.querySelector('[data-cancel]').addEventListener('click', () => d.close());
  f.addEventListener('submit', async (e) => {
    e.preventDefault();
    const a = formArgs(f);
    if (!a.title) { f.elements.title.focus(); return; }
    const cal = f.elements.calendar.value;
    if (isNew) {
      d.close();
      const r = await act('calendar.add', { ...a, calendar: cal });
      if (r) toast('Added ' + r.event.title);
      return;
    }
    let span = '';
    if (o.recurring) {
      span = await choose('This is a repeating event', 'Change only this event, or this and every later one, or all of them?', [['this', 'Save for This Event Only', 'primary'], ['following', 'Save for Future Events'], ['all', 'Save for All Events']]);
      if (!span) return;
    }
    d.close();
    const id = o.recurring ? o.id : o.event;
    if (cal !== o.calendar) {
      if (o.recurring && span !== 'all') { toast('Only all events of a repeating event can move to another calendar', { kind: 'error' }); return; }
      if (!(await act('calendar.move', { id: o.event, calendar: cal, span: 'all' }))) return;
    }
    await act('calendar.update', { ...a, id, span });
  });
  d.box.querySelector('[data-delete]')?.addEventListener('click', async () => {
    let span = 'all';
    if (o.recurring) {
      span = await choose('Delete a repeating event', '', [['this', 'Delete This Event Only', 'danger primary'], ['following', 'Delete All Future Events', 'danger'], ['all', 'Delete All Events', 'danger']]);
      if (!span) return;
    } else if (!(await choose(`Delete “${o.title}”?`, 'It is deleted on every device.', [['yes', 'Delete Event', 'danger primary']]))) return;
    d.close();
    const r = await act('calendar.delete', { id: o.recurring ? o.id : o.event, span });
    if (r) toast('Deleted');
  });
}

// ---------- start ----------

try {
  const c = JSON.parse(store.get('cal-cache', 'null'));
  if (c && c.snap) cache.set(c.key, c.snap);
} catch { /* optional */ }
const opening = fromHash();
show();

// Pushed changes: any feed entry or feed change means another read of the snapshot.
let evT = 0;
const onChange = () => { clearTimeout(evT); evT = setTimeout(() => { cache.clear(); state.rev = -1; refresh(); }, 60); };
events({}, (ev) => { if (ev.type === 'entry' || ev.type === 'feed') onChange(); }, () => { /* reconnects by itself */ });

/** Opens what the deep link names, as soon as it is known (from the cache, or after the first read). */
function openLinked() {
  if (opening === 'new') { $('add').click(); return true; }
  if (opening && occOf(opening)) { openEvent(opening); return true; }
  return !opening;
}
const linked = openLinked();
await refresh();
if (!linked) openLinked();
