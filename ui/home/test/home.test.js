// Tests for the Sol app's pure helpers and the admin client's request shapes (stub fetch).
import { test, assert } from '../../kit/testing.js';
import { createAdmin, AdminError } from '../admin.js';
import { greeting, greetingFor, clockText, msToNextMinute, relTime, minutesLeft, bytes, appIconSpec, parseRoute, toMs, hueOf, wallpaperOf, wallpaperFileError, accentChoice, storageView, isAndroidApp, agentScopes, agentCommand, AGENT_NAME_RE } from '../util.js';

const at = (h, m = 0) => new Date(2026, 10, 19, h, m, 0, 0);

test('greeting follows the time of day', () => {
  assert.equal(greeting(at(6)), 'Good morning');
  assert.equal(greeting(at(12)), 'Good afternoon');
  assert.equal(greeting(at(18)), 'Good evening');
  assert.equal(greeting(at(23)), 'Hello');
  assert.equal(greeting(at(3)), 'Hello');
});

test('greeting uses the first name only', () => {
  assert.equal(greetingFor('Alice Lindqvist', at(8)), 'Good morning, Alice');
  assert.equal(greetingFor('', at(8)), 'Good morning');
});

test('clock drops the AM/PM marker and ticks on the minute', () => {
  assert.ok(!/[AP]M/i.test(clockText(at(14, 5), 'en-US')));
  assert.equal(clockText(at(14, 5), 'en-US'), '2:05');
  assert.equal(msToNextMinute(new Date(2026, 0, 1, 0, 0, 59, 500)), 500);
});

test('relative time and expiry', () => {
  const now = Date.now();
  assert.equal(relTime(now / 1000 - 5, now), 'just now');
  assert.equal(relTime(now / 1000 - 300, now), '5 minutes ago');
  assert.equal(relTime(now / 1000 - 7200, now), '2 hours ago');
  assert.equal(relTime(0, now), 'never');
  assert.equal(minutesLeft(now / 1000 + 600, now), 10);
  assert.equal(minutesLeft(now / 1000 - 5, now), 0);
  assert.equal(toMs('2026-01-01T00:00:00Z'), Date.parse('2026-01-01T00:00:00Z'));
});

test('bytes', () => {
  assert.equal(bytes(512), '512 B');
  assert.equal(bytes(14_200_000_000), '14 GB');
  assert.equal(bytes(1_500_000), '1.5 MB');
});

test('app icons: safe manifest image, known glyph, or generated from the id', () => {
  const glyphs = { folder: 1, grid: 1, edit: 1 };
  assert.equal(appIconSpec({ id: 'x', icon: 'data:image/png;base64,AAAA' }, glyphs).kind, 'img');
  assert.equal(appIconSpec({ id: 'x', icon: 'https://evil.example/i.png' }, glyphs).kind, 'glyph');
  assert.equal(appIconSpec({ id: 'x', icon: '//evil.example/i.png' }, glyphs).kind, 'glyph');
  assert.equal(appIconSpec({ id: 'x', icon: 'edit' }, glyphs).glyph, 'edit');
  assert.equal(appIconSpec({ id: 'files' }, glyphs).glyph, 'folder');
  assert.equal(appIconSpec({ id: 'zzz' }, glyphs).glyph, 'grid');
  assert.equal(appIconSpec({ id: 'zzz' }, glyphs).hue, hueOf('zzz'));
});

test('routes', () => {
  assert.deepEqual(parseRoute(''), { name: 'home', id: null });
  assert.deepEqual(parseRoute('#/people'), { name: 'people', id: null });
  assert.deepEqual(parseRoute('#/people/bob%20x'), { name: 'people', id: 'bob x' });
  assert.deepEqual(parseRoute('#/apps'), { name: 'apps', id: null });
  assert.deepEqual(parseRoute('#/nope'), { name: 'home', id: null });
});

// ---- admin client ----

function stub(responses = {}) {
  const calls = [];
  const fetch = async (url, init) => {
    calls.push({ url, ...init, body: init.body ? JSON.parse(init.body) : undefined });
    const r = responses[init.method + ' ' + url] || { status: 200, body: {} };
    return { ok: r.status >= 200 && r.status < 300, status: r.status, statusText: 'x', text: async () => JSON.stringify(r.body) };
  };
  return { calls, fetch };
}

