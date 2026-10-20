// Loaded by templates/overview_new.html.
// First-visit intro: shown once per browser, dismissed by either button.
(function () {
  var box = document.getElementById('ovd-intro');
  if (!box) return;
  var KEY = 'mdm-ov-new-intro-o2'; // new key for the O2 layout (20 Oct), so everyone sees it once
  try { if (localStorage.getItem(KEY)) return; } catch (e) { return; }
  function done() { box.hidden = true; try { localStorage.setItem(KEY, '1'); } catch (e) {} }
  document.addEventListener('htmx:beforeSwap', function gone() { box.remove(); document.removeEventListener('htmx:beforeSwap', gone); });
  box.hidden = false;
  document.getElementById('ovd-intro-ok').addEventListener('click', done);
  document.addEventListener('keydown', function esc(e) { if (e.key === 'Escape' && !box.hidden) { done(); document.removeEventListener('keydown', esc); } });
})();

(function () {
  var t = document.getElementById('ovd-at');
  if (!t) return;
  var d = new Date(t.getAttribute('datetime'));
  if (!isNaN(d)) t.textContent = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  if (window.mdmTickRel) window.mdmTickRel();
})();

// Venue cards: the ‹ › buttons scroll the strip by one card.
document.addEventListener('click', function (e) {
  var b = e.target.closest('[data-ov2-strip]');
  if (!b) return;
  var s = document.getElementById('ov2-strip');
  if (!s) return;
  var card = s.querySelector('.ov2-v');
  var step = card ? card.getBoundingClientRect().width + 14 : s.clientWidth / 3;
  s.scrollBy({ left: step * (+b.getAttribute('data-ov2-strip')), behavior: 'smooth' });
});
