// sol.js: the browser client for the Sol app API (design/API.md).
//
// A UI page served at an app origin calls the API through the same-origin `/_sol/` bridge, which
// adds the app's token after the daemon checks Host and Origin. So this file holds no secrets and
// needs no configuration. Plain ES module, no build step, no dependencies.
//
// Rule for UI code: every API value that reaches the DOM goes through esc() or the `html` tag,
// which escapes interpolations by default. Never build markup with plain string concatenation.

let base = '/_sol';

/** Changes the bridge base path (tests and embedding only). */
export function configure(opts = {}) {
  if (opts.base !== undefined) base = String(opts.base).replace(/\/+$/, '');
}

/** Error thrown for every failed call; `code` is the API error code (bad_request, forbidden, ...). */
export class SolError extends Error {
  constructor(code, message, status) {
    super(message || code);
    this.name = 'SolError';
    this.code = code;
    this.status = status;
  }
}

const enc = encodeURIComponent;
// Feed addresses are <person>/<app>/<feed>: keep the slashes, encode each segment.
const addr = (a) => String(a).split('/').map(enc).join('/');

function query(params) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params || {})) {
    if (v !== undefined && v !== null && v !== '') q.set(k, String(v));
  }
  const s = q.toString();
  return s ? '?' + s : '';
}

async function request(method, path, { json, body, headers } = {}) {
  const h = { Accept: 'application/json', ...(headers || {}) };
  let payload = body;
  if (json !== undefined) {
    payload = JSON.stringify(json);
    h['Content-Type'] = 'application/json';
  }
  let res;
  try {
    res = await globalThis.fetch(base + path, { method, headers: h, body: payload });
  } catch (e) {
    throw new SolError('unavailable', 'Cannot reach the Sol daemon: ' + (e && e.message), 0);
  }
  const text = await res.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch { data = null; }
  }
  if (!res.ok) {
    const err = data && data.error ? data.error : {};
    throw new SolError(err.code || 'unavailable', err.message || res.statusText, res.status);
  }
  return data;
}

// Identity and people (API.md sections 1 and 2).

/** Who am I, which device, which app: {person, device, app}. */
export const whoami = () => request('GET', '/v1/whoami');
/** Contacts: [{id, name, petname, online, devices, shares_with_me, i_share}]. */
export const people = () => request('GET', '/v1/people');
/** This device first, then every device of my contacts: [{id, name, online, anchor, person?}]. */
export async function devices() {
  const [me, ppl] = await Promise.all([whoami(), people()]);
  const out = [{ ...me.device, person: me.person.id }];
  for (const c of ppl || []) for (const d of c.devices || []) out.push({ ...d, person: c.id });
  return out;
}

// Feeds (section 3).

/** Appends `body` (any JSON, max 64 KiB) to my feed `feed`; resolves to the stored entry. */
export const append = (feed, body) => request('POST', `/v1/feeds/${enc(feed)}/append`, { json: { body } });
/** Reads entries of `address` with pos >= from (default 1). Resolves to an array of entries. */
export const read = (address, { from, limit } = {}) => request('GET', `/v1/feeds/${addr(address)}${query({ from, limit })}`);
/** Lists readable feed addresses with their latest pos: [{feed, pos}]. */
export const feeds = ({ owner } = {}) => request('GET', `/v1/feeds${query({ owner })}`);

// Blobs (section 4).

/** Stores a File or Blob; resolves to {hash, size}. The name (if any) is sent as a hint. */
export function putBlob(file) {
  const headers = {};
  if (file && file.name) headers['Sol-Name'] = enc(file.name);
  return request('PUT', '/v1/blobs', { body: file, headers });
}
/** A URL for <img>, <audio> or <video> src; the daemon supports Range so media can seek. */
export const blobURL = (hash, from) => `${base}/v1/blobs/${enc(hash)}${query({ from })}`;
/** Keeps blobs and copies them to my anchors. */
export const pin = (hashes) => request('POST', '/v1/blobs/pin', { json: { hashes } });
/** Lets blobs be garbage collected when no feed of mine references them. */
export const unpin = (hashes) => request('POST', '/v1/blobs/unpin', { json: { hashes } });

// Sharing (section 5).

