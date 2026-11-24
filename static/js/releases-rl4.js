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
    [prod, ver, dev, url, q].forEach(function (e) { if (e) e.addEventListener('input', function () { opts(); sum(); }); });
    prod.addEventListener('change', function () { opts(); sum(); });
    dev.addEventListener('change', sum);
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

/* The ↗ in the release preview opens the release page as a slide-over sheet over the list, so
   the list is never left (24 Nov 2026, demo A picked). ✕, Esc or a click outside closes it; the
   ⤢ in its header opens the same release as a full page. */
(function () {
  var sheet = null;
  function close() {
    if (!sheet) return;
    var s = sheet; sheet = null;
    s.classList.remove('on');
    setTimeout(function () { s.remove(); }, 300);
    document.removeEventListener('keydown', onKey, true);
  }
  function onKey(e) { if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); close(); } }
  function runScripts(root) {
    root.querySelectorAll('script').forEach(function (o) {
      var n = document.createElement('script');
      Array.prototype.forEach.call(o.attributes, function (a) { n.setAttribute(a.name, a.value); });
      n.text = o.text; o.replaceWith(n);
    });
  }
  function open(href, label) {
    close();
    var s = document.createElement('div');
    s.className = 'rl4-sheet'; s.setAttribute('hx-boost', 'false');
    s.innerHTML = '<div class="rl4-sh-scrim"></div><section class="rl4-sh" role="dialog" aria-modal="true" aria-label="Release ' + label + '">' +
      '<header class="rl4-sh-h"><b>' + label + '</b><span class="sp"></span><a class="rl4-ib" href="' + href + '" title="Open as a page" aria-label="Open as a page"><svg viewBox="0 0 24 24"><path d="M15 3h6v6M9 21H3v-6M21 3l-7 7M3 21l7-7"/></svg></a><span class="rl4-sh-k">Esc</span><button type="button" class="rl4-ib" data-rl4-close aria-label="Close">✕</button></header>' +
      '<div class="rl4-sh-b"><p class="muted" style="padding:20px">Loading…</p></div></section>';
    document.body.appendChild(s); sheet = s;
    requestAnimationFrame(function () { s.classList.add('on'); });
    s.addEventListener('click', function (e) { if (e.target.closest('[data-rl4-close]') || e.target.classList.contains('rl4-sh-scrim')) close(); });
    document.addEventListener('keydown', onKey, true);
    fetch(href, { credentials: 'same-origin', headers: { 'X-Requested-With': 'rl4-sheet' } }).then(function (r) { if (!r.ok) throw new Error(r.status); return r.text(); }).then(function (t) {
      if (sheet !== s) return;
      var doc = new DOMParser().parseFromString(t, 'text/html'), main = doc.querySelector('main');
      if (!main) throw new Error('no page');
      main.querySelectorAll('template#page-island, .cc-top, .sr-only').forEach(function (x) { if (x.tagName === 'TEMPLATE' || x.classList.contains('cc-top')) x.remove(); });
      var body = s.querySelector('.rl4-sh-b'); body.innerHTML = main.innerHTML; runScripts(body);
      if (window.htmx) window.htmx.process(body);
    }).catch(function () {
      var body = s && s.querySelector('.rl4-sh-b'); if (body) body.innerHTML = '<p style="padding:20px">Could not load the release. <a href="' + href + '">Open it as a page</a></p>';
    });
  }
  document.addEventListener('click', function (e) {
    var a = e.target.closest && e.target.closest('a.rl4-ib[href^="/releases/"]');
    if (!a || a.closest('.rl4-sheet') || e.metaKey || e.ctrlKey || e.shiftKey || e.button) return;
    e.preventDefault(); e.stopPropagation();
    var row = document.querySelector('#rl4-pv h2'); open(a.getAttribute('href'), row ? row.textContent.trim() : 'Release');
  }, true);
})();
