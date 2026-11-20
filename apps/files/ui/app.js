// Files UI. It only calls actions (sol.call) and listens to events; it never touches the disk,
// feeds or blobs directly, except to hand an uploaded file to the daemon (putBlob) and then ask
// the app to write it (files.write). Every API value reaches the DOM through html`` (escaped).
import { call, events, putBlob, people, html, raw, render } from '/sol.js';
import { toast, setTheme, showMenu, contextMenu } from '/kit/kit.js';

const $main = document.getElementById('main');
const $shell = document.getElementById('shell');
const $veil = document.getElementById('dropveil');

const store = {
  get(k, d) { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* optional */ } },
};

const state = {
  route: { kind: 'dir', path: '' },
  view: new URLSearchParams(location.search).get('view') || store.get('files-view', 'list'),
  entries: [],
  here: null, // stat of the current folder
  sel: new Set(),
  query: '',
  status: null,
  conflicts: [],
  error: '',
  ready: false, // the folder's listing has arrived (or came from the local cache)
  selecting: false, // explicit select mode, like iOS "Select"
  counts: new Map(), // folder path -> number of items (filled lazily from files.list)
};
const phoneQ = matchMedia('(max-width: 760px)');

// ---------- helpers ----------

const join = (d, n) => (d ? d + '/' + n : n);
const parent = (p) => p.slice(0, Math.max(0, p.lastIndexOf('/')));
const rawURL = (p, dl) => '/raw?path=' + encodeURIComponent(p) + (dl ? '&download=1' : '');
const fail = (e) => toast(e?.message || String(e), { kind: 'error' });
const isVirtual = (p) => p === 'Shared' || (p.startsWith('Shared/') && p.split('/').length <= 2);