test('admin requests: method, path, cookie credentials, JSON body', async () => {
  const s = stub();
  const a = createAdmin({ fetch: s.fetch });
  await a.status();
  await a.renamePerson('bob 1', 'Bobby');
  await a.unpair('bob');
  await a.updateDevice('d1', { anchor: true });
  await a.invite(true);
  await a.pair('sol://pair?x');
  await a.addService({ name: 'blog', port: 8080, http: true, people: ['carol'] });
  await a.openTunnel({ person: 'bob', service: 'jellyfin' });
  await a.approve('i1');
  await a.deny('i2');
  await a.themeSettings({ density: 'compact' });
  await a.revokeAgent('claude-laptop');
  await a.removeApp('music');
  const got = s.calls.map((c) => `${c.method} ${c.url}`);
  assert.deepEqual(got, [
    'GET /admin/v1/status', 'PATCH /admin/v1/people/bob%201', 'DELETE /admin/v1/people/bob', 'PATCH /admin/v1/devices/d1',
    'POST /admin/v1/invite', 'POST /admin/v1/pair', 'POST /admin/v1/services', 'POST /admin/v1/tunnels',
    'POST /admin/v1/inbox/i1/approve', 'POST /admin/v1/inbox/i2/deny', 'POST /admin/v1/theme/settings', 'POST /admin/v1/agents/claude-laptop/revoke',
    'POST /admin/v1/apps/music/remove',
  ]);
  assert.ok(s.calls.every((c) => c.credentials === 'same-origin'));
  assert.deepEqual(s.calls[1].body, { petname: 'Bobby' });
  assert.deepEqual(s.calls[4].body, { join: true, anonymous: false });
  assert.deepEqual(s.calls[5].body, { link: 'sol://pair?x' });
  assert.deepEqual(s.calls[7].body, { person: 'bob', service: 'jellyfin' });
  assert.equal(s.calls[0].body, undefined);
});

test('admin requests: transports and per-person privacy', async () => {
  const s = stub();
  const a = createAdmin({ fetch: s.fetch });
  await a.transports();
  await a.setTransport('wg', false);
  await a.setAnonymous('bob 1', true);
  await a.invite(false, true);
  assert.deepEqual(s.calls.map((c) => `${c.method} ${c.url}`), [
    'GET /admin/v1/transports', 'POST /admin/v1/transports/wg', 'PATCH /admin/v1/people/bob%201', 'POST /admin/v1/invite',
  ]);
  assert.deepEqual(s.calls[1].body, { enabled: false });
  assert.deepEqual(s.calls[2].body, { anonymous: true });
  assert.deepEqual(s.calls[3].body, { join: false, anonymous: true });
});

test('a 401 calls onUnauthorized and throws', async () => {
  const s = stub({ 'GET /admin/v1/people': { status: 401, body: { error: { code: 'unauthorized', message: 'No session' } } } });
  let n = 0;
  const a = createAdmin({ fetch: s.fetch, onUnauthorized: () => n++ });
  await assert.rejects(a.people(), (e) => e instanceof AdminError && e.status === 401 && e.code === 'unauthorized');
  assert.equal(n, 1);
});

test('errors carry the API error code and message; offline is status 0', async () => {
  const s = stub({ 'POST /admin/v1/pair': { status: 400, body: { error: { code: 'invalid_link', message: 'That link expired' } } } });
  const a = createAdmin({ fetch: s.fetch });
  await assert.rejects(a.pair('x'), (e) => e.code === 'invalid_link' && e.message === 'That link expired');
  const off = createAdmin({ fetch: async () => { throw new Error('down'); } });
  await assert.rejects(off.status(), (e) => e.status === 0 && e.code === 'unavailable');
});

test('events subscribes to /events and parses each typed event', () => {
  const made = [];
  class FakeES { constructor(url, opt) { this.url = url; this.opt = opt; this.l = {}; made.push(this); } addEventListener(t, f) { this.l[t] = f; } }
  const a = createAdmin({ EventSource: FakeES });
  const seen = [];
  a.events((t, d) => seen.push([t, d]));
  assert.equal(made[0].url, '/admin/v1/events');
  made[0].l.person({ data: '{"id":"bob"}' });
  made[0].l.inbox({ data: '' });
  assert.deepEqual(seen, [['person', { id: 'bob' }], ['inbox', null]]);
});

// ---- wallpapers, accent, storage, agents ----

const ID = '0123456789abcdef';

test('wallpaperOf: built-ins, custom images and fallbacks', () => {
  assert.deepEqual(wallpaperOf('ember'), { name: 'ember', id: '' });
  assert.deepEqual(wallpaperOf(undefined), { name: 'dusk', id: '' });
  assert.deepEqual(wallpaperOf('nope'), { name: 'dusk', id: '' });
  assert.deepEqual(wallpaperOf('custom:' + ID), { name: 'custom', id: ID });
  assert.deepEqual(wallpaperOf('custom:' + ID, [{ id: ID }]), { name: 'custom', id: ID });
  assert.deepEqual(wallpaperOf('custom:' + ID, []), { name: 'dusk', id: '' }, 'a deleted image falls back');
  assert.deepEqual(wallpaperOf('custom:../x'), { name: 'dusk', id: '' });
});

