// Tests for the Files viewer (apps/files/ui/viewer.js): the pure swipe and step rules, and the
// real viewer in this page driven by keys, chevrons and synthetic touch pointer events.
import { test, assert } from '../../../ui/kit/testing.js';
import { viewKind, stepIndex, swipeIntent, resist, preloadTargets, position, openViewer, openViewerNow } from '../ui/viewer.js';

// A 1x1 PNG, so image slides load without a server.
const PNG = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==';
const items = [
  { name: 'a.jpg', path: 'P/a.jpg', size: 10 },
  { name: 'b.png', path: 'P/b.png', size: 20 },
  { name: 'notes.md', path: 'P/notes.md', size: 30 },
  { name: 'paper.pdf', path: 'P/paper.pdf', size: 40 },
  { name: 'c.webp', path: 'P/c.webp', size: 50 },
];
const open = (i, extra = {}) => openViewer(items, i, { src: () => PNG, duration: 0, history: false, ...extra });
const meta = () => document.querySelector('.viewer .v-meta').textContent;
const name = () => document.querySelector('.viewer .v-name').textContent;
const key = (k) => dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true }));

function swipe(v, dx, { ms = 60, dy = 0, steps = 6 } = {}) {
  const st = v.stage;
  const r = st.getBoundingClientRect();
  const x0 = r.left + r.width / 2, y0 = r.top + r.height / 2;
  const target = st.querySelector('.v-slide[data-current]') || st;
  const ev = (type, x, y) => target.dispatchEvent(new PointerEvent(type, { pointerId: 7, pointerType: 'touch', clientX: x, clientY: y, bubbles: true, isPrimary: true }));
  const t0 = performance.now();
  ev('pointerdown', x0, y0);
  for (let i = 1; i <= steps; i++) ev('pointermove', x0 + (dx * i) / steps, y0 + (dy * i) / steps);
  // Simulate the drag's duration without waiting: the viewer measures with performance.now().
  const real = performance.now;
  performance.now = () => t0 + ms;
  try { ev('pointerup', x0 + dx, y0 + dy); } finally { performance.now = real; }
}

test('kinds by extension', () => {
  assert.equal(viewKind('IMG_1.JPG'), 'image');
  assert.equal(viewKind('clip.mp4'), 'video');
  assert.equal(viewKind('song.flac'), 'audio');
  assert.equal(viewKind('paper.pdf'), 'pdf');
  assert.equal(viewKind('README.md'), 'text');
  assert.equal(viewKind('main.go'), 'text');
  assert.equal(viewKind('archive.zip'), 'file');
  assert.equal(viewKind('noext'), 'file');
});

test('step stops at the ends', () => {
  assert.equal(stepIndex(0, -1, 5), 0);
  assert.equal(stepIndex(0, 1, 5), 1);
  assert.equal(stepIndex(4, 1, 5), 4);
  assert.equal(stepIndex(2, -1, 5), 1);
});

test('swipe commits on distance or speed, not on slow small or vertical drags', () => {
  assert.equal(swipeIntent(-120, 0, 400, 400), 'next', 'past a quarter of the width');
  assert.equal(swipeIntent(120, 0, 400, 400), 'prev');
  assert.equal(swipeIntent(-60, 0, 600, 400), null, 'short and slow');
  assert.equal(swipeIntent(-60, 0, 80, 400), 'next', 'short but a flick');
  assert.equal(swipeIntent(-15, 0, 10, 400), null, 'a jitter is not a swipe');
  assert.equal(swipeIntent(-150, 200, 100, 400), null, 'mostly vertical');
});

test('rubber band at the ends only', () => {
  assert.equal(resist(90, 0, 5), 30);
  assert.equal(resist(-90, 4, 5), -30);
  assert.equal(resist(-90, 0, 5), -90);
  assert.equal(resist(90, 2, 5), 90);
});

test('neighbour images are preloaded, other kinds are not', () => {
  assert.deepEqual(preloadTargets(items, 0), [1]);
  assert.deepEqual(preloadTargets(items, 2), [1]);
  assert.deepEqual(preloadTargets(items, 3), [4]);
  assert.equal(position(2, 12), '3 / 12');
});

