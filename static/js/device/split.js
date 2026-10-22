// Loaded by templates/device.html.
(function () {
  // Split is the only wide-screen layout (14 Oct): graph on the left, tabs on the right.
  // Narrower windows stack them, as before.
  var WIDE = window.matchMedia('(min-width: 1100px)');
  // Markers remember where the graph and the tab row live in the normal layout, so
  // undocking puts them back exactly.
  function apply() {
    var main = document.querySelector('.ddc-main'), card = document.querySelector('.chart-card'),
        tabrow = document.querySelector('.ddc-main .dd-tabrow'), facts = document.querySelector('.ddc-main .dd-facts');
    if (!main || !card || !tabrow || !facts) return;
    var on = WIDE.matches, split = document.getElementById('dd-split');
    // The toggle lives in the top bar (copied there from the page's island template),
    // so its pressed look is driven by a body class rather than by the button itself.
    document.body.classList.toggle('dd-is-split', on);
    if (on && !split) {
      if (!card.__home) { card.__home = document.createComment('chart-home'); card.parentNode.insertBefore(card.__home, card); }
      if (!tabrow.__home) { tabrow.__home = document.createComment('tabs-home'); tabrow.parentNode.insertBefore(tabrow.__home, tabrow); }
      split = document.createElement('div'); split.id = 'dd-split'; split.className = 'dd-split';
      var l = document.createElement('div'); l.className = 'dd-split-l';
      var r = document.createElement('div'); r.className = 'dd-split-r';
      split.appendChild(l); split.appendChild(r);
      facts.parentNode.insertBefore(split, facts.nextSibling);
      // Facts line above both panes; the tab row heads the right pane.
      tabrow.parentNode.insertBefore(facts, split);
      facts.classList.add('is-split');
      l.appendChild(card);
      var lg = document.getElementById('vt-legend'); if (lg) l.appendChild(lg);
      r.appendChild(tabrow);
      // Pad and Notes join the end of the facts line, so the tabs keep one row.
      var chips = tabrow.querySelector('.dd-tabrow-r'); if (chips) facts.appendChild(chips);
      Array.prototype.slice.call(main.querySelectorAll(':scope > .dd-panel')).forEach(function (p) { r.appendChild(p); });
      // The facts (client, battery, vitals, venue, groups, pad, notes) open Overview rather
      // than spanning both panes (15 Oct); undocking puts them back above the tabs.
      var sum = document.getElementById('dd-panel-summary'); if (sum) sum.insertBefore(facts, sum.firstChild);
    } else if (!on && split) {
      var rr = split.querySelector('.dd-split-r');
      Array.prototype.slice.call(rr.querySelectorAll(':scope > .dd-panel')).forEach(function (p) { main.insertBefore(p, split); });
      if (tabrow.__home) tabrow.__home.parentNode.insertBefore(tabrow, tabrow.__home);
      var ch = facts.querySelector(':scope > .dd-tabrow-r'); if (ch) tabrow.appendChild(ch);
      facts.classList.remove('is-split');
      if (tabrow.__home) tabrow.parentNode.insertBefore(facts, tabrow.nextSibling);
      if (card.__home) card.__home.parentNode.insertBefore(card, card.__home);
      var lg2 = document.getElementById('vt-legend'); if (lg2) card.parentNode.insertBefore(lg2, card.nextSibling);
      split.remove();
    } else return;
    aboutIntoOverview(on);
    // In Overview the facts follow the device pane (and what needs a look).
    if (on) { var anc = document.getElementById('dd-hw'); if (anc) anc.parentNode.insertBefore(facts, anc.nextSibling); }
    paneFacts(on);
    overviewTab(on);
    fit();
    // Charts follow their new container width.
    setTimeout(function () { window.dispatchEvent(new Event('resize')); }, 30);
    setTimeout(function () { if (window.ddSyncMap) window.ddSyncMap(); }, 80);
  }
  // In the split the graph has left Overview; what stays there is the map and the pad
  // card, and a device with neither (a mains-powered stock device) would open on an empty
  // tab. Hide it then, and open the next tab instead.
  // The facts fold into the device pane (device-placement-demos.html, P2, picked 15 Oct):
  // the client and lock beside Online, the restaurant and groups as a line under the name,
  // notes (and the pad's popover; its column is in the vitals) in the pane's corner. The
  // elements move whole, so their popovers and live updates keep working; undocking
  // puts each back where it came from.
  function paneFacts(on) {
    var hw = document.getElementById('dd-hw'); if (!hw) return;
    var st = hw.querySelector('.dd-hw-st'), t = hw.querySelector('.ddc-hw-t');
    function mv(el, to) { if (!el || !to) return; if (!el.__home) { el.__home = document.createComment('home'); el.parentNode.insertBefore(el.__home, el); } to.appendChild(el); }
    function back(el) { if (el && el.__home && el.__home.parentNode) el.__home.parentNode.insertBefore(el, el.__home); }
    var cp = document.querySelector('.cpill-wrap'), place = document.querySelector('.dd-place'),
        lock = document.querySelector('.ddc-kstate'), notes = document.querySelector('.dd-notes-wrap'), pad = document.querySelector('.dd-pad-wrap');
    lock = lock && lock.closest('.dfc');
    var where = hw.querySelector('.dd-where');
    if (!where && t) { where = document.createElement('div'); where.className = 'dd-where'; t.appendChild(where); }
    var corner = hw.querySelector('.dd-pane-corner');
    if (!corner) { corner = document.createElement('div'); corner.className = 'dd-pane-corner'; hw.appendChild(corner); }
    if (on) { mv(cp, st); mv(lock, st); mv(place, where); mv(pad, corner); mv(notes, corner); }
    else [cp, lock, place, pad, notes].forEach(back);
    document.body.classList.toggle('dd-pane-facts', on);
  }
  function aboutIntoOverview(on) {
    var about = document.getElementById('dd-panel-about'), ov = document.getElementById('dd-panel-summary'),
        tab = document.getElementById('dd-tab-about'), grid = document.querySelector('#dd-panel-about > .about-grid, #dd-panel-summary > .about-grid');
    if (!about || !ov || !grid) return;
    // Only the device pane and what needs a look fold into the Overview (V1, 23 Oct 2026);
    // About keeps the rest (enrollment), so its tab stays unless nothing is left there.
    var more = about.querySelector('.about-more');
    var left = more && Array.prototype.some.call(more.children, function (c) { return !/^(SCRIPT|STYLE)$/.test(c.tagName); });
    if (on && grid.parentNode === about) {
      ov.appendChild(grid);
      if (tab) {
        tab.hidden = !left;
        if (!left && tab.classList.contains('active') && window.setDeviceTab) window.setDeviceTab('summary', document.getElementById('dd-tab-summary'));
      }
    } else if (!on && grid.parentNode === ov) {
      about.appendChild(grid);
      if (tab) tab.hidden = false;
    }
  }
  function overviewTab(on) {
    var t = document.getElementById('dd-tab-summary'), p = document.getElementById('dd-panel-summary');
    if (!t || !p) return;
    var empty = !Array.prototype.some.call(p.children, function (c) {
      return !/^(SCRIPT|STYLE|TEMPLATE|LINK)$/.test(c.tagName) && !c.hidden && getComputedStyle(c).display !== 'none';
    });
    t.hidden = on && empty;
    function away() {
      if (!t.hidden || !t.classList.contains('active') || !window.setDeviceTab) return;
      var alt = document.getElementById('dd-tab-apps') || document.getElementById('dd-tab-about');
      if (alt) window.setDeviceTab(alt.getAttribute('data-dd-tab'), alt);
    }
    // On first load setDeviceTab is defined further down the page.
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', away); else away();
  }
  // In the split the page itself does not scroll: the split fills the window above the
  // dock, and each pane scrolls on its own (the tabs pane, and the graph pane when the
  // window is short). Too short a window falls back to an ordinary scrolling page.
  function fit() {
    var split = document.getElementById('dd-split');
    var root = document.documentElement;
    if (!split) { root.classList.remove('dd-fit'); return; }
    split.style.height = '';
    var top = split.getBoundingClientRect().top + (window.scrollY || 0);
    var h = window.innerHeight - top - 100;            // clear of the floating dock
    var ok = h >= 420;
    root.classList.toggle('dd-fit', ok);
    if (ok) { split.style.height = h + 'px'; window.scrollTo(0, 0); }
  }
  window.addEventListener('resize', function () { requestAnimationFrame(fit); });
  // The split's height is worked out from where it starts. The page header above it hides
  // only when the top bar takes over (body.has-island, set by the layout after this ran),
  // so measured once it came out ~200 px short and left an empty band above the dock.
  // Measure again whenever the body's classes change, and once everything has loaded.
  if (window.MutationObserver) new MutationObserver(function () { requestAnimationFrame(fit); }).observe(document.body, { attributes: true, attributeFilter: ['class'] });
  window.addEventListener('load', function () { requestAnimationFrame(fit); });

  // The map used to fill Overview down to the graph's end; it has its own Location tab now
  // (15 Oct), so it keeps its own height.
  function syncMap() {
    var map = document.getElementById('dev-map');
    if (map) map.style.height = '';
  }
  window.ddSyncMap = syncMap;
  if (window.ResizeObserver) {
    var ro = new ResizeObserver(function () { requestAnimationFrame(syncMap); });
    var cc = document.querySelector('.chart-card'); if (cc) ro.observe(cc);
  }
  window.addEventListener('resize', function () { requestAnimationFrame(syncMap); });
  document.addEventListener('click', function (e) { if (e.target.closest && e.target.closest('.dd-tab')) setTimeout(syncMap, 30); });

  if (WIDE.addEventListener) WIDE.addEventListener('change', apply); else if (WIDE.addListener) WIDE.addListener(apply);
  // After an action reloads the page (a boosted form post: Reboot, wireless adb…) htmx
  // inserts the new <main> before it removes the old one and runs this in between, so
  // apply() found the old page's split, thought the work was done, and the new page
  // came up without one. Wait for the swap to finish when the old page is still here.
  if (document.querySelectorAll('.ddc-main').length > 1) setTimeout(apply, 0); else apply();
})();