test('wallpaperFileError: type and size', () => {
  assert.equal(wallpaperFileError({ type: 'image/png', size: 1000 }), '');
  assert.equal(wallpaperFileError({ type: 'image/webp', size: 15 * 1024 * 1024 }), '');
  assert.ok(wallpaperFileError({ type: 'image/gif', size: 10 }));
  assert.ok(wallpaperFileError({ type: 'image/jpeg', size: 15 * 1024 * 1024 + 1 }));
  assert.ok(wallpaperFileError(null));
});

test('accentChoice: swatch, default and custom colours', () => {
  const sw = ['#E8A100', '#FF6B4A', '#0A84FF'];
  assert.deepEqual(accentChoice('', sw), { kind: 'swatch', value: '' });
  assert.deepEqual(accentChoice('#e8a100', sw), { kind: 'swatch', value: '' });
  assert.deepEqual(accentChoice('#0a84ff', sw), { kind: 'swatch', value: '#0A84FF' });
  assert.deepEqual(accentChoice('#123abc', sw), { kind: 'custom', value: '#123abc' });
  assert.deepEqual(accentChoice('#abc', sw), { kind: 'custom', value: '#E8A100' }, 'a short form the picker cannot show');
});

test('storageView: the Sol folder and the free room, not the disk', () => {
  assert.equal(storageView(null), null);
  assert.deepEqual(storageView({ used: 2_400_000_000, free: 49_800_000_000, total: 64_000_000_000, measured: true }), { value: '2.4 GB', sub: '50 GB free on this device' });
  assert.equal(storageView({ used: 0, free: 5000, total: 9000, measured: false }).value, '…');
  assert.equal(storageView({ used: 5, total: 9 }).sub, '', 'an older daemon says nothing about free room');
});

test('isAndroidApp: a flag or a method on window.SolAndroid', () => {
  assert.equal(isAndroidApp({}), false);
  assert.equal(isAndroidApp({ SolAndroid: { isApp: true } }), true);
  assert.equal(isAndroidApp({ SolAndroid: { isApp: () => true } }), true);
  assert.equal(isAndroidApp({ SolAndroid: { isApp: () => false } }), false);
  assert.equal(isAndroidApp({ SolAndroid: { isApp() { throw new Error('x'); } } }), false);
});

test('agentScopes: everything, whole apps, single actions', () => {
  assert.deepEqual(agentScopes({ all: true, apps: ['files'] }), ['*']);
  assert.deepEqual(agentScopes({ apps: ['notes', 'files'] }), ['files.*', 'notes.*']);
  assert.deepEqual(agentScopes({ apps: ['files'], actions: ['files.list', 'notes.read', 'notes.read'] }), ['files.*', 'notes.read']);
  assert.deepEqual(agentScopes({}), []);
  assert.ok(AGENT_NAME_RE.test('claude-laptop') && !AGENT_NAME_RE.test('Bad Name') && !AGENT_NAME_RE.test('-x'));
});

test('agentCommand quotes what needs it', () => {
  assert.deepEqual(agentCommand('bot', './bot.key'), { here: 'sol do --as-agent bot <app>.<action>', elsewhere: 'SOL_AGENT_KEY=./bot.key sol do <app>.<action>' });
  assert.equal(agentCommand('b', "/tmp/it's here.key").elsewhere, "SOL_AGENT_KEY='/tmp/it'\\''s here.key' sol do <app>.<action>");
});

test('admin: wallpaper upload is the raw file with its type; agent key and sign-in shapes', async () => {
  const calls = [];
  const fetch = async (url, init) => { calls.push({ url, ...init }); return { ok: true, status: 200, statusText: 'ok', text: async () => '{}' }; };
  const a = createAdmin({ fetch });
  const file = new Blob(['x'], { type: 'image/png' });
  await a.addWallpaper(file);
  await a.removeWallpaper(ID);
  await a.newAgent({ name: 'bot', scopes: ['files.*'], days: 30 });
  await a.signIn();
  await a.actions();
  assert.deepEqual(calls.map((c) => `${c.method} ${c.url}`), [
    'POST /admin/v1/wallpapers', `DELETE /admin/v1/wallpapers/${ID}`, 'POST /admin/v1/agents/new', 'POST /_sol/signin', 'GET /admin/v1/actions',
  ]);
  assert.equal(calls[0].body, file);
  assert.equal(calls[0].headers['Content-Type'], 'image/png');
  assert.deepEqual(JSON.parse(calls[2].body), { name: 'bot', scopes: ['files.*'], days: 30 });
  assert.equal(calls[3].body, undefined);
});
