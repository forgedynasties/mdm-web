// Loaded by templates/restaurants.html.
(function () {
  var tbl = document.getElementById('vn-tbl'); if (!tbl) return;
  var q = document.getElementById('vn-q'), filter = 'all';
  function rows() { return Array.prototype.slice.call(tbl.querySelectorAll('.vn-tr')); }
  function apply() {
    var t = (q.value || '').trim().toLowerCase(), shown = 0;
    rows().forEach(function (r) {
      var ok = (!t || r.dataset.text.toLowerCase().indexOf(t) >= 0) && (filter === 'all' || r.dataset[filter] === '1');
      r.hidden = !ok; if (ok) shown++;
    });
    document.getElementById('vn-none').hidden = shown > 0;
  }
  q.addEventListener('input', apply);
  document.querySelectorAll('[data-vf]').forEach(function (b) {
    b.addEventListener('click', function () {
      filter = b.dataset.vf;
      document.querySelectorAll('[data-vf]').forEach(function (x) { x.classList.toggle('on', x === b); });
      apply();
    });
  });
  // Sorting is in the page: the list is every venue already, so a click needs no round trip.
  document.querySelectorAll('[data-vs]').forEach(function (b) {
    b.addEventListener('click', function () {
      var key = b.dataset.vs, asc = b.getAttribute('aria-sort') !== 'ascending';
      if (key !== 'name' && !b.hasAttribute('aria-sort')) asc = key === 'score';
      document.querySelectorAll('[data-vs]').forEach(function (x) { x.removeAttribute('aria-sort'); });
      b.setAttribute('aria-sort', asc ? 'ascending' : 'descending');
      var none = document.getElementById('vn-none');
      rows().sort(function (x, y) {
        var a = x.dataset[key], c = y.dataset[key];
        var d = key === 'name' ? a.localeCompare(c, undefined, { sensitivity: "base" }) : (+a) - (+c);
        return asc ? d : -d;
      }).forEach(function (r) { tbl.insertBefore(r, none); });
    });
  });
})();
