// Files UI. It only calls actions (sol.call) and listens to events; it never touches the disk,
// feeds or blobs directly, except to hand an uploaded file to the daemon (putBlob) and then ask
// the app to write it (files.write), and to read file content from /raw for previews. Every API
// value reaches the DOM through html`` (escaped). The viewer (viewer.js) lives outside the list.
import { call, events, putBlob, people, html, raw, render } from '/sol.js';
import { toast, showMenu, contextMenu } from '/kit/kit.js';
import { openViewer, openViewerNow, viewKind, fmtSize } from './viewer.js';
import { sendable, sendWhat, statusLabel, orderPeople, personLabel, pendingIn, recentOut } from './send.js';
import { trashWhere, trashWhat, trashWho, goneIn, trashSummary, expiryDelay, hasVersions, versionWho } from './trash.js';

const $main = document.getElementById('main');
const $shell = document.getElementById('shell');
const $veil = document.getElementById('dropveil');

function readTrash() { try { return JSON.parse(localStorage.getItem('files-trash') || 'null'); } catch { return null; } }
const store = {
  get(k, d) { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* optional */ } },
};

const state = {
  route: { kind: 'dir', path: '' },
  view: new URLSearchParams(location.search).get('view') || store.get('files-view', 'list'),
  entries: [],
  here: null, // the current folder (files.list "here")
  sel: new Set(),
  query: '',
  status: null,
  conflicts: [],
  error: '',
  ready: false, // the folder's listing has arrived (or came from the local cache)
  selecting: false, // explicit select mode, like iOS "Select"
  sends: { received: [], sent: [] }, // files people sent me, files I sent (the Shared view)
  trash: readTrash(), // files.trash_list, kept locally so Trash paints at once
};
const phoneQ = matchMedia('(max-width: 760px)');
// The Android app opens files in the system viewer: its WebView cannot show a PDF in a frame.
const opener = window.SolAndroid?.open ? (url, mime, name) => window.SolAndroid.open(url, mime, name) : null;

// ---------- helpers ----------

const join = (d, n) => (d ? d + '/' + n : n);
const parent = (p) => p.slice(0, Math.max(0, p.lastIndexOf('/')));
const trimSlashes = (p) => p.replace(/^\/+|\/+$/g, '');
/** /raw URL of a file; with its blob the URL names one content, so the browser caches it. */
const rawURL = (e, dl) => '/raw?path=' + encodeURIComponent(e.path) + (e.blob ? '&v=' + encodeURIComponent(e.blob) : '') + (dl ? '&download=1' : '');
const fail = (e) => toast(e?.message || String(e), { kind: 'error' });
const isVirtual = (p) => p === 'Shared' || (p.startsWith('Shared/') && p.split('/').length <= 2);
const plural = (n, w) => n + ' ' + w + (n === 1 ? '' : 's');

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
const kindOf = (e) => (e.type === 'dir' ? 'dir' : viewKind(e.name));
const ICON = { dir: 'folder', image: 'image', audio: 'music', video: 'video', pdf: 'file', text: 'file', file: 'file' };

// ---------- routing and loading ----------

function parseHash() {
  const h = decodeURIComponent(location.hash || '#/');
  if (h.startsWith('#!conflicts')) return { kind: 'conflicts' };
  if (h.startsWith('#!sync')) return { kind: 'sync' };
  if (h.startsWith('#!trash')) return { kind: 'trash' };
  if (h.startsWith('#!search=')) return { kind: 'search', q: h.slice(9) };
  return { kind: 'dir', path: h.replace(/^#\/?/, '').replace(/\/+$/, '') };
}
const hashOf = (p) => '#/' + p.split('/').filter(Boolean).map(encodeURIComponent).join('/');
const go = (p) => { location.hash = hashOf(p); };

let loadSeq = 0;
// The last listing per folder is kept locally so the folder paints at once on the next visit;
// the live answer replaces it a moment later (this is local data, so no skeletons).
const cacheKey = (p) => 'files-cache:' + p;
function readCache(p) { try { return JSON.parse(localStorage.getItem(cacheKey(p)) || 'null'); } catch { return null; } }
function writeCache(p, entries, here) { store.set(cacheKey(p), JSON.stringify({ entries: entries.slice(0, 300), here })); }

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
  // Trash paints at once from the copy kept in the page; the live list follows.
  if (r.kind === 'trash') { state.entries = []; state.error = ''; draw(); }
  if (r.kind !== 'search') state.query = '';
  try {
    const main = r.kind === 'dir' ? call('files.list', { path: r.path })
      : r.kind === 'search' ? call('files.search', { query: r.q })
        : r.kind === 'trash' ? call('files.trash_list', {}) : null;
    const sends = r.kind === 'dir' && r.path === 'Shared' ? Promise.all([call('files.received', {}), call('files.sent', {})]) : null;
    const [status, c, got, sd] = await Promise.all([call('files.status', {}), call('files.conflicts', {}), main, sends]);
    if (seq !== loadSeq) return;
    if (sd) state.sends = { received: sd[0].received, sent: sd[1].sent };
    state.status = status;
    state.conflicts = c.conflicts;
    if (r.kind === 'dir') {
      state.entries = got.entries;
      state.here = got.here;
      writeCache(r.path, got.entries, got.here);
    } else if (r.kind === 'search') {
      state.query = r.q;
      state.entries = got.results;
      state.here = null;
    } else if (r.kind === 'trash') {
      state.trash = got;
      state.entries = [];
      store.set('files-trash', JSON.stringify({ ...got, items: got.items.slice(0, 200) }));
      armExpiry();
    }
    state.error = '';
    // Keep only selections that still exist.
    const live = new Set(state.entries.map((e) => e.path));
    state.sel = new Set([...state.sel].filter((p) => live.has(p)));
  } catch (e) {
    if (seq !== loadSeq) return;
    if (r.kind === 'dir' && r.path && e.code === 'bad_request' && (await openLink(r.path, seq))) return;
    state.error = e.code === 'not_found' ? 'That folder is not here (any more).' : e.message;
    state.entries = [];
  }
  state.ready = true;
  draw();
  painted();
}
let painted;
const firstPaint = new Promise((r) => { painted = r; });

// A link to a file (#/Photos/dusk.png) shows its folder with the file open in the viewer;
// closing the viewer leaves the folder's address behind.
async function openLink(path, seq) {
  try {
    if ((await call('files.stat', { path })).type !== 'file') return false;
  } catch { return false; }
  if (seq !== loadSeq) return true;
  const dir = parent(path);
  history.replaceState(null, '', hashOf(dir));
  await load();
  preview(path, undefined, { history: false });
  return true;
}

let timer = null;
const soon = () => { clearTimeout(timer); timer = setTimeout(load, 120); };
// Trash changes by itself only when something expires: one timer to that moment, no polling.
let expiryTimer = null;
function armExpiry() {
  clearTimeout(expiryTimer);
  const d = expiryDelay(state.trash);
  if (d !== null) expiryTimer = setTimeout(() => { if (state.route.kind === 'trash') soon(); }, d);
}

// ---------- drawing ----------

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
  return html`${e.conflict ? html`<span class="badge warn">${e.copy ? 'Conflict copy' : 'Conflict'}</span>` : ''}${e.shared ? html`<span class="badge sun">Shared</span>` : ''}${e.read_only && !e.virtual ? html`<span class="badge">Read only</span>` : ''}${e.type === 'file' && !e.local ? html`<sol-icon class="cloud" name="globe" title="Online only: fetched when opened" aria-label="Online only"></sol-icon>` : ''}`;
}

