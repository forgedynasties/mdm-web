// Loaded by templates/packages.html.
// Save the system-app override. Reads the checkbox state directly (reliable), reverts
// on failure, and updates the Effective badge for immediate feedback.
function flagPkg(el) {
  var on = el.checked;
  fetch('/packages/flag', {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: 'package=' + encodeURIComponent(el.dataset.pkg) + '&flagged=' + (on ? '1' : '0')
  }).then(function (r) {
    if (!r.ok) { el.checked = !on; if (window.showToast) showToast('Could not save — try again', 'error'); return; }
    var eff = el.closest('.pk-row').querySelector('.pk-eff');
    if (eff) {
      var v = on ? 'system' : (el.dataset.reported || 'user');
      eff.textContent = v;
      eff.className = 'pk-eff ' + (v === 'system' ? 'sys' : 'usr');
    }
  }).catch(function () { el.checked = !on; });
}
