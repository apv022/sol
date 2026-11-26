// The Apps grid (Home and the Apps page) and the sidebar's app list. A web app opens its own
// origin; an Android app connected to Sol (external, design/APPS_EVERYWHERE.md M5) has no web UI
// and opens natively through the Android shell's bridge (window.SolAndroid.openApp, which checks
// the id against the node's own app list). Without the bridge (a browser) such an app shows as
// "Android app" and is not a link: nothing here could open it. Every value from the API reaches
// the DOM through html`` (escaped).
import { html, raw } from '/sol.js';
import { icons } from '/kit/icons.js';
import { appIconSpec, isAndroidApp } from '/util.js';

/** The apps the grid shows: those with a web UI and the connected Android apps. */
export function gridApps(apps) {
  return (Array.isArray(apps) ? apps : []).filter((a) => a && (a.url || (a.external && a.package)));
}

/**
 * How a grid app opens here: "web" (its URL), "native" (the Android shell launches it) or
 * "none" (an Android app seen from a browser, which cannot start it).
 */
export function appOpenKind(a, win = globalThis) {
  if (a?.url) return 'web';
  if (a?.external && a.package && isAndroidApp(win) && typeof win.SolAndroid?.openApp === 'function') return 'native';
  return 'none';
}

/** An app's icon: its image (a manifest image, or a connected app's launcher icon), or a glyph on a gradient. */
export function appArt(a) {
  const spec = appIconSpec(a, icons);
  return spec.kind === 'img' ? html`<span class="app-img"><img src="${spec.src}" alt=""></span>` : html`<sol-app-icon glyph="${spec.glyph}" hue="${spec.hue}"></sol-app-icon>`;
}

/** One tile of the grid; i staggers the first paint's animation. */
export function appTile(a, i, win = globalThis) {
  const art = appArt(a);
  switch (appOpenKind(a, win)) {
    case 'web': return html`<a class="app" data-var-i="${i}" href="${a.url}" ${a.running === false ? raw('data-off') : ''}>${art}<span>${a.name}</span></a>`;
    case 'native': return html`<a class="app" data-var-i="${i}" href="#" role="button" data-act="open-native" data-id="${a.id}">${art}<span>${a.name}</span></a>`;
    default: return html`<div class="app" data-var-i="${i}" data-native aria-disabled="true" title="An Android app: open it in Sol on your phone">${art}<span>${a.name}</span><small class="app-where">Android app</small></div>`;
  }
}

/** One app of the sidebar's Apps group. */
export function sideApp(a, win = globalThis) {
  switch (appOpenKind(a, win)) {
    case 'web': return html`<a class="side-item side-app" href="${a.url}" ${a.running === false ? raw('data-off') : ''}>${appArt(a)}${a.name}</a>`;
    case 'native': return html`<a class="side-item side-app" href="#" role="button" data-act="open-native" data-id="${a.id}">${appArt(a)}${a.name}</a>`;
    default: return html`<span class="side-item side-app" data-native aria-disabled="true" title="An Android app: open it in Sol on your phone">${appArt(a)}<span class="grow">${a.name}<span class="row-sub">Android app</span></span></span>`;
  }
}
