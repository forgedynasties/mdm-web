// The venue page V1 (picked 4 Nov 2026): Fleet's rail with Overview and every setting, one
// pane. A setting shows the Venue settings card with that row open (the rows are
// <details name="vn-set">, one open at a time); each rail item carries its row's value as a
// hint and turns amber when the row says something needs a look. ?open=<row> and #wifi
// (what the settings' saves send back to) land on that setting.
(function () {
  var root = document.getElementById('vn1');
  if (!root) return;
  var items = Array.prototype.slice.call(root.querySelectorAll('[data-vn-pane]'));
  var panes = Array.prototype.slice.call(root.querySelectorAll('[data-vn-for]'));
  function rowOf(k) { return document.getElementById(k === 'wifi' ? 'wifi' : 'vn-' + k); }
  function keyOf(d) { return d.id === 'wifi' ? 'wifi' : d.id.replace(/^vn-/, ''); }
  function mark(pane, row) {
    items.forEach(function (b) {
      var on = b.dataset.vnPane === pane && (pane !== 'settings' || b.dataset.vnRow === row);
      b.classList.toggle('on', on); b.setAttribute('aria-pressed', on);
    });
  }
  function show(pane, row, scroll) {
    panes.forEach(function (p) { p.hidden = p.dataset.vnFor !== pane; });
    if (pane === 'settings' && row) {
      var d = rowOf(row);
      if (d) {
        d.open = true;
        if (scroll !== false) d.scrollIntoView({ block: 'start', behavior: 'smooth' });
      }
    }
    mark(pane, row);
  }
  items.forEach(function (b) {
    b.addEventListener('click', function () { show(b.dataset.vnPane, b.dataset.vnRow); });
  });
  // Hints: each setting's value, and amber when it says paused, not linked or carries a tag.
  items.forEach(function (b) {
    if (!b.dataset.vnRow) return;
    var d = rowOf(b.dataset.vnRow); if (!d) { b.hidden = true; return; }
    var v = d.querySelector('summary .vs-v'), h = b.querySelector('.vn1-hint');
    if (v && h) h.textContent = v.textContent.replace(/\s+/g, ' ').trim();
    var warn = d.querySelector('summary .vu-paused, summary .cc-tag.warn, summary .cc-tag.bad') ||
      /Not linked|Off ·/.test(v ? v.textContent : '');
    var dot = b.querySelector('.dot'); if (dot && warn) { dot.classList.remove('ok'); dot.classList.add('warn'); }
  });
  // Opening a row in the card itself moves the rail with it.
  root.addEventListener('toggle', function (e) {
    var d = e.target;
    if (d && d.classList && d.classList.contains('vs-row') && d.open) mark('settings', keyOf(d));
  }, true);
  // The island's Edit (and anything else) calls vnOpenCard: switch to the settings first.
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
