// Loaded by templates/device.html.
// A popover inside the facts bar is fixed-positioned under its cell (the bar clips), and
// any scroll closes it rather than leaving it floating away from its cell.
window.ddFactsPop = function (pop, anchor) {
  if (!pop || !anchor) return;
  // Fixed under its anchor wherever it sits, so a card's overflow cannot clip it.
  pop.style.position = 'fixed'; pop.style.right = 'auto';
  var cell = (pop.closest('.dd-facts') && anchor.closest('.dfc, .ddc-kv')) || anchor, r = cell.getBoundingClientRect();
  pop.style.top = (r.bottom + 6) + 'px';
  pop.style.left = Math.max(12, Math.min(r.left, window.innerWidth - pop.offsetWidth - 12)) + 'px';
};
if (!window.__ddFactsScroll) {
  window.__ddFactsScroll = true;
  // Fixed menus do not follow what scrolls under them: close them on any scroll (the
  // panes scroll on their own in the split, so listen in the capture phase).
  window.addEventListener('scroll', function (e) {
    if (e.target && e.target.closest && e.target.closest('.cc-menu')) return; // a long list scrolling inside the menu
    document.querySelectorAll('.pl-pop:not([hidden])').forEach(function (p) { p.setAttribute('hidden', ''); });
    var c = document.querySelector('.cpill-pop:not([hidden])'); if (c) { var b = document.getElementById('cpill-btn'); if (b) b.click(); }
  }, { passive: true, capture: true });
}
window.ddPadToggle = function (e) {
  if (e) { e.preventDefault(); e.stopPropagation(); }
  var pop = document.getElementById('dd-pad-pop'), btn = e && e.currentTarget;
  if (!pop) return;
  var open = pop.hasAttribute('hidden');
  if (open) pop.removeAttribute('hidden'); else pop.setAttribute('hidden', '');
  if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
};
// Mirror the pad state onto the chip: na (charging off) | on | vacant | flapping | bad.
window.ddPadChip = function (st) {
  var c = document.getElementById('pad-chip'), t = document.getElementById('pad-chip-t');
  if (!c || !t) return;
  c.setAttribute('data-st', st);
  t.textContent = { na: 'off', on: 'phone on', vacant: 'vacant', flapping: 'misplaced' }[st] || 'unreadable';
};
if (!window.__ddPadHook) {
  window.__ddPadHook = true;
  document.addEventListener('click', function (e) { var p = document.getElementById('dd-pad-pop'); if (p && !p.hidden && !e.target.closest('.dd-pad-wrap')) p.setAttribute('hidden', ''); });
  document.addEventListener('keydown', function (e) { var p = document.getElementById('dd-pad-pop'); if (e.key === 'Escape' && p && !p.hidden) p.setAttribute('hidden', ''); });
}
window.ddNotesToggle = function (e) {
  if (e) { e.preventDefault(); e.stopPropagation(); }
  var pop = document.getElementById('dd-notes-pop'), btn = e && e.currentTarget;
  if (!pop) return;
  var open = pop.hasAttribute('hidden');
  if (open) { pop.removeAttribute('hidden'); var ta = pop.querySelector('textarea'); if (ta) ta.focus(); } else pop.setAttribute('hidden', '');
  if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
};
if (!window.__ddNotesHook) {
  window.__ddNotesHook = true;
  document.addEventListener('click', function (e) { var p = document.getElementById('dd-notes-pop'); if (p && !p.hidden && !e.target.closest('.dd-notes-wrap')) p.setAttribute('hidden', ''); });
  document.addEventListener('keydown', function (e) { var p = document.getElementById('dd-notes-pop'); if (e.key === 'Escape' && p && !p.hidden) p.setAttribute('hidden', ''); });
}
