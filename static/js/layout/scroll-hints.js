/* Scroll hints, site-wide (23 Nov 2026: "in any pane which is scrollable there is not a good hint").
   A pane that really overflows gets:
     - a soft fade at the bottom (and at the top once scrolled) while there is more in that direction;
     - a thin, always-visible scrollbar on desktop (class .sh, style.css);
     - on lists and tables, a "↓ N more" chip that scrolls down and goes at the end.
   Panes are found by looking, not by class, so new pages get it by themselves; a short pane shows
   nothing. The fades and the chip are fixed overlays (never children of the pane), so they cannot
   clip menus or change a layout. Opt out with data-no-hint on a pane. */
(function () {
  'use strict';
  if (window.__mdmScrollHints) return; window.__mdmScrollHints = true;
  var ROWS = 'tbody > tr, .app-row, .ddc-tl, .ovb-li, .lr, .pl-vi, .fleet-rail-item, .alr, li';
  var tracked = new Map(); // pane -> { top, bot, chip }
  var queued = false;

  function visible(el) { var r = el.getBoundingClientRect(); return r.width > 40 && r.height > 60 && r.bottom > 0 && r.top < innerHeight; }
  function isScroller(el) {
    if (el === document.body || el === document.documentElement || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.tagName === 'HTML') return false;
    if (el.hasAttribute('data-no-hint') || el.closest('[data-no-hint]')) return false;
    if (el.scrollHeight <= el.clientHeight + 6) return false;
    var oy = getComputedStyle(el).overflowY;
    return oy === 'auto' || oy === 'scroll';
  }
  function bgOf(el) {
    for (var n = el; n && n.nodeType === 1; n = n.parentElement) {
      var c = getComputedStyle(n).backgroundColor, m = c.match(/rgba?\(([^)]+)\)/);
      if (m) { var p = m[1].split(',').map(parseFloat); if (p.length < 4 || p[3] > 0.6) return 'rgb(' + p[0] + ',' + p[1] + ',' + p[2] + ')'; }
    }
    return getComputedStyle(document.body).backgroundColor;
  }
  function zOf(el) {
    for (var n = el; n && n.nodeType === 1; n = n.parentElement) {
      var cs = getComputedStyle(n), z = parseInt(cs.zIndex, 10);
      if (cs.position !== 'static' && !isNaN(z)) return z + 1;
    }
    return 20;
  }
  function mk(cls) { var d = document.createElement('div'); d.className = cls; d.setAttribute('aria-hidden', 'true'); document.body.appendChild(d); return d; }

  function track(p) {
    if (tracked.has(p)) return;
    p.classList.add('sh');
    var t = { top: mk('sh-fade sh-fade-t'), bot: mk('sh-fade sh-fade-b'), chip: null, rows: !!p.querySelector(ROWS) && p.querySelectorAll(ROWS).length > 4 };
    if (t.rows) {
      t.chip = mk('sh-chip'); t.chip.setAttribute('role', 'button'); t.chip.tabIndex = -1;
      t.chip.addEventListener('click', function () { p.scrollBy({ top: p.clientHeight * 0.8, behavior: 'smooth' }); });
    }
    tracked.set(p, t);
  }
  function untrack(p) {
    var t = tracked.get(p); if (!t) return;
    [t.top, t.bot, t.chip].forEach(function (x) { if (x) x.remove(); });
    p.classList.remove('sh'); tracked.delete(p);
  }
  function remaining(p) {
    var pr = p.getBoundingClientRect(), n = 0;
    p.querySelectorAll(ROWS).forEach(function (r) { var b = r.getBoundingClientRect(); if (b.height && b.top >= pr.bottom - 4) n++; });
    return n;
  }
  function place(p, t) {
    var r = p.getBoundingClientRect(), more = p.scrollHeight - p.clientHeight - p.scrollTop > 6, back = p.scrollTop > 6;
    var z = zOf(p), bg = bgOf(p), w = p.clientWidth;
    [[t.top, back, r.top, 'to bottom'], [t.bot, more, r.bottom - 36, 'to top']].forEach(function (a) {
      var el = a[0], on = a[1] && r.width > 0 && r.bottom > 0 && r.top < innerHeight;
      el.style.cssText = 'left:' + r.left + 'px;width:' + w + 'px;top:' + a[2] + 'px;z-index:' + z + ';background:linear-gradient(' + a[3] + ',' + bg + ',transparent);opacity:' + (on ? 1 : 0);
    });
    if (t.chip) {
      var n = remaining(p), on = more && n > 0;
      t.chip.textContent = '↓ ' + n + ' more';
      t.chip.style.cssText = 'left:' + (r.left + w / 2) + 'px;top:' + (r.bottom - 44) + 'px;z-index:' + (z + 1) + ';opacity:' + (on ? 1 : 0) + ';pointer-events:' + (on ? 'auto' : 'none');
    }
  }
  function refresh() {
    queued = false;
    tracked.forEach(function (t, p) {
      if (!p.isConnected || !visible(p) || !isScroller(p)) { untrack(p); return; }
      place(p, t);
    });
  }
  function schedule() { if (!queued) { queued = true; requestAnimationFrame(refresh); } }
  function scan() {
    var cands = document.querySelectorAll('main *, dialog *, .modal *, .pl-modal *, .bt-sheet-h ~ *, .fleet-rail, .cc-menu');
    for (var i = 0; i < cands.length; i++) {
      var el = cands[i];
      if (el.scrollHeight > el.clientHeight + 6 && !tracked.has(el) && isScroller(el) && visible(el)) track(el);
    }
    schedule();
  }
  var st = null;
  function scanSoon() { clearTimeout(st); st = setTimeout(function () { if (window.requestIdleCallback) requestIdleCallback(scan, { timeout: 500 }); else scan(); }, 120); }

  document.addEventListener('scroll', schedule, true);
  addEventListener('resize', scanSoon);
  document.addEventListener('htmx:afterSettle', scanSoon);
  document.addEventListener('click', function () { setTimeout(scanSoon, 200); }, true);
  new MutationObserver(scanSoon).observe(document.body, { childList: true, subtree: true });
  if (document.readyState !== 'loading') scanSoon(); else document.addEventListener('DOMContentLoaded', scanSoon);
})();
