// Loaded by templates/devices.html.
  (function () {
    // Rail sections show their busiest rows (restaurants 10, user 30 Oct 2026; others 5);
    // "Show all N" reveals the rest and the choice is remembered per section. The active
    // row is always kept visible.
    document.querySelectorAll('.fleet-rail-list').forEach(function (list) {
      var CAP = list.dataset.sec === 'restaurants' ? 10 : 5;
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
  
