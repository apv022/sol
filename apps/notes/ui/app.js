// Notes UI. It only calls actions (sol.call): the notes are files on disk that the app reads and
// writes; Files syncs them. Every API value reaches the DOM through html`` (escaped); the only
// raw markup is the output of md.js (which escapes all source HTML) and of highlight() below.
import { call, putBlob, html, raw, render, esc } from '/sol.js';
import { toast, setTheme } from '/kit/kit.js';
import { render as mdRender, renderInline } from '/md.js';

const $ = (id) => document.getElementById(id);
const $app = $('app'), $list = $('list'), $q = $('q'), $ta = $('ta'), $hl = $('hl'), $prev = $('prev');
const store = {
  get(k, d) { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* optional */ } },
};

// The list is local data: keep the last copy so the next start paints it at once.
const CACHE = 'notes-cache';
function readCache() { try { return JSON.parse(localStorage.getItem(CACHE) || 'null'); } catch { return null; } }
function writeCache() { try { localStorage.setItem(CACHE, JSON.stringify({ notes: state.all.slice(0, 300), folders: state.folders, tags: state.tags })); } catch { /* optional */ } }

const state = {
  rev: undefined, all: [], folders: [], tags: [],
  folder: null, tag: null, pinned: false, // filters
  query: '', results: null, shown: 200,
  cur: null, // {path, etag, saved, dirty, saving, pinned}
  pane: store.get('notes-pane', 'edit'),
  titles: new Map(), // lowercase stem/title -> path
};

// ---------- helpers ----------

const dirOf = (p) => p.slice(0, Math.max(0, p.lastIndexOf('/')));
const baseOf = (p) => p.slice(p.lastIndexOf('/') + 1);
const stemOf = (p) => baseOf(p).replace(/\.md$/i, '');
const joinp = (d, n) => (d ? d + '/' + n : n);
const fail = (e) => toast(e?.message || String(e), { kind: 'error' });
const enc = (p) => p.split('/').map(encodeURIComponent).join('/');
/** iOS Notes style: "9:23 PM" today, "Yesterday", a weekday within the week, then "Nov 3" / "Nov 3, 2025". */
function fmtTime(ms) {
  const d = new Date(ms), now = new Date();
  const day = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate());
  const diff = Math.round((day(now) - day(d)) / 864e5);
  if (diff <= 0) return d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  if (diff === 1) return 'Yesterday';
  if (diff < 7) return d.toLocaleDateString([], { weekday: 'long' });
  return d.toLocaleDateString([], { month: 'short', day: 'numeric', year: d.getFullYear() === now.getFullYear() ? undefined : 'numeric' });
}
const phoneQ = matchMedia('(max-width: 760px)');
/** Resolves a relative url against the note's folder; null if it would leave Notes/. */
function resolveRel(url, base) {
  if (/^[a-z][a-z0-9+.-]*:/i.test(url) || url.startsWith('/') || url.startsWith('#')) return null;
  let dec;
  try { dec = decodeURIComponent(url.split('#')[0]); } catch { return null; }
  const out = base ? base.split('/') : [];
  for (const seg of dec.split('/')) {
    if (seg === '' || seg === '.') continue;
    if (seg === '..') { if (!out.length) return null; out.pop(); } else out.push(seg);
  }
  return out.join('/');
}
const mdOpts = () => ({
  href(url, kind) {
    const p = resolveRel(url, state.cur ? dirOf(state.cur.path) : '');
    if (p === null) return url.startsWith('#') ? url : '#';
    return /\.md$/i.test(p) && kind === 'link' ? '#/n/' + enc(p) : '/file?path=' + encodeURIComponent(p);
  },
  wiki: (t) => !!lookup(t),
});
function lookup(target) {
  const t = target.toLowerCase().replace(/\.md$/, '');
  if (t.includes('/')) return state.all.find((n) => n.path.toLowerCase() === t + '.md')?.path;
  return state.titles.get(t);
}

// ---------- data ----------

