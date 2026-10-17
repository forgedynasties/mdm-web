// Loaded by templates/overview.html.
// Sites heatmap: legend chips toggle a class filter; "Show N more" reveals the overflow.
(function () {
  var root = document.getElementById('ov3-sites');
  if (!root) return;
  var active = null, expanded = false;
  var tiles = Array.prototype.slice.call(root.querySelectorAll('.ov3-site'));
  var btns = Array.prototype.slice.call(root.querySelectorAll('.ov3-sfilter button'));
  var more = document.getElementById('ov3-sites-more');
  function apply() {
    var shown = 0;
    tiles.forEach(function (t) {
      var match = !active || t.getAttribute('data-c') === active;
      var over = !expanded && !active && t.classList.contains('xtra');
      t.classList.toggle('hide', !match || over);
      if (match && !over) shown++;
    });
    btns.forEach(function (b) { b.classList.toggle('on', b.getAttribute('data-f') === active); });
    if (more) more.style.display = (active || expanded) ? 'none' : '';
  }
  btns.forEach(function (b) {
    b.addEventListener('click', function () {
      var f = b.getAttribute('data-f');
      active = (active === f) ? null : f;
      apply();
    });
  });
  if (more) more.addEventListener('click', function (e) { e.preventDefault(); expanded = true; apply(); });
})();

