// Loaded by templates/devices.html.
// The whole row opens the device (same as its arrow link, so the fleet inspector
// still catches it); controls inside the row and text selection keep their own
// behaviour. Registered once — the list is re-rendered by htmx swaps.
if (!window.__dlRowNav) {
  window.__dlRowNav = true;
  document.addEventListener('click', function (e) {
    var card = e.target.closest && e.target.closest('.ft-row, .dl-card');
    if (!card || e.target.closest('a, button, input, label, select, form, .ft-legacy, .dl-legacy-tag')) return;
    var sel = window.getSelection && window.getSelection();
    if (sel && String(sel).length) return;
    var link = card.querySelector('.dl-view');
    if (!link) return;
    if (e.metaKey || e.ctrlKey) { window.open(link.href, '_blank'); return; }
    link.click();
  });

  // Shift-click range select: tick one box, shift-click another, and every box in
  // between takes the clicked box's new state (like a mail client). The anchor is
  // the last box clicked without shift; it survives list re-renders by serial.
  var anchorSerial = null;
  document.addEventListener('click', function (e) {
    var cb = e.target.closest && e.target.closest('.ft-row .device-cb, .dl-card .device-cb');
    if (!cb) return;
    var boxes = Array.prototype.slice.call(document.querySelectorAll('.ft .device-cb, .dl-list .device-cb'));
    var from = anchorSerial ? boxes.findIndex(function (b) { return b.value === anchorSerial; }) : -1;
    if (e.shiftKey && from >= 0) {
      var to = boxes.indexOf(cb), lo = Math.min(from, to), hi = Math.max(from, to);
      for (var i = lo; i <= hi; i++) boxes[i].checked = cb.checked;
      if (window.getSelection) { try { window.getSelection().removeAllRanges(); } catch (_) {} }
      if (typeof updateBulkBar === 'function') updateBulkBar();
    }
    anchorSerial = cb.value;
  });
}
