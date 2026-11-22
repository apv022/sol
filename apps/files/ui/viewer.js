// The Files viewer: one full-screen layer you move through, next/previous among the files of
// the folder (or search results) it was opened from. It lives outside the re-rendered list, so
// a live update never reloads what it shows. Swipe on touch (follows the finger, commits on
// distance or speed), arrow keys and chevrons on desktop, Escape or Back closes.
//
// Every value from data reaches the DOM through html`` (escaped). The pure helpers at the top are
// exported for test/index.html.
import { html, render } from '/sol.js';

const EXT = {
  image: ['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'bmp', 'ico'],
  video: ['mp4', 'webm', 'mov', 'm4v', 'ogv', 'mkv'],
  audio: ['mp3', 'ogg', 'oga', 'wav', 'flac', 'm4a', 'opus', 'aac'],
  pdf: ['pdf'],
  text: ['txt', 'md', 'markdown', 'json', 'csv', 'tsv', 'log', 'js', 'mjs', 'ts', 'go', 'py', 'rs', 'c', 'h', 'cc', 'cpp',
    'java', 'kt', 'swift', 'rb', 'php', 'css', 'html', 'htm', 'xml', 'yml', 'yaml', 'toml', 'ini', 'conf', 'cfg', 'sh',
    'svg', 'sql', 'ics', 'vcf', 'srt', 'env', 'gitignore', 'solignore'],
};

/** What a file is shown as: image, video, audio, pdf, text or file (no preview). */
export function viewKind(name) {
  const n = String(name || '').toLowerCase();
  const x = n.includes('.') ? n.slice(n.lastIndexOf('.') + 1) : '';
  for (const [k, list] of Object.entries(EXT)) if (list.includes(x)) return k;
  return 'file';
}

/** The index one step in dir (+1 next, -1 previous); it stops at the ends (no wrap). */
export function stepIndex(i, dir, n) {
  const j = i + dir;
  return j < 0 || j >= n ? i : j;
}

/**
 * Whether a finished drag moves the viewer: 'next' (dragged left), 'prev' (right) or null.
 * It commits when the drag is mostly horizontal and either went a quarter of the width or was
 * a quick flick (over 0.4 px/ms and 24 px).
 */
export function swipeIntent(dx, dy, ms, width) {
  const ax = Math.abs(dx);
  if (ax < 24 || ax < Math.abs(dy) * 1.2) return null;
  const fast = ax / Math.max(ms, 1) > 0.4;
  if (ax < width * 0.25 && !fast) return null;
  return dx < 0 ? 'next' : 'prev';
}

/** Rubber band at the ends: dragging past the first or last file moves a third as far. */
export function resist(dx, i, n) {
  return (i === 0 && dx > 0) || (i === n - 1 && dx < 0) ? dx / 3 : dx;
}

/** Indices of the neighbours worth preloading (images only: the rest load when shown). */
export function preloadTargets(items, i) {
  return [i - 1, i + 1].filter((j) => j >= 0 && j < items.length && viewKind(items[j].name) === 'image');
}

/** "3 / 12". */
export const position = (i, n) => `${i + 1} / ${n}`;

export function fmtSize(n) {
  if (n == null) return '';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1000 && i < u.length - 1) { n /= 1000; i++; }
  return (i ? n.toFixed(n < 10 ? 1 : 0) : n) + ' ' + u[i];
}

const TEXT_MAX = 200 * 1024;
const MIME = { pdf: 'application/pdf' };
const icon = (name) => html`<sol-icon name="${name}"></sol-icon>`;
const KIND_ICON = { image: 'image', video: 'video', audio: 'music', pdf: 'file', text: 'file', file: 'file' };

let current = null;

/** The open viewer, if any. */
export const openViewerNow = () => current;

