// Sol kit: a handful of plain custom elements (light DOM, styled by kit.css). No framework.
// Text is always set with textContent; nothing here parses API data as markup.
import { icons } from './icons.js';

const SVGNS = 'http://www.w3.org/2000/svg';

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

/** <sol-avatar name="Ada" id="person-id" online size="3rem">: initial on a disc tinted from the id. */
class SolAvatar extends HTMLElement {
  static observedAttributes = ['name', 'id', 'online', 'size'];
  connectedCallback() { this.render(); }
  attributeChangedCallback() { if (this.isConnected) this.render(); }
  render() {
    const name = this.getAttribute('name') || '?';
    const key = this.getAttribute('id') || name;
    let h = 0;
    for (const c of key) h = (h * 31 + c.codePointAt(0)) % 360;
    this.style.setProperty('--av-bg', `hsl(${h} 38% 50% / 0.2)`);
    this.style.setProperty('--av-fg', `hsl(${h} 45% 40%)`);
    if (this.hasAttribute('size')) this.style.setProperty('--size', this.getAttribute('size'));
    this.textContent = [...name.trim()][0]?.toUpperCase() || '?';
    this.setAttribute('role', 'img');
    this.setAttribute('aria-label', name + (this.hasAttribute('online') ? ', online' : ''));
  }
}

/**
 * <sol-app-shell>: put <nav slot="nav"> and <main> inside. Marks the nav landmark, and
 * set-current(href|id) / theme helpers live on the element.
 */
class SolAppShell extends HTMLElement {
  connectedCallback() {
    const nav = this.querySelector('[slot="nav"]');
    if (nav && !nav.hasAttribute('role')) nav.setAttribute('role', 'navigation');
    if (nav && !nav.hasAttribute('aria-label')) nav.setAttribute('aria-label', 'Main');
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
 * <sol-list>: a keyboard-navigable list. Children with data-item are rows (Up/Down/Home/End move,
 * Enter or Space fires "sol-select" with the row as detail). A child with slot="empty" shows
 * when there are no rows. Set the `loading` attribute only while remote data is on its way.
 */
class SolList extends HTMLElement {
  connectedCallback() {
    this.setAttribute('role', 'list');
    this.addEventListener('keydown', this.onKey);
    this.addEventListener('click', this.onClick);
    this.mo = new MutationObserver(() => this.sync());
    this.mo.observe(this, { childList: true });
    this.sync();
  }
  disconnectedCallback() {
    this.removeEventListener('keydown', this.onKey);
    this.removeEventListener('click', this.onClick);
    this.mo?.disconnect();
  }
  static observedAttributes = ['loading'];
  attributeChangedCallback() { if (this.isConnected) this.sync(); }
  get items() { return [...this.querySelectorAll(':scope > [data-item]')]; }
  sync() {
    const items = this.items;
    items.forEach((el, i) => { el.setAttribute('role', 'listitem'); el.tabIndex = i === 0 ? 0 : -1; });
    this.toggleAttribute('data-empty', items.length === 0 && !this.hasAttribute('loading'));
    const sk = this.querySelectorAll(':scope > .skeleton-row');
    if (this.hasAttribute('loading') && sk.length === 0) {
      for (let i = 0; i < 4; i++) { const s = document.createElement('div'); s.className = 'skeleton skeleton-row'; s.setAttribute('aria-hidden', 'true'); this.append(s); }
    } else if (!this.hasAttribute('loading')) sk.forEach((s) => s.remove());
  }
  onClick = (e) => { const row = e.target.closest?.('[data-item]'); if (row && this.contains(row)) this.dispatchEvent(new CustomEvent('sol-select', { detail: row, bubbles: true })); };
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
    if (next >= 0) { e.preventDefault(); items.forEach((el) => { el.tabIndex = -1; }); items[next].tabIndex = 0; items[next].focus(); }
  };
}

/**
 * <sol-dialog heading="Share folder">: modal on the native <dialog> (focus trap, Esc, inert
 * page). Children become the body. Methods: show(), close(); fires "close".
 */
class SolDialog extends HTMLElement {
  connectedCallback() {
    if (this.dlg) return;
    const body = document.createElement('div');
    body.className = 'dlg-body';
    body.append(...this.childNodes);
    const head = document.createElement('div');
    head.className = 'dlg-head';
    const h = document.createElement('h2');
    h.id = 'dlg-' + Math.random().toString(36).slice(2, 8);
    h.textContent = this.getAttribute('heading') || '';
    const x = document.createElement('button');
    x.className = 'ghost'; x.type = 'button'; x.setAttribute('aria-label', 'Close');
    const ic = document.createElement('sol-icon'); ic.setAttribute('name', 'close'); x.append(ic);
    x.addEventListener('click', () => this.close());
    head.append(h, x);
    this.dlg = document.createElement('dialog');
    this.dlg.setAttribute('aria-labelledby', h.id);
    this.dlg.append(head, body);
    this.dlg.addEventListener('close', () => this.dispatchEvent(new Event('close')));
    this.dlg.addEventListener('click', (e) => { if (e.target === this.dlg) this.close(); });
    this.append(this.dlg);
  }
  show() { if (!this.dlg.open) this.dlg.showModal(); }
  close() { this.dlg.close(); }
  get open() { return !!this.dlg?.open; }
}

/**
 * <sol-toast>: one per page (created on first use). SolToast.show(message, {kind, ms}); kind is
 * "info" or "error". Announced politely to screen readers.
 */
class SolToast extends HTMLElement {
  connectedCallback() { this.setAttribute('role', 'status'); this.setAttribute('aria-live', 'polite'); }
  static show(message, { kind = 'info', ms = 4000 } = {}) {
    let host = document.querySelector('sol-toast');
    if (!host) { host = document.createElement('sol-toast'); document.body.append(host); }
    const t = document.createElement('div');
    t.className = 'toast'; t.dataset.kind = kind;
    t.textContent = message;
    host.append(t);
    setTimeout(() => t.remove(), ms);
    return t;
  }
}

for (const [name, cls] of [['sol-icon', SolIcon], ['sol-avatar', SolAvatar], ['sol-app-shell', SolAppShell], ['sol-list', SolList], ['sol-dialog', SolDialog], ['sol-toast', SolToast]]) {
  if (!customElements.get(name)) customElements.define(name, cls);
}

/** Shows a toast. */
export const toast = SolToast.show;

/** Sets the theme: "light", "dark", or "auto" (follow the system). Remembered per origin. */
export function setTheme(theme) {
  const root = document.documentElement;
  if (theme === 'auto') root.removeAttribute('data-theme'); else root.setAttribute('data-theme', theme);
  try { localStorage.setItem('sol-theme', theme); } catch { /* storage may be unavailable */ }
}
try {
  const t = localStorage.getItem('sol-theme');
  if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t);
} catch { /* storage may be unavailable */ }

export { SolIcon, SolAvatar, SolAppShell, SolList, SolDialog, SolToast };
