// The Sol app: Home, People, Devices, Inbox, Sharing and Settings (design/ADMIN.md). It reads the
// admin API on the same origin and stays current from /admin/v1/events (no polling). Every value
// from the API reaches the DOM through html`` (escaped). Dialogs live outside the re-rendered
// screens, so a live update never wipes a form you are typing in.
import { html, raw, render, esc } from '/sol.js';
import { applySettings, toast } from '/kit/kit.js';
import { icons } from '/kit/icons.js';
import '/kit/qr.js';
import { createAdmin } from '/admin.js';
import { greetingFor, clockText, dateText, msToNextMinute, relTime, minutesLeft, bytes, appIconSpec, parseRoute } from '/util.js';

const $ = (id) => document.getElementById(id);
// The UI root forbids inline styles (CSP style-src 'self'): markup uses classes and data-var-*
// (sol.js render). Any violation is kept for the self-test to report.
const cspViolations = [];
document.addEventListener('securitypolicyviolation', (e) => cspViolations.push(`${e.violatedDirective}: ${e.blockedURI || 'inline'} ${e.sample || ''}`.trim()));
const phoneQ = matchMedia('(max-width: 760px)');

// ---------- state ----------

const KEYS = ['status', 'people', 'devices', 'apps', 'services', 'tunnels', 'inbox', 'theme', 'themes', 'backup', 'agents'];
const state = Object.fromEntries(KEYS.map((k) => [k, null]));
let locked = false;
let route = parseRoute(location.hash);

// Local data first: paint the last snapshot at once, then refresh.
const CACHE = 'sol-home-cache';
try { Object.assign(state, JSON.parse(localStorage.getItem(CACHE) || '{}')); } catch { /* optional */ }
const saveCache = () => { try { localStorage.setItem(CACHE, JSON.stringify(Object.fromEntries(KEYS.filter((k) => k !== 'backup').map((k) => [k, state[k]])))); } catch { /* optional */ } };

const admin = createAdmin({ onUnauthorized: () => { locked = true; draw(); } });
const fetchers = {
  status: () => admin.status(), people: () => admin.people(), devices: () => admin.devices(), apps: () => admin.apps(),
  services: () => admin.services(), tunnels: () => admin.tunnels(), inbox: () => admin.inbox(), theme: () => admin.theme(),
  themes: () => admin.themes(), backup: () => admin.backup(), agents: () => admin.agents(),
};
async function load(key) {
  try {
    state[key] = await fetchers[key]();
    if (key === 'theme') applyTheme();
    saveCache(); draw();
  } catch (e) { if (e.status !== 401) console.warn('load', key, e.message); }
}
const loadAll = () => Promise.all(KEYS.map(load));
const EVENT_KEYS = { status: ['status'], person: ['people'], device: ['devices'], session: ['status'], app: ['apps'], inbox: ['inbox'], service: ['services'], tunnel: ['tunnels'], theme: ['theme'] };

// ---------- helpers ----------

const ic = (n) => html`<sol-icon name="${n}"></sol-icon>`;
const list = (v) => v || [];
const meName = () => state.status?.me?.person?.name || '';
const nameOf = (p) => p.petname || p.name || p.id;
const fail = (e) => { if (e?.status !== 401) toast(e?.message || String(e), { kind: 'error' }); };
const act = async (fn, done) => { try { await fn(); if (done) toast(done); } catch (e) { fail(e); } };
const onlineDevices = (p) => list(p.devices).filter((d) => d.online).length;
const isOnline = (p) => p.online ?? onlineDevices(p) > 0;
const inboxCount = () => list(state.inbox).length;
const settings = () => state.theme?.settings || {};

// ?theme=light|dark (kit) forces a scheme for this view; the saved setting must not undo it.
const FORCED_SCHEME = ['light', 'dark'].includes(new URLSearchParams(location.search).get('theme'));
function applyTheme() {
  const s = settings();
  applySettings({ ...(FORCED_SCHEME ? {} : { scheme: s.scheme || 'auto' }), accent: s.accent || '', density: s.density, radius: s.radius, glass: s.glass, font: s.font });
  const wp = ['dusk', 'aurora', 'ember', 'slate'].includes(s.wallpaper) ? s.wallpaper : 'dusk';
  document.documentElement.setAttribute('data-wallpaper', wp);
  try { localStorage.setItem('sol-wallpaper', wp); } catch { /* optional */ }
}
try { const w = localStorage.getItem('sol-wallpaper'); if (w) document.documentElement.setAttribute('data-wallpaper', w); } catch { /* optional */ }

// ---------- dialogs ----------

/** Opens a kit dialog/sheet. `build(root, close)` wires the body; returns the element. */
function dialog(heading, body, build) {
  const d = document.createElement('sol-dialog');
  d.setAttribute('heading', heading);
  render(d, body);
  document.body.append(d);
  const close = () => d.close();
  d.addEventListener('close', () => d.remove());
  queueMicrotask(() => { d.show(); build?.(d, close); });
  return d;
}

function confirmDialog({ title, text, action, danger = true, run }) {
  dialog(title, html`<p>${text}</p><div class="dlg-actions"><button type="button" data-no>Cancel</button><button type="button" class="primary ${danger ? 'danger' : ''}" data-yes>${action}</button></div>`, (d, close) => {
    d.querySelector('[data-no]').onclick = close;
    d.querySelector('[data-yes]').onclick = async () => { close(); await act(run, null); };
  });
}

async function copy(text) {
  try { await navigator.clipboard.writeText(text); toast('Copied'); } catch { toast('Copy failed: select the link and copy it', { kind: 'error' }); }
}