/** Lists who a feed is shared with: [{feed, person, mode}]. */
export const shares = (feed) => request('GET', `/v1/shares${query({ feed })}`);
/** Shares my feed with people; mode is 'read' (default) or 'write'. */
export const share = (feed, people, mode = 'read') => request('POST', '/v1/shares', { json: { feed, people, mode } });
/** Stops sharing. */
export const unshare = (feed, people, mode = 'read') => request('DELETE', '/v1/shares', { json: { feed, people, mode } });

// Actions (section 6).

/**
 * Calls an action. Resolves to the action's output, or {approval: id} when a person must approve
 * first. With dryRun the app only describes what it would do. `person` runs it on their app.
 */
export function call(action, args, { dryRun = false, person } = {}) {
  const body = { action };
  if (args !== undefined) body.args = args;
  if (person) body.person = person;
  if (dryRun) body.dry_run = true;
  return request('POST', '/v1/actions/call', { json: body });
}
/** Lists action manifests (with JSON Schemas), optionally for one app. */
export const actions = (app) => request('GET', `/v1/actions${query({ app })}`);

// Inbox (section 8) and activity.

/** Posts a notice or an approval: {kind, title, body?, to?, action?}; resolves to {id}. */
export const inbox = (post) => request('POST', '/v1/inbox', { json: post });
/** The signed activity log. */
export const activity = (opts = {}) => request('GET', `/v1/activity${query(opts)}`);

// Events (section 3).

/**
 * Subscribes to server-sent events. onEvent receives {id, type, entry|feed|person|inbox} where
 * type is "entry", "feed", "person", "inbox" or "theme" (see onTheme). EventSource reconnects on its own and resends
 * Last-Event-ID, so the stream resumes without gaps. Optional onError(err) is called when the
 * connection drops (it is retried automatically) or the stream closes for good.
 * Returns a function that closes the stream.
 */
export function events({ feeds: fs = [], people: ppl = false, inbox: ib = false } = {}, onEvent, onError) {
  const url = base + '/v1/events' + query({ feeds: fs.join(','), people: ppl ? 1 : '', inbox: ib ? 1 : '' });
  const es = new EventSource(url);
  for (const type of ['entry', 'feed', 'person', 'inbox', 'theme']) {
    es.addEventListener(type, (e) => {
      let data;
      try { data = JSON.parse(e.data); } catch { return; }
      onEvent({ id: e.lastEventId, type, [type]: data });
    });
  }
  if (onError) {
    es.onerror = () => onError(new SolError('unavailable', es.readyState === 2 ? 'event stream closed' : 'event stream interrupted, retrying'));
  }
  return () => es.close();
}

// Themes (design/UI.md "Themes", API.md section 10).

const THEME_ID = 'sol-theme';
const SETTING_VALUES = { density: ['compact'], radius: ['square'], glass: ['off'], font: ['inter', 'mono'] };

/**
 * Applies appearance settings to the root element as data- attributes (the kit's contract):
 * {scheme: 'auto'|'light'|'dark', accent: '#rrggbb'|'', density, radius, glass, font}. Only keys
 * present are changed; a default or unknown value removes the attribute. Returns what was applied.
 */
