// Loaded by templates/group_detail.html.
// Bulk-remove from group. The device-cards checkboxes call updateBulkBar() (the
// name the shared "Selectable" card markup hard-codes); here it drives the group
// members bar. Markup lives inside #group-members so it survives the in-place
// htmx refresh; these handlers are re-defined each time this fragment loads
// (standalone page, or the fleet-page devices popup) and persist from there.
function grpUpdateBulkBar() {
  var boxes = document.querySelectorAll('#group-members .device-cb');
  var n = 0;
  boxes.forEach(function (b) { if (b.checked) n++; });
  var bar = document.getElementById('grp-bulk-bar');
  var cnt = document.getElementById('grp-bulk-count');
  var all = document.getElementById('grp-select-all');
  if (cnt) cnt.textContent = n + ' selected';
  if (bar) bar.style.display = n ? 'flex' : 'none';
  if (all) all.checked = boxes.length > 0 && n === boxes.length;
}
window.updateBulkBar = grpUpdateBulkBar;
function grpToggleAll(cb) {
  document.querySelectorAll('#group-members .device-cb').forEach(function (b) { b.checked = cb.checked; });
  grpUpdateBulkBar();
}
// These add/remove calls go out as a plain fetch, not through htmx, so htmx never
// sees the HX-Trigger header and its toast listener never fires. Read the header
// here and show the server's own message — without it an add that matched nothing
// (serials not in the fleet, or already members) looked exactly like a success.
function grpToastFrom(r) {
  var h = r.headers.get('HX-Trigger');
  if (!h) return;
  try {
    var d = JSON.parse(h).toast;
    if (d && window.showToast) showToast(d.msg || 'Done', d.type || 'info');
  } catch (e) {}
}
function grpRemoveSelected(gid) {
  var serials = [];
  document.querySelectorAll('#group-members .device-cb').forEach(function (b) { if (b.checked) serials.push(b.value); });
  if (!serials.length) return;
  if (!confirm('Remove ' + serials.length + ' device(s) from this group?')) return;
  var body = new URLSearchParams();
  serials.forEach(function (s) { body.append('serials', s); });
  fetch('/groups/' + gid + '/devices/remove', { method: 'POST', body: body, headers: { 'HX-Request': 'true' } })
    .then(function (r) {
      if (!r.ok) { if (window.showToast) showToast('Could not remove those devices', 'error'); return; }
      grpToastFrom(r);
      document.body.dispatchEvent(new CustomEvent('group-updated')); // refresh members card
      if (window.pkReload) window.pkReload();                        // removed devices reappear in "Available devices"
    })
    .catch(function () { if (window.showToast) showToast('Could not remove those devices', 'error'); });
}

// Add staged devices without reloading the page: POST via fetch, then fire
// "group-updated" so the members card refreshes in place, clear the basket, and
// reload the "available" pane so the just-added devices drop off it. Falls back to
// a normal form POST if fetch is unavailable.
function dpAddSubmit(e, gid) {
  var field = document.getElementById('pk-serials');
  if (!field || !field.value.trim() || !window.fetch) return true; // let the form submit normally
  e.preventDefault();
  var btn = document.querySelector('[data-dp-submit]');
  if (btn) btn.disabled = true;
  var body = new URLSearchParams();
  body.set('serials', field.value);
  fetch('/groups/' + gid + '/devices', { method: 'POST', body: body, headers: { 'HX-Request': 'true' } })
    .then(function (r) {
      if (!r.ok) {
        if (btn) btn.disabled = false;
        if (window.showToast) showToast('Could not add those devices', 'error');
        return;
      }
      grpToastFrom(r);                                               // "Added N devices · not on this server: …"
      document.body.dispatchEvent(new CustomEvent('group-updated')); // refresh members card
      if (window.pkClear) window.pkClear();                          // empty the basket + reset count/button
      if (window.pkReload) window.pkReload();                        // added devices leave the "available" pane
    })
    .catch(function () { if (btn) btn.disabled = false; if (window.showToast) showToast('Could not add those devices', 'error'); });
  return false;
}
