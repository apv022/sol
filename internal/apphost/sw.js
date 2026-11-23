// Sol's service worker, served by the daemon at /_sol/sw.js on every app origin and the Sol app
// (design/APPS_EVERYWHERE.md M1). It makes an installed app open instantly and shows a plain
// "Sol isn't running" page instead of the browser's error when the daemon is down. It never
// caches API data: apps' data is local, pushed and always live.
//
// What is cached (same-origin GET only): the kit, sol.js, the theme and the icons
// (stale-while-revalidate), scripts, styles and fonts of the app, and navigations to a plain
// path without a query (the app shell; /raw?path=... style loads are never kept).
const CACHE = 'sol-shell-v1';
const SHELL = ['/kit/tokens.css', '/kit/kit.css', '/_sol/theme.css', '/sol.js'];
// The only /_sol/ paths that may be cached; the rest (the API bridge, /nav, login) never is.
const STATIC_SOL = ['/_sol/theme.css', '/_sol/manifest.webmanifest', '/_sol/icon.svg', '/_sol/icon-192.png', '/_sol/icon-512.png'];
// Styled with the kit's classes only: the page's CSP forbids inline styles and scripts.
const OFFLINE = '<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">' +
  '<title>Sol isn\'t running</title><link rel="stylesheet" href="/kit/tokens.css"><link rel="stylesheet" href="/kit/kit.css"></head>' +
  '<body><main class="empty"><h2>Sol isn\'t running</h2><p>Start it with <code>sol</code> or from your app menu.</p></main></body></html>';

self.addEventListener('install', (e) => {
  // One missing file must not stop the worker installing.
  e.waitUntil(caches.open(CACHE).then((c) => Promise.all(SHELL.map((u) => c.add(u).catch(() => {})))).then(() => self.skipWaiting()));
});
self.addEventListener('activate', (e) => {
  e.waitUntil(caches.keys().then((ks) => Promise.all(ks.filter((k) => k !== CACHE).map((k) => caches.delete(k)))).then(() => self.clients.claim()));
});

// Answers from the cache at once and refreshes it in the background; the first visit waits for the network.
async function swr(req) {
  const cache = await caches.open(CACHE);
  const hit = await cache.match(req);
  const net = fetch(req).then((res) => { if (res.ok && !/no-store/.test(res.headers.get('Cache-Control') || '')) cache.put(req, res.clone()); return res; });
  if (hit) { net.catch(() => {}); return hit; }
  return net;
}

self.addEventListener('fetch', (e) => {
  const req = e.request, url = new URL(req.url);
  if (req.method !== 'GET' || url.origin !== self.location.origin || req.headers.has('Range')) return;
  const p = url.pathname;
  if (p.startsWith('/_sol/')) { if (STATIC_SOL.includes(p)) e.respondWith(swr(req)); return; }
  if (req.mode === 'navigate') {
    e.respondWith((async () => {
      try {
        const res = await fetch(req);
        // Keep the shell only: a plain path, a good HTML answer, nothing private-looking.
        if (res.ok && !url.search && /text\/html/.test(res.headers.get('Content-Type') || '')) (await caches.open(CACHE)).put(req, res.clone());
        return res;
      } catch {
        // The daemon is down: the last shell if we have one (the page shows its own errors), else the offline page.
        return (await caches.match(req)) || new Response(OFFLINE, { status: 503, headers: { 'Content-Type': 'text/html; charset=utf-8' } });
      }
    })());
    return;
  }
  if (p === '/sol.js' || p.startsWith('/kit/') || ['script', 'style', 'font'].includes(req.destination)) e.respondWith(swr(req));
});
