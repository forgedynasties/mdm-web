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
