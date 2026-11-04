// Loaded by templates/overview_new.html.
// First-visit intro: shown once per browser, dismissed by either button.
(function () {
  var box = document.getElementById('ovd-intro');
  if (!box) return;
  var KEY = 'mdm-ov-intro-b1'; // new key for the B1 layout (5 Nov 2026), so everyone sees it once
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

// Overview B1 (5 Nov 2026): Today's tabs (Needs you · Venues · Rollouts · Crashes) and Next,
// which steps through the open tab's items one at a time and wraps round.
(function () {
  if (window.__ovbWired) return; window.__ovbWired = true;
  document.addEventListener('click', function (e) {
    var tab = e.target.closest('[data-ovb-tab]');
    if (tab) {
      var hero = tab.closest('[data-ovb-hero]'), key = tab.getAttribute('data-ovb-tab');
      hero.querySelectorAll('[data-ovb-tab]').forEach(function (b) { var on = b === tab; b.classList.toggle('on', on); b.setAttribute('aria-selected', on ? 'true' : 'false'); });
      hero.querySelectorAll('[data-ovb-pane]').forEach(function (p) { p.hidden = p.getAttribute('data-ovb-pane') !== key; });
      return;
    }
    var next = e.target.closest('[data-ovb-next]');
    if (next) {
      var pane = next.closest('[data-ovb-pane]'), items = pane.querySelectorAll('[data-ovb-item]'), i = 0;
      items.forEach(function (it, k) { if (!it.hidden) i = k; });
      items[i].hidden = true; items[(i + 1) % items.length].hidden = false;
      var b = items[(i + 1) % items.length].querySelector('[data-ovb-next]'); if (b) b.focus();
    }
  });
})();
