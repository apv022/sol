// Files UI. It only calls actions (sol.call) and listens to events; it never touches the disk,
// feeds or blobs directly, except to hand an uploaded file to the daemon (putBlob) and then ask
// the app to write it (files.write). Every API value reaches the DOM through html`` (escaped).
import { call, events, putBlob, people, html, raw, render } from '/sol.js';
import { toast, setTheme } from '/kit/kit.js';

const $main = document.getElementById('main');
const $shell = document.getElementById('shell');
const $veil = document.getElementById('dropveil');

const store = {
  get(k, d) { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* optional */ } },
};

const state = {
  route: { kind: 'dir', path: '' },
  view: store.get('files-view', 'list'),
  entries: [],
  here: null, // stat of the current folder
  sel: new Set(),
  query: '',
  status: null,
  conflicts: [],
  error: '',
  loading: true,
};

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
function fmtTime(ms) {
  if (!ms) return '';
  const d = new Date(ms);
  const today = new Date();
  return d.toDateString() === today.toDateString()
    ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    : d.toLocaleDateString([], { year: 'numeric', month: 'short', day: 'numeric' });
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
async function load() {
  const seq = ++loadSeq;
  const r = state.route = parseHash();
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
  state.loading = false;
  draw();
}

let timer = null;
const soon = () => { clearTimeout(timer); timer = setTimeout(load, 120); };

// ---------- drawing ----------

function crumbs(path) {
  const parts = path ? path.split('/') : [];
  let acc = '';
  const items = parts.map((p, i) => {
    acc = join(acc, p);
    const here = i === parts.length - 1;
    return html`<sol-icon name="chevron"></sol-icon>${here
      ? html`<span aria-current="page">${p}</span>`
      : html`<a href="#/${acc.split('/').map(encodeURIComponent).join('/')}">${p}</a>`}`;
  });
  return html`<nav class="crumbs" aria-label="Folder path"><a href="#/">My files</a>${items}</nav>`;
}

function badges(e) {
  return html`${e.conflict ? html`<span class="badge warn">${e.copy ? 'Conflict copy' : 'Conflict'}</span>` : ''}${e.shared ? html`<span class="badge sun">Shared</span>` : ''}${e.read_only && !e.virtual ? html`<span class="badge">Read only</span>` : ''}${e.type === 'file' && !e.local ? html`<span class="badge" title="Not stored on this device; opened on demand">Online only</span>` : ''}`;
}

function listRow(e) {
  const sel = state.sel.has(e.path);
  const k = kindOf(e);
  return html`<div data-item class="${sel ? 'sel' : ''}" data-path="${e.path}" data-type="${e.type}">
    <input type="checkbox" class="pick" data-act="pick" data-path="${e.path}" aria-label="Select ${e.name}" ${raw(sel ? 'checked' : '')} ${raw(e.virtual ? 'disabled' : '')}>
    <sol-icon class="ico ${e.type}" name="${ICON[k]}"></sol-icon>
    <div class="name"><span class="title">${e.name}</span>${badges(e)}</div>
    <span class="cell-size">${e.type === 'file' ? fmtSize(e.size) : ''}</span>
    <span class="cell-mtime">${e.type === 'file' ? fmtTime(e.mtime) : ''}</span>
  </div>`;
}

function tile(e) {
  const sel = state.sel.has(e.path);
  const k = kindOf(e);
  return html`<div class="tile ${sel ? 'sel' : ''}" data-item data-path="${e.path}" data-type="${e.type}" tabindex="0">
    <input type="checkbox" class="pick" data-act="pick" data-path="${e.path}" aria-label="Select ${e.name}" ${raw(sel ? 'checked' : '')} ${raw(e.virtual ? 'disabled' : '')}>
    ${k === 'image' ? html`<img class="thumb" loading="lazy" alt="" src="${rawURL(e.path)}">` : html`<sol-icon class="ico ${e.type}" name="${ICON[k]}"></sol-icon>`}
    <span class="title">${e.name}</span>
    <small>${badges(e)}</small>
  </div>`;
}

function toolbar() {
  const n = state.sel.size;
  const one = n === 1 ? state.entries.find((e) => e.path === [...state.sel][0]) : null;
  const here = state.here;
  const canWrite = state.route.kind === 'dir' && !isVirtual(state.route.path) && !(here && here.read_only);
  const btn = (act, icon, label, cls = '') => html`<button type="button" class="${cls}" data-act="${act}" title="${label}"><sol-icon name="${icon}"></sol-icon><span class="lbl">${label}</span></button>`;
  if (n > 0) {
    return html`<div class="toolbar" role="toolbar" aria-label="Selection">
      <span class="sel-info">${n} selected</span><span class="grow"></span>
      ${one && one.type === 'file' ? btn('download', 'download', 'Download') : ''}
      ${one && !one.read_only ? btn('rename', 'edit', 'Rename') : ''}
      ${one && one.type === 'dir' && !one.virtual && !one.path.startsWith('Shared/') ? btn('share', 'share', 'Share') : ''}
      ${!state.entries.filter((e) => state.sel.has(e.path)).some((e) => e.read_only) ? btn('move', 'folder', 'Move') : ''}
      ${btn('copy', 'copy', 'Copy')}
      ${!state.entries.filter((e) => state.sel.has(e.path)).some((e) => e.read_only) ? btn('delete', 'trash', 'Delete', 'danger') : ''}
      ${btn('clear', 'close', 'Clear')}
    </div>`;
  }
  return html`<div class="toolbar" role="toolbar" aria-label="Folder">
    ${canWrite ? html`<button class="primary" type="button" data-act="upload"><sol-icon name="upload"></sol-icon><span class="lbl">Upload</span></button>
      ${btn('mkdir', 'plus', 'New folder')}` : ''}
    <input type="file" id="picker" multiple hidden>
    <span class="grow"></span>
    ${here && state.route.path ? btn('keep', here.kept ? 'check' : 'download', here.kept ? 'On this device' : 'Online only') : ''}
    ${btn('view', state.view === 'list' ? 'menu' : 'folder', state.view === 'list' ? 'Grid' : 'List')}
  </div>`;
}

function dirView() {
  const r = state.route;
  const empty = r.path === 'Shared'
    ? html`<div slot="empty" class="empty"><sol-icon name="people"></sol-icon><h3>Nothing shared with you yet</h3><p>Folders people share with you show up here, online only until you keep them on this device.</p></div>`
    : html`<div slot="empty" class="empty"><sol-icon name="folder"></sol-icon><h3>This folder is empty</h3><p>${isVirtual(r.path) ? 'Open one of the folders inside.' : 'Drop files here or use Upload.'}</p></div>`;
  const rows = state.entries.map(state.view === 'list' ? listRow : tile);
  return html`${crumbs(r.path)}
    <div class="head"><h1>${r.path ? r.path.split('/').pop() : 'My files'}</h1>${syncPill()}
      <input class="search" type="search" id="search" placeholder="Search names" aria-label="Search names" value="${state.query}"></div>
    ${toolbar()}
    ${state.error ? html`<div class="card flat empty"><sol-icon name="alert"></sol-icon><h3>${state.error}</h3></div>` :
      state.view === 'list'
        ? html`<sol-list class="files" aria-label="Files">${rows}${empty}</sol-list>`
        : html`<div class="grid" role="list">${rows}</div>${state.entries.length ? '' : empty}`}`;
}

function syncPill() {
  const s = state.status;
  if (!s) return '';
  const label = { synced: 'Up to date', syncing: 'Syncing', offline: 'Waiting for the network' }[s.state] || s.state;
  return html`<a class="pill" href="#!sync" data-state="${s.state}">${label}</a>`;
}

function searchView() {
  const rows = state.entries.map(listRow);
  return html`<div class="head"><h1>Search</h1>${syncPill()}
    <input class="search" type="search" id="search" value="${state.query}" aria-label="Search names"></div>
    ${toolbar()}
    <sol-list class="files">${rows}<div slot="empty" class="empty"><sol-icon name="search"></sol-icon><h3>No match</h3><p>Nothing has a name like that.</p></div></sol-list>`;
}

function conflictsView() {
  const cs = state.conflicts;
  return html`<div class="head"><h1>Conflicts</h1>${syncPill()}</div>
    <p class="muted">When two of your devices (or you and someone you share with) edit the same file at the same time, both versions are kept. Choose what to keep.</p>
    ${cs.length === 0 ? html`<div class="card flat empty"><sol-icon name="check"></sol-icon><h3>No conflicts</h3><p>Every file has one version everywhere.</p></div>` :
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
  if (!s) return html`<div class="skeleton" style="height:10rem"></div>`;
  return html`<div class="head"><h1>Sync</h1>${syncPill()}</div>
    <h2>Devices</h2>
    <sol-list>${s.devices.map((d) => html`<div class="dev"><sol-icon name="${d.anchor ? 'device' : 'phone'}"></sol-icon>
      <div class="grow"><div class="title">${d.name}${d.self ? ' (this device)' : ''}</div>
      <small>${d.entries} changes${d.last_entry ? ' · last ' + new Date(d.last_entry).toLocaleString() : ''}</small></div>
      <span class="badge ${d.online ? 'sun' : ''}">${d.online ? 'Online' : 'Offline'}</span></div>`)}</sol-list>
    <h2 style="margin-top: var(--space-6)">This device</h2>
    <dl class="kv"><dt>Folder</dt><dd>${s.folder}</dd><dt>Files</dt><dd>${s.files}</dd><dt>Watching</dt><dd>${s.watcher === 'inotify' ? 'live (inotify)' : 'full scan every few seconds'}</dd>
    <dt>Waiting to sync</dt><dd>${s.pending}</dd>${s.last_error ? html`<dt>Last problem</dt><dd>${s.last_error}</dd>` : ''}</dl>
    ${s.skipped.length ? html`<h2 style="margin-top: var(--space-6)">Not synced</h2><p class="muted">These names or changes cannot be synced everywhere:</p>
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
    const d = dialog(title, html`<p>${body}</p><div class="dlg-actions"><button type="button" data-no>Cancel</button><button class="${danger ? 'danger' : 'primary'}" type="button" data-yes>${ok}</button></div>`);
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
  clear() { state.sel.clear(); draw(); },
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

$main.addEventListener('click', (ev) => {
  const t = ev.target.closest('[data-act]');
  if (t && actions[t.dataset.act]) {
    if (t.dataset.act === 'pick') { actions.pick(t.dataset.path, t); return; }
    ev.preventDefault();
    actions[t.dataset.act](t.dataset.path, t);
    return;
  }
  if (ev.target.closest('a, button, input, select, label')) return;
  const row = ev.target.closest('[data-item][data-path]');
  if (row) actions.open(row.dataset.path, row);
});
$main.addEventListener('change', (ev) => {
  if (ev.target.id === 'picker') { upload([...ev.target.files]); ev.target.value = ''; }
});
let searchTimer = null;
$main.addEventListener('input', (ev) => {
  if (ev.target.id !== 'search') return;
  const q = ev.target.value.trim();
  state.query = ev.target.value;
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => { location.hash = q ? '#!search=' + encodeURIComponent(q) : '#/'; }, 250);
});
document.addEventListener('keydown', (ev) => {
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

document.getElementById('theme').addEventListener('click', () => {
  const cur = document.documentElement.getAttribute('data-theme');
  setTheme(cur === 'dark' ? 'light' : 'dark');
});

window.addEventListener('hashchange', () => { state.sel.clear(); load(); });
events({ people: true }, soon, () => { state.status = state.status && { ...state.status, state: 'offline' }; draw(); });
load();
