// Loaded by templates/release_workspace.html.
// Package composer type toggle (Full / Incremental).
function setPkgType(mode) {
  var box = document.querySelector('.composer'); if (!box) return;
  var fullBtn = box.querySelector('.tcard[data-mode="full"]');
  if (mode === 'full' && fullBtn && fullBtn.disabled) return; // only one full per release
  document.getElementById('pkg-type-input').value = mode;
  var srcRow = document.getElementById('source-build-row');
  var srcInput = document.getElementById('pkg-source-build');
  if (srcRow) srcRow.style.display = mode === 'incremental' ? 'block' : 'none';
  if (srcInput) srcInput.required = mode === 'incremental';
  box.querySelectorAll('.tcard').forEach(function (b) { b.classList.toggle('on', b.dataset.mode === mode); });
}
function pkgReset() { var o = document.getElementById('pkg-detected'); if (o) o.innerHTML = ''; }
// Verify: read the update package's metadata by URL (server range-reads it, no download).
function pkgVerify() {
  var el = document.getElementById('pkg-update-url'); var url = (el.value || '').trim();
  var out = document.getElementById('pkg-detected'); if (!out) return;
  if (!url) { out.innerHTML = '<span class="pkg-bad">Enter a package URL first.</span>'; return; }
  out.innerHTML = '<span class="muted small"><span class="ws-dot"></span>Reading package metadata…</span>';
  var url64 = btoa(unescape(encodeURIComponent(url))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  var body = new URLSearchParams(); body.set('update_url_b64', url64);
  fetch('/releases/' + window.RELEASE_PAGE.release + '/packages/inspect', { method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' }, body: body })
    .then(function (r) { return r.json(); })
    .then(function (d) {
      if (!d.ok) { out.innerHTML = '<span class="pkg-bad">✗ ' + (d.error || 'Could not read the package') + '</span>'; return; }
      if (d.type) setPkgType(d.type);
      var sz = d.size_bytes > 1073741824 ? (d.size_bytes / 1073741824).toFixed(2) + ' GB' : Math.round(d.size_bytes / 1048576) + ' MB';
      out.innerHTML = '<span class="pkg-ok">✓ Reachable</span> <span class="pkg-chip">' + sz + '</span> <span class="pkg-chip">' + d.type + (d.device ? ' · for ' + d.device : '') + '</span>';
    })
    .catch(function () { out.innerHTML = '<span class="pkg-bad">✗ Couldn’t reach the package.</span>'; });
}
// base64-encode the add-package URL on submit (WAF bypass), mirroring the QFIL form.
(function () {
  var form = document.getElementById('add-pkg-form');
  if (!form) return;
  form.addEventListener('submit', function () {
    var urlInput = document.getElementById('pkg-update-url');
    var b64Input = document.getElementById('pkg-update-url-b64');
    if (!urlInput || !b64Input || !urlInput.value) return;
    b64Input.value = btoa(unescape(encodeURIComponent(urlInput.value))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
    urlInput.removeAttribute('name');
  });
})();
(function () {
  var form = document.getElementById('add-qfil-form');
  if (!form) return;
  form.addEventListener('submit', function () {
    var urlInput = document.getElementById('qfil-url');
    var b64Input = document.getElementById('qfil-url-b64');
    if (!urlInput || !b64Input || !urlInput.value) return;
    b64Input.value = btoa(unescape(encodeURIComponent(urlInput.value))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
    urlInput.removeAttribute('name');
  });
})();
