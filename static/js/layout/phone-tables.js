// Phones (site-wide audit, 7 Nov 2026; user: "the next table is not responsive", "make every
// page viewable on phone easily"). Below 720 px a table with four or more columns, or any
// table that doesn't fit, turns into a list of cards: the first cell is the row's title,
// every other cell is a small label (its column's header) over its value, a checkbox and an
// icon-only action sit in the corners, and an action with words gets its own line. Nothing
// scrolls sideways any more. This file only adds classes and a data attribute; the look is
// in phone.css, inside its 720 px media query, so desktop is untouched.
// Runs again when anything in the page changes (htmx swaps, JS-built rows, live updates).
(function () {
  var mq = window.matchMedia('(max-width: 720px)');

  function labelOf(th) {
    var c = th.cloneNode(true);
    c.querySelectorAll('.sr-only, .ft-dir, input, svg').forEach(function (x) { x.remove(); });
    return c.textContent.replace(/\s+/g, ' ').trim();
  }
  function headers(t) {
    var out = [], tr = t.tHead && t.tHead.rows[t.tHead.rows.length - 1];
    if (tr) [].forEach.call(tr.cells, function (th) {
      var l = labelOf(th);
      for (var i = 0; i < th.colSpan; i++) out.push(l);
    });
    return out;
  }
  function dataRow(t) {
    return [].find.call(t.querySelectorAll(':scope > tbody > tr, :scope > tr'), function (r) { return r.cells.length > 1; });
  }
  function needsCards(t) {
    if (t.classList.contains('ph-cards')) return true;
    var row = dataRow(t), heads = headers(t).length, n = Math.max(row ? row.cells.length : 0, heads);
    if (n >= 4) return true;
    if (!t.offsetWidth) return false; // hidden for now; looked at again when it shows
    var p = t.parentElement;
    return t.offsetWidth > p.clientWidth + 2 || t.scrollWidth > p.clientWidth + 2 ||
      [].some.call(t.querySelectorAll('th, td'), function (c) { return c.clientWidth > 0 && c.clientWidth < 36 && c.textContent.trim().length > 2; });
  }
  var CONTROL = 'a, button, input, select, textarea, details, img, svg, canvas, progress, meter';
  function classify(t) {
    var heads = headers(t);
    t.classList.add('ph-cards');
    if (t.tHead) [].forEach.call(t.tHead.rows, function (r) {
      [].forEach.call(r.cells, function (th) { if (th.querySelector('input[type=checkbox]')) { th.classList.add('ph-th-cb'); r.classList.add('ph-sel-all'); } });
    });
    t.querySelectorAll(':scope > tbody > tr, :scope > tfoot > tr, :scope > tr').forEach(function (tr) {
      tr.classList.add('ph-row');
      var col = 0, main = false, hasCb = false, hasCorner = false, cells = tr.cells;
      [].forEach.call(cells, function (td) {
        var i = col; col += td.colSpan;
        var text = td.textContent.replace(/\s+/g, ' ').trim();
        var label = td.colSpan > 1 ? '' : (heads[i] || '');
        var cb = td.querySelector('input[type=checkbox]');
        var isCb = !!cb && !text && !td.querySelector('a, button, select');
        var hasCtl = !!td.querySelector(CONTROL);
        var blank = !isCb && !hasCtl && (!text || /^(—|-|–|nobody)$/i.test(text));
        // Actions: no header of its own and something to press.
        var act = !isCb && !label && !blank && main && !!td.querySelector('a, button, summary, form, details');
        var glyph = /^[↗→›⋯…]$/.test(text); // the row's "open" arrow
        var corner = (act && text.length <= 2) || (main && glyph);
        var full = cells.length === 1 || td.colSpan > 1;
        td.classList.toggle('ph-cb', isCb);
        td.classList.toggle('ph-blank', blank);
        td.classList.toggle('ph-corner', corner);
        td.classList.toggle('ph-acts', act && !corner);
        // A picker, a row of buttons or one long word (an email, a package) needs the width.
        var wide = !!td.querySelector('select') || td.querySelectorAll('button, label').length > 1 || text.split(' ').some(function (w) { return w.length > 22; });
        td.classList.toggle('ph-span', wide);
        td.classList.toggle('ph-full', full);
        var isMain = !main && !isCb && !blank && !full;
        td.classList.toggle('ph-main', isMain);
        if (isMain) main = true;
        if (full) main = true;
        if (label && !isMain && !isCb && !act && !corner) td.setAttribute('data-ph-l', label);
        else td.removeAttribute('data-ph-l');
        hasCb = hasCb || isCb;
        hasCorner = hasCorner || corner;
      });
      tr.classList.toggle('ph-has-cb', hasCb);
      tr.classList.toggle('ph-has-corner', hasCorner);
    });
  }
  function run() {
    if (!mq.matches) return;
    document.querySelectorAll('main table, .container table').forEach(function (t) {
      if (t.closest('.ph-keep') || t.parentElement.closest('td')) return;
      if (needsCards(t)) classify(t);
    });
  }
  var timer = 0;
  function soon() { if (!timer) timer = setTimeout(function () { timer = 0; run(); }, 120); }
  function boot() {
    run();
    new MutationObserver(soon).observe(document.body, { childList: true, subtree: true });
    document.addEventListener('click', function () { setTimeout(soon, 60); }, true);
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
  document.addEventListener('htmx:afterSettle', soon);
  window.addEventListener('load', soon);
  mq.addEventListener && mq.addEventListener('change', soon);
})();
