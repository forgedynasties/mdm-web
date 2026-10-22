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
  // By physical key (KeyboardEvent.code), so the device gets the key that was pressed
  // whatever the browser's layout; the device's own layout turns it into a character.
  var KBD = window.REMOTE_PAGE.kbd || 'keys'; // full | keys | dpc (remoteKbdLevel)
  var CODE = {
    Escape: 111, Tab: 61, Backspace: 67, Enter: 66, NumpadEnter: 160, Space: 62,
    ArrowUp: 19, ArrowDown: 20, ArrowLeft: 21, ArrowRight: 22,
    Home: 122, End: 123, PageUp: 92, PageDown: 93, Insert: 124, Delete: 112,
    Minus: 69, Equal: 70, BracketLeft: 71, BracketRight: 72, Backslash: 73, IntlBackslash: 73,
    Semicolon: 74, Quote: 75, Backquote: 68, Comma: 55, Period: 56, Slash: 76,
    ShiftLeft: 59, ShiftRight: 60, ControlLeft: 113, ControlRight: 114, AltLeft: 57, AltRight: 58,
    MetaLeft: 117, MetaRight: 118, CapsLock: 115, NumLock: 143, ScrollLock: 116,
    PrintScreen: 120, Pause: 121, ContextMenu: 82,
    NumpadDivide: 154, NumpadMultiply: 155, NumpadSubtract: 156, NumpadAdd: 157,
    NumpadDecimal: 158, NumpadComma: 159, NumpadEqual: 161,
    AudioVolumeUp: 24, AudioVolumeDown: 25, AudioVolumeMute: 164,
    MediaPlayPause: 85, MediaStop: 86, MediaTrackNext: 87, MediaTrackPrevious: 88
  };
  var i;
  for (i = 0; i < 26; i++) CODE['Key' + String.fromCharCode(65 + i)] = 29 + i;
  for (i = 0; i < 10; i++) { CODE['Digit' + i] = 7 + i; CODE['Numpad' + i] = 144 + i; }
  for (i = 1; i <= 12; i++) CODE['F' + i] = 130 + i;
  // Older clients get no modifiers, so a shifted symbol can only be sent as the key
  // that types it unshifted; these few have their own Android key.
  var SYMKEY = { '@': 77, '+': 81, '*': 17, '#': 18 };
  // Names for the log.
  var NAME = { 3: 'HOME', 4: 'BACK', 24: 'VOLUME_UP', 25: 'VOLUME_DOWN', 26: 'POWER', 187: 'APP_SWITCH' };
  Object.keys(CODE).forEach(function (k) {
    if (!NAME[CODE[k]]) NAME[CODE[k]] = k.replace(/^Key|^Digit/, '').replace(/([a-z])([A-Z])/g, '$1_$2').toUpperCase();
  });
  NAME[19] = 'DPAD_UP'; NAME[20] = 'DPAD_DOWN'; NAME[21] = 'DPAD_LEFT'; NAME[22] = 'DPAD_RIGHT'; NAME[67] = 'DEL'; NAME[112] = 'FORWARD_DEL';
  NAME[122] = 'MOVE_HOME'; NAME[123] = 'MOVE_END';
  // Android KeyEvent meta state bits (the generic and the left flag together).
  function metaOf(e) {
    var m = 0;
    if (e.shiftKey) m |= 0x41;
    if (e.altKey) m |= 0x12;
    if (e.ctrlKey) m |= 0x3000;
    if (e.metaKey) m |= 0x30000;
    if (e.getModifierState) {
      if (e.getModifierState('CapsLock')) m |= 0x100000;
      if (e.getModifierState('NumLock')) m |= 0x200000;
    }
    return m;
  }
  // A key that types a character: one code point (or a grapheme) in e.key.
  function printable(e) { return e.key && (e.key.length === 1 || /^\p{L}\p{M}*$|^\p{Extended_Pictographic}/u.test(e.key)) && e.key !== 'Dead'; }

  function setStatus(cls, text) {
    $('rc-status').className = 'rc-chip rc-status ' + cls;
    $('rc-status-text').textContent = text.charAt(0).toUpperCase() + text.slice(1);
    $('rc-fps-wrap').style.display = cls === 'live' ? '' : 'none';
    $('rc-link').textContent = text;
  }
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
        setStatus('live', 'connected'); enableControls(true);
        fpsStart = bwStart = performance.now();
        if (!sessionStart) { sessionStart = Date.now(); timerH = window.mdmTrackTimer ? window.mdmTrackTimer(setInterval(tickTimer, 1000)) : setInterval(tickTimer, 1000); }
      };
      ws.onmessage = function (e) {
        if (typeof e.data === 'string') { deviceText(e.data); return; }
        ensureCanvas(); if (!ctx) return;
        if (e.data.byteLength < 1) return;
        var b0 = new Uint8Array(e.data, 0, 1)[0];
        if (b0 <= 1) paintH264(e.data); else paintStill(e.data);
        noteFrameStats(e.data.byteLength);
      };
      ws.onclose = function () {
        enableControls(false); rcSetCapture(false);
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
  function live() { return ws && ws.readyState === WebSocket.OPEN; }
  function send(o) { if (live()) ws.send(JSON.stringify(o)); }
  function logLine(html) {
    var l = $('rc-log'); if (!l) return;
    var ph = $('rc-log-ph'); if (ph) ph.remove();
    var d = document.createElement('div'); d.innerHTML = html; l.appendChild(d);
    while (l.childNodes.length > 60) l.removeChild(l.firstChild);
    l.scrollTop = l.scrollHeight;
  }
  function esc(t) { return String(t).replace(/[&<>"]/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]; }); }
  function flashNote(t) {
    var n = $('rc-capnote'); if (!n) return;
    n.textContent = t; n.className = 'rc-capnote flash';
    clearTimeout(n._h); n._h = setTimeout(function () { n.className = 'rc-capnote' + (captureKbd ? '' : ' idle'); n.textContent = 'Click to use your keyboard here'; }, 1400);
  }
  function setupInput(el) {
    var pointerId = null;
    el.tabIndex = 0;
    el.setAttribute('role', 'application');
    el.setAttribute('aria-label', 'Device screen. Click or focus to send your keyboard to the device; Shift+Escape gives it back.');
    el.addEventListener('focus', function () { rcSetCapture(true); });
    el.addEventListener('blur', function () { rcSetCapture(false); });
    el.addEventListener('pointerdown', function (e) {
      el.focus({ preventScroll: true });
      if (e.button === 1) { e.preventDefault(); rcTap(3); logLine('<b>middle-click</b> → HOME'); return; }
      if (e.button === 2) { e.preventDefault(); rcWake(); rcTap(4); logLine('<b>right-click</b> → BACK'); return; }
      if (e.button > 0) return;
      pointerId = e.pointerId; el.setPointerCapture(pointerId); sendTouch(e, 'down'); ripple(e); e.preventDefault();
    });
    el.addEventListener('pointermove', function (e) { if (e.pointerId !== pointerId) return; sendTouch(e, 'move'); e.preventDefault(); });
    el.addEventListener('pointerup', function (e) { if (e.pointerId !== pointerId) return; sendTouch(e, 'up'); pointerId = null; e.preventDefault(); });
    el.addEventListener('pointercancel', function (e) { if (e.pointerId !== pointerId) return; sendTouch(e, 'up'); pointerId = null; });
    el.addEventListener('contextmenu', function (e) { e.preventDefault(); });
    el.addEventListener('auxclick', function (e) { e.preventDefault(); });
    var wheelAcc = 0, wheelH = null, wheelAt = null;
    el.addEventListener('wheel', function (e) {
      e.preventDefault();
      if (KBD !== 'full' || !live()) return;
      // One scroll event per animation frame, in notches (100 px of wheel = 1).
      wheelAcc += e.deltaMode === 1 ? e.deltaY * 33 : e.deltaY;
      wheelAt = e;
      if (wheelH) return;
      wheelH = requestAnimationFrame(function () {
        wheelH = null;
        var v = Math.max(-8, Math.min(8, -wheelAcc / 100)); wheelAcc = 0;
        if (!v) return;
        var p = pointFor(wheelAt); if (!p) return;
        send({ type: 'input_event', action: 'scroll', x: p.x, y: p.y, v: +v.toFixed(2), h: 0 });
      });
    }, { passive: false });
  }
  // Keys whose down went out as text (or as a page shortcut): their up is swallowed.
  var eaten = {}, repeats = {}, pressed = {}; // pressed: code → keycode sent down, not yet up
  document.addEventListener('keydown', function (e) { forwardKey(e, 'down'); }, true);
  document.addEventListener('keyup', function (e) { forwardKey(e, 'up'); }, true);
  var SHORTCUT = { KeyH: 3, KeyB: 4, KeyS: 187, KeyP: 26, ArrowUp: 24, ArrowDown: 25 };
  function forwardKey(e, phase) {
    var t = e.target, inField = t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable);
    if (!captureKbd) {
      if (phase === 'down' && !inField && (e.key === 'f' || e.key === 'F') && !e.metaKey && !e.ctrlKey && !e.altKey) rcFullscreen();
      return;
    }
    // Capturing: every key belongs to the device except Shift+Esc, the way out (said on
    // screen and in the Keyboard card, so this is not a keyboard trap).
    e.preventDefault(); e.stopPropagation();
    if (phase === 'up' && eaten[e.code]) { delete eaten[e.code]; return; }
    if (phase === 'down' && e.key === 'Escape' && e.shiftKey) { eaten[e.code] = true; view && view.blur(); return; }
    if (!live()) return;
    if (phase === 'down' && e.altKey && !e.ctrlKey && !e.metaKey && SHORTCUT[e.code]) {
      eaten[e.code] = true; rcTap(SHORTCUT[e.code]); logLine('<b>Alt+' + esc(e.key.length === 1 ? e.key.toUpperCase() : e.key.replace('Arrow', '')) + '</b> → ' + NAME[SHORTCUT[e.code]]);
      return;
    }
    if (phase === 'down' && (e.ctrlKey || e.metaKey) && !e.altKey && e.code === 'KeyV' && KBD !== 'dpc') {
      eaten[e.code] = true; pasteClipboard(); return;
    }
    if (KBD === 'full') {
      var altGr = e.getModifierState && e.getModifierState('AltGraph');
      var plain = /^[a-zA-Z0-9 ]$/.test(e.key);
      // Letters, digits and space go as keys (with Shift), so shortcuts and games see
      // keys; other characters (symbols, accents, any layout) go as text, which the
      // device types exactly whatever its own layout is.
      if (phase === 'down' && printable(e) && !plain && (!(e.ctrlKey || e.metaKey || e.altKey) || altGr)) {
        eaten[e.code] = true; sendText(e.key); return;
      }
    }
    var kc = CODE[e.code];
    if (KBD !== 'full' && phase === 'down' && SYMKEY[e.key]) { kc = SYMKEY[e.key]; }
    if (!kc) return;
    var meta = KBD === 'full' ? metaOf(e) : 0;
    var rep = 0;
    if (phase === 'down') { rep = e.repeat ? (repeats[e.code] = (repeats[e.code] || 0) + 1) : (repeats[e.code] = 0); }
    send({ type: 'input_event', action: 'key', event: phase, keycode: kc, meta: meta, repeat: rep });
    if (phase === 'down') pressed[e.code] = kc; else delete pressed[e.code];
    if (phase === 'down' && !rep && (kc < 57 || kc > 60) && (kc < 113 || kc > 118)) {
      var mods = (e.ctrlKey ? 'Ctrl+' : '') + (e.altKey ? 'Alt+' : '') + (e.shiftKey ? 'Shift+' : '') + (e.metaKey ? 'Meta+' : '');
      logLine('key <b>' + (KBD === 'full' ? mods : '') + (NAME[kc] || kc) + '</b>');
    }
  }
  function sendText(text) {
    if (!text || !live()) return;
    if (KBD === 'full') {
      send({ type: 'input_event', action: 'text', text: text });
    } else {
      // Older clients: one key per character they can type (a–z, 0–9, a few symbols).
      for (var j = 0; j < text.length; j++) {
        var c = text[j], kc = 0;
        if (/[a-z]/i.test(c)) kc = 29 + c.toLowerCase().charCodeAt(0) - 97;
        else if (/[0-9]/.test(c)) kc = 7 + c.charCodeAt(0) - 48;
        else if (c === ' ') kc = 62; else if (c === '\n') kc = 66;
        else kc = SYMKEY[c] || ({ '.': 56, ',': 55, '-': 69, '=': 70, '/': 76, ';': 74, "'": 75, '[': 71, ']': 72, '\\': 73, '`': 68 })[c] || 0;
        if (kc) { send({ type: 'input_event', action: 'key', event: 'down', keycode: kc }); send({ type: 'input_event', action: 'key', event: 'up', keycode: kc }); }
      }
    }
    logLine('text <b>"' + esc(text.length > 40 ? text.slice(0, 40) + '…' : text) + '"</b>');
  }
  function pasteClipboard() {
    if (!navigator.clipboard || !navigator.clipboard.readText) { logLine('<span class="x">This browser does not let the page read the clipboard: use the Clipboard card.</span>'); return; }
    navigator.clipboard.readText().then(function (t) {
      if (!t) { logLine('<span class="x">Your clipboard is empty.</span>'); return; }
      sendText(t);
    }).catch(function () { logLine('<span class="x">Clipboard access was refused: use the Clipboard card.</span>'); });
  }
  function pointFor(e) {
    if (!view) return null;
    var rect = view.getBoundingClientRect(); if (!rect.width || !rect.height) return null;
    return { x: Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width)), y: Math.max(0, Math.min(1, (e.clientY - rect.top) / rect.height)) };
  }
  function sendTouch(e, event) {
    var p = pointFor(e); if (!p) return;
    send({ type: 'input_event', action: 'touch', event: event, x: p.x, y: p.y });
  }
  function sendKey(keycode, event) { send({ type: 'input_event', action: 'key', event: event, keycode: keycode }); }

  // ── Public handlers ──
  window.rcTap = function (keycode) { sendKey(keycode, 'down'); sendKey(keycode, 'up'); };
  window.rcWake = function () { send({ type: 'wake_screen' }); };
  window.rcSetCapture = function (on) {
    if (captureKbd === on) return;
    captureKbd = on;
    if (!on) {
      // Let go of whatever is still held (the Shift of Shift+Esc), so the device is not
      // left with a key down.
      Object.keys(pressed).forEach(function (c) { sendKey(pressed[c], 'up'); });
      eaten = {}; repeats = {}; pressed = {};
    }
    ['rc-kchip', 'rc-kchip2'].forEach(function (id) { var c = $(id); if (c) c.classList.toggle('on', on); });
    $('rc-kchip-t').textContent = on ? 'Keyboard → device' : 'Keyboard: page';
    $('rc-kchip2-t').textContent = on ? 'On the device' : 'Page';
    var bz = $('rc-bezel'); if (bz) bz.classList.toggle('cap', on);
    var n = $('rc-capnote'); if (n) n.className = 'rc-capnote' + (on ? '' : ' idle');
    if (on) flashNote('Keyboard on the device · Shift+Esc to give it back');
    logLine('<span class="x">— keyboard ' + (on ? 'to the device' : 'back to the page') + ' —</span>');
  };
  window.rcSendText = function () {
    var input = $('rc-text'); if (!input) return;
    var text = input.value;
    if (!text) return;
    sendText(text);
    input.value = '';
  };
  window.rcCopyFromDevice = function () { send({ type: 'clipboard_get' }); logLine('<span class="x">Asking the device for its clipboard…</span>'); };
  function deviceText(msg) {
    var m; try { m = JSON.parse(msg); } catch (e) { return; }
    if (!m || m.type !== 'remote_clipboard') return;
    var t = m.text || '';
    var box = $('rc-text'); if (box) box.value = t;
    if (!t) { logLine('<span class="x">The device clipboard is empty.</span>'); return; }
    var done = function () { logLine('<span class="x">Device clipboard copied (' + t.length + ' characters).</span>'); };
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(t).then(done, function () { logLine('<span class="x">Device clipboard is in the box below: copy it from there.</span>'); });
    else done();
  }
  window.rcSetPaintMode = function (smooth) { paintMode = smooth ? 'perf' : 'feel'; };
  window.rcFullscreen = function () {
    var st = $('rc-stage');
    if (document.fullscreenElement) { document.exitFullscreen && document.exitFullscreen(); }
    else if (st.requestFullscreen) st.requestFullscreen();
  };
  // In fullscreen, Chrome and Edge let a page take keys the browser would otherwise keep
  // (Ctrl+W, Ctrl+T, Esc), so they reach the device too; holding Esc still leaves.
  document.addEventListener('fullscreenchange', function () {
    var kb = navigator.keyboard;
    if (document.fullscreenElement) { if (kb && kb.lock) kb.lock().catch(function () {}); if (view) view.focus({ preventScroll: true }); }
    else if (kb && kb.unlock) kb.unlock();
  });
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
