// Loaded by templates/device.html.
function openAppModal() {
  document.getElementById('new-app-name').value = '';
  document.getElementById('new-app-url').value  = '';
  document.getElementById('app-modal-error').style.display = 'none';
  document.getElementById('app-modal').classList.add('modal-open');
  document.getElementById('new-app-name').focus();
}

function closeAppModal(e) {
  if (!e || e.target === document.getElementById('app-modal')) {
    document.getElementById('app-modal').classList.remove('modal-open');
  }
}

async function saveNewApp() {
  const name   = document.getElementById('new-app-name').value.trim();
  const apkURL = document.getElementById('new-app-url').value.trim();
  const errEl  = document.getElementById('app-modal-error');

  if (!name || !apkURL) {
    errEl.textContent = 'Both fields are required.';
    errEl.style.display = '';
    return;
  }

  const body = new URLSearchParams({ name, apk_url: apkURL });
  const res  = await fetch('/setup/apps/create', { method: 'POST', body });

  if (!res.ok) {
    errEl.textContent = 'Failed to save app. Try again.';
    errEl.style.display = '';
    return;
  }

  const app = await res.json();

  document.querySelectorAll('select[name="apk_url"]').forEach(sel => {
    const opt = new Option(app.name, app.apk_url, true, true);
    sel.add(opt);
    sel.value = app.apk_url;
  });

  const hidden = document.getElementById('cmd-apk-url');
  if (hidden) hidden.value = app.apk_url;

  closeAppModal();
}

