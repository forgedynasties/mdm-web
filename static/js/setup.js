// Loaded by templates/setup.html.
// Direct-to-S3 APK upload (presigned PUT → register parses package/version/icon → reload).
function fmtBytes(n) { if (n < 1024) return n + ' B'; if (n < 1048576) return (n / 1024).toFixed(0) + ' KB'; if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB'; return (n / 1073741824).toFixed(2) + ' GB'; }
function s3Put(url, file, onProgress) {
  return new Promise(function (resolve, reject) {
    var xhr = new XMLHttpRequest();
    xhr.open('PUT', url);
    xhr.setRequestHeader('Content-Type', 'application/vnd.android.package-archive');
    xhr.upload.onprogress = function (e) { if (e.lengthComputable && onProgress) onProgress(e.loaded, e.total); };
    xhr.onload = function () { (xhr.status >= 200 && xhr.status < 300) ? resolve() : reject(new Error('S3 upload failed (' + xhr.status + ')')); };
    xhr.onerror = function () { reject(new Error('network error uploading to S3')); };
    xhr.send(file);
  });
}
function ensureParser() {
  return new Promise(function (resolve, reject) {
    if (window.AppInfoParser) return resolve();
    var s = document.createElement('script'); s.src = '/static/vendor/app-info-parser.min.js';
    s.onload = function () { resolve(); }; s.onerror = function () { reject(new Error('could not load APK parser')); };
    document.head.appendChild(s);
  });
}
function iconPng(iconStr) {
  return new Promise(function (resolve) {
    if (!iconStr) return resolve('');
    var src = iconStr.indexOf('data:') === 0 ? iconStr : ('data:image/png;base64,' + iconStr);
    var img = new Image();
    img.onload = function () { try { var c = document.createElement('canvas'); c.width = 96; c.height = 96; c.getContext('2d').drawImage(img, 0, 0, 96, 96); resolve(c.toDataURL('image/png').split(',')[1] || ''); } catch (e) { resolve(''); } };
    img.onerror = function () { resolve(''); };
    img.src = src;
  });
}
function setupUploadApk(file) {
  if (!file) return;
  var el = document.getElementById('setup-upload-status');
  var set = function (t, err) { if (!el) return; el.hidden = false; el.textContent = t; el.style.color = err ? 'var(--danger,#c0392b)' : 'var(--accent-text)'; };
  var meta = { pkg: '', name: '', icon: '' };
  set('Reading ' + file.name + ' (' + fmtBytes(file.size) + ')…');
  ensureParser()
    .then(function () { return new AppInfoParser(file).parse(); })
    .then(function (info) {
      meta.pkg = info.package || '';
      // Label may be a string OR an array (multi-locale) — always coerce to one string,
      // or the server's JSON decode (name is a string) fails.
      var lbl = (info.application && info.application.label) || info.label || meta.pkg;
      if (Array.isArray(lbl)) lbl = lbl[0];
      meta.name = String(lbl || meta.pkg);
      return iconPng(info.icon).then(function (png) { meta.icon = png; });
    })
    .catch(function () { /* client parse failed → server falls back to S3 parse */ })
    .then(function () { return fetch('/apps/upload-url', { method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin', body: JSON.stringify({ filename: file.name }) }); })
    .then(function (r) { if (!r.ok) throw new Error('could not get upload URL'); return r.json(); })
    .then(function (d) {
      set('Uploading ' + file.name + ' (' + fmtBytes(file.size) + ')…');
      return s3Put(d.url, file, function (loaded, total) {
        set('Uploading ' + file.name + ' — ' + Math.round(loaded / total * 100) + '% (' + fmtBytes(loaded) + ' / ' + fmtBytes(total) + ')');
      }).then(function () { return d.key; });
    })
    .then(function (key) {
      set('Saving…');
      return fetch('/apps/register', { method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin', body: JSON.stringify({ key: key, package: meta.pkg, name: meta.name, icon: meta.icon }) })
        .then(function (r) { return r.json().then(function (j) { if (!r.ok) throw new Error(j.error || 'register failed'); return j; }); });
    })
    .then(function (app) { set('Added "' + (app.name || 'app') + '". Reloading…'); setTimeout(function () { location.reload(); }, 800); })
    .catch(function (err) { set('Error: ' + err.message, true); });
}
function openRepositoryAppModal() {
  var modal = document.getElementById('repository-app-modal');
  if (!modal) return;
  // Reset to "add" mode.
  document.getElementById('repository-app-form').action = '/setup/apps';
  document.getElementById('repository-app-title').textContent = 'Add App';
  document.getElementById('repository-app-submit').lastChild.textContent = 'Add App';
  document.getElementById('repository-app-name').value = '';
  document.getElementById('repository-app-pkg').value = '';
  document.getElementById('repository-app-apk').value = '';
  modal.classList.add('modal-open');
  var input = document.getElementById('repository-app-name');
  if (input) setTimeout(function() { input.focus(); }, 0);
}

// Reuse the same modal for editing: prefill fields and point the form at the
// app's edit endpoint.
function openEditAppModal(btn) {
  var modal = document.getElementById('repository-app-modal');
  if (!modal) return;
  document.getElementById('repository-app-form').action = '/setup/apps/' + btn.dataset.id + '/edit';
  document.getElementById('repository-app-title').textContent = 'Edit App';
  document.getElementById('repository-app-submit').lastChild.textContent = 'Save';
  document.getElementById('repository-app-name').value = btn.dataset.name || '';
  document.getElementById('repository-app-pkg').value = btn.dataset.pkg || '';
  document.getElementById('repository-app-apk').value = btn.dataset.apk || '';
  modal.classList.add('modal-open');
  setTimeout(function() { document.getElementById('repository-app-name').focus(); }, 0);
}

function closeRepositoryAppModal(e) {
  var modal = document.getElementById('repository-app-modal');
  if (!modal) return;
  if (!e || e.target === modal) modal.classList.remove('modal-open');
}

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') closeRepositoryAppModal();
});