async function refresh(force) {
  let r;
  try { r = await call('notes.list', { since: force ? undefined : state.rev, limit: 50000, snippets: true }); } catch (e) { if (force) fail(e); return false; }
  if (r.unchanged) return false;
  state.rev = r.rev; state.all = r.notes; state.folders = r.folders;
  state.titles = new Map();
  for (const n of [...r.notes].reverse()) { state.titles.set(n.title.toLowerCase(), n.path); }
  for (const n of [...r.notes].reverse()) { state.titles.set(stemOf(n.path).toLowerCase(), n.path); }
  try { state.tags = (await call('notes.tags', {})).tags; } catch { /* keep old */ }
  drawFilters(); drawList(); writeCache();
  if (state.cur) await checkExternal();
  return true;
}

// ---------- sidebar ----------

function visible() {
  let l = state.all;
  if (state.pinned) l = l.filter((n) => n.pinned);
  if (state.folder !== null) l = l.filter((n) => n.folder === state.folder);
  if (state.tag) l = l.filter((n) => n.tags.some((t) => t === state.tag || t.startsWith(state.tag + '/')));
  return l;
}

function drawFilters() {
  const chip = (label, on, attrs) => html`<button type="button" aria-pressed="${on}" ${raw(attrs)}>${label}</button>`;
  render($('filters'), html`
    ${chip('All', state.folder === null && !state.tag && !state.pinned, 'data-f="all"')}
    ${chip('Pinned', state.pinned, 'data-f="pinned"')}
    ${state.folders.map((f) => chip(f, state.folder === f, `data-f="folder" data-v="${esc(f)}"`))}
    ${state.tags.slice(0, 40).map((t) => chip('#' + t.tag, state.tag === t.tag, `data-f="tag" data-v="${esc(t.tag)}"`))}`);
}

