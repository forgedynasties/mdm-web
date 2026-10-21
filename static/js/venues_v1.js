// Loaded by templates/restaurants.html (Venues V1, 21 Oct 2026): search and the Health /
// Open / Type pills filter the cards in place; every venue is already on the page.
(function () {
  var grid = document.getElementById('vc1-grid');
  if (!grid) return;
  var q = document.getElementById('vc1-q');
  var f = { health: '', open: '', type: '' };
  function apply() {
    var t = (q.value || '').trim().toLowerCase(), shown = 0;
    grid.querySelectorAll('.ov2-v').forEach(function (c) {
      var d = c.dataset;
      var ok = (!t || (d.text || '').toLowerCase().indexOf(t) >= 0) &&
        (!f.health || d.health === f.health) &&
        (!f.open || d.open === f.open) &&
        (!f.type || (' ' + d.types + ' ').indexOf(' ' + f.type + ' ') >= 0);
      c.hidden = !ok;
      if (ok) shown++;
    });
    document.getElementById('vc1-none').hidden = shown > 0;
    var n = document.getElementById('vc1-n');
    if (n) n.textContent = shown + ' venue' + (shown === 1 ? '' : 's');
  }
  q.addEventListener('input', apply);
  document.querySelectorAll('[data-vc1]').forEach(function (b) {
    b.addEventListener('click', function () {
      var key = b.dataset.vc1;
      f[key] = b.dataset.val;
      document.querySelectorAll('[data-vc1="' + key + '"]').forEach(function (x) { x.classList.toggle('active', x === b); });
      var pill = document.querySelector('[data-vc1-pill="' + key + '"]');
      if (pill) {
        pill.querySelector('b').textContent = b.dataset.label;
        pill.classList.toggle('set', !!b.dataset.val);
      }
      if (window.ocCloseMenus) window.ocCloseMenus();
      apply();
    });
  });
})();
