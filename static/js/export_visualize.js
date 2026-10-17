// Loaded by templates/export_visualize.html.
(function () {
  // Everything below runs entirely client-side: FileReader reads the chosen
  // file straight off disk into a JS string, and every parse/chart/table step
  // that follows never touches fetch/XHR. No network request carries the file.
  var MAX_ROWS = 20000;   // cap on parsed rows, so a huge file can't hang the tab
  var MAX_SERIES = 8;     // validated categorical palette only covers 8 series (see .evz-root)
  var METRICS = [
    { key: 'battery_pct', label: 'Battery', unit: '%' },
    { key: 'battery_temp_c', label: 'Battery temp', unit: '°C' },
    // The export writes percentages now; the MB/GB keys stay so an older CSV
    // someone kept still charts.
    { key: 'ram_used_pct', label: 'RAM used', unit: '%' },
    { key: 'storage_free_pct', label: 'Storage free', unit: '%' },
    { key: 'ram_used_mb', label: 'RAM used', unit: 'MB' },
    { key: 'storage_free_gb', label: 'Storage free', unit: 'GB' }
  ];

  var fileInput = document.getElementById('evz-file');
  var filenameEl = document.getElementById('evz-filename');
  var errorEl = document.getElementById('evz-error');
  var resultsEl = document.getElementById('evz-results');

  var drop = document.getElementById('evz-drop');
  drop.addEventListener('click', function () { fileInput.click(); });
  drop.addEventListener('keydown', function (e) { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); fileInput.click(); } });
  drop.addEventListener('dragover', function (e) { e.preventDefault(); drop.classList.add('over'); });
  drop.addEventListener('dragleave', function () { drop.classList.remove('over'); });
  drop.addEventListener('drop', function (e) {
    e.preventDefault(); drop.classList.remove('over');
    var f = e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files[0];
    if (f) loadFile(f);
  });
  fileInput.addEventListener('change', function () {
    var file = fileInput.files && fileInput.files[0];
    if (!file) return;
    loadFile(file);
    fileInput.value = '';
  });
  function loadFile(file) {
    filenameEl.textContent = file.name;
    errorEl.hidden = true;
    resultsEl.hidden = true;
    var reader = new FileReader();
    reader.onerror = function () { showError('Could not read that file.'); };
    reader.onload = function () {
      try {
        render(String(reader.result || ''));
      } catch (e) {
        console.error('[mdm] export-visualize parse failed:', e);
        showError('Could not parse this file as CSV.');
      }
    };
    reader.readAsText(file);
  }

  // ?serials=A,B,C (Compare from the fleet selection panel): pull the last 7 days
  // straight from /export/csv and render it, no file needed.
  (function () {
    var raw = new URLSearchParams(location.search).get('serials'); if (!raw) return;
    var serials = raw.split(',').map(function (x) { return x.trim(); }).filter(Boolean); if (!serials.length) return;
    var body = new URLSearchParams();
    serials.forEach(function (s) { body.append('serials', s); });
    var end = new Date(), start = new Date(end.getTime() - 7 * 86400000);
    function loc(d) { var p = function (n) { return (n < 10 ? '0' : '') + n; }; return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' + p(d.getHours()) + ':' + p(d.getMinutes()); }
    // Columns are fixed server-side now (exportColumns), so nothing is posted for them.
    body.set('start', loc(start)); body.set('end', loc(end)); body.set('tz_offset', String(-new Date().getTimezoneOffset()));
    filenameEl.textContent = 'Loading ' + serials.length + ' device' + (serials.length === 1 ? '' : 's') + ', last 7 days…';
    fetch('/export/csv', { method: 'POST', body: body }).then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error(t || ('HTTP ' + r.status)); });
      return r.text();
    }).then(function (text) {
      filenameEl.textContent = serials.length + ' selected device' + (serials.length === 1 ? '' : 's') + ' · last 7 days';
      render(text);
    }).catch(function (e) { showError('Could not load the export: ' + (e && e.message ? e.message : e)); });
  })();

  function showError(msg) {
    errorEl.textContent = msg;
    errorEl.hidden = false;
    resultsEl.hidden = true;
  }

  // Minimal RFC4180-ish CSV parser: handles quoted fields (with embedded
  // commas/newlines/escaped "" quotes) and bare fields. Matches what Go's
  // encoding/csv (the /export writer) produces.
  function parseCSV(text) {
    var rows = [], row = [], field = '', i = 0, inQuotes = false;
    var n = text.length;
    while (i < n) {
      var c = text[i];
      if (inQuotes) {
        if (c === '"') {
          if (text[i + 1] === '"') { field += '"'; i += 2; continue; }
          inQuotes = false; i++; continue;
        }
        field += c; i++; continue;
      }
      if (c === '"') { inQuotes = true; i++; continue; }
      if (c === ',') { row.push(field); field = ''; i++; continue; }
      if (c === '\r') { i++; continue; }
      if (c === '\n') { row.push(field); rows.push(row); row = []; field = ''; i++; continue; }
      field += c; i++;
    }
    if (field !== '' || row.length) { row.push(field); rows.push(row); }
    // Drop a trailing blank line (common at EOF).
    if (rows.length && rows[rows.length - 1].length === 1 && rows[rows.length - 1][0] === '') rows.pop();
    return rows;
  }

  // Parsed file, kept so the range/duplicate controls can re-render without
  // re-reading the file.
  var state = null; // { header, colIndex, rows: [{rec, serial, ts}], minTime, maxTime, totalRows, truncated }

  function render(text) {
    var rows = parseCSV(text);
    if (!rows.length) { showError('That file has no rows.'); return; }
    var header = rows[0];
    if (header.length < 2 || header[0].trim().toLowerCase() !== 'serial_number' || header[1].trim().toLowerCase() !== 'timestamp') {
      showError('Expected the first two columns to be "serial_number" and "timestamp" — this doesn’t look like an /export CSV.');
      return;
    }
    var colIndex = {};
    header.forEach(function (c, i) { colIndex[c.trim().toLowerCase()] = i; });
    var dataRows = rows.slice(1);
    var totalRows = dataRows.length;
    var truncated = totalRows > MAX_ROWS;
    var parsed = [], minTime = null, maxTime = null;
    dataRows.slice(0, MAX_ROWS).forEach(function (rec) {
      if (rec.length < 2) return;
      var serial = (rec[0] || '').trim();
      if (!serial) return;
      var ts = Date.parse(rec[1]);
      if (isNaN(ts)) return;
      if (minTime === null || ts < minTime) minTime = ts;
      if (maxTime === null || ts > maxTime) maxTime = ts;
      parsed.push({ rec: rec, serial: serial, ts: ts });
    });
    // Exports are newest-first; work oldest-first so "consecutive" means in time.
    parsed.sort(function (a, b) { return a.ts - b.ts || (a.serial < b.serial ? -1 : a.serial > b.serial ? 1 : 0); });
    state = { header: header, colIndex: colIndex, rows: parsed, minTime: minTime, maxTime: maxTime, totalRows: totalRows, truncated: truncated };
    setupRange();
    apply();
    resultsEl.hidden = false;
  }

  // ---- range controls: bounded to what the file actually covers
  function toLocalInput(ms) {
    var d = new Date(ms), p = function (n) { return String(n).padStart(2, '0'); };
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' + p(d.getHours()) + ':' + p(d.getMinutes());
  }
  function clampMs(ms) { if (!state) return ms; return Math.min(Math.max(ms, state.minTime), state.maxTime); }
  function setupRange() {
    var from = document.getElementById('evz-from'), to = document.getElementById('evz-to');
    if (state.minTime === null) return;
    from.min = to.min = toLocalInput(state.minTime);
    from.max = to.max = toLocalInput(state.maxTime + 60000);
    from.value = toLocalInput(state.minTime);
    to.value = toLocalInput(state.maxTime + 60000);
    document.getElementById('evz-range-hint').textContent = 'file covers ' + fmtDateTime(state.minTime) + ' → ' + fmtDateTime(state.maxTime);
  }
  function currentRange() {
    var from = document.getElementById('evz-from'), to = document.getElementById('evz-to');
    var a = Date.parse(from.value), b = Date.parse(to.value);
    if (isNaN(a)) a = state.minTime; if (isNaN(b)) b = state.maxTime + 60000;
    a = clampMs(a); b = Math.min(Math.max(b, state.minTime), state.maxTime + 60000);
    if (b < a) { var t = a; a = b; b = t; }
    from.value = toLocalInput(a); to.value = toLocalInput(b);
    return [a, b];
  }
  document.querySelectorAll('.evz-presets button').forEach(function (btn) {
    btn.addEventListener('click', function () {
      if (!state || state.minTime === null) return;
      var k = btn.getAttribute('data-preset'), from = document.getElementById('evz-from'), to = document.getElementById('evz-to');
      var lo = state.minTime, hi = state.maxTime + 60000;
      if (k === 'first24') hi = Math.min(hi, lo + 86400000);
      else if (k === 'last24') lo = Math.max(lo, hi - 86400000);
      else if (k === 'last7') lo = Math.max(lo, hi - 7 * 86400000);
      from.value = toLocalInput(lo); to.value = toLocalInput(hi);
      apply();
    });
  });
  document.getElementById('evz-apply').addEventListener('click', apply);
  document.getElementById('evz-dedupe').addEventListener('change', apply);
  ['evz-from', 'evz-to'].forEach(function (id) { document.getElementById(id).addEventListener('keydown', function (e) { if (e.key === 'Enter') apply(); }); });

  // ---- apply range + duplicate folding, then draw
  function apply() {
    if (!state) return;
    var r = currentRange(), lo = r[0], hi = r[1];
    var inRange = state.rows.filter(function (x) { return x.ts >= lo && x.ts <= hi; });
    var dedupe = document.getElementById('evz-dedupe').checked;
    var kept = inRange, dropped = 0;
    if (dedupe) {
      // per device, drop a row whose every column except the timestamp equals the
      // previous kept row for that device
      kept = []; var lastSig = {};
      inRange.forEach(function (x) {
        var sig = x.rec.map(function (c, i) { return i === 1 ? '' : c; }).join('\u0001');
        if (lastSig[x.serial] === sig) { dropped++; return; }
        lastSig[x.serial] = sig; kept.push(x);
      });
    }
    var deviceOrder = [], seenDevice = {};
    var seriesByMetric = {};
    METRICS.forEach(function (m) { seriesByMetric[m.key] = {}; });
    kept.forEach(function (x) {
      var rec = x.rec, serial = x.serial, ts = x.ts;
      if (!seenDevice[serial]) { seenDevice[serial] = true; deviceOrder.push(serial); }
      var idx = deviceOrder.indexOf(serial);
      if (idx >= MAX_SERIES) return; // charted devices capped; still counted/tabled
      METRICS.forEach(function (m) {
        var ci = state.colIndex[m.srcCol || m.key];
        if (ci === undefined || ci >= rec.length) return;
        var raw = (rec[ci] || '').trim();
        if (raw === '') return;
        var v = parseFloat(raw);
        if (isNaN(v)) return;
        if (m.xform) v = m.xform(v);
        var sr = seriesByMetric[m.key][serial];
        if (!sr) { sr = { serial: serial, points: [] }; seriesByMetric[m.key][serial] = sr; }
        sr.points.push({ t: ts, v: v });
      });
    });
    var charts = [];
    METRICS.forEach(function (m) {
      var byDevice = seriesByMetric[m.key];
      var series = deviceOrder.map(function (sname) { return byDevice[sname]; }).filter(Boolean);
      if (series.length) charts.push({ key: m.key, label: m.label, unit: m.unit, series: series });
    });
    // earliest/latest describe the selected range of the file, not just the kept rows
    var minT = inRange.length ? inRange[0].ts : null, maxT = inRange.length ? inRange[inRange.length - 1].ts : null;
    renderSummary(kept.length, deviceOrder.length, minT, maxT, state.truncated, deviceOrder.length > MAX_SERIES, Math.min(state.totalRows, MAX_ROWS));
    var dn = document.getElementById('evz-note-dedupe');
    if (dedupe && dropped) { dn.hidden = false; dn.className = 'evz-note info'; dn.textContent = dropped + ' row' + (dropped === 1 ? '' : 's') + ' hidden: identical to the previous row for the same device except for the timestamp.'; }
    else dn.hidden = true;
    // table newest-first, like the export
    lastKept = kept; deviceOrderAll = deviceOrder; renderTables();
    renderCharts(charts);
  }

  function fmtFull(ms) {
    var d = new Date(ms);
    return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' }) + ' ' + d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  }
  function fmtDateTime(ms) {
    var d = new Date(ms);
    return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) + ' ' + d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
  }

  function renderSummary(rowCount, deviceCount, minTime, maxTime, truncated, capped, shownCount) {
    document.getElementById('evz-stat-rows').textContent = rowCount;
    document.getElementById('evz-stat-devices').textContent = deviceCount;
    var firstWrap = document.getElementById('evz-stat-first-wrap'), lastWrap = document.getElementById('evz-stat-last-wrap');
    if (minTime !== null) { document.getElementById('evz-stat-first').textContent = fmtDateTime(minTime); firstWrap.hidden = false; } else firstWrap.hidden = true;
    if (maxTime !== null) { document.getElementById('evz-stat-last').textContent = fmtDateTime(maxTime); lastWrap.hidden = false; } else lastWrap.hidden = true;
    var tn = document.getElementById('evz-note-truncated');
    if (truncated) { tn.hidden = false; tn.textContent = 'The file is large: only its first ' + shownCount + ' rows were read.'; }
    else tn.hidden = true;
    document.getElementById('evz-note-capped').hidden = !capped;
  }

  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }

  // ---- table views: all rows / one table per device / compare columns across devices
  var lastKept = [], deviceOrderAll = [];
  var tbl = { view: 'all', cols: null, devs: null, bucket: 300000 };
  try { var savedView = localStorage.getItem('evz-view'); if (savedView) tbl.view = savedView; } catch (e) {}
  var tablesBox = document.getElementById('evz-tables');
  document.querySelectorAll('#evz-view input').forEach(function (i) {
    i.checked = i.value === tbl.view;
    i.addEventListener('change', function () { tbl.view = i.value; try { localStorage.setItem('evz-view', tbl.view); } catch (e) {} tbl.cols = null; renderTables(); });
  });
  document.getElementById('evz-bucket').addEventListener('change', function () { tbl.bucket = +this.value; renderTables(); });

  function dataCols() { // every column except serial + timestamp, in file order
    return state.header.map(function (h, i) { return { name: h, i: i }; }).filter(function (c) { return c.i > 1; });
  }
  function chipList(host, items, selected, onToggle, swatch) {
    host.innerHTML = '';
    items.forEach(function (it, idx) {
      var b = document.createElement('button'); b.type = 'button';
      b.className = 'evz-chip' + (selected.indexOf(it.key) !== -1 ? ' on' : '');
      b.innerHTML = (swatch ? '<span class="sw" style="background:' + swatch(idx) + '"></span>' : '') + esc(it.label);
      b.onclick = function () { onToggle(it.key); };
      host.appendChild(b);
    });
  }
  function palette(i) {
    var root = document.querySelector('.evz-root');
    return getComputedStyle(root).getPropertyValue('--evz-s' + ((i % 8) + 1)).trim() || '#888';
  }
  function fmtCell(c, ci) {
    if (ci === 1) { var ms = Date.parse(c); if (!isNaN(ms)) return '<td class="evz-ts" title="' + esc(c) + '">' + esc(fmtFull(ms)) + '</td>'; }
    return '<td>' + esc(c) + '</td>';
  }
  function tableHTML(cols, rows, withSerial) { // rows: [{rec, ts}], newest first
    var head = (withSerial ? '<th>' + esc(state.header[0]) + '</th>' : '') + '<th>' + esc(state.header[1]) + '</th>' + cols.map(function (c) { return '<th>' + esc(c.name) + '</th>'; }).join('');
    var body = [];
    for (var i = 0; i < rows.length; i++) {
      var rec = rows[i].rec;
      body.push('<tr>' + (withSerial ? fmtCell(rec[0], 0) : '') + fmtCell(rec[1], 1) + cols.map(function (c) { return fmtCell(rec[c.i] || '', c.i); }).join('') + '</tr>');
    }
    return '<div class="evz-table-wrap"><table class="evz-table"><thead><tr>' + head + '</tr></thead><tbody>' + body.join('') + '</tbody></table></div>';
  }

  function renderTables() {
    if (!state) return;
    var all = dataCols();
    var metricDefault = all.filter(function (c) { return /battery_pct/i.test(c.name); }).map(function (c) { return c.i; });
    if (!tbl.cols) tbl.cols = tbl.view === 'compare' ? (metricDefault.length ? metricDefault : all.slice(0, 1).map(function (c) { return c.i; })) : all.map(function (c) { return c.i; });
    if (!tbl.devs) tbl.devs = deviceOrderAll.slice(0, 8);
    tbl.devs = tbl.devs.filter(function (d) { return deviceOrderAll.indexOf(d) !== -1; });
    var cols = all.filter(function (c) { return tbl.cols.indexOf(c.i) !== -1; });
    chipList(document.getElementById('evz-cols'), all.map(function (c) { return { key: c.i, label: c.name }; }), tbl.cols, function (k) {
      var at = tbl.cols.indexOf(k); if (at !== -1) { if (tbl.cols.length > 1) tbl.cols.splice(at, 1); } else tbl.cols.push(k); renderTables();
    });
    document.getElementById('evz-devs-wrap').hidden = tbl.view !== 'compare';
    document.getElementById('evz-bucket-wrap').hidden = tbl.view === 'all';
    var note = document.getElementById('evz-tbl-note'); note.hidden = true;
    var hint = document.getElementById('evz-tbl-hint');
    var newest = lastKept.slice().reverse();
    if (tbl.view === 'all') {
      hint.textContent = newest.length + ' rows, newest first';
      tablesBox.innerHTML = tableHTML(cols, newest, true);
      return;
    }
    if (tbl.view === 'device') {
      // Same "Align to" as Compare: rows collapse onto a time bucket per device,
      // the latest report in each bucket wins (newest is first, so first seen wins).
      var byDev = {}, bd = tbl.bucket;
      newest.forEach(function (x) {
        var arr = byDev[x.serial] = byDev[x.serial] || [];
        if (!bd) { arr.push(x); return; }
        var k = Math.floor(x.ts / bd) * bd;
        if (arr.length && arr[arr.length - 1].ts === k) return;
        arr.push({ serial: x.serial, ts: k, rec: x.rec });
      });
      if (bd) { note.hidden = false; note.textContent = 'Rows are aligned to ' + (bd >= 3600000 ? bd / 3600000 + '-hour' : bd / 60000 + '-minute') + ' buckets; when a device reported more than once in a bucket the latest value is shown.'; }
      hint.textContent = deviceOrderAll.length + ' device' + (deviceOrderAll.length === 1 ? '' : 's') + ', one table each';
      tablesBox.innerHTML = deviceOrderAll.map(function (serial, idx) {
        var rows = byDev[serial] || [];
        var last = rows.length ? fmtFull(rows[0].ts) : '';
        return '<details class="evz-dev"' + (idx < 3 ? ' open' : '') + '><summary><span class="sw" style="background:' + palette(idx) + '"></span><span class="mono">' + esc(serial) + '</span><span class="n">' + rows.length + ' row' + (rows.length === 1 ? '' : 's') + (last ? ' · latest ' + esc(last) : '') + '</span></summary>' + tableHTML(cols, rows, false) + '</details>';
      }).join('');
      return;
    }
    // compare: one column per (device × chosen column), rows aligned on a time bucket
    chipList(document.getElementById('evz-devs'), deviceOrderAll.map(function (d) { return { key: d, label: d }; }), tbl.devs, function (k) {
      var at = tbl.devs.indexOf(k); if (at !== -1) { if (tbl.devs.length > 1) tbl.devs.splice(at, 1); } else tbl.devs.push(k); renderTables();
    }, function (i) { return palette(i); });
    var devs = deviceOrderAll.filter(function (d) { return tbl.devs.indexOf(d) !== -1; });
    var bucket = tbl.bucket;
    var key = function (ts) { return bucket ? Math.floor(ts / bucket) * bucket : ts; };
    var grid = {}, times = [];
    lastKept.forEach(function (x) {
      if (devs.indexOf(x.serial) === -1) return;
      var k = key(x.ts);
      if (!grid[k]) { grid[k] = {}; times.push(k); }
      grid[k][x.serial] = x.rec; // last row in the bucket wins
    });
    times.sort(function (a, b) { return b - a; });
    var head1 = '<th rowspan="2">' + esc(state.header[1]) + (bucket ? ' <span class="u" style="font-weight:500;text-transform:none;letter-spacing:0">(' + (bucket >= 3600000 ? bucket / 3600000 + ' h' : bucket / 60000 + ' min') + ' buckets)</span>' : '') + '</th>';
    var head2 = '';
    devs.forEach(function (d, di) {
      head1 += '<th class="g first" colspan="' + cols.length + '"><span class="sw" style="background:' + palette(deviceOrderAll.indexOf(d)) + '"></span>' + esc(d) + '</th>';
      cols.forEach(function (c, ci) { head2 += '<th class="sub' + (ci === 0 ? ' first' : '') + '">' + esc(c.name) + '</th>'; });
    });
    var body = [];
    times.forEach(function (t, ri) {
      var prev = times[ri + 1] ? grid[times[ri + 1]] : null;
      var cells = '';
      devs.forEach(function (d) {
        var rec = grid[t][d];
        cols.forEach(function (c, ci) {
          var cls = ci === 0 ? ' class="first"' : '';
          if (!rec) { cells += '<td class="empty' + (ci === 0 ? ' first' : '') + '">·</td>'; return; }
          var v = rec[c.i] || '';
          var delta = '';
          if (prev && prev[d] && v !== '' && !isNaN(parseFloat(v)) && !isNaN(parseFloat(prev[d][c.i]))) {
            var dv = parseFloat(v) - parseFloat(prev[d][c.i]);
            if (dv !== 0) delta = '<span class="delta ' + (dv > 0 ? 'up' : 'down') + '">' + (dv > 0 ? '▲' : '▼') + Math.abs(Math.round(dv * 100) / 100) + '</span>';
          }
          cells += '<td' + cls + '>' + esc(v) + delta + '</td>';
        });
      });
      body.push('<tr>' + '<td class="evz-ts">' + esc(fmtFull(t)) + '</td>' + cells + '</tr>');
    });
    hint.textContent = devs.length + ' device' + (devs.length === 1 ? '' : 's') + ' × ' + cols.length + ' column' + (cols.length === 1 ? '' : 's') + ', ' + times.length + ' time rows';
    if (bucket) { note.hidden = false; note.textContent = 'Rows are aligned to ' + (bucket >= 3600000 ? bucket / 3600000 + '-hour' : bucket / 60000 + '-minute') + ' buckets; when a device reported more than once in a bucket the latest value is shown. ▲▼ is the change from the previous row.'; }
    tablesBox.innerHTML = '<div class="evz-table-wrap"><table class="evz-table evz-cmp"><thead><tr>' + head1 + '</tr><tr>' + head2 + '</tr></thead><tbody>' + body.join('') + '</tbody></table></div>';
  }

  var chartsBox = document.getElementById('evz-charts');
  function renderCharts(charts) {
    chartsBox.innerHTML = '';
    if (!charts.length) return;
    window.mdmChartReady = window.mdmChartReady || (window.Chart
      ? Promise.resolve()
      : new Promise(function (resolve, reject) {
          var s = document.createElement('script');
          s.src = '/static/vendor/chart.umd.min.js';
          s.onload = resolve;
          s.onerror = function () { reject(new Error('Chart.js failed to load')); };
          document.head.appendChild(s);
        }));
    window.mdmChartReady.then(function () {
      var root = document.querySelector('.evz-root');
      var palette = [];
      for (var i = 1; i <= 8; i++) palette.push(getComputedStyle(root).getPropertyValue('--evz-s' + i).trim());
      var gridColor = getComputedStyle(document.body).getPropertyValue('--chart-grid').trim() || 'rgba(140,140,160,0.15)';
      var tickColor = getComputedStyle(document.body).getPropertyValue('--chart-tick').trim() || '#8a8a99';
      charts.forEach(function (chart) {
        var card = document.createElement('div');
        card.className = 'evz-chart-card';
        card.innerHTML = '<div class="evz-chart-h">' + esc(chart.label) + ' <span class="u">' + esc(chart.unit) + '</span></div><div class="evz-chart-wrap"><canvas></canvas></div>';
        chartsBox.appendChild(card);
        var ctx = card.querySelector('canvas').getContext('2d');
        var datasets = chart.series.map(function (s, i) {
          var color = palette[i % palette.length];
          return {
            label: s.serial,
            data: s.points.map(function (p) { return { x: p.t, y: p.v }; }),
            borderColor: color, backgroundColor: color,
            borderWidth: 2, pointRadius: 0, pointHoverRadius: 4, tension: 0.25
          };
        });
        // Plain linear x-axis over epoch-ms (not Chart.js's 'time' scale — this
        // app doesn't vendor a date adapter, see device.html's sharedXScale for
        // the same pattern), with a short local-time tick/tooltip formatter.
        new Chart(ctx, {
          type: 'line',
          data: { datasets: datasets },
          options: {
            responsive: true, maintainAspectRatio: false, animation: false,
            interaction: { mode: 'nearest', axis: 'x', intersect: false },
            plugins: {
              legend: { display: datasets.length > 1, position: 'bottom', labels: { boxWidth: 10, font: { size: 11 } } },
              tooltip: { mode: 'nearest', intersect: false, callbacks: { title: function (items) { return items.length ? fmtDateTime(items[0].parsed.x) : ''; } } }
            },
            scales: {
              x: { type: 'linear', grid: { color: gridColor }, ticks: { color: tickColor, maxRotation: 0, callback: fmtDateTime } },
              y: { grid: { color: gridColor }, ticks: { color: tickColor } }
            }
          }
        });
      });
    }).catch(function (err) { console.error('[mdm] export-visualize charts failed:', err); });
  }
})();
