// Loaded by templates/devices.html. Fleet pages P1 (picked 5 Nov 2026, user: "P1 instead of
// T1"): on a desktop window (html.shell) a page holds exactly the rows that fit, so nothing is
// ever off screen and Per page goes away; a short page keeps the table full with empty lines.
// Pages flip with ‹ ›, the numbers, ← → (or PgUp/PgDn) and one wheel gesture per page, and two
// pages either side are fetched in the background so a flip is instant. Scroll feel (10 Nov 2026,
// user picked "All four"): a real one-gesture-one-page, rows slide in the direction of travel,
// a position bar and edge fades show where you are and that more follows. Rows come from
// DeviceList with ?start=&fit= (fit sizes one request and is never saved). Phones and the
// Cards view keep the old pager.
(function () {
  var fp = window.flTL = { on: false, start: 0, fit: 25 };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init); else init();
  function init() {
  var C = document.getElementById('device-table-container');
  if (!C) return;
  var root = document.documentElement, total = 0, cache = {}, busy = false, queued = 0, barWas = 0;
  var noMotion = window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)');

  function wrap() { return C.querySelector('.ft-wrap'); }
  function usable() {
    var pane = document.getElementById('fl-map-pane');
    return root.classList.contains('shell') && !!C.querySelector('table.ft') && !(pane && !pane.hidden);
  }
  function rowH() { var r = C.querySelector('tbody tr.ft-row'); return r ? r.getBoundingClientRect().height : 44; }
  function headH() { var th = C.querySelector('.ft-wrap thead'); return th ? th.getBoundingClientRect().height : 34; }
  function fits() { var w = wrap(); return w ? Math.max(3, Math.floor((w.clientHeight - headH()) / rowH())) : 10; }
  function readFoot() { var f = C.querySelector('.fl-foot'); if (f) total = +f.dataset.total || 0; }
  function pageURL(start) {
    var keep = fp.start; fp.start = start;
    var u = fleetURL(getFilterVals()); fp.start = keep;
    return u;
  }

  // A short page keeps the table full: empty lines under the last device.
  function fill() {
    var tb = C.querySelector('.ft-wrap tbody'); if (!tb) return;
    tb.querySelectorAll('tr.ft-fill').forEach(function (r) { r.remove(); });
    var n = tb.querySelectorAll('tr.ft-row').length, cols = (C.querySelector('.ft-wrap thead tr') || {}).children;
    for (var i = n; i < fp.fit; i++) {
      var tr = document.createElement('tr'); tr.className = 'ft-fill';
      tr.innerHTML = '<td colspan="' + (cols ? cols.length : 12) + '"></td>';
      tr.style.height = rowH() + 'px';
      tb.appendChild(tr);
    }
  }
  function foot() {
    var el = C.querySelector('.fl-foot-n'); if (!el || !fp.on) return;
    var n = C.querySelectorAll('.ft-wrap tbody tr.ft-row').length;
    el.innerHTML = total ? '<b>' + (fp.start + 1) + '–' + (fp.start + n) + '</b> of ' + total + ' device' + (total === 1 ? '' : 's') : 'No devices';
  }

  // Where you are: a bar down the right edge, and a fade at an edge with more rows past it. The
  // rows are replaced on every flip, so the bar is drawn at its old spot and then moved.
  function cue() {
    var w = wrap(), table = w && w.querySelector('table.ft');
    if (!w || !table || !fp.on) return;
    var track = w.querySelector(':scope > .fl-cue');
    var pages = Math.ceil(total / fp.fit);
    if (pages <= 1) { if (track) track.hidden = true; w.querySelectorAll(':scope > .fl-fade').forEach(function (f) { f.classList.remove('on'); }); return; }
    if (!track) {
      w.insertAdjacentHTML('beforeend', '<div class="fl-fade top"></div><div class="fl-fade bot"></div><div class="fl-cue" aria-hidden="true"><div class="fl-cue-bar"></div></div>');
      track = w.querySelector(':scope > .fl-cue');
    }
    track.hidden = false;
    var head = headH(), h = table.offsetHeight, page = Math.floor(fp.start / fp.fit);
    track.style.top = (head + 4) + 'px';
    track.style.height = Math.max(0, h - head - 10) + 'px';
    var top = w.querySelector(':scope > .fl-fade.top'), bot = w.querySelector(':scope > .fl-fade.bot');
    top.style.top = head + 'px';
    bot.style.top = (h - 30) + 'px';
    top.classList.toggle('on', fp.start > 0);
    bot.classList.toggle('on', fp.start + fp.fit < total);
    var bar = track.firstChild, size = Math.max(8, 100 / pages), at = page / (pages - 1) * (100 - size);
    bar.style.height = size + '%';
    bar.style.transition = 'none';
    bar.style.top = barWas + '%';
    bar.getBoundingClientRect();
    bar.style.transition = '';
    bar.style.top = at + '%';
    barWas = at;
  }
  // Rows arrive from the direction of travel.
  function slide(dir) {
    var tb = C.querySelector('.ft-wrap tbody');
    if (!tb || !tb.animate || (noMotion && noMotion.matches)) return;
    tb.animate([{ transform: 'translateY(' + dir * 18 + 'px)', opacity: 0.35 }, { transform: 'none', opacity: 1 }], { duration: 150, easing: 'cubic-bezier(.2,.7,.2,1)' });
  }

  // Two pages either side, fetched quietly so a flip needs no wait (kept 30 s).
  function prefetch() {
    [fp.start - 2 * fp.fit, fp.start - fp.fit, fp.start + fp.fit, fp.start + 2 * fp.fit].forEach(function (st) {
      if (st < 0 || (total && st >= total)) return;
      var u = pageURL(st);
      if (cache[u] && Date.now() - cache[u].at < 30000) return;
      cache[u] = { at: Date.now(), p: fetch(u, { credentials: 'same-origin', headers: { 'HX-Request': 'true', 'HX-Current-URL': location.href } }).then(function (r) { return r.ok ? r.text() : null; }) };
    });
  }
  function flip(st) {
    if (!fp.on || busy) return;
    st = Math.max(0, st);
    if (total && st >= total) return;
    if (st === fp.start) return;
    busy = true;
    var u = pageURL(st), c = cache[u], dir = st > fp.start ? 1 : -1;
    fp.start = st;
    var done = function () {
      busy = false;
      history.replaceState(null, '', fleetURL(getFilterVals()));
      slide(dir);
      if (queued) { var d = queued; queued = 0; step(d); }
    };
    if (c && Date.now() - c.at < 30000) {
      c.p.then(function (html) {
        if (html == null) { htmx.ajax('GET', u, { target: C, swap: 'innerHTML' }).then(done, done); return; }
        htmx.swap(C, html, { swapStyle: 'innerHTML' });
        // The list's other listeners (selection, arrivals, the shell) hear a normal swap.
        C.dispatchEvent(new CustomEvent('htmx:afterSwap', { bubbles: true, detail: { target: C, elt: C } }));
        document.dispatchEvent(new CustomEvent('htmx:afterSettle', { detail: { target: C, elt: C } }));
        done();
      });
    } else htmx.ajax('GET', u, { target: C, swap: 'innerHTML' }).then(done, done);
  }
  // One page on or back. A step asked for while a page is still arriving runs after it.
  function step(d) {
    if (busy) { queued = d; return; }
    flip(fp.start + d * fp.fit);
  }
  function pageOf(n) { return (Math.max(1, n) - 1) * fp.fit; }

  // ‹ › and the page numbers flip through the cache (ahead of roster.js's own handler).
  document.addEventListener('click', function (e) {
    if (!fp.on) return;
    var b = e.target.closest && e.target.closest('.page-nav[data-page]');
    if (!b || !C.contains(b)) return;
    e.preventDefault(); e.stopImmediatePropagation();
    flip(pageOf(+b.dataset.page));
  }, true);
  // One wheel gesture = one page. Movement adds up until it is enough to flip once; then the
  // wheel is ignored until it goes quiet, so a trackpad's momentum can't flip again. Three things
  // start a new gesture early: a change of direction, a push harder than the last event (a new
  // flick during the old one's momentum), and a mouse wheel's next notch.
  var acc = 0, armed = true, lastT = 0, lastD = 0, flippedAt = 0;
  C.addEventListener('wheel', function (e) {
    if (!fp.on || !e.target.closest('.ft-wrap')) return;
    e.preventDefault();
    var now = Date.now(), d = e.deltaMode === 1 ? e.deltaY * 40 : e.deltaY, abs = Math.abs(d), was = Math.abs(lastD);
    if (!d) return;
    var notch = abs >= 80 && Number.isInteger(d) && abs === was && now - flippedAt > 220;
    var harder = abs > was * 1.6 && abs > 30 && now - flippedAt > 200;
    if (now - lastT > 140 || d * lastD < 0 || notch || harder) { armed = true; acc = 0; }
    lastT = now; lastD = d; acc += d;
    if (armed && Math.abs(acc) >= 40) {
      armed = false; flippedAt = now;
      step(acc > 0 ? 1 : -1);
    }
  }, { passive: false });
  document.addEventListener('keydown', function (e) {
    if (!fp.on || e.altKey || e.ctrlKey || e.metaKey) return;
    var a = document.activeElement;
    if (a && a !== document.body && !C.contains(a)) return;
    if (document.querySelector('.modal-backdrop.modal-open, dialog[open]')) return;
    var d = { ArrowRight: 1, ArrowLeft: -1, PageDown: 1, PageUp: -1 }[e.key];
    if (d) { e.preventDefault(); step(d); return; }
    if (e.key === 'Home') { e.preventDefault(); flip(0); }
    if (e.key === 'End' && total) { e.preventDefault(); flip(Math.floor((total - 1) / fp.fit) * fp.fit); }
  });

  // On, off, and every new set of rows.
  function size() {
    var n = fits();
    if (n === fp.fit) return false;
    fp.fit = n; fp.start = Math.floor(fp.start / n) * n; cache = {};
    return true;
  }
  function sync() {
    var want = usable();
    root.classList.toggle('fl-tl', want);
    if (want && !fp.on) {
      fp.on = true; readFoot();
      // The first rows tell the height of a row; ask again for exactly what fits.
      size(); htmx.ajax('GET', fleetURL(getFilterVals()), { target: C, swap: 'innerHTML' });
    } else if (!want && fp.on) { fp.on = false; cache = {}; }
  }
  document.addEventListener('htmx:afterSwap', function (e) {
    if (!e.detail || e.detail.target !== C) return;
    readFoot();
    if (!fp.on) { sync(); return; }
    var f = C.querySelector('.fl-foot');
    if (f && f.dataset.start) fp.start = Math.max(0, +f.dataset.start - 1);
    fill(); foot(); cue(); prefetch();
  });
  document.body.addEventListener('fleet:scoped', function () { cache = {}; });
  window.addEventListener('resize', function () {
    clearTimeout(window.__flFpR);
    window.__flFpR = setTimeout(function () {
      sync();
      if (fp.on && size()) htmx.ajax('GET', fleetURL(getFilterVals()), { target: C, swap: 'innerHTML' });
    }, 180);
  });
  if (document.readyState === 'complete') setTimeout(sync, 60); else window.addEventListener('load', function () { setTimeout(sync, 60); });
  document.addEventListener('htmx:afterSettle', function () { if (!fp.on) sync(); });
  }
})();
