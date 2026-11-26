// Tests for the profile card and the picture pipeline (profile.js), rendered for real.
import { test, assert } from '../../kit/testing.js';
import { render } from '/sol.js';
import '/kit/kit.js';
import { profileCard, meView, squareCrop, encodeAvatar, avatarFileError, AVATAR_PX, AVATAR_MAX } from '../profile.js';
import { createAdmin } from '../admin.js';

const status = (profile, name = 'Ada') => ({ me: { person: { id: 'p1', name }, device: { id: 'd1', name: 'laptop' }, profile } });

function card(st) {
  const el = document.createElement('div');
  render(el, profileCard(st));
  document.body.append(el);
  return el;
}

test('profile card: the picture, the name and the three actions', () => {
  const el = card(status({ name: 'Ada Lovelace', name_source: 'profile', cert_name: 'Ada', avatar: '/avatars/abc', can_sign: true }));
  const av = el.querySelector('sol-avatar');
  assert.equal(av.getAttribute('src'), '/avatars/abc');
  assert.equal(av.querySelector('img')?.getAttribute('alt'), '', 'decorative img');
  assert.equal(el.querySelector('.profile-name').textContent, 'Ada Lovelace');
  assert.ok(el.textContent.includes('You started as Ada'), 'says where the name came from');
  assert.deepEqual([...el.querySelectorAll('button')].map((b) => b.dataset.act), ['profile-name', 'profile-picture', 'profile-clear']);
  el.remove();
});

test('profile card: no picture means the letter and no Remove; names are escaped', () => {
  const el = card(status({ name: '<b>Eve</b>', name_source: 'profile', can_sign: false }));
  const av = el.querySelector('sol-avatar');
  assert.equal(av.querySelector('img'), null);
  assert.equal(av.textContent, '<');
  assert.equal(el.querySelector('.profile-name').textContent, '<b>Eve</b>');
  assert.equal(el.querySelector('b'), null, 'no markup from data');
  assert.equal(el.querySelector('[data-act="profile-clear"]'), null);
  assert.ok(el.textContent.includes('admin device'), 'a device without P says who signs');
  el.remove();
});

test('meView: falls back to the certificate name and drops foreign picture URLs', () => {
  assert.equal(meView(status(undefined, 'Ada')).name, 'Ada');
  assert.equal(meView(status({ avatar: 'https://evil.example/x.png' })).avatar, '');
  assert.equal(meView(status({ avatar: '//evil.example/x.png' })).avatar, '');
  assert.equal(meView(null).name, '');
});

test('<sol-avatar src>: the picture over the letter, the letter back when it fails', async () => {
  const av = document.createElement('sol-avatar');
  av.setAttribute('name', 'Bob');
  av.setAttribute('src', '/nope/missing.png');
  document.body.append(av);
  const img = av.querySelector('img');
  assert.ok(img && img.alt === '', 'an img with an empty alt');
  assert.equal(av.getAttribute('aria-label'), 'Bob');
  for (let i = 0; i < 50 && !img.hidden; i++) await new Promise((r) => setTimeout(r, 20));
  assert.ok(img.hidden, 'a broken picture hides');
  assert.equal(av.textContent, 'B', 'the letter shows');
  // An off-origin src is never loaded.
  av.setAttribute('src', 'https://evil.example/x.png');
  assert.equal(av.querySelector('img'), null);
  // A data: picture loads and stays across a re-render with the same src.
  const c = document.createElement('canvas'); c.width = c.height = 4;
  const url = c.toDataURL('image/png');
  av.setAttribute('src', url);
  const pic = av.querySelector('img');
  for (let i = 0; i < 50 && !av.hasAttribute('data-picture'); i++) await new Promise((r) => setTimeout(r, 20));
  assert.ok(av.hasAttribute('data-picture'), 'loaded');
  av.setAttribute('name', 'Bobby');
  assert.equal(av.querySelector('img'), pic, 'same img kept');
  assert.equal(av.getAttribute('aria-label'), 'Bobby');
  av.removeAttribute('src');
  assert.equal(av.querySelector('img'), null);
  av.remove();
});

test('squareCrop centres the largest square', () => {
  assert.deepEqual(squareCrop(400, 300), { sx: 50, sy: 0, size: 300 });
  assert.deepEqual(squareCrop(300, 500), { sx: 0, sy: 100, size: 300 });
  assert.deepEqual(squareCrop(256, 256), { sx: 0, sy: 0, size: 256 });
});

test('encodeAvatar: a big photo becomes a small square', async () => {
  const c = document.createElement('canvas');
  c.width = 1600; c.height = 900;
  const g = c.getContext('2d');
  for (let i = 0; i < 4000; i++) { g.fillStyle = `hsl(${i % 360} 70% ${30 + (i % 40)}%)`; g.fillRect((i * 37) % 1600, (i * 53) % 900, 23, 17); }
  const src = await new Promise((ok) => c.toBlob(ok, 'image/png'));
  const out = await encodeAvatar(src);
  assert.ok(out.size > 0 && out.size <= AVATAR_MAX, `size ${out.size}`);
  assert.ok(['image/webp', 'image/jpeg'].includes(out.type), out.type);
  const bmp = await createImageBitmap(out);
  assert.equal(bmp.width, AVATAR_PX); assert.equal(bmp.height, AVATAR_PX);
  let threw = false;
  try { await encodeAvatar(new Blob(['not an image'], { type: 'image/png' })); } catch { threw = true; }
  assert.ok(threw, 'an unreadable image rejects');
  assert.ok(avatarFileError({ type: 'text/plain', size: 3 }));
  assert.equal(avatarFileError({ type: 'image/heic', size: 3 }), '');
});

test('admin client: profile requests', async () => {
  const calls = [];
  const f = async (url, init) => { calls.push([init.method, url, init.headers['Content-Type'] || '', init.body]); return new Response('{"name":"x"}', { status: 200 }); };
  const a = createAdmin({ fetch: f });
  await a.me();
  await a.setName('Ada');
  const blob = new Blob(['x'], { type: 'image/webp' });
  await a.setAvatar(blob);
  await a.clearAvatar();
  assert.deepEqual(calls.map((c) => c.slice(0, 3)), [
    ['GET', '/admin/v1/me', ''], ['PATCH', '/admin/v1/me', 'application/json'],
    ['PUT', '/admin/v1/me/avatar', 'image/webp'], ['DELETE', '/admin/v1/me/avatar', '']]);
  assert.equal(calls[1][3], '{"name":"Ada"}');
  assert.equal(calls[2][3], blob, 'the bytes as they are');
});