/** A QR + link block shared by Add a person and Add my device. */
function shareBlock(invite, hint) {
  const mins = minutesLeft(invite.expires);
  return html`<div class="share-box">
    <sol-qr value="${invite.link}" label="${hint}"></sol-qr>
    <p class="muted flush">${hint}</p>
    <div class="link-line"><code>${invite.link}</code><button type="button" class="tinted" data-copy>${ic('copy')}Copy</button>
      ${typeof navigator.share === 'function' ? html`<button type="button" class="tinted" data-share>${ic('share')}Share</button>` : ''}</div>
    <small>One use, valid for ${mins} more minute${mins === 1 ? '' : 's'}.</small>
  </div>`;
}
function wireShare(d, invite, title) {
  d.querySelector('[data-copy]')?.addEventListener('click', () => copy(invite.link));
  d.querySelector('[data-share]')?.addEventListener('click', () => navigator.share({ title, url: invite.link }).catch(() => {}));
}

// Shown before a contact is made anonymous (the daemon's AnonymousWarning says the same).
const ANON_WARNING = 'From now on they only reach you over Tor and only learn your onion address. Any IP address of yours they have already seen can’t be taken back.';

function addPerson() {
  dialog('Add a person', html`<div class="form" id="ap"><div class="list"><div data-item><span class="grow">Anonymous<span class="row-sub">They reach you over Tor only and never learn your IP address</span></span><input type="checkbox" role="switch" id="ap-anon" aria-label="Anonymous"></div></div>
    <div id="ap-qr" class="share-box"><p class="muted">Making a link</p></div>
    <hr class="rule">
    <form id="ap-paste" class="form"><label>Or paste a link from them<input name="link" placeholder="sol://pair?..." autocomplete="off" autocapitalize="off" spellcheck="false" required></label>
    <div class="dlg-actions"><button class="primary" type="submit">Connect</button></div></form></div>`, async (d, close) => {
    const make = async () => {
      const anon = d.querySelector('#ap-anon').checked;
      render(d.querySelector('#ap-qr'), html`<p class="muted">Making a link</p>`);
      try {
        const inv = await admin.invite(false, anon);
        render(d.querySelector('#ap-qr'), shareBlock(inv, anon ? 'An anonymous link: it carries only your onion address.' : 'Ask them to scan this with Sol, or send them the link.'));
        wireShare(d, inv, 'Add me on Sol');
      } catch (e) { render(d.querySelector('#ap-qr'), html`<p class="muted">${e?.message || String(e)}</p>`); }
    };
    d.querySelector('#ap-anon').onchange = make;
    await make();
    d.querySelector('#ap-paste').onsubmit = async (ev) => {
      ev.preventDefault();
      const link = new FormData(ev.target).get('link').trim();
      try { await admin.pair(link); close(); toast('Connected'); load('people'); } catch (e) { fail(e); }
    };
  });
}

function addDevice() {
  dialog('Add my device', html`<div id="ad" class="share-box"><p class="muted">Making a link</p></div>`, async (d) => {
    try {
      const inv = await admin.invite(true);
      render(d.querySelector('#ad'), shareBlock(inv, 'Scan with your phone’s camera'));
      wireShare(d, inv, 'Add this device to my Sol');
    } catch (e) { fail(e); }
  });
}

function renameDialog(title, current, run) {
  dialog(title, html`<form class="form"><label>Name<input name="v" value="${current}" required autocomplete="off"></label><div class="dlg-actions"><button type="button" data-no>Cancel</button><button class="primary" type="submit">Save</button></div></form>`, (d, close) => {
    const input = d.querySelector('input'); input.focus(); input.select();
    d.querySelector('[data-no]').onclick = close;
    d.querySelector('form').onsubmit = async (ev) => { ev.preventDefault(); const v = input.value.trim(); close(); if (v) await act(() => run(v), 'Renamed'); };
  });
}

function deviceDialog(dev) {
  const me = dev.this;
  dialog(dev.name, html`<div class="form">
    <div class="list"><div data-item><span class="grow">Anchor<span class="row-sub">Stays on, so others can always reach your data</span></span><input type="checkbox" role="switch" data-k="anchor" ${dev.anchor ? raw('checked') : ''} aria-label="Anchor"></div>
    <div data-item><span class="grow">Can manage Sol<span class="row-sub">Admin devices change settings and people</span></span><input type="checkbox" role="switch" data-k="admin" ${dev.admin ? raw('checked') : ''} ${me ? raw('disabled') : ''} aria-label="Can manage Sol"></div></div>
    <div class="dlg-actions spread"><button type="button" class="danger" data-rm ${me ? raw('disabled') : ''}>Remove</button><span class="inline-actions"><button type="button" data-rename>Rename</button><button type="button" class="primary" data-ok>Done</button></span></div></div>`, (d, close) => {
    d.querySelectorAll('input[data-k]').forEach((i) => { i.onchange = () => act(() => admin.updateDevice(dev.id, { [i.dataset.k]: i.checked })); });
    d.querySelector('[data-ok]').onclick = close;
    d.querySelector('[data-rename]').onclick = () => { close(); renameDialog('Rename device', dev.name, (name) => admin.updateDevice(dev.id, { name })); };
    d.querySelector('[data-rm]').onclick = () => { close(); confirmDialog({ title: `Remove ${dev.name}?`, text: 'It leaves your device list. You can add it again later.', action: 'Remove', run: async () => { await admin.removeDevice(dev.id); toast('Removed'); } }); };
  });
}

