// Loaded by templates/restaurant_detail.html.
// Popover and ⋯ menu: one open at a time, closed by Escape or a click outside.
if (!window.__vnPops) {
  window.__vnPops = true;
  document.addEventListener('click', function (e) {
    // The picker re-renders a row as it is clicked, so a detached target was inside.
    if (!e.target.isConnected) return;
    document.querySelectorAll('details.vn-add[open], details.vn-more-m[open]').forEach(function (d) { if (!d.contains(e.target)) d.removeAttribute('open'); });
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') document.querySelectorAll('details.vn-add[open], details.vn-more-m[open]').forEach(function (d) { d.removeAttribute('open'); });
  });
  document.addEventListener('toggle', function (e) {
    var d = e.target; if (!d.classList || !d.classList.contains('vn-add') || !d.open) return;
    var q = d.querySelector('#pk-q'); if (q) setTimeout(function () { q.focus(); }, 30);
  }, true);
}

// D1 (22 Oct 2026): the type tabs over the device table filter its rows in place. The
// table refreshes on restaurant-updated, so the chosen tab is applied again after a swap.
if (!window.__vp1Tabs) {
  window.__vp1Tabs = true;
  var vp1Apply = function () {
    var on = document.querySelector('.vp1-tab.on');
    var cls = on ? on.dataset.class : '';
    document.querySelectorAll('.vp1-dev .ft-row, .vp1-dev .dl-card').forEach(function (r) {
      r.classList.toggle('vp1-hide', cls !== '' && r.dataset.class !== cls);
    });
  };
  document.addEventListener('click', function (e) {
    var t = e.target.closest && e.target.closest('.vp1-tab');
    if (!t) return;
    document.querySelectorAll('.vp1-tab').forEach(function (b) {
      var me = b === t;
      b.classList.toggle('on', me);
      b.setAttribute('aria-selected', me ? 'true' : 'false');
    });
    vp1Apply();
  });
  document.addEventListener('htmx:afterSwap', function (e) {
    if (e.target && e.target.id === 'restaurant-members') vp1Apply();
  });
}
