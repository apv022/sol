// Tasks UI. It only calls actions (sol.call) and listens to events; the lists and tasks live in
// feeds that the app folds. State is one snapshot (tasks.snapshot), painted from a cached copy
// at once and refreshed when an event says a feed changed: no polling. Every API value reaches
// the DOM through html`` (escaped); there are no inline styles (CSP), colours are data-color
// attributes that app.css maps to custom properties.
import { call, events, people as listPeople, html, raw, render } from '/sol.js';
import { toast, showMenu, contextMenu } from '/kit/kit.js';
import { parseQuick, describeParsed, formatDue, formatDay, formatLong, formatRepeat, ymd } from '/dates.js';

const $ = (id) => document.getElementById(id);
const $app = $('app');
const phoneQ = matchMedia('(max-width: 760px)');
const panelQ = matchMedia('(min-width: 1060px)');
const store = {
  get(k, d) { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* optional */ } },
};

const SMART = [
  { id: 'today', name: 'Today', color: 'blue', icon: 'cal' },
  { id: 'scheduled', name: 'Scheduled', color: 'red', icon: 'clock' },
  { id: 'all', name: 'All', color: 'gray', icon: 'tray' },
  { id: 'flagged', name: 'Flagged', color: 'orange', icon: 'flag' },
  { id: 'completed', name: 'Completed', color: 'gray', icon: 'check' },
];
const COLORS = ['red', 'orange', 'yellow', 'green', 'teal', 'blue', 'indigo', 'purple', 'pink', 'brown', 'gray'];
const ICONS = ['list', 'home', 'star', 'bell', 'tag', 'people', 'file', 'folder', 'globe', 'bolt', 'clock', 'image', 'music', 'video', 'send', 'storage', 'lock', 'eye', 'pin', 'check'];
const REPEATS = ['', 'daily', 'weekly', 'monthly', 'yearly'];

const state = {
  rev: -1, today: ymd(new Date()), lists: [], tasks: [],
  route: { kind: 'smart', id: 'today' }, // or {kind:'list', id}, {kind:'search'}
  query: '', sel: null, showDone: false, linger: new Set(),
};

// ---------- icons the kit lacks (same 24px, 1.75 stroke look) ----------

const PATHS = {
  flag: ['M6 20V4.5', 'M6 5h11l-2.2 3.6L17 12H6'],
  cal: ['M5.5 5.5h13a1.5 1.5 0 011.5 1.5v11.5a1.5 1.5 0 01-1.5 1.5h-13A1.5 1.5 0 014 18.5V7a1.5 1.5 0 011.5-1.5z', 'M4 10h16', 'M8.5 3.5v3', 'M15.5 3.5v3'],
  tray: ['M4.5 13l2-7a1.5 1.5 0 011.4-1h8.2a1.5 1.5 0 011.4 1l2 7', 'M4.5 13v4.5A1.5 1.5 0 006 19h12a1.5 1.5 0 001.5-1.5V13h-4.2a3.3 3.3 0 01-6.6 0z'],
  repeat: ['M5 11V9.5A2.5 2.5 0 017.5 7H18', 'M15.5 4.5L18 7l-2.5 2.5', 'M19 13v1.5a2.5 2.5 0 01-2.5 2.5H6', 'M8.5 19.5L6 17l2.5-2.5'],
  info: ['M12 4a8 8 0 100 16 8 8 0 000-16z', 'M12 11v5', 'M12 8h.01'],
  share: ['M12 15V4', 'M8.5 7.5L12 4l3.5 3.5', 'M7 11H6a1.5 1.5 0 00-1.5 1.5v6A1.5 1.5 0 006 20h12a1.5 1.5 0 001.5-1.5v-6A1.5 1.5 0 0018 11h-1'],
};
const svg = (name, size = 18) => raw(`<svg class="ico" viewBox="0 0 24 24" width="${size}" height="${size}" aria-hidden="true">${PATHS[name].map((d) => `<path d="${d}"/>`).join('')}</svg>`);
/** An icon by name: the kit's, or one of ours. */
const icon = (name, size) => (PATHS[name] ? svg(name, size) : html`<sol-icon name="${name}"></sol-icon>`);

const fail = (e) => toast(e?.message || String(e), { kind: 'error' });
const plural = (n, w) => `${n} ${w}${n === 1 ? '' : 's'}`;

// ---------- data ----------

function adopt(snap) {
  state.rev = snap.rev; state.today = snap.today; state.lists = snap.lists; state.tasks = snap.tasks;
  if (state.route.kind === 'list' && !state.lists.some((l) => l.id === state.route.id)) go({ kind: 'smart', id: 'today' }, true);
  if (state.sel && !state.tasks.some((t) => t.id === state.sel)) state.sel = null;
}

let refreshing = null, again = false;
/** Reads the snapshot; calls that arrive meanwhile share one more read afterwards. */
async function refresh() {
  if (refreshing) { again = true; return refreshing; }
  refreshing = (async () => {
    try {
      const snap = await call('tasks.snapshot', {});
      if (snap.rev !== state.rev || snap.today !== state.today) {
        adopt(snap); draw();
        try { localStorage.setItem('tasks-cache', JSON.stringify(snap)); } catch { /* optional */ }
      }
    } catch (e) { fail(e); }
  })();
  await refreshing;
  refreshing = null;
  if (again) { again = false; await refresh(); }
}