function serviceDialog() {
  const people = list(state.people);
  dialog('Share a service', html`<form class="form"><label>Name<input name="name" placeholder="blog" required pattern="[a-z0-9-]+" autocomplete="off"></label>
    <label>Local port<input name="port" type="number" min="1" max="65535" placeholder="8080" required></label>
    <label class="check"><input type="checkbox" role="switch" name="http"> It is a website</label>
    <fieldset><legend>Share with</legend>${people.length ? people.map((p) => html`<label class="check"><input type="checkbox" name="p" value="${p.id}"> ${nameOf(p)}</label>`) : html`<span class="muted">Add a person first.</span>`}</fieldset>
    <div class="dlg-actions"><button type="button" data-no>Cancel</button><button class="primary" type="submit">Share</button></div></form>`, (d, close) => {
    d.querySelector('[data-no]').onclick = close;
    d.querySelector('form').onsubmit = async (ev) => {
      ev.preventDefault();
      const f = new FormData(ev.target);
      const s = { name: f.get('name').trim(), port: Number(f.get('port')), http: f.has('http'), people: f.getAll('p') };
      close(); await act(() => admin.addService(s), 'Shared');
    };
  });
}

function tunnelDialog() {
  const opts = [];
  for (const p of list(state.people)) for (const s of list(p.shares_with_me)) if (s.kind === 'service') opts.push({ p, s });
  dialog('Open a service', opts.length ? html`<form class="form"><label>Service<select name="o">${opts.map((o, i) => html`<option value="${i}">${o.s.name} from ${nameOf(o.p)}</option>`)}</select></label>
    <label>Local port (optional)<input name="port" type="number" min="1" max="65535" placeholder="Pick one for me"></label>
    <div class="dlg-actions"><button type="button" data-no>Cancel</button><button class="primary" type="submit">Open</button></div></form>`
    : html`<p class="muted">Nobody shares a service with you yet.</p>`, (d, close) => {
    d.querySelector('[data-no]')?.addEventListener('click', close);
    const form = d.querySelector('form');
    if (form) form.onsubmit = async (ev) => {
      ev.preventDefault();
      const f = new FormData(ev.target), o = opts[Number(f.get('o'))];
      const t = { person: o.p.id, service: o.s.name }; if (f.get('port')) t.port = Number(f.get('port'));
      close(); await act(() => admin.openTunnel(t), 'Opened');
    };
  });
}

function backupFlow() {
  const words = list(state.backup?.words);
  if (!words.length) return;
  dialog('Your backup words', html`<p>These 24 words are the only way to restore your Sol if you lose every device. Write them down on paper and keep them private. Sol shows them once.</p>
    <ol class="words hidden" id="wl">${words.map((w) => html`<li>${w}</li>`)}</ol>
    <div class="dlg-actions"><button type="button" data-show>Show words</button><button type="button" class="primary" data-next disabled>I wrote them down</button></div>`, (d, close) => {
    const wl = d.querySelector('#wl');
    d.querySelector('[data-show]').onclick = () => { wl.classList.remove('hidden'); d.querySelector('[data-next]').disabled = false; };
    d.querySelector('[data-next]').onclick = () => { close(); confirmBackup(words); };
  });
}
function confirmBackup(words) {
  const idx = [...words.keys()].sort(() => Math.random() - .5).slice(0, 3).sort((a, b) => a - b);
  dialog('Check your backup', html`<form class="form"><p class="flush">Type these words from your list to confirm.</p>
    ${idx.map((i) => html`<label>Word ${i + 1}<input name="w${i}" autocomplete="off" autocapitalize="off" spellcheck="false" required></label>`)}
    <div class="dlg-actions"><button type="button" data-no>Cancel</button><button class="primary" type="submit">Confirm</button></div></form>`, (d, close) => {
    d.querySelector('[data-no]').onclick = close;
    d.querySelector('form').onsubmit = async (ev) => {
      ev.preventDefault();
      const f = new FormData(ev.target);
      if (!idx.every((i) => f.get('w' + i).trim().toLowerCase() === words[i].toLowerCase())) { toast('Those words do not match. Check your list.', { kind: 'error' }); return; }
      close(); await act(async () => { await admin.confirmBackup(); await load('backup'); }, 'Backup confirmed');
    };
  });
}

// ---------- views ----------

function personRow(p) {
  const on = isOnline(p), n = onlineDevices(p);
  return html`<a data-item data-chevron href="#/people/${encodeURIComponent(p.id)}"><sol-avatar name="${nameOf(p)}" id="${p.id}" ${on ? raw('online') : ''}></sol-avatar>
    <span class="grow"><span class="row-title">${nameOf(p)}</span><span class="row-sub">${on ? `Online on ${n} device${n === 1 ? '' : 's'}` : (p.last_seen ? `Last seen ${relTime(p.last_seen)}` : 'Offline')}${p.anonymous ? ' · Anonymous' : ''}</span></span></a>`;
}

function pageHead(title, actions = '', back = '') {
  return html`<header class="page-head">${back ? html`<a class="btn plain back" href="${back}" aria-label="Back">${ic('back')}</a>` : ''}<div class="grow"><h1>${title}</h1></div>${actions}</header>`;
}

function peopleView() {
  const ppl = list(state.people);
  return html`<div class="page">${pageHead('People', html`<button class="primary" type="button" data-act="add-person">${ic('plus')}Add a person</button>`)}
    ${ppl.length ? html`<div class="list">${ppl.map(personRow)}</div>` : html`<div class="empty"><h3>Just you so far</h3><p>Add a person to share files, notes and more, privately and directly.</p><button class="primary" type="button" data-act="add-person">Add a person</button></div>`}</div>`;
}

function shareRow(s) {
  const open = s.url ? html`<a class="btn tinted" href="${s.url}">Open</a>` : '';
  return html`<div data-item><span class="row-icon">${ic(s.kind === 'service' ? 'globe' : s.kind === 'folder' ? 'folder' : 'grid')}</span><span class="grow"><span class="row-title">${s.name}</span><span class="row-sub">${s.kind || 'app'}</span></span>${open}</div>`;
}