// App locked-app picker: an always-visible, searchable grid/list of installed
// apps. Selecting one writes the package id to the hidden #kiosk-pkg-value and
// fires 'change' (which re-applies if kiosk is on and un-gates the toggle). Apps
// come from the server-rendered #kiosk-apps-src data island so grid and list stay
// in sync from one source.
(function () {
  var picker = document.getElementById('kiosk-picker');
  if (!picker) return;
  var src = document.getElementById('kiosk-apps-src');
  var hidden = document.getElementById('kiosk-pkg-value');
  var q = document.getElementById('kpk-q');
  var gridEl = document.getElementById('kpk-grid');
  var listEl = document.getElementById('kpk-list');
  // No "None" option — use the Turn off kiosk button to release to launcher.
  // Build from the server island (authoritative names/icons + the "locked-but-missing"
  // current package) and merge in any apps currently in the live drawer the island lacks
  // — so an app just installed shows in the picker without a page reload.
  function loadApps() {
    var out = [], seen = {};
    if (src) [].forEach.call(src.querySelectorAll('i'), function (el) {
      var v = el.getAttribute('data-value'); if (!v || seen[v]) return; seen[v] = 1;
      out.push({ v: v, n: el.getAttribute('data-name'), icon: el.getAttribute('data-icon'), missing: el.hasAttribute('data-missing') });
    });
    // The live drawer merge must respect the kiosk allowlist (Settings › Devices
    // & Kiosk), otherwise every installed app leaks into the picker.
    var allow = (src && src.getAttribute('data-allow') || '').split(',').filter(Boolean).map(function (pat) {
      return new RegExp('^' + pat.trim().split('*').map(function (x) { return x.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'); }).join('.*') + '$');
    });
    function allowed(pkg) { if (!allow.length) return true; for (var i = 0; i < allow.length; i++) if (allow[i].test(pkg)) return true; return false; }
    var live = document.getElementById('apps-live');
    if (live) [].forEach.call(live.querySelectorAll('.app-tile:not(.sys)'), function (t) {
      if (t.classList.contains('app-uninstalling') || t.classList.contains('app-installing')) return;
      var v = t.getAttribute('data-pkg'); if (!v || seen[v] || !allowed(v)) return; seen[v] = 1;
      var icon = '', img = t.querySelector('img.app-icon');
      if (img) { var s = img.getAttribute('src') || '', k = s.indexOf('base64,'); if (k >= 0) icon = s.slice(k + 7); }
      out.push({ v: v, n: t.getAttribute('data-name') || v, icon: icon, missing: false });
    });
    return out;
  }
  var apps = loadApps();
  var kbd = -1;

  function esc(s) { return (s || '').replace(/[&<>"]/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]; }); }
  function colorFor(s) { var h = 0; for (var i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) & 0xffffff; return 'hsl(' + (h % 360) + ' 52% 45%)'; }
  function initial(n) { n = (n || '').replace(/^[^A-Za-z0-9]+/, ''); return (n.slice(0, 1) || '?').toUpperCase(); }
  function match(a, s) { if (!s) return true; if (a.none) return 'none release launcher clear'.indexOf(s) >= 0; return (a.n + ' ' + a.v).toLowerCase().indexOf(s) >= 0; }
  function filtered() { var s = q.value.trim().toLowerCase(); return apps.filter(function (a) { return match(a, s); }); }

  function tileHtml(a) {
    if (a.none) return '<span class="kpk-tile none">–</span>';
    if (a.icon) return '<img class="kpk-tile kpk-img" src="data:image/png;base64,' + a.icon + '" alt="">';
    return '<span class="kpk-tile" style="background:' + colorFor(a.v || a.n) + '">' + esc(initial(a.n)) + '</span>';
  }
  function optHtml(a, i) {
    var sel = hidden.value === a.v;
    return '<button type="button" class="kpk-opt' + (sel ? ' sel' : '') + (i === kbd ? ' kbd' : '') + '" data-value="' + esc(a.v) + '" role="option" aria-selected="' + sel + '">' +
      tileHtml(a) + '<span class="kpk-txt"><span class="kpk-nm">' + esc(a.n) + '</span>' +
      (a.v ? '<span class="kpk-pk mono">' + esc(a.v) + (a.missing ? ' · not installed' : '') + '</span>' : '') + '</span>' +
      '<span class="kpk-check"><svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3"><path d="M20 6 9 17l-5-5"/></svg></span></button>';
  }
  function render() {
    var items = filtered();
    var html = items.length ? items.map(optHtml).join('') : '<div class="kpk-empty">No apps match.</div>';
    gridEl.innerHTML = html; listEl.innerHTML = html;
  }
  function choose(v) { hidden.value = v; render(); hidden.dispatchEvent(new Event('change', { bubbles: true })); }

  gridEl.addEventListener('click', pick); listEl.addEventListener('click', pick);
  function pick(e) { var b = e.target.closest('.kpk-opt'); if (b) choose(b.getAttribute('data-value')); }

  q.addEventListener('input', function () { kbd = -1; render(); });
  q.addEventListener('keydown', function (e) {
    var items = filtered();
    if (e.key === 'ArrowDown') { e.preventDefault(); kbd = Math.min(kbd + 1, items.length - 1); render(); scrollKbd(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); kbd = Math.max(kbd - 1, 0); render(); scrollKbd(); }
    else if (e.key === 'Enter') { e.preventDefault(); if (kbd >= 0 && items[kbd]) choose(items[kbd].v); else if (items.length === 1) choose(items[0].v); }
  });
  function scrollKbd() { var box = picker.classList.contains('listview') ? listEl : gridEl; var el = box.querySelector('.kpk-opt.kbd'); if (el) el.scrollIntoView({ block: 'nearest' }); }

  function setView(v) {
    picker.classList.toggle('listview', v === 'list');
    document.getElementById('kpk-vgrid').classList.toggle('active', v === 'grid');
    document.getElementById('kpk-vlist').classList.toggle('active', v === 'list');
  }
  document.getElementById('kpk-vgrid').onclick = function () { setView('grid'); };
  document.getElementById('kpk-vlist').onclick = function () { setView('list'); };

  // Exposed so the modal can re-highlight the selection after resetting it on open.
  window.kioskPickerRender = render;
  // Rebuild the app list from the (freshly refreshed) drawer — called after an install/
  // uninstall so the kiosk picker reflects it without a page reload. Runs on EVERY drawer
  // refresh (every check-in), so only actually re-render when the app set changed — else the
  // picker's whole grid/list innerHTML was being replaced on every tick (a flash when the
  // kiosk modal is open, wasted work when it isn't).
  var _appsSig = '';
  function _sig(a) { return a.map(function (x) { return x.v + '|' + x.n + '|' + (x.missing ? 1 : 0); }).join(','); }
  _appsSig = _sig(apps);
  window.kioskPickerReload = function () {
    var next = loadApps(), sig = _sig(next);
    if (sig === _appsSig) return; // nothing changed — don't touch the DOM
    _appsSig = sig; apps = next; render();
  };
  render();
})();
