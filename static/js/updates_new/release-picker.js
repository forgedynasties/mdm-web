// Loaded by templates/updates_new.html.
  // Both the product select and the release combobox navigate, because the release
  // drives what the rest of the page can even show; the chosen serials ride along so
  // a re-pick doesn't lose the basket.
  function pushKeepSerials() {
    var s = new URLSearchParams(location.search).get('serials');
    return s ? '&serials=' + encodeURIComponent(s) : '';
  }
  // Swap <main> instead of reloading the document: picking a release re-renders
  // the page around it (step 2 is scoped to the release), but a full reload throws
  // away the app shell and flashes white for what is really an in-page change.
  function pushNavigate(url) {
    if (!window.htmx || !document.querySelector('main')) { location.href = url; return; }
    try {
      var p = window.htmx.ajax('GET', url, { source: document.body, target: 'main', swap: 'outerHTML', select: 'main' });
      if (p && p.then) {
        p.then(function () { try { history.pushState(null, '', url); } catch (e) {} },
               function () { location.href = url; });
      } else {
        try { history.pushState(null, '', url); } catch (e) {}
      }
    } catch (e) { location.href = url; }
  }
  function pushPickProduct(p) {
    pushNavigate('/updates/new?product=' + encodeURIComponent(p) + pushKeepSerials());
  }
  function pushPickRelease(id) {
    if (id) pushNavigate('/updates/new?release=' + id + pushKeepSerials());
  }
  // Type-ahead release combobox: filters as you type, Enter/click picks, and picking
  // navigates (the release drives everything below it). Options are hidden, not
  // removed, so the release already chosen is still there when the search clears.
  (function () {
    var combo = document.getElementById('rel-combo');
    if (!combo) return;
    var input = document.getElementById('rel-search');
    var none = document.getElementById('rel-none');
    var opts = [].slice.call(combo.querySelectorAll('.rel-opt'));
    var active = -1;

    function labelFor(val) {
      for (var i = 0; i < opts.length; i++) {
        if (opts[i].getAttribute('data-value') === val) return opts[i].textContent.trim();
      }
      return '';
    }
    input.value = labelFor(input.getAttribute('data-current') || ''); // prefill the chosen release

    function open() { combo.classList.add('open'); input.setAttribute('aria-expanded', 'true'); }
    function close() { combo.classList.remove('open'); input.setAttribute('aria-expanded', 'false'); active = -1; paint(); }
    function visible() { return opts.filter(function (o) { return !o.classList.contains('hidden'); }); }
    function paint() {
      var vis = visible();
      opts.forEach(function (o) { o.classList.remove('active'); });
      if (active >= 0 && active < vis.length) { vis[active].classList.add('active'); vis[active].scrollIntoView({ block: 'nearest' }); }
    }
    function filter(q) {
      q = q.trim().toLowerCase();
      opts.forEach(function (o) {
        var s = (o.getAttribute('data-search') || '').toLowerCase();
        o.classList.toggle('hidden', q !== '' && s.indexOf(q) === -1);
      });
      none.hidden = visible().length > 0;
      active = -1; paint();
    }
    function choose(o) { pushPickRelease(o.getAttribute('data-value')); }

    input.addEventListener('focus', function () { filter(''); open(); input.select(); });
    input.addEventListener('input', function () { filter(input.value); open(); });
    input.addEventListener('keydown', function (e) {
      var vis = visible();
      if (e.key === 'ArrowDown') { e.preventDefault(); open(); active = Math.min(active + 1, vis.length - 1); paint(); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); active = Math.max(active - 1, 0); paint(); }
      else if (e.key === 'Enter') { if (combo.classList.contains('open')) { e.preventDefault(); if (active >= 0) choose(vis[active]); } }
      else if (e.key === 'Escape') { input.value = labelFor(input.getAttribute('data-current') || ''); close(); }
    });
    opts.forEach(function (o) { o.addEventListener('mousedown', function (e) { e.preventDefault(); choose(o); }); });
    // A swapped-in page runs this script again, so the document listener is bound
    // once and looks the combo up live rather than closing over a detached node.
    if (!window.__relComboBound) {
      window.__relComboBound = true;
      document.addEventListener('click', function (e) {
        var c = document.getElementById('rel-combo'), i = document.getElementById('rel-search');
        if (!c || !i || c.contains(e.target)) return;
        var cur = i.getAttribute('data-current') || '', lbl = '';
        c.querySelectorAll('.rel-opt').forEach(function (o) { if (o.getAttribute('data-value') === cur) lbl = o.textContent.trim(); });
        i.value = lbl;
        c.classList.remove('open');
        i.setAttribute('aria-expanded', 'false');
      });
    }
  })();
  
