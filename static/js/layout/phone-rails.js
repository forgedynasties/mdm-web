// Phones (site-wide audit, 7 Nov 2026; user: "on phone the side rail shows up first"). Fleet's
// left rail, and every list page that copies it, would sit above the list, up to 580 px tall.
// Below 720 px it folds to one line naming the current view; a tap opens the full list in
// place and picking an item folds it again. phone.css does the hiding, inside its 720 px
// media query, so desktop never sees any of this.
(function () {
  var mq = window.matchMedia('(max-width: 720px)');
  var CHEV = '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg>';

  function current(rail) {
    var on = rail.querySelector('.fleet-rail-item.on, .fleet-rail-item[aria-pressed="true"], .fleet-rail-item[aria-current]');
    if (on) {
      var nm = on.querySelector('.nm'), ct = on.querySelector('.ct');
      // .nm can hold a note after its name ("prod" + "17.0.0"): the first piece names it.
      var el = nm || on, first = el.firstChild;
      var name = (first && first.textContent.trim()) || el.textContent;
      return { name: name.replace(/\s+/g, ' ').trim(), count: ct ? ct.textContent.trim() : '' };
    }
    var title = document.querySelector('main [data-cl-title], main .fleet-pane-head h2, main .fleet-pane-head .t');
    return { name: ((title && title.textContent) || 'Everything').replace(/\s+/g, ' ').trim(), count: '' };
  }
  function setup(rail) {
    var btn = rail.querySelector(':scope > .ph-rail-btn');
    if (!btn) {
      btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'ph-rail-btn';
      btn.setAttribute('aria-expanded', 'false');
      btn.innerHTML = '<span class="ph-rail-k">View</span><span class="ph-rail-nm"></span><span class="ph-rail-ct"></span>' + CHEV;
      rail.insertBefore(btn, rail.firstChild);
      btn.addEventListener('click', function () {
        var open = !rail.classList.contains('ph-open');
        rail.classList.toggle('ph-open', open);
        btn.setAttribute('aria-expanded', open);
      });
    }
    function label() {
      var c = current(rail), nm = btn.querySelector('.ph-rail-nm'), ct = btn.querySelector('.ph-rail-ct');
      if (nm.textContent !== c.name) nm.textContent = c.name;
      if (ct.textContent !== c.count) ct.textContent = c.count;
    }
    label();
  }
  function run() {
    if (!mq.matches) return;
    document.querySelectorAll('.fleet-rail').forEach(setup);
  }
  // Picking something folds the rail; its tabs and search keep it open. On the window,
  // in capture: Fleet's rail links stop the click before it reaches the rail.
  window.addEventListener('click', function (e) {
    if (!mq.matches || !e.target.closest) return;
    var rail = e.target.closest('.fleet-rail.ph-open');
    if (!rail || e.target.closest('.ph-rail-btn') || !e.target.closest('.fleet-rail-item, .ri-main, .ri-go')) return;
    rail.classList.remove('ph-open');
    rail.querySelector('.ph-rail-btn').setAttribute('aria-expanded', 'false');
    setTimeout(run, 50);
  }, true);
  // A menu pinned to its button's edge can open past the screen: slide it back in.
  function clampMenus() {
    if (!mq.matches) return;
    var vw = document.documentElement.clientWidth;
    document.querySelectorAll('.cc-menu, [role=menu], main details[open] > :not(summary)').forEach(function (m) {
      if (m.checkVisibility && !m.checkVisibility({ contentVisibilityAuto: true })) return;
      if (!m.offsetWidth) return;
      m.style.translate = '';
      m.style.maxWidth = (vw - 16) + 'px';
      var r = m.getBoundingClientRect();
      var dx = r.left < 8 ? 8 - r.left : r.right > vw - 8 ? vw - 8 - r.right : 0;
      if (dx) m.style.translate = dx + 'px';
    });
  }
  document.addEventListener('click', function () { setTimeout(clampMenus, 30); }, true);
  var timer = 0;
  function soon() { if (!timer) timer = setTimeout(function () { timer = 0; run(); }, 100); }
  function boot() {
    run();
    new MutationObserver(soon).observe(document.body, { childList: true, subtree: true });
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
  document.addEventListener('htmx:afterSettle', soon);
  mq.addEventListener && mq.addEventListener('change', soon);
})();