/**
 * Opens the viewer on items[index]. opts:
 *   src(item) -> URL of the content; download(item) -> URL that saves it;
 *   opener: (absURL, mime, name) => bool, the Android bridge (no iframe for PDFs then);
 *   actions: [{act, icon, label, danger, when(item)}]; onAction(act, item, viewer);
 *   onClose(); duration (ms, the slide animation); history (push a state so Back closes).
 * Returns the viewer: {index, items, go(dir), show(i), replace(i, item), remove(i), close()}.
 */
export function openViewer(items, index, opts = {}) {
  current?.close();
  const v = new Viewer(items, index, opts);
  current = v;
  return v;
}

class Viewer {
  constructor(items, index, opts) {
    this.items = items.slice();
    this.index = Math.max(0, Math.min(index, items.length - 1));
    this.opts = { duration: 220, history: true, actions: [], ...opts };
    if (matchMedia('(prefers-reduced-motion: reduce)').matches) this.opts.duration = 0;
    this.slots = new Map(); // item index -> slide element (only the current one and its neighbours)
    this.texts = new Map(); // src -> text already fetched
    this.zoom = { s: 1, x: 0, y: 0 };
    this.build();
    this.layout();
    this.listen();
    if (this.opts.history) { history.pushState({ solViewer: true }, ''); this.pushed = true; }
  }

  build() {
    const root = document.createElement('div');
    root.className = 'viewer';
    root.setAttribute('role', 'dialog');
    root.setAttribute('aria-modal', 'true');
    root.setAttribute('aria-label', 'Preview');
    render(root, html`
      <div class="v-top">
        <button type="button" class="ghost icon-btn v-close" data-v="close" aria-label="Close">${icon('close')}</button>
        <div class="v-title"><div class="v-name"></div><div class="v-meta"></div></div>
        <div class="v-acts" role="toolbar" aria-label="File actions"></div>
      </div>
      <div class="v-stage"><div class="v-track"></div></div>
      <div class="v-acts v-bottom" role="toolbar" aria-label="File actions"></div>
      <button type="button" class="v-nav v-prev" data-v="prev" aria-label="Previous">${icon('back')}</button>
      <button type="button" class="v-nav v-next" data-v="next" aria-label="Next">${icon('chevron')}</button>`);
    this.el = root;
    this.stage = root.querySelector('.v-stage');
    this.track = root.querySelector('.v-track');
    document.body.append(root);
    this.prevFocus = document.activeElement;
    root.querySelector('.v-close').focus({ preventScroll: true });
  }

  get item() { return this.items[this.index]; }

  /** Puts the current file and its neighbours in place, reusing slides that are already built. */
  layout() {
    const want = new Set([this.index - 1, this.index, this.index + 1].filter((j) => j >= 0 && j < this.items.length));
    for (const [j, s] of this.slots) if (!want.has(j)) { s.remove(); this.slots.delete(j); }
    for (const j of want) {
      let s = this.slots.get(j);
      if (!s) {
        s = document.createElement('div');
        s.className = 'v-slide';
        s.dataset.kind = viewKind(this.items[j].name);
        this.slots.set(j, s);
        this.track.append(s);
      }
      s.style.translate = (j - this.index) * 100 + '% 0';
      const active = j === this.index;
      s.toggleAttribute('data-current', active);
      s.setAttribute('aria-hidden', active ? 'false' : 'true');
      this.fill(s, this.items[j], active);
    }
    this.setTrack(0, false);
    this.resetZoom();
    this.chrome();
  }

