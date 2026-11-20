// The kit demo page (demo.html): swatches, icons, appearance controls and floating things.
import { icons } from './icons.js';
import { applySettings, showMenu, contextMenu, showPopover, toast } from './kit.js';

if (location.search.includes('embed')) document.body.classList.add('embed');
const $ = (id) => document.getElementById(id);
const shell = document.querySelector('sol-app-shell');
const hash = () => shell.setCurrent(location.hash || '#appearance');
addEventListener('hashchange', hash); hash();

// swatches read the live computed tokens, so a theme switch shows up at once
const toks = ['--canvas', '--surface', '--surface-2', '--glass', '--label', '--label-2', '--label-3', '--separator', '--tint', '--tint-hover', '--tint-pressed', '--tint-subtle', '--on-tint', '--green', '--red', '--blue', '--orange'];
for (const t of toks) {
  const d = document.createElement('div'); d.className = 'sw';
  const i = document.createElement('i'); i.style.background = `var(${t})`;
  const s = document.createElement('span'); s.textContent = t;
  d.append(i, s); $('colors').append(d);
}
for (const name of Object.keys(icons)) {
  const d = document.createElement('div'), ic = document.createElement('sol-icon');
  ic.setAttribute('name', name); d.append(ic, document.createTextNode(name)); $('icon-grid').append(d);
}

// appearance controls: the same settings the Sol app writes
const state = JSON.parse(localStorage.getItem('demo-state') || '{}');
const save = () => localStorage.setItem('demo-state', JSON.stringify(state));
function apply() {
  $('demo-theme').href = `themes/${state.theme || 'sol'}.css`;
  applySettings({ scheme: state.scheme || 'auto', accent: state.accent || '', density: state.density ? 'compact' : 'comfortable', radius: state.radius ? 'square' : 'rounded', glass: state.glass === false ? 'off' : 'on', font: state.font || 'system' });
  $('scheme').querySelectorAll('button').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.v === (state.scheme || 'auto'))));
  $('theme').value = state.theme || 'sol'; $('font').value = state.font || 'system';
  $('density').checked = !!state.density; $('radius').checked = !!state.radius; $('glass').checked = state.glass !== false;
  save();
  const f = $('phone'); if ($('phone-dlg').open) f.contentWindow?.location.reload();
}
$('scheme').addEventListener('click', (e) => { const v = e.target.closest('button')?.dataset.v; if (v) { state.scheme = v; apply(); } });
$('theme').addEventListener('change', (e) => { state.theme = e.target.value; apply(); });
$('font').addEventListener('change', (e) => { state.font = e.target.value; apply(); });
$('accent').addEventListener('input', (e) => { state.accent = e.target.value; apply(); });
$('accent-reset').addEventListener('click', () => { state.accent = ''; apply(); });
for (const k of ['density', 'radius']) $(k).addEventListener('change', (e) => { state[k] = e.target.checked; apply(); });
$('glass').addEventListener('change', (e) => { state.glass = e.target.checked; apply(); });
apply();

// floating things
const items = () => [
  { label: 'Open', icon: 'folder', onSelect: () => toast('Opened') },
  { label: 'Share', icon: 'share', onSelect: () => toast('Shared') },
  { label: 'Copy link', icon: 'link', onSelect: () => toast('Copied') },
  { separator: true },
  { label: 'Move to Trash', icon: 'trash', danger: true, onSelect: () => toast('Moved to Trash') },
];
$('menu-btn').addEventListener('click', (e) => showMenu(items(), { anchor: e.currentTarget }));
contextMenu($('menu-zone'), items);
$('pop-btn').addEventListener('click', (e) => {
  const c = document.createElement('div');
  const h = document.createElement('h3'); h.textContent = 'Storage';
  const p = document.createElement('p'); p.className = 'muted'; p.textContent = '42% of 256 GB used on this device.';
  c.append(h, p);
  showPopover(e.currentTarget, c);
});
$('sheet-btn').addEventListener('click', () => $('sheet').show());
$('sheet').addEventListener('click', (e) => { if (e.target.closest('[data-close]')) $('sheet').close(); });
$('toast-btn').addEventListener('click', () => toast('Shared'));
$('toast-err').addEventListener('click', () => toast('Could not reach Bo. Check that their device is online.', { kind: 'error' }));
$('phone-btn').addEventListener('click', () => { $('phone').src = 'demo.html?embed'; $('phone-dlg').show(); });
