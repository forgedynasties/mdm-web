// Loaded by templates/device.html.
function toggleInstallForm() {
  var f = document.getElementById('install-form');
  if (!f) return;
  if (f.hasAttribute('hidden')) { f.removeAttribute('hidden'); var s = f.querySelector('select,input'); if (s) s.focus(); }
  else f.setAttribute('hidden', '');
}
function appsShowAll() {
  var list = document.getElementById('apps-table');
  if (list) list.classList.remove('collapsed');
  var btn = document.getElementById('apps-more');
  if (btn) btn.style.display = 'none';
}
// After the apps list live-refreshes (device check-in), re-apply the active search
// filter (the input is hx-preserved) and refresh the package count.
function appsAfterRefresh() {
  colorizeApps();
  var f = document.getElementById('apps-filter');
  filterInstalledApps(f ? f.value : '');
  var c = document.getElementById('apps-count-n');
  if (c) {
    var n = document.querySelectorAll('#apps-table .app-row:not(.app-installing)').length;
    c.textContent = n + ' app' + (n === 1 ? '' : 's');
  }
  // Keep the app locked-app picker in sync with the drawer, so a newly installed app
  // is selectable without a page reload.
  if (window.kioskPickerReload) { try { window.kioskPickerReload(); } catch (e) {} }
}
// Deterministic monogram tint per package (no real icons are reported). Translucent
// fill + mid-tone text read fine in both light and dark themes.
function colorizeApps() {
  document.querySelectorAll('.app-mono[data-seed]').forEach(function (el) {
    var s = el.getAttribute('data-seed') || '', h = 0;
    for (var i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
    var hue = h % 360;
    el.style.background = 'hsla(' + hue + ',65%,55%,0.16)';
    el.style.color = 'hsl(' + hue + ',48%,50%)';
  });
}
function filterInstalledApps(q) {
  q = q.trim().toLowerCase();
  var rows = document.querySelectorAll('#apps-table .app-row');
  var list = document.getElementById('apps-table');
  var btn = document.getElementById('apps-more');
  var noMatch = document.getElementById('apps-no-match');
  var sep = document.getElementById('apps-sys-sep');
  if (q === '') {
    // Cleared: restore every tile and the "System apps" divider.
    rows.forEach(function (row) { row.style.display = ''; });
    if (sep) sep.style.display = '';
    if (btn) { if (list) list.classList.add('collapsed'); btn.style.display = ''; }
    if (noMatch) noMatch.style.display = 'none';
    return;
  }
  // Searching: look across every app, the folded system apps included.
  var sysd = document.getElementById('apps-sys'); if (sysd) sysd.open = true;
  if (list) list.classList.remove('collapsed');
  if (btn) btn.style.display = 'none';
  var visible = 0;
  rows.forEach(function (row) {
    var show = (row.getAttribute('data-app') || '').toLowerCase().includes(q);
    row.style.display = show ? '' : 'none';
    if (show) visible++;
  });
  if (noMatch) noMatch.style.display = (rows.length > 0 && visible === 0) ? '' : 'none';
}
colorizeApps();
// Install picker modal (app-drawer-style grid of repository apps).
window.installModalOpen = function () {
  var m = document.getElementById('install-modal'); if (!m) return;
  m.classList.add('modal-open');
  colorizeApps();
  var form = document.getElementById('install-form');
  if (form) {
    form.querySelectorAll('.install-tile.on').forEach(function (t) { t.classList.remove('on'); });
    var holder = form.querySelector('.install-hidden'); if (holder) holder.innerHTML = '';
    var c = document.getElementById('install-count'), s = document.getElementById('install-submit');
    if (c) c.textContent = 'Select one or more apps';
    if (s) { s.disabled = true; s.textContent = 'Install'; }
  }
  var f = document.getElementById('install-filter');
  if (f) { f.value = ''; filterInstallApps(''); setTimeout(function () { f.focus(); }, 30); }
};
window.installModalClose = function () {
  var m = document.getElementById('install-modal'); if (m) m.classList.remove('modal-open');
};
window.installModalBackdrop = function (e) { if (e.target.id === 'install-modal') window.installModalClose(); };
// App-info popup — populated from the clicked tile's data attributes + its icon.
window.appInfoOpen = function (el) {
  var m = document.getElementById('appinfo-modal'); if (!m || !el) return;
  // A tile that's mid-uninstall is inert — it just shows "Uninstalling…"; don't open the
  // details/uninstall modal for an app that's already on its way out.
  if (el.classList && el.classList.contains('app-uninstalling')) return;
  window.__aiTile = el; // remember the drawer tile so uninstall can mark it optimistically
  var byId = function (id) { return document.getElementById(id); };
  var name = el.getAttribute('data-name') || '', pkg = el.getAttribute('data-pkg') || '',
      ver = el.getAttribute('data-ver') || '', sys = el.getAttribute('data-sys') === '1';
  byId('ai-name').textContent = name;
  byId('ai-pkg').textContent = pkg;
  byId('ai-pkg2').textContent = pkg;
  byId('ai-ver').textContent = ver ? ('v' + ver) : '—';
  var aid = el.getAttribute('data-aid') || '';
  byId('ai-aid').textContent = aid; byId('ai-aid-row').hidden = !aid;
  byId('ai-type').innerHTML = sys ? '<span class="ai-badge ai-sys">System app</span>' : '<span class="ai-badge ai-user">User app</span>';
  var host = byId('ai-icon'); host.innerHTML = '';
  var ic = el.querySelector('.app-icon, .app-mono');
  if (ic) { var c = ic.cloneNode(true); c.classList.add('ai-iconimg'); host.appendChild(c); }
  var save = byId('ai-save-pkg'); if (save) save.value = pkg;
  var btn = byId('ai-uninstall-btn');
  if (btn) {
    var note = byId('ai-sysnote');
    byId('ai-uninstall-pkg').value = pkg;
    btn.hidden = sys;
    if (note) note.hidden = !sys;
  }
  m.classList.add('modal-open');
};
// Copy an ID and say so for a second on the control that was clicked.
window.aidCopy = function (btn, text) {
  var done = function () { var k = btn.querySelector('.mono') || btn, old = k.textContent; k.textContent = 'Copied'; setTimeout(function () { k.textContent = old; }, 1000); };
  try { navigator.clipboard.writeText(text).then(done, function () {}); } catch (e) {}
};
// Header chip with several different Nugget IDs: list them in a dialog. The chip row is
// re-fetched on every device update, so the dialog lives outside it and is built on click.
window.aidList = function (btn) {
  var m = document.getElementById('aid-modal'), host = document.getElementById('aid-list'); if (!m || !host) return;
  var ids = []; try { ids = JSON.parse(btn.getAttribute('data-ids') || '[]'); } catch (e) {}
  host.innerHTML = '';
  ids.forEach(function (x) {
    var row = document.createElement('div'); row.className = 'appinfo-row';
    var k = document.createElement('span'); k.className = 'k'; k.textContent = x.package;
    var v = document.createElement('span'); v.className = 'v mono'; v.textContent = x.android_id + ' ';
    var c = document.createElement('button'); c.type = 'button'; c.className = 'ai-copy'; c.textContent = 'Copy';
    c.onclick = function () { window.aidCopy(c, x.android_id); };
    v.appendChild(c); row.appendChild(k); row.appendChild(v); host.appendChild(row);
  });
  m.classList.add('modal-open');
};
window.aidClose = function () { var m = document.getElementById('aid-modal'); if (m) m.classList.remove('modal-open'); };
document.addEventListener('keydown', function (e) { if (e.key === 'Escape') window.aidClose(); });
window.appInfoClose = function () { var m = document.getElementById('appinfo-modal'); if (m) m.classList.remove('modal-open'); };
window.appInfoBackdrop = function (e) { if (e.target.id === 'appinfo-modal') window.appInfoClose(); };
// Direct-to-S3 APK upload: presigned PUT (browser → S3), then register (server parses
// package/version/icon from S3), then reload so the new app appears in the grid.
window.mdmFmtBytes = function (n) { if (n < 1024) return n + ' B'; if (n < 1048576) return (n / 1024).toFixed(0) + ' KB'; if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB'; return (n / 1073741824).toFixed(2) + ' GB'; };
// PUT to S3 via XHR (fetch can't report upload progress). onProgress(loaded, total).
window.mdmS3Put = function (url, file, onProgress) {
  return new Promise(function (resolve, reject) {
    var xhr = new XMLHttpRequest();
    xhr.open('PUT', url);
    xhr.setRequestHeader('Content-Type', 'application/vnd.android.package-archive');
    xhr.upload.onprogress = function (e) { if (e.lengthComputable && onProgress) onProgress(e.loaded, e.total); };
    xhr.onload = function () { (xhr.status >= 200 && xhr.status < 300) ? resolve() : reject(new Error('S3 upload failed (' + xhr.status + ')')); };
    xhr.onerror = function () { reject(new Error('network error uploading to S3')); };
    xhr.send(file);
  });
};
// Lazy-load the (large) in-browser APK parser only when an upload actually happens.
window.mdmEnsureParser = function () {
  return new Promise(function (resolve, reject) {
    if (window.AppInfoParser) return resolve();
    var s = document.createElement('script');
    s.src = '/static/vendor/app-info-parser.min.js';
    s.onload = function () { resolve(); };
    s.onerror = function () { reject(new Error('could not load APK parser')); };
    document.head.appendChild(s);
  });
};
// Normalise any icon (png/webp/…) to a 96px base64 PNG via canvas.
window.mdmIconPng = function (iconStr) {
  return new Promise(function (resolve) {
    if (!iconStr) return resolve('');
    var src = iconStr.indexOf('data:') === 0 ? iconStr : ('data:image/png;base64,' + iconStr);
    var img = new Image();
    img.onload = function () {
      try {
        var c = document.createElement('canvas'); c.width = 96; c.height = 96;
        c.getContext('2d').drawImage(img, 0, 0, 96, 96);
        resolve(c.toDataURL('image/png').split(',')[1] || '');
      } catch (e) { resolve(''); }
    };
    img.onerror = function () { resolve(''); };
    img.src = src;
  });
};
// Parse the APK IN THE BROWSER (the file is local — no server download), then upload
// to S3 and register with the parsed metadata. Server only re-parses as a fallback.
window.uploadApk = function (file) {
  if (!file) return;
  var el = document.getElementById('install-upload-status');
  var set = function (t, err) { if (!el) return; el.hidden = false; el.textContent = t; el.classList.toggle('is-error', !!err); };
  var meta = { pkg: '', name: '', icon: '' };
  set('Reading ' + file.name + ' (' + mdmFmtBytes(file.size) + ')…');
  mdmEnsureParser()
    .then(function () { return new AppInfoParser(file).parse(); })
    .then(function (info) {
      meta.pkg = info.package || '';
      // Label may be a string OR an array (multi-locale) — always coerce to one string,
      // or the server's JSON decode (name is a string) fails.
      var lbl = (info.application && info.application.label) || info.label || meta.pkg;
      if (Array.isArray(lbl)) lbl = lbl[0];
      meta.name = String(lbl || meta.pkg);
      return mdmIconPng(info.icon).then(function (png) { meta.icon = png; });
    })
    .catch(function () { /* parse failed → server will fall back to parsing from S3 */ })
    .then(function () {
      return fetch('/apps/upload-url', { method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin', body: JSON.stringify({ filename: file.name }) });
    })
    .then(function (r) { if (!r.ok) throw new Error('could not get upload URL'); return r.json(); })
    .then(function (d) {
      set('Uploading ' + file.name + ' (' + mdmFmtBytes(file.size) + ')…');
      return mdmS3Put(d.url, file, function (loaded, total) {
        set('Uploading ' + file.name + ' — ' + Math.round(loaded / total * 100) + '% (' + mdmFmtBytes(loaded) + ' / ' + mdmFmtBytes(total) + ')');
      }).then(function () { return d.key; });
    })
    .then(function (key) {
      set('Saving…');
      return fetch('/apps/register', { method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin', body: JSON.stringify({ key: key, package: meta.pkg, name: meta.name, icon: meta.icon }) })
        .then(function (r) { return r.json().then(function (j) { if (!r.ok) throw new Error(j.error || 'register failed'); return j; }); });
    })
    .then(function (app) {
      set('Added "' + (app.name || 'app') + '". Reloading…');
      setTimeout(function () { location.reload(); }, 800);
    })
    .catch(function (err) { set('Error: ' + err.message, true); });
};
window.filterInstallApps = function (q) {
  q = (q || '').trim().toLowerCase();
  var vis = 0;
  document.querySelectorAll('#install-drawer .install-tile').forEach(function (f) {
    var box = f.closest('.install-tile-wrap') || f;
    // Everything the library has is offered, installed or not — the tile says which.
    var show = (f.getAttribute('data-app') || '').toLowerCase().indexOf(q) !== -1;
    box.style.display = show ? '' : 'none'; if (show) vis++;
  });
  var nm = document.getElementById('install-no-match');
  if (nm) nm.style.display = (q && vis === 0) ? '' : 'none';
};
// After an uninstall settles, the app is no longer on the device — surface it again in
// the Install picker without a full page reload (server-rendered InstalledSet is stale).
window.mdmMarkUninstalled = function (pkg) {
  if (!pkg) return;
  document.querySelectorAll('#install-drawer .install-tile.install-installed').forEach(function (t) {
    if (t.getAttribute('data-pkg') === pkg) { t.classList.remove('install-installed'); t.style.display = ''; }
  });
};
// After an install settles, mark the tile instead of leaving it looking un-installed.
window.mdmMarkInstalled = function (pkg) {
  if (!pkg) return;
  document.querySelectorAll('#install-drawer .install-tile').forEach(function (t) {
    if (t.getAttribute('data-pkg') === pkg) t.classList.add('install-installed');
  });
};
// Multi-select install: toggling a tile adds/removes a hidden apk_url input on the
// shared form, so one submit queues an install command per selected app and lands on
// the combined progress page (progress_view=1).
window.installToggle = function (btn) {
  var url = btn.getAttribute('data-url'); if (!url) return;
  var form = document.getElementById('install-form'); if (!form) return;
  var holder = form.querySelector('.install-hidden');
  var on = btn.classList.toggle('on');
  var pick = btn.parentElement && btn.parentElement.querySelector('.install-pick');
  if (pick) { pick.hidden = !on; if (on) { pick.value = url; } }
  var existing = holder.querySelector('input[data-url="' + (window.CSS && CSS.escape ? CSS.escape(url) : url) + '"]');
  if (on && !existing) {
    var i = document.createElement('input');
    i.type = 'hidden'; i.name = 'apk_url'; i.value = url; i.setAttribute('data-url', url);
    holder.appendChild(i);
  } else if (!on && existing) {
    existing.remove();
  }
  var n = holder.querySelectorAll('input[name="apk_url"]').length;
  var c = document.getElementById('install-count'), s = document.getElementById('install-submit');
  if (c) c.textContent = n ? (n + ' app' + (n === 1 ? '' : 's') + ' selected') : 'Select one or more apps';
  if (s) { s.disabled = n === 0; s.textContent = n ? ('Install (' + n + ')') : 'Install'; }
};
// Optimistic install: the instant Install is clicked, drop an "installing" tile into the
// drawer for each selected app (cloning its icon) so feedback is immediate — the server
// round-trip then refreshes the drawer with the real pending/progress state.
window.installOptimistic = function () {
  var form = document.getElementById('install-form'); if (!form) return;
  var table = document.getElementById('apps-table'); if (!table) return;
  form.querySelectorAll('.install-tile.on').forEach(function (t) {
    var name = (t.getAttribute('data-app') || '').trim();
    var iconEl = t.querySelector('.app-icon, .app-mono');
    var tile = document.createElement('div');
    tile.className = 'app-row app-tile app-installing';
    tile.setAttribute('data-optimistic', '1');
    if (iconEl) { var ic = iconEl.cloneNode(true); ic.classList.remove('on'); tile.appendChild(ic); }
    var nm = document.createElement('div'); nm.className = 'app-tname'; nm.textContent = name; tile.appendChild(nm);
    var sub = document.createElement('div'); sub.className = 'app-tsub'; sub.textContent = 'installing'; tile.appendChild(sub);
    table.insertBefore(tile, table.firstChild);
  });
};
// A family's chooser changed: point the tile (and its hidden input) at that APK.
window.installPick = function (sel) {
  var wrap = sel.parentElement, btn = wrap && wrap.querySelector('.install-tile'); if (!btn) return;
  var form = document.getElementById('install-form'), holder = form && form.querySelector('.install-hidden'); if (!holder) return;
  var old = btn.getAttribute('data-url'), esc = window.CSS && CSS.escape ? CSS.escape(old) : old;
  var inp = holder.querySelector('input[data-url="' + esc + '"]');
  var opt = sel.options[sel.selectedIndex];
  btn.setAttribute('data-url', sel.value); btn.setAttribute('data-pkg', opt.getAttribute('data-pkg') || '');
  if (inp) { inp.value = sel.value; inp.setAttribute('data-url', sel.value); }
  var ver = btn.querySelector('.app-tver'); if (ver) ver.textContent = opt.textContent.replace(' (latest)', '') + ' · ' + opt.parentElement.label.split(' · ')[0];
  if (window.installUpdateCount) installUpdateCount();
};
// Optimistic uninstall: dim the tile + show "Uninstalling…" the instant it's clicked,
// before the server responds; the drawer refresh then reconciles to the real state.
window.markUninstalling = function (tile) {
  if (!tile) return;
  tile.classList.add('app-uninstalling');
  var sub = tile.querySelector('.app-tsub');
  if (!sub) { sub = document.createElement('div'); sub.className = 'app-tsub un'; tile.appendChild(sub); }
  else { sub.classList.add('un'); }
  sub.innerHTML = '<span class="app-spin"></span>Uninstalling…';
  var x = tile.querySelector('.app-x-form'); if (x) x.style.display = 'none';
};
// Controlled uninstall: show the spinner immediately and keep it (auto-refresh is
// suppressed via __uninstallPending), queue the uninstall, then poll the app list and do
// exactly ONE refresh when the app is actually gone — instead of the drawer reloading on
// every telemetry frame while the device works, which flickered and wiped the spinner.
window.doUninstall = function (tile) {
  if (!tile) return;
  var pkg = tile.getAttribute('data-pkg'); if (!pkg) return;
  var name = tile.getAttribute('data-name') || pkg;
  if (!confirm('Uninstall ' + name + ' from this device?')) return;
  markUninstalling(tile);
  window.__uninstallPending = (window.__uninstallPending || 0) + 1;
  var serial = window.DEVICE_SERIAL;
  var base = '/devices/' + encodeURIComponent(serial);
  fetch(base + '/commands', {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'HX-Request': 'true' },
    body: 'type=uninstall&package=' + encodeURIComponent(pkg)
  }).then(function (r) {
    if (r.ok) return;
    // Refused (e.g. the agent can't uninstall): say why and put the tile back now,
    // instead of spinning "Uninstalling…" for 30 s and silently reverting.
    return r.text().then(function (t) {
      clearInterval(iv); settle();
      tile.classList.remove('app-uninstalling');
      var sub = tile.querySelector('.app-tsub.un'); if (sub) sub.remove();
      if (window.showToast) showToast((t || 'Uninstall failed').trim(), 'error'); else alert((t || 'Uninstall failed').trim());
    });
  }).catch(function () {});
  var tries = 0;
  var settle = function () { window.__uninstallPending = Math.max(0, (window.__uninstallPending || 1) - 1); };
  var iv = setInterval(function () {
    tries++;
    fetch(base + '/apps-list', { credentials: 'same-origin' }).then(function (r) {
      // A transient 502/503 (e.g. server redeploy) still returns a body; never swap
      // an error page into the drawer. Treat non-2xx as "not settled yet" and retry.
      if (!r.ok) return null;
      return r.text();
    }).then(function (html) {
      if (html === null) { if (tries >= 20) { clearInterval(iv); settle(); } return; }
      // Only trust a real partial (has the apps table), not a stray page.
      if (html.indexOf('id="apps-table"') === -1) { if (tries >= 20) { clearInterval(iv); settle(); } return; }
      var gone = html.indexOf('data-pkg="' + pkg + '"') === -1;
      if (gone || tries >= 20) {           // gone, or give up after ~30s
        clearInterval(iv); settle();
        var live = document.getElementById('apps-live');
        if (live) {
          // Morph, don't innerHTML-replace: only the removed tile disappears, the rest of
          // the drawer keeps its identity/scroll (no flash).
          if (window.mdmMorph) { var fr = window.mdmMorph(live, html); if (window.htmx) fr.forEach(function (n) { try { htmx.process(n); } catch (e) {} }); }
          else { live.innerHTML = html; if (window.htmx) htmx.process(live); }
          try { appsAfterRefresh(); } catch (e) {}
        }
        if (gone) { try { window.mdmMarkUninstalled && window.mdmMarkUninstalled(pkg); } catch (e) {} }
      }
    }).catch(function () { if (tries >= 20) { clearInterval(iv); settle(); } });
  }, 1500);
};
// The apps list live-refreshes on check-in (hx-get → innerHTML swap), which drops the
// monogram tints and can leave the package count stale. The inline hx-on::after-swap
// is unreliable across htmx versions, so recolor + recount from a delegated listener
// too — whichever fires, the tiles stay coloured and the count stays honest.
document.body.addEventListener('htmx:afterSwap', function (e) {
  var t = e.target;
  if (t && (t.id === 'apps-live' || (t.querySelector && t.querySelector('#apps-table')))) {
    if (window.appsAfterRefresh) appsAfterRefresh();
  }
});
// Suppress the drawer's live refresh while an uninstall is being confirmed optimistically
// (replaces the CSP-blocked hx-on::before-request — inline hx-on needs new Function, which
// the page's Content-Security-Policy forbids, so it silently never ran).
document.body.addEventListener('htmx:beforeRequest', function (e) {
  var t = e.target;
  if (t && t.id === 'apps-live' && window.__uninstallPending) { e.preventDefault(); }
});
