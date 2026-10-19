// Loaded by templates/manage_policy_form.html.
(function () {
  var $ = function (id) { return document.getElementById(id); };
  var tt = $('ke-tt'), tid = $('ke-tid'), ts = $('ke-ts'), nameEl = $('ke-name');
  var nameTouched = !!nameEl.value, targetLabel = '', seq = 0;
  function esc(s) { return String(s || '').replace(/[&<>"]/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]; }); }

  function clearTargets() {
    document.querySelectorAll('.ke-chip.on').forEach(function (c) { c.classList.remove('on'); });
    var g = $('ke-group'); if (g) g.classList.remove('on');
    $('ke-serial').classList.remove('on');
  }
  function setTarget(type, id, serial, label) {
    tt.value = type; tid.value = id || ''; ts.value = serial || ''; targetLabel = label || '';
    autoName(); refresh();
  }
  document.querySelectorAll('.ke-chip').forEach(function (c) {
    c.addEventListener('click', function () {
      clearTargets(); c.classList.add('on');
      var g = $('ke-group'); if (g) g.value = '';
      $('ke-serial').value = '';
      setTarget(c.dataset.tt, c.dataset.id, '', c.dataset.label);
    });
  });
  var g = $('ke-group');
  if (g) g.addEventListener('change', function () {
    clearTargets(); $('ke-serial').value = '';
    if (!g.value) { setTarget('', '', '', ''); return; }
    g.classList.add('on');
    setTarget('group', g.value, '', g.options[g.selectedIndex].text.replace(/ \(\d+\)$/, ''));
  });
  $('ke-serial').addEventListener('change', function () {
    var v = this.value.trim();
    clearTargets(); if (g) g.value = '';
    if (!v) { setTarget('', '', '', ''); return; }
    this.classList.add('on');
    setTarget('device', '', v, v);
  });

  function autoName() {
    if (nameTouched) return;
    nameEl.value = targetLabel ? targetLabel + ' · locked' : '';
  }
  nameEl.addEventListener('input', function () { nameTouched = !!nameEl.value; });

  // Rules lock each device to its role's app (Lineup), so the editor asks only where.
  function renderPreview(d) {
    var el = $('ke-prev');
    if (!tt.value) { el.innerHTML = '<div class="ke-empty">Pick where.</div>'; return; }
    var h = '<div class="ke-big">' + (d.devices || 0) + ' device' + (d.devices === 1 ? '' : 's') + '</div><div style="color:var(--muted)">' + esc(targetLabel) + ' · ' + (d.online || 0) + ' online</div><ul>';
    if (d.lock) h += '<li>' + d.lock + ' lock to their role\'s app' + (d.online ? '' : ' when they next check in') + '</li>';
    if (d.missing) h += '<li class="w">' + d.missing + ' don\'t have their role\'s app, or their role has none: they lock once it\'s installed</li>';
    if (d.cant) h += '<li class="b">' + d.cant + ' can\'t lock (their agent has no lock)</li>';
    for (var k in (d.higher || {})) h += '<li>' + d.higher[k] + ' follow “' + esc(k) + '”, a rule above this one</li>';
    if (!d.lock && !d.missing && !d.cant && !Object.keys(d.higher || {}).length) h += '<li>Nothing to lock here yet</li>';
    h += '</ul>';
    el.innerHTML = h;
    $('ke-save').textContent = d.lock ? 'Save and lock ' + d.lock + ' device' + (d.lock === 1 ? '' : 's') : 'Save';
  }

  function refresh() {
    var ready = tt.value && (tt.value === 'all' || tid.value || ts.value);
    $('ke-save').disabled = !ready;
    if (!ready) { renderPreview({}); return; }
    var q = new URLSearchParams({ target_type: tt.value, target_id: tid.value, target_serial: ts.value, id: $('ke-id').value });
    var my = ++seq;
    fetch('/manage/policies/preview?' + q.toString(), { credentials: 'same-origin', headers: { Accept: 'application/json' } })
      .then(function (r) { return r.json(); })
      .then(function (d) { if (my === seq) renderPreview(d); })
      .catch(function () {});
  }

  $('ke-form').addEventListener('submit', function (e) {
    if ($('ke-save').disabled) { e.preventDefault(); return; }
    if (!nameEl.value.trim()) { autoName(); }
  });

  // Start from the rule being edited, or a target passed in the link (?target=all).
  (function init() {
    var want = new URLSearchParams(location.search).get('target');
    if (!tt.value && want === 'all') tt.value = 'all';
    if (tt.value === 'restaurant' || tt.value === 'all') {
      document.querySelectorAll('.ke-chip').forEach(function (c) {
        if (c.dataset.tt === tt.value && (tt.value === 'all' || c.dataset.id === tid.value)) { c.classList.add('on'); targetLabel = c.dataset.label; }
      });
    } else if (tt.value === 'group' && g) {
      g.value = tid.value; g.classList.add('on');
      if (g.selectedIndex > 0) targetLabel = g.options[g.selectedIndex].text.replace(/ \(\d+\)$/, '');
    } else if (tt.value === 'device') {
      $('ke-serial').value = ts.value; $('ke-serial').classList.add('on'); targetLabel = ts.value;
    }
    refresh();
  })();
})();
