// Browser tests for ui/kit modules (run: make browser-test).
import { test, assert } from '../testing.js';
import { setNav } from '../kit.js';
import { qrMatrix, qrPath } from '../qr.js';
import { appIconColors, MIN_CONTRAST } from '../appicon.js';

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

test('the app shell shows Home and the apps from /_sol/nav, and drops unsafe URLs', async () => {
  const shell = document.createElement('sol-app-shell');
  shell.setAttribute('heading', 'Files');
  shell.innerHTML = '<nav slot="nav"><a class="nav-item" href="#/">Mine</a></nav><main></main>';
  document.body.append(shell);
  assert.ok(setNav({ home: 'http://localhost:7777/', apps: [
    { id: 'files', name: 'Files', icon: '', url: 'http://files.localhost:7777/', current: true },
    { id: 'notes', name: '<b>Notes</b>', icon: '', url: 'http://notes.localhost:7777/', current: false },
    { id: 'evil', name: 'Evil', icon: '', url: 'javascript:alert(1)', current: false }] }));
  const links = [...shell.querySelectorAll('.sol-group a')];
  assert.deepEqual(links.map((a) => a.textContent), ['Home', 'Files', '<b>Notes</b>'], 'escaped names, no unsafe app');
  assert.equal(links[0].getAttribute('href'), 'http://localhost:7777/');
  assert.equal(links[1].getAttribute('aria-current'), 'true');
  assert.ok(shell.querySelector('.shell-bar sol-nav:not([hidden])'), 'the phone button is there');
  assert.ok(!setNav({ home: 'javascript:1', apps: [] }), 'a bad home is refused');
  shell.remove();
});

// sRGB hex -> contrast of white on it (WCAG).
const whiteOn = (hex) => {
  const lin = [1, 3, 5].map((i) => { const c = parseInt(hex.slice(i, i + 2), 16) / 255; return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; });
  return 1.05 / (0.2126 * lin[0] + 0.7152 * lin[1] + 0.0722 * lin[2] + 0.05);
};

test('app icon gradients keep a white glyph readable at every hue', () => {
  for (let h = 0; h < 360; h += 5) {
    const [a, b] = appIconColors(h);
    assert.ok(/^#[0-9a-f]{6}$/.test(a) && /^#[0-9a-f]{6}$/.test(b), `${h}: ${a} ${b}`);
    // Rounding to 8 bits can move the ratio by a hair; 4.4 still means the light stop was fitted.
    assert.ok(whiteOn(a) >= MIN_CONTRAST - 0.1, `hue ${h}: white on ${a} is ${whiteOn(a).toFixed(2)}:1`);
    assert.ok(whiteOn(b) > whiteOn(a), `hue ${h}: the dark stop is darker`);
  }
});
