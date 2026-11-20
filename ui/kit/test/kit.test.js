// Browser tests for ui/kit modules (run: make browser-test).
import { test, assert } from '../testing.js';
import { qrMatrix, qrPath } from '../qr.js';

test('qr encodes a long join link at medium error correction', async () => {
  const link = 'sol://join?v=2&' + 'x'.repeat(600);
  const rows = await qrMatrix(link, 'M');
  assert.ok(rows.length >= 21 && rows.length <= 177, `size ${rows.length}`);
  assert.ok(rows.every((r) => r.length === rows.length), 'square');
  // Finder pattern: top-left 7x7 has a dark border.
  assert.ok(rows[0].slice(0, 7).every(Boolean), 'finder top row');
});

test('qr path has one square per dark module, offset by the quiet zone', () => {
  const d = qrPath([[true, false], [false, true]]);
  assert.equal(d, 'M4 4h1v1h-1zM5 5h1v1h-1z');
});

test('<sol-qr> renders an svg', async () => {
  const el = document.createElement('sol-qr');
  el.setAttribute('value', 'sol://pair?v=2&s=abc');
  document.body.append(el);
  for (let i = 0; i < 50 && !el.querySelector('svg'); i++) await new Promise((r) => setTimeout(r, 20));
  assert.ok(el.querySelector('svg path'), 'svg path drawn');
  el.remove();
});