function personView(id) {
  const p = list(state.people).find((x) => x.id === id);
  if (!p) return html`<div class="page">${pageHead('Person', '', '#/people')}<div class="empty"><h3>Not found</h3><p>This person is not in your contacts any more.</p></div></div>`;
  const on = isOnline(p);
  return html`<div class="page">${pageHead('', '', '#/people')}
    <div class="person-head"><sol-avatar name="${nameOf(p)}" id="${p.id}" size="84px" ${on ? raw('online') : ''}></sol-avatar><h1>${nameOf(p)}</h1>
      <span class="muted">${p.petname && p.name !== p.petname ? `${p.name} · ` : ''}<span class="${on ? 'dot-online' : 'dot-off'}"></span>${on ? 'Online' : `Last seen ${relTime(p.last_seen)}`}</span>
      <div class="person-actions"><button type="button" data-act="rename-person" data-id="${p.id}">${ic('edit')}Rename</button><button type="button" class="danger" data-act="unpair" data-id="${p.id}">Unpair</button></div></div>
    <section class="section"><h3 class="group-title">Privacy</h3><div class="list"><div data-item><span class="grow">Anonymous<span class="row-sub">${p.anonymous ? 'Tor only, both ways: they only know your onion address' : 'Off: the fastest path (WireGuard, then direct, Tor last)'}</span></span><input type="checkbox" role="switch" data-anon="${p.id}" aria-label="Anonymous" ${p.anonymous ? raw('checked') : ''}></div></div></section>
    <section class="section"><h3 class="group-title">Devices</h3><div class="list">${list(p.devices).map((d) => html`<div data-item><span class="row-icon">${ic('device')}</span><span class="grow"><span class="row-title">${d.name}</span><span class="row-sub">${d.online ? 'Online' : 'Offline'}</span></span>${d.anchor ? html`<span class="badge tint">Anchor</span>` : ''}</div>`)}</div></section>
    <section class="section"><h3 class="group-title">Shared with me</h3>${list(p.shares_with_me).length ? html`<div class="list">${p.shares_with_me.map(shareRow)}</div>` : html`<div class="list"><div data-item class="muted">Nothing yet</div></div>`}</section>
    <section class="section"><h3 class="group-title">I share with them</h3>${list(p.i_share).length ? html`<div class="list">${p.i_share.map(shareRow)}</div>` : html`<div class="list"><div data-item class="muted">Nothing yet</div></div>`}</section></div>`;
}

function devicesView() {
  const ds = list(state.devices);
  return html`<div class="page">${pageHead('Devices', html`<button class="primary" type="button" data-act="add-device">${ic('plus')}Add my device</button>`)}
    <div class="list">${ds.map((d) => html`<button data-item data-chevron type="button" data-act="device" data-id="${d.id}"><span class="row-icon">${ic(/phone/i.test(d.name) ? 'phone' : 'device')}</span>
      <span class="grow"><span class="row-title">${d.name}</span><span class="row-sub"><span class="${d.online ? 'dot-online' : 'dot-off'}"></span>${d.online ? 'Online' : `Last seen ${relTime(d.last_seen)}`}</span></span>
      ${d.this ? html`<span class="badge tint">This device</span>` : ''}${d.anchor ? html`<span class="badge">Anchor</span>` : ''}</button>`)}</div>
    <p class="group-footer">An anchor is a device that stays on and keeps your data available to the people you share it with.</p></div>`;
}

function inboxItem(i, mini = false) {
  const approval = (i.kind || 'approval') === 'approval';
  const actions = html`<span class="${mini ? 'mini-actions' : 'inline-actions'}">${approval ? html`<button class="primary" type="button" data-act="approve" data-id="${i.id}">Approve</button><button type="button" data-act="deny" data-id="${i.id}">Deny</button>` : html`<button type="button" data-act="deny" data-id="${i.id}">Dismiss</button>`}</span>`;
  return mini
    ? html`<div class="mini"><span><span class="row-title">${i.title}</span><span class="row-sub">${i.app} · ${relTime(i.time)}</span></span>${actions}</div>`
    : html`<div data-item class="wrap"><span class="grow"><span class="row-title wraps">${i.title}</span><span class="row-sub wraps">${i.body || ''}</span><span class="row-sub">${i.app} · ${relTime(i.time)}</span></span>${actions}</div>`;
}
function inboxView() {
  const items = list(state.inbox);
  return html`<div class="page">${pageHead('Inbox')}${items.length ? html`<div class="list">${items.map((i) => inboxItem(i))}</div>` : html`<div class="empty"><h3>All caught up</h3><p>Requests from your apps and the people you share with appear here.</p></div>`}</div>`;
}

