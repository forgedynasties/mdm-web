// Releases RL4 + AP1 (Nov 2026). Loaded by templates/releases.html.
//  - the New release dialog (open / close, live summary, source-build list per product)
//  - package forms ([data-rl4-pkgform]: the dialog and the in-place Add package): Inspect reads
//    the package header by URL, the URL goes out base64-encoded (an upstream WAF refuses raw
//    internal addresses), the type toggle shows the source build for incrementals
//  - the product the page was opened for (?product=) is picked in the rail once the list is ready
function rl4NewOpen() {
  var m = document.getElementById('create-pkg-modal'); if (!m) return;
  m.classList.add('modal-open');
  var v = m.querySelector('[data-rl4-ver]'); if (v) setTimeout(function () { v.focus(); }, 30);
}
function rl4NewClose() {
  var m = document.getElementById('create-pkg-modal'); if (m) m.classList.remove('modal-open');
}
(function () {
  function b64(s) { return btoa(unescape(encodeURIComponent(s))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, ''); }
  function size(n) { return n > 1073741824 ? (n / 1073741824).toFixed(2) + ' GB' : Math.max(1, Math.round(n / 1048576)) + ' MB'; }
  function esc(s) { var d = document.createElement('div'); d.textContent = s == null ? '' : String(s); return d.innerHTML; }

  function wire(form) {
    if (form.dataset.rl4Ready) return;
    form.dataset.rl4Ready = '1';
    var url = form.querySelector('[data-rl4-url]'), out = form.querySelector('[data-rl4-out]');
    var src = form.querySelector('[data-rl4-src]'), srcSel = src && src.querySelector('select');
    var hidden = form.querySelector('input[name="update_url_b64"]');
    var state = { size: 0, type: '', wipe: false };
    form._rl4 = state;
    function type() { var r = form.querySelector('input[name="type"]:checked'); return r ? r.value : 'full'; }
    function sync() {
      var inc = type() === 'incremental';
      if (src) src.hidden = !inc;
      if (srcSel) srcSel.required = inc && !!(url && url.value);
      form.dispatchEvent(new Event('rl4:change'));
    }
    form.querySelectorAll('input[name="type"]').forEach(function (r) { r.addEventListener('change', sync); });
    if (url) url.addEventListener('input', function () {
      state.size = 0; state.type = ''; state.wipe = false;
      if (out) out.innerHTML = '<span class="hint">Inspect reads the file header, not the whole package.</span>';
      sync();
    });
    var btn = form.querySelector('[data-rl4-inspect]');
    if (btn) btn.addEventListener('click', function () {
      var u = (url.value || '').trim();
      if (!u) { out.innerHTML = '<span class="bad">Enter a package URL first.</span>'; return; }
      out.innerHTML = '<span class="hint">Reading package metadata…</span>';
      var body = new URLSearchParams(); body.set('update_url_b64', b64(u));
      fetch(form.dataset.inspect, { method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' }, body: body, credentials: 'same-origin' })
        .then(function (r) { return r.json(); })
        .then(function (d) {
          if (!d.ok) { out.innerHTML = '<span class="bad">✗ ' + esc(d.error || 'Could not read the package') + '</span>'; return; }
          state.size = d.size_bytes; state.type = d.type; state.wipe = !!d.wipe;
          var want = form.querySelector('input[name="type"][value="' + d.type + '"]');
          if (want && !want.disabled) { want.checked = true; sync(); }
          out.innerHTML = '<div class="rl4-tiles"><div class="rl4-tile"><b>' + esc(d.type) + '</b><span>type</span></div>' +
            (d.device ? '<div class="rl4-tile"><b>' + esc(d.device) + '</b><span>built for</span></div>' : '') +
            '<div class="rl4-tile"><b>' + size(d.size_bytes) + '</b><span>size</span></div></div>' +
            (d.wipe ? '<div class="rl4-wipe"><b>This package wipes the device.</b> Installing it erases apps, settings and data; it only goes to devices sent with Allow wipe.</div>'
              : '<div class="rl4-keep">Keeps device data.' + (d.type === 'incremental' ? ' Pick the build it updates from.' : '') + '</div>');
          form.dispatchEvent(new Event('rl4:change'));
        })
        .catch(function () { out.innerHTML = '<span class="bad">✗ Couldn’t reach the package.</span>'; });
    });
    form.addEventListener('submit', function () {
      [['[data-rl4-url]', 'update_url_b64', 'update_url'], ['[data-rl4-qfil]', 'qfil_url_b64', 'qfil_url']].forEach(function (p) {
        var i = form.querySelector(p[0]), h = form.querySelector('input[name="' + p[1] + '"]');
        if (!i || !h || !i.value) return;
        h.value = b64(i.value.trim());
        i.removeAttribute('name'); // only the encoded copy goes out
      });
    });
    sync();
  }

  // The QFIL form in the preview (its own form, not a package form).
  function wireQfil(form) {
    form.addEventListener('submit', function () {
      var i = form.querySelector('input[name="qfil_url"]'), h = form.querySelector('input[name="qfil_url_b64"]');
      if (!i || !h || !i.value) return;
      h.value = b64(i.value.trim());
      i.removeAttribute('name');
    });
  }

  // The dialog: summary tile and the source-build list for the chosen product.
  function wireDialog() {
    var form = document.getElementById('rl4-new'); if (!form || form.dataset.rl4Dlg) return;
    form.dataset.rl4Dlg = '1';
    var prod = form.querySelector('[data-rl4-prod]'), ver = form.querySelector('[data-rl4-ver]');
    var url = form.querySelector('[data-rl4-url]');
    var q = form.querySelector('[data-rl4-qfil]');
    var st = form.querySelector('[data-rl4-sumt]'), ss = form.querySelector('[data-rl4-sums]');
    var srcSel = form.querySelector('[data-rl4-srcsel]');
    function sum() {
      var v = (ver.value || '').trim();
      st.textContent = 'Create ' + (v || 'a release') + ' as a release';
      var parts = [];
      if (url.value) {
        var s = form._rl4 || {};
        var t = s.type || form.querySelector('input[name="type"]:checked').value;
        parts.push('1 package · ' + t + (s.size ? ' · ' + size(s.size) : '') + (s.wipe ? ' · wipes the device' : ''));
      } else parts.push('No package yet, add one later');
      if (q && q.value) parts.push('QFIL link attached');
      ss.textContent = parts.join(' · ');
    }
    function opts() {
      if (!srcSel) return;
      var p = prod.value, cur = srcSel.value, keep = false;
      srcSel.querySelectorAll('option[data-prod]').forEach(function (o) {
        var show = o.dataset.prod === p && o.value !== (ver.value || '').trim();
        o.hidden = !show; o.disabled = !show;
        if (show && o.value === cur) keep = true;
      });
      if (!keep) srcSel.value = '';
    }
    [prod, ver, url, q].forEach(function (e) { if (e) e.addEventListener('input', function () { opts(); sum(); }); });
    prod.addEventListener('change', function () { opts(); sum(); });
    form.addEventListener('rl4:change', sum);
    opts(); sum();
  }

  function boot() {
    document.querySelectorAll('[data-rl4-pkgform]').forEach(wire);
    var qf = document.getElementById('rl4-qfil-form'); if (qf && !qf.dataset.rl4Ready) { qf.dataset.rl4Ready = '1'; wireQfil(qf); }
    wireDialog();
    var page = document.querySelector('.rl4[data-prod]');
    if (page && page.dataset.prod && !page.dataset.prodDone) {
      var root = page.querySelector('[data-cl]');
      if (root && root.dataset.clReady) {
        page.dataset.prodDone = '1';
        var b = page.querySelector('[data-clp="' + page.dataset.prod + '"]'); if (b) b.click();
      }
    }
  }
  // On a phone the preview sits under the table: after a row is picked, land on it.
  function narrow() { return window.matchMedia('(max-width: 1100px)').matches; }
  document.addEventListener('click', function (e) {
    if (e.target.closest('.rl4 tr.ft-row') && narrow()) { try { sessionStorage.setItem('rl4go', '1'); } catch (x) {} }
  }, true);
  function landOnPreview() {
    var go = false; try { go = sessionStorage.getItem('rl4go') === '1'; sessionStorage.removeItem('rl4go'); } catch (x) {}
    var pv = document.getElementById('rl4-pv');
    if (go && pv && narrow()) setTimeout(function () { pv.scrollIntoView({ block: 'start' }); }, 60);
  }
  landOnPreview();
  document.addEventListener('htmx:afterSettle', landOnPreview);
  boot();
  document.addEventListener('DOMContentLoaded', function () { boot(); setTimeout(boot, 50); });
  document.addEventListener('htmx:afterSettle', function () { setTimeout(boot, 0); });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') rl4NewClose(); });
})();


// Choosing a release swaps only the preview pane (20 Nov 2026: "the whole page reloads"): the list
// keeps its scroll, filters and sort, and the URL follows so Back and Forward work. A click on a row,
// or on the version link inside it, takes this path; anything else (new tab, modifier keys) is left alone.
(function () {
  var pv = function () { return document.getElementById('rl4-pv'); };
  function mark(url) {
    var rows = document.querySelectorAll('tr.ft-row[data-href]');
    rows.forEach(function (r) { r.classList.toggle('sel', r.dataset.href === url || r.dataset.page === url); });
  }
  function open(url, push) {
    var cur = pv(); if (!cur || !window.fetch) return false;
    cur.style.opacity = '.55';
    fetch(url, { credentials: 'same-origin', headers: { 'X-Requested-With': 'fetch' } })
      .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.text(); })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, 'text/html');
        var next = doc.getElementById('rl4-pv'), now = pv();
        if (!next || !now) { location.href = url; return; }
        now.replaceWith(next);
        if (window.htmx) htmx.process(next);
        mark(url);
        if (push) { try { history.pushState({ rl4: 1 }, '', url); } catch (e) {} }
        var t = doc.querySelector('title'); if (t) document.title = t.textContent;
      })
      .catch(function () { location.href = url; });
    return true;
  }
  window.clRowOpen = function (tr) {
    if (!pv() || !tr.closest('.fleet-md')) return false;
    return open(tr.dataset.href, true);
  };
  document.addEventListener('click', function (e) {
    var a = e.target.closest && e.target.closest('tr.ft-row[data-href] a.cl-nm');
    if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || !pv()) return;
    e.preventDefault(); e.stopPropagation();
    open(a.getAttribute('href'), true);
  }, true);
  window.addEventListener('popstate', function (e) { if (pv() && (!e.state || e.state.rl4)) open(location.pathname + location.search, false); });
})();