// ── Widget system: home-screen grid. Edit mode (Customize → Edit layout): drag to
// reorder, half/full width, compact/normal/tall, hide, add from the tray. Presets
// and reset live in the popover. Every change saves to the account.
(function () {
  var root = document.getElementById('ov3');
  if (!root) return;
  var grid = document.getElementById('ov3-grid');

  // ── masonry: each grid child spans as many 4px rows as it is tall (plus the gap),
  // re-measured whenever a card's content changes size ──
  var ROW = 4, GAP = 16;
  function span(el) {
    var h = el.getBoundingClientRect().height;
    el.style.gridRowEnd = h ? 'span ' + Math.ceil((h + GAP) / ROW) : '';
  }
  function pack() { Array.prototype.forEach.call(grid.children, span); }
  if (window.ResizeObserver) {
    var ro = new ResizeObserver(function (es) { es.forEach(function (e) { span(e.target); }); });
    Array.prototype.forEach.call(grid.children, function (c) { ro.observe(c); });
    new MutationObserver(function (ms) {
      ms.forEach(function (m) { Array.prototype.forEach.call(m.addedNodes, function (n) { if (n.nodeType === 1) ro.observe(n); }); });
    }).observe(grid, { childList: true });
    grid.classList.add('ov3-masonry');
    pack();
  }

  function readJSON(id, fallback) {
    try {
      var v = JSON.parse(document.getElementById(id).textContent);
      if (typeof v === 'string') v = JSON.parse(v);
      return v == null ? fallback : v;
    } catch (e) { return fallback; }
  }
  var layout = readJSON('ov3-layout', { hidden: [], order: [], sizes: {} });
  var widgets = readJSON('ov3-widgets', []);
  if (!Array.isArray(widgets)) widgets = [];
  var titles = {}, slot = {}; widgets.forEach(function (w) { titles[w.id] = w.title; slot[w.id] = w.slot; });
  layout.hidden = layout.hidden || []; layout.sizes = layout.sizes || {};

  function toast(msg) {
    var t = document.getElementById('ov3-toast');
    if (!t) { t = document.createElement('div'); t.id = 'ov3-toast'; t.className = 'ov3-toast'; document.body.appendChild(t); }
    t.textContent = msg; t.classList.add('show');
    clearTimeout(t._h); t._h = setTimeout(function () { t.classList.remove('show'); }, 1800);
  }
  function cells() { return Array.prototype.filter.call(grid.children, function (e) { return e.classList.contains('ov3-w'); }); }
  function collect() {
    var sizes = {};
    // every widget's footprint, explicitly: the server knows the per-widget
    // defaults (headline widgets are full width) and drops what matches them
    cells().forEach(function (w) {
      sizes[w.getAttribute('data-w')] = { w: parseInt(w.getAttribute('data-cw') || '1', 10), h: parseInt(w.getAttribute('data-ch') || '2', 10) };
    });
    return { preset: 'custom', hidden: layout.hidden.slice(), order: cells().map(function (e) { return e.getAttribute('data-w'); }), sizes: sizes };
  }
  var saveTimer = null;
  function save(body, msg) {
    return fetch('/overview/layout', { method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
      .then(function (r) { if (!r.ok) throw new Error('save failed'); return r.json(); })
      .then(function (j) { if (msg) toast(msg); return j; })
      .catch(function () { toast('Could not save layout'); });
  }
  function saveSoon(msg) { clearTimeout(saveTimer); saveTimer = setTimeout(function () { layout = Object.assign(collect(), { hidden: layout.hidden }); save(layout, msg); }, 350); }
  function markCustom() {
    var btn = document.getElementById('ov3-cust-btn');
    if (btn && !btn.querySelector('.n')) { var n = document.createElement('span'); n.className = 'n'; n.textContent = 'custom'; btn.appendChild(n); }
    else if (btn) btn.querySelector('.n').textContent = 'custom';
    Array.prototype.forEach.call(document.querySelectorAll('#ov3-pop [data-preset]'), function (b) { b.classList.remove('on'); });
  }
  function refreshHiddenMenu() {
    var host = document.getElementById('ov3-hidden-list');
    if (host) {
      host.innerHTML = '';
      if (!layout.hidden.length) host.innerHTML = '<div class="ov3-pop-none">Nothing hidden. Use the × on a card to hide it.</div>';
      layout.hidden.forEach(function (id) {
        var b = document.createElement('button'); b.type = 'button'; b.className = 'ov3-pop-item'; b.setAttribute('data-show', id);
        b.innerHTML = '<b></b><span>Show again</span>'; b.querySelector('b').textContent = titles[id] || id;
        host.appendChild(b);
      });
    }
    var row = document.getElementById('ov3-tray-row');
    if (row) {
      row.innerHTML = '';
      if (!layout.hidden.length) row.innerHTML = '<span class="none">Every widget is on the page.</span>';
      layout.hidden.forEach(function (id) {
        var b = document.createElement('button'); b.type = 'button'; b.setAttribute('data-show', id); b.textContent = '+ ' + (titles[id] || id);
        row.appendChild(b);
      });
    }
  }

  // ── per-widget tools (edit mode) + the always-there × ──
  function svg(d) { return '<svg viewBox="0 0 24 24"><path d="' + d + '"/></svg>'; }
  function syncTools(w) {
    var t = w.querySelector('.ov3-tools'); if (!t) return;
    var cw = w.getAttribute('data-cw') || '1', ch = w.getAttribute('data-ch') || '2';
    t.querySelector('[data-cw="1"]').classList.toggle('on', cw === '1');
    t.querySelector('[data-cw="2"]').classList.toggle('on', cw === '2');
    Array.prototype.forEach.call(t.querySelectorAll('[data-ch]'), function (b) { b.classList.toggle('on', b.getAttribute('data-ch') === ch); });
  }
  function hideWidget(w, id) {
    if (layout.hidden.indexOf(id) < 0) layout.hidden.push(id);
    w.remove(); markCustom(); refreshHiddenMenu();
    saveSoon((titles[id] || 'Widget') + ' hidden');
  }
  cells().forEach(function (w) {
    var id = w.getAttribute('data-w');
    var x = document.createElement('button');
    x.type = 'button'; x.className = 'ov3-hide'; x.title = 'Hide ' + (titles[id] || 'widget'); x.setAttribute('aria-label', x.title);
    x.innerHTML = svg('M6 6l12 12M18 6L6 18');
    x.addEventListener('click', function (e) { e.preventDefault(); e.stopPropagation(); hideWidget(w, id); });
    var head = w.querySelector('.ov3-ch');
    if (head) head.appendChild(x); else w.appendChild(x);
    var tools = document.createElement('div'); tools.className = 'ov3-tools';
    tools.innerHTML =
      '<button type="button" class="grab" title="Drag to move" draggable="true">' + svg('M9 5h.01M15 5h.01M9 12h.01M15 12h.01M9 19h.01M15 19h.01') + '</button><span class="sep"></span>' +
      '<button type="button" data-cw="1" title="Half width">' + svg('M3 5h8v14H3zM13 5h8v14h-8') + '</button>' +
      '<button type="button" data-cw="2" title="Full width">' + svg('M3 5h18v14H3z') + '</button><span class="sep"></span>' +
      '<button type="button" data-ch="1" title="Compact (scrolls)">S</button>' +
      '<button type="button" data-ch="2" title="Normal height">M</button>' +
      '<button type="button" data-ch="3" title="Tall">L</button><span class="sep"></span>' +
      '<button type="button" class="x" title="Hide">' + svg('M6 6l12 12M18 6L6 18') + '</button>';
    w.appendChild(tools);
    syncTools(w);
    tools.addEventListener('click', function (e) {
      var b = e.target.closest('button'); if (!b) return;
      e.preventDefault(); e.stopPropagation();
      if (b.classList.contains('x')) { hideWidget(w, id); return; }
      if (b.hasAttribute('data-cw')) w.setAttribute('data-cw', b.getAttribute('data-cw'));
      if (b.hasAttribute('data-ch')) w.setAttribute('data-ch', b.getAttribute('data-ch'));
      syncTools(w); markCustom(); saveSoon('Layout saved');
    });
  });

  // ── edit mode ──
  function setEditing(on) {
    root.classList.toggle('ov3-editing', on);
    cells().forEach(function (w) { w.setAttribute('draggable', on ? 'true' : 'false'); });
    if (on) { refreshHiddenMenu(); var bar = document.getElementById('ov3-editbar'); if (bar) bar.scrollIntoView({ block: 'nearest', behavior: 'smooth' }); }
  }
  document.getElementById('ov3-edit-done').addEventListener('click', function () { setEditing(false); toast('Layout saved'); });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && root.classList.contains('ov3-editing')) setEditing(false); });
  var tray = document.getElementById('ov3-tray');
  tray.addEventListener('click', function (e) {
    var b = e.target.closest('[data-show]'); if (!b) return;
    var id = b.getAttribute('data-show');
    layout.hidden = layout.hidden.filter(function (x) { return x !== id; });
    var body = Object.assign(collect(), { hidden: layout.hidden }); body.order.push(id);
    save(body).then(function () { reloadOverview(true); });
  });

  // ── drag to reorder within the grid (a drop slot follows the pointer) ──
  var dragging = null, marker = document.createElement('div'); marker.className = 'ov3-drop';
  grid.addEventListener('dragstart', function (e) {
    var w = e.target.closest && e.target.closest('.ov3-w');
    if (!w || !root.classList.contains('ov3-editing')) { e.preventDefault(); return; }
    dragging = w; w.classList.add('dragging');
    marker.classList.toggle('wide', w.getAttribute('data-cw') === '2');
    try { e.dataTransfer.setData('text/plain', w.getAttribute('data-w')); e.dataTransfer.effectAllowed = 'move'; } catch (err) {}
  });
  grid.addEventListener('dragend', function () {
    if (dragging) dragging.classList.remove('dragging');
    dragging = null; marker.remove();
  });
  grid.addEventListener('dragover', function (e) {
    if (!dragging) return;
    e.preventDefault(); e.dataTransfer.dropEffect = 'move';
    // nearest widget by centre distance; drop before it when the pointer is in its
    // first half (x for side-by-side cards, y for stacked ones)
    // the widget under the pointer wins; otherwise the nearest by centre
    var best = null, bd = Infinity, before = false;
    cells().forEach(function (w) {
      if (w === dragging) return;
      var r = w.getBoundingClientRect(), cx = r.left + r.width / 2, cy = r.top + r.height / 2;
      var inside = e.clientX >= r.left && e.clientX <= r.right && e.clientY >= r.top && e.clientY <= r.bottom;
      var d = inside ? -1 : Math.hypot(e.clientX - cx, e.clientY - cy);
      if (d < bd) { bd = d; best = w; before = (r.width < grid.clientWidth * 0.75) ? (e.clientX < cx) : (e.clientY < cy); }
    });
    if (!best) { tray.before(marker); return; }
    if (before) best.before(marker); else best.after(marker);
  });
  grid.addEventListener('drop', function (e) {
    if (!dragging) return;
    e.preventDefault();
    if (marker.parentNode) marker.replaceWith(dragging); 
    markCustom(); saveSoon('Layout saved');
  });

  // ── customize popover (views, hidden, edit, reset) ──
  var cust = document.getElementById('ov3-cust'), btn = document.getElementById('ov3-cust-btn'), pop = document.getElementById('ov3-pop');
  function reloadOverview(keepEditing) {
    var url = location.pathname + location.search;
    if (keepEditing) try { sessionStorage.setItem('ov3-editing', '1'); } catch (err) {}
    if (!window.htmx || !document.querySelector('main')) { location.reload(); return; }
    if (pop) { pop.hidden = true; btn.setAttribute('aria-expanded', 'false'); }
    try {
      var p = window.htmx.ajax('GET', url, { source: document.body, target: 'main', swap: 'outerHTML', select: 'main' });
      if (p && p.catch) p.catch(function () { location.reload(); });
    } catch (e) { location.reload(); }
  }
  try { if (sessionStorage.getItem('ov3-editing') === '1') { sessionStorage.removeItem('ov3-editing'); setEditing(true); } } catch (err) {}
  if (btn && pop) {
    btn.addEventListener('click', function (e) { e.stopPropagation(); pop.hidden = !pop.hidden; btn.setAttribute('aria-expanded', String(!pop.hidden)); });
    if (!window.__ov3PopBound) {
      window.__ov3PopBound = true;
      document.addEventListener('click', function (e) {
        var p = document.getElementById('ov3-pop'), c = document.getElementById('ov3-cust');
        if (p && !p.hidden && c && !c.contains(e.target)) { p.hidden = true; document.getElementById('ov3-cust-btn').setAttribute('aria-expanded', 'false'); }
      });
      document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { var p = document.getElementById('ov3-pop'); if (p) p.hidden = true; } });
    }
    function switching(t) {
      Array.prototype.forEach.call(pop.querySelectorAll('button'), function (b) { b.disabled = true; });
      Array.prototype.forEach.call(pop.querySelectorAll('.ov3-pop-item.on'), function (b) { b.classList.remove('on'); });
      t.disabled = true; t.classList.add('on', 'busy');
      btn.classList.add('busy');
      root.classList.add('ov3-switching');
    }
    pop.addEventListener('click', function (e) {
      var t = e.target.closest('button'); if (!t || t.disabled) return;
      if (t.id === 'ov3-edit') { pop.hidden = true; btn.setAttribute('aria-expanded', 'false'); setEditing(true); return; }
      if (t.hasAttribute('data-preset')) {
        if (t.classList.contains('on')) { pop.hidden = true; btn.setAttribute('aria-expanded', 'false'); return; }
        switching(t);
        save({ preset: t.getAttribute('data-preset') }).then(function () { reloadOverview(false); });
      } else if (t.hasAttribute('data-show')) {
        var id = t.getAttribute('data-show');
        layout.hidden = layout.hidden.filter(function (x) { return x !== id; });
        var body = Object.assign(collect(), { hidden: layout.hidden }); body.order.push(id);
        switching(t);
        save(body).then(function () { reloadOverview(false); });
      } else if (t.id === 'ov3-reset') {
        switching(t);
        fetch('/overview/layout/reset', { method: 'POST', credentials: 'same-origin' }).then(function () { reloadOverview(false); }, function () { reloadOverview(false); });
      }
    });
  }
  refreshHiddenMenu();
})();