function sharingView() {
  const svcs = list(state.services), tns = list(state.tunnels);
  const who = (id) => { const p = list(state.people).find((x) => x.id === id); return p ? nameOf(p) : id; };
  return html`<div class="page">${pageHead('Sharing', '', phoneQ.matches ? '#/settings' : '')}
    <section class="section"><h3 class="group-title">Services I share</h3>${svcs.length ? html`<div class="list">${svcs.map((s) => html`<div data-item><span class="row-icon">${ic('globe')}</span><span class="grow"><span class="row-title">${s.name}</span><span class="row-sub">Port ${s.port}${s.http ? ' · website' : ''} · ${list(s.people).length ? list(s.people).map(who).join(', ') : 'nobody yet'}</span></span><button class="danger" type="button" data-act="rm-service" data-id="${s.name}">Stop sharing</button></div>`)}</div>` : html`<div class="list"><div data-item class="muted">Nothing shared</div></div>`}
      <p class="group-footer"><button type="button" class="tinted" data-act="add-service">${ic('plus')}Share a service</button></p></section>
    <section class="section"><h3 class="group-title">Tunnels I opened</h3>${tns.length ? html`<div class="list">${tns.map((t) => html`<div data-item><span class="row-icon">${ic('link')}</span><span class="grow"><span class="row-title">${t.service} from ${who(t.person)}</span><span class="row-sub"><code>127.0.0.1:${t.port}</code> · ${t.state}</span></span>${t.http ? html`<a class="btn tinted" href="http://127.0.0.1:${t.port}/">Open</a>` : ''}<button type="button" data-act="close-tunnel" data-id="${t.id}">Close</button></div>`)}</div>` : html`<div class="list"><div data-item class="muted">No tunnels open</div></div>`}
      <p class="group-footer"><button type="button" class="tinted" data-act="open-tunnel">${ic('plus')}Open a service</button></p></section></div>`;
}

const ACCENTS = ['#E8A100', '#FF6B4A', '#E0489B', '#8E5CF7', '#0A84FF', '#1FB5A0', '#34C759'];
const seg = (key, opts, cur) => html`<div class="segmented" role="group">${opts.map(([v, l]) => html`<button type="button" data-set="${key}" data-v="${v}" aria-pressed="${cur === v}">${l}</button>`)}</div>`;
const WALLPAPERS = [['dusk', 'Dusk'], ['aurora', 'Aurora'], ['ember', 'Ember'], ['slate', 'Slate']];

function settingsView() {
  const s = settings(), st = state.status || {};
  const bk = state.backup || {};
  return html`<div class="page">${pageHead('Settings')}
    ${phoneQ.matches ? html`<div class="list section"><a data-item data-chevron href="#/devices"><span class="row-icon">${ic('device')}</span><span class="grow">Devices</span></a><a data-item data-chevron href="#/sharing"><span class="row-icon">${ic('share')}</span><span class="grow">Sharing</span></a></div>` : ''}
    <section class="section"><h3 class="group-title">Appearance</h3><div class="list">
      <div data-item><span class="grow">Look</span>${seg('scheme', [['auto', 'Auto'], ['light', 'Light'], ['dark', 'Dark']], s.scheme || 'auto')}</div>
      <div data-item><span class="grow">Accent</span><span class="swatches">${ACCENTS.map((c) => html`<button type="button" class="swatch" data-var-sw="${c}" data-set="accent" data-v="${c === ACCENTS[0] ? '' : c}" aria-label="Accent ${c}" aria-pressed="${(s.accent || '').toLowerCase() === (c === ACCENTS[0] ? '' : c.toLowerCase())}"></button>`)}</span></div>
      <div data-item><span class="grow">Density</span>${seg('density', [['comfortable', 'Comfortable'], ['compact', 'Compact']], s.density || 'comfortable')}</div>
      <div data-item><span class="grow">Corners</span>${seg('radius', [['rounded', 'Rounded'], ['square', 'Square']], s.radius || 'rounded')}</div>
      <div data-item><span class="grow">Glass<span class="row-sub">Translucent bars and panels</span></span><input type="checkbox" role="switch" data-toggle="glass" aria-label="Glass" ${s.glass === 'off' ? '' : raw('checked')}></div>
      <div data-item><span class="grow">Font</span><select data-select="font" class="fit" aria-label="Font">${[['system', 'System'], ['inter', 'Inter'], ['mono', 'Monospace']].map(([v, l]) => html`<option value="${v}" ${(s.font || 'system') === v ? raw('selected') : ''}>${l}</option>`)}</select></div>
      <div data-item><span class="grow">Theme</span><select data-select="theme" class="fit" aria-label="Theme">${list(state.themes?.themes).map((t) => html`<option value="${t.name}" ${t.name === (state.theme?.name || state.themes?.active) ? raw('selected') : ''}>${t.title || t.name}</option>`)}</select></div></div></section>
    <section class="section"><h3 class="group-title">Wallpaper</h3><div class="list"><div class="wps">${WALLPAPERS.map(([v, l]) => html`<button type="button" data-set="wallpaper" data-v="${v}" aria-pressed="${(s.wallpaper || 'dusk') === v}"><span class="wp" data-wallpaper="${v}"><span class="sun"></span></span>${l}</button>`)}</div></div></section>
    <section class="section"><h3 class="group-title">Backup</h3><div class="list"><div data-item><span class="grow">Backup words<span class="row-sub">${bk.confirmed ? 'You confirmed your backup.' : 'Write down your 24 words so you can restore Sol.'}</span></span>${bk.confirmed ? html`<span class="badge ok">Backed up</span>` : list(bk.words).length ? html`<button class="primary" type="button" data-act="backup">Show words</button>` : ''}</div></div>
      <p class="group-footer">To restore on a new device, choose “Restore from backup words” when you set up Sol.</p></section>
    <section class="section"><h3 class="group-title">Agents</h3>${list(state.agents).length ? html`<div class="list">${list(state.agents).map((a) => html`<div data-item><span class="row-icon">${ic('agent')}</span><span class="grow"><span class="row-title">${a.name}</span><span class="row-sub">${list(a.scopes).join(', ') || 'no scopes'} · expires ${relTime(a.expires)}</span></span><button type="button" class="danger" data-act="revoke-agent" data-id="${a.name}">Revoke</button></div>`)}</div>` : html`<div class="list"><div data-item class="muted">No agent keys</div></div>`}
      <p class="group-footer">Agents run <code class="inline">sol do</code> with a key you issued, limited to its scopes.</p></section>
    <section class="section"><h3 class="group-title">Browser sessions</h3><div class="list">${list(st.sessions).map((b) => html`<div data-item><span class="row-icon">${ic('globe')}</span><span class="grow"><span class="row-title">${b.client || b.name || 'Browser'}</span><span class="row-sub">Last used ${relTime(b.last_used)}</span></span>${b.current ? html`<span class="badge tint">This browser</span><button type="button" data-act="logout">Sign out</button>` : html`<button type="button" class="danger" data-act="revoke-session" data-id="${b.id}">Revoke</button>`}</div>`)}</div></section>
    <section class="section"><details class="adv"><summary class="group-title">Advanced</summary>
      <h3 class="group-title">Addresses</h3><div class="list">${list(st.endpoints).map((e) => html`<div data-item><span class="grow"><code>${e.addr}</code></span><span class="badge">${e.kind}</span></div>`)}</div>
      <h3 class="group-title">Transports</h3><div class="list">${transportRows(list(st.transports))}</div>
      <h3 class="group-title">Connections</h3><div class="list">${list(st.peer_sessions).length ? list(st.peer_sessions).map((x) => html`<div data-item><span class="grow"><span class="row-title">${x.peer}</span><span class="row-sub">${x.relation || ''} · since ${relTime(x.since)}</span></span></div>`) : html`<div data-item class="muted">None</div>`}</div>
      <p class="group-footer"><button type="button" class="tinted" data-act="network">${ic('refresh')}Check network now</button></p></details></section></div>`;
}

