// Loaded by templates/shell_console.html.
(function () {
  var card = document.getElementById('sh-card'); if (!card) return;
  var serial = card.getAttribute('data-serial');
  var enc = encodeURIComponent(serial);

  // ── Tabs ──
  function showTab(name) {
    card.querySelectorAll('.sh-tab').forEach(function (t) { var on = t.getAttribute('data-sh-tab') === name; t.classList.toggle('on', on); t.setAttribute('aria-selected', on ? 'true' : 'false'); });
    card.querySelectorAll('.sh-pane').forEach(function (p) { p.hidden = p.getAttribute('data-sh-pane') !== name; });
    var ctl = document.getElementById('sh-ctl'); if (ctl) ctl.hidden = name !== 'term';
    if (name === 'term' && window.__shTerm) { window.__shFit(); window.__shTerm.focus(); }
    if (name === 'queue') { var qi = document.getElementById('sh-qin'); if (qi) qi.focus(); }
  }
  card.querySelectorAll('.sh-tab').forEach(function (t) { t.addEventListener('click', function () { showTab(t.getAttribute('data-sh-tab')); }); });

  // ── Queued commands ──
  var qform = document.getElementById('sh-qform'), qin = document.getElementById('sh-qin'), qlist = document.getElementById('sh-qlist');
  card.querySelectorAll('.sh-chip').forEach(function (c) { c.addEventListener('click', function () { qin.value = c.getAttribute('data-cmd'); qin.focus(); }); });
  function qRow(cmd) {
    var none = document.getElementById('sh-qnone'); if (none) none.remove();
    qlist.querySelectorAll('details[open]').forEach(function (d) { d.open = false; });
    var d = document.createElement('details'); d.className = 'sh-q'; d.open = true;
    var s = document.createElement('summary');
    var dot = document.createElement('span'); dot.className = 'sh-qd wait';
    var code = document.createElement('code'); code.textContent = cmd;
    var st = document.createElement('span'); st.className = 'sh-qs'; st.textContent = 'sending';
    var t = document.createElement('span'); t.className = 'sh-qt'; t.textContent = 'just now';
    s.appendChild(dot); s.appendChild(code); s.appendChild(st); s.appendChild(t);
    var pre = document.createElement('pre'); pre.className = 'sh-qo sh-qo-wait'; pre.textContent = 'waiting for the device…';
    d.appendChild(s); d.appendChild(pre);
    qlist.insertBefore(d, qlist.firstChild);
    var n = document.getElementById('sh-qn');
    if (n) n.textContent = String(+n.textContent + 1);
    return { dot: dot, st: st, pre: pre };
  }
  if (qform) qform.addEventListener('submit', function (e) {
    e.preventDefault();
    // Sent in place, not a page submit: drop the page-wide busy spinner at once.
    var sb = qform.querySelector('button[type=submit]');
    setTimeout(function () { sb.classList.remove('is-busy'); sb.removeAttribute('aria-busy'); }, 0);
    var cmd = qin.value.trim(); if (!cmd) return;
    qin.value = '';
    var row = qRow(cmd);
    fetch('/devices/' + enc + '/commands', {
      method: 'POST',
      headers: { 'Accept': 'application/json', 'Content-Type': 'application/x-www-form-urlencoded' },
      body: 'type=shell&shell_cmd=' + encodeURIComponent(cmd)
    })
      .then(function (r) { if (!r.ok) throw new Error('send failed (' + r.status + ')'); return r.json(); })
      .then(function (d) {
        row.st.textContent = 'queued';
        var es = window.mdmStream('/commands/' + d.id + '/output/' + enc + '/stream');
        es.onmessage = function (ev) {
          if (row.pre.classList.contains('sh-qo-wait')) { row.pre.classList.remove('sh-qo-wait'); row.pre.textContent = ''; }
          row.st.textContent = 'running'; row.pre.textContent += JSON.parse(ev.data);
        };
        es.addEventListener('done', function (ev) {
          var s = ''; try { s = (JSON.parse(ev.data) || {}).status || ''; } catch (_) {}
          row.st.textContent = s || 'completed';
          row.dot.className = 'sh-qd ' + (!s || s === 'completed' ? 'ok' : 'err');
          if (row.pre.classList.contains('sh-qo-wait')) { row.pre.classList.remove('sh-qo-wait'); row.pre.textContent = '(no output)'; }
          es.close();
        });
        es.onerror = function () { es.close(); };
      })
      .catch(function (err) { row.st.textContent = 'failed'; row.dot.className = 'sh-qd err'; row.pre.textContent = err.message; });
  });

  // ── Terminal (live session) ──
  var host = document.getElementById('sh-xterm');
  if (!host) return;
  // xterm is loaded on first use (the page can arrive by a boosted navigation, where a
  // <script src> would not be waited for).
  function need(src, ready) {
    if (ready()) return Promise.resolve();
    return new Promise(function (res) { var sc = document.createElement('script'); sc.src = src; sc.onload = res; sc.onerror = res; document.head.appendChild(sc); });
  }
  need('/static/vendor/xterm.min.js', function () { return !!window.Terminal; })
    .then(function () { return need('/static/vendor/xterm-addon-fit.min.js', function () { return !!window.FitAddon; }); })
    .then(function () { if (window.Terminal) initTerm(); else host.textContent = 'Could not load the terminal.'; });
  function initTerm() {
  var css = getComputedStyle(document.documentElement);
  var mono = (css.getPropertyValue('--mono') || '').trim() || 'ui-monospace, SFMono-Regular, Menlo, monospace';
  var term = new window.Terminal({
    convertEol: true, cursorBlink: true, fontFamily: mono, fontSize: 13, lineHeight: 1.25, scrollback: 5000,
    theme: { background: '#0b0d12', foreground: '#d7dae0', cursor: '#d7dae0', selectionBackground: 'rgba(120,140,200,.35)' }
  });
  var fit = window.FitAddon ? new window.FitAddon.FitAddon() : null;
  if (fit) term.loadAddon(fit);
  term.open(host);
  // xterm 5.3 measures glyphs in a 50000px-wide box on <body>, moved off-screen upwards
  // only; that widened the page. Move it off to the left as well.
  setTimeout(function () {
    document.querySelectorAll('body > div').forEach(function (d) { if (d.style.width === '50000px') d.style.left = '-50000px'; });
  }, 0);
  window.__shTerm = term;
  window.__shFit = function () { try { if (fit) fit.fit(); } catch (e) {} };
  window.__shFit();
  window.addEventListener('resize', window.__shFit);

  var es = null, sid = null, line = '', hist = [], hi = 0, opened = 0, timer = null;
  var stEl = document.getElementById('sh-st'), stT = document.getElementById('sh-st-t');
  var endBtn = document.getElementById('sh-end'), openBtn = document.getElementById('sh-open');
  function status(st, text) { stEl.setAttribute('data-st', st); stT.textContent = text; }
  function tick() {
    if (!sid) return;
    var m = Math.floor((Date.now() - opened) / 60000);
    status('on', 'Connected · ' + (m < 1 ? 'just now' : m < 60 ? m + 'm' : Math.floor(m / 60) + 'h ' + (m % 60) + 'm'));
  }
  function dim(s) { term.write('\x1b[90m' + s + '\x1b[0m'); }
  function stop(note) {
    if (es) { es.close(); es = null; }
    if (timer) { clearInterval(timer); timer = null; }
    if (note) dim('\r\n' + note + '\r\n');
    sid = null; line = '';
    status('off', 'Not connected');
    endBtn.hidden = true; openBtn.hidden = false;
  }
  function start() {
    if (es) return;
    status('wait', 'Connecting…');
    endBtn.hidden = false; openBtn.hidden = true;
    es = window.mdmStream('/devices/' + enc + '/shell/session');
    es.addEventListener('session', function (e) {
      sid = JSON.parse(e.data).id; opened = Date.now();
      tick(); timer = setInterval(tick, 30000);
      term.focus();
    });
    es.onmessage = function (e) { term.write(JSON.parse(e.data)); };
    es.addEventListener('end', function () { stop('[session ended]'); });
    // Never let the browser reconnect on its own: that would open a second shell.
    es.onerror = function () { stop(sid ? '[connection lost]' : '[could not open a session — is the device online?]'); };
  }
  function send(data) {
    if (!sid) return;
    fetch('/devices/' + enc + '/shell/session/' + sid + '/input', {
      method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: 'data=' + encodeURIComponent(data)
    }).then(function (r) { if (!r.ok) return r.text().then(function (t) { dim('\r\n[' + t.trim() + ']\r\n'); }); })
      .catch(function (err) { dim('\r\n[send failed: ' + err.message + ']\r\n'); });
  }
  function replaceLine(s) {
    term.write('\b \b'.repeat(line.length));
    line = s; term.write(line);
  }
  function submit() {
    term.write('\r\n');
    if (line.trim()) { hist.push(line); }
    hi = hist.length;
    send(line + '\n');
    line = '';
  }
  // Line editing happens here (the device's shell has no terminal to do it): printable
  // keys echo locally, Enter sends the whole line, Backspace, ↑/↓ history, Ctrl+C drops
  // the line and asks for a fresh prompt, Ctrl+L clears the screen. A paste of several
  // lines runs them one after another.
  term.onData(function (d) {
    if (!sid) return;
    if (d === '\r') { submit(); return; }
    if (d === '\x7f' || d === '\b') { if (line.length) { line = line.slice(0, -1); term.write('\b \b'); } return; }
    if (d === '\x03') { term.write('^C\r\n'); line = ''; hi = hist.length; send('\n'); return; }
    if (d === '\x0c') { term.clear(); return; }
    if (d === '\x04') { if (!line) { endSession(); } return; }
    if (d === '\x1b[A') { if (hi > 0) { hi--; replaceLine(hist[hi]); } return; }
    if (d === '\x1b[B') { if (hi < hist.length - 1) { hi++; replaceLine(hist[hi]); } else { hi = hist.length; replaceLine(''); } return; }
    if (d.charAt(0) === '\x1b') return;                       // other keys: not supported
    var parts = d.replace(/\r\n/g, '\n').replace(/\r/g, '\n').split('\n');
    for (var i = 0; i < parts.length; i++) {
      var p = parts[i].replace(/[\x00-\x1f]/g, '');
      line += p; term.write(p);
      if (i < parts.length - 1) submit();
    }
  });
  function endSession() { stop('[session ended]'); }
  endBtn.addEventListener('click', endSession);
  openBtn.addEventListener('click', function () { term.clear(); start(); });
  start();
  }
})();
