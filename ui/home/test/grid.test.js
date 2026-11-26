// Tests for the Apps grid: web apps and connected Android apps (grid.js), rendered for real.
import { test, assert } from '../../kit/testing.js';
import { html, render } from '/sol.js';
import { gridApps, appOpenKind, appTile, sideApp } from '../grid.js';

const files = { id: 'files', name: 'Files', url: 'http://files.localhost:7777/', running: true };
const raypass = { id: 'raypass', name: 'Ray<b>pass</b>', external: true, package: 'app.raypass', icon: '/app-icons/raypass?v=1' };
const headless = { id: 'tool', name: 'Tool', external: true }; // external, not an APK: nothing to show
const hidden = { id: 'worker', name: 'Worker' }; // no UI
const bridge = (open) => ({ SolAndroid: { isApp: () => true, openApp: open } });

test('gridApps: web apps and connected Android apps, nothing else', () => {
  assert.deepEqual(gridApps([files, raypass, headless, hidden, null]).map((a) => a.id), ['files', 'raypass']);
  assert.deepEqual(gridApps(null), []);
});

test('appOpenKind: web, native through the bridge, none in a browser', () => {
  assert.equal(appOpenKind(files, {}), 'web');
  assert.equal(appOpenKind(raypass, {}), 'none');
  assert.equal(appOpenKind(raypass, bridge(() => true)), 'native');
  assert.equal(appOpenKind(raypass, { SolAndroid: { isApp: () => false, openApp: () => true } }), 'none'); // not a Sol page
  assert.equal(appOpenKind(raypass, { SolAndroid: { isApp: true } }), 'none'); // an older shell
});

function grid(win) {
  const el = document.createElement('div');
  render(el, html`${gridApps([files, raypass, hidden]).map((a, i) => appTile(a, i, win))}`);
  document.body.append(el);
  return el;
}

test('the grid in the Android app: the connected app is a tile with its icon that opens natively', () => {
  const el = grid(bridge(() => true));
  const tiles = el.querySelectorAll('.app');
  assert.equal(tiles.length, 2);
  assert.equal(tiles[0].getAttribute('href'), files.url);
  const t = tiles[1];
  assert.equal(t.dataset.act, 'open-native');
  assert.equal(t.dataset.id, 'raypass');
  assert.equal(t.querySelector('img').getAttribute('src'), '/app-icons/raypass?v=1');
  assert.equal(t.textContent.trim(), 'Ray<b>pass</b>'); // escaped, not markup
  assert.equal(t.querySelector('b'), null);
  el.remove();
});

test('the grid in a browser: the Android app shows, is labelled and is not a link', () => {
  const el = grid({});
  const t = el.querySelectorAll('.app')[1];
  assert.equal(t.tagName, 'DIV');
  assert.equal(t.getAttribute('aria-disabled'), 'true');
  assert.equal(t.dataset.act, undefined);
  assert.ok(t.textContent.includes('Android app'));
  el.remove();
});

test('an Android app without an icon gets the kit glyph', () => {
  const el = document.createElement('div');
  render(el, appTile({ ...raypass, icon: '' }, 0, bridge(() => true)));
  assert.ok(el.querySelector('sol-app-icon'));
  assert.equal(el.querySelector('img'), null);
});

test('the sidebar lists the Android app too', () => {
  const el = document.createElement('div');
  render(el, html`${[files, raypass].map((a) => sideApp(a, bridge(() => true)))}`);
  const items = el.querySelectorAll('.side-app');
  assert.equal(items.length, 2);
  assert.equal(items[1].dataset.act, 'open-native');
  render(el, sideApp(raypass, {}));
  assert.equal(el.querySelector('[data-act]'), null);
});