  /** Builds a slide's content once; heavy kinds (video, audio, PDF, text) only when shown. */
  fill(s, it, active) {
    const k = s.dataset.kind;
    if (!active) for (const m of s.querySelectorAll('video, audio')) m.pause();
    if (s.dataset.filled === 'full' || (s.dataset.filled === 'light' && !active)) return;
    const src = this.opts.src ? this.opts.src(it) : '';
    const opener = this.opts.opener;
    if (k === 'image') {
      render(s, html`<img class="v-img" alt="${it.name}" src="${src}" draggable="false">`);
      s.dataset.filled = 'full';
      return;
    }
    if (!active) {
      render(s, html`<div class="v-card">${icon(KIND_ICON[k])}<div class="v-card-name">${it.name}</div></div>`);
      s.dataset.filled = 'light';
      return;
    }
    s.dataset.filled = 'full';
    if (k === 'video') render(s, html`<video class="v-media" controls playsinline preload="metadata" src="${src}"></video>`);
    else if (k === 'audio') render(s, html`<div class="v-card">${icon('music')}<div class="v-card-name">${it.name}</div><audio controls preload="metadata" src="${src}"></audio></div>`);
    else if (k === 'pdf' && !opener) {
      // The browser's own PDF viewer in a frame; "New tab" for when framing is refused.
      render(s, html`<iframe class="v-pdf" title="${it.name}" src="${src}"></iframe>
        <a class="btn v-newtab" href="${src}" target="_blank" rel="noopener">New tab</a>`);
    }
    else if (k === 'text') {
      render(s, html`<pre class="v-text" tabindex="0"></pre>`);
      this.loadText(s.firstElementChild, src);
    } else {
      const label = k === 'pdf' ? 'PDF document' : 'No preview for this kind of file';
      render(s, html`<div class="v-card">${icon(KIND_ICON[k])}<div class="v-card-name">${it.name}</div>
        <div class="v-card-sub">${label} · ${fmtSize(it.size)}</div>
        <div class="v-card-acts">${opener ? html`<button type="button" class="primary" data-v="open">Open</button>` : ''}
        <a class="btn ${opener ? '' : 'primary'}" data-v="download" href="${this.opts.download ? this.opts.download(it) : src}" download="${it.name}">${icon('download')}Download</a></div></div>`);
    }
  }

  async loadText(pre, src) {
    if (this.texts.has(src)) { pre.textContent = this.texts.get(src); return; }
    try {
      const res = await fetch(src, { headers: { Range: 'bytes=0-' + (TEXT_MAX - 1) } });
      if (!res.ok) throw new Error(res.statusText || 'could not read the file');
      let text = await res.text();
      if (res.status === 200 && text.length > TEXT_MAX) text = text.slice(0, TEXT_MAX);
      const total = Number((res.headers.get('Content-Range') || '').split('/')[1]);
      if (total > TEXT_MAX) text += '\n\n[… the first ' + fmtSize(TEXT_MAX) + ' of ' + fmtSize(total) + ']';
      this.texts.set(src, text);
      pre.textContent = text;
    } catch (e) {
      pre.textContent = 'Could not read this file: ' + (e.message || e);
    }
  }

  /** Title, position and the action buttons for the current file. */
  chrome() {
    const it = this.item;
    const n = this.items.length;
    this.el.querySelector('.v-name').textContent = it.name;
    this.el.querySelector('.v-meta').textContent = [fmtSize(it.size), n > 1 ? position(this.index, n) : ''].filter(Boolean).join(' · ');
    this.el.querySelector('.v-prev').hidden = this.index === 0;
    this.el.querySelector('.v-next').hidden = this.index === n - 1;
    const acts = this.opts.actions.filter((a) => !a.when || a.when(it));
    for (const bar of this.el.querySelectorAll('.v-acts')) render(bar, html`${acts.map((a) => html`<button type="button" class="ghost icon-btn ${a.danger ? 'v-danger' : ''}" data-v-act="${a.act}" aria-label="${a.label}" title="${a.label}">${icon(a.icon)}<span class="v-act-label">${a.label}</span></button>`)}`);
    for (const j of preloadTargets(this.items, this.index)) {
      if (!this.slots.has(j) && this.opts.src) { const im = new Image(); im.src = this.opts.src(this.items[j]); }
    }
  }

  setTrack(px, animate, done) {
    const t = this.track;
    const d = animate ? this.opts.duration : 0;
    t.style.transition = d ? `translate ${d}ms var(--ease, ease-out)` : 'none';
    t.style.translate = px + 'px 0';
    if (done) { if (d) setTimeout(done, d); else done(); }
  }