const sizeCell = (e) => (e.type === 'dir' ? (e.items != null ? plural(e.items, 'item') : '') : fmtSize(e.size));
const subLine = (e) => [e.type === 'file' ? fmtTime(e.mtime) : '', sizeCell(e)].filter(Boolean).join(' · ');
const pickBox = (e, sel) => (state.selecting
  ? html`<input type="checkbox" class="pick" data-act="pick" data-path="${e.path}" aria-label="Select ${e.name}" ${raw(sel ? 'checked' : '')} ${raw(e.virtual ? 'disabled' : '')}>` : '');
const glyph = (e, k) => (k === 'image' && !e.virtual
  ? html`<img class="thumb" loading="lazy" alt="" src="${rawURL(e)}">`
  : html`<sol-icon class="ico ${e.type}" name="${ICON[k]}"></sol-icon>`);

function listRow(e) {
  const sel = state.sel.has(e.path);
  return html`<div data-item class="frow ${sel ? 'sel' : ''}" ${raw(e.type === 'dir' && !state.selecting ? 'data-chevron' : '')} data-path="${e.path}" data-type="${e.type}" tabindex="0">
    ${pickBox(e, sel)}
    <span class="glyph">${glyph(e, kindOf(e))}</span>
    <div class="grow name"><div class="line"><span class="title">${e.name}</span>${badges(e)}</div><span class="row-sub">${state.route.kind === 'search' ? parent(e.path) || 'My files' : subLine(e)}</span></div>
    <span class="cell-mtime">${e.type === 'file' ? fmtTime(e.mtime) : ''}</span>
    <span class="cell-size">${sizeCell(e)}</span>
  </div>`;
}

function tile(e) {
  const sel = state.sel.has(e.path);
  return html`<div class="tile ${sel ? 'sel' : ''}" data-item data-path="${e.path}" data-type="${e.type}" tabindex="0">
    ${pickBox(e, sel)}
    <span class="art">${glyph(e, kindOf(e))}</span>
    <span class="title">${e.name}</span>
    <small>${e.type === 'dir' ? sizeCell(e) : subLine(e)}</small>
  </div>`;
}

const writable = (es) => !es.some((e) => e.read_only || e.virtual);
/** What can be done with these entries: the one list behind the selection bar and context menus. */
function entryActions(es) {
  const one = es.length === 1 ? es[0] : null;
  const w = writable(es);
  return [
    one && one.type === 'file' && { act: 'download', icon: 'download', label: 'Download' },
    one && w && { act: 'rename', icon: 'edit', label: 'Rename' },
    one && hasVersions(one) && { act: 'versions', icon: 'clock', label: 'Versions' },
    one && one.type === 'dir' && w && !one.path.startsWith('Shared/') && { act: 'share', icon: 'share', label: 'Share' },
    sendable(es) && { act: 'send', icon: 'send', label: 'Send to…' },
    w && { act: 'move', icon: 'folder', label: 'Move' },
    !es.some((e) => e.virtual) && { act: 'copy', icon: 'copy', label: 'Copy' },
    w && { act: 'delete', icon: 'trash', label: 'Delete', danger: true },
  ].filter(Boolean);
}

function selBar() {
  const es = selected();
  if (!state.selecting || !es.length) return '';
  return html`<div class="selbar glass" role="toolbar" aria-label="Selection">${entryActions(es).map((a) => html`<button type="button" class="${a.danger ? 'danger' : 'ghost'}" data-act="${a.act}"><sol-icon name="${a.icon}"></sol-icon><span>${a.label}</span></button>`)}</div>`;
}

const canWriteHere = () => state.route.kind === 'dir' && !isVirtual(state.route.path) && !(state.here && state.here.read_only);

const searchBox = () => html`<input class="search" type="search" id="search" placeholder="Search" aria-label="Search names" value="${state.query}">`;