/** Runs a write action, then refreshes. */
async function act(name, args) {
  try { const r = await call(name, args); await refresh(); return r; } catch (e) { fail(e); await refresh(); return null; }
}

const listOf = (id) => state.lists.find((l) => l.id === id);
const taskOf = (id) => state.tasks.find((t) => t.id === id);
const canWrite = (list) => list && list.mode !== 'read';
const isOverdue = (t) => !t.done && t.due && formatDue(t.due, t.time, new Date()).overdue;

function inSmart(id, t) {
  switch (id) {
    case 'today': return !t.done && t.due && t.due <= state.today;
    case 'scheduled': return !t.done && !!t.due;
    case 'flagged': return !t.done && t.flag;
    case 'all': return !t.done;
    case 'completed': return t.done;
  }
  return false;
}
const smartCount = (id) => state.tasks.filter((t) => inSmart(id, t)).length;

// ---------- routing ----------

function parseHash() {
  const [, kind, id] = location.hash.split('/');
  if (kind === 'smart' && SMART.some((s) => s.id === id)) return { kind: 'smart', id };
  if (kind === 'list' && id) return { kind: 'list', id: decodeURIComponent(id) };
  return null;
}
function go(route, replace) {
  state.route = route;
  state.query = ''; $('q').value = '';
  const h = '#/' + route.kind + '/' + encodeURIComponent(route.id);
  if (replace) history.replaceState({ in: 1 }, '', h); else if (location.hash !== h) history.pushState({ in: 1 }, '', h);
  $app.dataset.screen = 'list';
  draw();
}
function showLists() {
  if (location.hash !== '') history.replaceState(null, '', location.pathname + location.search);
  $app.dataset.screen = 'lists';
  state.query = ''; $('q').value = '';
  state.sel = null; closeDetails();
  draw();
}
window.addEventListener('popstate', () => {
  const r = parseHash();
  if (r) { state.route = r; $app.dataset.screen = 'list'; } else if (phoneQ.matches) $app.dataset.screen = 'lists';
  draw();
});

// ---------- the sidebar ----------

function drawSide() {
  const cur = state.route;
  render($('tiles'), html`${SMART.map((s) => html`<button type="button" class="tile" data-act="smart" data-id="${s.id}" data-color="${s.color}" role="listitem" aria-current="${!state.query && cur.kind === 'smart' && cur.id === s.id}">
      <span class="tile-top"><span class="disc">${icon(s.icon, 18)}</span><span class="n">${smartCount(s.id)}</span></span><span class="tile-name">${s.name}</span></button>`)}`);
  render($('lists'), state.lists.length ? html`${state.lists.map((l) => html`<div class="lrow" data-item data-act="list" data-id="${l.id}" data-color="${l.color}" role="listitem" tabindex="0" aria-current="${!state.query && cur.kind === 'list' && cur.id === l.id}">
      <span class="disc">${icon(l.icon, 16)}</span><span class="lname">${l.name}</span>${l.shared || l.mode !== 'owner' ? html`<span class="lshare" title="${l.mode === 'owner' ? 'Shared' : 'Shared with you'}"><sol-icon name="people"></sol-icon></span>` : ''}<span class="lcount">${l.count || ''}</span></div>`)}`
    : html`<div class="empty small"><p>No lists yet. Add one, or just type a task below.</p></div>`);
}

// ---------- the task list ----------

/** Sections of tasks for the current route: [{title, tone, tasks}]. */
function sections() {
  const r = state.route;
  if (state.query) {
    const words = state.query.toLowerCase().split(/\s+/).filter(Boolean);
    const hit = state.tasks.filter((t) => { const h = (t.title + '\n' + t.notes + '\n' + t.list_name).toLowerCase(); return words.every((w) => h.includes(w)); });
    return [{ title: '', tasks: hit }];
  }
  if (r.kind === 'list') {
    const all = state.tasks.filter((t) => t.list === r.id);
    const top = all.filter((t) => !t.parent);
    const rows = [];
    const put = (t) => { rows.push(t, ...all.filter((x) => x.parent === t.id)); };
    for (const t of top) if (!t.done || state.linger.has(t.id)) put(t);
    if (state.showDone) for (const t of top) if (t.done && !state.linger.has(t.id)) put(t);
    return [{ title: '', tasks: rows }];
  }
  const ts = state.tasks.filter((t) => inSmart(r.id, t) || (state.linger.has(t.id) && !t.parent && t.done && r.id !== 'completed')).filter((t) => r.id === 'completed' || !t.done || state.linger.has(t.id));
  if (r.id === 'completed') return [{ title: '', tasks: ts.sort((a, b) => b.done_at - a.done_at) }];
  if (r.id === 'today' || r.id === 'scheduled') {
    const by = (a, b) => (a.due + a.time).localeCompare(b.due + b.time) || a.order - b.order;
    ts.sort(by);
    const groups = [];
    for (const t of ts) {
      const key = t.due && t.due < state.today ? 'overdue' : t.due || 'none';
      const title = key === 'overdue' ? 'Overdue' : key === 'none' ? '' : formatDay(t.due, new Date());
      const g = groups.find((x) => x.key === key);
      if (g) g.tasks.push(t); else groups.push({ key, title, tone: key === 'overdue' ? 'bad' : '', tasks: [t] });
    }
    if (r.id === 'today') for (const g of groups) if (g.key !== 'overdue') g.title = groups.length > 1 ? 'Today' : '';
    return groups;
  }
  return [{ title: '', tasks: ts }];
}

