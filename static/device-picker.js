/* Shared two-pane device picker. One picker per page.
 *
 * Markup contract:
 *   #dp-rows[data-endpoint]   container the candidate rows load into (HTMX target)
 *   #dp-q                     search input  (oninput="dpReloadDebounced()")
 *   .dp-pill                  quick-filter pills, single-select; each carries the
 *                             params it sets as data-status / data-battery / data-scope
 *   .dp-row[data-serial,...]  a candidate row with a .dp-add button (onclick="dpAdd(serial)")
 *   #dp-basket                staging basket (rendered here)
 *   #dp-serials               hidden field the form submits (newline-separated)
 *   [data-dp-count]           elements that show the staged count
 *   [data-dp-submit]          submit button(s) disabled while empty
 *   [data-dp-moves]           "N will move" banner (restaurant flow), shown when >0
 */
(function () {
  var staged = []; // {serial, batt, bclass, move}
  var t;

  function field() { return document.getElementById('dp-serials'); }
  function rowsEl() { return document.getElementById('dp-rows'); }
  function basketEl() { return document.getElementById('dp-basket'); }
  function esc(s) { return (window.CSS && CSS.escape) ? CSS.escape(s) : String(s).replace(/"/g, '\\"'); }

  function chip(it) { return '<span class="dp-bchip ' + it.bclass + '">' + it.batt + '%</span>'; }

  function renderBasket() {
    var b = basketEl();
    if (b) {
      if (!staged.length) {
        b.innerHTML = '<div class="dp-bk-empty">No devices selected yet.<br>Pick from the left.</div>';
      } else {
        b.innerHTML = staged.map(function (it) {
          var move = it.move ? '<div class="dp-bk-move">↦ moving from ' + it.move + '</div>' : '';
          return '<div class="dp-bk-row" data-serial="' + it.serial + '">' + chip(it) +
            '<span class="dp-bk-id"><span class="dp-serial mono">' + it.serial + '</span>' + move + '</span>' +
            '<button type="button" class="dp-x" onclick="dpRemove(\'' + it.serial + '\')" aria-label="Remove">&times;</button></div>';
        }).join('');
      }
    }
    var f = field();
    if (f) f.value = staged.map(function (it) { return it.serial; }).join('\n');
    document.querySelectorAll('[data-dp-count]').forEach(function (el) { el.textContent = staged.length; });
    var moves = staged.filter(function (it) { return it.move; }).length;
    document.querySelectorAll('[data-dp-moves]').forEach(function (el) {
      el.style.display = moves ? '' : 'none';
      var n = el.querySelector('[data-dp-moves-n]'); if (n) n.textContent = moves;
    });
    document.querySelectorAll('[data-dp-submit]').forEach(function (el) { el.disabled = staged.length === 0; });
    syncRows();
  }

  function syncRows() {
    var set = {}; staged.forEach(function (it) { set[it.serial] = true; });
    document.querySelectorAll('.dp-row').forEach(function (r) {
      var s = r.getAttribute('data-serial');
      var add = r.querySelector('.dp-add');
      if (set[s]) { r.classList.add('staged'); if (add) { add.textContent = '✓'; add.classList.add('done'); add.disabled = true; } }
      else { r.classList.remove('staged'); if (add) { add.textContent = '+'; add.classList.remove('done'); add.disabled = false; } }
    });
  }

  window.dpAdd = function (serial) {
    if (staged.some(function (it) { return it.serial === serial; })) return;
    var row = document.querySelector('.dp-row[data-serial="' + esc(serial) + '"]');
    var it = { serial: serial, batt: '?', bclass: 'battery-ok', move: '' };
    if (row) {
      it.batt = row.getAttribute('data-batt') || '?';
      it.bclass = row.getAttribute('data-bclass') || 'battery-ok';
      it.move = row.getAttribute('data-move') || '';
    }
    staged.push(it);
    renderBasket();
  };

  window.dpRemove = function (serial) {
    staged = staged.filter(function (it) { return it.serial !== serial; });
    renderBasket();
  };

  window.dpClear = function () { staged = []; renderBasket(); };

  window.dpReload = function () {
    var rows = rowsEl(); if (!rows) return;
    var ep = rows.getAttribute('data-endpoint'); if (!ep) return;
    var sel = document.querySelector('.dp-pill.sel');
    var q = (document.getElementById('dp-q') || {}).value || '';
    var params = new URLSearchParams();
    if (q) params.set('q', q);
    if (sel) { ['status', 'battery', 'scope'].forEach(function (k) { var v = sel.getAttribute('data-' + k); if (v) params.set(k, v); }); }
    if (window.htmx) htmx.ajax('GET', ep + '?' + params.toString(), { target: '#dp-rows', swap: 'innerHTML' });
  };

  window.dpReloadDebounced = function () { clearTimeout(t); t = setTimeout(window.dpReload, 250); };

  window.dpPill = function (btn) {
    document.querySelectorAll('.dp-pill').forEach(function (p) { p.classList.remove('sel'); });
    btn.classList.add('sel');
    window.dpReload();
  };

  // re-apply staged dimming whenever the candidate rows reload
  document.body.addEventListener('htmx:afterSettle', function (e) {
    if (e.detail && e.detail.target && e.detail.target.id === 'dp-rows') syncRows();
  });
  // The page this sits on is usually reached by a boosted navigation, where
  // DOMContentLoaded has long since fired — render the empty basket (and the
  // disabled submit) straight away in that case.
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', renderBasket);
  else renderBasket();
})();