// The window's toolbar: sync pill, view switch, search, + menu and ... menu.
function drawBar() {
  const phone = phoneQ.matches;
  const r = state.route;
  if (state.selecting) {
    render($shell.actions, html`<button type="button" class="plain" data-bar="done">Done</button>`);
    return;
  }
  const menuBtn = (id, icon, label) => html`<button type="button" class="ghost icon-btn" data-bar="${id}" aria-label="${label}" aria-haspopup="menu"><sol-icon name="${icon}"></sol-icon></button>`;
  const listing = r.kind === 'dir' || r.kind === 'search';
  render($shell.actions, html`
    ${phone ? '' : html`<span class="bar-pill">${syncPill()}</span>
      ${listing ? html`<div class="segmented" role="group" aria-label="View"><button type="button" class="icon-btn" data-bar="list" aria-label="List" aria-pressed="${state.view === 'list'}"><sol-icon name="list"></sol-icon></button><button type="button" class="icon-btn" data-bar="grid" aria-label="Grid" aria-pressed="${state.view === 'grid'}"><sol-icon name="grid"></sol-icon></button></div>` : ''}
      ${searchBox()}`}
    ${canWriteHere() ? menuBtn('add', 'plus', 'Add') : ''}
    ${listing ? menuBtn('more', 'more', 'More') : ''}`);
}

const barMenus = {
  add: (el) => showMenu([
    { label: 'Upload files', icon: 'upload', onSelect: () => actions.upload() },
    { label: 'New folder', icon: 'folder-plus', onSelect: () => actions.mkdir() },
  ], { anchor: el, placement: 'bottom-end' }),
  more(el) {
    const here = state.here;
    const mine = here && state.route.kind === 'dir' && state.route.path && !isVirtual(state.route.path);
    showMenu([
      { label: 'Select', icon: 'check', disabled: !state.entries.some((e) => !e.virtual), onSelect: () => { state.selecting = true; draw(); } },
      { label: state.view === 'list' ? 'View as grid' : 'View as list', icon: state.view === 'list' ? 'grid' : 'list', onSelect: () => actions.view() },
      mine && { separator: true },
      mine && { label: here.kept ? 'Make online only' : 'Keep on this device', icon: here.kept ? 'globe' : 'download', onSelect: () => actions.keep() },
      mine && !here.read_only && !state.route.path.startsWith('Shared/') && { label: 'Share folder', icon: 'share', onSelect: () => shareSheet(state.route.path) },
    ].filter(Boolean), { anchor: el, placement: 'bottom-end' });
  },
};

function folderNote() {
  const h = state.here;
  if (!h || !state.route.path || isVirtual(state.route.path)) return '';
  return html`<p class="folder-note">${h.kept ? 'Kept on this device' : 'Online only: files are fetched when you open them'}${h.shared ? ' · Shared' : ''}${h.read_only ? ' · Read only' : ''}</p>`;
}

function dirView() {
  const r = state.route;
  const sel = state.selecting ? state.sel.size : 0;
  const title = state.selecting ? (sel ? sel + ' selected' : 'Select items') : r.path ? r.path.split('/').pop() : 'My files';
  const empty = r.path === 'Shared'
    ? html`<div slot="empty" class="empty"><sol-icon name="people"></sol-icon><h3>Nothing shared with you yet</h3><p>Folders people share with you show up here.</p></div>`
    : html`<div slot="empty" class="empty"><sol-icon name="folder"></sol-icon><h3>This folder is empty</h3><p>${isVirtual(r.path) ? 'Open one of the folders inside.' : canWriteHere() ? (phoneQ.matches ? 'Tap + to add files.' : 'Drop files here or use + to add some.') : ''}</p></div>`;
  const rows = state.entries.map(state.view === 'list' ? listRow : tile);
  const none = state.ready ? empty : '';
  return html`${state.selecting ? '' : crumbs(r.path)}
    <div class="head"><h1 class="large-title">${title}</h1></div>
    ${phoneQ.matches && !state.selecting ? html`<div class="searchrow">${searchBox()}${phonePill()}</div>` : ''}
    ${folderNote()}
    ${r.path === 'Shared' && !state.selecting ? sendsView() : ''}
    ${state.error ? html`<div class="card empty"><sol-icon name="alert"></sol-icon><h3>${state.error}</h3></div>` :
      state.view === 'list'
        ? html`<sol-list class="files" aria-label="Files">${rows}${none}</sol-list>`
        : html`<div class="grid" role="list">${rows}</div>${state.entries.length ? '' : none}`}
    ${selBar()}`;
}

/** Incoming sends waiting for an answer, and a short list of what I sent (top of Shared). */
function sendsView() {
  const inc = pendingIn(state.sends.received);
  const out = recentOut(state.sends.sent);
  if (!inc.length && !out.length) return '';
  return html`${inc.map((r) => html`<div class="card incoming" data-send="${r.send}">
      <div class="incoming-head"><sol-icon name="send"></sol-icon>
        <div class="grow"><div class="title">${r.from_name} sent you ${sendWhat(r.items)}</div>
        <small>${fmtSize(r.bytes)} · ${fmtTime(r.time)}</small></div></div>
      ${r.note ? html`<p class="incoming-note">${r.note}</p>` : ''}
      <ul class="incoming-files">${r.items.slice(0, 5).map((it) => html`<li>${it.name} <small>${fmtSize(it.size)}</small></li>`)}${r.items.length > 5 ? html`<li><small>and ${r.items.length - 5} more</small></li>` : ''}</ul>
      <div class="row-actions"><button type="button" class="primary" data-act="receive" data-send="${r.send}" data-accept="1">Save</button>
        <button type="button" data-act="receive" data-send="${r.send}" data-accept="0">Decline</button></div></div>`)}
    ${out.length ? html`<h2 class="sent-head">Sent</h2>
      <sol-list class="sent">${out.map((o) => html`<div data-item><sol-icon name="send"></sol-icon>
        <div class="grow"><div class="title">${sendWhat(o.items)} to ${o.to_name}</div><small>${fmtTime(o.time)}</small></div>
        <span class="badge ${o.status === 'saved' ? 'ok' : o.status === 'declined' ? 'warn' : ''}">${statusLabel(o.status)}</span></div>`)}</sol-list>` : ''}`;
}

