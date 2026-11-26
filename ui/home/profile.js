// My profile in the Sol app (design/ADMIN.md §Profile): the card at the top of Settings, and the
// picture pipeline. The daemon decodes no image, so the picture is cropped to a centred square
// and re-encoded to AVATAR_PX × AVATAR_PX here, with a canvas, until it fits in AVATAR_MAX bytes.
// Every value from the API reaches the DOM through html`` (escaped); the picture's URL is used
// only when it is a same-origin path (sol-avatar checks it again).
import { html } from '/sol.js';

/** The picture's side in pixels and its largest encoded size (the daemon's limit). */
export const AVATAR_PX = 256;
export const AVATAR_MAX = 64 * 1024;

/** The source rectangle of the centred square crop of a w × h image. */
export function squareCrop(w, h) {
  const size = Math.min(w, h);
  return { sx: Math.floor((w - size) / 2), sy: Math.floor((h - size) / 2), size };
}

/** My name, the picture URL and a line saying where the name comes from, from `status.me`. */
export function meView(status) {
  const me = status?.me || {};
  const p = me.profile || {};
  const name = p.name || me.person?.name || '';
  const avatar = /^\/(?![/\\])/.test(p.avatar || '') ? p.avatar : '';
  const sub = p.name_source === 'profile' && p.cert_name && p.cert_name !== p.name
    ? `Shown to your devices and contacts. You started as ${p.cert_name}.`
    : 'Shown to your devices and contacts.';
  return { id: me.person?.id || 'me', name, avatar, sub, canSign: p.can_sign !== false };
}

/**
 * The profile card: my picture (or the letter), my name, and Change name, Change picture and
 * Remove picture. Buttons carry data-act (profile-name, profile-picture, profile-clear) for the
 * page's click handler.
 */
export function profileCard(status) {
  const v = meView(status);
  return html`<section class="section profile-card"><div class="list"><div data-item class="profile-row">
    <sol-avatar name="${v.name || '?'}" id="${v.id}" size="64px" src="${v.avatar}"></sol-avatar>
    <span class="grow"><span class="row-title profile-name">${v.name || 'No name yet'}</span><span class="row-sub">${v.sub}${v.canSign ? '' : ' Changes are signed by your admin device, which must be on.'}</span></span></div>
    <div data-item class="profile-actions"><button type="button" data-act="profile-name">Change name</button><button type="button" data-act="profile-picture">Change picture</button>${v.avatar ? html`<button type="button" class="danger" data-act="profile-clear">Remove picture</button>` : ''}</div></div></section>`;
}

/** Why a picked file can't be a picture, or '' if it may be one. */
export function avatarFileError(file) {
  if (!file) return 'No file chosen';
  if (!/^image\//.test(file.type || '')) return 'Choose an image (JPEG, PNG or WebP)';
  if (file.size > 50 * 1024 * 1024) return 'That image is too large';
  return '';
}

/**
 * Crops an image (a File or Blob the browser can decode) to a centred square, scales it to
 * AVATAR_PX and encodes it as WebP (JPEG where WebP can't be made), lowering the quality until it
 * fits in AVATAR_MAX. Resolves to a Blob; rejects when the image can't be read.
 */
export async function encodeAvatar(file, { px = AVATAR_PX, max = AVATAR_MAX } = {}) {
  const img = await createImageBitmap(file);
  const { sx, sy, size } = squareCrop(img.width, img.height);
  const canvas = document.createElement('canvas');
  canvas.width = canvas.height = px;
  const ctx = canvas.getContext('2d');
  ctx.imageSmoothingQuality = 'high';
  ctx.drawImage(img, sx, sy, size, size, 0, 0, px, px);
  img.close?.();
  const blobOf = (type, q) => new Promise((ok) => canvas.toBlob(ok, type, q));
  for (const type of ['image/webp', 'image/jpeg']) {
    for (const q of [0.9, 0.8, 0.7, 0.6, 0.5, 0.4, 0.3]) {
      const b = await blobOf(type, q);
      if (!b || b.type !== type) break; // this browser can't make it: try the next type
      if (b.size <= max) return b;
    }
  }
  throw new Error('Could not make the picture small enough');
}
