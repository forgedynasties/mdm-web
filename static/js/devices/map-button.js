// Loaded by templates/devices.html.
// The top bar's Map button mirrors the page's own (hidden) one.
function flMapSync() {
  var b = document.getElementById('fl-map-btn'), on = !!b && b.getAttribute('aria-pressed') === 'true';
  document.querySelectorAll('#appbar-island [data-fl-map]').forEach(function (x) { x.textContent = on ? 'Back to list' : 'Map view'; x.setAttribute('aria-pressed', String(on)); });
}
document.addEventListener('htmx:oobAfterSwap', function (e) {
  var t = e.detail && e.detail.target;
  if (t && t.id === 'fl-headline' && window.mdmIslandRefresh) { window.mdmIslandRefresh(); flMapSync(); }
});
window.addEventListener('load', flMapSync);