function taskRow(t, showList) {
  const due = t.due ? formatDue(t.due, t.time, new Date()) : null;
  const note = (t.notes || '').split('\n').find((l) => l.trim()) || '';
  const sub = t.parent ? ' sub' : '';
  const ro = !canWrite(listOf(t.list));
  const meta = [
    note ? html`<span class="note">${note}</span>` : '',
    due ? html`<span class="due ${due.overdue ? 'over' : ''}">${due.text}</span>` : '',
    t.repeat ? html`<span class="rep" title="${formatRepeat(t.repeat)}">${svg('repeat', 13)}</span>` : '',
    t.subtasks && !t.parent ? html`<span class="subn">${t.subtasks_done}/${t.subtasks}</span>` : '',
    showList ? html`<span class="inlist" data-color="${t.list_color}">${t.list_name}</span>` : '',
  ].filter((x) => x !== '');
  return html`<div class="task${sub}${t.done ? ' done' : ''}${state.sel === t.id ? ' current' : ''}" data-item data-id="${t.id}" data-color="${t.list_color}" draggable="${!ro && !t.done}">
    <button class="check" type="button" data-act="toggle" aria-label="${t.done ? 'Mark as not completed' : 'Mark as completed'}" aria-pressed="${t.done}" ${ro ? raw('disabled') : ''}><sol-icon name="check"></sol-icon></button>
    <div class="tbody"><input class="ttl" value="${t.title}" aria-label="Title" ${ro ? raw('readonly') : ''} autocomplete="off" spellcheck="true" data-id="${t.id}">
      ${meta.length ? html`<div class="meta">${meta}</div>` : ''}</div>
    ${t.flag ? html`<span class="flag" title="Flagged">${svg('flag', 16)}</span>` : ''}
    <button class="info" type="button" data-act="info" aria-label="Details">${svg('info', 20)}</button>
    ${ro ? '' : html`<div class="swipe-actions"><button type="button" data-act="swipe-flag">${t.flag ? 'Unflag' : 'Flag'}</button><button type="button" class="danger" data-act="swipe-delete">Delete</button></div>`}
  </div>`;
}

function drawMain() {
  const r = state.route;
  const searching = !!state.query;
  const list = r.kind === 'list' ? listOf(r.id) : null;
  const smart = r.kind === 'smart' ? SMART.find((s) => s.id === r.id) : null;
  if (!list && !smart && !searching) { // a list that has not arrived yet: show nothing rather than a wrong title
    $('title').textContent = ''; $('count').textContent = ''; render($('items'), html``); $('quick').hidden = true; drawBar(null, true);
    return;
  }
  const title = searching ? 'Search' : list ? list.name : smart.name;
  const color = searching ? 'gray' : list ? list.color : smart.color;
  const secs = sections();
  const n = secs.reduce((a, s) => a + s.tasks.filter((t) => !t.done).length, 0);
  const total = secs.reduce((a, s) => a + s.tasks.length, 0);
  $('main').dataset.color = color;
  $('title').textContent = title;
  $('count').textContent = searching ? (total ? plural(total, 'result') : '') : (r.id === 'completed' ? (total ? plural(total, 'task') : '') : String(n));
  $('banner').hidden = !(list && !canWrite(list));
  if (list && !canWrite(list)) $('banner').textContent = 'This list is shared with you read-only.';
  const showList = !list;
  const empty = searching ? html`<div class="empty"><sol-icon name="search"></sol-icon><h3>No results</h3><p>Nothing matches “${state.query}”.</p></div>`
    : list ? html`<div class="empty"><sol-icon name="check"></sol-icon><h3>${state.tasks.some((t) => t.list === list.id) ? 'All done' : 'No tasks yet'}</h3><p>${canWrite(list) ? 'Type below to add one. Try “call mom tomorrow 9am”.' : 'Nothing here yet.'}</p></div>`
      : html`<div class="empty"><sol-icon name="${smart.id === 'completed' ? 'check' : 'clock'}"></sol-icon><h3>${{ today: 'Nothing due today', scheduled: 'Nothing scheduled', all: 'No open tasks', flagged: 'Nothing flagged', completed: 'Nothing completed yet' }[smart.id]}</h3><p>${smart.id === 'completed' ? 'Tasks you complete show up here.' : 'Add a task below.'}</p></div>`;
  render($('items'), total ? html`${secs.map((s) => html`<section class="sec">${s.title ? html`<h2 class="sec-title ${s.tone || ''}">${s.title}</h2>` : ''}<sol-list class="rows plain" aria-label="${s.title || title}">${s.tasks.map((t) => taskRow(t, showList))}</sol-list></section>`)}` : empty);
  for (const el of $('items').querySelectorAll('.task')) el.dataset.swipeReady = '1';
  drawBar(list, searching);
  const writable = !searching && (!list || canWrite(list)) && r.id !== 'completed';
  $('quick').hidden = !writable;
  $('quick-in').placeholder = list ? 'New task in ' + list.name : 'New task';
}

