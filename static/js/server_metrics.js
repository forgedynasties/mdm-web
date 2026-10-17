// Loaded by templates/server_metrics.html.
(function () {
  var paused = false;
  var last = null;

  window.svTogglePause = function () {
    paused = !paused;
    document.getElementById('sv-pause').textContent = paused ? 'Resume' : 'Pause';
    if (!paused && last) render(last);
  };

  function fmt(n, digits) {
    if (n === null || n === undefined || isNaN(n)) return '—';
    return Number(n).toFixed(digits === undefined ? 0 : digits);
  }
  var MIN_P95_CALLS = 20;
  function total(ms) {
    if (ms >= 60000) return fmt(ms / 60000, 1) + ' min';
    if (ms >= 1000) return fmt(ms / 1000, 1) + ' s';
    return fmt(ms) + ' ms';
  }
  // A stream's life: seconds matter here (a reconnect loop), so unlike dur() it
  // shows them below a minute. — until a stream has closed at least once.
  function life(sec) {
    if (!sec) return '—';
    if (sec < 60) return Math.round(sec) + 's';
    return dur(sec);
  }
  function dur(sec) {
    if (!sec) return '—';
    var d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
    if (d) return d + 'd ' + String(h).padStart(2, '0') + 'h';
    if (h) return h + 'h ' + String(m).padStart(2, '0') + 'm';
    return m + 'm';
  }
  // ── The hour ───────────────────────────────────────────────────────────────
  // One chart, the tiles above as its legend. Every series is drawn against its
  // OWN min/max: requests/s, heap MB and goroutine counts share no scale, and a
  // single y-axis across them would be a number that means nothing. So there is
  // no y-axis at all — the chart is for shape, and the exact values come back on
  // hover, where each active line reports what it actually was at that moment.
  var SERIES = [
    { key: 'requests',   label: 'Requests',   unit: '/s',   dp: 1, color: 'var(--accent)',        pick: function (x) { return x.requests; } },
    { key: 'checkins',   label: 'Check-ins',  unit: '/min', dp: 0, color: 'var(--accent-2)',      pick: function (x) { return x.checkins; } },
    { key: 'ws',         label: 'WebSockets', unit: '',     dp: 0, color: 'var(--chart-ram)',     pick: function (x) { return x.ws; } },
    { key: 'db_used',    label: 'DB pool',    unit: '',     dp: 0, color: 'var(--chart-battery)', pick: function (x) { return x.db_used; } },
    { key: 'heap_mb',    label: 'Heap',       unit: ' MB',  dp: 0, color: 'var(--chart-temp)',    pick: function (x) { return x.heap_mb; } },
    { key: 'goroutines', label: 'Goroutines', unit: '',     dp: 0, color: 'var(--info)',          pick: function (x) { return x.goroutines; } }
  ];
  var SERIES_BY_KEY = {};
  SERIES.forEach(function (x) { SERIES_BY_KEY[x.key] = x; });

  // Which lines are on, and how far back — remembered per browser, because this is
  // a page people leave open and come back to.
  var CH_KEY = 'mdm-server-chart';
  var chartOn = { requests: true, checkins: true, db_used: true }, chartRange = 60;
  try {
    var saved = JSON.parse(localStorage.getItem(CH_KEY) || 'null');
    if (saved && saved.on) { chartOn = saved.on; chartRange = saved.range || 60; }
  } catch (e) {}
  function saveChartPrefs() {
    try { localStorage.setItem(CH_KEY, JSON.stringify({ on: chartOn, range: chartRange })); } catch (e) {}
  }

  var chartSeries = [];   // the last frame's samples, for the hover readout
  var chartGeom = null;   // { x0, x1, n } so a pointer position maps back to a sample

  function drawChart(series) {
    chartSeries = series || [];
    var svg = document.getElementById('sv-chart');
    if (!svg) return;
    // The series is 10s samples; the range buttons are minutes.
    var take = Math.min(chartSeries.length, Math.round(chartRange * 6));
    var data = chartSeries.slice(-take);
    var on = SERIES.filter(function (x) { return chartOn[x.key]; });
    if (data.length < 2 || !on.length) {
      svg.innerHTML = '';
      document.getElementById('sv-chart-wrap').classList.toggle('empty', true);
      document.getElementById('sv-read').innerHTML = '<span class="rt">' +
        (on.length ? 'Collecting — the first points appear within a minute.' : 'No series selected.') + '</span>';
      chartGeom = null;
      return;
    }
    document.getElementById('sv-chart-wrap').classList.remove('empty');
    var W = 1000, H = 200, padY = 10, x0 = 0, x1 = W;
    var out = [];
    for (var g = 0; g <= 3; g++) {
      var gy = padY + g * ((H - padY * 2) / 3);
      out.push('<line class="gl" x1="0" y1="' + gy + '" x2="' + W + '" y2="' + gy + '"/>');
    }
    on.forEach(function (ser) {
      var vals = data.map(ser.pick), lo = Infinity, hi = -Infinity;
      vals.forEach(function (v) { if (v < lo) lo = v; if (v > hi) hi = v; });
      // A flat line sits in the middle of its band rather than on the floor, so
      // "nothing is happening" does not look like "this dropped to zero".
      if (hi - lo < 1e-9) { lo -= 1; hi += 1; }
      var span = hi - lo;
      var d = vals.map(function (v, i) {
        var x = x0 + (i / (vals.length - 1)) * (x1 - x0);
        var y = H - padY - ((v - lo) / span) * (H - padY * 2);
        return (i ? 'L' : 'M') + x.toFixed(1) + ' ' + y.toFixed(1);
      }).join(' ');
      out.push('<path class="ln" stroke="' + ser.color + '" d="' + d + '"/>');
      ser._lo = lo; ser._hi = hi;   // kept for the hover dots
    });
    svg.innerHTML = out.join('');
    chartGeom = { x0: x0, x1: x1, W: W, H: H, padY: padY, data: data, on: on };
    if (hoverIdx == null) readoutLatest();
  }

  // ── Hover: the y-axis this chart deliberately does not have ────────────────
  var hoverIdx = null;
  function readoutAt(i) {
    if (!chartGeom) return;
    var d = chartGeom.data[i]; if (!d) return;
    var when = new Date(d.at).toLocaleTimeString();
    document.getElementById('sv-read').innerHTML =
      '<span class="rt">' + when + '</span>' +
      chartGeom.on.map(function (ser) {
        return '<span class="rv"><i style="background:' + ser.color + '"></i>' + ser.label +
          ' <b>' + fmt(ser.pick(d), ser.dp) + ser.unit + '</b></span>';
      }).join('');
  }
  function readoutLatest() {
    if (!chartGeom || !chartGeom.data.length) return;
    readoutAt(chartGeom.data.length - 1);
  }
  (function () {
    var wrap = document.getElementById('sv-chart-wrap'), cross = document.getElementById('sv-cross');
    if (!wrap) return;
    wrap.addEventListener('mousemove', function (e) {
      if (!chartGeom) return;
      var r = wrap.getBoundingClientRect();
      var f = Math.min(1, Math.max(0, (e.clientX - r.left) / r.width));
      hoverIdx = Math.round(f * (chartGeom.data.length - 1));
      cross.hidden = false;
      cross.style.left = (f * r.width).toFixed(1) + 'px';
      readoutAt(hoverIdx);
    });
    wrap.addEventListener('mouseleave', function () {
      hoverIdx = null; cross.hidden = true; readoutLatest();
    });
  })();

  // Tiles are the legend. Delegated, because the tile row is rebuilt from
  // scratch on every frame.
  document.getElementById('sv-tiles').addEventListener('click', function (e) {
    var t = e.target.closest('[data-series]'); if (!t) return;
    var k = t.getAttribute('data-series');
    chartOn[k] = !chartOn[k]; saveChartPrefs();
    if (last) render(last);
  });
  document.getElementById('sv-tiles').addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    var t = e.target.closest('[data-series]'); if (!t) return;
    e.preventDefault(); t.click();
  });
  document.querySelectorAll('.sv-rng button').forEach(function (b) {
    b.onclick = function () {
      document.querySelectorAll('.sv-rng button').forEach(function (x) { x.classList.remove('on'); });
      b.classList.add('on');
      chartRange = +b.getAttribute('data-r'); saveChartPrefs();
      if (last) drawChart(last.snapshot.series || []);
    };
  });
  // Restore the saved range onto the buttons.
  document.querySelectorAll('.sv-rng button').forEach(function (b) {
    b.classList.toggle('on', +b.getAttribute('data-r') === chartRange);
  });
  // A tile is also this series' legend entry: the swatch shows its colour, and
  // clicking anywhere on it adds or removes the line. The 36-point sparkline that
  // used to sit here is gone — the chart below draws all 360 samples, so the
  // sparkline was the same data at a tenth of the resolution and twice the height.
  function tile(k, v, unit, sub, cls, key) {
    var ser = key ? SERIES_BY_KEY[key] : null, on = ser && chartOn[key];
    return '<div class="sv-tile ' + (cls || '') + (ser && !on ? ' off' : '') + '"' +
      (ser ? ' data-series="' + key + '" role="switch" tabindex="0" aria-checked="' + (on ? 'true' : 'false') +
             '" aria-label="' + k + ' — show on the chart"' : '') + '>' +
      (ser ? '<span class="sw" style="background:' + ser.color + '"></span>' : '') +
      '<div class="k">' + k + '</div>' +
      '<div class="v">' + v + (unit ? '<small>' + unit + '</small>' : '') + '</div>' +
      '<div class="s">' + (sub || '') + '</div></div>';
  }
  function bar(label, n, of, cls) {
    var pct = of > 0 ? Math.min(100, Math.round(n / of * 100)) : (n > 0 ? 100 : 0);
    return '<div class="sv-row"><span>' + label + '</span>' +
      '<span class="sv-bar"><i class="' + (cls || '') + '" style="width:' + pct + '%"></i></span>' +
      '<span class="sv-num">' + n + '</span></div>';
  }
  // One database stat: a label, a value and a line saying what it means. The "why it
  // matters" line is the point — a cache-hit percentage means nothing to someone who
  // has not memorised what a good one looks like.
  function dbstat(k, v, sub, cls) {
    return '<div class="sv-dbs ' + (cls || '') + '"><div class="k">' + k + '</div>' +
      '<div class="v">' + v + '</div><div class="s">' + sub + '</div></div>';
  }
  function esc(s) {
    return String(s).replace(/[&<>"]/g, function (c) {
      return ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c];
    });
  }

  function render(p) {
    last = p;
    if (paused) return;
    var s = p.snapshot, l = s.latest || {}, q = p.queues, series = s.series || [];

    var poolCls = l.db_total && l.db_used / l.db_total > 0.7 ? 'warn' : '';
    // Only routes with enough calls for a p95 to mean anything; below that it is just
    // the slowest single call. Mirrors metrics.MinCallsForP95.
    var p95 = (s.routes || []).reduce(function (m, r) {
      return r.calls >= MIN_P95_CALLS ? Math.max(m === null ? 0 : m, r.p95_ms) : m;
    }, null);

    document.getElementById('sv-tiles').innerHTML =
      tile('Requests', fmt(l.requests, 1), '/s', (p95 === null ? 'peak p95 —' : 'peak p95 ' + fmt(p95) + ' ms') + ' · ' + s.req_total + ' total',
        '', 'requests') +
      tile('Check-ins', fmt(l.checkins), '/min', s.checkin_total + ' since boot',
        '', 'checkins') +
      tile('WebSockets', fmt(l.ws), '', q.devices_online + ' of ' + q.devices_total + ' devices' +
        (q.devices_elsewhere ? ' · ' + q.devices_elsewhere + ' on another MDM' : ''),
        '', 'ws') +
      tile('DB pool', fmt(l.db_used), '/' + fmt(l.db_total), (l.db_waiting || 0) + ' waits since boot',
        poolCls, 'db_used') +
      tile('Heap', fmt(l.heap_mb), ' MB', 'process ' + fmt(l.sys_mb) + ' MB',
        '', 'heap_mb') +
      tile('Goroutines', fmt(l.goroutines), '', 'uptime ' + dur(s.uptime_sec),
        l.goroutines > 5000 ? 'warn' : '', 'goroutines');

    drawChart(series);

    document.getElementById('sv-queues').innerHTML =
      bar('Commands awaiting a device', q.commands_pending, Math.max(q.devices_total, 1)) +
      bar('Deployments active', q.deployments_live, 3, 'ok') +
      bar('OTA downloads in flight', q.ota_in_flight, Math.max(q.devices_total, 1), 'warn') +
      bar('Remote screen sessions', q.shell_sessions, 4) +
      bar('Peer outbox', q.peer_outbox, 20, q.peer_outbox ? 'warn' : 'ok') +
      bar('Open alerts', q.alerts_open, Math.max(q.devices_total, 1), 'warn');

    var ig = p.ingest, pw = document.getElementById('sv-pipe-wrap');
    pw.hidden = !ig;
    if (ig) {
      var shedAt = Math.floor(ig.capacity * 0.8);
      document.getElementById('sv-pipe').innerHTML =
        '<div class="sv-pipe">' +
          '<div class="st' + (ig.slots_in_use >= ig.slots ? ' warn' : '') + '"><div class="k">Running now</div>' +
            '<div class="v">' + ig.slots_in_use + '<small>/' + ig.slots + '</small></div>' +
            '<div class="s">check-ins in their synchronous half</div></div>' +
          '<div class="ar">→</div>' +
          '<div class="st' + (ig.depth >= shedAt ? ' warn' : '') + '"><div class="k">History queued</div>' +
            '<div class="v">' + ig.depth + '<small>/' + ig.capacity + '</small></div>' +
            '<div class="s">429 past ' + shedAt + '</div></div>' +
          '<div class="ar">→</div>' +
          '<div class="st"><div class="k">Written</div>' +
            '<div class="v">' + Number(ig.written).toLocaleString() + '</div>' +
            '<div class="s">' + ig.batches + ' batches · last ' + fmt(ig.last_flush_ms) + ' ms · worst ' + fmt(ig.max_flush_ms) + ' ms</div></div>' +
        '</div>' +
        '<div class="sv-pipe-foot">' +
          '<span class="' + (ig.shed ? 'warn' : '') + '">Turned away <b>' + ig.shed + '</b></span>' +
          '<span class="' + (ig.rejected ? 'warn' : '') + '">Written inline <b>' + ig.rejected + '</b></span>' +
          '<span class="' + (ig.history_failed ? 'warn' : '') + '">Lost <b>' + ig.history_failed + '</b></span>' +
          '<span>since the server started</span>' +
        '</div>';
    }

    var d = p.db || {};
    var hit = d.cache_hit_pct || 0;
    var conn = (d.conn_total || 0) + (d.conn_max ? ' / ' + d.conn_max : '');
    document.getElementById('sv-db').innerHTML =
      '<div class="sv-dbgrid">' +
        dbstat('Size', fmt(d.size_mb, 0) + ' MB', (d.tables || []).length + ' tables') +
        dbstat('Cache hit', fmt(hit, 1) + '%', hit < 95 ? 'reads are going to disk' : 'served from memory',
               hit < 95 ? 'warn' : '') +
        dbstat('Connections', conn, (d.conn_active || 0) + ' active · ' + (d.conn_idle || 0) + ' idle' +
               (d.conn_idle_tx ? ' · ' + d.conn_idle_tx + ' idle in txn' : ''),
               d.conn_idle_tx > 0 ? 'warn' : '') +
        dbstat('Longest query', fmt(d.longest_query_sec, 1) + 's', 'running right now',
               d.longest_query_sec > 30 ? 'warn' : '') +
        dbstat('Rollbacks', String(d.rollbacks || 0), (d.deadlocks || 0) + ' deadlocks · ' + (d.temp_files || 0) + ' temp files',
               d.deadlocks > 0 ? 'warn' : '') +
      '</div>' +
      '<table class="sv-tbl" style="margin-top:10px"><thead><tr><th>Table</th><th class="n">Size</th><th class="n">Rows</th><th class="n">Dead</th></tr></thead><tbody>' +
      ((d.tables || []).map(function (t) {
        return '<tr><td class="rt">' + esc(t.name) + '</td>' +
          '<td class="n">' + fmt(t.size_mb, 1) + ' MB</td>' +
          '<td class="n">' + (t.rows >= 1000000 ? fmt(t.rows / 1000000, 1) + 'M' : t.rows >= 1000 ? fmt(t.rows / 1000, 0) + 'k' : t.rows) + '</td>' +
          '<td class="n' + (t.dead_pct > 20 ? ' slow' : '') + '">' + fmt(t.dead_pct, 0) + '%</td></tr>';
      }).join('') || '<tr><td colspan="4" class="sv-empty">No table stats yet.</td></tr>') +
      '</tbody></table>';

    document.querySelector('#sv-routes tbody').innerHTML = (s.routes || []).length
      ? s.routes.map(function (r) {
          return '<tr' + (r.probe ? ' class="probe"' : '') + '><td class="rt" title="' + esc(r.route) + '">' + esc(r.route) + '</td>' +
            '<td class="n">' + r.calls + '</td>' +
            '<td class="n">' + fmt(r.p50_ms) + '</td>' +
            (r.calls >= MIN_P95_CALLS
              ? '<td class="n' + (r.p95_ms > 500 ? ' slow' : '') + '">' + fmt(r.p95_ms) + '</td>'
              : '<td class="n" title="Too few calls for a p95">—</td>') +
            '<td class="n">' + (r.queries_per_call ? fmt(r.db_ms_per_call) + '<span class="q">×' + fmt(r.queries_per_call, r.queries_per_call < 10 ? 1 : 0) + '</span>' : '—') + '</td>' +
            '<td class="n">' + total(r.total_ms) + '</td>' +
            '<td class="n">' + fmt(r.share_pct) + '%</td>' +
            '<td class="n' + (r.errors ? ' slow' : '') + '">' + (r.errors || 0) + '</td></tr>';
        }).join('')
      : '<tr><td colspan="8" class="sv-empty">No requests in the last 15 minutes.</td></tr>';

    // Warn on a slow start (p95 over 500 ms) or a reconnect loop: a route that has
    // opened 20+ streams whose median life is under 30 s.
    document.querySelector('#sv-streams tbody').innerHTML = (s.streams || []).length
      ? s.streams.map(function (st) {
          var loop = st.opened >= 20 && st.life_p50_sec > 0 && st.life_p50_sec < 30;
          return '<tr><td class="rt" title="' + esc(st.route) + '">' + esc(st.route) + '</td>' +
            '<td class="n">' + st.open + '</td>' +
            '<td class="n">' + st.opened + '</td>' +
            '<td class="n">' + fmt(st.ttfb_p50_ms) + '</td>' +
            '<td class="n' + (st.ttfb_p95_ms > 500 ? ' slow' : '') + '">' + (st.opened >= 2 ? fmt(st.ttfb_p95_ms) : '—') + '</td>' +
            '<td class="n' + (loop ? ' slow" title="Many short-lived streams: clients reconnecting' : '') + '">' + life(st.life_p50_sec) + '</td></tr>';
        }).join('')
      : '<tr><td colspan="6" class="sv-empty">No streams opened since the server started.</td></tr>';

    document.getElementById('sv-feed').innerHTML = (s.events || []).length
      ? s.events.map(function (e) {
          var t = new Date(e.at).toLocaleTimeString();
          return '<div><span class="t">' + t + '</span> <span class="k">' + esc(e.kind) + '</span> ' +
            '<span class="' + esc(e.class || '') + '">' + esc(e.text) + '</span></div>';
        }).join('')
      : '<div class="sv-empty">Nothing yet — the feed fills as devices check in and commands are issued.</div>';

    var live = document.getElementById('sv-live');
    live.classList.remove('stale');
    document.getElementById('sv-live-t').textContent = 'live · ' + new Date(p.now).toLocaleTimeString();
  }

  try { render(JSON.parse(document.getElementById('sv-initial').textContent)); } catch (_) {}

  var es = new EventSource('/events/server');
  es.addEventListener('metrics', function (e) {
    try { render(JSON.parse(e.data)); } catch (_) {}
  });
  es.onerror = function () {
    var live = document.getElementById('sv-live');
    live.classList.add('stale');
    document.getElementById('sv-live-t').textContent = 'reconnecting…';
  };
})();
