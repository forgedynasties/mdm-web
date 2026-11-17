// A rail that swaps its pane (htmx: Setup, Models, …) answers the click at once: the picked
// row is marked, and the pane dims with a moving bar until the new view is in (17 Nov 2026,
// UI review: "when i select another item nothing happens … then the view is instantly updated").
(function () {
  function targetOf(elt) {
    var sel = elt.getAttribute('hx-target');
    if (!sel || sel === 'this') return null;
    try { return document.querySelector(sel); } catch (e) { return null; }
  }
  document.body.addEventListener('htmx:beforeRequest', function (e) {
    var elt = e.detail && e.detail.elt;
    if (!elt || !elt.closest || !elt.closest('.fleet-rail') || !elt.hasAttribute('hx-get')) return;
    var rail = elt.closest('.fleet-rail');
    if (elt.classList.contains('fleet-rail-item')) {
      rail.querySelectorAll('.fleet-rail-item.on').forEach(function (x) { if (x !== elt) { x.classList.remove('on'); x.removeAttribute('aria-current'); } });
      elt.classList.add('on'); elt.setAttribute('aria-current', 'page');
    }
    var t = targetOf(elt);
    if (t) { t.classList.add('pane-loading'); t.setAttribute('aria-busy', 'true'); }
  });
  function done(e) {
    var elt = e.detail && e.detail.elt; if (!elt || !elt.getAttribute) return;
    var t = targetOf(elt);
    if (t) { t.classList.remove('pane-loading'); t.removeAttribute('aria-busy'); }
    document.querySelectorAll('.pane-loading').forEach(function (x) { if (!x.isConnected || x === t) x.classList.remove('pane-loading'); });
  }
  document.body.addEventListener('htmx:afterRequest', done);
  document.body.addEventListener('htmx:sendError', done);
})();
