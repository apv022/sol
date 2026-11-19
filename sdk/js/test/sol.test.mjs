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