// Scroll cue (19 Oct): in the fitted split the right pane scrolls on its own and nothing
// said so. While there is more below, a floating ↓ sits at the pane's foot; clicking it
// scrolls most of a pane further down. It goes away at the end and when the page is not
// fitted.
(function () {
  function setup() {
    var pane = document.querySelector('.dd-split-r');
    if (!pane || pane.dataset.cue === '1') return;
    pane.dataset.cue = '1';
    var wrap = document.createElement('div');
    wrap.className = 'dd-cue';
    wrap.innerHTML = '<button type="button" class="dd-cue-b" aria-label="Scroll down" title="More below">'
      + '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14"/><path d="m6 13 6 6 6-6"/></svg></button>';
    pane.appendChild(wrap);
    var btn = wrap.firstChild, raf = 0;
    function sync() {
      raf = 0;
      var fitted = document.documentElement.classList.contains('dd-fit');
      var more = pane.scrollHeight - pane.scrollTop - pane.clientHeight > 24;
      wrap.classList.toggle('on', fitted && more);
    }
    function later() { if (!raf) raf = requestAnimationFrame(sync); }
    btn.addEventListener('click', function () {
      pane.scrollBy({ top: Math.max(120, pane.clientHeight * 0.8), behavior: 'smooth' });
    });
    pane.addEventListener('scroll', later, { passive: true });
    window.addEventListener('resize', later);
    // Tabs switch, cards load and collapse: the pane's height changes under us.
    if (window.ResizeObserver) {
      var ro = new ResizeObserver(later);
      Array.prototype.forEach.call(pane.children, function (c) { ro.observe(c); });
      if (window.MutationObserver) new MutationObserver(function (ms) {
        ms.forEach(function (m) { Array.prototype.forEach.call(m.addedNodes, function (n) { if (n.nodeType === 1) ro.observe(n); }); });
        later();
      }).observe(pane, { childList: true });
    }
    if (window.MutationObserver) new MutationObserver(later).observe(document.documentElement, { attributes: true, attributeFilter: ['class'] });
    document.addEventListener('click', function (e) { if (e.target.closest && e.target.closest('.dd-tab')) setTimeout(later, 60); });
    later();
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', setup); else setTimeout(setup, 0);
  window.addEventListener('load', setup);
})();
