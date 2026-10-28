// Phones (phone audit, 28 Oct 2026): a table wider than its container used to be cut off
// by the card around it. Below 720 px such a table is wrapped in a box that scrolls
// sideways; narrower tables and desktop are left alone. Runs again after htmx swaps.
(function () {
  var mq = window.matchMedia('(max-width: 720px)');
  function wrap(root) {
    if (!mq.matches) return;
    (root || document).querySelectorAll('main table, .container table').forEach(function (t) {
      var p = t.parentElement;
      if (!p || p.classList.contains('ph-tscroll')) return;
      var wide = t.offsetWidth > p.clientWidth + 2 || t.scrollWidth > p.clientWidth + 2;
      // Columns squeezed until their text runs into the next one: give the table room.
      var squeezed = [].some.call(t.querySelectorAll('th, td'), function (c) { return c.scrollWidth > c.clientWidth + 2 || (c.clientWidth < 36 && c.textContent.trim().length > 2); });
      if (!wide && !squeezed) return;
      if (squeezed) {
        t.classList.add('ph-wide');
        // Fixed-layout tables: room for every column as set, plus 160 px for each one
        // that was squeezed to nothing (usually the name column).
        var row = t.querySelector('tr'), need = 0;
        if (row) [].forEach.call(row.children, function (c) { need += c.clientWidth < 36 ? 160 : c.offsetWidth; });
        if (need > 600) t.style.minWidth = need + 'px';
      }
      var box = document.createElement('div');
      box.className = 'ph-tscroll';
      p.insertBefore(box, t);
      box.appendChild(t);
    });
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', function () { wrap(); });
  else wrap();
  document.addEventListener('htmx:afterSettle', function (e) { wrap(e.target); });
  window.addEventListener('load', function () { wrap(); });
})();
