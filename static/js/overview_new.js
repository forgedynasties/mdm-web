// Loaded by templates/overview_new.html.
// First-visit intro: shown once per browser, dismissed by either button.
(function () {
  var box = document.getElementById('ovd-intro');
  if (!box) return;
  var KEY = 'mdm-ov-new-intro-c'; // new key for the C layout (21 Oct), so everyone sees it once
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

// Filter pills and the cards' ··· menus: one open at a time, closed by a click outside
// or Escape. Bound once, since boosted navigation re-runs this file.
if (!window.__ocMenus) {
  window.__ocMenus = true;
  var closeAll = function (except) {
    document.querySelectorAll('[data-oc-menu][aria-expanded="true"]').forEach(function (b) {
      if (b === except) return;
      b.setAttribute('aria-expanded', 'false');
      var m = b.nextElementSibling;
      if (m) m.hidden = true;
    });
  };
  document.addEventListener('click', function (e) {
    var b = e.target.closest('[data-oc-menu]');
    if (b) {
      e.preventDefault();
      var m = b.nextElementSibling, open = b.getAttribute('aria-expanded') === 'true';
      closeAll(b);
      b.setAttribute('aria-expanded', open ? 'false' : 'true');
      if (m) m.hidden = open;
      return;
    }
    if (!e.target.closest('.oc-f .cc-menu, .oc-cm .cc-menu')) closeAll(null);
  });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') closeAll(null); });
}
