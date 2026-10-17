// Loaded by templates/release_crashes.html.
// The full crashes page has no inline report form; "Report" jumps to the release's
// Problems tab with the crash seeded (consumed by probFromCrash's on-load hook there).
function probFromCrash(btn) {
  var p = new URLSearchParams({
    rc_kind: btn.dataset.kind || '', rc_sum: btn.dataset.sum || '', rc_serial: btn.dataset.serial || ''
  });
  location.href = '/releases/' + window.RELEASE_CRASHES_PAGE.release + '?tab=problems&' + p.toString();
}