// On a phone the pill only shows when something needs attention; "Up to date" is the norm.
const phonePill = () => (phoneQ.matches && state.status && state.status.state !== 'synced' ? syncPill() : '');

function syncPill() {
  const s = state.status;
  if (!s) return '';
  const label = { synced: 'Up to date', syncing: 'Syncing', offline: 'Waiting for the network' }[s.state] || s.state;
  return html`<a class="pill" href="#!sync" data-state="${s.state}">${label}</a>`;
}

function searchView() {
  const rows = state.entries.map(state.view === 'list' ? listRow : tile);
  const none = html`<div slot="empty" class="empty"><sol-icon name="search"></sol-icon><h3>No match</h3><p>Nothing has a name like that.</p></div>`;
  return html`<div class="head"><h1 class="large-title">Search</h1></div>
    ${phoneQ.matches ? html`<div class="searchrow">${searchBox()}${phonePill()}</div>` : ''}
    ${state.view === 'list' ? html`<sol-list class="files">${rows}${none}</sol-list>` : html`<div class="grid" role="list">${rows}</div>${state.entries.length ? '' : none}`}
    ${selBar()}`;
}

function conflictsView() {
  const cs = state.conflicts;
  return html`<div class="head"><h1 class="large-title">Conflicts</h1></div>
    <p class="muted">When two of your devices (or you and someone you share with) edit the same file at the same time, both versions are kept. Choose what to keep.</p>
    ${cs.length === 0 ? html`<div class="card empty"><sol-icon name="check"></sol-icon><h3>No conflicts</h3><p>Every file has one version everywhere.</p></div>` :
      cs.map((c) => html`<div class="card conflict"><h3>${c.path}</h3>
        <div class="versions">${c.versions.map((v) => html`<div class="version">
          <span class="badge ${v.winner ? 'ok' : 'warn'}">${v.winner ? 'Has the name' : 'Other version'}</span>
          <div class="grow"><strong>${v.device || 'a device'}</strong> <small class="muted">${fmtTime(v.mtime)} · ${fmtSize(v.size)}</small></div>
          <button type="button" data-act="preview" data-path="${v.winner ? c.path : v.copy_path}" data-size="${v.size}">View</button></div>`)}</div>
        <div class="row-actions">
          <button type="button" class="primary" data-act="resolve" data-keep="this" data-path="${c.path}">Keep this</button>
          <button type="button" data-act="resolve" data-keep="that" data-path="${c.path}">Keep that</button>
          <button type="button" data-act="resolve" data-keep="both" data-path="${c.path}">Keep both</button></div></div>`)}`;
}

function syncView() {
  const s = state.status;
  const head = html`<div class="head"><h1 class="large-title">Sync</h1></div>`;
  if (!s) return head;
  return html`${head}
    <div class="sync-now"><p class="muted">${{ synced: 'Everything is up to date.', syncing: 'Syncing changes…', offline: 'Waiting for the network: changes are kept and sent later.' }[s.state] || ''}</p>
      <button type="button" data-act="syncnow"><sol-icon name="refresh"></sol-icon>Sync now</button></div>
    <h2>Devices</h2>
    <sol-list>${s.devices.map((d) => html`<div class="dev" data-item><sol-icon name="${d.anchor ? 'device' : 'phone'}"></sol-icon>
      <div class="grow"><div class="title">${d.name}${d.self ? ' (this device)' : ''}</div>
      <small>${plural(d.entries || 0, 'change')}${d.last_entry ? ' · last ' + fmtTime(Date.parse(d.last_entry)) : ''}</small></div>
      <span class="badge ${d.online ? 'sun' : ''}">${d.online ? 'Online' : 'Offline'}</span></div>`)}</sol-list>
    <h2 class="gap-above">This device</h2>
    <dl class="kv"><dt>Folder</dt><dd>${s.folder}</dd><dt>Files</dt><dd>${s.files}</dd><dt>Watching</dt><dd>${s.watcher === 'inotify' ? 'live' : 'a full scan every few seconds'}</dd>
    <dt>Waiting to sync</dt><dd>${s.pending}</dd>${s.last_error ? html`<dt>Last problem</dt><dd>${s.last_error}</dd>` : ''}</dl>
    ${s.skipped.length ? html`<h2 class="gap-above">Not synced</h2><p class="muted">These names or changes cannot be synced everywhere:</p>
      <sol-list>${s.skipped.map((k) => html`<div data-item><div class="grow"><div class="title">${k.path}</div><small>${k.reason}</small></div></div>`)}</sol-list>` : ''}`;
}

const trashIcon = (r) => (r.dir ? 'folder' : ICON[viewKind(r.name)] || 'file');

