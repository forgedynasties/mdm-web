// Loaded by templates/release_crashes.html.
// The full crashes page has no inline report form; "Report" jumps to the release's
// Problems tab with the crash seeded (consumed by probFromCrash's on-load hook there).
function probFromCrash(btn) {
  var p = new URLSearchParams({
    rc_kind: btn.dataset.kind || '', rc_sum: btn.dataset.sum || '', rc_serial: btn.dataset.serial || ''
  });
  location.href = '/releases/' + window.RELEASE_CRASHES_PAGE.release + '?tab=problems&' + p.toString();
}

// CR1: filter the group list on this page by signature or kind.
(function () {
  var q = document.getElementById('cr1-q'), box = document.getElementById('cr1-items');
  if (!q || !box) return;
  q.addEventListener('input', function () {
    var t = q.value.trim().toLowerCase();
    box.querySelectorAll('.ovb-li').forEach(function (a) {
      a.hidden = !!t && (a.dataset.s || '').toLowerCase().indexOf(t) < 0;
    });
  });
})();