const TRANSPORTS = { wg: ['WireGuard', 'Direct and fast; punches through home routers'], direct: ['Direct (TLS)', 'For networks that block UDP'], tor: ['Tor', 'Works anywhere; hides your address'], clearnet: ['Clearnet', 'Unencrypted; tests and LAN only'] };
function transportRows(ts) {
  const on = ts.filter((t) => t.enabled).length;
  return ts.map((t) => {
    const [title, sub] = TRANSPORTS[t.name] || [t.name, ''];
    const last = t.enabled && on === 1;
    return html`<div data-item><span class="grow">${title}<span class="row-sub">${t.enabled && t.detail ? t.detail : sub}</span></span>${t.enabled ? html`<span class="badge ${t.state === 'up' ? 'ok' : ''}">${t.state}</span>` : ''}<input type="checkbox" role="switch" data-transport="${t.name}" aria-label="${title}" ${t.enabled ? raw('checked') : ''} ${last ? raw('disabled') : ''}></div>`;
  });
}

// Home: wallpaper, greeting and clock, glass widgets, app grid, Inbox preview, dock.
let firstPaint = true;
function appTile(a, i) {
  const spec = appIconSpec(a, icons);
  const art = spec.kind === 'img' ? html`<span class="app-img"><img src="${spec.src}" alt=""></span>` : html`<sol-app-icon glyph="${spec.glyph}" hue="${spec.hue}"></sol-app-icon>`;
  return html`<a class="app" data-var-i="${i}" href="${a.url}" ${a.running === false ? raw('data-off') : ''}>${art}<span>${a.name}</span></a>`;
}
function homeView() {
  const now = new Date();
  const ppl = list(state.people), online = ppl.filter(isOnline), ds = list(state.devices);
  const st = state.status?.storage, pct = st ? Math.round((st.used / st.total) * 100) : 0;
  const items = list(state.inbox), apps = list(state.apps);
  return html`<div class="home">
    <header class="hero"><p class="date" id="date">${dateText(now)}</p><h1 class="clock" id="clock">${clockText(now)}</h1><p class="greet" id="greet">${greetingFor(meName(), now)}</p></header>
    <div class="widgets">
      <a class="widget" data-var-i="0" href="#/people"><span class="widget-title">${ic('people')}People online</span><span class="widget-body"><span class="widget-value">${online.length}</span>
        <span class="widget-row"><span class="faces">${online.slice(0, 4).map((p) => html`<sol-avatar name="${nameOf(p)}" id="${p.id}" size="24px"></sol-avatar>`)}</span><span class="widget-sub">${ppl.length ? `of ${ppl.length}` : 'No one yet'}</span></span></span></a>
      <a class="widget" data-var-i="1" href="#/devices"><span class="widget-title">${ic('device')}My devices</span><span class="widget-body"><span class="widget-value">${ds.length}</span><span class="widget-sub">${ds.filter((d) => d.online).length} online</span></span></a>
      ${st ? html`<a class="widget" data-var-i="2" href="#/settings"><span class="widget-title">${ic('storage')}Storage</span><sol-ring value="${pct}" size="40px">${pct}%</sol-ring><span class="widget-body"><span class="widget-value">${bytes(st.used)}</span><span class="widget-sub">of ${bytes(st.total)}</span></span></a>` : ''}
    </div>
    <div class="home-cols">
      <section aria-label="Apps">${apps.length ? html`<div class="app-grid">${apps.map(appTile)}</div>` : html`<p class="home-empty">No apps installed yet.</p>`}</section>
      <aside class="inbox-card glass" aria-label="Inbox"><h2>Inbox <a href="#/inbox">See all</a></h2>${items.length ? items.slice(0, 3).map((i) => inboxItem(i, true)) : html`<div class="empty-sm">All caught up</div>`}</aside>
    </div></div>`;
}

const NAV = [['home', 'Home', 'home', '#/'], ['people', 'People', 'people', '#/people'], ['devices', 'Devices', 'device', '#/devices'], ['inbox', 'Inbox', 'inbox', '#/inbox'], ['sharing', 'Sharing', 'share', '#/sharing'], ['settings', 'Settings', 'settings', '#/settings']];
const TABS = ['home', 'people', 'inbox', 'settings'];
const badge = (n, cls) => (n ? html`<span class="${cls}">${n}</span>` : '');
const current = (name) => (route.name === name || (name === 'settings' && phoneQ.matches && ['devices', 'sharing'].includes(route.name)) ? 'page' : 'false');

