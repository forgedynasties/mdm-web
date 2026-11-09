// Loaded by templates/devices.html.
  (function () {
    // Busiest rows first. Every row is on the list since the rail runs the pane's height and
    // has a search (S2, 10 Nov 2026); the selected one is scrolled into view once it is sized.
    document.querySelectorAll('.fleet-rail-list').forEach(function (list) {
      var items = Array.prototype.slice.call(list.querySelectorAll('.fleet-rail-item'));
      items.sort(function (a, b) { return (+b.dataset.ct || 0) - (+a.dataset.ct || 0); });
      items.forEach(function (el) { list.appendChild(el); });
      setTimeout(function () {
        var on = list.querySelector('.fleet-rail-item.on');
        if (!on || list.scrollHeight <= list.clientHeight) return;
        var top = on.getBoundingClientRect().top - list.getBoundingClientRect().top;
        if (top < 0 || top + on.offsetHeight > list.clientHeight) list.scrollTop += top - (list.clientHeight - on.offsetHeight) / 2;
      }, 200);
    });
  })();
  
  // Rail tabs (T1, picked 2 Nov 2026): Restaurants or Groups, one list at a time. The page
  // opens the tab of the selected collection, else the server's default: Restaurants for
  // everyone, Groups only for someone whose restaurants are locked (engineers). No tab is
  // remembered (3 Nov 2026: a remembered Groups tab carried over to other accounts on the
  // same browser).
  (function () {
    var bar = document.querySelector('[data-rail-tabs]');
    if (!bar) return;
    var tabs = Array.prototype.slice.call(bar.querySelectorAll('[role=tab]'));
    try { localStorage.removeItem('mdm-rail-tab'); } catch (e) {}
    function show(which) {
      tabs.forEach(function (t) { t.setAttribute('aria-selected', t.dataset.rt === which ? 'true' : 'false'); t.tabIndex = t.dataset.rt === which ? 0 : -1; });
      document.querySelectorAll('.fleet-rail-list[data-sec]').forEach(function (l) {
        if (l.dataset.sec === 'restaurants' || l.dataset.sec === 'groups') l.hidden = l.dataset.sec !== which;
      });
      bar.querySelectorAll('[data-rt-add]').forEach(function (a) { a.hidden = a.dataset.rtAdd !== which; });
      bar.dataset.tab = which;
    }
    show(bar.dataset.tab);
    tabs.forEach(function (t) {
      t.addEventListener('click', function () { show(t.dataset.rt); });
      t.addEventListener('keydown', function (e) {
        if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
        e.preventDefault();
        var next = tabs[(tabs.indexOf(t) + (e.key === 'ArrowRight' ? 1 : tabs.length - 1)) % tabs.length];
        show(next.dataset.rt); next.focus();
      });
    });
  })();
