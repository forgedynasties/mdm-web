// Shared by the Overview and the Venues list (moved out of overview_new.js, 21 Oct 2026).
// Filter pills and the cards' ··· menus: one open at a time, closed by a click outside
// or Escape. Bound once, since boosted navigation re-runs this file. A page that acts on
// a menu item in place (no navigation) calls window.ocCloseMenus() after.
if (!window.__ocMenus) {
  window.__ocMenus = true;
  var closeAll = function (except) {
    document.querySelectorAll('[data-oc-menu][aria-expanded="true"]').forEach(function (b) {
      if (b === except) return;
      b.setAttribute('aria-expanded', 'false');
      var m = b.nextElementSibling;
      if (m) m.hidden = true;
    });
  };
  window.ocCloseMenus = function () { closeAll(null); };
  document.addEventListener('click', function (e) {
    var b = e.target.closest('[data-oc-menu]');
    if (b) {
      e.preventDefault();
      var m = b.nextElementSibling, open = b.getAttribute('aria-expanded') === 'true';
      closeAll(b);
      b.setAttribute('aria-expanded', open ? 'false' : 'true');
      if (m) m.hidden = open;
      return;
    }
    if (!e.target.closest('.oc-f .cc-menu, .oc-cm .cc-menu')) closeAll(null);
  });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') closeAll(null); });
}