test('keys and chevrons move, Escape closes', () => {
  const v = open(1);
  assert.equal(name(), 'b.png');
  assert.ok(meta().endsWith('2 / 5'), meta());
  key('ArrowRight');
  assert.equal(v.index, 2);
  assert.equal(name(), 'notes.md');
  key('ArrowLeft');
  key('ArrowLeft');
  assert.equal(v.index, 0);
  key('ArrowLeft');
  assert.equal(v.index, 0, 'no wrap at the start');
  assert.ok(document.querySelector('.viewer .v-prev').hidden, 'no previous chevron on the first file');
  document.querySelector('.viewer .v-next').click();
  assert.equal(v.index, 1);
  key('Escape');
  assert.ok(!document.querySelector('.viewer'), 'closed');
  assert.equal(openViewerNow(), null);
});

test('swipes follow the finger and move one file', () => {
  const v = open(0);
  swipe(v, -200);
  assert.equal(v.index, 1, 'swiped left: next');
  swipe(v, 200);
  assert.equal(v.index, 0, 'swiped right: previous');
  swipe(v, 200);
  assert.equal(v.index, 0, 'nothing before the first');
  swipe(v, -40, { ms: 50 });
  assert.equal(v.index, 1, 'a short flick moves');
  swipe(v, -40, { ms: 800 });
  assert.equal(v.index, 1, 'a short slow drag springs back');
  swipe(v, -200, { dy: 260 });
  assert.equal(v.index, 1, 'a vertical drag does not move');
  v.close();
});

test('only the current file and its neighbours are in the page, and slides are reused', () => {
  const v = open(0);
  const slides = () => [...v.track.querySelectorAll('.v-slide')];
  assert.equal(slides().length, 2);
  const b = v.slots.get(1);
  const bImg = b.querySelector('img');
  key('ArrowRight');
  assert.equal(slides().length, 3);
  assert.equal(v.slots.get(1), b, 'the slide that became current is the same element');
  assert.equal(b.querySelector('img'), bImg, 'its image was not reloaded');
  assert.ok(b.hasAttribute('data-current'));
  key('ArrowRight'); key('ArrowRight');
  assert.ok(!v.slots.has(0) && !v.slots.has(1), 'far slides are dropped');
  v.close();
});

test('text loads only when shown; PDF uses a frame on desktop and Open on Android', async () => {
  let opened = null;
  const v = open(1, { opener: (url, mime, n) => { opened = { url, mime, n }; return true; } });
  const md = v.slots.get(2);
  assert.ok(!md.querySelector('pre'), 'the text neighbour is a light card until shown');
  key('ArrowRight'); key('ArrowRight');
  const pdf = v.slots.get(3);
  assert.ok(!pdf.querySelector('iframe'), 'no frame with the Android opener');
  pdf.querySelector('[data-v="open"]').click();
  assert.equal(opened.mime, 'application/pdf');
  assert.equal(opened.n, 'paper.pdf');
  assert.ok(opened.url.startsWith('data:') || opened.url.startsWith('http'), opened.url);
  v.close();
  const d = open(3);
  assert.ok(d.slots.get(3).querySelector('iframe.v-pdf'), 'desktop: the browser PDF viewer in a frame');
  d.close();
});

test('actions, rename and delete keep the viewer in step', () => {
  const seen = [];
  const v = open(1, { actions: [{ act: 'delete', icon: 'trash', label: 'Delete' }, { act: 'rename', icon: 'edit', label: 'Rename', when: (e) => e.name !== 'b.png' }],
    onAction: (act, e) => seen.push(act + ':' + e.name) });
  assert.equal(document.querySelectorAll('.viewer .v-top [data-v-act]').length, 1, 'rename hidden by when()');
  document.querySelector('.viewer .v-top [data-v-act="delete"]').click();
  assert.deepEqual(seen, ['delete:b.png']);
  v.remove(v.index);
  assert.equal(name(), 'notes.md', 'after a delete the next file shows');
  assert.ok(meta().endsWith('2 / 4'), meta());
  v.replace(v.index, { ...v.item, name: 'renamed.md' });
  assert.equal(name(), 'renamed.md');
  v.close();
  const one = openViewer([items[0]], 0, { src: () => PNG, duration: 0, history: false });
  one.remove(0);
  assert.ok(!document.querySelector('.viewer'), 'deleting the last file closes the viewer');
});

test('Back closes the viewer', async () => {
  const v = openViewer(items, 0, { src: () => PNG, duration: 0 });
  assert.ok(history.state?.solViewer);
  const closed = new Promise((r) => addEventListener('popstate', () => setTimeout(r, 0), { once: true }));
  history.back();
  await closed;
  assert.ok(v.closed && !document.querySelector('.viewer'));
});