export function applyThemeSettings(settings, root = globalThis.document && document.documentElement) {
  if (!root || !settings || typeof settings !== 'object') return {};
  if ('scheme' in settings) {
    if (settings.scheme === 'light' || settings.scheme === 'dark') root.setAttribute('data-theme', settings.scheme);
    else root.removeAttribute('data-theme');
  }
  for (const [k, ok] of Object.entries(SETTING_VALUES)) {
    if (!(k in settings)) continue;
    if (ok.includes(settings[k])) root.setAttribute('data-' + k, settings[k]); else root.removeAttribute('data-' + k);
  }
  if ('accent' in settings) {
    if (/^#[0-9a-f]{3,8}$/i.test(settings.accent || '')) root.style.setProperty('--tint', settings.accent);
    else root.style.removeProperty('--tint');
  }
  try { // same key kit.js reads, so the next page paints with the new settings at once
    const prev = JSON.parse(localStorage.getItem('sol-settings') || '{}');
    localStorage.setItem('sol-settings', JSON.stringify({ ...prev, ...settings }));
  } catch { /* storage may be unavailable */ }
  return settings;
}

/**
 * Swaps the theme stylesheet without a reload: adds a new <link> with a cache-busting href, and
 * removes the old one only once the new one has loaded (so the page never flashes unthemed). If
 * the new file is gone (404: no theme any more) both are removed. Resolves true when a theme is
 * now applied, false when none is. A swap that is still loading is replaced by the newer one.
 */
export function swapThemeStylesheet({ doc = globalThis.document, stamp = Date.now() } = {}) {
  for (const stale of doc.querySelectorAll('link[data-sol-theme-pending]')) stale.remove();
  const old = doc.getElementById(THEME_ID);
  const next = doc.createElement('link');
  next.rel = 'stylesheet';
  next.href = `${base}/theme.css?v=${stamp}`;
  next.setAttribute('data-sol-theme-pending', '');
  return new Promise((resolve) => {
    next.addEventListener('load', () => {
      if (old) old.remove();
      next.removeAttribute('data-sol-theme-pending');
      next.id = THEME_ID;
      resolve(true);
    });
    next.addEventListener('error', () => {
      next.remove();
      if (old) old.remove();
      resolve(false);
    });
    doc.head.append(next);
  });
}

/**
 * Handles one `theme` event: reapplies the settings it carries ({settings: {...}}, or the
 * settings directly), then swaps the stylesheet. Resolves to the detail.
 */
export async function applyTheme(detail = {}, opts = {}) {
  const settings = detail && typeof detail.settings === 'object' && detail.settings ? detail.settings : detail;
  applyThemeSettings(settings, opts.root);
  await swapThemeStylesheet(opts);
  return detail;
}

/**
 * Keeps this page themed live: when the daemon pushes a `theme` event (the person picked another
 * theme, changed a setting, or edited the theme file), swaps /_sol/theme.css and reapplies the
 * settings, with no reload. `handler(detail)` is optional. Returns a function that stops it.
 * Apps that already call events() need not call this: kit.js pages get it from here.
 */
export function onTheme(handler, opts = {}) {
  return events({}, (ev) => {
    if (ev.type !== 'theme') return;
    applyTheme(ev.theme, opts).then((d) => { if (handler) handler(d); });
  });
}

// Safe HTML.

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
/** Escapes text for HTML text and attribute values. */
export const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ESC[c]);

/** Markup that is already safe; html`` returns it and does not escape it again. */
export class SafeHtml {
  constructor(s) { this.value = s; }
  toString() { return this.value; }
}
/** Marks a string you wrote yourself (never API data) as safe markup. */
export const raw = (s) => new SafeHtml(String(s));

/**
 * Tagged template that escapes every interpolation unless it is itself html`` output (or raw()).
 * Arrays are joined, so lists map straight into it.
 */
export function html(strings, ...vals) {
  const part = (v) => (v instanceof SafeHtml ? v.value : Array.isArray(v) ? v.map(part).join('') : esc(v));
  let out = strings[0];
  vals.forEach((v, i) => { out += part(v) + strings[i + 1]; });
  return new SafeHtml(out);
}

/**
 * Replaces the children of `el` with html`` output. Only SafeHtml is accepted.
 *
 * UI origins forbid inline style attributes (CSP style-src 'self'), so markup sets CSS custom
 * properties with `data-var-<name>="<value>"` instead: render sets `--<name>` on that element
 * through the CSSOM, which the policy allows (`<a class="app" data-var-i="3">` → `--i: 3`).
 */
export function render(el, safe) {
  if (!(safe instanceof SafeHtml)) throw new TypeError('render() takes html`` output; use esc() or the html tag');
  const t = document.createElement('template');
  t.innerHTML = safe.value; // safe: SafeHtml, every interpolation already escaped (SAFE-HTML)
  applyVars(t.content);
  el.replaceChildren(t.content);
}

/** Sets the custom properties named by data-var-* attributes under root (see render). */
export function applyVars(root) {
  for (const n of root.querySelectorAll('*')) {
    for (const a of [...n.attributes]) {
      if (a.name.startsWith('data-var-')) n.style.setProperty('--' + a.name.slice(9), a.value);
    }
  }
}
