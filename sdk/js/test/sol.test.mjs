// Smoke test for sol.js against a fetch stub. Run: node sdk/js/test/sol.test.mjs
import assert from 'node:assert/strict';
import * as sol from '../sol.js';

const calls = [];
let reply = { status: 200, body: {} };
globalThis.fetch = async (url, init) => {
  calls.push({ url, ...init });
  return {
    ok: reply.status < 400, status: reply.status, statusText: 'x',
    text: async () => (reply.body === null ? '' : JSON.stringify(reply.body)),
  };
};

reply = { status: 200, body: { person: { id: 'p', name: 'A' } } };
assert.equal((await sol.whoami()).person.id, 'p');
assert.equal(calls[0].url, '/_sol/v1/whoami');

reply = { status: 200, body: [] };
await sol.read('p1/docs/doc.1', { from: 5, limit: 10 });
assert.equal(calls.at(-1).url, '/_sol/v1/feeds/p1/docs/doc.1?from=5&limit=10');

await sol.append('notes', { a: 1 });
assert.equal(calls.at(-1).method, 'POST');
assert.deepEqual(JSON.parse(calls.at(-1).body), { body: { a: 1 } });

await sol.call('files.delete', { path: '/x' }, { dryRun: true });
assert.deepEqual(JSON.parse(calls.at(-1).body), { action: 'files.delete', args: { path: '/x' }, dry_run: true });

await sol.unshare('tree', ['bob']);
assert.equal(calls.at(-1).method, 'DELETE');

assert.equal(sol.blobURL('sha256:ab', 'bob'), '/_sol/v1/blobs/sha256%3Aab?from=bob');

reply = { status: 403, body: { error: { code: 'forbidden', message: 'no scope' } } };
await assert.rejects(sol.people(), (e) => e.code === 'forbidden' && e.message === 'no scope');

assert.equal(sol.esc('<a href="x">&\'</a>'), '&lt;a href=&quot;x&quot;&gt;&amp;&#39;&lt;/a&gt;');
const evil = '<img onerror=x>';
assert.equal(String(sol.html`<p>${evil}</p>`), '<p>&lt;img onerror=x&gt;</p>');
assert.equal(String(sol.html`<ul>${['a', '<b>'].map((x) => sol.html`<li>${x}</li>`)}</ul>`), '<ul><li>a</li><li>&lt;b&gt;</li></ul>');
console.log('sol.js ok');

// Theme swap logic against a tiny DOM stub.
function fakeDoc() {
  const mk = (tag) => {
    const attrs = {}; const ls = {};
    const n = { tag, attrs, removed: false, id: '', rel: '', href: '',
      setAttribute(k, v) { attrs[k] = v; }, removeAttribute(k) { delete attrs[k]; },
      addEventListener(t, f) { ls[t] = f; }, fire(t) { ls[t](); }, remove() { n.removed = true; doc.links = doc.links.filter((x) => x !== n); } };
    return n;
  };
  const doc = {
    links: [],
    createElement: mk,
    getElementById: (id) => doc.links.find((l) => l.id === id) || null,
    querySelectorAll: (sel) => (sel === 'link[data-sol-theme-pending]' ? doc.links.filter((l) => 'data-sol-theme-pending' in l.attrs) : []),
    head: { append(n) { doc.links.push(n); } },
  };
  const attrs = {}; const props = {};
  doc.root = { setAttribute: (k, v) => { attrs[k] = v; }, removeAttribute: (k) => { delete attrs[k]; }, style: { setProperty: (k, v) => { props[k] = v; }, removeProperty: (k) => { delete props[k]; } }, attrs, props };
  return doc;
}
{
  const doc = fakeDoc();
  // First swap: no old sheet; the new one becomes #sol-theme on load.
  const p1 = sol.swapThemeStylesheet({ doc, stamp: 1 });
  assert.equal(doc.links.length, 1);
  assert.equal(doc.links[0].href, '/_sol/theme.css?v=1');
  doc.links[0].fire('load');
  assert.equal(await p1, true);
  const first = doc.links[0];
  assert.equal(first.id, 'sol-theme');
  // Second swap: the old sheet stays until the new one loaded (no unthemed flash).
  const p2 = sol.swapThemeStylesheet({ doc, stamp: 2 });
  assert.equal(doc.links.length, 2);
  assert.equal(first.removed, false);
  assert.equal(doc.links[1].href, '/_sol/theme.css?v=2');
  doc.links[1].fire('load');
  await p2;
  assert.equal(first.removed, true);
  assert.equal(doc.links.length, 1);
  assert.equal(doc.links[0].id, 'sol-theme');
  // A newer swap replaces one that is still loading.
  sol.swapThemeStylesheet({ doc, stamp: 3 });
  const pending = doc.links[1];
  sol.swapThemeStylesheet({ doc, stamp: 4 });
  assert.equal(pending.removed, true);
  // A 404 (theme cleared) removes the new and the old sheet and resolves false.
  const p5 = sol.swapThemeStylesheet({ doc, stamp: 5 });
  doc.links.at(-1).fire('error');
  assert.equal(await p5, false);
  assert.equal(doc.links.length, 0);
}
{
  const { root } = fakeDoc();
  sol.applyThemeSettings({ scheme: 'dark', density: 'compact', radius: 'square', glass: 'off', font: 'mono', accent: '#112233' }, root);
  assert.deepEqual(root.attrs, { 'data-theme': 'dark', 'data-density': 'compact', 'data-radius': 'square', 'data-glass': 'off', 'data-font': 'mono' });
  assert.equal(root.props['--tint'], '#112233');
  // Defaults, unknown values and a bad accent clear the attribute; absent keys are left alone.
  sol.applyThemeSettings({ scheme: 'auto', density: 'comfortable', glass: 'weird', accent: 'red; x' }, root);
  assert.deepEqual(root.attrs, { 'data-radius': 'square', 'data-font': 'mono' });
  assert.equal(root.props['--tint'], undefined);
}
{
  // onTheme wires a `theme` event to the settings and the stylesheet.
  const doc = fakeDoc();
  const handlers = {};
  globalThis.EventSource = class { constructor(u) { this.url = u; } addEventListener(t, f) { handlers[t] = f; } close() { this.closed = true; } };
  const got = new Promise((res) => {
    const stop = sol.onTheme(res, { doc, root: doc.root, stamp: 9 });
    assert.equal(typeof stop, 'function');
  });
  assert.ok(handlers.theme, 'theme events are subscribed');
  handlers.theme({ data: JSON.stringify({ settings: { density: 'compact' } }), lastEventId: '7' });
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(doc.root.attrs['data-density'], 'compact');
  assert.equal(doc.links[0].href, '/_sol/theme.css?v=9');
  doc.links[0].fire('load');
  assert.deepEqual(await got, { settings: { density: 'compact' } });
}
console.log('theme ok');
