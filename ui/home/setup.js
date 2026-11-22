// Setup in the browser (internal/daemon/setup.go): a device with no Sol identity picks Start
// new, Add to my Sol (a join link from another device) or Restore from backup words. Every
// call carries the setup token, which arrives once in the URL (?setup=) and is kept in
// sessionStorage, out of the address bar. After success the setup server hands the address to
// the daemon; the page waits for it to answer and goes to the Sol app.
import { html, render } from '/sol.js';
import '/kit/kit.js';

const KEY = 'sol-setup-token';
const q = new URLSearchParams(location.search);
if (q.has('setup')) {
  try { sessionStorage.setItem(KEY, q.get('setup')); } catch { /* kept in memory below */ }
}
const token = q.get('setup') || (() => { try { return sessionStorage.getItem(KEY) || ''; } catch { return ''; } })();
if (q.has('setup')) history.replaceState(null, '', '/');

const root = document.getElementById('setup');
let defaults = { name: '', device: '', files: '' };

async function call(method, path, body) {
  const r = await fetch(path, { method, headers: { 'Sol-Setup': token, 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined });
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j?.error?.message || j?.message || `Error ${r.status}`);
  return j;
}

const card = (inner) => html`<div class="setup-card glass"><img class="locked-logo" src="/icon.svg" alt="">${inner}</div>`;

function welcome() {
  render(root, card(html`<h1>Welcome to Sol</h1>
    <p class="muted">Your own private network for your devices and the people you trust.</p>
    <div class="setup-choices">
      <button type="button" class="primary" data-go="new">Start new</button>
      <button type="button" data-go="join">Add to my Sol</button>
      <button type="button" data-go="restore">Restore from backup words</button>
    </div>
    ${defaults.files ? html`<p class="setup-foot muted">Your synced folder will be <code>${defaults.files}</code>.</p>` : ''}`));
}

function form(kind) {
  const back = html`<button type="button" data-go="welcome">Back</button>`;
  const device = html`<label>This device's name<input name="device" value="${defaults.device}" autocomplete="off"></label>`;
  const views = {
    new: html`<h1>Start new</h1><p class="muted">What should your devices and the people you share with call you?</p>
      <form class="form" data-kind="new"><label>Your name<input name="name" value="${defaults.name}" required autocomplete="name"></label>${device}
      <p class="err" role="alert"></p><div class="dlg-actions">${back}<button class="primary" type="submit">Start Sol</button></div></form>`,
    join: html`<h1>Add to my Sol</h1><p class="muted">On a device that already has Sol, open Devices, choose Add my device, and paste its link here.</p>
      <form class="form" data-kind="join">${device}<label>Link<input name="link" placeholder="sol://join…" required autocomplete="off" autocapitalize="off" spellcheck="false"></label>
      <p class="err" role="alert"></p><div class="dlg-actions">${back}<button class="primary" type="submit">Join</button></div></form>`,
    restore: html`<h1>Restore</h1><p class="muted">Your 24 backup words bring back who you are. Files come back by syncing from your devices that still have them.</p>
      <form class="form" data-kind="restore"><label>Backup words<textarea name="words" rows="4" required autocomplete="off" autocapitalize="off" spellcheck="false" placeholder="24 words, separated by spaces"></textarea></label>
      <label>Your name<input name="name" value="${defaults.name}" required autocomplete="name"></label>${device}
      <label>A link from a device you still have (optional)<input name="from" placeholder="sol://join… from Devices, Add my device" autocomplete="off" autocapitalize="off" spellcheck="false"></label>
      <p class="hint muted">With a link, your other devices recognise this one again. Without it, Sol restores from the words alone and you add your devices and people again.</p>
      <p class="err" role="alert"></p><div class="dlg-actions">${back}<button class="primary" type="submit">Restore</button></div></form>`,
  };
  render(root, card(views[kind]));
  root.querySelector('input, textarea')?.focus();
}

function waiting(text) {
  render(root, card(html`<h1>${text}</h1><p class="muted">This takes a moment.</p>`));
}

// The daemon takes over the address a moment after setup answers: wait until the admin API
// replies (401 or 200; the setup server says 404), then open the Sol app.
async function handOver() {
  waiting('Starting Sol…');
  for (let i = 0; i < 100; i++) {
    try {
      const r = await fetch('/admin/v1/status', { cache: 'no-store' });
      if (r.status === 200 || r.status === 401) { location.replace('/'); return; }
    } catch { /* not listening yet */ }
    await new Promise((x) => setTimeout(x, 300));
  }
  render(root, card(html`<h1>Sol is taking long to start</h1><p class="muted">Run <code>sol logs</code> in a terminal to see why, then <code>sol open</code>.</p>`));
}

root.addEventListener('click', (e) => {
  const go = e.target.closest('[data-go]')?.dataset.go;
  if (go === 'welcome') welcome();
  else if (go) form(go);
});
root.addEventListener('submit', async (e) => {
  e.preventDefault();
  const f = e.target, kind = f.dataset.kind, data = Object.fromEntries(new FormData(f));
  const err = f.querySelector('.err');
  const btn = f.querySelector('[type=submit]');
  btn.disabled = true; err.textContent = '';
  if (kind === 'restore') data.words = data.words.trim().toLowerCase().split(/\s+/).join(' ');
  try {
    waiting(kind === 'restore' ? 'Restoring…' : kind === 'join' ? 'Joining…' : 'Setting up…');
    await call('POST', `/setup/v1/${kind}`, data);
    await handOver();
  } catch (x) {
    form(kind);
    const nf = root.querySelector('form');
    for (const [k, v] of Object.entries(data)) { const el = nf.elements[k]; if (el && k !== 'words') el.value = v; }
    nf.querySelector('.err').textContent = x.message;
  }
});

(async () => {
  if (!token) {
    render(root, card(html`<h1>Set up Sol</h1><p class="muted">Run <code>sol</code> (or <code>sol open</code>) in a terminal on this computer to continue.</p>`));
    return;
  }
  welcome(); // paint at once; the prefilled names arrive a moment later
  try {
    defaults = await call('GET', '/setup/v1/state');
    if (!root.querySelector('form')) welcome(); // don't wipe a form the person started typing in
    else for (const k of ['name', 'device']) { const el = root.querySelector(`input[name=${k}]`); if (el && !el.value) el.value = defaults[k]; }
  } catch (x) {
    // Already set up (the daemon answers here now), or a stale link.
    try { const r = await fetch('/admin/v1/status'); if (r.status === 200 || r.status === 401) { location.replace('/'); return; } } catch { /* fall through */ }
    render(root, card(html`<h1>Set up Sol</h1><p class="muted">${x.message}</p>`));
  }
})();