function fmtSize(n) {
  if (n == null) return '';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1000 && i < u.length - 1) { n /= 1000; i++; }
  return (i ? n.toFixed(n < 10 ? 1 : 0) : n) + ' ' + u[i];
}
const clock = (d) => d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
/** Relative, human dates: "Today 9:21", "Yesterday 9:21", "Mon 9:21", then "Nov 3" or "Nov 3, 2025". */
function fmtTime(ms) {
  if (!ms) return '';
  const d = new Date(ms), now = new Date();
  const day = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate());
  const diff = Math.round((day(now) - day(d)) / 864e5);
  if (diff === 0) return 'Today ' + clock(d);
  if (diff === 1) return 'Yesterday ' + clock(d);
  if (diff > 1 && diff < 7) return d.toLocaleDateString([], { weekday: 'short' }) + ' ' + clock(d);
  return d.toLocaleDateString([], { month: 'short', day: 'numeric', year: d.getFullYear() === now.getFullYear() ? undefined : 'numeric' });
}
const ext = (n) => (n.includes('.') ? n.slice(n.lastIndexOf('.') + 1).toLowerCase() : '');
const KINDS = {
  image: ['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'bmp'],
  audio: ['mp3', 'ogg', 'oga', 'wav', 'flac', 'm4a', 'opus', 'aac'],
  video: ['mp4', 'webm', 'mkv', 'mov', 'm4v', 'ogv'],
  pdf: ['pdf'],
  text: ['txt', 'md', 'json', 'csv', 'log', 'js', 'ts', 'go', 'py', 'rs', 'c', 'h', 'css', 'html', 'xml', 'yml', 'yaml', 'toml', 'ini', 'sh', 'svg', 'tsv'],
};
function kindOf(e) {
  if (e.type === 'dir') return 'dir';
  const x = ext(e.name);
  for (const [k, list] of Object.entries(KINDS)) if (list.includes(x)) return k;
  return 'file';
}
const ICON = { dir: 'folder', image: 'image', audio: 'music', video: 'video', pdf: 'file', text: 'file', file: 'file' };

// ---------- routing and loading ----------

function parseHash() {
  const h = decodeURIComponent(location.hash || '#/');
  if (h.startsWith('#!conflicts')) return { kind: 'conflicts' };
  if (h.startsWith('#!sync')) return { kind: 'sync' };
  if (h.startsWith('#!search=')) return { kind: 'search', q: h.slice(9) };
  return { kind: 'dir', path: h.replace(/^#\/?/, '').replace(/\/+$/, '') };
}
const go = (p) => { location.hash = '#/' + p.split('/').map(encodeURIComponent).join('/'); };

let loadSeq = 0;
// The last listing per folder is kept locally so the folder paints at once on the next visit;
// the live answer replaces it a moment later (this is local data, so no skeletons).
const cacheKey = (p) => 'files-cache:' + p;
function readCache(p) { try { return JSON.parse(localStorage.getItem(cacheKey(p)) || 'null'); } catch { return null; } }
function writeCache(p, entries, here) { try { localStorage.setItem(cacheKey(p), JSON.stringify({ entries: entries.slice(0, 300), here })); } catch { /* optional */ } }

async function load() {
  const seq = ++loadSeq;
  const r = state.route = parseHash();
  if (r.kind === 'dir' && state.here?.path !== r.path) {
    const c = readCache(r.path);
    state.entries = c ? c.entries : [];
    state.here = c ? c.here : null;
    state.error = '';
    state.ready = !!c;
    draw();
  }
  try {
    const [status, c] = await Promise.all([call('files.status', {}), call('files.conflicts', {})]);
    if (seq !== loadSeq) return;
    state.status = status;
    state.conflicts = c.conflicts;
    if (r.kind === 'dir') {
      const [list, here] = await Promise.all([call('files.list', { path: r.path }), call('files.stat', { path: r.path })]);
      if (seq !== loadSeq) return;
      state.entries = list.entries;
      state.here = here;
      state.error = '';
      writeCache(r.path, list.entries, here);
    } else if (r.kind === 'search') {
      state.query = r.q;
      state.entries = (await call('files.search', { query: r.q })).results;
      state.here = null;
    }
    // Keep only selections that still exist.
    const live = new Set(state.entries.map((e) => e.path));
    state.sel = new Set([...state.sel].filter((p) => live.has(p)));
  } catch (e) {
    if (seq !== loadSeq) return;
    state.error = e.code === 'not_found' ? 'That folder is not here (any more).' : e.message;
    state.entries = [];
  }
  state.ready = true;
  draw();
  countFolders(seq);
}

// Folder item counts for the subtitle, asked lazily (one local list call per visible folder).
async function countFolders(seq) {
  const todo = state.entries.filter((e) => e.type === 'dir' && !e.virtual).slice(0, 40);
  if (!todo.length) return;
  await Promise.all(todo.map(async (e) => {
    try { state.counts.set(e.path, (await call('files.list', { path: e.path })).entries.length); } catch { /* no count */ }
  }));
  if (seq === loadSeq) draw();
}

let timer = null;
const soon = () => { clearTimeout(timer); timer = setTimeout(load, 120); };

// ---------- drawing ----------

const hashOf = (p) => '#/' + p.split('/').filter(Boolean).map(encodeURIComponent).join('/');

// Ancestors only: the current folder is the title. On a phone this is a single "back" link.
function crumbs(path) {
  const parts = path ? path.split('/') : [];
  if (!parts.length) return '';
  let acc = '';
  const anc = [{ name: 'My files', path: '' }];
  for (const p of parts.slice(0, -1)) { acc = join(acc, p); anc.push({ name: p, path: acc }); }
  const last = anc[anc.length - 1];
  return html`<nav class="crumbs" aria-label="Folder path">
    <a class="backlink" href="${hashOf(last.path)}"><sol-icon name="back"></sol-icon>${last.name}</a>
    <span class="trail">${anc.map((a) => html`<a href="${hashOf(a.path)}">${a.name}</a><sol-icon name="chevron"></sol-icon>`)}</span></nav>`;
}

function badges(e) {
  return html`${e.conflict ? html`<span class="badge warn">${e.copy ? 'Conflict copy' : 'Conflict'}</span>` : ''}${e.shared ? html`<span class="badge sun">Shared</span>` : ''}${e.read_only && !e.virtual ? html`<span class="badge">Read only</span>` : ''}${e.type === 'file' && !e.local ? html`<span class="badge" title="Not stored on this device; opened on demand">Online only</span>` : ''}`;
}

const plural = (n, w) => n + ' ' + w + (n === 1 ? '' : 's');
function sizeCell(e) {
  if (e.type === 'dir') return state.counts.has(e.path) ? plural(state.counts.get(e.path), 'item') : '';
  return fmtSize(e.size);
}
function subLine(e) {
  const when = e.type === 'file' ? fmtTime(e.mtime) : '';
  return [when, sizeCell(e)].filter(Boolean).join(' · ');
}
const pickBox = (e, sel) => (state.selecting
  ? html`<input type="checkbox" class="pick" data-act="pick" data-path="${e.path}" aria-label="Select ${e.name}" ${raw(sel ? 'checked' : '')} ${raw(e.virtual ? 'disabled' : '')}>` : '');
const glyph = (e, k) => (k === 'image' && !e.virtual
  ? html`<img class="thumb" loading="lazy" alt="" src="${rawURL(e.path)}">`
  : html`<sol-icon class="ico ${e.type}" name="${ICON[k]}"></sol-icon>`);

function listRow(e) {
  const sel = state.sel.has(e.path);
  const k = kindOf(e);
  return html`<div data-item class="frow ${sel ? 'sel' : ''}" ${raw(e.type === 'dir' && !state.selecting ? 'data-chevron' : '')} data-path="${e.path}" data-type="${e.type}">
    ${pickBox(e, sel)}
    <span class="glyph">${glyph(e, k)}</span>
    <div class="grow name"><div class="line"><span class="title">${e.name}</span>${badges(e)}</div><span class="row-sub">${subLine(e)}</span></div>
    <span class="cell-mtime">${e.type === 'file' ? fmtTime(e.mtime) : ''}</span>
    <span class="cell-size">${sizeCell(e)}</span>
  </div>`;
}

function tile(e) {
  const sel = state.sel.has(e.path);
  const k = kindOf(e);
  return html`<div class="tile ${sel ? 'sel' : ''}" data-item data-path="${e.path}" data-type="${e.type}" tabindex="0">
    ${pickBox(e, sel)}
    <span class="art">${glyph(e, k)}</span>
    <span class="title">${e.name}</span>
    <small>${e.type === 'dir' ? sizeCell(e) : subLine(e)}</small>
  </div>`;
}

const writable = (es) => !es.some((e) => e.read_only);
/** What can be done with these entries: the one list behind the selection bar and context menus. */
function entryActions(es) {
  const one = es.length === 1 ? es[0] : null;
  const w = writable(es);
  return [
    one && one.type === 'file' && { act: 'download', icon: 'download', label: 'Download' },
    one && !one.read_only && !one.virtual && { act: 'rename', icon: 'edit', label: 'Rename' },
    one && one.type === 'dir' && !one.virtual && !one.path.startsWith('Shared/') && { act: 'share', icon: 'share', label: 'Share' },
    w && { act: 'move', icon: 'folder', label: 'Move' },
    { act: 'copy', icon: 'copy', label: 'Copy' },
    w && { act: 'delete', icon: 'trash', label: 'Delete', danger: true },
  ].filter(Boolean);
}

function selBar() {
  const es = selected();
  if (!state.selecting || !es.length) return '';
  return html`<div class="selbar glass" role="toolbar" aria-label="Selection">${entryActions(es).map((a) => html`<button type="button" class="${a.danger ? 'danger' : 'ghost'}" data-act="${a.act}"><sol-icon name="${a.icon}"></sol-icon><span>${a.label}</span></button>`)}</div>`;
}

const canWriteHere = () => state.route.kind === 'dir' && !isVirtual(state.route.path) && !(state.here && state.here.read_only);

function searchBox() {
  return html`<input class="search" type="search" id="search" placeholder="Search" aria-label="Search names" value="${state.query}">`;
}

// The window's glass toolbar: view switch, search, + menu and ... menu (Select, keep offline, theme).
function drawBar() {
  const phone = phoneQ.matches;
  const r = state.route;
  if (state.selecting) {
    render($shell.actions, html`<button type="button" class="plain" data-bar="done">Done</button>`);
    return;
  }
  const menuBtn = (id, icon, label) => html`<button type="button" class="ghost icon-btn" data-bar="${id}" aria-label="${label}" aria-haspopup="menu"><sol-icon name="${icon}"></sol-icon></button>`;
  render($shell.actions, html`
    ${phone ? '' : html`<span class="bar-pill">${syncPill()}</span>
      <div class="segmented" role="group" aria-label="View"><button type="button" class="icon-btn" data-bar="list" aria-label="List" aria-pressed="${state.view === 'list'}"><sol-icon name="list"></sol-icon></button><button type="button" class="icon-btn" data-bar="grid" aria-label="Grid" aria-pressed="${state.view === 'grid'}"><sol-icon name="grid"></sol-icon></button></div>
      ${r.kind === 'conflicts' || r.kind === 'sync' ? '' : searchBox()}`}
    ${canWriteHere() ? menuBtn('add', 'plus', 'Add') : ''}
    ${r.kind === 'dir' || r.kind === 'search' ? menuBtn('more', 'more', 'More') : ''}`);
}

const barMenus = {
  add: (el) => showMenu([
    { label: 'Upload files', icon: 'upload', onSelect: () => actions.upload() },
    { label: 'New folder', icon: 'folder-plus', onSelect: () => actions.mkdir() },
  ], { anchor: el, placement: 'bottom-end' }),
  more(el) {
    const here = state.here;
    showMenu([
      { label: 'Select', icon: 'check', disabled: !state.entries.length, onSelect: () => { state.selecting = true; draw(); } },
      { label: state.view === 'list' ? 'View as grid' : 'View as list', icon: state.view === 'list' ? 'grid' : 'list', onSelect: () => actions.view() },
      here && state.route.path && { label: here.kept ? 'Online only' : 'Keep on this device', icon: here.kept ? 'check' : 'download', onSelect: () => actions.keep() },
      { separator: true },
      { label: 'Theme', icon: 'moon', onSelect: toggleTheme },
    ].filter(Boolean), { anchor: el, placement: 'bottom-end' });
  },
};

function dirView() {
  const r = state.route;
  const sel = state.selecting ? state.sel.size : 0;
  const title = state.selecting ? (sel ? sel + ' selected' : 'Select items') : r.path ? r.path.split('/').pop() : 'My files';
  const empty = r.path === 'Shared'
    ? html`<div slot="empty" class="empty"><sol-icon name="people"></sol-icon><h3>Nothing shared with you yet</h3><p>Folders people share with you show up here, online only until you keep them on this device.</p></div>`
    : html`<div slot="empty" class="empty"><sol-icon name="folder"></sol-icon><h3>This folder is empty</h3><p>${isVirtual(r.path) ? 'Open one of the folders inside.' : 'Drop files here or tap + to add some.'}</p></div>`;
  const rows = state.entries.map(state.view === 'list' ? listRow : tile);
  const none = state.ready ? empty : '';
  return html`${state.selecting ? '' : crumbs(r.path)}
    <div class="head"><h1 class="large-title">${title}</h1>${phoneQ.matches ? syncPill() : ''}</div>
    ${phoneQ.matches ? html`<div class="searchrow">${searchBox()}</div>` : ''}
    ${state.error ? html`<div class="card empty"><sol-icon name="alert"></sol-icon><h3>${state.error}</h3></div>` :
      state.view === 'list'
        ? html`<sol-list class="files" aria-label="Files">${rows}${none}</sol-list>`
        : html`<div class="grid" role="list">${rows}</div>${state.entries.length ? '' : none}`}
    ${selBar()}`;
}

function syncPill() {
  const s = state.status;
  if (!s) return '';
  const label = { synced: 'Up to date', syncing: 'Syncing', offline: 'Waiting for the network' }[s.state] || s.state;
  return html`<a class="pill" href="#!sync" data-state="${s.state}">${label}</a>`;
}

function searchView() {
  const rows = state.entries.map(listRow);
  return html`<div class="head"><h1 class="large-title">Search</h1>${phoneQ.matches ? syncPill() : ''}</div>
    ${phoneQ.matches ? html`<div class="searchrow">${searchBox()}</div>` : ''}
    <sol-list class="files">${rows}<div slot="empty" class="empty"><sol-icon name="search"></sol-icon><h3>No match</h3><p>Nothing has a name like that.</p></div></sol-list>`;
}

function conflictsView() {
  const cs = state.conflicts;
  return html`<div class="head"><h1 class="large-title">Conflicts</h1>${phoneQ.matches ? syncPill() : ''}</div>
    <p class="muted">When two of your devices (or you and someone you share with) edit the same file at the same time, both versions are kept. Choose what to keep.</p>
    ${cs.length === 0 ? html`<div class="card empty"><sol-icon name="check"></sol-icon><h3>No conflicts</h3><p>Every file has one version everywhere.</p></div>` :
      cs.map((c) => html`<div class="card conflict"><div><h3>${c.path}</h3></div>
        <div class="versions">${c.versions.map((v) => html`<div class="version">
          <span class="badge ${v.winner ? 'ok' : 'warn'}">${v.winner ? 'Has the name' : 'Other version'}</span>
          <div class="grow"><strong>${v.device || 'a device'}</strong> <small class="muted">${fmtTime(v.mtime)} · ${fmtSize(v.size)}</small></div>
          <button type="button" data-act="preview" data-path="${v.winner ? c.path : v.copy_path}">View</button></div>`)}</div>
        <div class="row-actions">
          <button type="button" class="primary" data-act="resolve" data-keep="this" data-path="${c.path}">Keep this</button>
          <button type="button" data-act="resolve" data-keep="that" data-path="${c.path}">Keep that</button>
          <button type="button" data-act="resolve" data-keep="both" data-path="${c.path}">Keep both</button></div></div>`)}`;
}

function syncView() {
  const s = state.status;
  if (!s) return html`<div class="skeleton tall"></div>`;
  return html`<div class="head"><h1 class="large-title">Sync</h1>${phoneQ.matches ? syncPill() : ''}</div>
    <h2>Devices</h2>
    <sol-list>${s.devices.map((d) => html`<div class="dev" data-item><sol-icon name="${d.anchor ? 'device' : 'phone'}"></sol-icon>
      <div class="grow"><div class="title">${d.name}${d.self ? ' (this device)' : ''}</div>
      <small>${d.entries} changes${d.last_entry ? ' · last ' + new Date(d.last_entry).toLocaleString() : ''}</small></div>
      <span class="badge ${d.online ? 'sun' : ''}">${d.online ? 'Online' : 'Offline'}</span></div>`)}</sol-list>
    <h2 class="gap-above">This device</h2>
    <dl class="kv"><dt>Folder</dt><dd>${s.folder}</dd><dt>Files</dt><dd>${s.files}</dd><dt>Watching</dt><dd>${s.watcher === 'inotify' ? 'live (inotify)' : 'full scan every few seconds'}</dd>
    <dt>Waiting to sync</dt><dd>${s.pending}</dd>${s.last_error ? html`<dt>Last problem</dt><dd>${s.last_error}</dd>` : ''}</dl>
    ${s.skipped.length ? html`<h2 class="group-title gap-above">Not synced</h2><p class="muted">These names or changes cannot be synced everywhere:</p>
      <sol-list>${s.skipped.map((k) => html`<div data-item><div class="grow"><div class="title">${k.path}</div><small>${k.reason}</small></div></div>`)}</sol-list>` : ''}`;
}

function draw() {
  const r = state.route;
  const cc = document.getElementById('conflict-count');
  cc.hidden = state.conflicts.length === 0;
  cc.textContent = state.conflicts.length;
  $shell.setCurrent(r.kind === 'conflicts' ? 'conflicts' : r.kind === 'sync' ? 'sync' : r.kind === 'dir' && (r.path === 'Shared' || r.path.startsWith('Shared/')) ? 'shared' : 'files');
  const active = document.activeElement;
  const hadSearch = active && active.id === 'search';
  if (!state.entries.length && state.selecting) state.selecting = false;
  drawBar();
  render($main, r.kind === 'conflicts' ? conflictsView() : r.kind === 'sync' ? syncView() : r.kind === 'search' ? searchView() : dirView());
  if (hadSearch) { const s = document.getElementById('search'); if (s) { s.focus(); s.setSelectionRange(s.value.length, s.value.length); } }
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

function ask({ title, label, value = '', ok = 'OK' }) {
  return new Promise((resolve) => {
    let done = false;
    const d = dialog(title, html`<form><label>${label}<input name="v" value="${value}" autocomplete="off" required></label>
      <div class="dlg-actions"><button type="button" data-no>Cancel</button><button class="primary" type="submit">${ok}</button></div></form>`);
    const finish = (v) => { if (!done) { done = true; resolve(v); } d.close(); };
    const form = d.box.querySelector('form');
    const input = form.elements.v;
    input.focus();
    input.select();
    form.addEventListener('submit', (e) => { e.preventDefault(); finish(input.value.trim() || null); });
    d.box.querySelector('[data-no]').addEventListener('click', () => finish(null));
    d.el.addEventListener('close', () => finish(null));
  });
}

function confirmBox({ title, body, ok, danger }) {
  return new Promise((resolve) => {
    let done = false;
    const d = dialog(title, html`<p>${body}</p><div class="dlg-actions"><button type="button" data-no>Cancel</button><button class="${danger ? 'danger primary' : 'primary'}" type="button" data-yes>${ok}</button></div>`);
    const finish = (v) => { if (!done) { done = true; resolve(v); } d.close(); };
    d.box.querySelector('[data-yes]').addEventListener('click', () => finish(true));
    d.box.querySelector('[data-no]').addEventListener('click', () => finish(false));
    d.el.addEventListener('close', () => finish(false));
  });
}

async function preview(path) {
  const name = path.split('/').pop();
  const k = kindOf({ type: 'file', name });
  const src = rawURL(path);
  let body;
  if (k === 'image') body = html`<img alt="${name}" src="${src}">`;
  else if (k === 'audio') body = html`<audio controls autoplay src="${src}"></audio>`;
  else if (k === 'video') body = html`<video controls autoplay src="${src}"></video>`;
  else if (k === 'pdf') body = html`<iframe title="${name}" src="${src}"></iframe>`;
  else if (k === 'text') {
    try {
      const res = await fetch(src);
      if (!res.ok) throw new Error((await res.json()).error?.message || res.statusText);
      const text = (await res.text()).slice(0, 200000);
      body = html`<pre>${text}</pre>`;
    } catch (e) { return fail(e); }
  } else body = html`<div class="empty"><sol-icon name="file"></sol-icon><h3>No preview</h3><p>Download it to open it with another program.</p></div>`;
  dialog(name, html`<div class="preview">${body}<a class="btn" href="${rawURL(path, true)}" download="${name}"><sol-icon name="download"></sol-icon>Download</a></div>`);
}

async function shareSheet(path) {
  let ppl, shared;
  try { [ppl, shared] = await Promise.all([people(), call('files.shared', {})]); } catch (e) { return fail(e); }
  const current = new Map(); // person id -> mode
  for (const s of shared.by_me) if (s.path === path) for (const p of s.people) current.set(p.id, s.mode);
  const d = dialog('Share ' + path.split('/').pop(), html`
    <p class="muted">People you share with see this folder on their devices. Folders are online only for them until they choose to keep them.</p>
    ${ppl.length === 0 ? html`<div class="empty"><sol-icon name="people"></sol-icon><h3>No one to share with</h3><p>Pair a friend in the Sol app first.</p></div>` :
      ppl.map((p) => html`<div class="person"><sol-avatar name="${p.name}" id="${p.id}" ${raw(p.online ? 'online' : '')}></sol-avatar>
        <div class="grow"><div class="title">${p.name}</div><small>${p.petname}</small></div>
        <select data-person="${p.id}" aria-label="Access for ${p.name}">
          ${[['', 'Not shared'], ['read', 'Can view'], ['write', 'Can edit']].map(([v, l]) => html`<option value="${v}" ${raw((current.get(p.id) || '') === v ? 'selected' : '')}>${l}</option>`)}
        </select></div>`)}
    <div class="dlg-actions"><button type="button" data-no>Cancel</button><button class="primary" type="button" data-save>Save</button></div>`);
  d.box.querySelector('[data-no]').addEventListener('click', d.close);
  d.box.querySelector('[data-save]').addEventListener('click', async () => {
    try {
      for (const sel of d.box.querySelectorAll('select[data-person]')) {
        const id = sel.dataset.person;
        const was = current.get(id) || '';
        if (sel.value === was) continue;
        if (sel.value === '') await call('files.unshare', { path, people: [id] });
        else await call('files.share', { path, people: [id], mode: sel.value });
      }
      toast('Sharing updated');
      d.close();
      soon();
    } catch (e) { fail(e); }
  });
}

// ---------- actions ----------

async function run(label, fn) {
  try { await fn(); soon(); } catch (e) { fail(e); soon(); }
}

async function upload(files, base) {
  const dir = base ?? state.route.path;
  if (isVirtual(dir) || state.route.kind !== 'dir') return toast('Open one of your folders to upload', { kind: 'error' });
  let n = 0;
  for (const f of files) {
    const rel = f.relPath || f.name;
    try {
      toast(`Uploading ${++n} of ${files.length}: ${f.name}`, { ms: 1500 });
      const b = await putBlob(f);
      await call('files.write', { path: join(dir, rel), blob: b.hash });
    } catch (e) { fail(e); }
  }
  soon();
}

// Dropped folders: walk them with the (widely supported) entry API.
async function collect(dt) {
  const out = [];
  const walk = async (entry, prefix) => {
    if (entry.isFile) {
      const file = await new Promise((ok, no) => entry.file(ok, no));
      file.relPath = prefix + entry.name;
      out.push(file);
    } else if (entry.isDirectory) {
      const rd = entry.createReader();
      for (;;) {
        const batch = await new Promise((ok, no) => rd.readEntries(ok, no));
        if (!batch.length) break;
        for (const c of batch) await walk(c, prefix + entry.name + '/');
      }
    }
  };
  const items = [...(dt.items || [])].map((i) => i.webkitGetAsEntry?.()).filter(Boolean);
  if (items.length) for (const e of items) await walk(e, '');
  else out.push(...dt.files);
  return out;
}

const selected = () => state.entries.filter((e) => state.sel.has(e.path));

const actions = {
  open(p, el) {
    const type = el.closest('[data-type]')?.dataset.type;
    if (type === 'dir') { state.sel.clear(); go(p); } else preview(p);
  },
  pick(p, el) { if (el.checked) state.sel.add(p); else state.sel.delete(p); draw(); },
  toggle(p) { if (state.sel.has(p)) state.sel.delete(p); else state.sel.add(p); draw(); },
  clear() { state.sel.clear(); state.selecting = false; draw(); },
  upload() { document.getElementById('picker').click(); },
  view() { state.view = state.view === 'list' ? 'grid' : 'list'; store.set('files-view', state.view); draw(); },
  async mkdir() {
    const name = await ask({ title: 'New folder', label: 'Name', ok: 'Create' });
    if (name) await run('mkdir', () => call('files.mkdir', { path: join(state.route.path, name) }));
  },
  async rename() {
    const [e] = selected();
    const name = await ask({ title: 'Rename', label: 'New name', value: e.name, ok: 'Rename' });
    if (name && name !== e.name) { state.sel.clear(); await run('rename', () => call('files.move', { from: e.path, to: join(parent(e.path), name) })); }
  },
  async move() {
    const es = selected();
    const dest = await ask({ title: 'Move to', label: 'Folder (empty for My files)', value: state.route.path, ok: 'Move' });
    if (dest === null && !es.length) return;
    if (dest !== null) { state.sel.clear(); await run('move', async () => { for (const e of es) await call('files.move', { from: e.path, to: join(dest.replace(/^\/+|\/+$/g, ''), e.name) }); }); }
  },
  async copy() {
    const es = selected();
    const dest = await ask({ title: 'Copy to', label: 'Folder (empty for My files)', value: state.route.path, ok: 'Copy' });
    if (dest !== null) await run('copy', async () => {
      for (const e of es) {
        const base = join(dest.replace(/^\/+|\/+$/g, ''), e.name);
        const to = dest.replace(/^\/+|\/+$/g, '') === parent(e.path) ? join(parent(e.path), 'Copy of ' + e.name) : base;
        await call('files.copy', { from: e.path, to });
      }
    });
  },
  async delete() {
    const es = selected();
    const what = es.length === 1 ? `"${es[0].name}"` : `${es.length} items`;
    if (await confirmBox({ title: 'Delete', body: `Delete ${what} from all your devices?`, ok: 'Delete', danger: true })) {
      state.sel.clear();
      await run('delete', () => call('files.delete', { paths: es.map((e) => e.path) }));
    }
  },
  download() { const [e] = selected(); const a = document.createElement('a'); a.href = rawURL(e.path, true); a.download = e.name; document.body.append(a); a.click(); a.remove(); },
  share() { const [e] = selected(); shareSheet(e.path); },
  async keep() { await run('keep', () => call('files.keep', { path: state.route.path, keep: !state.here.kept })); },
  async resolve(p, el) { await run('resolve', () => call('files.resolve', { path: p, keep: el.dataset.keep })); toast('Conflict settled'); },
  preview(p) { preview(p); },
};

const rowOf = (ev) => ev.target.closest('[data-item][data-path]');
$main.addEventListener('click', (ev) => {
  const t = ev.target.closest('[data-act]');
  if (t && actions[t.dataset.act]) {
    if (t.dataset.act === 'pick') { actions.pick(t.dataset.path, t); return; }
    ev.preventDefault();
    const p = t.dataset.path;
    const r = actions[t.dataset.act](p, t);
    // Actions on the selection leave select mode once they have read it.
    if (t.closest('.selbar')) { state.selecting = false; Promise.resolve(r).finally(() => { state.sel.clear(); draw(); }); }
    return;
  }
  if (ev.target.closest('a, button, input, select, label')) return;
  const row = rowOf(ev);
  if (!row) return;
  const e = state.entries.find((x) => x.path === row.dataset.path);
  if (state.selecting || ev.ctrlKey || ev.metaKey) {
    if (e && !e.virtual) { state.selecting = true; actions.toggle(row.dataset.path); }
    return;
  }
  actions.open(row.dataset.path, row);
});
document.getElementById('shell').addEventListener('click', (ev) => {
  const b = ev.target.closest('[data-bar]');
  if (!b) return;
  const id = b.dataset.bar;
  if (id === 'done') actions.clear();
  else if (id === 'list' || id === 'grid') { if (state.view !== id) actions.view(); }
  else barMenus[id]?.(b);
});
// Right-click (desktop) or long-press (touch) on a row: the same actions as the selection bar.
contextMenu($main, (ev) => {
  const row = rowOf(ev);
  const e = row && state.entries.find((x) => x.path === row.dataset.path);
  if (!e || e.virtual) return [];
  const items = [{ label: e.type === 'dir' ? 'Open' : 'Preview', icon: 'eye', onSelect: () => actions.open(e.path, row) }];
  const run = (act) => () => { state.sel = new Set([e.path]); const r = actions[act](e.path, row); state.sel.clear(); Promise.resolve(r).finally(draw); };
  for (const a of entryActions([e])) items.push({ label: a.label, icon: a.icon, danger: a.danger, onSelect: run(a.act) });
  items.push({ separator: true }, { label: 'Select', icon: 'check', onSelect: () => { state.selecting = true; state.sel = new Set([e.path]); draw(); } });
  return items;
});
document.getElementById('picker').addEventListener('change', (ev) => { upload([...ev.target.files]); ev.target.value = ''; });
$shell.addEventListener('input', (ev) => { if (ev.target.id === 'search') searchInput(ev.target); });
let searchTimer = null;
$main.addEventListener('input', (ev) => { if (ev.target.id === 'search') searchInput(ev.target); });
function searchInput(input) {
  const q = input.value.trim();
  state.query = input.value;
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => { location.hash = q ? '#!search=' + encodeURIComponent(q) : '#/'; }, 250);
}
document.addEventListener('keydown', (ev) => {
  if (ev.key === 'Escape' && state.selecting && !document.querySelector('sol-dialog[open], .menu')) actions.clear();
  if (ev.key === 'Delete' && state.sel.size && !/INPUT|TEXTAREA|SELECT/.test(document.activeElement.tagName) && !document.querySelector('sol-dialog')) actions.delete();
});

// Drag and drop upload.
let dragDepth = 0;
const hasFiles = (e) => [...(e.dataTransfer?.types || [])].includes('Files');
document.addEventListener('dragenter', (e) => { if (hasFiles(e)) { dragDepth++; $veil.hidden = false; e.preventDefault(); } });
document.addEventListener('dragover', (e) => { if (hasFiles(e)) e.preventDefault(); });
document.addEventListener('dragleave', () => { dragDepth = Math.max(0, dragDepth - 1); if (!dragDepth) $veil.hidden = true; });
document.addEventListener('drop', async (e) => {
  if (!hasFiles(e)) return;
  e.preventDefault();
  dragDepth = 0;
  $veil.hidden = true;
  try { upload(await collect(e.dataTransfer)); } catch (err) { fail(err); }
});

function toggleTheme() {
  const root = document.documentElement;
  const dark = root.getAttribute('data-theme') === 'dark' || (!root.hasAttribute('data-theme') && matchMedia('(prefers-color-scheme: dark)').matches);
  setTheme(dark ? 'light' : 'dark');
}
document.getElementById('theme').addEventListener('click', toggleTheme);
phoneQ.addEventListener('change', draw);

window.addEventListener('hashchange', () => { state.sel.clear(); state.selecting = false; load(); });
events({ people: true }, soon, () => { state.status = state.status && { ...state.status, state: 'offline' }; draw(); });
// Hold the page's load event for the first answer (capped), so the first paint is the real list.
await Promise.race([load(), new Promise((r) => setTimeout(r, 3000))]);
