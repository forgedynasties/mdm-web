// Loaded by templates/logcat_live.html.
(function () {
  var es = null, running = false, lineCount = 0, pending = '';
  var MAX_LINES = 5000;
  var serial = window.LOGCAT_PAGE.serial;
  var console = document.getElementById('llc-console');
  var countEl = document.getElementById('llc-count');
  var statusEl = document.getElementById('llc-status');
  var statusText = document.getElementById('llc-status-text');
  var toggleBtn = document.getElementById('llc-toggle');
  var level = 'V';

  document.querySelectorAll('#llc-levels .llc-lv').forEach(function (b) {
    b.onclick = function () {
      document.querySelectorAll('#llc-levels .llc-lv').forEach(function (x) { x.classList.toggle('on', x === b); });
      level = b.dataset.lv;
      if (running) restart();
    };
  });
  ['llc-buffer','llc-tag','llc-grep','llc-tail'].forEach(function (id) {
    var el = document.getElementById(id);
    el.addEventListener('change', function () { if (running) restart(); });
  });

  function setStatus(state, text) {
    statusEl.className = 'llc-status' + (state ? ' ' + state : '');
    statusText.textContent = text;
  }

  function streamURL() {
    var p = new URLSearchParams();
    p.set('level', level);
    p.set('buffer', document.getElementById('llc-buffer').value);
    var tag = document.getElementById('llc-tag').value.trim(); if (tag) p.set('tag', tag);
    var grep = document.getElementById('llc-grep').value.trim(); if (grep) p.set('grep', grep);
    p.set('tail', document.getElementById('llc-tail').value || '200');
    return '/devices/' + encodeURIComponent(serial) + '/logcat/stream?' + p.toString();
  }

  // Parse the level char out of a `-v threadtime` line and wrap it for coloring.
  function renderLine(raw) {
    var span = document.createElement('span');
    span.className = 'llc-line';
    var m = raw.match(/^\d\d-\d\d \d\d:\d\d:\d\d\.\d+\s+\d+\s+\d+\s+([VDIWEF])\s/);
    if (m) span.classList.add('llc-' + m[1]);
    span.textContent = raw + '\n';
    return span;
  }

  function append(text) {
    pending += text;
    var idx = pending.lastIndexOf('\n');
    if (idx === -1) return;
    var complete = pending.slice(0, idx);
    pending = pending.slice(idx + 1);
    var lines = complete.split('\n');
    var frag = document.createDocumentFragment();
    for (var i = 0; i < lines.length; i++) frag.appendChild(renderLine(lines[i]));
    console.appendChild(frag);
    lineCount += lines.length;
    // Trim from the top to cap memory.
    while (lineCount > MAX_LINES && console.firstChild) {
      console.removeChild(console.firstChild);
      lineCount--;
    }
    countEl.textContent = lineCount.toLocaleString() + ' lines';
    if (document.getElementById('llc-autoscroll').checked) console.scrollTop = console.scrollHeight;
  }

  function start() {
    if (running) return;
    var empty = console.querySelector('.llc-empty');
    if (empty) console.innerHTML = '';
    running = true;
    toggleBtn.textContent = 'Stop';
    toggleBtn.classList.remove('btn-primary'); toggleBtn.classList.add('btn-danger');
    setStatus('', 'Connecting…');
    es = window.mdmStream(streamURL());
    es.onopen = function () { setStatus('live', 'Live'); };
    es.onmessage = function (e) { try { append(JSON.parse(e.data)); } catch (x) {} };
    es.addEventListener('end', function () { setStatus('err', 'Stream ended'); stop(true); });
    es.onerror = function () {
      // EventSource auto-retries; surface the state but keep the connection object.
      setStatus('err', running ? 'Reconnecting…' : 'Disconnected');
    };
  }

  function stop(keepStatus) {
    running = false;
    if (es) { es.close(); es = null; }
    toggleBtn.textContent = 'Start';
    toggleBtn.classList.add('btn-primary'); toggleBtn.classList.remove('btn-danger');
    if (!keepStatus) setStatus('', 'Stopped');
  }

  function restart() { stop(true); start(); }

  window.llcToggle = function () { if (running) stop(); else start(); };
  window.llcClear = function () { console.innerHTML = ''; lineCount = 0; pending = ''; countEl.textContent = '0 lines'; };

  // Download the captured log buffer as a .log file. Uses the console's text (what's
  // on screen after level/tag/grep filtering), named by serial + timestamp.
  window.llcSave = function () {
    var text = (console.innerText || console.textContent || '').replace(/​/g, '');
    if (!text.trim()) return;
    var stamp = new Date().toISOString().replace(/[:.]/g, '-').replace('T', '_').slice(0, 19);
    var name = serial + '-logcat-' + stamp + '.log';
    var blob = new Blob([text], { type: 'text/plain' });
    var url = URL.createObjectURL(blob);
    var a = document.createElement('a');
    a.href = url; a.download = name;
    document.body.appendChild(a); a.click(); a.remove();
    setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
  };

  // Stop the device-side stream cleanly when leaving the page.
  window.addEventListener('beforeunload', function () { if (es) es.close(); });
})();