function drawBar(list, searching) {
  const btn = (act, ic, label) => html`<button class="ghost icon-btn" type="button" data-act="${act}" aria-label="${label}">${icon(ic, 20)}</button>`;
  render($('actions'), html`${list && list.mode === 'owner' ? btn('share', 'share', 'Share list') : ''}${list ? btn('list-menu', 'more', 'List options') : searching ? '' : btn('smart-menu', 'more', 'Options')}`);
}

function draw() {
  drawSide();
  drawMain();
  drawDetails();
  const sc = $('scroll');
  void sc;
}

// While the person is typing in a title, a remote change waits so it cannot steal the caret.
let drawLater = false;
function safeDraw() {
  const a = document.activeElement;
  if (a && a.classList?.contains('ttl') && a.value !== a.defaultValue) { drawLater = true; return; }
  draw();
}
document.addEventListener('focusout', () => { if (drawLater) { drawLater = false; setTimeout(draw, 0); } });

// ---------- details (a panel on wide screens, a sheet otherwise) ----------

let sheet = null;
function closeDetails() { if (sheet) { const s = sheet; sheet = null; s.close(); } }

function detailsView(t, inSheet) {
  const list = listOf(t.list), ro = !canWrite(list), dis = ro ? raw('disabled') : '';
  const subs = state.tasks.filter((x) => x.parent === t.id);
  const writable = state.lists.filter((l) => canWrite(l));
  return html`${inSheet ? '' : html`<div class="d-head"><h2>Details</h2><button class="ghost icon-btn" type="button" data-d="close" aria-label="Close"><sol-icon name="close"></sol-icon></button></div>`}
    <div class="d-body" data-id="${t.id}" data-color="${t.list_color}">
      <div class="card group">
        <input class="d-title" name="title" value="${t.title}" aria-label="Title" ${dis} autocomplete="off">
        <textarea name="notes" placeholder="Notes" aria-label="Notes" ${dis}>${t.notes}</textarea>
      </div>
      <div class="card group">
        <label class="line"><span class="lbl"><span class="disc d-blue">${icon('cal', 16)}</span>Date</span><input type="checkbox" role="switch" name="hasdate" ${t.due ? raw('checked') : ''} ${dis}></label>
        ${t.due ? html`<label class="line sub-line"><span class="lbl">${formatLong(t.due)}</span><input type="date" name="due" value="${t.due}" ${dis}></label>
          <label class="line"><span class="lbl"><span class="disc d-blue">${icon('clock', 16)}</span>Time</span><input type="checkbox" role="switch" name="hastime" ${t.time ? raw('checked') : ''} ${dis}></label>
          ${t.time ? html`<label class="line sub-line"><span class="lbl">At</span><input type="time" name="time" value="${t.time}" ${dis}></label>` : ''}
          <label class="line"><span class="lbl"><span class="disc d-gray">${svg('repeat', 16)}</span>Repeat</span><select name="repeat" ${dis}>${REPEATS.map((r) => html`<option value="${r}" ${r === t.repeat ? raw('selected') : ''}>${r ? formatRepeat(r) : 'Never'}</option>`)}</select></label>` : ''}
        <label class="line"><span class="lbl"><span class="disc d-orange">${svg('flag', 16)}</span>Flag</span><input type="checkbox" role="switch" name="flag" ${t.flag ? raw('checked') : ''} ${dis}></label>
      </div>
      <div class="card group">
        <label class="line"><span class="lbl"><span class="disc" data-color="${t.list_color}">${icon(list?.icon || 'list', 16)}</span>List</span>
          <select name="list" ${dis}>${writable.map((l) => html`<option value="${l.id}" ${l.id === t.list ? raw('selected') : ''}>${l.name}</option>`)}${ro ? html`<option selected>${t.list_name}</option>` : ''}</select></label>
      </div>
      ${t.parent ? '' : html`<div class="card group subs"><h3 class="group-h">Subtasks</h3>
        ${subs.map((s) => html`<div class="line subrow ${s.done ? 'done' : ''}" data-id="${s.id}"><button class="check" type="button" data-d="subtoggle" aria-pressed="${s.done}" aria-label="Toggle subtask" ${dis}><sol-icon name="check"></sol-icon></button><input class="ttl" value="${s.title}" data-sub="${s.id}" aria-label="Subtask" ${dis}><button class="ghost icon-btn" type="button" data-d="subdel" aria-label="Delete subtask" ${dis}><sol-icon name="close"></sol-icon></button></div>`)}
        ${ro ? '' : html`<form class="line addsub" data-d="addsub"><sol-icon name="plus"></sol-icon><input name="sub" placeholder="Add subtask" aria-label="Add subtask" autocomplete="off"></form>`}</div>`}
      ${ro ? '' : html`<button class="danger d-delete" type="button" data-d="delete">Delete task</button>`}
    </div>`;
}

