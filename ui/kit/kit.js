// Sol kit: plain custom elements (light DOM, styled by kit.css) plus a few helpers. No framework.
// Text is always set with textContent; nothing here parses API data as markup.
import { icons } from './icons.js';

const SVGNS = 'http://www.w3.org/2000/svg';
const phone = () => matchMedia('(max-width: 760px)').matches;
const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
const store = {
  get(k) { try { return localStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* storage may be unavailable */ } },
};

/** <sol-icon name="home" label="Home">: inline SVG stroke icon; decorative unless label is set. */
class SolIcon extends HTMLElement {
  static observedAttributes = ['name', 'label'];
  connectedCallback() { this.render(); }
  attributeChangedCallback() { if (this.isConnected) this.render(); }
  render() {
    const svg = document.createElementNS(SVGNS, 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    for (const d of icons[this.getAttribute('name')] || []) {
      const p = document.createElementNS(SVGNS, 'path');
      p.setAttribute('d', d);
      svg.append(p);
    }
    const label = this.getAttribute('label');
    if (label) { svg.setAttribute('role', 'img'); svg.setAttribute('aria-label', label); } else svg.setAttribute('aria-hidden', 'true');
    this.replaceChildren(svg);
  }
}

/** <sol-avatar name="Ada" id="person-id" online size="40px">: initial on a disc tinted from the id; `online` adds the presence ring. */
class SolAvatar extends HTMLElement {
  static observedAttributes = ['name', 'id', 'online', 'size'];
  connectedCallback() { this.render(); }
  attributeChangedCallback() { if (this.isConnected) this.render(); }
  render() {
    const name = this.getAttribute('name') || '?';
    const key = this.getAttribute('id') || name;
    let h = 0;
    for (const c of key) h = (h * 31 + c.codePointAt(0)) % 360;
    this.style.setProperty('--av-bg', `hsl(${h} 45% 52% / 0.22)`);
    this.style.setProperty('--av-fg', `light-dark(hsl(${h} 50% 32%), hsl(${h} 60% 74%))`);
    if (this.hasAttribute('size')) this.style.setProperty('--size', this.getAttribute('size'));
    this.textContent = [...name.trim()][0]?.toUpperCase() || '?';
    this.setAttribute('role', 'img');
    this.setAttribute('aria-label', name + (this.hasAttribute('online') ? ', online' : ''));
  }
}

/** <sol-app-icon glyph="folder" hue="210" size="60px">: a squircle with a gradient and a glyph. No hue means the accent. */
class SolAppIcon extends HTMLElement {
  static observedAttributes = ['glyph', 'hue', 'size'];
  connectedCallback() { this.render(); }
  attributeChangedCallback() { if (this.isConnected) this.render(); }
  render() {
    const hue = this.getAttribute('hue');
    if (hue !== null && hue !== '') {
      const h = Number(hue) || 0;
      this.style.setProperty('--icon-a', `hsl(${h} 82% 62%)`);
      this.style.setProperty('--icon-b', `hsl(${(h + 14) % 360} 78% 46%)`);
    } else { this.style.removeProperty('--icon-a'); this.style.removeProperty('--icon-b'); this.toggleAttribute('data-dark-glyph', true); }
    if (hue !== null && hue !== '') this.removeAttribute('data-dark-glyph');
    if (this.hasAttribute('size')) this.style.setProperty('--size', this.getAttribute('size'));
    const ic = el('sol-icon');
    ic.setAttribute('name', this.getAttribute('glyph') || 'file');
    this.replaceChildren(ic);
    this.setAttribute('role', 'img');
    if (!this.hasAttribute('aria-label')) this.setAttribute('aria-hidden', 'true');
  }
}

/** <sol-ring value="62" size="44px">: progress ring, value 0..100; `indeterminate` spins. Content is the label. */
class SolRing extends HTMLElement {
  static observedAttributes = ['value', 'size'];
  connectedCallback() { this.setAttribute('role', 'progressbar'); this.setAttribute('aria-valuemin', '0'); this.setAttribute('aria-valuemax', '100'); this.render(); }
  attributeChangedCallback() { if (this.isConnected) this.render(); }
  render() {
    const v = Math.max(0, Math.min(100, Number(this.getAttribute('value')) || 0));
    this.style.setProperty('--v', String(v));
    if (this.hasAttribute('size')) this.style.setProperty('--size', this.getAttribute('size'));
    if (this.hasAttribute('indeterminate')) this.removeAttribute('aria-valuenow'); else this.setAttribute('aria-valuenow', String(Math.round(v)));
  }
}

/**
 * <sol-app-shell heading="Files">: put <nav slot="nav"> and <main> inside.
 * Desktop: a glass source-list sidebar (collapsible) and a unified glass toolbar showing the page
 * title. Phone (760px and narrower): the nav becomes a glass tab bar, and a `.large-title`
 * element in <main> collapses into the glass nav bar as you scroll.
 * The toolbar title follows the first `.large-title` in <main> (or the `heading` attribute, or
 * setHeading()). Elements with slot="actions" are moved into the toolbar.
 */
class SolAppShell extends HTMLElement {
  connectedCallback() {
    if (this.bar) return;
    const nav = this.querySelector(':scope > [slot="nav"]');
    if (nav && !nav.hasAttribute('role')) nav.setAttribute('role', 'navigation');
    if (nav && !nav.hasAttribute('aria-label')) nav.setAttribute('aria-label', 'Main');
    this.main = this.querySelector(':scope > main');
    this.bar = el('header', 'shell-bar');
    this.toggle = el('button', 'ghost icon-btn sidebar-toggle');
    this.toggle.type = 'button';
    this.toggle.setAttribute('aria-label', 'Show or hide the sidebar');
    const ic = el('sol-icon'); ic.setAttribute('name', 'sidebar'); this.toggle.append(ic);
    this.toggle.addEventListener('click', () => this.toggleSidebar());
    this.titleEl = el('div', 'bar-title');
    this.titleEl.setAttribute('role', 'heading'); this.titleEl.setAttribute('aria-level', '1');
    this.actions = el('div', 'bar-actions');
    for (const a of this.querySelectorAll(':scope > [slot="actions"]')) this.actions.append(a);
    this.bar.append(this.toggle, this.titleEl, this.actions);
    this.insertBefore(this.bar, this.main);
    if (store.get('sol-sidebar') === 'hidden') this.setAttribute('data-sidebar', 'hidden');
    this.setHeading(this.getAttribute('heading') || '');
    if (this.main) {
      this.main.addEventListener('scroll', this.onScroll, { passive: true });
      this.mo = new MutationObserver(() => this.syncTitle());
      this.mo.observe(this.main, { childList: true, subtree: true });
      this.syncTitle();
    }
  }
  disconnectedCallback() { this.mo?.disconnect(); }
  onScroll = () => this.toggleAttribute('data-collapsed', this.main.scrollTop > 36);
  syncTitle() {
    const t = this.main.querySelector('.large-title');
    if (t && t.textContent.trim() && this.titleEl.textContent !== t.textContent.trim()) this.titleEl.textContent = t.textContent.trim();
    this.onScroll();
  }
  /** Sets the toolbar title (used when <main> has no .large-title). */
  setHeading(text) { this.titleEl.textContent = text; }
  /** Shows or hides the desktop sidebar; remembered per origin. */
  toggleSidebar(force) {
    const hide = force === undefined ? !this.hasAttribute('data-sidebar') : !force;
    if (hide) this.setAttribute('data-sidebar', 'hidden'); else this.removeAttribute('data-sidebar');
    store.set('sol-sidebar', hide ? 'hidden' : 'shown');
  }
  /** Marks the nav item whose data-id (or href) equals `id` as the current page. */
  setCurrent(id) {
    for (const a of this.querySelectorAll('.nav-item')) {
      const on = a.dataset.id === id || a.getAttribute('href') === id;
      if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    }
  }
}

/**
 * <sol-list>: an inset grouped list, keyboard-navigable. Children with data-item are rows (Up/Down/
 * Home/End move, Enter or Space fires "sol-select" with the row as detail). A child with
 * slot="empty" shows when there are no rows. Set `loading` only while remote data is on its way.
 * Row hooks: data-chevron (disclosure arrow), aria-selected / .sel (selection), and a
 * `.swipe-actions` child with buttons for swipe actions on touch screens.
 */
class SolList extends HTMLElement {
  connectedCallback() {
    this.setAttribute('role', 'list');
    this.addEventListener('keydown', this.onKey);
    this.addEventListener('click', this.onClick);
    this.addEventListener('pointerdown', this.onDown);
    this.mo = new MutationObserver(() => this.sync());
    this.mo.observe(this, { childList: true });
    this.sync();
  }
  disconnectedCallback() {
    this.removeEventListener('keydown', this.onKey);
    this.removeEventListener('click', this.onClick);
    this.removeEventListener('pointerdown', this.onDown);
    this.mo?.disconnect();
  }
  static observedAttributes = ['loading'];
  attributeChangedCallback() { if (this.isConnected) this.sync(); }
  get items() { return [...this.querySelectorAll(':scope > [data-item]')]; }
  sync() {
    const items = this.items;
    items.forEach((r, i) => { r.setAttribute('role', 'listitem'); r.tabIndex = i === 0 ? 0 : -1; if (r.querySelector(':scope > .swipe-actions')) r.style.touchAction = 'pan-y'; });
    this.toggleAttribute('data-empty', items.length === 0 && !this.hasAttribute('loading'));
    const sk = this.querySelectorAll(':scope > .skeleton-row');
    if (this.hasAttribute('loading') && sk.length === 0) {
      for (let i = 0; i < 4; i++) { const s = el('div', 'skeleton skeleton-row'); s.setAttribute('aria-hidden', 'true'); this.append(s); }
    } else if (!this.hasAttribute('loading')) sk.forEach((s) => s.remove());
  }
  onClick = (e) => {
    const row = e.target.closest?.('[data-item]');
    if (!row || !this.contains(row)) return;
    if (row.hasAttribute('data-swipe') && row.dataset.swipe === 'open' && !e.target.closest('.swipe-actions')) { this.closeSwipe(row); return; }
    this.dispatchEvent(new CustomEvent('sol-select', { detail: row, bubbles: true }));
  };
  onKey = (e) => {
    const row = e.target.closest?.('[data-item]');
    if (!row) return;
    const items = this.items;
    const i = items.indexOf(row);
    let next = -1;
    if (e.key === 'ArrowDown') next = Math.min(i + 1, items.length - 1);
    else if (e.key === 'ArrowUp') next = Math.max(i - 1, 0);
    else if (e.key === 'Home') next = 0;
    else if (e.key === 'End') next = items.length - 1;
    else if ((e.key === 'Enter' || e.key === ' ') && e.target === row) { e.preventDefault(); row.click(); return; }
    if (next >= 0) { e.preventDefault(); items.forEach((r) => { r.tabIndex = -1; }); items[next].tabIndex = 0; items[next].focus(); }
  };
  closeSwipe(row) { row.removeAttribute('data-swipe'); row.style.removeProperty('--sx'); }
  // Swipe actions: drag a row left on a touch screen to reveal its .swipe-actions buttons.
  onDown = (e) => {
    if (e.pointerType !== 'touch') return;
    const row = e.target.closest?.('[data-item]');
    const acts = row && row.querySelector(':scope > .swipe-actions');
    if (!acts || !this.contains(row)) return;
    for (const r of this.items) if (r !== row && r.dataset.swipe === 'open') this.closeSwipe(r);
    const w = acts.offsetWidth, was = row.dataset.swipe === 'open' ? -w : 0;
    const x0 = e.clientX, y0 = e.clientY;
    let dragging = false;
    const move = (m) => {
      const dx = m.clientX - x0, dy = m.clientY - y0;
      if (!dragging) { if (Math.abs(dy) > 10 && Math.abs(dy) > Math.abs(dx)) return end(); if (Math.abs(dx) < 8) return; dragging = true; row.dataset.swipe = 'drag'; }
      row.style.setProperty('--sx', Math.max(-w - 24, Math.min(0, was + dx)) + 'px');
    };
    const end = () => {
      removeEventListener('pointermove', move); removeEventListener('pointerup', end); removeEventListener('pointercancel', end);
      if (!dragging) return;
      const sx = parseFloat(row.style.getPropertyValue('--sx')) || 0;
      if (sx < -w / 2) { row.dataset.swipe = 'open'; row.style.setProperty('--sx', -w + 'px'); } else this.closeSwipe(row);
    };
    addEventListener('pointermove', move); addEventListener('pointerup', end); addEventListener('pointercancel', end);
  };
}

/**
 * <sol-dialog heading="Share folder"> (alias <sol-sheet>): on the native <dialog> (focus trap, Esc,
 * inert page). A bottom sheet with a grabber on phones (drag it down to dismiss), a centred dialog
 * on desktop. Children become the body. Methods: show(), close(); fires "close".
 */
class SolDialog extends HTMLElement {
  connectedCallback() {
    if (this.dlg) return;
    const body = el('div', 'dlg-body');
    body.append(...this.childNodes);
    const grab = el('div', 'grabber'); grab.setAttribute('aria-hidden', 'true');
    const head = el('div', 'dlg-head');
    const h = el('h2', '', this.getAttribute('heading') || '');
    h.id = 'dlg-' + Math.random().toString(36).slice(2, 8);
    const x = el('button', 'ghost icon-btn'); x.type = 'button'; x.setAttribute('aria-label', 'Close');
    const ic = el('sol-icon'); ic.setAttribute('name', 'close'); x.append(ic);
    x.addEventListener('click', () => this.close());
    head.append(h, x);
    this.dlg = el('dialog');
    this.dlg.setAttribute('aria-labelledby', h.id);
    this.dlg.append(grab, head, body);
    this.dlg.addEventListener('close', () => this.dispatchEvent(new Event('close')));
    this.dlg.addEventListener('click', (e) => { if (e.target === this.dlg) this.close(); });
    this.dlg.addEventListener('pointerdown', (e) => this.onDrag(e, grab, head));
    this.append(this.dlg);
  }
  onDrag(e, grab, head) {
    if (!phone() || !(grab.contains(e.target) || head.contains(e.target)) || e.target.closest('button')) return;
    const y0 = e.clientY, d = this.dlg;
    let dy = 0;
    d.style.transition = 'none';
    const move = (m) => { dy = Math.max(0, m.clientY - y0); d.style.translate = `0 ${dy}px`; };
    const end = () => {
      removeEventListener('pointermove', move); removeEventListener('pointerup', end); removeEventListener('pointercancel', end);
      d.style.transition = ''; d.style.translate = '';
      if (dy > 100) this.close();
    };
    addEventListener('pointermove', move); addEventListener('pointerup', end); addEventListener('pointercancel', end);
  }
  show() { if (!this.dlg.open) this.dlg.showModal(); }
  close() { this.dlg.close(); }
  get open() { return !!this.dlg?.open; }
}
class SolSheet extends SolDialog {}

/**
 * <sol-toast>: one per page (created on first use). SolToast.show(message, {kind, ms}); kind is
 * "ok" (default), "info" or "error". A banner that slides in from the top, announced politely.
 */
class SolToast extends HTMLElement {
  connectedCallback() { this.setAttribute('role', 'status'); this.setAttribute('aria-live', 'polite'); }
  static show(message, { kind = 'ok', ms = 4000 } = {}) {
    let host = document.querySelector('sol-toast');
    if (!host) { host = document.createElement('sol-toast'); document.body.append(host); }
    const t = el('div', 'toast', message);
    t.dataset.kind = kind;
    host.append(t);
    setTimeout(() => { t.dataset.leaving = ''; setTimeout(() => t.remove(), 220); }, ms);
    return t;
  }
}

for (const [name, cls] of [['sol-icon', SolIcon], ['sol-avatar', SolAvatar], ['sol-app-icon', SolAppIcon], ['sol-ring', SolRing], ['sol-app-shell', SolAppShell], ['sol-list', SolList], ['sol-dialog', SolDialog], ['sol-sheet', SolSheet], ['sol-toast', SolToast]]) {
  if (!customElements.get(name)) customElements.define(name, cls);
}

// ---- menus and popovers: one floating layer at a time ----
let layer = null;
function closeLayer() {
  if (!layer) return;
  const l = layer; layer = null;
  removeEventListener('pointerdown', l.outside, true); removeEventListener('keydown', l.key, true); removeEventListener('blur', l.close); removeEventListener('resize', l.close);
  l.node.remove();
  if (l.returnFocus?.isConnected) l.returnFocus.focus({ preventScroll: true });
}
function place(node, { x, y, anchor, placement = 'bottom' }) {
  document.body.append(node);
  const r = node.getBoundingClientRect(), m = 8;
  let px = x ?? 0, py = y ?? 0;
  if (anchor) {
    const a = anchor.getBoundingClientRect();
    px = placement === 'bottom-end' ? a.right - r.width : a.left;
    py = placement === 'top' ? a.top - r.height - 6 : a.bottom + 6;
    if (py + r.height > innerHeight - m && a.top - r.height - 6 > m) py = a.top - r.height - 6;
  }
  px = Math.max(m, Math.min(px, innerWidth - r.width - m));
  py = Math.max(m, Math.min(py, innerHeight - r.height - m));
  node.style.left = px + 'px'; node.style.top = py + 'px';
  node.style.setProperty('--ox', (x ?? (anchor?.getBoundingClientRect().left ?? px)) - px + 'px'); node.style.setProperty('--oy', py < (y ?? py) ? '100%' : '0');
}
function openLayer(node, pos, { onKey } = {}) {
  closeLayer();
  const returnFocus = document.activeElement;
  place(node, pos);
  const outside = (e) => { if (!node.contains(e.target)) closeLayer(); };
  const key = (e) => { if (e.key === 'Escape') { e.preventDefault(); closeLayer(); } else onKey?.(e); };
  layer = { node, outside, key, close: closeLayer, returnFocus };
  addEventListener('pointerdown', outside, true); addEventListener('keydown', key, true); addEventListener('blur', closeLayer); addEventListener('resize', closeLayer);
  return closeLayer;
}

/**
 * Shows a context menu at {x, y} or under {anchor}. Items: {label, icon, danger, disabled,
 * onSelect} or {separator: true}. Returns a function that closes it. Arrow keys move, Enter
 * selects, Esc closes.
 */
export function showMenu(items, pos) {
  const menu = el('div', 'menu');
  menu.setAttribute('role', 'menu');
  const btns = [];
  for (const it of items) {
    if (it.separator) { const s = el('div'); s.setAttribute('role', 'separator'); menu.append(s); continue; }
    const b = el('button', it.danger ? 'danger' : '', it.label);
    b.type = 'button'; b.setAttribute('role', 'menuitem'); b.disabled = !!it.disabled;
    if (it.icon) { const ic = el('sol-icon'); ic.setAttribute('name', it.icon); b.append(ic); }
    b.addEventListener('click', () => { closeLayer(); it.onSelect?.(); });
    btns.push(b); menu.append(b);
  }
  const move = (e) => {
    const live = btns.filter((b) => !b.disabled), i = live.indexOf(document.activeElement);
    let n = -1;
    if (e.key === 'ArrowDown') n = (i + 1) % live.length; else if (e.key === 'ArrowUp') n = (i - 1 + live.length) % live.length;
    else if (e.key === 'Home') n = 0; else if (e.key === 'End') n = live.length - 1;
    if (n >= 0) { e.preventDefault(); live[n].focus(); }
  };
  const close = openLayer(menu, pos, { onKey: move });
  btns.find((b) => !b.disabled)?.focus({ preventScroll: true });
  return close;
}

/**
 * Binds a context menu to `target`: right-click on desktop, long-press on touch. `build(event)`
 * returns the items (or an empty list to show nothing). Returns an unbind function.
 */
export function contextMenu(target, build) {
  const open = (x, y, ev) => { const items = build(ev); if (items && items.length) showMenu(items, { x, y }); };
  const onCtx = (e) => { e.preventDefault(); open(e.clientX, e.clientY, e); };
  let timer = 0, sx = 0, sy = 0, fired = false;
  const down = (e) => {
    if (e.pointerType === 'mouse') return;
    sx = e.clientX; sy = e.clientY; fired = false;
    timer = setTimeout(() => { fired = true; open(sx, sy, e); }, 500);
  };
  const cancel = () => clearTimeout(timer);
  const move = (e) => { if (Math.hypot(e.clientX - sx, e.clientY - sy) > 8) cancel(); };
  const click = (e) => { if (fired) { e.preventDefault(); e.stopPropagation(); fired = false; } };
  target.addEventListener('contextmenu', onCtx);
  target.addEventListener('pointerdown', down);
  target.addEventListener('pointermove', move);
  target.addEventListener('pointerup', cancel);
  target.addEventListener('pointercancel', cancel);
  target.addEventListener('click', click, true);
  return () => {
    cancel();
    target.removeEventListener('contextmenu', onCtx); target.removeEventListener('pointerdown', down); target.removeEventListener('pointermove', move);
    target.removeEventListener('pointerup', cancel); target.removeEventListener('pointercancel', cancel); target.removeEventListener('click', click, true);
  };
}

/**
 * Shows a popover next to `anchor` with `content` (a Node, or a string shown as text).
 * Options: {placement: 'bottom' | 'bottom-end' | 'top'}. Returns {el, close}.
 */
export function showPopover(anchor, content, { placement = 'bottom' } = {}) {
  const p = el('div', 'popover');
  p.setAttribute('role', 'dialog');
  p.append(content instanceof Node ? content : document.createTextNode(String(content)));
  const close = openLayer(p, { anchor, placement });
  return { el: p, close };
}
export const closePopover = closeLayer;

/** Shows a toast. */
export const toast = SolToast.show;

// ---- theme and settings ----
const SETTING_KEY = 'sol-settings';
const SETTINGS = { density: ['compact'], radius: ['square'], glass: ['off'], font: ['system', 'inter', 'mono'] };

/**
 * Applies appearance settings to <html>: {scheme: 'auto'|'light'|'dark', accent: '#rrggbb' | '',
 * density: 'comfortable'|'compact', radius: 'rounded'|'square', glass: 'on'|'off',
 * font: 'system'|'inter'|'mono'}. Unknown or default values remove the attribute. Remembered per
 * origin so the next page paints right away. Only the keys present are changed.
 */
export function applySettings(s = {}) {
  const root = document.documentElement;
  if ('scheme' in s) { if (s.scheme === 'light' || s.scheme === 'dark') root.setAttribute('data-theme', s.scheme); else root.removeAttribute('data-theme'); }
  for (const k of Object.keys(SETTINGS)) {
    if (!(k in s)) continue;
    if (SETTINGS[k].includes(s[k])) root.setAttribute('data-' + k, s[k]); else root.removeAttribute('data-' + k);
  }
  if ('accent' in s) { if (/^#[0-9a-f]{3,8}$/i.test(s.accent || '')) root.style.setProperty('--tint', s.accent); else root.style.removeProperty('--tint'); }
  let all = {};
  try { all = JSON.parse(store.get(SETTING_KEY) || '{}'); } catch { all = {}; }
  store.set(SETTING_KEY, JSON.stringify({ ...all, ...s }));
}

/** Sets the colour scheme: "light", "dark", or "auto" (follow the system). Remembered per origin. */
export function setTheme(theme) {
  applySettings({ scheme: theme });
  store.set('sol-theme', theme);
}

/**
 * Loads the active theme (/_sol/theme.css) after every other stylesheet. A 404 (no theme
 * set, or a page not served by the daemon) is ignored: the link is simply removed.
 */
export function loadTheme(href = '/_sol/theme.css') {
  if (document.getElementById('sol-theme')) return;
  const l = document.createElement('link');
  l.rel = 'stylesheet'; l.id = 'sol-theme'; l.href = href;
  l.addEventListener('error', () => l.remove());
  document.head.append(l);
}

try {
  const t = store.get('sol-theme'), saved = JSON.parse(store.get(SETTING_KEY) || '{}');
  if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t);
  if (saved && typeof saved === 'object') applySettings(saved);
} catch { /* a bad saved value is ignored */ }
// ?theme=light|dark forces a scheme for this page view only (screenshots, bug reports); nothing is remembered.
try { const q = new URLSearchParams(location.search).get('theme'); if (q === 'light' || q === 'dark') document.documentElement.setAttribute('data-theme', q); } catch { /* not in a page */ }
try { if (location.hostname.endsWith('.localhost')) document.documentElement.setAttribute('data-app', location.hostname.split('.')[0]); } catch { /* not in a page */ }
if (!document.querySelector('meta[name="sol-theme"][content="off"]')) loadTheme();

export { SolIcon, SolAvatar, SolAppIcon, SolRing, SolAppShell, SolList, SolDialog, SolSheet, SolToast };
