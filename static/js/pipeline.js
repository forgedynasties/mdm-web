// Loaded by templates/pipeline.html.
// + Add model popover on the Hardware section.
window.luAddToggle = function (e) {
  e.preventDefault(); e.stopPropagation();
  var pop = document.getElementById('lu-add'), btn = e.currentTarget; if (!pop) return;
  var open = pop.hasAttribute('hidden');
  if (open) pop.removeAttribute('hidden'); else pop.setAttribute('hidden', '');
  btn.setAttribute('aria-expanded', open ? 'true' : 'false');
};
if (!window.__luPopHook) {
  window.__luPopHook = true;
  document.addEventListener('click', function (e) { var p = document.getElementById('lu-add'); if (p && !p.hidden && !e.target.closest('.lu-pop-wrap')) p.setAttribute('hidden', ''); });
  document.addEventListener('keydown', function (e) { var p = document.getElementById('lu-add'); if (e.key === 'Escape' && p && !p.hidden) p.setAttribute('hidden', ''); });
}
// Delegated, so the chips keep working after the side list swaps the lanes in place.
if (!window.__plChips) {
  window.__plChips = true;
  document.addEventListener('click', function (e) {
    var b = e.target.closest && e.target.closest('.pl-chip'); if (!b) return;
    document.querySelectorAll('.pl-chip').forEach(function (x) { x.classList.toggle('on', x === b); x.setAttribute('aria-selected', x === b ? 'true' : 'false'); });
    document.querySelectorAll('.pl-model').forEach(function (m) { m.hidden = m.dataset.model !== b.dataset.model; });
  });
}