function drawDetails() {
  const t = state.sel && taskOf(state.sel);
  const panel = $('detail');
  if (!t) { panel.hidden = true; $app.classList.remove('with-detail'); if (sheet) closeDetails(); return; }
  if (panelQ.matches) {
    if (sheet) closeDetails();
    // Keep the field being typed in: re-render only what changed.
    const active = panel.contains(document.activeElement) && document.activeElement.matches('input[type=text],input:not([type]),textarea');
    if (!active) render(panel, detailsView(t));
    panel.hidden = false; $app.classList.add('with-detail');
    return;
  }
  panel.hidden = true; $app.classList.remove('with-detail');
  if (!sheet) {
    const el = document.createElement('sol-sheet');
    el.setAttribute('heading', 'Details');
    const box = document.createElement('div');
    box.className = 'dbox';
    el.append(box);
    document.body.append(el);
    el.addEventListener('close', () => { el.remove(); if (sheet === el) { sheet = null; } state.sel = null; drawMain(); });
    sheet = el; el.box = box;
    el.show();
    render(box, detailsView(t, true));
    wireDetails(box);
    return;
  }
  if (!sheet.box.contains(document.activeElement) || !document.activeElement.matches('input[type=text],input:not([type]),textarea')) render(sheet.box, detailsView(t, true));
}

function wireDetails(root) {
  root.addEventListener('change', onDetailChange);
  root.addEventListener('click', onDetailClick);
  root.addEventListener('submit', onDetailSubmit);
}
wireDetails($('detail'));

async function onDetailChange(e) {
  const el = e.target, body = el.closest('.d-body');
  if (!body) return;
  const id = body.dataset.id, t = taskOf(id);
  if (!t) return;
  const n = el.name;
  if (el.dataset.sub) { if (el.value.trim()) await act('tasks.update', { id: el.dataset.sub, title: el.value.trim() }); return; }
  if (n === 'title') { if (el.value.trim()) await act('tasks.update', { id, title: el.value.trim() }); else el.value = t.title; }
  else if (n === 'notes') await act('tasks.update', { id, notes: el.value });
  else if (n === 'hasdate') await act('tasks.update', { id, due: el.checked ? state.today : '' });
  else if (n === 'due') await act('tasks.update', { id, due: el.value || '' });
  else if (n === 'hastime') await act('tasks.update', { id, time: el.checked ? '09:00' : '' });
  else if (n === 'time') await act('tasks.update', { id, time: el.value });
  else if (n === 'repeat') await act('tasks.update', { id, repeat: el.value });
  else if (n === 'flag') await act('tasks.update', { id, flag: el.checked });
  else if (n === 'list' && el.value !== t.list) await act('tasks.move', { id, list: el.value });
  const box = el.closest('.d-body')?.parentElement;
  if (box) drawDetails();
}

async function onDetailClick(e) {
  const b = e.target.closest('[data-d]');
  if (!b) return;
  const body = b.closest('.d-body'), id = body?.dataset.id;
  switch (b.dataset.d) {
    case 'close': state.sel = null; draw(); break;
    case 'delete': if (await confirmBox('Delete this task?', 'It is deleted on all your devices and for anyone this list is shared with.', 'Delete')) { await act('tasks.delete', { ids: [id] }); state.sel = null; draw(); } break;
    case 'subtoggle': { const sid = b.closest('.subrow').dataset.id; const s = taskOf(sid); await act('tasks.complete', { id: sid, done: !s.done }); break; }
    case 'subdel': await act('tasks.delete', { ids: [b.closest('.subrow').dataset.id] }); break;
  }
}

async function onDetailSubmit(e) {
  e.preventDefault();
  const f = e.target.closest('form[data-d="addsub"]');
  if (!f) return;
  const v = f.elements.sub.value.trim(), id = f.closest('.d-body').dataset.id;
  if (!v) return;
  f.elements.sub.value = '';
  await act('tasks.add', { title: v, parent: id });
  document.querySelector('.addsub input')?.focus();
}

// ---------- dialogs ----------

function dialog(heading, content) {
  const el = document.createElement('sol-dialog');
  el.setAttribute('heading', heading);
  const box = document.createElement('div');
  el.append(box);
  document.body.append(el);
  render(box, content);
  el.addEventListener('close', () => el.remove());
  el.show();
  return { el, box, close: () => el.close() };
}

function confirmBox(title, body, ok) {
  return new Promise((resolve) => {
    let done = false;
    const d = dialog(title, html`<p>${body}</p><div class="dlg-actions"><button type="button" data-no>Cancel</button><button class="danger primary" type="button" data-yes>${ok}</button></div>`);
    const finish = (v) => { if (!done) { done = true; resolve(v); } d.close(); };
    d.box.querySelector('[data-no]').addEventListener('click', () => finish(false));
    d.box.querySelector('[data-yes]').addEventListener('click', () => finish(true));
    d.el.addEventListener('close', () => finish(false));
  });
}

