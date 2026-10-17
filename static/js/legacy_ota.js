// Loaded by templates/legacy_ota.html.
// Fold away the devices that can also take an MDM OTA. Default on, remembered.
function loOnlyLegacy(on) {
  var t = document.querySelector('.lo-tbl');
  if (t) t.classList.toggle('only-legacy', on);
  try { localStorage.setItem('mdm-legacy-only', on ? '1' : '0'); } catch (e) {}
}
(function () {
  // The table is swapped in by htmx every few seconds, so re-apply after each swap.
  function apply() {
    var on = true;
    try { var v = localStorage.getItem('mdm-legacy-only'); if (v !== null) on = v === '1'; } catch (e) {}
    var box = document.getElementById('lo-only-legacy');
    if (box) box.checked = on;
    loOnlyLegacy(on);
  }
  setTimeout(apply, 0);
  document.body.addEventListener('htmx:afterSwap', function (e) {
    if (e.target && e.target.id === 'lo-devices') apply();
  });
})();

// The picker's rows depend on the chosen release (eligible vs blocked), so a
// change re-points it and reloads.
function loRelease(id) {
  var rail = document.getElementById('pk-rail'), wrap = document.getElementById('lo-picker-wrap');
  if (!rail) return;
  if (wrap) wrap.hidden = !id;
  rail.dataset.release = id || '';
  if (window.pkClear) pkClear();
  if (window.pkReload) pkReload();
}
