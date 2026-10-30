// Venues and Groups lists (V1 / G1 "Fleet's twin", 30 Oct 2026). Every row is on the page,
// so the rail, product tabs, quick chips, search and sort work in place:
//   rail   [data-clf="key"]   one saved view at a time ("" = all)
//   chips  [data-clc="key"]   any number, all must match
//   tabs   [data-clp="class"] one product ("" = all)
//   search [data-clq]         against the row's data-text
//   sort   [data-cls="key"]   data-s-<key> on each row; "name" sorts A→Z, the rest as set by data-dir
// A row carries data-flags (space-separated keys), data-classes and data-text.
(function () {
  function init(root) {
    if (!root || root.dataset.clReady) return;
    root.dataset.clReady = '1';
    var tbody = root.querySelector('tbody');
    if (!tbody) return;
    var rows = Array.prototype.slice.call(tbody.querySelectorAll('tr.ft-row'));
    var st = { view: '', chips: {}, prod: '', q: '' };
    var one = root.dataset.one || 'item', many = root.dataset.many || 'items';
    var countEl = root.querySelector('[data-cl-count]'), titleEl = root.querySelector('[data-cl-title]');
    var none = root.querySelector('[data-cl-none]');
    function has(r, k) { return (' ' + (r.dataset.flags || '') + ' ').indexOf(' ' + k + ' ') >= 0; }
    function apply() {
      var q = st.q.toLowerCase(), shown = 0;
      rows.forEach(function (r) {
        var ok = (!st.view || has(r, st.view)) &&
          (!st.prod || (' ' + (r.dataset.classes || '') + ' ').indexOf(' ' + st.prod + ' ') >= 0) &&
          (!q || (r.dataset.text || '').toLowerCase().indexOf(q) >= 0);
        for (var k in st.chips) if (ok && st.chips[k] && !has(r, k)) ok = false;
        // A row for one view only (Apps: a product's channel rows) stays out of the others.
        if (r.dataset.onlyView && r.dataset.onlyView !== st.view) ok = false;
        r.hidden = !ok;
        if (ok) shown++;
      });
      if (none) none.hidden = shown > 0;
      if (countEl) countEl.textContent = shown + ' ' + (shown === 1 ? one : many) + (shown === rows.length && countEl.dataset.extra ? countEl.dataset.extra : '');
      var clr = root.querySelector('[data-cl-clear]');
      if (clr) clr.hidden = !Object.keys(st.chips).some(function (k) { return st.chips[k]; });
    }
    root.addEventListener('click', function (e) {
      var b = e.target.closest('[data-clf]');
      if (b && root.contains(b)) {
        st.view = b.dataset.clf;
        root.querySelectorAll('[data-clf]').forEach(function (x) { x.classList.toggle('on', x === b); x.setAttribute('aria-pressed', x === b); });
        if (titleEl) titleEl.textContent = b.dataset.title || b.querySelector('.nm').textContent;
        apply(); return;
      }
      b = e.target.closest('[data-clc]');
      if (b) {
        var k = b.dataset.clc; st.chips[k] = !st.chips[k];
        b.classList.toggle('on', st.chips[k]); b.setAttribute('aria-pressed', st.chips[k]);
        apply(); return;
      }
      b = e.target.closest('[data-cl-clear]');
      if (b) {
        st.chips = {};
        root.querySelectorAll('[data-clc]').forEach(function (x) { x.classList.remove('on'); x.setAttribute('aria-pressed', 'false'); });
        apply(); return;
      }
      b = e.target.closest('[data-clp]');
      if (b) {
        st.prod = b.dataset.clp;
        root.querySelectorAll('[data-clp]').forEach(function (x) { x.classList.toggle('on', x === b); x.setAttribute('aria-selected', x === b); });
        apply(); return;
      }
      b = e.target.closest('[data-cl-sortbtn]');
      if (b) {
        var m = root.querySelector('[data-cl-sortmenu]');
        m.hidden = !m.hidden; b.setAttribute('aria-expanded', !m.hidden); return;
      }
      b = e.target.closest('[data-cls]');
      if (b) { sortBy(b.dataset.cls, b.dataset.dir || 'asc', b.textContent.trim()); return; }
      // A click anywhere else on a row opens it, like Fleet's rows.
      var tr = e.target.closest('tr.ft-row[data-href]');
      if (tr && !e.target.closest('a,button,summary,details,input,form')) location.href = tr.dataset.href;
    });
    function sortBy(key, dir, label) {
      var sign = dir === 'desc' ? -1 : 1;
      rows.sort(function (a, b) {
        var x = a.dataset['s' + key[0].toUpperCase() + key.slice(1)], y = b.dataset['s' + key[0].toUpperCase() + key.slice(1)];
        if (key === 'name') return sign * x.localeCompare(y, undefined, { sensitivity: 'base' });
        return sign * (parseFloat(x) - parseFloat(y)) || a.dataset.sName.localeCompare(b.dataset.sName, undefined, { sensitivity: 'base' });
      });
      rows.forEach(function (r) { tbody.appendChild(r); });
      var cur = root.querySelector('[data-cl-sortcur]');
      if (cur) cur.textContent = '· ' + label;
      root.querySelectorAll('[data-cls]').forEach(function (x) { var on = x.dataset.cls === key && (x.dataset.dir || 'asc') === dir; x.classList.toggle('active', on); x.setAttribute('aria-checked', on); });
      var m = root.querySelector('[data-cl-sortmenu]'); if (m) m.hidden = true;
      var sb = root.querySelector('[data-cl-sortbtn]'); if (sb) sb.setAttribute('aria-expanded', 'false');
    }
    var qi = root.querySelector('[data-clq]');
    if (qi) qi.addEventListener('input', function () { st.q = qi.value.trim(); apply(); });
    document.addEventListener('click', function (e) {
      var m = root.querySelector('[data-cl-sortmenu]');
      if (m && !m.hidden && !e.target.closest('[data-cl-sortmenu],[data-cl-sortbtn]')) m.hidden = true;
    });
    apply();
  }
  function boot() { document.querySelectorAll('[data-cl]').forEach(init); }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot); else boot();
  document.addEventListener('htmx:afterSettle', boot);

  // Delete / rename dialogs on the Groups list: the delete button stays off until the
  // group's name is typed exactly. Their forms are not boosted: htmx reads a boosted
  // form's action once, before this sets it.
  document.addEventListener('click', function (e) {
    var b = e.target.closest('[data-cl-dlg]');
    if (!b) return;
    e.preventDefault();
    var d = document.getElementById(b.dataset.clDlg);
    if (!d) return;
    var det = b.closest('details'); if (det) det.open = false;
    d.querySelector('form').action = b.dataset.action;
    d.querySelectorAll('[data-cl-name]').forEach(function (x) { x.textContent = b.dataset.name; });
    var inp = d.querySelector('input[type=text]');
    if (inp) {
      inp.value = d.dataset.prefill ? b.dataset.name : '';
      inp.dataset.want = b.dataset.name;
    }
    var go = d.querySelector('[data-cl-go]');
    if (go && d.dataset.confirm) go.disabled = true;
    d.showModal();
    if (inp) { inp.focus(); inp.select(); }
  });
  document.addEventListener('input', function (e) {
    var d = e.target.closest('dialog.cl-dlg');
    if (!d || !d.dataset.confirm) return;
    var go = d.querySelector('[data-cl-go]');
    if (go) go.disabled = e.target.value.trim() !== e.target.dataset.want;
  });
  document.addEventListener('click', function (e) {
    var c = e.target.closest('[data-cl-cancel]');
    if (c) { e.preventDefault(); c.closest('dialog').close(); }
  });
})();
