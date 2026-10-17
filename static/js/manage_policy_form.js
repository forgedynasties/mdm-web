// Loaded by templates/manage_policy_form.html.
(function () {
  var $ = function (id) { return document.getElementById(id); };
  var tt = $('ke-tt'), tid = $('ke-tid'), ts = $('ke-ts'), pkg = $('ke-pkg'), nameEl = $('ke-name');
  var nameTouched = !!nameEl.value, targetLabel = '', appName = '', apps = [], last = null, seq = 0;
  function esc(s) { return String(s || '').replace(/[&<>"]/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]; }); }
  function color(s) { var h = 0; for (var i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) & 0xffffff; return 'hsl(' + (h % 360) + ' 52% 45%)'; }

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
    nameEl.value = targetLabel && appName ? targetLabel + ' · ' + appName : '';
  }
  nameEl.addEventListener('input', function () { nameTouched = !!nameEl.value; });

  function renderApps() {
    var box = $('ke-apps'), q = ($('ke-appq').value || '').toLowerCase();
    if (!tt.value) { box.innerHTML = ''; return; }
    var list = apps.filter(function (a) { return !q || (a.name + ' ' + a.pkg).toLowerCase().indexOf(q) >= 0; });
    if (!apps.length) { box.innerHTML = '<div class="ke-empty">No apps reported on these devices yet.</div>'; return; }
    box.innerHTML = list.map(function (a) {
      var ic = a.icon ? '<img src="data:image/png;base64,' + a.icon + '" alt="">' : esc((a.name || '?').replace(/^[^A-Za-z0-9]+/, '').slice(0, 1).toUpperCase());
      return '<button type="button" class="ke-app' + (a.pkg === pkg.value ? ' on' : '') + '" role="option" aria-selected="' + (a.pkg === pkg.value) + '" data-pkg="' + esc(a.pkg) + '" data-name="' + esc(a.name) + '">' +
        '<i style="background:' + (a.icon ? 'transparent' : color(a.pkg)) + '">' + ic + '</i><span><b>' + esc(a.name) + '</b><small>on ' + a.count + ' of ' + (last ? last.devices : '?') + '</small></span></button>';
    }).join('') || '<div class="ke-empty">No app matches.</div>';
    box.querySelectorAll('.ke-app').forEach(function (b) {
      b.addEventListener('click', function () { pkg.value = b.dataset.pkg; appName = b.dataset.name; autoName(); renderApps(); refresh(); });
    });
  }
  $('ke-appq').addEventListener('input', renderApps);

  function renderPreview(d) {
    var el = $('ke-prev');
    if (!tt.value) { el.innerHTML = '<div class="ke-empty">Pick where and which app.</div>'; return; }
    var h = '<div class="ke-big">' + d.devices + ' device' + (d.devices === 1 ? '' : 's') + '</div><div style="color:var(--muted)">' + esc(targetLabel) + ' · ' + d.online + ' online</div>';
    if (pkg.value) {
      h += '<ul>';
      if (d.lock) h += '<li>' + d.lock + ' lock to ' + esc(appName || pkg.value) + (d.online ? '' : ' when they next check in') + '</li>';
      if (d.missing) h += '<li class="w">' + d.missing + ' don\'t have the app: they lock once it\'s installed</li>';
      if (d.cant) h += '<li class="b">' + d.cant + ' can\'t lock (their agent has no kiosk)</li>';
      for (var k in (d.higher || {})) h += '<li>' + d.higher[k] + ' follow “' + esc(k) + '”, a rule above this one</li>';
      if (!d.lock && !d.missing && !d.cant && !Object.keys(d.higher || {}).length) h += '<li>Nothing to lock here yet</li>';
      h += '</ul>';
    }
    el.innerHTML = h;
    $('ke-save').textContent = pkg.value && d.lock ? 'Save and lock ' + d.lock + ' device' + (d.lock === 1 ? '' : 's') : 'Save';
  }

  function refresh() {
    var ready = tt.value && (tt.value === 'all' || tid.value || ts.value);
    $('ke-save').disabled = !(ready && pkg.value);
    $('ke-appq').hidden = !ready;
    $('ke-apphint').textContent = ready ? 'Only apps installed on these devices, with how many have each.' : 'Pick where first: the list shows only apps installed on those devices.';
    if (!ready) { apps = []; renderApps(); renderPreview({}); return; }
    var q = new URLSearchParams({ target_type: tt.value, target_id: tid.value, target_serial: ts.value, kiosk_package: pkg.value, id: $('ke-id').value });
    var my = ++seq;
    fetch('/manage/policies/preview?' + q.toString(), { credentials: 'same-origin', headers: { Accept: 'application/json' } })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (my !== seq) return;
        last = d; apps = d.apps || [];
        var cur = apps.filter(function (a) { return a.pkg === pkg.value; })[0];
        if (cur) appName = cur.name;
        renderApps(); renderPreview(d);
      }).catch(function () {});
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