function trashView() {
  const t = state.trash;
  const rows = t ? t.items : [];
  const days = t ? t.days : 30;
  return html`<div class="head"><h1 class="large-title">Trash</h1></div>
    <div class="trash-top"><p class="muted">Deleted files stay here for ${plural(days, 'day')}, the same on all your devices, wherever they were deleted (in Files, or on disk by any program). ${t ? trashSummary(t, fmtSize) : ''}</p>
      ${rows.length ? html`<button type="button" class="danger" data-act="trashEmpty"><sol-icon name="trash"></sol-icon>Empty trash</button>` : ''}</div>
    ${state.error ? html`<div class="card empty"><sol-icon name="alert"></sol-icon><h3>${state.error}</h3></div>` :
      html`<sol-list class="files trash" aria-label="Deleted items">${rows.map((r) => html`<div data-item class="frow trow" data-id="${r.id}">
        <span class="glyph"><sol-icon class="ico ${r.dir ? 'dir' : 'file'}" name="${trashIcon(r)}"></sol-icon></span>
        <div class="grow name"><div class="line"><span class="title">${r.name}</span>${r.dir ? html`<span class="badge">${trashWhat(r)}</span>` : ''}</div>
          <span class="row-sub">${trashWhere(r)} · ${fmtTime(r.deleted_at)} · ${trashWho(r)} · ${fmtSize(r.size)} · ${goneIn(r.expires_at)}</span></div>
        <div class="row-actions"><button type="button" data-act="trashRestore" data-id="${r.id}">Restore</button>
          <button type="button" class="ghost danger" data-act="trashPurge" data-id="${r.id}" aria-label="Delete ${r.name} forever">Delete forever</button></div>
      </div>`)}${state.ready || t ? html`<div slot="empty" class="empty"><sol-icon name="trash"></sol-icon><h3>Trash is empty</h3><p>Deleted files show up here.</p></div>` : ''}</sol-list>`}`;
}