function chrome() {
  render($('side'), html`<div class="brand"><span class="brand-sun"></span>Sol</div>${NAV.map(([n, l, i, h]) => html`<a class="side-item" href="${h}" aria-current="${n === route.name ? 'page' : 'false'}">${ic(i)}${l}${n === 'inbox' ? badge(inboxCount(), 'badge tint') : ''}</a>`)}<span class="spacer"></span>
    <div class="me"><sol-avatar name="${meName() || '?'}" id="me"></sol-avatar><span><strong>${meName()}</strong><small class="muted">${state.status?.me?.device?.name || ''}</small></span></div>`);
  render($('tabs'), html`${NAV.filter((n) => TABS.includes(n[0])).map(([n, l, i, h]) => html`<a href="${h}" aria-current="${current(n)}">${ic(i)}${l}${n === 'inbox' ? badge(inboxCount(), 'badge') : ''}</a>`)}`);
  const dock = list(state.apps).slice(0, 4).map((a) => { const sp = appIconSpec(a, icons); return html`<a href="${a.url}" title="${a.name}" aria-label="${a.name}"><sol-app-icon glyph="${sp.glyph || 'grid'}" hue="${sp.hue}"></sol-app-icon>${a.running ? html`<span class="dot"></span>` : ''}</a>`; });
  render($('dock'), html`${dock}<span class="sep"></span>
    <a href="#/people" title="People" aria-label="People"><sol-app-icon glyph="people" hue="210"></sol-app-icon></a>
    <a href="#/inbox" title="Inbox" aria-label="Inbox"><sol-app-icon glyph="inbox" hue="12"></sol-app-icon>${badge(inboxCount(), 'dock-badge')}</a>
    <a href="#/settings" title="Settings" aria-label="Settings"><sol-app-icon glyph="settings" hue="250" data-dark-glyph></sol-app-icon></a>`);
}

// ---------- render loop ----------

let queued = false;
// Paint on the next frame, or within 100 ms when frames are throttled (background tabs, headless
// browsers): new data must never wait for the tab to become visible again.
function draw() {
  if (queued) return;
  queued = true;
  const run = () => { if (queued) { queued = false; paint(); } };
  requestAnimationFrame(run);
  setTimeout(run, 100);
}

function paint() {
  $('locked').hidden = !locked;
  $('shell').hidden = locked;
  $('wallpaper').hidden = false;
  const shell = $('shell'), main = $('main');
  shell.dataset.route = route.name;
  const top = main.scrollTop;
  const views = { home: homeView, people: () => (route.id ? personView(route.id) : peopleView()), devices: devicesView, inbox: inboxView, sharing: sharingView, settings: settingsView };
  render(main, views[route.name]());
  chrome();
  main.scrollTop = top;
  if (firstPaint && route.name === 'home' && state.apps) { firstPaint = false; document.body.classList.add('first'); setTimeout(() => document.body.classList.remove('first'), 2600); }
  const t = (NAV.find((n) => n[0] === route.name) || NAV[0])[1];
  document.title = route.name === 'home' ? 'Sol' : `${t} · Sol`;
}

// The clock ticks on the minute, from a timer aligned to it (this is a clock, not polling).
function tick() {
  const now = new Date();
  const set = (id, v) => { const e = $(id); if (e && e.textContent !== v) e.textContent = v; };
  set('clock', clockText(now)); set('date', dateText(now)); set('greet', greetingFor(meName(), now));
  setTimeout(tick, msToNextMinute() + 20);
}

// ---------- actions ----------

const personOf = (id) => list(state.people).find((p) => p.id === id);
const ACTIONS = {
  'add-person': addPerson, 'add-device': addDevice, 'add-service': serviceDialog, 'open-tunnel': tunnelDialog, backup: backupFlow,
  device: (el) => { const d = list(state.devices).find((x) => x.id === el.dataset.id); if (d) deviceDialog(d); },
  'rename-person': (el) => { const p = personOf(el.dataset.id); if (p) renameDialog('Rename', nameOf(p), (v) => admin.renamePerson(p.id, v)); },
  unpair: (el) => { const p = personOf(el.dataset.id); if (p) confirmDialog({ title: `Unpair from ${nameOf(p)}?`, text: 'You both lose the connection. Things you already shared stay with whoever has them.', action: 'Unpair', run: async () => { await admin.unpair(p.id); location.hash = '#/people'; toast('Unpaired'); } }); },
  approve: (el) => act(() => admin.approve(el.dataset.id), 'Approved'),
  deny: (el) => act(() => admin.deny(el.dataset.id), 'Denied'),
  'rm-service': (el) => confirmDialog({ title: `Stop sharing ${el.dataset.id}?`, text: 'People lose access to it. Tunnels they have open will close.', action: 'Stop sharing', run: async () => { await admin.removeService(el.dataset.id); toast('Stopped'); } }),
  'close-tunnel': (el) => act(() => admin.closeTunnel(el.dataset.id), 'Closed'),
  'revoke-agent': (el) => confirmDialog({ title: `Revoke ${el.dataset.id}?`, text: 'Its key stops working right away.', action: 'Revoke', run: async () => { await admin.revokeAgent(el.dataset.id); await load('agents'); toast('Revoked'); } }),
  'revoke-session': (el) => act(async () => { await admin.revokeSession(el.dataset.id); await load('status'); }, 'Revoked'),
  logout: () => act(async () => { await admin.logout(); locked = true; draw(); }),
  network: () => act(() => admin.networkChanged(), 'Checking the network'),
};

