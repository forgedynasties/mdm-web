// The venue page V1 (picked 4 Nov 2026): Fleet's rail and one pane. Since 25 Nov 2026 (B,
// user: "the left rail is so crowded") the rail is Overview and Settings only; Settings
// shows a grid of cards, one per setting, each with its row's value and an amber dot when
// the row says something needs a look. A card shows the Venue settings card with that row
// open (the rows are <details name="vn-set">, one open at a time), with "All settings" to go
// back to the grid. ?open=<row> and #wifi (what the settings' saves send back to) land on
// that setting, as does the island's Edit (vnOpenCard).
(function () {
  var root = document.getElementById('vn1');
  if (!root) return;
  var items = Array.prototype.slice.call(root.querySelectorAll('[data-vn-pane]'));
  var panes = Array.prototype.slice.call(root.querySelectorAll('[data-vn-for]'));
  var cards = Array.prototype.slice.call(root.querySelectorAll('.vn1-sc[data-vn-row]'));
  var grid = document.getElementById('vn1-sgrid'), one = document.getElementById('vn1-sone');
  function rowOf(k) { return document.getElementById(k === 'wifi' ? 'wifi' : 'vn-' + k); }
  function keyOf(d) { return d.id === 'wifi' ? 'wifi' : d.id.replace(/^vn-/, ''); }
  function mark(pane) {
    items.forEach(function (b) {
      var on = b.dataset.vnPane === pane;
      b.classList.toggle('on', on); b.setAttribute('aria-pressed', on);
    });
  }
  // pane: overview | settings. row: a setting to open, or none for the grid.
  function show(pane, row, scroll) {
    panes.forEach(function (p) { p.hidden = p.dataset.vnFor !== pane; });
    if (pane === 'settings') {
      var d = row ? rowOf(row) : null;
      if (grid) grid.hidden = !!d;
      if (one) one.hidden = !d;
      if (d) {
        d.open = true;
        if (scroll !== false) d.scrollIntoView({ block: 'start', behavior: 'smooth' });
      }
    }
    mark(pane);
  }
  items.forEach(function (b) {
    b.addEventListener('click', function () { show(b.dataset.vnPane); });
  });
  cards.forEach(function (c) {
    c.addEventListener('click', function () { show('settings', c.dataset.vnRow); });
  });
  var back = document.getElementById('vn1-back');
  if (back) back.addEventListener('click', function () { show('settings'); if (grid) grid.scrollIntoView({ block: 'start' }); });
  // Each card: its row's value, and amber when the row says paused, not linked or carries a
  // warning tag. A card whose row is not on the page (no rights to it) is not shown.
  var shown = 0, warns = 0;
  cards.forEach(function (c) {
    var d = rowOf(c.dataset.vnRow);
    if (!d) { c.hidden = true; return; }
    shown++;
    var v = d.querySelector('summary .vs-v'), h = c.querySelector('.v');
    if (v && h) { h.textContent = v.textContent.replace(/\s+/g, ' ').trim(); c.title = h.textContent; }
    var warn = d.querySelector('summary .vu-paused, summary .cc-tag.warn, summary .cc-tag.bad') ||
      /Not linked|Off ·/.test(v ? v.textContent : '');
    if (warn) { c.classList.add('warn'); warns++; }
  });
  // A group with no cards left (all hidden) goes too.
  Array.prototype.forEach.call(root.querySelectorAll('.vn1-sgrp'), function (g) {
    g.hidden = !g.querySelector('.vn1-sc:not([hidden])');
  });
  var item = document.getElementById('vn1-set-item'), ct = document.getElementById('vn1-set-ct');
  if (ct) ct.textContent = warns ? warns + ' to check' : (shown ? String(shown) : '');
  if (item && warns) { var dot = item.querySelector('.dot'); if (dot) { dot.classList.remove('ok'); dot.classList.add('warn'); } }
  // Opening a row in the card itself keeps the rail on Settings.
  root.addEventListener('toggle', function (e) {
    var d = e.target;
    if (d && d.classList && d.classList.contains('vs-row') && d.open) mark('settings');
  }, true);
  // The island's Edit (and anything else) calls vnOpenCard: switch to that setting first.
  var open = window.vnOpenCard;
  window.vnOpenCard = function (id) {
    show('settings', id === 'wifi' ? 'wifi' : String(id).replace(/^vn-/, ''), false);
    if (open) open(id);
  };
  // Devices by type: a row opens Fleet on this venue and type.
  root.addEventListener('click', function (e) {
    var tr = e.target.closest && e.target.closest('tr[data-href]');
    if (tr && !e.target.closest('a,button')) location.href = tr.dataset.href;
  });
  // Landing on a setting: a save sent us back with it open (?open= / #wifi, handled by
  // venue_location.js, which opens the row).
  var already = root.querySelector('details.vs-row[open]');
  if (already) show('settings', keyOf(already), true);
})();