  /** Moves one file in dir (+1 or -1), with the slide animation. Returns whether it moved. */
  go(dir) {
    const j = stepIndex(this.index, dir, this.items.length);
    if (j === this.index || this.moving) { if (j === this.index) this.setTrack(0, true); return false; }
    this.moving = true;
    this.setTrack(-dir * this.stage.clientWidth, true, () => { this.moving = false; this.index = j; this.layout(); });
    if (!this.opts.duration) this.moving = false;
    return true;
  }

  /** Shows items[i] at once. */
  show(i) { if (i >= 0 && i < this.items.length) { this.index = i; this.layout(); } }

  /** Replaces one file (after a rename): its slide is rebuilt. */
  replace(i, it) {
    this.items[i] = it;
    const s = this.slots.get(i);
    if (s) { s.remove(); this.slots.delete(i); }
    this.layout();
  }

  /** Drops one file (after a delete) and shows the next one, or closes when none is left. */
  remove(i) {
    this.items.splice(i, 1);
    for (const s of this.slots.values()) s.remove();
    this.slots.clear();
    if (!this.items.length) { this.close(); return; }
    this.index = Math.min(this.index, this.items.length - 1);
    this.layout();
  }

  close() {
    if (this.closed) return;
    this.teardown();
    if (this.pushed && history.state?.solViewer) history.back();
  }

  teardown() {
    if (this.closed) return;
    this.closed = true;
    for (const m of this.el.querySelectorAll('video, audio')) m.pause();
    removeEventListener('keydown', this.onKey, true);
    removeEventListener('popstate', this.onPop);
    this.el.remove();
    if (current === this) current = null;
    if (this.prevFocus?.isConnected) this.prevFocus.focus({ preventScroll: true });
    this.opts.onClose?.();
  }

  open() {
    const it = this.item;
    const url = new URL(this.opts.src(it), location.href).href;
    const mime = MIME[viewKind(it.name)] || '';
    if (this.opts.opener(url, mime, it.name) === false) this.opts.onAction?.('open-failed', it, this);
  }

  // ---- input ----

  listen() {
    this.onKey = (e) => {
      if (document.querySelector('sol-dialog dialog[open], sol-sheet dialog[open], .menu')) return;
      if (/INPUT|TEXTAREA|SELECT/.test(document.activeElement?.tagName || '')) return;
      if (e.key === 'Escape') { e.preventDefault(); this.close(); } else if (e.key === 'ArrowRight') { e.preventDefault(); this.go(1); } else if (e.key === 'ArrowLeft') { e.preventDefault(); this.go(-1); }
    };
    addEventListener('keydown', this.onKey, true);
    this.onPop = () => { this.pushed = false; this.teardown(); };
    addEventListener('popstate', this.onPop);
    this.el.addEventListener('click', (e) => {
      const b = e.target.closest('[data-v], [data-v-act]');
      if (!b) return;
      const v = b.dataset.v;
      if (v === 'close') this.close();
      else if (v === 'next') this.go(1);
      else if (v === 'prev') this.go(-1);
      else if (v === 'open') this.open();
      else if (v === 'download') return; // the link does it
      else if (b.dataset.vAct) this.opts.onAction?.(b.dataset.vAct, this.item, this);
      e.preventDefault();
    });
    this.stage.addEventListener('dblclick', (e) => { if (e.target.closest('.v-img')) this.toggleZoom(e.clientX, e.clientY); });
    this.pointers = new Map();
    this.stage.addEventListener('pointerdown', (e) => this.down(e));
    this.stage.addEventListener('pointermove', (e) => this.move(e));
    for (const t of ['pointerup', 'pointercancel']) this.stage.addEventListener(t, (e) => this.up(e));
  }

