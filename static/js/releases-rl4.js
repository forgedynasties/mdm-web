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
    var dev = form.querySelector('[data-rl4-dev]'), url = form.querySelector('[data-rl4-url]');
    var q = form.querySelector('[data-rl4-qfil]');
    var st = form.querySelector('[data-rl4-sumt]'), ss = form.querySelector('[data-rl4-sums]');
    var srcSel = form.querySelector('[data-rl4-srcsel]');
    function sum() {
      var v = (ver.value || '').trim();
      st.textContent = 'Create ' + (v || 'a release') + ' as a ' + (dev.checked ? 'dev build' : 'release');
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
