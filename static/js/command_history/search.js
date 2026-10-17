// Loaded by templates/command_history.html.
// Client-side search of the current page only. Status, type and pagination are
// all server-side (tabs + type dropdown + page links carry each other).
function filterHistory() {
  var q = (document.getElementById('cmd-search').value || '').toLowerCase();
  document.querySelectorAll('#cmd-history-table .htrow[data-cmd-type]').forEach(function (r) {
    r.style.display = (!q || (r.dataset.cmdSearch || '').toLowerCase().indexOf(q) !== -1) ? '' : 'none';
  });
  document.querySelectorAll('#cmd-history-table .hist-day').forEach(function (h) {
    var vis = false, n = h.nextElementSibling;
    while (n && (n.classList.contains('htrow') || n.classList.contains('hist-day'))) {
      if (n.classList.contains('htrow') && n.style.display !== 'none') { vis = true; break; }
      if (n.classList.contains('hist-day')) break;
      n = n.nextElementSibling;
    }
    h.style.display = vis ? '' : 'none';
  });
}
// Toast on resend (mirrors the Actions page).
document.addEventListener('submit', function (evt) {
  var form = evt.target;
  if (form && form.tagName === 'FORM' && form.action.indexOf('/resend') !== -1) {
    setTimeout(function () { if (window.showToast) showToast('Action resent', 'success'); }, 100);
  }
});
// Live history: nudge #hist-live to re-fetch + morph on any command-update from the
// global feed. mdmStream auto-closes on navigation.
(function () {
  if (!window.EventSource || !window.mdmStream) return;
  var es = window.mdmStream('/commands/events');
  es.addEventListener('command-update', function () {
    document.body.dispatchEvent(new CustomEvent('commands-feed-update'));
  });
})();
// After each live morph, re-apply the client-side search filter to the new rows.
document.body.addEventListener('htmx:afterSwap', function (e) {
  if (e.target && e.target.id === 'hist-live') filterHistory();
});
