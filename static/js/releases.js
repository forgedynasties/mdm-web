// Loaded by templates/releases.html.
function roDev(on) {
  var l = document.getElementById('ro-list'); if (l) l.classList.toggle('show-dev', on);
  try { localStorage.setItem('mdm-show-dev', on ? '1' : '0'); } catch (e) {}
}
(function () {
  setTimeout(function () {
    var on = false; try { on = localStorage.getItem('mdm-show-dev') === '1'; } catch (e) {}
    var box = document.getElementById('ro-dev'); if (box) box.checked = on;
    var l = document.getElementById('ro-list'); if (l) l.classList.toggle('show-dev', on);
    // A selected dev or reported row stays visible whatever the toggles say.
    var sel = l && l.querySelector('.ro-it.on'); if (sel) sel.style.display = 'grid';
  }, 0);
  if (/[?&]new=1(&|$)/.test(location.search)) { var m = document.getElementById('create-pkg-modal'); if (m) m.classList.add('modal-open'); }
})();

// "Show more" (19 Oct): the list shows its first ROMORE_N rows that the toggles leave
// visible, plus the selected one wherever it is; the button under them shows the rest.
// It re-counts whenever the dev or reported toggle changes what is visible.
var ROMORE_N = 12;
function roMore(expand) {
  var l = document.getElementById('ro-list'), btn = document.getElementById('ro-more');
  if (!l || !btn) return;
  if (expand) l.classList.add('ro-all');
  var all = l.classList.contains('ro-all'), shown = 0, hidden = 0;
  l.querySelectorAll('.ro-it').forEach(function (a) {
    a.classList.remove('ro-cut');
    // The Hidden group sits under the button and keeps its own rows.
    if (btn.compareDocumentPosition(a) & Node.DOCUMENT_POSITION_FOLLOWING) return;
    if (getComputedStyle(a).display === 'none') return;
    shown++;
    if (!all && shown > ROMORE_N && !a.classList.contains('on')) { a.classList.add('ro-cut'); hidden++; }
  });
  btn.hidden = hidden === 0;
  btn.textContent = 'Show ' + hidden + ' more';
}
(function () {
  setTimeout(function () { roMore(false); }, 0);
  document.addEventListener('change', function (e) {
    if (e.target && e.target.type === 'checkbox') setTimeout(function () { roMore(false); }, 0);
  });
})();
