// Update settings US1: the list switches the pane in place (every pane is on the page);
// the URL keeps ?s= so a setting can be linked and a save comes back to it.
(function () {
  var root = document.getElementById('us1');
  if (!root || root.dataset.wired) return;
  root.dataset.wired = '1';
  function show(key, push) {
    var found = false;
    root.querySelectorAll('.us1-p').forEach(function (p) {
      var on = p.getAttribute('data-pane') === key; p.hidden = !on; if (on) found = true;
    });
    if (!found) return;
    root.querySelectorAll('.us1-li').forEach(function (a) {
      var on = a.getAttribute('data-us') === key; a.classList.toggle('on', on);
      if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    });
    var pane = document.getElementById('us1-pane'); if (pane) pane.scrollTop = 0;
    if (push) { try { history.replaceState(null, '', '/releases/settings?s=' + key); } catch (e) {} }
  }
  root.addEventListener('click', function (e) {
    var a = e.target.closest('.us1-li');
    if (!a || e.metaKey || e.ctrlKey || e.shiftKey || e.button) return;
    e.preventDefault(); e.stopPropagation(); show(a.getAttribute('data-us'), true);
  }, true);
})();
