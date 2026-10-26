// Loaded by templates/apps.html (A1, 21 Oct 2026). The search box and the Show / Channels
// pills filter the app rows in the page; a row opens its app page (versions, channels and
// the family tools live there); a one-package row's Install on… opens its chooser under the row.
(function () {
  if (window.__a1Apps) return;
  window.__a1Apps = true;
  var F = { show: '', chan: '', q: '' };
  function apply() {
    var rows = document.querySelectorAll('.a1-r'), shown = 0;
    // Role apps only, unless "Show all apps" is ticked; a search looks through every app.
    var allBox = document.getElementById('a1-all'), all = !allBox || allBox.checked || !!F.q;
    rows.forEach(function (r) {
      var ok = (all || r.dataset.role === '1') &&
        (!F.q || (r.dataset.search || '').indexOf(F.q) !== -1) &&
        (!F.show || r.dataset[F.show] === '1') &&
        (!F.chan || r.dataset.chan === F.chan);
      r.hidden = !ok;
      var slot = r.nextElementSibling;
      if (slot && slot.classList.contains('a1-slot') && !ok) slot.hidden = true;
      if (ok) shown++;
    });
    var none = document.querySelector('.a1-none');
    if (none && rows.length) none.hidden = shown > 0;
    ['show', 'chan'].forEach(function (k) {
      var pill = document.querySelector('[data-a1-pill="' + k + '"]');
      if (!pill) return;
      var label = '';
      document.querySelectorAll('[data-a1-set="' + k + '"]').forEach(function (i) {
        var on = i.dataset.val === F[k];
        i.classList.toggle('active', on);
        if (on) label = i.textContent;
      });
      pill.querySelector('b').textContent = label;
      pill.classList.toggle('set', !!F[k]);
    });
  }
  document.addEventListener('click', function (e) {
    var i = e.target.closest('[data-a1-set]');
    if (i) {
      F[i.dataset.a1Set] = i.dataset.val;
      if (window.ocCloseMenus) window.ocCloseMenus();
      apply();
      return;
    }
    var r = e.target.closest('.a1-r[data-href]');
    if (r && !e.target.closest('a, button, form, input, select')) {
      var a = r.querySelector('.a1-nm a');
      if (a) a.click(); else location.href = r.dataset.href;
    }
  });
  document.addEventListener('change', function (e) {
    if (e.target.id !== 'a1-all') return;
    var u = new URL(location.href);
    if (e.target.checked) u.searchParams.set('all', '1'); else u.searchParams.delete('all');
    history.replaceState(history.state, '', u);
    apply();
  });
  document.addEventListener('input', function (e) {
    if (e.target.id !== 'a1-q') return;
    F.q = (e.target.value || '').trim().toLowerCase();
    apply();
  });
  window.a1Slot = function (i) {
    var s = document.getElementById('a1-slot-' + i);
    if (s) s.hidden = false;
  };
})();
