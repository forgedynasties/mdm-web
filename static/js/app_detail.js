// App page B (19 Nov 2026). Picking a version row filters the Devices list to the devices
// running it (click again, the chip's x, or the Version pill clears it); the Version and
// Venue pills and the search box filter the same rows in the page.
(function () {
  if (window.__ap1) return;
  window.__ap1 = true;
  var F = { ver: '', venue: '', q: '' };
  function $(s) { return document.querySelector(s); }
  function apply() {
    var rows = document.querySelectorAll('.d1-r'), shown = 0, q = F.q.toLowerCase();
    rows.forEach(function (r) {
      var ok = (!F.ver || r.dataset.ver === F.ver) && (!F.venue || r.dataset.venue === F.venue) &&
        (!q || (r.dataset.q || '').toLowerCase().indexOf(q) >= 0);
      r.hidden = !ok;
      if (ok) shown++;
    });
    var none = $('#d1-devices .d1-none');
    if (none && rows.length) none.hidden = shown > 0;
    var cnt = $('#ap1-count');
    if (cnt) cnt.textContent = (shown === rows.length) ? rows.length : shown + ' of ' + rows.length;
    var vf = $('#ap1-vf');
    if (vf) {
      vf.hidden = !F.ver;
      vf.querySelector('b').textContent = F.ver === 'none' ? 'no version' : F.ver;
    }
    document.querySelectorAll('.ap1-vr').forEach(function (r) { r.classList.toggle('on', !!F.ver && r.dataset.v === F.ver); });
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
  function pickVersion(v) { F.ver = (F.ver === v) ? '' : v; apply(); }
  document.addEventListener('click', function (e) {
    var i = e.target.closest('[data-d1-set]');
    if (i) {
      F[i.dataset.d1Set] = i.dataset.val;
      if (window.ocCloseMenus) window.ocCloseMenus();
      apply();
      return;
    }
    if (e.target.closest('#ap1-vf-x')) { F.ver = ''; apply(); return; }
    var r = e.target.closest('.ap1-vr');
    if (r && !e.target.closest('a,button,form')) pickVersion(r.dataset.v);
  });
  document.addEventListener('keydown', function (e) {
    if ((e.key === 'Enter' || e.key === ' ') && e.target.classList && e.target.classList.contains('ap1-vr')) {
      e.preventDefault(); pickVersion(e.target.dataset.v);
    }
  });
  document.addEventListener('input', function (e) {
    if (e.target.id === 'ap1-q') { F.q = e.target.value; apply(); }
  });
})();