function draw() {
  const r = state.route;
  const cc = document.getElementById('conflict-count');
  cc.hidden = state.conflicts.length === 0;
  cc.textContent = state.conflicts.length;
  $shell.setCurrent(r.kind === 'conflicts' ? 'conflicts' : r.kind === 'sync' ? 'sync' : r.kind === 'trash' ? 'trash' : r.kind === 'dir' && (r.path === 'Shared' || r.path.startsWith('Shared/')) ? 'shared' : 'files');
  const active = document.activeElement;
  const hadSearch = active && active.id === 'search';
  if (!state.entries.length && state.selecting) state.selecting = false;
  drawBar();
  render($main, r.kind === 'conflicts' ? conflictsView() : r.kind === 'sync' ? syncView() : r.kind === 'trash' ? trashView() : r.kind === 'search' ? searchView() : dirView());
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
    // Select the name without its extension, like a file manager does.
    const dot = value.lastIndexOf('.');
    input.setSelectionRange(0, dot > 0 ? dot : value.length);
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

/** A folder chooser for Move and Copy: browse my writable folders, then pick one. */
function pickFolder({ title, ok, start, exclude }) {
  return new Promise((resolve) => {
    let done = false, at = isVirtual(start) || start.startsWith('Shared/') ? '' : start;
    const d = dialog(title, html`<div class="picker"></div>`);
    const box = d.box.querySelector('.picker');
    const finish = (v) => { if (!done) { done = true; resolve(v); } d.close(); };
    const show = async () => {
      let dirs = [];
      try { dirs = (await call('files.list', { path: at })).entries.filter((e) => e.type === 'dir' && !e.virtual && !e.read_only && !exclude.some((x) => x === e.path || e.path.startsWith(x + '/'))); } catch (e) { fail(e); }
      render(box, html`<div class="pick-at">${at ? html`<button type="button" class="ghost icon-btn" data-up aria-label="Up"><sol-icon name="back"></sol-icon></button>` : ''}<strong>${at ? at.split('/').pop() : 'My files'}</strong></div>
        <sol-list class="pick-list">${dirs.map((e) => html`<div data-item data-chevron data-path="${e.path}"><sol-icon class="ico dir" name="folder"></sol-icon><div class="grow">${e.name}</div></div>`)}<div slot="empty" class="muted pick-none">No folders inside</div></sol-list>
        <div class="dlg-actions"><button type="button" data-no>Cancel</button><button class="primary" type="button" data-yes>${ok}</button></div>`);
    };
    box.addEventListener('click', (e) => {
      if (e.target.closest('[data-no]')) finish(null);
      else if (e.target.closest('[data-yes]')) finish(at);
      else if (e.target.closest('[data-up]')) { at = parent(at); show(); } else {
        const row = e.target.closest('[data-path]');
        if (row) { at = row.dataset.path; show(); }
      }
    });
    d.el.addEventListener('close', () => finish(null));
    show();
  });
}

async function shareSheet(path) {
  let ppl, shared;
  try { [ppl, shared] = await Promise.all([people(), call('files.shared', {})]); } catch (e) { return fail(e); }
  const current = new Map(); // person id -> mode
  for (const s of shared.by_me) if (s.path === path) for (const p of s.people) current.set(p.id, s.mode);
  const d = dialog('Share ' + path.split('/').pop(), html`
    <p class="muted">People you share with see this folder on their devices, under Shared.</p>
    ${ppl.length === 0 ? html`<div class="empty"><sol-icon name="people"></sol-icon><h3>No one to share with</h3><p>Pair with someone in Sol first.</p></div>` :
      ppl.map((p) => html`<div class="person"><sol-avatar name="${p.name}" id="${p.id}" ${raw(p.online ? 'online' : '')}></sol-avatar>
        <div class="grow"><div class="title">${p.name}</div><small>${p.petname}</small></div>
        <select data-person="${p.id}" aria-label="Access for ${p.name}">
          ${[['', 'Not shared'], ['read', 'Can view'], ['write', 'Can edit']].map(([v, l]) => html`<option value="${v}" ${raw((current.get(p.id) || '') === v ? 'selected' : '')}>${l}</option>`)}
        </select></div>`)}
    <div class="dlg-actions"><button type="button" data-no>Cancel</button>${ppl.length ? html`<button class="primary" type="button" data-save>Save</button>` : ''}</div>`);
  d.box.querySelector('[data-no]').addEventListener('click', d.close);
  d.box.querySelector('[data-save]')?.addEventListener('click', async () => {
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

/** "Send to…": pick a contact, add a note, send these files (a copy goes to their Files). */
async function sendSheet(es) {
  let ppl;
  try { ppl = orderPeople(await people()); } catch (e) { return fail(e); }
  const d = dialog('Send ' + sendWhat(es), html`
    <p class="muted">They are asked first. If they accept, the files are saved on their devices in Received.</p>
    ${ppl.length === 0 ? html`<div class="empty"><sol-icon name="people"></sol-icon><h3>No one to send to</h3><p>Pair with someone in Sol first.</p></div>` : html`
      <div class="people" role="radiogroup" aria-label="Send to">${ppl.map((p) => html`<label class="person pick"><sol-avatar name="${p.name}" id="${p.id}" ${raw(p.online ? 'online' : '')}></sol-avatar>
        <div class="grow"><div class="title">${personLabel(p)}</div><small>${p.online ? 'Online' : 'Offline: they get it when they are back'}</small></div>
        <input type="radio" name="to" value="${p.id}" aria-label="${personLabel(p)}"></label>`)}</div>
      <label class="field">Note (optional)<input type="text" id="send-note" maxlength="500" placeholder="Say what it is"></label>`}
    <div class="dlg-actions"><button type="button" data-no>Cancel</button>${ppl.length ? html`<button class="primary" type="button" data-go disabled>Send</button>` : ''}</div>`);
  const go = d.box.querySelector('[data-go]');
  d.box.querySelector('[data-no]').addEventListener('click', d.close);
  d.box.addEventListener('change', (ev) => { if (ev.target.name === 'to' && go) go.disabled = false; });
  go?.addEventListener('click', async () => {
    const id = d.box.querySelector('input[name=to]:checked')?.value;
    if (!id) return;
    go.disabled = true;
    try {
      const out = await call('files.send', { paths: es.map((e) => e.path), person: id, note: d.box.querySelector('#send-note').value.trim() });
      toast('Sent to ' + out.to_name);
      d.close();
      soon();
    } catch (e) { go.disabled = false; fail(e); }
  });
}

/** A toast with an Undo button (the kit's toast takes text; the button is added here). */
function undoToast(message, undo) {
  const t = toast(message, { ms: 6000 });
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'plain undo';
  b.textContent = 'Undo';
  b.addEventListener('click', async () => { b.disabled = true; t.dataset.leaving = ''; try { await undo(); } catch (e) { fail(e); } soon(); }, { once: true });
  t.append(b);
}

// ---------- the viewer ----------

const viewerActions = [
  { act: 'download', icon: 'download', label: 'Download' },
  { act: 'send', icon: 'send', label: 'Send to…', when: (e) => sendable([e]) },
  { act: 'share', icon: 'share', label: 'Share', when: () => canShareFiles() },
  { act: 'rename', icon: 'edit', label: 'Rename', when: (e) => writable([e]) },
  { act: 'versions', icon: 'clock', label: 'Versions', when: (e) => hasVersions(e) },
  { act: 'delete', icon: 'trash', label: 'Delete', danger: true, when: (e) => writable([e]) },
];
const canShareFiles = () => { try { return !!navigator.canShare?.({ files: [new File([''], 'x.txt', { type: 'text/plain' })] }); } catch { return false; } };

/** Opens the viewer on one file, moving through the files of the list it was opened from. */
function preview(path, size, more = {}) {
  const files = state.entries.filter((e) => e.type === 'file');
  let i = files.findIndex((e) => e.path === path);
  const list = i >= 0 ? files : [{ path, name: path.split('/').pop(), size: Number(size) || undefined, type: 'file' }];
  if (i < 0) i = 0;
  openViewer(list, i, {
    src: (e) => rawURL(e), download: (e) => rawURL(e, true), opener, actions: viewerActions,
    onAction: (act, e, v) => viewerAct(act, e, v),
    onClose: () => soon(),
    ...more,
  });
}

async function viewerAct(act, e, v) {
  if (act === 'download') return saveFile(e);
  if (act === 'send') return sendSheet([e]);
  if (act === 'versions') return versionsSheet(e);
  if (act === 'open-failed') return toast('Could not open it here; try Download', { kind: 'error' });
  if (act === 'share') {
    try {
      const blob = await (await fetch(rawURL(e))).blob();
      await navigator.share({ files: [new File([blob], e.name, { type: blob.type })], title: e.name });
    } catch (err) { if (err.name !== 'AbortError') fail(err); }
    return;
  }
  if (act === 'rename') {
    const name = await ask({ title: 'Rename', label: 'New name', value: e.name, ok: 'Rename' });
    if (!name || name === e.name) return;
    const to = join(parent(e.path), name);
    try { await call('files.move', { from: e.path, to }); v.replace(v.index, { ...e, name, path: to }); } catch (err) { fail(err); }
    return;
  }
  if (act === 'delete') {
    try { await removeFiles([e]); v.remove(v.index); } catch (err) { fail(err); }
  }
}

function saveFile(e, url = rawURL(e, true)) {
  const a = document.createElement('a');
  a.href = url;
  a.download = e.name;
  document.body.append(a);
  a.click();
  a.remove();
}

/** Deletes entries into Trash; the toast's Undo restores them from there (folders too). */
async function removeFiles(es) {
  const out = await call('files.delete', { paths: es.map((e) => e.path) });
  const what = es.length === 1 ? es[0].name : plural(es.length, 'item');
  const ids = out.trash || [];
  // Things deleted in a folder someone shares with me go to its owner's Trash, not mine.
  if (ids.length) undoToast('Moved ' + what + ' to Trash', () => call('files.trash_restore', { ids }));
  else toast('Deleted ' + what);
  soon();
}

/** The Versions sheet of one file: earlier contents with Download, Restore and Delete. */
async function versionsSheet(e) {
  const d = dialog('Versions of ' + e.name, html`<div class="versions-box"></div>`);
  const box = d.box.querySelector('.versions-box');
  let list = null;
  const show = () => render(box, html`
    <p class="muted">Earlier contents of this file, kept when an edit, another device or a merge replaced them${state.trash ? ' (' + plural(state.trash.days, 'day') + ', at most ' + state.trash.versions + ')' : ''}.</p>
    ${list && list.current ? html`<div class="vrow current"><sol-icon name="file"></sol-icon><div class="grow"><strong>Current</strong> <small class="muted">${fmtTime(list.current.mtime)} · ${fmtSize(list.current.size)}</small></div></div>` : ''}
    <sol-list class="vlist">${(list ? list.versions : []).map((v) => html`<div data-item class="vrow" data-id="${v.id}">
      <sol-icon name="clock"></sol-icon>
      <div class="grow"><div class="title">${fmtTime(v.mtime)}</div><small class="muted">${versionWho(v)} ${fmtTime(v.replaced_at)} · ${fmtSize(v.size)}</small></div>
      <div class="row-actions"><button type="button" class="ghost icon-btn" data-vact="download" data-blob="${v.blob}" aria-label="Download this version"><sol-icon name="download"></sol-icon></button>
        <button type="button" data-vact="restore" data-id="${v.id}">Restore</button>
        <button type="button" class="ghost danger" data-vact="delete" data-id="${v.id}" aria-label="Delete this version">Delete</button></div></div>`)}
      ${list ? html`<div slot="empty" class="muted pick-none">No earlier versions</div>` : ''}</sol-list>
    <div class="dlg-actions"><button type="button" data-no>Close</button></div>`);
  const refresh = async () => {
    try { list = await call('files.versions', { path: e.path }); } catch (err) { fail(err); list = { versions: [] }; }
    show();
  };
  box.addEventListener('click', async (ev) => {
    if (ev.target.closest('[data-no]')) return d.close();
    const b = ev.target.closest('[data-vact]');
    if (!b) return;
    if (b.dataset.vact === 'download') return saveFile({ name: e.name }, versionURL(e.path, b.dataset.blob));
    b.disabled = true;
    try {
      if (b.dataset.vact === 'restore') {
        await call('files.version_restore', { path: e.path, id: b.dataset.id });
        toast('Version restored');
      } else {
        if (!(await confirmBox({ title: 'Delete version', body: 'Delete this version forever, on all your devices?', ok: 'Delete', danger: true }))) { b.disabled = false; return; }
        await call('files.version_delete', { path: e.path, ids: [b.dataset.id] });
      }
      await refresh();
      soon();
    } catch (err) { b.disabled = false; fail(err); }
  });
  show();
  refresh();
}
const versionURL = (p, blob) => '/raw?path=' + encodeURIComponent(p) + '&blob=' + encodeURIComponent(blob) + '&download=1';

// ---------- actions ----------

async function run(fn) {
  try { await fn(); } catch (e) { fail(e); }
  soon();
}

async function upload(files, base) {
  const dir = base ?? state.route.path;
  if (state.route.kind !== 'dir' || !canWriteHere()) return toast('Open one of your folders to upload', { kind: 'error' });
  if (!files.length) return;
  const t = toast(files.length === 1 ? 'Uploading ' + files[0].name : 'Uploading ' + plural(files.length, 'file'), { kind: 'info', ms: 600000 });
  let ok = 0;
  for (const f of files) {
    try {
      const b = await putBlob(f);
      await call('files.write', { path: join(dir, f.relPath || f.name), blob: b.hash });
      ok++;
      soon();
    } catch (e) { fail(e); }
  }
  t.remove();
  if (ok) toast(ok === 1 ? 'Uploaded ' + (files.length === 1 ? files[0].name : '1 file') : 'Uploaded ' + plural(ok, 'file'));
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
    if (name) await run(() => call('files.mkdir', { path: join(state.route.path, name) }));
  },
  async rename() {
    const [e] = selected();
    const name = await ask({ title: 'Rename', label: 'New name', value: e.name, ok: 'Rename' });
    if (name && name !== e.name) { state.sel.clear(); await run(() => call('files.move', { from: e.path, to: join(parent(e.path), name) })); }
  },
  async move() {
    const es = selected();
    const dest = await pickFolder({ title: es.length === 1 ? 'Move ' + es[0].name : 'Move ' + plural(es.length, 'item'), ok: 'Move here', start: state.route.kind === 'dir' ? state.route.path : '', exclude: es.filter((e) => e.type === 'dir').map((e) => e.path) });
    if (dest === null) return;
    state.sel.clear();
    await run(async () => {
      for (const e of es) if (parent(e.path) !== dest) await call('files.move', { from: e.path, to: join(dest, e.name) });
      toast('Moved to ' + (dest || 'My files'));
    });
  },
  async copy() {
    const es = selected();
    const dest = await pickFolder({ title: es.length === 1 ? 'Copy ' + es[0].name : 'Copy ' + plural(es.length, 'item'), ok: 'Copy here', start: state.route.kind === 'dir' ? state.route.path : '', exclude: es.filter((e) => e.type === 'dir').map((e) => e.path) });
    if (dest === null) return;
    await run(async () => {
      for (const e of es) await call('files.copy', { from: e.path, to: join(dest, parent(e.path) === trimSlashes(dest) ? 'Copy of ' + e.name : e.name) });
    });
  },
  async delete() {
    const es = selected();
    if (!es.length) return;
    const dirs = es.filter((e) => e.type === 'dir');
    if (dirs.length) {
      const what = es.length === 1 ? `the folder "${es[0].name}" and everything in it` : plural(es.length, 'item') + ', folders with everything in them';
      if (!(await confirmBox({ title: 'Delete', body: `Delete ${what} from all your devices? It stays in Trash for a while.`, ok: 'Delete', danger: true }))) return;
    }
    state.sel.clear();
    await run(() => removeFiles(es));
  },
  download() { const [e] = selected(); saveFile(e); },
  share() { const [e] = selected(); shareSheet(e.path); },
  send() { const es = selected(); if (sendable(es)) return sendSheet(es); },
  async receive(p, el) {
    el.disabled = true;
    try {
      const accept = el.dataset.accept === '1';
      const r = await call('files.receive', { send: el.dataset.send, accept });
      toast(accept ? 'Saved to ' + r.saved_to : 'Declined');
    } catch (e) { el.disabled = false; fail(e); }
    soon();
  },
  async keep() { await run(() => call('files.keep', { path: state.route.path, keep: !state.here.kept })); },
  async resolve(p, el) { await run(async () => { await call('files.resolve', { path: p, keep: el.dataset.keep }); toast('Conflict settled'); }); },
  async syncnow(p, el) {
    el.disabled = true;
    try { state.status = await call('files.sync', {}); toast('Synced'); } catch (e) { fail(e); }
    el.disabled = false;
    soon();
  },
  preview(p, el) { preview(p, el.dataset.size); },
  versions() { const [e] = selected(); if (hasVersions(e)) return versionsSheet(e); },
  async trashRestore(p, el) {
    el.disabled = true;
    try {
      const r = await call('files.trash_restore', { ids: [el.dataset.id] });
      const moved = r.restored.find((x) => x.from !== x.to);
      toast(moved ? 'Restored as ' + moved.to : 'Restored');
    } catch (e) { el.disabled = false; fail(e); }
    soon();
  },
  async trashPurge(p, el) {
    const row = state.trash?.items.find((r) => r.id === el.dataset.id);
    if (!(await confirmBox({ title: 'Delete forever', body: `Delete "${row ? row.name : 'this'}" forever, on all your devices? It cannot be restored.`, ok: 'Delete forever', danger: true }))) return;
    await run(() => call('files.trash_purge', { ids: [el.dataset.id] }));
  },
  async trashEmpty() {
    const n = state.trash?.count || 0;
    if (!(await confirmBox({ title: 'Empty trash', body: `Delete ${plural(n, 'item')} forever, on all your devices? They cannot be restored.`, ok: 'Empty trash', danger: true }))) return;
    await run(() => call('files.trash_empty', {}));
  },
};

const rowOf = (ev) => ev.target.closest('[data-item][data-path]');
$main.addEventListener('click', (ev) => {
  const t = ev.target.closest('[data-act]');
  if (t && actions[t.dataset.act]) {
    if (t.dataset.act === 'pick') { actions.pick(t.dataset.path, t); return; }
    ev.preventDefault();
    const r = actions[t.dataset.act](t.dataset.path, t);
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
$main.addEventListener('keydown', (ev) => {
  const row = ev.key === 'Enter' && ev.target.matches?.('[data-item][data-path]') ? ev.target : null;
  if (row) { ev.preventDefault(); actions.open(row.dataset.path, row); }
});
$shell.addEventListener('click', (ev) => {
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
  if (!e) return [];
  const items = [{ label: e.type === 'dir' ? 'Open' : 'Preview', icon: 'eye', onSelect: () => actions.open(e.path, row) }];
  if (e.virtual) return items;
  const one = (act) => () => { state.sel = new Set([e.path]); const r = actions[act](e.path, row); Promise.resolve(r).finally(() => { state.sel.clear(); draw(); }); };
  for (const a of entryActions([e])) items.push({ label: a.label, icon: a.icon, danger: a.danger, onSelect: one(a.act) });
  items.push({ separator: true }, { label: 'Select', icon: 'check', onSelect: () => { state.selecting = true; state.sel = new Set([e.path]); draw(); } });
  return items;
});
document.getElementById('picker').addEventListener('change', (ev) => { upload([...ev.target.files]); ev.target.value = ''; });
let searchTimer = null;
const searchInput = (input) => {
  const q = input.value.trim();
  state.query = input.value;
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => { location.hash = q ? '#!search=' + encodeURIComponent(q) : '#/'; }, 250);
};
$shell.addEventListener('input', (ev) => { if (ev.target.id === 'search') searchInput(ev.target); });
document.addEventListener('keydown', (ev) => {
  if (ev.defaultPrevented || openViewerNow() || document.querySelector('sol-dialog dialog[open]')) return;
  if (ev.key === 'Escape' && state.selecting && !document.querySelector('.menu')) actions.clear();
  if (ev.key === 'Delete' && state.sel.size && !/INPUT|TEXTAREA|SELECT/.test(document.activeElement.tagName)) actions.delete();
});

// Drag and drop upload.
let dragDepth = 0;
const hasFiles = (e) => [...(e.dataTransfer?.types || [])].includes('Files');
document.addEventListener('dragenter', (e) => { if (hasFiles(e) && canWriteHere()) { dragDepth++; $veil.hidden = false; e.preventDefault(); } });
document.addEventListener('dragover', (e) => { if (hasFiles(e)) e.preventDefault(); });
document.addEventListener('dragleave', () => { dragDepth = Math.max(0, dragDepth - 1); if (!dragDepth) $veil.hidden = true; });
document.addEventListener('drop', async (e) => {
  if (!hasFiles(e)) return;
  e.preventDefault();
  dragDepth = 0;
  $veil.hidden = true;
  try { upload(await collect(e.dataTransfer)); } catch (err) { fail(err); }
});

phoneQ.addEventListener('change', draw);
window.addEventListener('hashchange', () => { state.sel.clear(); state.selecting = false; openViewerNow()?.teardown(); load(); });
events({ people: true }, soon, () => { state.status = state.status && { ...state.status, state: 'offline' }; draw(); });
// Hold the page's load event for the first complete answer (capped), so the first paint is the
// real list. A live event may start a newer load meanwhile; whichever finishes first counts.
load();
await Promise.race([firstPaint, new Promise((r) => setTimeout(r, 3000))]);
