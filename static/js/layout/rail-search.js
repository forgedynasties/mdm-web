// Side rail search (S2, picked 10 Nov 2026; user: "on side rails add search", long rails only).
// A rail with more than 8 rows gets a search field: under the tabs where it has tabs (Fleet),
// else on top. It hides the rows that don't match and any heading left with nothing under it.
// Alerts and Wireless adb swap their rail in on a live refresh, so the text and focus carry over.
(function () {
  var LONG = 8;
  var MAG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/></svg>';
  var carried = null;

  function textOf(row) {
    return (row.querySelector('.nm') || row).textContent.replace(/\s+/g, ' ').trim().toLowerCase();
  }
  // The match is coloured with the Highlight API: wrapping it in <mark> would rewrite rows
  // other scripts read. A browser without the API just doesn't colour it.
  function rangesIn(row, q) {
    var out = [], w = document.createTreeWalker(row.querySelector('.nm') || row, NodeFilter.SHOW_TEXT), n;
    while ((n = w.nextNode())) {
      var i = n.data.toLowerCase().indexOf(q);
      if (i < 0) continue;
      var r = new Range();
      r.setStart(n, i);
      r.setEnd(n, i + q.length);
      out.push(r);
    }
    return out;
  }

  var painters = [];
  function paintHits() {
    if (!window.Highlight || !CSS.highlights) return;
    painters = painters.filter(function (p) { return p.rail.isConnected; });
    var all = [];
    painters.forEach(function (p) { all = all.concat(p.ranges); });
    if (!all.length) { CSS.highlights.delete('rail-hit'); return; }
    var h = new Highlight();
    all.forEach(function (r) { h.add(r); });
    CSS.highlights.set('rail-hit', h);
  }

  function setup(rail) {
    rail._railSearch = true;
    var rows = Array.prototype.slice.call(rail.querySelectorAll('.fleet-rail-item'));
    // data-rail-search: always a search, whatever the length (Models), its value the placeholder.
    if (rows.length <= LONG && !rail.hasAttribute('data-rail-search')) return;
    var tabs = rail.querySelector(':scope > .rail-tabs');

    var box = document.createElement('div');
    box.className = 'rail-find';
    box.innerHTML = '<label class="rail-find-box">' + MAG + '<input type="search" autocomplete="off" spellcheck="false">' +
      '<button type="button" class="rail-find-x" aria-label="Clear search" hidden>×</button></label>';
    var none = document.createElement('div');
    none.className = 'rail-none';
    none.hidden = true;
    var after = tabs || rail.querySelector(':scope > .ph-rail-btn');
    rail.insertBefore(box, after ? after.nextSibling : rail.firstChild);
    rail.insertBefore(none, box.nextSibling);
    var input = box.querySelector('input'), clear = box.querySelector('.rail-find-x');
    var painter = { rail: rail, ranges: [] };
    painters.push(painter);
    var key = location.href + '|' + rail.getAttribute('aria-label');

    function label() {
      var t = tabs && tabs.dataset.tab;
      var ph = t ? 'Search ' + t : (rail.getAttribute('data-rail-search') || 'Search this list');
      input.placeholder = ph;
      input.setAttribute('aria-label', ph);
    }
    function closedList(row) { return row.closest('.fleet-rail-list[hidden]'); }

    function apply() {
      var q = input.value.trim().toLowerCase(), ranges = [];
      rail.classList.toggle('rail-q', !!q);
      clear.hidden = !q;
      var here = 0, elsewhere = {};
      rows.forEach(function (row) {
        var hit = !q || textOf(row).indexOf(q) >= 0;
        row.classList.toggle('rail-out', !hit);
        if (!hit || !q) return;
        var shut = closedList(row);
        if (shut) { elsewhere[shut.dataset.sec] = (elsewhere[shut.dataset.sec] || 0) + 1; return; }
        here++;
        ranges = ranges.concat(rangesIn(row, q));
      });
      // A heading stays only while a row under it does.
      rail.querySelectorAll('.fleet-rail-cap').forEach(function (cap) {
        var any = !q;
        for (var s = cap.nextElementSibling; s && !any && !s.classList.contains('fleet-rail-cap'); s = s.nextElementSibling) {
          var inside = s.matches('.fleet-rail-item') ? [s] : Array.prototype.slice.call(s.querySelectorAll('.fleet-rail-item'));
          any = inside.some(function (r) { return !r.classList.contains('rail-out'); });
        }
        cap.classList.toggle('rail-out', !any);
      });
      none.hidden = !q || here > 0;
      none.textContent = '';
      if (!none.hidden) {
        none.appendChild(document.createTextNode('Nothing here matches "' + input.value.trim() + '".'));
        Object.keys(elsewhere).forEach(function (sec) {
          var b = document.createElement('button');
          b.type = 'button';
          b.textContent = elsewhere[sec] + ' in ' + sec.charAt(0).toUpperCase() + sec.slice(1) + ' →';
          b.addEventListener('click', function () {
            var tab = tabs.querySelector('[data-rt="' + sec + '"]');
            if (tab) tab.click();
          });
          none.appendChild(b);
        });
      }
      painter.ranges = ranges;
      paintHits();
    }
    function remember() {
      carried = input.value ? { key: key, q: input.value, focus: document.activeElement === input, at: input.selectionStart } : null;
    }

    input.addEventListener('input', function () { apply(); remember(); });
    input.addEventListener('keydown', function (e) {
      if (e.key !== 'Escape' || !input.value) return;
      e.stopPropagation();
      input.value = '';
      apply();
      remember();
    });
    input.addEventListener('focus', remember);
    // Later, so a rail being swapped out (which blurs it too) keeps "this had focus".
    input.addEventListener('blur', function () { setTimeout(function () { if (rail.isConnected) remember(); }); });
    input.addEventListener('keyup', remember);
    clear.addEventListener('click', function () { input.value = ''; apply(); remember(); input.focus(); });
    // Fleet's tabs: the field names the open one, and the matches follow it.
    if (tabs) new MutationObserver(function () { label(); apply(); }).observe(tabs, { attributes: true, attributeFilter: ['data-tab'] });

    label();
    if (carried && carried.key === key) {
      var c = carried;
      input.value = c.q;
      apply();
      if (c.focus) { input.focus(); input.setSelectionRange(c.at, c.at); }
    }
  }

  function run() {
    document.querySelectorAll('.fleet-rail').forEach(function (rail) {
      if (!rail._railSearch) setup(rail);
    });
  }
  function boot() {
    run();
    // Not debounced: a swapped-in rail has to get its text and focus back before the next key.
    new MutationObserver(run).observe(document.body, { childList: true, subtree: true });
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
})();