/** The person page's Anonymous switch: turning it on warns first (an IP already seen stays seen). */
function setAnonymous(input) {
  const p = personOf(input.dataset.anon);
  if (!p) return;
  const run = async () => { await admin.setAnonymous(p.id, input.checked); await load('people'); };
  if (!input.checked) { act(run, `${nameOf(p)} is a buddy again`); return; }
  input.checked = false; // until confirmed
  confirmDialog({ title: `Make ${nameOf(p)} anonymous?`, text: ANON_WARNING, action: 'Make anonymous', danger: false,
    run: async () => { input.checked = true; await run(); toast(`${nameOf(p)} is anonymous`); } });
}

/** Saves an appearance setting: updates the screen first, then tells the daemon. */
async function setSetting(patch) {
  const prev = state.theme;
  state.theme = { ...(prev || {}), settings: { ...settings(), ...patch } };
  applyTheme(); draw();
  try { await admin.themeSettings(patch); } catch (e) { state.theme = prev; applyTheme(); draw(); fail(e); }
}

document.addEventListener('click', (e) => {
  const t = e.target.closest('[data-act],[data-set]');
  if (!t) return;
  if (t.dataset.set) { setSetting({ [t.dataset.set]: t.dataset.v }); return; }
  ACTIONS[t.dataset.act]?.(t);
});
document.addEventListener('change', (e) => {
  const t = e.target;
  if (t.dataset.toggle) setSetting({ [t.dataset.toggle]: t.checked ? 'on' : 'off' });
  else if (t.dataset.transport) act(async () => { await admin.setTransport(t.dataset.transport, t.checked); await load('status'); }, t.checked ? 'Turned on' : 'Turned off').then(() => draw());
  else if (t.dataset.anon) setAnonymous(t);
  else if (t.dataset.select === 'font') setSetting({ font: t.value });
  else if (t.dataset.select === 'theme') act(() => admin.setTheme(t.value), 'Theme changed');
});
// Deep links that open a dialog: #/people/add, #/devices/add, #/settings/backup.
const DEEP = { 'people/add': addPerson, 'devices/add': addDevice, 'settings/backup': backupFlow };
function go() {
  route = parseRoute(location.hash);
  const deep = DEEP[route.name + '/' + route.id];
  if (deep) { history.replaceState(null, '', '#/' + route.name); route = parseRoute(location.hash); setTimeout(deep, 0); }
  draw();
}
addEventListener('hashchange', () => { go(); $('main').scrollTop = 0; });
phoneQ.addEventListener('change', draw);

// ---------- live updates ----------

let stream = null, wasDown = false;
function connect() {
  stream = admin.events((type) => {
    for (const k of EVENT_KEYS[type] || []) load(k);
    if (type === 'theme') load('themes');
    if (type === 'inbox' || type === 'person') { /* counts live in the same state */ }
  }, {
    onOpen: () => { if (wasDown) { wasDown = false; loadAll(); } },
    onError: async () => { wasDown = true; try { await admin.status(); } catch { /* 401 shows the locked screen */ } },
  });
}

applyTheme();
go();
tick();
loadAll().then(() => { connect(); if (SELFTEST) selftest(); });

// ---------- self-test hook ----------

// `?selftest=1` (honoured by the daemon's login redirect only when it runs with SOL_UI_SELFTEST=1)
// walks the main screens and POSTs a summary of the rendered DOM to /__selftest, so `make ui-e2e`
// can assert what a real daemon's data looks like on screen. Normal runs never use it.
const SELFTEST = new URLSearchParams(location.search).has('selftest');
// `?shot=1` (test daemons only) holds the load event for a moment and stops animations, for screenshots.
if (new URLSearchParams(location.search).has('shot')) {
  document.documentElement.toggleAttribute('data-shot', true); // home.css stops animations
  document.head.append(Object.assign(document.createElement('link'), { rel: 'stylesheet', href: '/__hold' }));
}
async function selftest() {
  const txt = (el) => (el?.textContent || '').replace(/\s+/g, ' ').trim();
  const all = (sel) => [...document.querySelectorAll(sel)];
  const settle = async (r) => { for (let i = 0; i < 40 && $('shell').dataset.route !== (r || 'home'); i++) await new Promise((x) => setTimeout(x, 50)); await new Promise((x) => setTimeout(x, 100)); };
  const out = { screens: {}, errors: [] };
  addEventListener('error', (e) => out.errors.push(String(e.message)));
  for (const r of ['', 'people', 'devices', 'sharing', 'inbox', 'settings']) {
    location.hash = '#/' + r; await settle(r);
    const main = $('main');
    out.screens[r || 'home'] = {
      title: txt(main.querySelector('h1.clock, .page-head h1')),
      greet: txt($('greet')),
      widgets: all('.widget').map((w) => ({ title: txt(w.querySelector('.widget-title')), value: txt(w.querySelector('.widget-value')), sub: txt(w.querySelector('.widget-sub')) })),
      apps: all('.app-grid .app').map((a) => txt(a)),
      rows: all('#main [data-item]').map(txt),
      me: txt(document.querySelector('#side .me')),
      avatar: document.querySelector('#side sol-avatar')?.getAttribute('name') || '',
      empty: txt(document.querySelector('.home-empty')),
      overflow: document.documentElement.scrollWidth > innerWidth + 1,
    };
  }
  location.hash = '#/';
  out.errors.push(...cspViolations.map((v) => 'CSP ' + v));
  try { await fetch('/__selftest', { method: 'POST', body: JSON.stringify(out) }); } catch { /* the harness will time out */ }
}
