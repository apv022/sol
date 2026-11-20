// Tests for the Sol app's pure helpers and the admin client's request shapes (stub fetch).
import { test, assert } from '../../kit/testing.js';
import { createAdmin, AdminError } from '../admin.js';
import { greeting, greetingFor, clockText, msToNextMinute, relTime, minutesLeft, bytes, appIconSpec, parseRoute, toMs, hueOf } from '../util.js';

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
  const got = s.calls.map((c) => `${c.method} ${c.url}`);
  assert.deepEqual(got, [
    'GET /admin/v1/status', 'PATCH /admin/v1/people/bob%201', 'DELETE /admin/v1/people/bob', 'PATCH /admin/v1/devices/d1',
    'POST /admin/v1/invite', 'POST /admin/v1/pair', 'POST /admin/v1/services', 'POST /admin/v1/tunnels',
    'POST /admin/v1/inbox/i1/approve', 'POST /admin/v1/inbox/i2/deny', 'POST /admin/v1/theme/settings', 'POST /admin/v1/agents/claude-laptop/revoke',
  ]);
  assert.ok(s.calls.every((c) => c.credentials === 'same-origin'));
  assert.deepEqual(s.calls[1].body, { petname: 'Bobby' });
  assert.deepEqual(s.calls[4].body, { join: true });
  assert.deepEqual(s.calls[5].body, { link: 'sol://pair?x' });
  assert.deepEqual(s.calls[7].body, { person: 'bob', service: 'jellyfin' });
  assert.equal(s.calls[0].body, undefined);
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
