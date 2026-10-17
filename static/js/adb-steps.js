// Loaded by templates/adb.html and templates/device.html.
// The wireless adb stepper both pages draw from GET /adb/progress (adb_flow.go): four steps
// per device — sent, device answered, adb listening, tunnel open — each pending / now / wait /
// ok / failed / skip, then the `adb connect` lines.
window.AdbSteps = window.AdbSteps || (function () {
  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }
  // steps(device) → the <ol> for one device.
  function steps(d) {
    var h = '<ol class="adb-st">';
    (d.steps || []).forEach(function (s) {
      h += '<li class="' + esc(s.state) + '"><span class="dot"></span><span>' + esc(s.label) +
        (s.detail ? '<small>' + esc(s.detail) + '</small>' : '') + '</span><span></span></li>';
    });
    return h + '</ol>';
  }
  // cmd(line, where) → one copyable command row.
  function cmd(line, where) {
    return '<div class="adb-cmd"><code>' + esc(line) + '</code>' + (where ? '<span class="w">' + esc(where) + '</span>' : '') +
      '<button type="button" class="cc-btn sm" data-cmd="' + esc(line) + '">Copy</button></div>';
  }
  function tag(d) {
    switch (d.state) {
      case 'ready': return '<span class="cc-tag ok">ready</span>';
      case 'failed': return '<span class="cc-tag bad">failed</span>';
      case 'working': return '<span class="cc-tag info">' + esc(d.word) + '…</span>';
      case 'cant': return '<span class="cc-tag">not available</span>';
    }
    return '<span class="cc-tag">off</span>';
  }
  function copy(text, btn) {
    if (window.copyText) { window.copyText(text, btn); return; }
    try { navigator.clipboard.writeText(text); } catch (e) {}
  }
  // One delegated handler for every Copy button these renderers draw.
  document.addEventListener('click', function (e) {
    var b = e.target.closest && e.target.closest('.adb-cmd [data-cmd]');
    if (b) copy(b.getAttribute('data-cmd'), b);
  });
  function fetchProgress(serials) {
    return fetch('/adb/progress?serials=' + encodeURIComponent(serials.join(',')), { credentials: 'same-origin', headers: { Accept: 'application/json' } })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); });
  }
  function settled(d) { return d.state === 'ready' || d.state === 'failed' || d.state === 'cant' || d.state === 'off'; }
  return { esc: esc, steps: steps, cmd: cmd, tag: tag, copy: copy, fetch: fetchProgress, settled: settled };
})();
