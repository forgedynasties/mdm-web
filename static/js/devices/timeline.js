// Loaded by templates/devices.html. Fleet's timeline (T1, picked 5 Nov 2026; user: "scroll
// wheel in pane scrolls one device at a time, and instead of pagination there is a continous
// timeline"). On a desktop window (html.shell) the list has no pages: the server sends a few
// screens of rows from a device on (?start=&fit=), the wheel moves exactly one device per
// notch, and the next rows load before the edge is reached. A strip down the card's right
// edge stands for every device in the view (/devices/timeline.json): green online, grey
// offline, labelled by last seen; the coral window is what is on screen. Drag or click it to
// jump; hover names the device there. Phones and the Cards view keep the pager.
(function () {
  var C = document.getElementById('device-table-container');
  if (!C) return;
  var root = document.documentElement;
  var tl = window.flTL = { on: false, start: 0, fit: 30 };
  var top = 0, chunk = { start: 0, n: 0 }, total = 0, vis = 1, loading = false, again = null;
  var line = null, lineAt = 0, rail = null, tip = null;

  function wrap() { return C.querySelector('.ft-wrap'); }
  function rows() { var w = wrap(); return w ? w.querySelectorAll('tbody tr.ft-row, tbody tr[data-serial]') : []; }
  function usable() {
    var pane = document.getElementById('fl-map-pane');
    return root.classList.contains('shell') && !!C.querySelector('table.ft') && !(pane && !pane.hidden);
  }
  function rowH() { var r = rows()[0]; return r ? r.getBoundingClientRect().height : 44; }
  function headH() { var w = wrap(), th = w && w.querySelector('thead'); return th ? th.getBoundingClientRect().height : 34; }
  function perScreen() { var w = wrap(); if (!w) return 10; return Math.max(3, Math.floor((w.clientHeight - headH()) / rowH())); }
  function readFoot() {
    var f = C.querySelector('.fl-foot');
    if (!f) return;
    chunk.start = Math.max(0, (+f.dataset.start || 1) - 1);
    chunk.n = +f.dataset.n || rows().length;
    total = +f.dataset.total || 0;
  }

  // Show the device at index top as the first row: scroll inside the loaded rows.
  function place(smooth) {
    var w = wrap(), rs = rows(), i = top - chunk.start;
    if (!w || !rs.length) return;
    i = Math.max(0, Math.min(rs.length - 1, i));
    var y = rs[i].offsetTop - headH();
    w.scrollTo({ top: Math.max(0, y), behavior: smooth ? 'smooth' : 'auto' });
    // How many rows are fully on screen, for the footer and the window.
    var bottom = y + w.clientHeight - headH(), n = 0;
    for (var k = i; k < rs.length; k++) { if (rs[k].offsetTop - headH() + rs[k].offsetHeight <= bottom + 1) n++; else break; }
    vis = Math.max(1, n);
    foot(); drawWin();
  }
  function foot() {
    var el = C.querySelector('.fl-foot-n');
    if (!el || !tl.on) return;
    el.innerHTML = total ? '<b>' + (top + 1) + '–' + Math.min(total, top + vis) + '</b> of ' + total + ' device' + (total === 1 ? '' : 's') : 'No devices';
  }
  function load(start) {
    tl.start = Math.max(0, start);
    if (loading) { again = tl.start; return; }
    loading = true;
    htmx.ajax('GET', fleetURL(getFilterVals()), { target: '#device-table-container', swap: 'innerHTML' })
      .then(function () { loading = false; if (again != null) { var a = again; again = null; if (a !== chunk.start) load(a); } });
  }
  function go(t, smooth) {
    if (!tl.on) return;
    var per = perScreen();
    top = Math.max(0, Math.min(Math.max(0, total - per), t));
    var inChunk = top >= chunk.start && top + per <= chunk.start + chunk.n;
    if (inChunk) place(smooth !== false);
    else { drawWin(); foot(); }
    // Keep a screen of rows either side loaded: fetch around top before the edge.
    var nearTop = chunk.start > 0 && top < chunk.start + Math.floor(per / 2);
    var nearEnd = chunk.start + chunk.n < total && top + per > chunk.start + chunk.n - Math.floor(per / 2);
    if (!inChunk || nearTop || nearEnd) load(top - per);
  }

  // ── the strip ──
  function ago(sec) {
    var m = Math.max(0, Math.round(sec / 60));
    if (m < 3) return 'now'; if (m < 60) return m + ' min'; if (m < 2880) return Math.round(m / 60) + ' h';
    return Math.round(m / 1440) + ' d';
  }
  function ensureRail() {
    if (rail && C.contains(rail)) return;
    rail = document.createElement('div'); rail.className = 'fl-tl-rail';
    rail.innerHTML = '<div class="trk"></div><div class="win"></div>';
    tip = document.createElement('div'); tip.className = 'fl-tl-tip';
    C.appendChild(rail); C.appendChild(tip);
    var drag = false;
    function at(e) { var r = rail.getBoundingClientRect(); return Math.floor((e.clientY - r.top) / r.height * total); }
    rail.addEventListener('pointerdown', function (e) { drag = true; rail.setPointerCapture(e.pointerId); go(at(e) - Math.floor(vis / 2), false); });
    rail.addEventListener('pointermove', function (e) {
      if (drag) go(at(e) - Math.floor(vis / 2), false);
      var i = Math.max(0, Math.min(total - 1, at(e))), d = line && line.devices[i];
      if (!d) return;
      var cr = C.getBoundingClientRect();
      tip.textContent = d.s + (d.v ? ' · ' + d.v : '') + (d.t ? ' · ' + ago(line.now - d.t) : '');
      tip.style.top = (e.clientY - cr.top - 12) + 'px'; tip.style.opacity = 1;
    });
    rail.addEventListener('pointerup', function () { drag = false; });
    rail.addEventListener('pointerleave', function () { tip.style.opacity = 0; });
    paint();
  }
  function fitRail() {
    if (!rail) return;
    var w = wrap(), f = C.querySelector('.fl-foot');
    var cr = C.getBoundingClientRect(), wr = w ? w.getBoundingClientRect() : cr;
    rail.style.top = (wr.top - cr.top + headH() + 6) + 'px';
    rail.style.bottom = (f ? cr.bottom - f.getBoundingClientRect().top + 6 : 8) + 'px';
  }
  function paint() {
    if (!rail) return;
    fitRail();
    var trk = rail.querySelector('.trk'), html = '';
    rail.querySelectorAll('.lbl').forEach(function (l) { l.remove(); });
    var ds = line ? line.devices : [], n = ds.length || total || 1, seg = 0;
    for (var i = 1; i <= ds.length; i++) {
      if (i === ds.length || !!ds[i].on !== !!ds[seg].on) {
        html += '<i class="' + (ds[seg].on ? 'on' : 'off') + '" style="top:' + (seg / n * 100) + '%;height:' + ((i - seg) / n * 100) + '%"></i>';
        seg = i;
      }
    }
    trk.innerHTML = html;
    // Last seen reads as time down the strip: now, 1 h, 1 d, 1 w, the oldest.
    if (line && line.sort === 'last_seen' && ds.length > 1) {
      var marks = [[0, 'now']], want = [[3600, '1 h'], [86400, '1 d'], [604800, '1 w']], desc = line.dir !== 'asc';
      if (desc) {
        want.forEach(function (w) { for (var k = 0; k < ds.length; k++) if (ds[k].t && line.now - ds[k].t >= w[0]) { marks.push([k, w[1]]); break; } });
        var last = ds[ds.length - 1]; if (last.t) marks.push([ds.length - 1, ago(line.now - last.t)]);
        var prev = -1e9;
        marks.forEach(function (m) {
          var y = m[0] / n * 100; if (y - prev < 7) return; prev = y;
          var l = document.createElement('span'); l.className = 'lbl'; l.style.top = y + '%'; l.textContent = m[1]; rail.appendChild(l);
        });
      }
    }
    drawWin();
  }
  function drawWin() {
    if (!rail || !total) return;
    var win = rail.querySelector('.win');
    win.style.top = (top / total * 100) + '%';
    win.style.height = (Math.max(vis, 1) / total * 100) + '%';
  }
  function fetchLine(force) {
    if (!tl.on) return;
    if (!force && Date.now() - lineAt < 30000) return;
    lineAt = Date.now();
    var v = getFilterVals(); delete v.page; delete v.start; delete v.fit;
    var q = new URLSearchParams(); Object.keys(v).forEach(function (k) { if (v[k]) q.set(k, v[k]); });
    fetch('/devices/timeline.json?' + q.toString(), { credentials: 'same-origin', headers: { 'Accept': 'application/json' } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) { if (d) { line = d; paint(); } })
      .catch(function () {});
  }

  // ── wheel and keys: one device per notch ──
  var acc = 0;
  C.addEventListener('wheel', function (e) {
    if (!tl.on || !e.target.closest('.ft-wrap')) return;
    e.preventDefault();
    acc += e.deltaMode === 1 ? e.deltaY * 30 : e.deltaY;
    var steps = Math.abs(e.deltaY) >= 50 && e.deltaMode === 0 ? Math.sign(e.deltaY) : (Math.abs(acc) >= 30 ? Math.trunc(acc / 30) : 0);
    if (steps) { acc = 0; go(top + steps); }
  }, { passive: false });
  document.addEventListener('keydown', function (e) {
    if (!tl.on || e.altKey || e.ctrlKey || e.metaKey) return;
    var a = document.activeElement;
    if (a && a !== document.body && !C.contains(a)) return;
    if (document.querySelector('.modal-backdrop.modal-open, dialog[open]')) return;
    var per = perScreen(), k = { ArrowDown: 1, ArrowUp: -1, PageDown: per, PageUp: -per, Home: -1e6, End: 1e6 }[e.key];
    if (k == null) return;
    e.preventDefault(); go(top + k);
  });

  // ── switching on and off, and every new set of rows ──
  function sync() {
    var want = usable();
    root.classList.toggle('fl-tl', want);
    if (want && !tl.on) {
      tl.on = true; tl.fit = Math.min(150, Math.max(30, perScreen() * 3)); top = 0;
      readFoot(); ensureRail(); fetchLine(true); load(0);
    } else if (!want && tl.on) {
      tl.on = false;
      if (rail) { rail.remove(); rail = null; } if (tip) { tip.remove(); tip = null; }
    }
  }
  document.addEventListener('htmx:afterSwap', function (e) {
    if (!e.detail || e.detail.target !== C) return;
    readFoot();
    if (!tl.on) { sync(); return; }
    if (tl.start === 0 && chunk.start === 0 && top >= chunk.n) top = 0;
    if (top < chunk.start || top >= chunk.start + Math.max(1, chunk.n)) top = chunk.start;
    ensureRail(); fitRail(); place(false); fetchLine(false);
  });
  document.body.addEventListener('fleet:scoped', function () { if (!tl.on) return; top = 0; place(false); fetchLine(true); });
  window.addEventListener('resize', function () {
    clearTimeout(window.__flTlR);
    window.__flTlR = setTimeout(function () {
      sync();
      if (tl.on) { var f = Math.min(150, Math.max(30, perScreen() * 3)); fitRail(); if (f > tl.fit) { tl.fit = f; load(top - perScreen()); } else place(false); }
    }, 150);
  });
  // The shell class arrives from chrome.js after layout; start once it is there.
  if (document.readyState === 'complete') setTimeout(sync, 60); else window.addEventListener('load', function () { setTimeout(sync, 60); });
  document.addEventListener('htmx:afterSettle', function () { if (!tl.on) sync(); });
})();
