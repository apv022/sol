// App icon colours: one function shared by <sol-app-icon> (kit.js) and cmd/iconrender (the
// icon.svg/PNG files), so the icon a UI draws and the icon a launcher shows are the same.
//
// The gradient is picked in OKLCH, where equal lightness looks equally light whatever the hue (in
// HSL a yellow at 62% is far lighter than a blue at 62%, which is why a white glyph on the old
// lime and cyan icons was barely visible). The light end is then darkened until white on it has
// at least 4.5:1 contrast (WCAG AA for text), so every hue passes, not just the blues.

/** Minimum contrast of the white glyph against the lightest point of the gradient. */
export const MIN_CONTRAST = 4.5;

/** Stroke width of the glyph in an app icon, on the 24-unit glyph grid (the UI glyphs use 1.75). */
export const ICON_STROKE = 2.25;

// OKLab -> linear sRGB (Björn Ottosson's published matrices).
function oklchToLinear(L, C, h) {
  const r = (h * Math.PI) / 180, a = C * Math.cos(r), b = C * Math.sin(r);
  const l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3;
  const m = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3;
  const s = (L - 0.0894841775 * a - 1.2914855480 * b) ** 3;
  return [
    4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s,
    -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s,
    -0.0041960863 * l - 0.7034186147 * m + 1.7076147010 * s,
  ];
}

const inGamut = (rgb) => rgb.every((v) => v >= -1e-4 && v <= 1 + 1e-4);

// The colour at lightness L and hue h with the most chroma (up to C) that sRGB can show.
function fit(L, C, h) {
  let rgb = oklchToLinear(L, C, h);
  for (let c = C; !inGamut(rgb) && c > 0; c -= 0.005) rgb = oklchToLinear(L, Math.max(c, 0), h);
  return rgb.map((v) => Math.min(1, Math.max(0, v)));
}

const luminance = ([r, g, b]) => 0.2126 * r + 0.7152 * g + 0.0722 * b;

/** Contrast ratio of white against a linear-sRGB colour. */
export const whiteContrast = (rgb) => 1.05 / (luminance(rgb) + 0.05);

const hex = (rgb) => '#' + rgb.map((v) => {
  const s = v <= 0.0031308 ? 12.92 * v : 1.055 * v ** (1 / 2.4) - 0.055;
  return Math.round(Math.min(1, Math.max(0, s)) * 255).toString(16).padStart(2, '0');
}).join('');

const cache = new Map();

/**
 * The two gradient stops of an app icon of hue h (degrees), as #rrggbb: [light, dark]. The light
 * stop is as light as it can be while white on it keeps MIN_CONTRAST.
 */
export function appIconColors(h) {
  const hue = ((Number(h) || 0) % 360 + 360) % 360;
  if (cache.has(hue)) return cache.get(hue);
  let L = 0.66, top = fit(L, 0.19, hue);
  while (whiteContrast(top) < MIN_CONTRAST && L > 0.3) { L -= 0.01; top = fit(L, 0.19, hue); }
  const out = [hex(top), hex(fit(L - 0.14, 0.17, (hue + 12) % 360))];
  cache.set(hue, out);
  return out;
}
