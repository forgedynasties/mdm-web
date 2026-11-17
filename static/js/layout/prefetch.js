// L4 loading (picked 17 Nov 2026, UI review: "make sure the loading is as less as possible
// through smart fetching"): a view is ready before it is clicked, and the bar
// (pane-loading.js, navigation.js) covers the clicks that couldn't be predicted.
//
// Resting the pointer 80 ms on a rail row, a page pill or a release in the list fetches its
// view in the background with the same headers htmx's click will send, plus X-Mdm-Pf: the
// server lets the browser keep that answer for 60 s (render.go), so the click is answered
// from the browser's cache and swaps in at once; htmx never knows. A view opened in the last
// minute is kept the same way, so going back and forth costs nothing. A copy shown that was
// more than 3 s old is fetched again quietly and swapped in only if it changed and the page
// hasn't been touched since. One prefetch at a time; none on touch screens or with data-saver.
//
// X-Mdm-Pf carries a random token, and the browser keys its copies by it (Vary): a new token on
// every page load and after anything sent that changes data (a save, a command), so a copy
// kept before that can never be served.
(function () {
  var SEL = '.fleet-md > .fleet-rail [hx-get], .fleet-md > .fleet-rail a[href], [data-pill-nav] a[href], .ro-pills a[href], .ro-list a[href]';
  var TTL = 60000, REFRESH_AFTER = 3000;
  var seen = {}, busy = false, hoverT = null, touched = 0;
  function newToken() { return Math.random().toString(36).slice(2, 12); }
  var token = newToken();
  var canHover = !(window.matchMedia && matchMedia('(hover: none)').matches);
  var saveData = !!(navigator.connection && navigator.connection.saveData);

  function urlOf(el) {
    var u = el.getAttribute('hx-get') || el.getAttribute('href');
    if (!u || u.charAt(0) !== '/' || u.indexOf('//') === 0) return null;
    if (el.closest('[hx-boost="false"]') || el.hasAttribute('download') || el.getAttribute('target')) return null;
    if (el.hasAttribute('hx-post') || el.hasAttribute('hx-delete')) return null;
    return u;
  }
  function boosted(el) { return !el.hasAttribute('hx-get'); }
  function headersFor(el) {
    var h = { 'HX-Request': 'true', 'X-Mdm-Pf': token };
    if (boosted(el)) h['HX-Boosted'] = 'true';
    return h;
  }
  function keyOf(u, el) { return token + ' ' + (boosted(el) ? 'b ' : 'p ') + new URL(u, location.href).href; }
  function fresh(k) { var s = seen[k]; return !!s && Date.now() - s.at < TTL; }
  function candidate(t) { var el = t && t.closest && t.closest(SEL); return el && urlOf(el) ? el : null; }

  function prefetch(el) {
    var u = urlOf(el); if (!u || busy) return;
    var k = keyOf(u, el);
    if (fresh(k)) return;
    busy = true;
    fetch(u, { headers: headersFor(el), credentials: 'same-origin', cache: 'no-cache' })
      .then(function (r) { if (r.ok && r.headers.get('Cache-Control') && /max-age=[1-9]/.test(r.headers.get('Cache-Control'))) return r.text().then(function () { if (k.indexOf(token + ' ') === 0) seen[k] = { at: Date.now() }; }); })
      .catch(function () {})
      .then(function () { busy = false; });
  }
  if (canHover && !saveData) {
    document.addEventListener('pointerover', function (e) {
      var el = candidate(e.target); if (!el) return;
      if (el.classList.contains('on') || el.getAttribute('aria-current') === 'page') return;
      clearTimeout(hoverT);
      hoverT = setTimeout(function () { prefetch(el); }, 80);
    });
    document.addEventListener('pointerout', function (e) { if (candidate(e.target)) clearTimeout(hoverT); });
  }

  // A change retires every kept copy (new token): htmx sends, and the pages' own fetch() calls.
  function changed() { token = newToken(); seen = {}; }
  document.addEventListener('htmx:beforeRequest', function (e) {
    var v = (e.detail && e.detail.requestConfig && e.detail.requestConfig.verb || 'get').toLowerCase();
    if (v !== 'get') changed();
  });
  var _fetch = window.fetch;
  if (_fetch) window.fetch = function (input, init) {
    var m = (init && init.method) || (input && input.method) || 'GET';
    if (String(m).toUpperCase() !== 'GET' && String(m).toUpperCase() !== 'HEAD') changed();
    return _fetch.apply(this, arguments);
  };

  // htmx's own request for a rail/pill click carries the token: a copy kept under it answers
  // at once, otherwise the server does and its answer is kept for the next minute.
  document.addEventListener('htmx:configRequest', function (e) {
    var d = e.detail || {}, el = d.elt;
    if ((d.verb || '').toLowerCase() !== 'get' || !el || !el.matches || !el.matches(SEL)) return;
    var u = urlOf(el); if (!u) return;
    var k = keyOf(u, el);
    d.headers['X-Mdm-Pf'] = token;
    var pf = { k: k, kept: fresh(k), at: seen[k] && seen[k].at, url: u, refresh: !!el._pfRefresh, el: el };
    el._pfRefresh = false;
    pending = pf;
  });
  var pending = null;
  document.addEventListener('htmx:beforeSend', function (e) {
    if (pending && e.detail && e.detail.xhr) { e.detail.xhr._pf = pending; pending = null; }
  });
  document.addEventListener('htmx:afterRequest', function (e) {
    var d = e.detail || {}, pf = d.xhr && d.xhr._pf;
    if (!pf || !d.successful) return;
    var text = d.xhr.responseText;
    if (pf.kept) {
      // Shown from the kept copy. Older than a few seconds: check it in the background.
      if (!pf.refresh && Date.now() - pf.at > REFRESH_AFTER) refreshLater(pf, pf.el, text);
    } else if (pf.k.indexOf(token + ' ') === 0) {
      seen[pf.k] = { at: Date.now() };
    }
  });
  ['pointerdown', 'keydown', 'wheel'].forEach(function (n) {
    document.addEventListener(n, function () { touched = Date.now(); }, true);
  });
  function refreshLater(pf, el, shownText) {
    var since = Date.now();
    fetch(pf.url, { headers: headersFor(el), credentials: 'same-origin', cache: 'no-cache' })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (t) {
        if (t == null) return;
        if (pf.k.indexOf(token + ' ') === 0) seen[pf.k] = { at: Date.now() };
        if (t === shownText || touched > since) return;
        // Swap the fresh view in the same way it came: the same rail row or pill, now
        // answered from the copy just refreshed.
        var again = null;
        document.querySelectorAll(SEL).forEach(function (x) { if (!again && urlOf(x) === pf.url) again = x; });
        if (!again) return;
        again._pfRefresh = true;
        // Not a new step in the browser's history: the same view, refreshed.
        var had = again.getAttribute('hx-push-url');
        again.setAttribute('hx-push-url', 'false');
        var restore = function () { if (had == null) again.removeAttribute('hx-push-url'); else again.setAttribute('hx-push-url', had); };
        setTimeout(restore, 0);
        again.click();
      })
      .catch(function () {});
  }
})();