/* The ⋯ menu in the release pane was cut off by the pane's own scrolling edge (24 Nov 2026).
   Opened, it is placed against the window instead, under the button and right-aligned to it. */
(function () {
  // The page's script is run again each time the page is opened in place; a second copy would double every handler.
  if (window.__rl4MenuFix) return; window.__rl4MenuFix = true;
  document.addEventListener('toggle', function (e) {
    var d = e.target; if (!d || !d.open || !d.classList || !d.classList.contains('rl4-more')) return;
    var m = d.querySelector('.cc-menu'), s = d.querySelector('summary'); if (!m || !s) return;
    m.style.position = 'fixed'; m.style.top = '0'; m.style.right = 'auto'; m.style.left = '0';
    var r = s.getBoundingClientRect(), w = m.offsetWidth, h = m.offsetHeight;
    var top = innerHeight - r.bottom - 8 >= h || r.top < h ? r.bottom + 6 : r.top - h - 6;
    m.style.top = Math.max(8, Math.min(top, innerHeight - h - 8)) + 'px';
    m.style.left = Math.max(8, Math.min(r.right - w, innerWidth - w - 8)) + 'px';
  }, true);
  addEventListener('scroll', function (e) {
    document.querySelectorAll('details.rl4-more[open]').forEach(function (d) { if (!d.contains(e.target)) d.open = false; });
  }, true);
})();