/** The list editor: a name, a colour and an icon. Resolves with the values, or null. */
function listDialog(list) {
  return new Promise((resolve) => {
    let done = false;
    const cur = { name: list?.name || '', color: list?.color || 'blue', icon: list?.icon || 'list' };
    const body = () => html`<form><div class="preview" data-color="${cur.color}"><span class="disc big">${icon(cur.icon, 30)}</span></div>
      <input name="name" value="${cur.name}" placeholder="List name" aria-label="List name" autocomplete="off" required>
      <div class="swatches" role="radiogroup" aria-label="Color">${COLORS.map((c) => html`<button type="button" class="swatch" data-color="${c}" data-pick-color="${c}" role="radio" aria-checked="${c === cur.color}" aria-label="${c}"></button>`)}</div>
      <div class="icons" role="radiogroup" aria-label="Icon">${ICONS.map((i) => html`<button type="button" class="ibtn" data-pick-icon="${i}" role="radio" aria-checked="${i === cur.icon}" aria-label="${i}">${icon(i, 20)}</button>`)}</div>
      <div class="dlg-actions"><button type="button" data-no>Cancel</button><button class="primary" type="submit">${list ? 'Done' : 'Add List'}</button></div></form>`;
    const d = dialog(list ? 'List info' : 'New list', body());
    const finish = (v) => { if (!done) { done = true; resolve(v); } d.close(); };
    d.box.addEventListener('click', (e) => {
      const c = e.target.closest('[data-pick-color]'), i = e.target.closest('[data-pick-icon]');
      if (c || i) {
        cur.name = d.box.querySelector('[name=name]').value;
        if (c) cur.color = c.dataset.pickColor; else cur.icon = i.dataset.pickIcon;
        render(d.box, body());
        d.box.querySelector('[name=name]').focus();
      } else if (e.target.closest('[data-no]')) finish(null);
    });
    d.box.addEventListener('submit', (e) => { e.preventDefault(); const name = d.box.querySelector('[name=name]').value.trim(); if (name) finish({ ...cur, name }); });
    d.el.addEventListener('close', () => finish(null));
    d.box.querySelector('[name=name]').focus();
  });
}

async function shareDialog(list) {
  let ppl = [];
  try { ppl = await listPeople(); } catch (e) { fail(e); return; }
  const mode = (p) => (list.members || []).find((m) => m.person === p.id)?.mode || 'none';
  const body = () => ppl.length ? html`<p class="muted">People you share this list with see its tasks. “Can edit” lets them add, change and complete tasks.</p>
    <div class="list">${ppl.map((p) => html`<div class="prow" data-item data-person="${p.id}"><sol-avatar name="${p.petname || p.name}"></sol-avatar><span class="pname">${p.petname || p.name}</span>
      <select data-share="${p.id}" aria-label="Access for ${p.petname || p.name}"><option value="none" ${mode(p) === 'none' ? raw('selected') : ''}>Not shared</option><option value="read" ${mode(p) === 'read' ? raw('selected') : ''}>Can view</option><option value="write" ${mode(p) === 'write' ? raw('selected') : ''}>Can edit</option></select></div>`)}</div>`
    : html`<div class="empty"><sol-icon name="people"></sol-icon><h3>No one to share with yet</h3><p>Add a person in Sol, then share lists with them here.</p></div>`;
  const d = dialog('Share “' + list.name + '”', body());
  d.box.addEventListener('change', async (e) => {
    const s = e.target.closest('[data-share]');
    if (!s) return;
    const r = await act('tasks.share', { list: list.id, person: s.dataset.share, mode: s.value });
    if (r) { toast(s.value === 'none' ? 'Stopped sharing' : 'Shared'); list = listOf(list.id) || list; }
  });
}

// ---------- actions in the UI ----------

function download(name, text) {
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([text], { type: 'text/markdown' }));
  a.download = name;
  document.body.append(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 4000);
}

async function addList() {
  const v = await listDialog(null);
  if (!v) return;
  const r = await act('tasks.create_list', v);
  if (r) go({ kind: 'list', id: r.list.id });
}

async function editList(list) {
  const v = await listDialog(list);
  if (v) await act('tasks.update_list', { list: list.id, ...v });
}

function pickFile(accept, onText) {
  const input = document.createElement('input');
  input.type = 'file'; input.accept = accept;
  input.addEventListener('change', async () => { const f = input.files[0]; if (f) onText(await f.text(), f.name); });
  input.click();
}

