// Loaded by templates/profile.html (PR1): the Display preview and the two small helpers.
(function () {
  var f = document.getElementById('pr1-form');
  if (!f) return;
  var prev = document.getElementById('pr1-prev'), note = document.getElementById('pr1-note');
  function val(name) { var c = f.querySelector('input[name="' + name + '"]:checked'); return c ? c.value : ''; }
  function fmt(t, h24) {
    var p = t.split(':'), h = +p[0], m = p[1];
    if (h24) return t;
    return ((h % 12) || 12) + ':' + m + ' ' + (h < 12 ? 'AM' : 'PM');
  }
  function sortLabel(v) {
    var map = {}, raw = f.getAttribute('data-sortlabels') || '';
    raw.split(';').forEach(function (kv) { var i = kv.indexOf('='); if (i > 0) map[kv.slice(0, i)] = kv.slice(i + 1); });
    return map[v] || v;
  }
  function render() {
    var dens = val('density') || f.getAttribute('data-fd-dens');
    var t24 = (val('time_format') || f.getAttribute('data-fd-time')) === '24';
    prev.classList.toggle('compact', dens === 'compact');
    [].forEach.call(prev.querySelectorAll('td[data-t]'), function (td) { td.textContent = fmt(td.getAttribute('data-t'), t24); });
    var rows = (f.querySelector('[name=page_size]').value || '').trim() || f.getAttribute('data-fd-rows');
    var sortv = f.querySelector('[name=default_sort]').value;
    var sl = sortv ? sortLabel(sortv) : f.getAttribute('data-fd-sort');
    note.textContent = 'Showing ' + rows + ' rows per page, sorted by ' + String(sl).toLowerCase() + '.';
  }
  f.addEventListener('input', render);
  f.addEventListener('change', render);
  render();

  var reset = document.getElementById('pr1-reset');
  if (reset) reset.addEventListener('click', function () {
    f.querySelector('[name=page_size]').value = '';
    f.querySelector('[name=default_sort]').value = '';
    ['density', 'time_format'].forEach(function (n) { var r = f.querySelector('input[name="' + n + '"][value=""]'); if (r) r.checked = true; });
    render();
    f.requestSubmit();
  });

  var sr = document.getElementById('pr1-splitreset');
  if (sr) sr.addEventListener('click', function () {
    fetch('/profile/device-split', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/x-www-form-urlencoded' }, body: 'split=' })
      .then(function (r) {
        if (!r.ok) throw new Error();
        document.getElementById('pr1-splitv').textContent = 'Default';
        sr.remove();
      })
      .catch(function () { if (window.showToast) window.showToast('Could not reset the divider', 'error'); });
  });
})();
