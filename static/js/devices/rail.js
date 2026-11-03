// Loaded by templates/devices.html.
  (function () {
    // Rail sections show their busiest rows (restaurants 10, user 30 Oct 2026; others 5);
    // "Show all N" reveals the rest and the choice is remembered per section. The active
    // row is always kept visible.
    document.querySelectorAll('.fleet-rail-list').forEach(function (list) {
      var CAP = 20; // one list at a time since the Restaurants | Groups tabs (2 Nov 2026)
      var items = Array.prototype.slice.call(list.querySelectorAll('.fleet-rail-item'));
      if (items.length <= CAP) return;
      items.sort(function (a, b) { return (+b.dataset.ct || 0) - (+a.dataset.ct || 0); });
      items.forEach(function (el) { list.appendChild(el); });
      var key = 'mdm-rail-open-' + list.dataset.sec, open = false;
      try { open = localStorage.getItem(key) === '1'; } catch (e) {}
      var btn = document.createElement('button'); btn.type = 'button'; btn.className = 'fleet-rail-more';
      function apply() {
        var hidden = 0;
        items.forEach(function (el, i) {
          var hide = !open && i >= CAP && !el.classList.contains('on');
          el.hidden = hide; if (hide) hidden++;
        });
        btn.textContent = open ? 'Show less' : 'Show all ' + items.length;
        btn.hidden = !open && hidden === 0;
      }
      btn.addEventListener('click', function () { open = !open; try { localStorage.setItem(key, open ? '1' : '0'); } catch (e) {} apply(); });
      list.appendChild(btn); apply();
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