function listMenu(anchor) {
  const list = listOf(state.route.id);
  if (!list) return;
  const owner = list.mode === 'owner';
  showMenu([
    { label: 'List info', icon: 'edit', disabled: !canWrite(list), onSelect: () => editList(list) },
    owner && { label: 'Share list', icon: 'people', onSelect: () => shareDialog(list) },
    { label: state.showDone ? 'Hide completed' : 'Show completed', icon: 'eye', onSelect: () => { state.showDone = !state.showDone; store.set('tasks-showdone', state.showDone ? '1' : ''); draw(); } },
    { separator: true },
    { label: 'Export Markdown', icon: 'download', onSelect: async () => { try { const r = await call('tasks.export', { list: list.id }); download(list.name.replace(/[\\/:*?"<>|]+/g, '-') + '.md', r.markdown); } catch (e) { fail(e); } } },
    { label: 'Import Markdown', icon: 'upload', disabled: !canWrite(list), onSelect: () => pickFile('.md,.markdown,.txt,text/markdown,text/plain', async (md) => { const r = await act('tasks.import', { markdown: md, list: list.id }); if (r) toast('Imported ' + plural(r.added, 'task')); }) },
    { separator: true },
    { label: owner ? 'Delete list' : 'Leave list', icon: 'trash', danger: true, onSelect: async () => {
      const ok = await confirmBox(owner ? 'Delete this list?' : 'Leave this list?', owner ? `“${list.name}” and its tasks are deleted on all your devices${list.shared ? ' and for the people you share it with' : ''}.` : `“${list.name}” stays with its owner; you stop seeing it.`, owner ? 'Delete' : 'Leave');
      if (ok) { await act('tasks.delete_list', { list: list.id }); showLists(); if (!phoneQ.matches) go({ kind: 'smart', id: 'today' }, true); }
    } },
  ].filter(Boolean), { anchor, placement: 'bottom-end' });
}

function smartMenu(anchor) {
  showMenu([
    { label: 'Import Markdown into a new list', icon: 'upload', onSelect: () => pickFile('.md,.markdown,.txt,text/markdown,text/plain', async (md, name) => { const r = await act('tasks.import', { markdown: md, name: name.replace(/\.(md|markdown|txt)$/i, '') }); if (r) { toast('Imported ' + plural(r.added, 'task')); go({ kind: 'list', id: r.list }); } }) },
    { label: 'New list', icon: 'plus', onSelect: addList },
  ], { anchor, placement: 'bottom-end' });
}

async function complete(id) {
  const t = taskOf(id);
  if (!t) return;
  const done = !t.done;
  t.done = done; t.done_at = done ? Date.now() : 0; // instantly, then the answer
  if (done) { state.linger.add(id); setTimeout(() => { state.linger.delete(id); safeDraw(); }, 1100); }
  draw();
  const r = await act('tasks.complete', { id, done });
  if (r && done && r.next) toast('Next one: ' + formatDay(r.next.due, new Date()), { kind: 'info' });
}

/** The list a quick-added task goes to. */
function targetList(tagged) {
  if (tagged) { const l = state.lists.find((x) => x.name.toLowerCase() === tagged.toLowerCase() && canWrite(x)); if (l) return l.id; }
  if (state.route.kind === 'list') return state.route.id;
  return state.lists.find((l) => canWrite(l))?.id;
}

async function quickAdd(text) {
  const p = parseQuick(text, new Date());
  const args = { title: p.title };
  const list = targetList(p.list);
  if (list) args.list = list;
  let due = p.due, time = p.time;
  if (!due && state.route.kind === 'smart' && (state.route.id === 'today' || state.route.id === 'scheduled')) due = state.today;
  if (due) args.due = due;
  if (time) args.time = time;
  if (p.repeat) args.repeat = p.repeat;
  if (p.flag || (state.route.kind === 'smart' && state.route.id === 'flagged')) args.flag = true;
  const r = await act('tasks.add', args);
  if (r) {
    // keep the new task in view when the current view would hide it
    const t = r.task;
    if (state.route.kind === 'smart' && !inSmart(state.route.id, t)) toast('Added to ' + t.list_name, { kind: 'info' });
  }
}

// ---------- events from the page ----------

document.addEventListener('click', async (e) => {
  const b = e.target.closest('[data-act]');
  if (!b) return;
  const row = b.closest('.task');
  switch (b.dataset.act) {
    case 'smart': go({ kind: 'smart', id: b.dataset.id }); break;
    case 'list': go({ kind: 'list', id: b.dataset.id }); break;
    case 'toggle': await complete(row.dataset.id); break;
    case 'info': state.sel = state.sel === row.dataset.id && panelQ.matches ? null : row.dataset.id; draw(); break;
    case 'share': shareDialog(listOf(state.route.id)); break;
    case 'list-menu': listMenu(b); break;
    case 'smart-menu': smartMenu(b); break;
    case 'swipe-flag': { const t = taskOf(row.dataset.id); row.closest('sol-list')?.closeSwipe?.(row); await act('tasks.update', { id: t.id, flag: !t.flag }); break; }
    case 'swipe-delete': await act('tasks.delete', { ids: [row.dataset.id] }); toast('Deleted'); break;
  }
});
document.addEventListener('keydown', (e) => {
  const row = e.target.closest?.('[data-act="list"],[data-act="smart"]');
  if (row && (e.key === 'Enter' || e.key === ' ') && row.tagName !== 'BUTTON') { e.preventDefault(); row.click(); }
});

$('add-list').addEventListener('click', addList);
$('back').addEventListener('click', () => { if (history.state && history.state.in) history.back(); else showLists(); });

// Inline title editing: the title is an input, saved on change.
$('items').addEventListener('change', async (e) => {
  const el = e.target.closest('.ttl');
  if (!el) return;
  const t = taskOf(el.dataset.id);
  const v = el.value.trim();
  if (!v) { el.value = t.title; return; }
  t.title = v;
  await act('tasks.update', { id: t.id, title: v });
});
$('items').addEventListener('keydown', (e) => {
  if (!e.target.classList.contains('ttl')) return;
  if (e.key === 'Enter') { e.preventDefault(); e.target.blur(); $('quick-in').focus(); }
  if (e.key === 'Escape') { e.target.value = taskOf(e.target.dataset.id)?.title ?? e.target.value; e.target.blur(); }
});
// Selecting a row opens its details on wide screens.
$('items').addEventListener('focusin', (e) => {
  const row = e.target.closest('.task');
  if (row && e.target.classList.contains('ttl') && panelQ.matches && state.sel !== row.dataset.id) { state.sel = row.dataset.id; drawDetails(); for (const r of $('items').querySelectorAll('.task')) r.classList.toggle('current', r.dataset.id === state.sel); }
});

contextMenu($('items'), (ev) => {
  const row = ev.target.closest('.task');
  if (!row) return [];
  const t = taskOf(row.dataset.id), ro = !canWrite(listOf(t.list));
  const moves = state.lists.filter((l) => canWrite(l) && l.id !== t.list);
  return [
    { label: 'Details', icon: 'info', onSelect: () => { state.sel = t.id; draw(); } },
    { label: t.done ? 'Mark as not completed' : 'Mark as completed', icon: 'check', disabled: ro, onSelect: () => complete(t.id) },
    { label: t.flag ? 'Remove flag' : 'Flag', icon: 'pin', disabled: ro, onSelect: () => act('tasks.update', { id: t.id, flag: !t.flag }) },
    ...(moves.length && !ro ? [{ separator: true }, ...moves.slice(0, 8).map((l) => ({ label: 'Move to ' + l.name, icon: 'folder', onSelect: () => act('tasks.move', { id: t.id, list: l.id }) }))] : []),
    { separator: true },
    { label: 'Delete', icon: 'trash', danger: true, disabled: ro, onSelect: () => act('tasks.delete', { ids: [t.id] }) },
  ];
});

// Drag to reorder inside a list (mouse; touch uses the swipe actions).
let dragId = null;
$('items').addEventListener('dragstart', (e) => { const r = e.target.closest?.('.task'); if (!r || state.route.kind !== 'list') { e.preventDefault(); return; } dragId = r.dataset.id; e.dataTransfer.effectAllowed = 'move'; e.dataTransfer.setData('text/plain', dragId); r.classList.add('dragging'); });
$('items').addEventListener('dragend', () => { dragId = null; for (const r of $('items').querySelectorAll('.dragging,.drop-before,.drop-after')) r.classList.remove('dragging', 'drop-before', 'drop-after'); });
$('items').addEventListener('dragover', (e) => {
  const r = e.target.closest?.('.task');
  if (!dragId || !r || r.dataset.id === dragId) return;
  e.preventDefault();
  const below = e.clientY > r.getBoundingClientRect().top + r.offsetHeight / 2;
  for (const x of $('items').querySelectorAll('.drop-before,.drop-after')) x.classList.remove('drop-before', 'drop-after');
  r.classList.add(below ? 'drop-after' : 'drop-before');
});
$('items').addEventListener('drop', async (e) => {
  const r = e.target.closest?.('.task');
  if (!dragId || !r || r.dataset.id === dragId) return;
  e.preventDefault();
  const below = e.clientY > r.getBoundingClientRect().top + r.offsetHeight / 2;
  const id = dragId; dragId = null;
  await act('tasks.move', below ? { id, after: r.dataset.id } : { id, before: r.dataset.id });
});

// Quick add, with a live read of the date it understood.
const qin = $('quick-in');
qin.addEventListener('input', () => {
  const p = parseQuick(qin.value, new Date());
  const hint = describeParsed(p, new Date());
  $('hint').textContent = qin.value.trim() && (hint || p.list) ? [hint, p.list ? '#' + p.list : ''].filter(Boolean).join(' · ') : '';
});
$('quick').addEventListener('submit', async (e) => {
  e.preventDefault();
  const v = qin.value.trim();
  if (!v) return;
  qin.value = ''; $('hint').textContent = '';
  await quickAdd(v);
  qin.focus();
});

let searchT = 0;
$('q').addEventListener('input', () => {
  clearTimeout(searchT);
  searchT = setTimeout(() => { state.query = $('q').value.trim(); if (state.query) $app.dataset.screen = 'list'; else if (phoneQ.matches && !parseHash()) $app.dataset.screen = 'lists'; draw(); }, 80);
});

document.addEventListener('keydown', (e) => {
  const typing = /INPUT|TEXTAREA|SELECT/.test(document.activeElement?.tagName);
  if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
  if (e.key === 'n' && !$('quick').hidden) { e.preventDefault(); qin.focus(); }
  if (e.key === '/') { e.preventDefault(); $('q').focus(); }
  if (e.key === 'Escape' && state.sel) { state.sel = null; draw(); }
});

panelQ.addEventListener('change', () => { closeDetails(); draw(); });
phoneQ.addEventListener('change', () => { $app.dataset.screen = phoneQ.matches ? (parseHash() ? 'list' : 'lists') : 'list'; draw(); });

// ---------- start ----------

state.showDone = !!store.get('tasks-showdone', '');
const fromHash = parseHash();
if (fromHash) state.route = fromHash;
$app.dataset.screen = phoneQ.matches && !fromHash ? 'lists' : 'list';
try { const c = JSON.parse(localStorage.getItem('tasks-cache') || 'null'); if (c && c.lists) { adopt(c); } } catch { /* optional */ }
draw();

// Pushed changes: any feed entry or feed change means another read of the snapshot.
let evT = 0;
const onChange = () => { clearTimeout(evT); evT = setTimeout(async () => { await refresh(); if (drawLater) safeDraw(); }, 60); };
events({}, (ev) => { if (ev.type === 'entry' || ev.type === 'feed') onChange(); }, () => { /* reconnects by itself */ });
// The day turns: Today and the overdue marks change with it, and a timer is enough to notice.
(function midnight() {
  const n = new Date();
  setTimeout(() => { refresh(); midnight(); }, new Date(n.getFullYear(), n.getMonth(), n.getDate() + 1, 0, 0, 5) - n);
})();

// Last, and awaited: the page counts as loaded once the first real snapshot is painted.
await refresh();
