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

// Overview B1 (5 Nov 2026): Today's tabs (Needs you · Venues · Rollouts · Crashes); T2: a
// row in the tab's list shows its detail on the right; arrow keys move along the list.
(function () {
  if (window.__ovbWired) return; window.__ovbWired = true;
  function ovbPick(li) {
    var pane = li.closest('[data-ovb-pane]'), j = li.getAttribute('data-ovb-pick');
    pane.querySelectorAll('[data-ovb-pick]').forEach(function (b) { var on = b === li; b.classList.toggle('on', on); b.setAttribute('aria-selected', on ? 'true' : 'false'); });
    pane.querySelectorAll('[data-ovb-det]').forEach(function (d) { d.hidden = d.getAttribute('data-ovb-det') !== j; });
  }
  document.addEventListener('keydown', function (e) {
    var li = e.target.closest && e.target.closest('[data-ovb-pick]');
    if (!li || (e.key !== 'ArrowDown' && e.key !== 'ArrowUp')) return;
    var sib = e.key === 'ArrowDown' ? li.nextElementSibling : li.previousElementSibling;
    if (sib) { e.preventDefault(); sib.focus(); ovbPick(sib); }
  });
  document.addEventListener('click', function (e) {
    var tab = e.target.closest('[data-ovb-tab]');
    if (tab) {
      var hero = tab.closest('[data-ovb-hero]'), key = tab.getAttribute('data-ovb-tab');
      hero.querySelectorAll('[data-ovb-tab]').forEach(function (b) { var on = b === tab; b.classList.toggle('on', on); b.setAttribute('aria-selected', on ? 'true' : 'false'); });
      hero.querySelectorAll('[data-ovb-pane]').forEach(function (p) { p.hidden = p.getAttribute('data-ovb-pane') !== key; });
      return;
    }
    var pick = e.target.closest('[data-ovb-pick]');
    if (pick) ovbPick(pick);
  });
})();

// B1 fitted (5 Nov 2026): on a wide window at least 640 px tall the Overview fills the space
// between the header and the dock and the page does not scroll; smaller screens scroll as
// before. The class is dropped as soon as the page is left.
(function () {
  var ovd = document.querySelector('.ovd'); if (!ovd || !ovd.querySelector('.ovb')) return;
  var root = document.documentElement;
  // The template's CSS already fits the page from the first frame (100vh less 188 px);
  // this only measures the exact room, or says ov-nofit when there is too little.
  function fit() {
    if (!document.body.contains(ovd)) return off();
    var dock = document.querySelector('nav.dock');
    var bottom = dock && dock.getBoundingClientRect().height ? dock.getBoundingClientRect().top - 12 : innerHeight - 16;
    var top = ovd.getBoundingClientRect().top + (root.classList.contains('ov-nofit') ? scrollY : 0);
    var h = Math.floor(bottom - top);
    if (innerWidth >= 1101 && innerHeight >= 640 && h >= 520) {
      root.style.setProperty('--ovd-h', h + 'px'); root.classList.remove('ov-nofit'); scrollTo(0, 0);
    } else {
      root.classList.add('ov-nofit');
    }
  }
  function off() {
    root.classList.remove('ov-nofit'); root.style.removeProperty('--ovd-h');
    removeEventListener('resize', fit); document.removeEventListener('htmx:beforeSwap', leaving);
  }
  // Only a swap that replaces the Overview itself ends it (the bell and the island swap too).
  function leaving(e) { var t = e.detail && e.detail.target; if (!t || t.contains(ovd)) off(); }
  addEventListener('resize', fit);
  document.addEventListener('htmx:beforeSwap', leaving);
  fit(); requestAnimationFrame(fit);
})();
