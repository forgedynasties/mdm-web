// AIO MDM service worker: makes the dashboard installable and keeps the app shell
// (fonts, CSS, vendor JS, icons) available offline. Pages themselves are always
// network-first — the dashboard is live data — with a small offline fallback so an
// installed app never shows the browser's error page.
var VERSION = 'mdm-shell-v1';
var SHELL = [
  '/static/style.css', '/static/fonts/fonts.css', '/static/vendor/htmx.min.js',
  '/static/favicon.svg', '/static/icons/icon-192.png', '/static/icons/icon-512.png', '/static/offline.html'
];
self.addEventListener('install', function (e) {
  e.waitUntil(caches.open(VERSION).then(function (c) { return c.addAll(SHELL).catch(function () {}); }).then(function () { return self.skipWaiting(); }));
});
self.addEventListener('activate', function (e) {
  e.waitUntil(caches.keys().then(function (keys) {
    return Promise.all(keys.filter(function (k) { return k !== VERSION; }).map(function (k) { return caches.delete(k); }));
  }).then(function () { return self.clients.claim(); }));
});
self.addEventListener('fetch', function (e) {
  var req = e.request;
  if (req.method !== 'GET') return;
  var url = new URL(req.url);
  if (url.origin !== location.origin) return;
  // Static assets: cache-first (they are content-hashed by ?v=).
  if (url.pathname.startsWith('/static/')) {
    e.respondWith(caches.match(req).then(function (hit) {
      return hit || fetch(req).then(function (res) {
        if (res.ok) { var copy = res.clone(); caches.open(VERSION).then(function (c) { c.put(req, copy); }); }
        return res;
      });
    }));
    return;
  }
  // Live streams / partials: never intercept.
  if (req.headers.get('accept') === 'text/event-stream' || req.headers.get('HX-Request')) return;
  // Pages: network-first, offline fallback.
  if (req.mode === 'navigate') {
    e.respondWith(fetch(req).catch(function () { return caches.match('/static/offline.html'); }));
  }
});
