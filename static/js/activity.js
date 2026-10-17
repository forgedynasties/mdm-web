// Loaded by templates/activity.html.
      (function () {
        var wrap = document.getElementById('act-up'), btn = document.getElementById('act-actor-btn'), pop = document.getElementById('act-up-pop');
        var q = document.getElementById('act-up-q'), list = document.getElementById('act-up-list'), more = document.getElementById('act-up-more'), rest = document.getElementById('act-up-rest');
        var none = document.getElementById('act-up-none'), hidden = document.getElementById('act-actor');
        if (!wrap || !btn || !pop) return;
        function open() { pop.hidden = false; btn.setAttribute('aria-expanded', 'true'); q.value = ''; filter(); setTimeout(function () { q.focus(); }, 30); }
        function close() { pop.hidden = true; btn.setAttribute('aria-expanded', 'false'); }
        function filter() {
          var s = q.value.trim().toLowerCase(), shown = 0;
          // a search looks through everyone; the fold only applies to an empty box
          if (rest) rest.hidden = !s && !rest.dataset.open;
          if (more) more.hidden = !!s || !!(rest && rest.dataset.open);
          Array.prototype.forEach.call(list.querySelectorAll('.ac-up-opt'), function (o) {
            var hit = !s || (o.dataset.s || '').toLowerCase().indexOf(s) >= 0;
            var folded = o.dataset.rest === '1' && !s && !(rest && rest.dataset.open);
            o.hidden = !hit || folded; if (hit && !folded) shown++;
          });
          Array.prototype.forEach.call(list.querySelectorAll('.ac-up-cap'), function (c) { c.hidden = !!s; });
          none.hidden = shown > 0;
        }
        btn.addEventListener('click', function (e) { e.stopPropagation(); pop.hidden ? open() : close(); });
        if (more) more.addEventListener('click', function () { rest.dataset.open = '1'; filter(); });
        q.addEventListener('input', filter);
        q.addEventListener('keydown', function (e) {
          if (e.key === 'Escape') { close(); btn.focus(); }
          if (e.key === 'Enter') { var first = list.querySelector('.ac-up-opt:not([hidden])'); if (first) first.click(); e.preventDefault(); }
        });
        list.addEventListener('click', function (e) {
          var o = e.target.closest('.ac-up-opt'); if (!o) return;
          hidden.value = o.dataset.v || '';
          hidden.form.submit();
        });
        document.addEventListener('click', function (e) { if (!wrap.contains(e.target)) close(); });
      })();
      
