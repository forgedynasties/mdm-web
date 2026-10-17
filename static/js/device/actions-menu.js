// Loaded by templates/device.html.
// Hero "Actions ▾" dropdown: toggle, close on outside-click / Escape.
(function () {
  var menu = document.getElementById('ddc-actions-menu');
  var btn = document.getElementById('ddc-actions-btn');
  if (!menu || !btn) return;
  window.ddActionsToggle = function (e) {
    if (e) { e.preventDefault(); e.stopPropagation(); }
    var open = menu.hasAttribute('hidden');
    if (open) menu.removeAttribute('hidden'); else menu.setAttribute('hidden', '');
    btn.setAttribute('aria-expanded', open ? 'true' : 'false');
    btn.classList.toggle('open', open);
  };
  document.addEventListener('click', function (e) {
    var wrap = document.getElementById('ddc-actions-wrap');
    if (wrap && !wrap.contains(e.target) && !menu.hasAttribute('hidden')) {
      menu.setAttribute('hidden', ''); btn.setAttribute('aria-expanded', 'false'); btn.classList.remove('open');
    }
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && !menu.hasAttribute('hidden')) {
      menu.setAttribute('hidden', ''); btn.setAttribute('aria-expanded', 'false'); btn.classList.remove('open');
    }
  });
})();