/** The server's snippet is the first prose line as written; drop list, quote and heading markers and emphasis for a calm preview. */
const plainSnip = (t) => (t || '').replace(/^\s*(?:[>#]+\s*|[-*+]\s+(?:\[[ xX]\]\s*)?|\d+[.)]\s+)+/, '').replace(/(\*\*|__|`)/g, '');
const sunIcon = (name) => html`<sol-icon name="${name}"></sol-icon>`;
function noteRow(n, snippetHTML) {
  return html`<div data-item class="note" data-path="${n.path}" ${raw(cur(n.path))}>
    <div class="line"><span class="row-title">${n.title}</span>${n.pinned ? html`<span class="pin" title="Pinned">${sunIcon('pin')}</span>` : ''}</div>
    <div class="line sub"><span class="when">${fmtTime(n.mtime)}</span><span class="snip">${snippetHTML}</span></div>
    <div class="line folder">${sunIcon('folder')}<span>${n.folder || 'Notes'}</span></div></div>`;
}
function drawList() {
  if (state.results) {
    render($list, state.results.length ? html`${state.results.map((r) => noteRow(r, raw(renderInline(r.snippet || '', {}))))}`
      : html`<div slot="empty" class="empty"><h3>No matches</h3><p>Try fewer words or a prefix.</p></div>`);
    return;
  }
  const l = visible();
  const rows = l.slice(0, state.shown);
  render($list, l.length ? html`${rows.map((n) => noteRow(n, plainSnip(n.snippet)))}
    ${l.length > rows.length ? html`<button class="more-row" type="button" data-more>Show more (${l.length - rows.length})</button>` : ''}`
    : html`<div slot="empty" class="empty"><sol-icon name="file"></sol-icon><h3>No notes here</h3><p>Tap + to write one.</p></div>`);
  $('count').textContent = plural(l.length, 'Note');
}
const plural = (n, w) => n + ' ' + w + (n === 1 ? '' : 's');
const cur = (p) => (state.cur && state.cur.path === p ? 'aria-current="true"' : '');

let searchSeq = 0;
async function search() {
  const q = $q.value.trim();
  state.query = q;
  if (!q) { state.results = null; drawList(); return; }
  const seq = ++searchSeq;
  try {
    const r = await call('notes.search', { query: q, limit: 50 });
    if (seq === searchSeq) { state.results = r.results; drawList(); }
  } catch (e) { fail(e); }
}
let searchT;
$q.addEventListener('input', () => { clearTimeout(searchT); searchT = setTimeout(search, 120); });

$('filters').addEventListener('click', (e) => {
  const b = e.target.closest('button[data-f]');
  if (!b) return;
  const f = b.dataset.f, v = b.dataset.v;
  state.folder = null; state.pinned = false; state.tag = null; state.shown = 200;
  if (f === 'folder') state.folder = v; else if (f === 'tag') state.tag = v; else if (f === 'pinned') state.pinned = true;
  drawFilters(); drawList();
});
$list.addEventListener('click', (e) => {
  if (e.target.closest('[data-more]')) { state.shown += 400; drawList(); return; }
  const it = e.target.closest('[data-path]');
  if (it) location.hash = '#/n/' + enc(it.dataset.path);
});
$list.addEventListener('sol-select', (e) => { const p = e.detail?.dataset?.path; if (p) location.hash = '#/n/' + enc(p); });

// ---------- opening, saving ----------

function route() {
  const m = /^#(?:\/n\/|note=)(.+)$/.exec(location.hash);
  if (!m) return null;
  try { return decodeURIComponent(m[1]); } catch { return null; }
}

async function open(path) {
  if (state.cur && state.cur.path === path) { setView('edit'); return; }
  await flush();
  let r;
  try { r = await call('notes.read', { path }); } catch (e) { fail(e); location.hash = '#/'; return; }
  state.cur = { path, etag: r.etag, saved: r.text, dirty: false, saving: false, pinned: r.pinned };
  $ta.value = r.text;
  $('ed').hidden = false; $('empty').hidden = true;
  banner(null);
  afterEdit();
  setView('edit');
  drawList();
  loadBacklinks();
  if (/^# \n*$/.test(r.text)) { $ta.focus(); $ta.setSelectionRange(2, 2); }
}

function close() {
  state.cur = null;
  $('ed').hidden = true; $('empty').hidden = false;
  setView('list');
  drawList();
}

function setView(v) { $app.dataset.view = v; }

function setPane(p) {
  state.pane = p; store.set('notes-pane', p);
  $('ed').dataset.pane = p;
  for (const b of $('seg').querySelectorAll('button')) b.setAttribute('aria-pressed', String(b.dataset.pane === p));
  if (p === 'preview') drawPreview();
}
$('seg').addEventListener('click', (e) => { const b = e.target.closest('button'); if (b) setPane(b.dataset.pane); });

window.addEventListener('hashchange', () => { const p = route(); if (p) open(p); else if (state.cur) close(); else setView('list'); });
$('back').addEventListener('click', async () => { await flush(); location.hash = '#/'; });

let saveT;
function setStatus(s) { $('status').textContent = s; }
function afterEdit() {
  autogrow(); drawHighlight(); drawPreview();
  const c = state.cur;
  const title = /^#[ \t]+(.+)$/m.exec($ta.value)?.[1] || stemOf(c.path);
  $('ed-title').textContent = title;
  $('pin').setAttribute('aria-pressed', String(!!c.pinned)); $('pin').setAttribute('aria-label', c.pinned ? 'Unpin' : 'Pin');
  setStatus(c.dirty ? 'Edited' : 'Saved');
}
function autogrow() { $ta.style.height = 'auto'; $ta.style.height = $ta.scrollHeight + 'px'; }

let hlRaf = 0, pvT = 0;
function drawHighlight() { cancelAnimationFrame(hlRaf); hlRaf = requestAnimationFrame(() => render($hl, raw(highlight($ta.value) + '\n '))); }
function drawPreview() {
  clearTimeout(pvT);
  pvT = setTimeout(() => { if (state.cur) render($prev, raw(mdRender($ta.value, mdOpts()))); }, 100);
}

$ta.addEventListener('input', () => {
  const c = state.cur;
  if (!c) return;
  c.dirty = $ta.value !== c.saved;
  afterEdit();
  acUpdate();
  clearTimeout(saveT);
  if (c.dirty) saveT = setTimeout(save, 700);
});

async function save() {
  const c = state.cur;
  if (!c || c.saving || !c.dirty) return;
  clearTimeout(saveT);
  const text = $ta.value;
  c.saving = true; setStatus('Saving…');
  try {
    const r = await call('notes.write', { path: c.path, text, base: c.etag });
    if (state.cur === c) { c.etag = r.etag; c.saved = text; c.dirty = $ta.value !== text; setStatus(c.dirty ? 'Edited' : 'Saved'); if (c.dirty) saveT = setTimeout(save, 300); }
  } catch (e) {
    if (e.code === 'conflict' && state.cur === c) { banner('conflict'); setStatus('Not saved'); } else fail(e);
  } finally { c.saving = false; }
}
async function flush() {
  const c = state.cur;
  if (c && c.dirty && !$('banner').dataset.kind) { clearTimeout(saveT); while (c.saving) await new Promise((r) => setTimeout(r, 30)); await save(); }
}
window.addEventListener('pagehide', () => { if (state.cur?.dirty && !$('banner').dataset.kind) save(); });
document.addEventListener('visibilitychange', () => { if (document.hidden) flush(); });

// ---------- external edits ----------

function banner(kind) {
  const b = $('banner');
  if (!kind) { b.hidden = true; delete b.dataset.kind; return; }
  b.hidden = false; b.dataset.kind = kind;
  const msg = kind === 'deleted' ? 'This note was deleted on disk.' : 'This note changed on another device or program.';
  render(b, html`<span>${msg}</span>${kind === 'deleted' ? html`<button type="button" data-b="keep">Keep mine</button><button type="button" data-b="close">Close</button>`
    : html`<button type="button" data-b="reload">Reload</button><button type="button" data-b="keep">Keep mine</button>`}`);
}
$('banner').addEventListener('click', async (e) => {
  const act = e.target.closest('[data-b]')?.dataset.b, c = state.cur;
  if (!act || !c) return;
  try {
    if (act === 'close') { banner(null); close(); location.hash = '#/'; return; }
    if (act === 'reload') {
      const r = await call('notes.read', { path: c.path });
      applyDisk(r); banner(null);
    } else if (act === 'keep') {
      let etag = '';
      try { etag = (await call('notes.read', { path: c.path })).etag; } catch { /* deleted: write it again */ }
      const text = $ta.value;
      const r = await call('notes.write', { path: c.path, text, base: etag });
      c.etag = r.etag; c.saved = text; c.dirty = false; banner(null); afterEdit();
    }
  } catch (err) { fail(err); }
});

function applyDisk(r) {
  const c = state.cur, a = $ta.selectionStart, b = $ta.selectionEnd;
  c.etag = r.etag; c.saved = r.text; c.dirty = false; c.pinned = r.pinned;
  $ta.value = r.text;
  $ta.setSelectionRange(Math.min(a, r.text.length), Math.min(b, r.text.length));
  afterEdit();
}

async function checkExternal() {
  const c = state.cur;
  if (!c) return;
  let r;
  try { r = await call('notes.read', { path: c.path }); } catch (e) {
    if (e.code === 'not_found') { if (c.dirty) banner('deleted'); else { toast('That note was deleted.'); close(); location.hash = '#/'; } }
    return;
  }
  if (state.cur !== c || r.etag === c.etag) return;
  if (!c.dirty && !c.saving) { applyDisk(r); toast('Updated from disk'); } else banner('external');
  loadBacklinks();
}

// ---------- preview interaction ----------

$prev.addEventListener('click', async (e) => {
  const c = state.cur;
  const box = e.target.closest('input[type=checkbox][data-line]');
  if (box && c) {
    e.preventDefault();
    try {
      await flush();
      const r = await call('notes.toggle_task', { path: c.path, line: +box.dataset.line, base: c.etag });
      c.etag = r.etag; c.saved = r.text; c.dirty = false;
      const a = $ta.selectionStart; $ta.value = r.text; $ta.setSelectionRange(a, a); afterEdit();
    } catch (err) { if (err.code === 'conflict') banner('external'); else fail(err); }
    return;
  }
  const w = e.target.closest('a[data-wiki]');
  if (w) { e.preventDefault(); openWiki(w.dataset.wiki); return; }
  const t = e.target.closest('a[data-tag]');
  if (t) { e.preventDefault(); state.tag = t.dataset.tag; state.folder = null; state.pinned = false; drawFilters(); drawList(); setView('list'); }
});
async function openWiki(name) {
  const p = lookup(name);
  if (p) { location.hash = '#/n/' + enc(p); return; }
  try {
    await flush();
    const r = await call('notes.create', { folder: state.cur ? dirOf(state.cur.path) : '', title: name, text: '# ' + name + '\n\n' });
    await refresh(true);
    location.hash = '#/n/' + enc(r.path);
  } catch (e) { fail(e); }
}
async function loadBacklinks() {
  const c = state.cur;
  if (!c) return;
  try {
    const r = await call('notes.backlinks', { path: c.path });
    if (state.cur !== c) return;
    render($('bl'), r.backlinks.length ? html`<h3>Linked from (${r.backlinks.length})</h3>${r.backlinks.map((b) => html`<button type="button" data-path="${b.path}"><strong>${b.title}</strong><small class="snip">${raw(renderInline(b.snippet, {}))}</small></button>`)}` : html``);
  } catch { /* optional */ }
}
$('bl').addEventListener('click', (e) => { const b = e.target.closest('[data-path]'); if (b) location.hash = '#/n/' + enc(b.dataset.path); });

// ---------- highlight (colour only, so it stays aligned with the textarea) ----------

const INLINE = /(`[^`\n]*`)|(\[\[[^\]\n]*\]\])|(!?\[[^\]\n]*\]\([^)\n]*\))|(\*\*[^*\n]+\*\*|__[^_\n]+__)|(\*[^*\s][^*\n]*\*|_[^_\s][^_\n]*_)|((?<![\p{L}\p{N}/&])#[\p{L}\p{N}_][\p{L}\p{N}_/-]*)/gu;
const CLS = [null, 'h-code', 'h-link', 'h-link', 'h-em', 'h-em', 'h-tag'];
function highlight(text) {
  let fence = false;
  return text.split('\n').map((l) => {
    if (/^ {0,3}(```|~~~)/.test(l)) { fence = !fence; return `<span class="h-mark">${esc(l)}</span>`; }
    if (fence) return `<span class="h-code">${esc(l)}</span>`;
    if (/^#{1,6}\s/.test(l)) return `<span class="h-head">${esc(l)}</span>`;
    let head = '', rest = l;
    const m = /^(\s*(?:>\s?)*(?:[-*+]|\d+[.)])?\s*(?:\[[ xX]\]\s)?)/.exec(l);
    if (m && m[1]) { head = `<span class="h-mark">${esc(m[1])}</span>`; rest = l.slice(m[1].length); }
    let out = '', last = 0;
    for (const x of rest.matchAll(INLINE)) {
      const k = x.findIndex((v, i) => i > 0 && v !== undefined);
      out += esc(rest.slice(last, x.index)) + `<span class="${CLS[k]}">${esc(x[0])}</span>`;
      last = x.index + x[0].length;
    }
    return head + out + esc(rest.slice(last));
  }).join('\n');
}

