// Loaded by templates/remote.html.
(function() {
  var serial = window.REMOTE_PAGE.serial;
  var online = window.REMOTE_PAGE.online;

  // Size the console to the exact space below the shell chrome, so the page
  // never scrolls: measure where the console actually starts and pin its height.
  var con = document.getElementById('rc-console');
  function rcFit() {
    con.style.setProperty('--rc-top', Math.max(0, con.getBoundingClientRect().top + window.scrollY) + 'px');
  }
  rcFit();
  window.addEventListener('resize', rcFit);

  var protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  // H.264 by default whenever the browser can decode video (WebCodecs); devices
  // without an H.264 encoder fall back to JPEG stills and we auto-detect per frame.
  // Force stills with ?codec=jpeg; pin ?max_fps= ?bitrate= ?quality= ?scale=.
  var qp = new URLSearchParams(location.search);
  var hasWebCodecs = (typeof VideoDecoder !== 'undefined');
  var codec = qp.get('codec');
  if (codec !== 'jpeg' && codec !== 'h264') codec = hasWebCodecs ? 'h264' : 'jpeg';
  if (codec === 'h264' && !hasWebCodecs) codec = 'jpeg';
  function passNum(name) {
    var v = qp.get(name);
    return (v && /^[0-9]+(\.[0-9]+)?$/.test(v)) ? ('&' + name + '=' + encodeURIComponent(v)) : '';
  }
  // The page carries one single-use token for the first connect; every later
  // connect (auto-reconnect, codec fallback) mints a fresh one — a redeemed token
  // is refused, so reusing it made every reconnect fail.
  var pageToken = window.REMOTE_PAGE.token;
  function wsUrlFor(tok) {
    return protocol + '//' + location.host + '/api/v1/remote/' + encodeURIComponent(serial)
      + '?token=' + encodeURIComponent(tok)
      + '&codec=' + codec
      + passNum('max_fps') + passNum('bitrate') + passNum('quality') + passNum('scale');
  }
  function withToken(cb) {
    if (pageToken) { var t = pageToken; pageToken = ''; cb(t); return; }
    fetch('/devices/' + encodeURIComponent(serial) + '/remote/token', { credentials: 'same-origin', cache: 'no-store' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (j) { cb(j && j.token || ''); })
      .catch(function () { cb(''); });
  }
  document.getElementById('rc-codec').textContent = codec === 'h264' ? 'H.264' : 'JPEG';

  var ws = null, view = null, ctx = null, vdec = null, vdecReady = false;
  var captureKbd = false, decodeErrors = 0, frameCount = 0, paintedCount = 0;
  var totalFrames = 0, totalBytes = 0, fpsStart = 0, bwStart = 0, bwBytes = 0;
  var sessionStart = 0, timerH = null, ended = false;
  var everPainted = false, switchingCodec = false;

  function $(id) { return document.getElementById(id); }
  function step(n, done) {
    var s = $('rc-steps'); if (!s) return;
    s.querySelectorAll('span').forEach(function (el) {
      var k = +el.getAttribute('data-step');
      el.className = k < n || (k === n && done) ? 'done' : (k === n ? 'on' : '');
    });
  }

  // ── Browser key → Android keycode ──
  var AKEY = { 'Backspace': 67, 'Tab': 61, 'Enter': 66, 'Escape': 111, ' ': 62,
    'ArrowUp': 19, 'ArrowDown': 20, 'ArrowLeft': 21, 'ArrowRight': 22,
    'Home': 122, 'End': 123, 'Delete': 112, 'PageUp': 92, 'PageDown': 93,
    '.': 56, ',': 55, '-': 69, '=': 70, '/': 76, ';': 74, "'": 75,
    '[': 71, ']': 72, '\\': 73, '`': 68, '@': 77, '+': 81, '*': 17, '#': 18 };
  function keycodeFor(key) {
    if (key == null) return 0;
    if (AKEY.hasOwnProperty(key)) return AKEY[key];
    if (key.length === 1) {
      var c = key.toLowerCase();
      if (c >= 'a' && c <= 'z') return 29 + (c.charCodeAt(0) - 97);
      if (c >= '0' && c <= '9') return 7 + (c.charCodeAt(0) - 48);
    }
    return 0;
  }

  function setStatus(cls, text) { $('rc-status').className = 'rc-status ' + cls; $('rc-status-text').textContent = text; $('rc-link').textContent = text; }
  function enableControls(on) {
    document.querySelectorAll('[data-needs-conn]').forEach(function (el) { el.disabled = !on; });
    $('rc-disconnect').style.display = on ? '' : 'none';
  }
  function fmtKB(b) { return b < 1048576 ? (b / 1024).toFixed(0) + ' KB' : (b / 1048576).toFixed(1) + ' MB'; }
  function tickTimer() {
    if (!sessionStart) return;
    var s = Math.floor((Date.now() - sessionStart) / 1000);
    $('rc-timer').textContent = (s >= 3600 ? Math.floor(s / 3600) + ':' : '') + ('0' + Math.floor(s % 3600 / 60)).slice(-2) + ':' + ('0' + s % 60).slice(-2);
  }

  function ensureCanvas() {
    if (ctx) return;
    var ph = $('rc-placeholder'); if (!ph) return;
    view = document.createElement('canvas'); view.id = 'rc-canvas'; view.width = 1; view.height = 1;
    ctx = view.getContext('2d');
    ph.parentNode.replaceChild(view, ph);
    setupInput(view);
    step(3, true);
  }
  function frameDecodeFailed(err, bytes) {
    decodeErrors++;
    $('rc-err-chip').style.display = ''; $('rc-err').textContent = decodeErrors;
    if (decodeErrors <= 3) console.error('[remote] frame decode failed (' + bytes + ' bytes):', err);
  }
  // This browser has WebCodecs but can't decode the stream (no H.264 decoder —
  // e.g. a Chromium build without proprietary codecs, or hardware decode off). The
  // closed decoder used to swallow every later frame: a blank screen while frames
  // kept arriving. Reconnect once asking the device for JPEG stills instead.
  function h264Failed(err, bytes) {
    frameDecodeFailed(err, bytes);
    vdecReady = false;
    if (codec !== 'h264' || everPainted || switchingCodec || !ws) return;
    console.warn('[remote] H.264 decode unavailable in this browser, switching to JPEG stills');
    codec = 'jpeg'; $('rc-codec').textContent = 'JPEG';
    switchingCodec = true;
    try { ws.close(); } catch (e) {}
  }
  function resizeTo(w, h) {
    if (view.width !== w || view.height !== h) { view.width = w; view.height = h; $('rc-res').textContent = w + '×' + h; }
  }
  function paintStill(data) {
    createImageBitmap(new Blob([data], { type: 'image/jpeg' })).then(function (bmp) {
      resizeTo(bmp.width, bmp.height); ctx.drawImage(bmp, 0, 0); bmp.close(); paintedCount++; everPainted = true;
    }).catch(function (err) { frameDecodeFailed(err, data.byteLength); });
  }

  // H.264 via WebCodecs. Wire: [type byte][payload]; 1 = key frame (SPS/PPS+IDR), 0 = delta.
  var vts = 0, sawKey = false, pendingFrame = null, rafScheduled = false, paintMode = 'feel';
  function drawLatest() {
    rafScheduled = false;
    var frame = pendingFrame; pendingFrame = null;
    if (!frame) return;
    if (ctx) { resizeTo(frame.displayWidth, frame.displayHeight); ctx.drawImage(frame, 0, 0, frame.displayWidth, frame.displayHeight); paintedCount++; everPainted = true; }
    frame.close();
  }
  function initDecoder() {
    vdec = new VideoDecoder({
      output: function (frame) {
        if (paintMode === 'perf') {
          if (ctx) { resizeTo(frame.displayWidth, frame.displayHeight); ctx.drawImage(frame, 0, 0, frame.displayWidth, frame.displayHeight); paintedCount++; everPainted = true; }
          frame.close(); return;
        }
        if (pendingFrame) pendingFrame.close();
        pendingFrame = frame;
        if (!rafScheduled) { rafScheduled = true; requestAnimationFrame(drawLatest); }
      },
      error: function (err) { h264Failed(err, 0); }
    });
    try { vdec.configure({ codec: 'avc1.42E01E', optimizeForLatency: true }); vdecReady = true; } catch (err) { h264Failed(err, 0); }
  }
  function paintH264(data) {
    if (!vdec) initDecoder();
    if (!vdecReady) return;
    var bytes = new Uint8Array(data);
    if (bytes.length < 2) return;
    var isKey = bytes[0] === 1;
    if (!sawKey) { if (!isKey) return; sawKey = true; }
    if (!isKey && vdec.decodeQueueSize > 4) { sawKey = false; return; } // resync at next key frame
    try {
      vdec.decode(new EncodedVideoChunk({ type: isKey ? 'key' : 'delta', timestamp: (vts += Math.round(1e6 / 15)), data: bytes.subarray(1) }));
    } catch (err) { h264Failed(err, bytes.length); sawKey = false; }
  }

  function noteFrameStats(bytes) {
    frameCount++; totalFrames++; bwBytes += bytes; totalBytes += bytes;
    var now = performance.now();
    if (now - fpsStart >= 1000) {
      $('rc-fps').textContent = frameCount;
      $('rc-painted').textContent = paintedCount + ' / ' + frameCount + ' fps';
      var q = (vdec && typeof vdec.decodeQueueSize === 'number') ? vdec.decodeQueueSize : 0;
      $('rc-lag').style.display = q > 2 ? '' : 'none'; $('rc-lag-n').textContent = q;
      frameCount = 0; paintedCount = 0; fpsStart = now;
      $('rc-frames').textContent = totalFrames; $('rc-data').textContent = fmtKB(totalBytes);
    }
    if (now - bwStart >= 1000) { $('rc-kbps').textContent = (bwBytes / 1024 / ((now - bwStart) / 1000)).toFixed(1); bwBytes = 0; bwStart = now; }
  }

  // ── Connection with auto-reconnect (3 tries, countdown) ──
  var attempts = 0, retryH = null;
  function connect() {
    withToken(function (tok) {
      if (ended) return;
      ws = window.mdmTrackStream(new WebSocket(wsUrlFor(tok)));
      ws.binaryType = 'arraybuffer';
      setStatus('connecting', attempts ? 'reconnecting' : 'connecting');
      ws.onopen = function () {
        attempts = 0; step(2, false);
        setStatus('live', 'live'); enableControls(true);
        fpsStart = bwStart = performance.now();
        if (!sessionStart) { sessionStart = Date.now(); timerH = window.mdmTrackTimer ? window.mdmTrackTimer(setInterval(tickTimer, 1000)) : setInterval(tickTimer, 1000); }
      };
      ws.onmessage = function (e) {
        if (typeof e.data === 'string') return;
        ensureCanvas(); if (!ctx) return;
        if (e.data.byteLength < 1) return;
        var b0 = new Uint8Array(e.data, 0, 1)[0];
        if (b0 <= 1) paintH264(e.data); else paintStill(e.data);
        noteFrameStats(e.data.byteLength);
      };
      ws.onclose = function () {
        enableControls(false); captureKbd = false;
        var kbd = $('rc-kbd'); if (kbd) kbd.checked = false;
        if (view) { view = null; ctx = null; }
        if (vdec) { try { vdec.close(); } catch (e2) {} vdec = null; vdecReady = false; sawKey = false; }
        if (pendingFrame) { try { pendingFrame.close(); } catch (e3) {} pendingFrame = null; }
        if (ended) { showEnded('Session ended', 'You closed the remote session.'); return; }
        if (switchingCodec) {
          switchingCodec = false;
          showEnded('Switching to compatible video', 'This browser can\'t decode H.264, reconnecting with still frames…', true);
          connect(); return;
        }
        if (attempts < 3) {
          attempts++;
          var wait = 2 * attempts;
          setStatus('connecting', 'reconnecting');
          showEnded('Connection dropped', 'Reconnecting in <b id="rc-cd">' + wait + '</b>s… (try ' + attempts + ' of 3)', true);
          var left = wait;
          retryH = setInterval(function () { left--; var c = $('rc-cd'); if (c) c.textContent = left; if (left <= 0) { clearInterval(retryH); connect(); } }, 1000);
        } else {
          setStatus('error', 'disconnected');
          showEnded('Could not reconnect', 'The device stopped answering. Check that it is online, then start a new session.');
        }
      };
      ws.onerror = function () { setStatus('error', 'error'); };
    });
  }
  function showEnded(title, body, spinning) {
    var screen = $('rc-screen');
    screen.innerHTML = '<div class="rc-placeholder" id="rc-placeholder">' +
      (spinning ? '<div class="rc-spinner"></div>' : '<div class="rc-ph-icon"><svg viewBox="0 0 24 24"><path d="M1 1l22 22"/><path d="M16.7 11.1a6 6 0 0 0-1.8-1.2M5 12.6a10 10 0 0 1 5.1-2.5M2 8.8a15 15 0 0 1 4.2-2.5M9 16l3-3 3 3"/></svg></div>') +
      '<h2>' + title + '</h2><p>' + body + '</p>' +
      (spinning ? '' : '<div class="rc-ph-row"><button class="rc-ph-btn" type="button" onclick="location.reload()">Start a new session</button><a class="rc-ph-btn" href="/devices/' + encodeURIComponent(serial) + '">Back to device</a></div>') +
      '</div>';
  }

  // ── Input ──
  function ripple(e) {
    var st = $('rc-stage'), r = st.getBoundingClientRect();
    var d = document.createElement('div'); d.className = 'rc-ripple';
    d.style.left = (e.clientX - r.left) + 'px'; d.style.top = (e.clientY - r.top) + 'px';
    st.appendChild(d); setTimeout(function () { d.remove(); }, 500);
  }
  function setupInput(el) {
    var pointerId = null;
    el.addEventListener('pointerdown', function (e) { if (e.button > 0) return; pointerId = e.pointerId; el.setPointerCapture(pointerId); sendTouch(e, 'down'); ripple(e); e.preventDefault(); });
    el.addEventListener('pointermove', function (e) { if (e.pointerId !== pointerId) return; sendTouch(e, 'move'); e.preventDefault(); });
    el.addEventListener('pointerup', function (e) { if (e.pointerId !== pointerId) return; sendTouch(e, 'up'); pointerId = null; e.preventDefault(); });
    el.addEventListener('pointercancel', function (e) { if (e.pointerId !== pointerId) return; sendTouch(e, 'up'); pointerId = null; });
    el.addEventListener('contextmenu', function (e) { e.preventDefault(); rcWake(); });
  }
  document.addEventListener('keydown', function (e) { forwardKey(e, 'down'); });
  document.addEventListener('keyup', function (e) { forwardKey(e, 'up'); });
  function forwardKey(e, event) {
    var t = e.target, inField = t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA');
    if (!captureKbd && event === 'down' && !inField && (e.key === 'f' || e.key === 'F') && !e.metaKey && !e.ctrlKey && !e.altKey) { rcFullscreen(); return; }
    if (!captureKbd || !ws || ws.readyState !== WebSocket.OPEN) return;
    if (inField) return;
    // Never trap the keys a keyboard-only user needs to leave capture (WCAG 2.1.2):
    // Tab must move focus away, and Escape exits capture. Let both reach the browser.
    if (e.key === 'Tab') return;
    if (e.key === 'Escape') { if (event === 'down') rcSetCapture(false); return; }
    var kc = keycodeFor(e.key); if (!kc) return;
    e.preventDefault(); sendKey(kc, event);
  }
  function sendTouch(e, event) {
    if (!ws || ws.readyState !== WebSocket.OPEN || !view) return;
    var rect = view.getBoundingClientRect(); if (!rect.width || !rect.height) return;
    var x = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
    var y = Math.max(0, Math.min(1, (e.clientY - rect.top) / rect.height));
    ws.send(JSON.stringify({ type: 'input_event', action: 'touch', event: event, x: x, y: y }));
  }
  function sendKey(keycode, event) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: 'input_event', action: 'key', event: event, keycode: keycode }));
  }

  // ── Public handlers ──
  window.rcTap = function (keycode) { sendKey(keycode, 'down'); sendKey(keycode, 'up'); };
  window.rcWake = function () { if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'wake_screen' })); };
  window.rcSetCapture = function (on) {
    captureKbd = on;
    var kbd = $('rc-kbd'); if (kbd) kbd.checked = on;
    $('rc-kbd-state').textContent = on ? 'capturing' : 'off';
    var h = $('rc-kbd-hint'); h.className = 'rc-hint' + (on ? ' on' : '');
    h.textContent = on ? 'Capturing: keys go to the device. Press Esc to stop.' : 'When on, keys you press are sent to the device. Esc turns it off.';
  };
  window.rcSendText = function () {
    var input = $('rc-text'), text = input.value;
    if (!text || !ws || ws.readyState !== WebSocket.OPEN) return;
    for (var i = 0; i < text.length; i++) { var kc = keycodeFor(text[i]); if (kc) { sendKey(kc, 'down'); sendKey(kc, 'up'); } }
    input.value = '';
  };
  window.rcSetPaintMode = function (smooth) { paintMode = smooth ? 'perf' : 'feel'; };
  window.rcFullscreen = function () {
    var st = $('rc-stage');
    if (document.fullscreenElement) { document.exitFullscreen && document.exitFullscreen(); }
    else if (st.requestFullscreen) st.requestFullscreen();
  };
  window.rcSnapshot = function () {
    if (!view) return;
    try { var w = window.open(); if (w) { w.document.write('<title>' + serial + ' snapshot</title><img style="max-width:100%" src="' + view.toDataURL('image/png') + '">'); w.document.close(); } } catch (e) {}
  };
  window.rcDisconnect = function () {
    ended = true;
    if (ws) { try { ws.close(); } catch (e) {} }
    window.location.href = '/devices/' + encodeURIComponent(serial);
  };

  if (online) connect();
})();
