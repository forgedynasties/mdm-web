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