// ---------- editing commands ----------

function replaceRange(from, to, text, selFrom, selTo) {
  $ta.focus();
  $ta.setSelectionRange(from, to);
  if (!document.execCommand('insertText', false, text)) {
    $ta.setRangeText(text, from, to, 'end');
    $ta.dispatchEvent(new Event('input', { bubbles: true }));
  }
  $ta.setSelectionRange(selFrom ?? from + text.length, selTo ?? selFrom ?? from + text.length);
}
function wrap(l, r = l) {
  const { selectionStart: a, selectionEnd: b, value: v } = $ta;
  if (a >= l.length && v.slice(a - l.length, a) === l && v.slice(b, b + r.length) === r) { // unwrap
    replaceRange(a - l.length, b + r.length, v.slice(a, b), a - l.length, b - l.length); return;
  }
  replaceRange(a, b, l + v.slice(a, b) + r, a + l.length, b + l.length);
}
function lineRange() {
  const v = $ta.value, a = $ta.selectionStart, b = $ta.selectionEnd;
  const s = v.lastIndexOf('\n', a - 1) + 1;
  let e = v.indexOf('\n', b);
  if (e < 0) e = v.length;
  return [s, e];
}
function mapLines(fn) {
  const [s, e] = lineRange();
  const lines = $ta.value.slice(s, e).split('\n');
  const out = fn(lines).join('\n');
  replaceRange(s, e, out, s, s + out.length);
}
const commands = {
  bold: () => wrap('**'), italic: () => wrap('*'), code: () => wrap('`'),
  link() {
    const { selectionStart: a, selectionEnd: b, value: v } = $ta;
    const label = v.slice(a, b);
    const t = `[${label}](url)`;
    replaceRange(a, b, t, a + label.length + 3, a + label.length + 6);
  },
  heading() {
    mapLines(([l, ...rest]) => {
      const m = /^(#{1,6}) /.exec(l);
      const n = m ? (m[1].length >= 3 ? 0 : m[1].length + 1) : 1;
      return [(n ? '#'.repeat(n) + ' ' : '') + l.replace(/^#{1,6} /, ''), ...rest];
    });
  },
  check() {
    mapLines((ls) => ls.map((l) => (/^\s*[-*+] \[[ xX]\] /.test(l) ? l.replace(/\[[ xX]\] /, '') : /^\s*[-*+] /.test(l) ? l.replace(/^(\s*[-*+] )/, '$1[ ] ') : '- [ ] ' + l)));
  },
  attach: () => $('file').click(),
};
$('toolbar').addEventListener('click', (e) => { const b = e.target.closest('[data-cmd]'); if (b) commands[b.dataset.cmd]?.(); });
$('toolbar').addEventListener('mousedown', (e) => { if (e.target.closest('button')) e.preventDefault(); }); // keep the caret

$ta.addEventListener('keydown', (e) => {
  if (acKey(e)) return;
  const mod = e.ctrlKey || e.metaKey;
  if (mod && !e.shiftKey && !e.altKey) {
    const k = e.key.toLowerCase();
    if (k === 'b') { e.preventDefault(); commands.bold(); return; }
    if (k === 'i') { e.preventDefault(); commands.italic(); return; }
    if (k === 'k') { e.preventDefault(); commands.link(); return; }
    if (k === 'e') { e.preventDefault(); commands.code(); return; }
  }
  if (mod && e.shiftKey && e.key.toLowerCase() === 'l') { e.preventDefault(); commands.check(); return; }
  if (mod && e.altKey && /^Digit[1-3]$/.test(e.code)) {
    e.preventDefault();
    const n = +e.code.slice(5);
    mapLines(([l, ...rest]) => ['#'.repeat(n) + ' ' + l.replace(/^#{1,6} /, ''), ...rest]);
    return;
  }
  if (e.key === 'Enter' && !e.shiftKey && !mod && !e.isComposing && $ta.selectionStart === $ta.selectionEnd) {
    const a = $ta.selectionStart, v = $ta.value, s = v.lastIndexOf('\n', a - 1) + 1;
    const m = /^(\s*(?:>\s?)*)(?:([-*+])|(\d+)([.)]))?\s?(\[[ xX]\]\s)?/.exec(v.slice(s, a));
    const marker = m && (m[2] || m[3] || /^\s*>/.test(m[0]));
    if (m && marker && v.slice(s, a).length >= m[0].length) {
      const atEnd = v.indexOf('\n', a) === a || a === v.length;
      if (v.slice(s + m[0].length, a).trim() === '' && atEnd) { e.preventDefault(); replaceRange(s, a, m[1].replace(/>\s?/g, '')); return; }
      const bullet = m[2] ? m[2] + ' ' : m[3] ? (+m[3] + 1) + m[4] + ' ' : '';
      e.preventDefault();
      replaceRange(a, a, '\n' + m[1] + bullet + (m[5] ? '[ ] ' : ''));
    }
    return;
  }
  if (e.key === 'Tab' && !mod) {
    const [s] = lineRange();
    const line = $ta.value.slice(s, $ta.value.indexOf('\n', s) < 0 ? undefined : $ta.value.indexOf('\n', s));
    if (/^\s*([-*+]|\d+[.)]) /.test(line)) {
      e.preventDefault();
      mapLines((ls) => ls.map((l) => (e.shiftKey ? l.replace(/^( {1,2}|\t)/, '') : '  ' + l)));
    }
  }
});

// ---------- [[link]] autocomplete ----------

let acItems = [], acSel = 0;
function acUpdate() {
  const a = $ta.selectionStart;
  const m = /\[\[([^\]\n|#]*)$/.exec($ta.value.slice(Math.max(0, a - 80), a));
  if (!m || $ta.selectionEnd !== a) { acHide(); return; }
  const q = m[1].toLowerCase();
  const seen = new Set();
  acItems = state.all.filter((n) => n.path !== state.cur?.path && (stemOf(n.path).toLowerCase().includes(q) || n.title.toLowerCase().includes(q)) && !seen.has(n.path) && seen.add(n.path))
    .sort((x, y) => (stemOf(x.path).toLowerCase().startsWith(q) ? 0 : 1) - (stemOf(y.path).toLowerCase().startsWith(q) ? 0 : 1)).slice(0, 8)
    .map((n) => ({ stem: stemOf(n.path), title: n.title, query: m[1] }));
  if (!acItems.length) { acHide(); return; }
  acSel = Math.min(acSel, acItems.length - 1);
  acDraw();
}
function acDraw() {
  const el = $('ac');
  el.hidden = false;
  render(el, html`${acItems.map((it, i) => html`<button type="button" role="option" aria-selected="${i === acSel}" data-i="${i}">${it.stem}${it.title !== it.stem ? html` <small class="muted">${it.title}</small>` : ''}</button>`)}`);
}
function acHide() { acItems = []; $('ac').hidden = true; }
function acPick(i) {
  const it = acItems[i];
  if (!it) return;
  const a = $ta.selectionStart, v = $ta.value;
  const closeAt = v.slice(a, a + 2) === ']]' ? 2 : 0;
  replaceRange(a - it.query.length, a + closeAt, it.stem + ']]');
  acHide();
}
function acKey(e) {
  if ($('ac').hidden || !acItems.length) return false;
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { e.preventDefault(); acSel = (acSel + (e.key === 'ArrowDown' ? 1 : acItems.length - 1)) % acItems.length; acDraw(); return true; }
  if (e.key === 'Enter' || e.key === 'Tab') { e.preventDefault(); acPick(acSel); return true; }
  if (e.key === 'Escape') { acHide(); return true; }
  return false;
}
$('ac').addEventListener('mousedown', (e) => e.preventDefault());
$('ac').addEventListener('click', (e) => { const b = e.target.closest('[data-i]'); if (b) acPick(+b.dataset.i); });
$ta.addEventListener('blur', () => setTimeout(acHide, 150));

// ---------- attachments ----------

/** The file goes to the daemon as a blob (no size cap, nothing base64'd); the app copies it in. */
async function attach(files) {
  const c = state.cur;
  if (!c) return;
  for (const f of files) {
    try {
      const b = await putBlob(f);
      const r = await call('notes.attach', { note: c.path, name: f.name || 'image.png', blob: b.hash });
      const a = $ta.selectionStart;
      replaceRange(a, $ta.selectionEnd, r.markdown + '\n');
    } catch (e) { fail(e); }
  }
}
$('file').addEventListener('change', (e) => { attach([...e.target.files]); e.target.value = ''; });
$ta.addEventListener('paste', (e) => { const fs = [...(e.clipboardData?.files || [])]; if (fs.length) { e.preventDefault(); attach(fs); } });
$('edpane').addEventListener('dragover', (e) => { if (e.dataTransfer?.types.includes('Files')) e.preventDefault(); });
$('edpane').addEventListener('drop', (e) => { const fs = [...(e.dataTransfer?.files || [])]; if (fs.length) { e.preventDefault(); attach(fs); } });

// ---------- note actions: new, pin, rename, move, delete ----------

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
function ask({ title, label, value = '', ok = 'OK', options }) {
  return new Promise((resolve) => {
    let done = false;
    const field = options ? html`<select name="v">${options.map((o) => html`<option value="${o}" ${raw(o === value ? 'selected' : '')}>${o || 'Notes (top level)'}</option>`)}</select>` : html`<input name="v" value="${value}" autocomplete="off" required>`;
    const d = dialog(title, html`<form><label>${label}${field}</label><div class="dlg-actions"><button type="button" data-no>Cancel</button><button class="primary" type="submit">${ok}</button></div></form>`);
    const finish = (v) => { if (!done) { done = true; resolve(v); } d.close(); };
    const form = d.box.querySelector('form');
    form.elements.v.focus();
    form.addEventListener('submit', (e) => { e.preventDefault(); finish(form.elements.v.value.trim() ?? null); });
    d.box.querySelector('[data-no]').addEventListener('click', () => finish(null));
    d.el.addEventListener('close', () => finish(null));
  });
}
function confirmBox(title, body, okLabel) {
  return new Promise((resolve) => {
    let done = false;
    const d = dialog(title, html`<p>${body}</p><div class="dlg-actions"><button type="button" data-no>Cancel</button><button class="danger" type="button" data-yes>${okLabel}</button></div>`);
    const finish = (v) => { if (!done) { done = true; resolve(v); } d.close(); };
    d.box.querySelector('[data-yes]').addEventListener('click', () => finish(true));
    d.box.querySelector('[data-no]').addEventListener('click', () => finish(false));
    d.el.addEventListener('close', () => finish(false));
  });
}

for (const b of document.querySelectorAll('[data-new]')) b.addEventListener('click', newNote);
async function newNote() {
  try {
    await flush();
    const r = await call('notes.create', { folder: state.folder ?? '', title: 'Untitled', text: '# \n\n' });
    await refresh(true);
    location.hash = '#/n/' + enc(r.path);
  } catch (e) { fail(e); }
}
$('pin').addEventListener('click', async () => {
  const c = state.cur;
  if (!c) return;
  try { await call('notes.pin', { path: c.path, pinned: !c.pinned }); c.pinned = !c.pinned; afterEdit(); await refresh(true); } catch (e) { fail(e); }
});
$('more').addEventListener('click', () => {
  const c = state.cur;
  if (!c) return;
  const d = dialog('Note', html`<div class="actions-menu"><button type="button" data-a="rename">Rename</button> <button type="button" data-a="move">Move to folder</button> <button type="button" class="danger" data-a="delete">Delete</button></div>`);
  d.box.addEventListener('click', async (e) => {
    const a = e.target.closest('[data-a]')?.dataset.a;
    if (!a) return;
    d.close();
    try {
      await flush();
      if (a === 'rename') {
        const n = await ask({ title: 'Rename', label: 'Name (links to this note are updated)', value: stemOf(c.path), ok: 'Rename' });
        if (n) await moveTo(c, joinp(dirOf(c.path), n.replace(/\//g, '-') + '.md'));
      } else if (a === 'move') {
        const f = await ask({ title: 'Move', label: 'Folder', value: dirOf(c.path), options: ['', ...state.folders], ok: 'Move' });
        if (f !== null) await moveTo(c, joinp(f, baseOf(c.path)));
      } else if (a === 'delete') {
        if (await confirmBox('Delete this note?', `"${stemOf(c.path)}" is deleted on all your devices.`, 'Delete')) {
          await call('notes.delete', { paths: [c.path] });
          close(); location.hash = '#/'; await refresh(true);
        }
      }
    } catch (err) { fail(err); }
  });
});
async function moveTo(c, to) {
  const r = await call('notes.move', { from: c.path, to });
  if (r.rewritten?.length) toast(`Updated links in ${r.rewritten.length} note${r.rewritten.length > 1 ? 's' : ''}`);
  c.path = r.path;
  await refresh(true);
  location.hash = '#/n/' + enc(r.path);
}

$('theme').addEventListener('click', () => {
  const dark = document.documentElement.getAttribute('data-theme') === 'dark' || (!document.documentElement.hasAttribute('data-theme') && matchMedia('(prefers-color-scheme: dark)').matches);
  setTheme(dark ? 'light' : 'dark');
});

// ---------- start ----------

setInterval(() => { if (!document.hidden) refresh(false); }, 3000);
window.addEventListener('focus', () => refresh(false));
setPane(state.pane);
const cached = readCache();
if (cached) { state.all = cached.notes; state.folders = cached.folders; state.tags = cached.tags; drawFilters(); drawList(); }
refresh(true).then(() => {
  const p = route();
  if (p) open(p);
  else if (!phoneQ.matches && state.all.length) { history.replaceState(null, '', '#/n/' + enc(state.all[0].path)); open(state.all[0].path); } // desktop: the editor is never empty
});
// Rotating or resizing to desktop with nothing open: show the latest note.
phoneQ.addEventListener('change', () => { if (!phoneQ.matches && !state.cur && state.all.length) location.hash = '#/n/' + enc(state.all[0].path); });
