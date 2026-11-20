// Pure helpers for the Sol app: greeting, clock, relative time, app icon specs, sizes. No DOM,
// no imports, so browser tests can call them directly.

/** "Good morning" etc. for a Date; small hours get a plain "Hello". */
export function greeting(date = new Date()) {
  const h = date.getHours();
  if (h >= 5 && h < 12) return 'Good morning';
  if (h >= 12 && h < 17) return 'Good afternoon';
  if (h >= 17 && h < 22) return 'Good evening';
  return 'Hello';
}

/** The greeting with the person's first name: "Good morning, Alice". */
export function greetingFor(name, date = new Date()) {
  const first = String(name || '').trim().split(/\s+/)[0];
  return first ? `${greeting(date)}, ${first}` : greeting(date);
}

/** Time for the big clock, in the person's locale ("9:41"); the AM/PM marker is dropped. */
export function clockText(date = new Date(), locale) {
  return date.toLocaleTimeString(locale, { hour: 'numeric', minute: '2-digit' }).replace(/\s?[AP]M$/i, '');
}

/** "Thursday, 19 November" in the person's locale. */
export function dateText(date = new Date(), locale) {
  return date.toLocaleDateString(locale, { weekday: 'long', day: 'numeric', month: 'long' });
}

/** Milliseconds until the next minute starts, so the clock ticks exactly when it changes. */
export function msToNextMinute(date = new Date()) {
  return 60000 - (date.getSeconds() * 1000 + date.getMilliseconds());
}

/** "just now", "5 minutes ago", "yesterday", "3 Nov". `ts` is unix seconds or milliseconds or ISO. */
export function relTime(ts, now = Date.now()) {
  const ms = toMs(ts);
  if (!ms) return 'never';
  const s = Math.round((now - ms) / 1000);
  if (s < 45) return 'just now';
  if (s < 3600) { const m = Math.max(1, Math.round(s / 60)); return `${m} minute${m === 1 ? '' : 's'} ago`; }
  if (s < 86400) { const h = Math.round(s / 3600); return `${h} hour${h === 1 ? '' : 's'} ago`; }
  if (s < 172800) return 'yesterday';
  return new Date(ms).toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
}

/** Normalises unix seconds, unix milliseconds or an ISO string to milliseconds (0 if unknown). */
export function toMs(ts) {
  if (!ts) return 0;
  if (typeof ts === 'string' && !/^\d+$/.test(ts)) { const t = Date.parse(ts); return Number.isNaN(t) ? 0 : t; }
  const n = Number(ts);
  return n < 1e12 ? n * 1000 : n;
}

/** Minutes left until `expires` (at least 0), for "Valid for 9 more minutes". */
export function minutesLeft(expires, now = Date.now()) {
  return Math.max(0, Math.ceil((toMs(expires) - now) / 60000));
}

/** Bytes as "1.4 GB". */
export function bytes(n) {
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = Number(n) || 0;
  while (v >= 1000 && i < u.length - 1) { v /= 1000; i++; }
  return `${v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)} ${u[i]}`;
}

/** A stable hue 0..359 from a string (same idea as the kit's avatar). */
export function hueOf(key) {
  let h = 0;
  for (const c of String(key)) h = (h * 31 + c.codePointAt(0)) % 360;
  return h;
}

const GLYPHS = { files: 'folder', notes: 'edit', music: 'music', photos: 'image', video: 'video', tasks: 'check', calendar: 'clock', chat: 'send', browser: 'globe' };

/**
 * What to draw for an app: its manifest icon when it is a safe image (a data: image or a path on
 * this origin) or a known glyph name, else a glyph chosen from the id with a gradient hue from it.
 * `glyphs` is the set of glyph names the kit has.
 */
export function appIconSpec(app, glyphs = null) {
  const icon = String(app?.icon || '');
  if (/^data:image\/(png|jpeg|webp|svg\+xml);/i.test(icon) || /^\/(?!\/)/.test(icon)) return { kind: 'img', src: icon, hue: hueOf(app.id) };
  const known = (g) => !glyphs || g in glyphs;
  if (icon && known(icon)) return { kind: 'glyph', glyph: icon, hue: hueOf(app.id) };
  const g = GLYPHS[app?.id] || 'grid';
  return { kind: 'glyph', glyph: known(g) ? g : 'grid', hue: hueOf(app?.id || '') };
}

/** Parses the route from a location hash: "#/people/bob" -> {name: 'people', id: 'bob'}. */
export function parseRoute(hash) {
  const parts = String(hash || '').replace(/^#\/?/, '').split('/').filter(Boolean).map(decodeURIComponent);
  const name = ['people', 'devices', 'inbox', 'sharing', 'settings'].includes(parts[0]) ? parts[0] : 'home';
  return { name, id: name === 'home' ? null : parts[1] || null };
}