  down(e) {
    if (e.pointerType === 'mouse' || this.moving) return;
    const tgt = e.target;
    if (tgt.closest('audio, button, a, input, iframe')) return;
    if (tgt.tagName === 'VIDEO' && e.clientY > tgt.getBoundingClientRect().bottom - 56) return; // its controls
    this.pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
    try { this.stage.setPointerCapture(e.pointerId); } catch { /* synthetic events have no capture */ }
    if (this.pointers.size === 2) { this.pinch = { d: this.spread(), s: this.zoom.s }; this.drag = null; return; }
    this.drag = { x0: e.clientX, y0: e.clientY, t0: performance.now(), dx: 0, dy: 0, zx: this.zoom.x, zy: this.zoom.y, horiz: null };
  }

  spread() {
    const [a, b] = [...this.pointers.values()];
    return Math.hypot(a.x - b.x, a.y - b.y) || 1;
  }

  move(e) {
    if (!this.pointers.has(e.pointerId)) return;
    this.pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
    if (this.pinch && this.pointers.size === 2) {
      this.zoom.s = Math.min(5, Math.max(1, this.pinch.s * this.spread() / this.pinch.d));
      this.applyZoom();
      return;
    }
    const d = this.drag;
    if (!d) return;
    d.dx = e.clientX - d.x0;
    d.dy = e.clientY - d.y0;
    if (this.zoom.s > 1) { this.zoom.x = d.zx + d.dx; this.zoom.y = d.zy + d.dy; this.applyZoom(); return; }
    if (d.horiz === null && Math.hypot(d.dx, d.dy) > 8) d.horiz = Math.abs(d.dx) > Math.abs(d.dy);
    if (d.horiz) this.setTrack(resist(d.dx, this.index, this.items.length), false);
  }

  up(e) {
    if (!this.pointers.delete(e.pointerId)) return;
    if (this.pinch) { if (this.pointers.size < 2) { this.pinch = null; if (this.zoom.s < 1.05) this.resetZoom(); } return; }
    const d = this.drag;
    this.drag = null;
    if (!d) return;
    const ms = performance.now() - d.t0;
    if (this.zoom.s > 1) return;
    if (Math.hypot(d.dx, d.dy) < 8 && e.type === 'pointerup') { this.tap(e, ms); return; }
    const intent = d.horiz ? swipeIntent(d.dx, d.dy, ms, this.stage.clientWidth || innerWidth) : null;
    if (intent === 'next' && this.go(1)) return;
    if (intent === 'prev' && this.go(-1)) return;
    this.setTrack(0, true);
  }

  /** One tap shows or hides the bars; two quick taps on an image zoom. */
  tap(e, ms) {
    const now = performance.now();
    if (ms < 300 && this.lastTap && now - this.lastTap.t < 300 && e.target.closest('.v-img')) {
      this.lastTap = null;
      clearTimeout(this.tapTimer);
      this.toggleZoom(e.clientX, e.clientY);
      return;
    }
    this.lastTap = { t: now };
    clearTimeout(this.tapTimer);
    this.tapTimer = setTimeout(() => { if (e.target.closest('.v-slide')) this.el.classList.toggle('v-bare'); }, 300);
  }

  toggleZoom(cx, cy) {
    if (this.zoom.s > 1) { this.resetZoom(); return; }
    const r = this.stage.getBoundingClientRect();
    const s = 2.5;
    this.zoom = { s, x: (r.left + r.width / 2 - cx) * (s - 1), y: (r.top + r.height / 2 - cy) * (s - 1) };
    this.applyZoom();
  }

  resetZoom() { this.zoom = { s: 1, x: 0, y: 0 }; this.applyZoom(); }

  applyZoom() {
    const img = this.slots.get(this.index)?.querySelector('.v-img');
    if (!img) return;
    const { s, x, y } = this.zoom;
    img.style.transform = s === 1 ? '' : `translate(${x}px, ${y}px) scale(${s})`;
    this.el.classList.toggle('v-zoomed', s > 1);
  }
}