/* The release window (24 Nov 2026, demo A picked): the ↗ in the release preview opens the release as a
   slide-over window over the list, with Overview · Packages · Changes · Problems tabs and the release's
   actions in its header, so the separate release page is not needed (it redirects here). The Overview is
   the pane already on the page, enlarged; the other tabs are the release page's own cards, fetched from
   /releases/{id} and moved in. ✕, Esc or a click on the dimmed side closes it. */
(function () {
  // Run once per window: this script is executed again every time Releases is opened in place, and
  // each copy would open its own window.
  if (window.__rl4Sheet) return; window.__rl4Sheet = true;
  var sheet = null, TABS = [['ov', 'Overview'], ['pk', 'Packages'], ['ch', 'Changes'], ['pr', 'Problems']];
  function close() {
    if (!sheet) return;
    var s = sheet; sheet = null;
    s.classList.remove('on');
    setTimeout(function () { s.remove(); }, 300);
    document.removeEventListener('keydown', onKey, true);
  }
  function onKey(e) { if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); close(); } }
  function tabFromHash(h) {
    h = (h || '').replace('#', '').toLowerCase();
    return { packages: 'pk', problems: 'pr', changelog: 'ch', details: 'ch', changes: 'ch', overview: 'ov' }[h] || '';
  }
  function runScripts(root) {
    root.querySelectorAll('script').forEach(function (o) {
      if (o.src) { o.remove(); return; }
      var n = document.createElement('script'); n.text = o.text; o.replaceWith(n);
    });
  }
  function select(s, t) {
    s.querySelectorAll('.rl4-ps-tabs [data-t]').forEach(function (b) { b.classList.toggle('on', b.dataset.t === t); b.setAttribute('aria-selected', b.dataset.t === t); });
    s.querySelectorAll('.rl4-ps-pane').forEach(function (p) { p.hidden = p.dataset.p !== t; });
    try { sessionStorage.setItem('rl4tab', t); } catch (e) {}
  }
  function cardOf(main, test) {
    var cards = main.querySelectorAll('.cc-wrap > .cc-card');
    for (var i = 0; i < cards.length; i++) if (test(cards[i])) return cards[i];
    return null;
  }
  function fill(s, id, text) {
    var doc = new DOMParser().parseFromString(text, 'text/html'), main = doc.querySelector('main');
    if (!main) throw new Error('no page');
    var wrap = main.querySelector('.cc-wrap') || main;
    // header actions (Show devices, Update devices, Publish, flashing bundle, Delete)
    var acts = wrap.querySelector('.cc-top .cc-actions'), host = s.querySelector('.rl4-ps-acts');
    if (acts && host) { host.innerHTML = acts.innerHTML; }
    var draft = cardOf(wrap, function (c) { return /draft/i.test(c.textContent) && !c.querySelector('h2') && !c.id; });
    var pk = wrap.querySelector('#packages'), pr = wrap.querySelector('#problems');
    var ch = cardOf(wrap, function (c) { var h = c.querySelector('h2'); return h && /Release details|Changelog/.test(h.textContent); });
    function put(t, nodes) {
      var pane = s.querySelector('.rl4-ps-pane[data-p="' + t + '"]'); pane.innerHTML = '';
      nodes.forEach(function (n) { if (n) pane.appendChild(n); });
      if (!pane.children.length) pane.innerHTML = '<p class="muted" style="padding:12px">Nothing here for your role.</p>';
    }
    put('pk', [draft, pk]); put('ch', [ch]); put('pr', [pr]);
    window.RELEASE_PAGE = { release: id };
    if (typeof window.pkgVerify === 'undefined') {
      var sc = document.createElement('script'); sc.src = '/static/js/release_workspace.js'; document.head.appendChild(sc);
    }
    runScripts(s);
    if (window.htmx) window.htmx.process(s);
  }
  function open(pv, href, label, tab) {
    close();
    var id = parseInt((href.match(/\/releases\/(\d+)/) || [])[1], 10);
    var root = document.querySelector('.rl4') || document.body;
    var s = document.createElement('div');
    s.className = 'rl4-slide';
    s.innerHTML = '<div class="rl4-ps-scrim"></div><section class="rl4-ps" role="dialog" aria-modal="true" aria-label="Release ' + label + '">' +
      '<header class="rl4-ps-h"><div class="rl4-ps-r"><b>' + label + '</b><span class="rl4-ps-tg"></span><span class="sp"></span><div class="rl4-ps-acts"></div><span class="rl4-ps-k">Esc</span><button type="button" class="rl4-ib" data-rl4-close aria-label="Close">\u2715</button></div>' +
      '<nav class="rl4-ps-tabs" role="tablist">' + TABS.map(function (t) { return '<button type="button" role="tab" data-t="' + t[0] + '">' + t[1] + '</button>'; }).join('') + '</nav></header>' +
      '<div class="rl4-ps-b">' + TABS.map(function (t) { return '<div class="rl4-ps-pane" data-p="' + t[0] + '" hidden></div>'; }).join('') + '</div></section>';
    // Overview: the pane that is already on the page, enlarged
    var ov = s.querySelector('.rl4-ps-pane[data-p="ov"]'), big = document.createElement('div'); big.className = 'rl4-pv rl4-pv-big';
    Array.prototype.forEach.call(pv.children, function (c) {
      var n = c.cloneNode(true); n.querySelectorAll('[id]').forEach(function (x) { x.removeAttribute('id'); }); big.appendChild(n);
    });
    big.querySelectorAll('a.rl4-ib[href^="/releases/"], .rl4-acts').forEach(function (a) { a.remove(); });
    var tags = big.querySelector('.rl4-ph'); if (tags) { s.querySelector('.rl4-ps-tg').innerHTML = Array.prototype.map.call(tags.querySelectorAll('.cc-tag'), function (t) { return t.outerHTML; }).join(' '); var h2 = tags.querySelector('h2'); if (h2) h2.remove(); tags.remove(); }
    ov.appendChild(big);
    ['pk', 'ch', 'pr'].forEach(function (t) { s.querySelector('.rl4-ps-pane[data-p="' + t + '"]').innerHTML = '<p class="muted" style="padding:16px">Loading…</p>'; });
    root.appendChild(s); sheet = s;
    var saved = ''; try { saved = sessionStorage.getItem('rl4tab') || ''; } catch (e) {}
    select(s, tab || tabFromHash(location.hash) || saved || 'ov');
    void s.offsetWidth; setTimeout(function () { s.classList.add('on'); }, 20);
    s.addEventListener('click', function (e) {
      if (e.target.closest('[data-rl4-close]') || e.target.classList.contains('rl4-ps-scrim')) close();
      var t = e.target.closest('.rl4-ps-tabs [data-t]'); if (t) select(s, t.dataset.t);
    });
    document.addEventListener('keydown', onKey, true);
    fetch('/releases/' + id, { credentials: 'same-origin', headers: { 'X-Requested-With': 'rl4-sheet' } })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.text(); })
      .then(function (t) { if (sheet === s) fill(s, id, t); })
      .catch(function () { if (sheet === s) ['pk', 'ch', 'pr'].forEach(function (k) { s.querySelector('.rl4-ps-pane[data-p="' + k + '"]').innerHTML = '<p style="padding:16px">Could not load this tab. Reload the page and try again.</p>'; }); });
  }
  function openFromPane(tab) {
    var pv = document.getElementById('rl4-pv'); if (!pv) return false;
    var a = pv.querySelector('a.rl4-ib[href^="/releases/"]'); if (!a) return false;
    var h = pv.querySelector('h2');
    open(pv, a.getAttribute('href'), h ? h.textContent.trim() : 'Release', tab);
    return true;
  }
  document.addEventListener('click', function (e) {
    var a = e.target.closest && e.target.closest('a.rl4-ib[href^="/releases/"]');
    if (!a || a.closest('.rl4-slide') || e.metaKey || e.ctrlKey || e.shiftKey || e.button) return;
    if (!document.getElementById('rl4-pv')) return;
    e.preventDefault(); e.stopPropagation(); openFromPane('');
  }, true);
  // ?win= : the old release page address (and a saved form coming back to it) lands here with the window open
  function auto() {
    var q = new URLSearchParams(location.search); if (!q.has('win') || !document.getElementById('rl4-pv')) return;
    var w = q.get('win'); q.delete('win');
    history.replaceState(history.state, '', location.pathname + (q.toString() ? '?' + q.toString() : '') + location.hash);
    openFromPane({ packages: 'pk', problems: 'pr', changes: 'ch', overview: 'ov' }[w] || '');
  }
  auto();
  document.addEventListener('htmx:afterSettle', function () { setTimeout(auto, 0); });
})();
