// Loaded by templates/app_detail.html (D1, 21 Oct 2026). The Version and Venue pills filter
// the device rows in the page; a banner link with data-d1-ver ("See them") sets the
// Version filter before jumping to the table.
(function () {
  if (window.__d1App) return;
  window.__d1App = true;
  var F = { ver: '', venue: '' };
  function apply() {
    var rows = document.querySelectorAll('.d1-r'), shown = 0;
    rows.forEach(function (r) {
      var ok = (!F.ver || r.dataset.ver === F.ver) && (!F.venue || r.dataset.venue === F.venue);
      r.hidden = !ok;
      if (ok) shown++;
    });
    var none = document.querySelector('#d1-devices .d1-none');
    if (none && rows.length) none.hidden = shown > 0;
    ['ver', 'venue'].forEach(function (k) {
      var pill = document.querySelector('[data-d1-pill="' + k + '"]');
      if (!pill) return;
      var label = 'All';
      document.querySelectorAll('[data-d1-set="' + k + '"]').forEach(function (i) {
        var on = i.dataset.val === F[k];
        i.classList.toggle('active', on);
        if (on) label = i.textContent;
      });
      pill.querySelector('b').textContent = label;
      pill.classList.toggle('set', !!F[k]);
    });
  }
  document.addEventListener('click', function (e) {
    var i = e.target.closest('[data-d1-set]');
    if (i) {
      F[i.dataset.d1Set] = i.dataset.val;
      if (window.ocCloseMenus) window.ocCloseMenus();
      apply();
      return;
    }
    var a = e.target.closest('a[data-d1-ver]');
    if (a) { F.ver = a.dataset.d1Ver; F.venue = ''; apply(); }
  });
})();
