// Apps library, Library A (19 Nov 2026): the preview pane follows the picked row, and the
// AIO apps | Other apps pills choose which group's rail entries show. Filtering, searching
// and sorting are coll_list.js's; this only reacts to what it left visible.
(function () {
  function init(page) {
    if (!page || page.dataset.al1) return;
    page.dataset.al1 = '1';
    var tbody = page.querySelector('.al1-t tbody');
    if (!tbody) return;
    var panes = page.querySelectorAll('.al1-p'), empty = page.querySelector('[data-al1-empty]');
    var head = page.querySelector('[data-al1-rh]');
    var sel = '';
    function rows() { return Array.prototype.slice.call(tbody.querySelectorAll('tr.al1-row')); }
    function visible() { return rows().filter(function (r) { return !r.hidden; }); }
    function pick(key) {
      sel = key || '';
      var any = false;
      rows().forEach(function (r) { r.classList.toggle('on', r.dataset.key === sel && !r.hidden); });
      panes.forEach(function (p) { var on = p.dataset.for === sel; p.hidden = !on; if (on) any = true; });
      if (empty) empty.hidden = any;
    }
    function settle() {
      var v = visible();
      if (!v.some(function (r) { return r.dataset.key === sel; })) pick(v[0] ? v[0].dataset.key : '');
      else pick(sel);
    }
    function group(g) {
      page.querySelectorAll('.al1-rail [data-grp]').forEach(function (b) { b.hidden = b.dataset.grp !== g; });
      page.querySelectorAll('.ovb-seg [data-grp]').forEach(function (b) { var on = b.dataset.grp === g; b.classList.toggle('on', on); b.setAttribute('aria-pressed', on); });
      if (head) head.textContent = g === 'other' ? 'Makers' : 'By product';
    }
    page.addEventListener('click', function (e) {
      var b = e.target.closest('[data-clf]');
      if (b) {
        var f = b.dataset.clf, g = b.dataset.grp;
        setTimeout(function () {
          if (g) group(g);
          page.querySelectorAll('[data-clf]').forEach(function (x) { var on = x.dataset.clf === f; x.classList.toggle('on', on); x.setAttribute('aria-pressed', on); });
          settle();
        }, 0);
        return;
      }
      var tr = e.target.closest('tr.al1-row');
      if (tr && !e.target.closest('a,button,input,form')) { pick(tr.dataset.key); return; }
      if (e.target.closest('[data-cls]')) setTimeout(settle, 0);
    });
    page.addEventListener('dblclick', function (e) {
      var tr = e.target.closest('tr.al1-row');
      if (!tr || e.target.closest('a,button')) return;
      var a = tr.querySelector('a.al1-go:last-child'); if (a) a.click();
    });
    page.addEventListener('keydown', function (e) {
      var tr = e.target.closest('tr.al1-row'); if (!tr) return;
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); pick(tr.dataset.key); }
      else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        var v = visible(), i = v.indexOf(tr), n = v[i + (e.key === 'ArrowDown' ? 1 : -1)];
        if (n) { e.preventDefault(); n.focus(); pick(n.dataset.key); }
      }
    });
    page.addEventListener('input', function (e) { if (e.target.closest('[data-clq]')) setTimeout(settle, 0); });
    group('role');
    settle();
  }
  function boot() { init(document.querySelector('.al1-root')); }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot); else boot();
  document.addEventListener('htmx:afterSettle', boot);
})();
