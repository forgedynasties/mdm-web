// Loaded by templates/device.html.
// Vitals toolbar menus (range, ⋯): one open at a time; any item, outside click or Escape
// closes them.
window.vtMenu = function (e, id) {
  e.stopPropagation();
  var m = document.getElementById(id); if (!m) return;
  var open = m.hasAttribute('hidden');
  document.querySelectorAll('.vt-menu').forEach(function (x) { x.setAttribute('hidden', ''); });
  document.querySelectorAll('[data-vt-trig]').forEach(function (b) { b.setAttribute('aria-expanded', 'false'); });
  if (open) { m.removeAttribute('hidden'); e.currentTarget.setAttribute('aria-expanded', 'true'); }
};
if (!window.__vtMenuHook) {
  window.__vtMenuHook = true;
  var vtCloseAll = function () {
    document.querySelectorAll('.vt-menu').forEach(function (x) { x.setAttribute('hidden', ''); });
    document.querySelectorAll('[data-vt-trig]').forEach(function (b) { b.setAttribute('aria-expanded', 'false'); });
  };
  document.addEventListener('click', function (e) {
    if (e.target.closest('[data-vt-trig]')) return;
    if (e.target.closest('.vt-menu .vt-mi')) { setTimeout(vtCloseAll, 0); return; }
    if (!e.target.closest('.vt-menu')) vtCloseAll();
  }, true);
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') vtCloseAll(); });
}

function mdmBootCharts() {
  // Page values from the template: the config script before this file's tag.
  const DV = window.DEVICE_VITALS;
  // Theme-aware chart colors (grid/ticks) read from CSS tokens.
  const _cs = getComputedStyle(document.documentElement);
  const GRID = (_cs.getPropertyValue('--chart-grid') || '').trim() || 'rgba(46,51,71,0.8)';
  const TICK = (_cs.getPropertyValue('--chart-tick') || '').trim() || '#7a7f9a';
  // Nicer axis labels everywhere: use the app's UI font, a smaller weight-600 tick,
  // and a touch of padding so numbers don't kiss the plot. Applies to all charts.
  if (window.Chart && Chart.defaults) {
    Chart.defaults.font.family = (getComputedStyle(document.body).fontFamily || 'inherit');
    Chart.defaults.font.size = 11;
    Chart.defaults.font.weight = 600;
    Chart.defaults.color = TICK;
  }
  // Devices without a battery (kiosks) get no battery series at all — the tab,
  // panel, and unified legend/line are all omitted, so this stays empty.
  const HAS_BATTERY = DV.showBattery;

  // Timezone display mode. All times on this page are formatted client-side; by default
  // they render in the viewer's browser timezone. The "Your time / Device time" toggle
  // flips DEVICE_TZ in, so graphs, tooltips and the check-in modal all read in the
  // device's own timezone. tzArg() is global because the check-in modal lives in a
  // separate script scope.
  window.DEVICE_SERIAL = DV.serial;
  window.DEVICE_TZ = DV.tz;
  // Validate once — a device could report an id Intl doesn't accept; if so, drop it so
  // formatting never throws (the toggle is hidden below when DEVICE_TZ is unusable).
  if (window.DEVICE_TZ && window.DEVICE_TZ !== '—') {
    try { new Date().toLocaleString('en-US', {timeZone: window.DEVICE_TZ}); }
    catch (e) { window.DEVICE_TZ = ''; }
  } else {
    window.DEVICE_TZ = '';
  }
  if (window.TZ_MODE === undefined) window.TZ_MODE = 'browser';
  function tzArg(o) {
    o = o || {};
    if (window.TZ_MODE === 'utc') o.timeZone = 'UTC';
    else if (window.TZ_MODE === 'device' && window.DEVICE_TZ && window.DEVICE_TZ !== '—') o.timeZone = window.DEVICE_TZ;
    return o;
  }
  window.tzArg = tzArg;
  // Mutable so the range picker can grow the loaded window backward on demand
  // (chartLoadOlder) — older builds' history is fetched from /chart-data instead of
  // only filtering the ~48h baked in here.
  // Charging-state spans ({from, to, c}) for the strip under the battery chart. Built
  // server-side from every check-in before the battery series is thinned, so a charger
  // flip is placed where it happened. c is null where the device sent no "charging".
  let allCharge = DV.charge;
  // Lifetime battery wear by day, for the "Cycles to here" readout. See
  // DeviceDischargeSeries: it is the series behind the Battery cycles card, so the two
  // finally answer the same question.
  const CYCLE_SERIES_RAW = DV.cycleSeries;
  // Readings the device kept while offline and sent later (client 1.7.0): shaded grey on
  // every chart, their line dashed. Grows as chart-data windows arrive (mergeLate).
  let lateSpans = DV.lateSpans || [];
  function mergeLate(arr) {
    (arr || []).forEach(function (s) {
      if (!lateSpans.some(function (t) { return t.from === s.from && t.to === s.to; })) lateSpans.push(s);
    });
  }
  function inLate(x) {
    for (var i = 0; i < lateSpans.length; i++) if (x >= lateSpans[i].from && x <= lateSpans[i].to) return true;
    return false;
  }
  let allBattery = DV.battery;
  // Temp/RAM come with the page only on a ?focus=temp/ram deep link (see the config
  // script); otherwise mdmLoadMetric below fetches them when the tab is first opened.
  let allTemp = DV.temp;
  let allRam = DV.ram;
  // Earliest check-in currently loaded (arrays are oldest-first). Widening the range
  // before this triggers a fetch that prepends the older window.
  let loadedMinX = (function () {
    var xs = [];
    if (allBattery.length) xs.push(allBattery[0].x);
    if (allTemp.length) xs.push(allTemp[0].x);
    if (allRam.length) xs.push(allRam[0].x);
    // Nothing is loaded at all — a device with no battery, whose page ships no series
    // because temp/RAM are fetched lazily. Falling back to "now" made the lazy fetch
    // compute from >= until, mark itself loaded and never fetch, so the first tab a
    // kiosk opens stayed empty. The default view is the last 6 hours, so start there.
    return xs.length ? Math.min.apply(null, xs) : Date.now() - 6 * 3600 * 1000;
  })();
  // Nothing exists before the device joined the fleet, so the graph and the picker
  // both stop there: panning further back used to walk into empty time and look like
  // data loss. Falls back to the row's creation date for a device enrolled before that
  // column existed.
  var CHART_MIN_X = DV.minX;
  function clampToOnboard(a, b) {
    if (a >= CHART_MIN_X) return [a, b];
    var span = b - a;
    a = CHART_MIN_X;
    b = Math.min(a + span, Date.now());
    return [a, b];
  }

  let chartFetching = false;
  // Fetch check-ins older than what's loaded, for [fromMs, loadedMinX), and prepend
  // them so the recent high-resolution data is kept. Then run `done` (re-render).
  // Re-show the same "Loading graphs…" overlay used on first paint while a
  // chartLoadOlder fetch is in flight — otherwise switching to 12h/24h/48h before
  // the background warm-up has caught up briefly draws an empty chart.
  function showChartLoading() {
    var cl = document.getElementById('chart-loading');
    if (!cl) return;
    cl.style.display = '';
    cl.classList.remove('hide');
  }
  function hideChartLoading() {
    var cl = document.getElementById('chart-loading');
    if (!cl) return;
    cl.classList.add('hide');
    setTimeout(function () { cl.style.display = 'none'; }, 300);
  }
  // Temp/RAM ship empty (see allTemp/allRam above) unless deep-linked. Fetch them
  // the first time their tab is actually opened, covering the same [loadedMinX,
  // now] span the battery chart already has loaded — so it lines up with whatever
  // chartLoadOlder widens later instead of leaving an unfetched gap behind it.
  let tempLoaded = allTemp.length > 0, ramLoaded = allRam.length > 0;
  // Published on window.mdmChart so the SSE pusher can tell a loaded series from an
  // empty one: appending a live point to a series that was never fetched leaves a
  // one-point array, and "nearest point" against one point is the same value
  // everywhere — the rail looked frozen while the cursor moved.
  function markLoaded(metric) {
    if (metric === 'temp') tempLoaded = true; else ramLoaded = true;
    if (window.mdmChart) window.mdmChart[metric === 'temp' ? 'tempLoaded' : 'ramLoaded'] = true;
  }
  // The crosshair rail on the right reads Temp and RAM whatever tab is open, so
  // the warm-up below loads both silently (spinner suppressed, no redraw needed)
  // right after first paint — otherwise the rail had only the point or two the SSE
  // stream had pushed in since load and reported the same value at every cursor
  // position until the Temp tab was opened once.
  function loadMetricLazy(metric, silent, done) {
    function fin() { if (done) done(); }
    if (metric === 'temp' && tempLoaded) { fin(); return; }
    if (metric === 'ram' && ramLoaded) { fin(); return; }
    var until = Math.floor(Date.now());
    var from = Math.floor(loadedMinX);
    if (from >= until) { if (metric === 'temp') markLoaded('temp'); else markLoaded('ram'); fin(); return; }
    if (!silent) showChartLoading();
    fetch('/devices/' + DV.serial + '/chart-data?from=' + from + '&until=' + until, { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) { if (d && d.late) mergeLate(d.late);
        if (d) {
          // Reassigns (not mutates) allTemp/allRam — window.mdmChart holds the old
          // (empty) reference for the live-SSE pusher, so repoint it here too or
          // live updates would keep appending to an array nothing renders from.
          // One response carries both series, and the stacked charts show both, so
          // take whichever is still missing rather than fetching twice.
          if (d.temp && (metric === 'temp' || !tempLoaded)) {
            allTemp = d.temp; markLoaded('temp');
            if (window.mdmChart) window.mdmChart.allTemp = allTemp;
          }
          if (d.ram && (metric === 'ram' || !ramLoaded)) {
            allRam = d.ram; markLoaded('ram');
            if (window.mdmChart) window.mdmChart.allRam = allRam;
          }
        }
      })
      .catch(function () {})
      .then(function () {
        if (!silent) hideChartLoading();
        updateCharts();
        if (activeTab === 'all' && unifiedChart) applyUnifiedData();
        fin();
      });
  }
  function loadOlderWithSpinner(fromMs, done) {
    var needFetch = fromMs < loadedMinX && !chartFetching;
    if (needFetch) showChartLoading();
    chartLoadOlder(fromMs, function () {
      if (needFetch) hideChartLoading();
      if (done) done();
    });
  }
  // ── Window fetch ────────────────────────────────────────────────────────────
  // The server thins each series to ~2500 points FOR THE RANGE IT IS ASKED FOR. The
  // old path asked for [chosen start, oldest loaded] — up to months — and then drew a
  // single day out of the answer, so a day that has 829 points on its own arrived with
  // 86 of them and the chart looked almost empty. Anything that moves the window to a
  // place the live arrays were not loaded for now fetches exactly the window it is
  // about to draw.
  let viewData = null;       // series for the window on screen, or null to use the live arrays
  let viewKey = '';          // which window viewData belongs to
  let viewCtl = null;        // in-flight fetch, so a newer window supersedes it
  const viewCache = new Map();
  const VIEW_CACHE_MAX = 12;

  function srcBattery() { return viewData && viewData.battery ? viewData.battery : allBattery; }
  function srcTemp() { return viewData && viewData.temp ? viewData.temp : allTemp; }
  function srcRam() { return viewData && viewData.ram ? viewData.ram : allRam; }
  function srcCharge() { return viewData && viewData.charge ? viewData.charge : allCharge; }
  window.mdmClearViewData = function () { viewData = null; viewKey = ''; };

  function fetchWindow(from, to, done) {
    from = Math.floor(from); to = Math.floor(to);
    var key = from + '|' + to;
    if (key === viewKey && viewData) { if (done) done(); return; }
    if (viewCache.has(key)) {
      viewData = viewCache.get(key); viewKey = key;
      if (done) done();
      return;
    }
    // Supersede rather than drop: the old code refused to start a second fetch while
    // one was in flight, so clicking the arrows twice left the second window showing
    // whatever the first had loaded.
    if (viewCtl) viewCtl.abort();
    var ctl = new AbortController();
    viewCtl = ctl;
    // Only show the overlay if the fetch is actually slow — a cached or fast window
    // otherwise flashes "Loading graphs…" for one frame on every arrow click.
    var spin = setTimeout(showChartLoading, 150);
    fetch('/devices/' + DV.serial + '/chart-data?from=' + from + '&until=' + to, { credentials: 'same-origin', signal: ctl.signal })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) { if (d && d.late) mergeLate(d.late);
        if (ctl !== viewCtl) return;   // a newer window already replaced this one
        if (d) {
          viewData = d; viewKey = key;
          viewCache.set(key, d);
          if (viewCache.size > VIEW_CACHE_MAX) viewCache.delete(viewCache.keys().next().value);
        }
      })
      .catch(function () { /* aborted or offline: keep whatever is on screen */ })
      .then(function () {
        clearTimeout(spin);
        if (ctl === viewCtl) { viewCtl = null; hideChartLoading(); }
        if (done) done();
      });
  }

  function chartLoadOlder(fromMs, done) {
    if (fromMs >= loadedMinX || chartFetching) { if (done) done(); return; }
    chartFetching = true;
    var until = Math.floor(loadedMinX) - 1;
    fetch('/devices/' + DV.serial + '/chart-data?from=' + Math.floor(fromMs) + '&until=' + until, { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) { if (d && d.late) mergeLate(d.late);
        if (d) {
          // Reassigns, not mutates — repoint window.mdmChart's copies too, or the
          // live-SSE pusher keeps appending to the array from before this widened
          // the window and nothing on screen reflects new live data anymore.
          if (HAS_BATTERY && d.battery && d.battery.length) {
            allBattery = d.battery.concat(allBattery);
            if (window.mdmChart) window.mdmChart.allBattery = allBattery;
          }
          if (d.charge && d.charge.length) {
            allCharge = d.charge.concat(allCharge);
            if (window.mdmChart) window.mdmChart.allCharge = allCharge;
          }
          // Only widen a series that has actually been loaded for the recent window.
          // Prepending older points to an unfetched series leaves a hole in front of
          // them, which the rail reads as a stale value.
          if (tempLoaded && d.temp && d.temp.length) {
            allTemp = d.temp.concat(allTemp);
            if (window.mdmChart) window.mdmChart.allTemp = allTemp;
          }
          if (ramLoaded && d.ram && d.ram.length) {
            allRam = d.ram.concat(allRam);
            if (window.mdmChart) window.mdmChart.allRam = allRam;
          }
          loadedMinX = fromMs;
        }
      })
      .catch(function () {})
      .then(function () { chartFetching = false; if (done) done(); });
  }
  // ── Background warm-up: widen to 48h right after first paint ────────────────
  // The page bakes only the default 6h window (a wider bake is the biggest cost
  // in first paint). Clicking 12h/24h/48h then had to fetch, and on a chatty
  // device that fetch is thousands of rows off a multi-GB table — seconds of
  // "Loading graphs…" every time. Pull the rest in the background once the first
  // chart is on screen, in two steps so 24h is usable before 48h lands, and the
  // range buttons become instant. Silent: no spinner, and failures are ignored
  // because the on-demand path still covers the click.
  function chartWarmBackground() {
    // Temp and RAM first: the rail shows them on every tab, so they have to be real
    // before the window widens (chartLoadOlder skips a series that was never loaded).
    // One fetch for both — the endpoint returns every series anyway.
    if (tempLoaded && ramLoaded) { widenWindow(); return; }
    var until = Math.floor(Date.now()), from = Math.floor(loadedMinX);
    if (from >= until) { markLoaded('temp'); markLoaded('ram'); widenWindow(); return; }
    fetch('/devices/' + DV.serial + '/chart-data?from=' + from + '&until=' + until, { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) { if (d && d.late) mergeLate(d.late);
        if (!d) return;
        if (!tempLoaded && d.temp) {
          allTemp = d.temp; markLoaded('temp');
          if (window.mdmChart) window.mdmChart.allTemp = allTemp;
        }
        if (!ramLoaded && d.ram) {
          allRam = d.ram; markLoaded('ram');
          if (window.mdmChart) window.mdmChart.allRam = allRam;
        }
        updateCharts();
        if (activeTab === 'all' && unifiedChart) applyUnifiedData();
      })
      .catch(function () {})
      .then(widenWindow);
  }
  function widenWindow() {
    var steps = [24 * 3600 * 1000, 48 * 3600 * 1000];
    (function next(i) {
      if (i >= steps.length) return;
      var from = Date.now() - steps[i];
      if (from >= loadedMinX) { next(i + 1); return; }   // already loaded
      chartLoadOlder(from, function () {
        // Redraw only if the user is looking at a range this just filled in.
        if (currentDuration >= steps[i]) {
          updateCharts();
          if (activeTab === 'all' && unifiedChart) applyUnifiedData();
        }
        // The navigator shows the last two days: draw what just arrived.
        if (window.vtNavSync) window.vtNavSync();
        setTimeout(function () { next(i + 1); }, 400);
      });
    })(0);
  }
  window.mdmChartWarm = chartWarmBackground;

  // ── Catch up on live points an open tab missed ──────────────────────────────
  // The visible window is anchored to the newest loaded sample. A tab left open
  // while the SSE stream dropped (laptop asleep, server redeploy, a device that
  // went quiet for days) therefore kept drawing its old window with no hint that
  // newer check-ins existed — the "graph shows Sep 8 but the CSV has today" report.
  // Pull anything newer than what's loaded whenever the tab comes back, the stream
  // reconnects, or every couple of minutes.
  function chartNewestX() {
    var xs = [];
    if (allBattery.length) xs.push(allBattery[allBattery.length - 1].x);
    if (allTemp.length) xs.push(allTemp[allTemp.length - 1].x);
    if (allRam.length) xs.push(allRam[allRam.length - 1].x);
    return xs.length ? Math.max.apply(null, xs) : 0;
  }
  let chartCatchingUp = false;
  function chartCatchUp() {
    if (chartCatchingUp || chartFetching) return;
    var newest = chartNewestX();
    if (!newest || Date.now() - newest < 120000) return;   // already current
    chartCatchingUp = true;
    fetch('/devices/' + DV.serial + '/chart-data?from=' + (Math.floor(newest) + 1) + '&until=' + Math.floor(Date.now()), { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) { if (d && d.late) mergeLate(d.late);
        if (!d) return;
        var added = 0;
        function fresher(pts) { return (pts || []).filter(function (p) { return p.x > newest; }); }
        if (HAS_BATTERY) {
          var b = fresher(d.battery);
          if (b.length) { allBattery = allBattery.concat(b); if (window.mdmChart) window.mdmChart.allBattery = allBattery; added += b.length; }
          var ch = (d.charge || []).filter(function (r) { return r.from > newest; });
          if (ch.length) { allCharge = allCharge.concat(ch); if (window.mdmChart) window.mdmChart.allCharge = allCharge; }
        }
        if (tempLoaded) {
          var t = fresher(d.temp);
          if (t.length) { allTemp = allTemp.concat(t); if (window.mdmChart) window.mdmChart.allTemp = allTemp; added += t.length; }
        }
        if (ramLoaded) {
          var m = fresher(d.ram);
          if (m.length) { allRam = allRam.concat(m); if (window.mdmChart) window.mdmChart.allRam = allRam; added += m.length; }
        }
        if (!added) return;
        if (window.mdmChart) window.mdmChart.lastPlottedMs = chartNewestX();
        updateCharts();
        if (activeTab === 'all' && unifiedChart) applyUnifiedData();
      })
      .catch(function () {})
      .then(function () { chartCatchingUp = false; });
  }
  window.mdmChartCatchUp = chartCatchUp;
  document.addEventListener('visibilitychange', function () { if (!document.hidden) chartCatchUp(); });
  (window.mdmInterval || setInterval)(chartCatchUp, 120000);

  const commandMarkers = DV.commandMarkers;
  const markerStorageKey = 'mdm-chart-markers:v2:' + DV.serial;
  const buildMarkers = DV.buildMarkers;
  const buildStorageKey = 'mdm-chart-builds:v1:' + DV.serial;

  // A device is silent between check-ins by design: telemetry is change-gated, the
  // HTTP safety net floors the interval at 5 min when unplugged, alarms carry up to
  // 20% jitter, and Doze batches them further. So silence only means "offline" once
  // it outlasts all of that. This mirrors the staleness cap in StreamExportCycles
  // (db.go) — GREATEST(poll × 10, 15 min) — so the chart and the CSV export agree
  // about when a device went dark. Change one, change the other.
  const pollIntervalMs = DV.pollIntervalMs;
  const gapThresholdMs = Math.max((pollIntervalMs > 0 ? pollIntervalMs : 30000) * 10, 900000);

  // Whether the device currently has a live WebSocket connection. This says nothing
  // about the past — see insertGaps.
  // Not const: the presence SSE event keeps it current, so a device dropping off
  // grows a trailing gap without needing a reload.
  let deviceOnline = DV.online;

  // How far past the newest sample the chart should reach when the device has gone
  // dark, or 0 while it is connected / still inside normal batching silence.
  //
  // insertGaps and filterWindow both call this so they cannot disagree. They used to:
  // insertGaps appended a trailing break at lastPoint + gapThresholdMs while
  // filterWindow pinned the axis max to lastPoint, which put the break outside the
  // plot area. Chart.js clipped it, and a device that was still offline drew a line
  // that simply ended — indistinguishable from one reporting normally. The break was
  // only ever visible once the device came back and the gap had a point on both sides.
  //
  // Capped at one window width so a multi-day outage doesn't squash the actual data
  // into a sliver at the left edge; a window of empty axis already reads as "it stopped".
  function trailingSilenceMs(lastPointX) {
    if (deviceOnline) return 0;
    const silent = Date.now() - lastPointX;
    if (silent <= gapThresholdMs) return 0;
    return Math.min(silent, Math.max(currentDuration, gapThresholdMs * 1.5));
  }

  // Insert a null sample between consecutive points further apart than
  // gapThresholdMs, so the line breaks over windows where the device was dark.
  // Expects a time-sorted {x,y} array; returns a new array.
  //
  // Historical gaps are drawn whether or not the device is connected right now.
  // Reconnecting does not retroactively fill in the hours it was unreachable, and
  // suppressing gaps while online made the chart depend on when the page happened
  // to be loaded: the same outage was visible on one reload and gone on the next.
  //
  // deviceOnline only governs the trailing edge, below.
  // A break must mean "this device went quiet", not "we are drawing thinned data".
  // The server returns at most ~2500 points for whatever range it is asked for, so a
  // 30-day window arrives ~17 minutes apart — past the fixed 15-minute threshold, which
  // put a break between every pair and turned the line into isolated dots with the area
  // fill dropping to zero between them. Measure the series being drawn and call a gap a
  // gap only when it is well past that series' own spacing.
  function gapThresholdFor(pts) {
    if (!pts || pts.length < 4) return gapThresholdMs;
    // Average spacing across the window, not the median of the deltas: the server
    // thins by keeping each bucket's first, min and max, so the deltas are bimodal —
    // clusters of near-zero gaps inside a bucket and one jump between buckets. The
    // median lands on the near-zero ones and every between-bucket jump then reads as
    // an outage, which is how a device reporting every 30 seconds drew 88 breaks.
    const span = pts[pts.length - 1].x - pts[0].x;
    const spacing = span / (pts.length - 1);
    // Six times the average spacing, measured against the real distribution rather
    // than guessed: on a 30-day window of a device that reports every 30 seconds and
    // was genuinely dark three times, four times the spacing still drew 26 breaks
    // (thinning spreads a bucket's representatives out), while six draws two. The
    // fixed floor keeps short windows honest, where the spacing is small and a real
    // twenty-minute silence should still break the line.
    return Math.max(gapThresholdMs, spacing * 6);
  }

  function insertGaps(pts, thr) {
    if (!pts || pts.length < 2) return pts;
    if (thr === undefined) thr = gapThresholdFor(pts);
    const out = [];
    for (let i = 0; i < pts.length; i++) {
      if (i > 0 && (pts[i].x - pts[i - 1].x) > thr) {
        out.push({ x: (pts[i - 1].x + pts[i].x) / 2, y: null });
      }
      out.push(pts[i]);
    }
    // Trailing edge: a device that is offline now has been silent since its last
    // point. Once that silence exceeds the threshold, break the line at the end so
    // the curve doesn't trail off looking current. A connected device is exempt —
    // it simply has nothing new to report.
    //
    // The break sits one threshold past the last point, and filterWindow extends the
    // axis by trailingSilenceMs — always the larger of the two — so the break lands
    // inside the plot area with empty space after it.
    if (out.length) {
      const last = out[out.length - 1];
      if (last.y !== null && trailingSilenceMs(last.x) > 0) {
        out.push({ x: last.x + thr, y: null });
      }
    }
    return out;
  }

  // Battery % arrives integer-quantised and very dense (tens of thousands of points
  // over history). Drawn raw, a 1% change between two touching samples renders as a
  // vertical stair no spline can round. This buckets the visible window into ~targetN
  // even-time buckets and averages each, yielding FRACTIONAL, evenly-spaced points a
  // cubic spline can flow through — smooth and accurate (centred, so no EMA-style lag
  // or drift), and far fewer points to draw. Null gap-markers split runs so offline
  // breaks survive; each bucket keeps its last raw wlc for segment colouring.
  function resampleAvg(points, targetN) {
    if (!points || points.length <= targetN) return points;
    const runs = []; let cur = [];
    for (let i = 0; i < points.length; i++) {
      if (points[i].y == null) { if (cur.length) { runs.push(cur); cur = []; } }
      else cur.push(points[i]);
    }
    if (cur.length) runs.push(cur);
    const total = runs.reduce((s, r) => s + r.length, 0) || 1;
    const out = [];
    for (let ri = 0; ri < runs.length; ri++) {
      const run = runs[ri];
      if (ri > 0) { const pe = runs[ri - 1]; out.push({ x: (pe[pe.length - 1].x + run[0].x) / 2, y: null }); }
      const share = Math.max(2, Math.round(targetN * run.length / total));
      if (run.length <= share) { for (let k = 0; k < run.length; k++) out.push(run[k]); continue; }
      const x0 = run[0].x, span = (run[run.length - 1].x - x0) || 1;
      const buckets = new Array(share);
      for (let k = 0; k < run.length; k++) {
        let b = Math.floor((run[k].x - x0) / span * share); if (b >= share) b = share - 1; if (b < 0) b = 0;
        (buckets[b] || (buckets[b] = [])).push(run[k]);
      }
      for (let b = 0; b < share; b++) {
        const arr = buckets[b]; if (!arr || !arr.length) continue;
        let sy = 0; for (let k = 0; k < arr.length; k++) sy += arr[k].y;
        const last = arr[arr.length - 1];
        out.push({ x: arr[Math.floor(arr.length / 2)].x, y: sy / arr.length, wlc: last.wlc });
      }
    }
    return out;
  }

  // Centred moving average (symmetric window, so no lag/drift like an EMA). Turns the
  // integer-% staircase into a smooth fractional ramp. Nulls reset the window so
  // offline breaks stay hard. wlc is carried from the centre sample for colouring.
  function centeredMA(points, win) {
    if (!points || points.length < 3) return points;
    const half = Math.max(1, Math.floor(win / 2));
    const out = new Array(points.length);
    for (let i = 0; i < points.length; i++) {
      const p = points[i];
      if (p.y == null) { out[i] = p; continue; }
      let sum = 0, cnt = 0;
      for (let j = i - half; j <= i + half; j++) {
        if (j < 0 || j >= points.length) continue;
        const q = points[j];
        if (!q || q.y == null) continue;
        sum += q.y; cnt++;
      }
      out[i] = cnt ? { x: p.x, y: sum / cnt, wlc: p.wlc } : p;
    }
    return out;
  }

  // Battery % holds flat for long runs then steps by 1. Plotting every check-in draws
  // that staircase. Instead keep only the points where the value CHANGES (the corners),
  // placed at the midpoint of each flat run so the step is centred, then let the cubic
  // spline flow through them — a smooth curve that still passes through the real values.
  // Nulls (offline gaps) pass through untouched; endpoints are always kept.
  //
  // Charging-pad (wlc) colouring is NOT done here — splitting flat runs at every wlc
  // flip would add vertices and make the spline choppy. The line keeps these sparse
  // change-points for a smooth shape, and the stroke is painted by an x-axis gradient
  // (buildBatteryGradient) keyed to the raw wlc timeline instead.
  function toChangePoints(points) {
    if (!points || points.length < 3) return points;
    const out = [];
    let runStart = 0;
    function flush(endIdxExclusive) {
      const a = points[runStart], b = points[endIdxExclusive - 1];
      const midX = (a.x + b.x) / 2;
      out.push({ x: midX, y: a.y, wlc: b.wlc });
    }
    for (let i = 0; i < points.length; i++) {
      const p = points[i];
      if (p.y == null) {
        if (i > runStart) flush(i);
        out.push(p);                 // gap marker
        runStart = i + 1;
        continue;
      }
      if (i > runStart && points[i].y !== points[runStart].y) {
        flush(i);                    // value changed — close the previous flat run
        runStart = i;
      }
    }
    if (runStart < points.length && points[points.length - 1].y != null) flush(points.length);
    // Always anchor the true first and last samples so the curve spans the full window.
    const first = points[0], last = points[points.length - 1];
    if (first.y != null && (!out.length || out[0].x !== first.x)) out.unshift({ x: first.x, y: first.y, wlc: first.wlc });
    if (last.y != null && out[out.length - 1].x !== last.x) out.push({ x: last.x, y: last.y, wlc: last.wlc });
    return out;
  }

  let currentDuration = 6 * 3600 * 1000;
  let customMin = null, customMax = null;
  let batteryChart, tempChart, ramChart, unifiedChart;
  let storedMarkerPref = localStorage.getItem(markerStorageKey);
  // Commands are not marked on the graph; build changes always are (14 Oct).
  let showMarkers = false;
  let storedBuildPref = null;
  try { storedBuildPref = localStorage.getItem(buildStorageKey); } catch (e) {}
  let showBuilds = true;
  // Wireless-charging-state colouring is always on: the raw battery line is
  // coloured by WLC state with the legend shown (no per-device toggle).
  // The chart tab is driven by context (a ?focus deep-link), not remembered across
  // visits — so a battery unit never reopens on the Temp tab just because it was
  // viewed there once. Defaults to Battery when there's no focus.
  // Tab set + default depend on whether this device has a battery. Kiosks drop
  // the battery tab entirely and default to temp.
  const CHART_TABS = HAS_BATTERY ? ['battery','temp','ram','all'] : ['temp','ram','all'];
  let activeTab = HAS_BATTERY ? 'battery' : 'temp';

  function fmtTime(ms) {
    return new Date(ms).toLocaleTimeString([], tzArg({hour: "2-digit", minute: "2-digit"}));
  }

  function sameCalendarDay(a, b) {
    const da = new Date(a), db = new Date(b);
    return da.getFullYear() === db.getFullYear() && da.getMonth() === db.getMonth() && da.getDate() === db.getDate();
  }

  // X-axis tick label. Normally just the time (AM/PM). When the visible window
  // spans more than one calendar day (e.g. a 48h+ range), the first tick of each
  // day also carries a "Mon D" date line below the time, so a multi-day window
  // isn't an ambiguous wall of times. `this` is the Chart.js scale (min/max in ms).
  function fmtTimeAxis(ms) {
    const d = new Date(ms);
    // Coarse ticks (a day or more apart) carry no useful clock time: a day step
    // shows the date only, a month step the month and year. Keeps a zoomed-out
    // axis to one short line per tick instead of a "12:00 AM" + date pair each.
    const step = (this && this._tickStep) || 0;
    if (step >= 28 * 86400e3) return d.toLocaleDateString([], tzArg({month: "short", year: "2-digit"}));
    if (step >= 86400e3) return d.toLocaleDateString([], tzArg({month: "short", day: "numeric"}));
    const t = fmtTime(ms);
    // The date label appears only on the midnight (12 AM) tick of each day; every
    // other tick shows just the time. Ticks are midnight-aligned in evenTimeTicks,
    // so a spanned day always gets exactly one dated tick.
    const isMidnight = d.getHours() === 0 && d.getMinutes() === 0;
    if (!isMidnight) return t;
    return [t, d.toLocaleDateString([], tzArg({month: "short", day: "numeric"}))];
  }

  function wlcColor(w) {
    if (w === 1) return "#e84393";  // device placed (charging on)
    if (w === 0) return "#6c5ce7";  // charging pad vacant (charging on)
    if (w === 2) return "#e8a13a";  // pad faulty / flapping (matches .pad-stat-flapping dot)
    return "#b2bec3";               // charging disabled / unknown (matches legend)
  }

  // Paint the battery stroke by charging-pad state WITHOUT adding geometry vertices:
  // a horizontal (x-axis) canvas gradient whose colour is keyed to the raw wlc timeline
  // stashed on chart.$wlcPts. Each state span holds its colour; at every flip we lay two
  // stops at the same offset (old colour ends, new colour starts) for a crisp edge at the
  // real transition time — the midpoint between the two straddling samples. Because it is
  // built from the raw per-sample wlc (not the sparse plotted points), the colour boundary
  // lands where the state actually changed, not retroactively across a whole flat run.
  // Merge WLC state runs shorter than this into the run before them — the pad's
  // immediate-push-on-change telemetry can land two flips a second or two apart
  // (a single momentary edge-count blip in readSettledWlc), which would otherwise
  // paint a near-invisible sliver on the gradient. Display-only: doesn't touch the
  // stored samples or the tooltip's raw nearest-reading lookup.
  const WLC_NOISE_FLOOR_MS = 60000;
  function denoiseWlcRuns(rp) {
    if (rp.length < 3) return rp;
    const out = rp.map(p => Object.assign({}, p));
    let i = 0;
    while (i < out.length) {
      let j = i;
      while (j + 1 < out.length && out[j + 1].wlc === out[i].wlc) j++;
      if (i > 0 && (out[j].x - out[i].x) < WLC_NOISE_FLOOR_MS) {
        const prevW = out[i - 1].wlc;
        for (let k = i; k <= j; k++) out[k].wlc = prevW;
      }
      i = j + 1;
    }
    return out;
  }

  // ── Charging strip ──────────────────────────────────────────────────────────
  // A thin band under the battery chart, filled where the device reported
  // charging=true. It is separate from the line colour on purpose: that colour is the
  // charging PAD (placed / vacant / misplaced), and a device on a pad is often not
  // charging (full, disabled) while one off the pad often is (USB). Neighbouring spans
  // meet at the midpoint between their check-ins; a silence past gapThresholdMs is
  // left blank, the same place the battery line breaks.
  const CHARGE_STRIP_H = 6, CHARGE_STRIP_GAP = 5;
  const CHARGE_ON = '#00b894', CHARGE_OFF = 'rgba(122,127,154,0.22)';
  // A flapping charger — a bad cable toggling every second or two, stored as one state
  // (charger_flapping). Yellow, and hatched so it reads as a fault rather than a third
  // kind of charging, and stays apart from the green without relying on colour.
  const CHARGE_FLAP = '#fdcb6e', CHARGE_FLAP_STRIPE = '#e0a93a';
  function chargeFlapFill(chart) {
    if (chart.$chargeFlapFill) return chart.$chargeFlapFill;
    const tile = document.createElement('canvas');
    tile.width = tile.height = 6;
    const g = tile.getContext('2d');
    g.fillStyle = CHARGE_FLAP;
    g.fillRect(0, 0, 6, 6);
    g.strokeStyle = CHARGE_FLAP_STRIPE;
    g.lineWidth = 2;
    g.beginPath();
    g.moveTo(-1, 7); g.lineTo(7, -1);   // the diagonal, plus its two corner ends so
    g.moveTo(-1, 1); g.lineTo(1, -1);   // the stripes join up across tiles
    g.moveTo(5, 7); g.lineTo(7, 5);
    g.stroke();
    chart.$chargeFlapFill = chart.ctx.createPattern(tile, 'repeat') || CHARGE_FLAP;
    return chart.$chargeFlapFill;
  }
  // Sub-minute flips are the charger's noise, not the device's behaviour: a pad losing
  // contact for one check-in, or a charger negotiating. Drawn literally they slice the
  // strip into slivers that read as a fault. Runs shorter than this are absorbed into
  // whatever surrounds them; anything a person would call an event survives.
  const CHARGE_NOISE_MS = 120000;
  function despeckleCharge(runs) {
    if (runs.length < 3) return runs;
    const out = [runs[0]];
    for (let i = 1; i < runs.length; i++) {
      const r = runs[i], prev = out[out.length - 1], next = runs[i + 1];
      const brief = r.to - r.from < CHARGE_NOISE_MS;
      // Only absorbed when both neighbours agree with each other — a short run between
      // two different states is a real transition, and flattening it would invent one.
      if (brief && next && prev.c === next.c && r.c !== prev.c) {
        prev.to = next.to;
        i++; // the neighbour just got merged in
        continue;
      }
      if (prev.c === r.c) { prev.to = r.to; continue; }
      out.push({ from: r.from, to: r.to, c: r.c });
    }
    return out;
  }
  function chargeSpans(rawRuns) {
    // A run's state as one value: true, false, null, or 'flap'. Copies, so despeckling
    // never edits the runs it was handed.
    const runs = despeckleCharge(rawRuns.map(function (r) {
      return { from: r.from, to: r.to, c: r.f ? 'flap' : r.c };
    }));
    const out = [];
    for (let i = 0; i < runs.length; i++) {
      const r = runs[i], prev = runs[i - 1], next = runs[i + 1];
      const from = prev && r.from - prev.to <= gapThresholdMs ? (prev.to + r.from) / 2 : r.from;
      const to = next && next.from - r.to <= gapThresholdMs ? (r.to + next.from) / 2 : r.to;
      out.push({ from: from, to: to, c: r.c });
    }
    return out;
  }
  function chargeAt(ms) {
    const spans = chargeSpans(srcCharge());
    for (let i = 0; i < spans.length; i++) {
      if (ms >= spans[i].from && ms <= spans[i].to) return spans[i].c;
    }
    return null;
  }
  // Live check-ins: extend the newest span, or open a new one on a flip or a gap.
  function pushCharge(x, c) {
    const last = allCharge[allCharge.length - 1];
    if (last && x <= last.to) return;
    if (last && last.c === c && x - last.to <= gapThresholdMs) { last.to = x; return; }
    allCharge.push({ from: x, to: x, c: c });
  }
  const chargeStripPlugin = {
    id: 'chargeStrip',
    afterDraw(chart) {
      const ca = chart.chartArea, xs = chart.scales && chart.scales.x;
      if (!ca || !xs || !srcCharge().length) return;
      const ctx = chart.ctx, y = ca.bottom + CHARGE_STRIP_GAP;
      ctx.save();
      chargeSpans(srcCharge()).forEach(function (sp) {
        if (sp.c === null || sp.c === undefined) return;
        const x0 = Math.max(ca.left, xs.getPixelForValue(sp.from));
        const x1 = Math.min(ca.right, xs.getPixelForValue(sp.to));
        if (x1 < ca.left || x0 > ca.right) return;
        ctx.fillStyle = sp.c === 'flap' ? chargeFlapFill(chart) : sp.c ? CHARGE_ON : CHARGE_OFF;
        ctx.fillRect(x0, y, Math.max(1, x1 - x0), CHARGE_STRIP_H);
      });
      ctx.restore();
    },
  };

  function buildBatteryGradient(chart) {
    const area = chart.chartArea, xs = chart.scales && chart.scales.x;
    const pts = chart.$wlcPts;
    if (!area || !xs || !pts || !pts.length) return wlcColor(0);
    let rp = pts.filter(p => p && p.y != null);   // drop gap markers
    if (!rp.length) return wlcColor(0);
    rp = denoiseWlcRuns(rp);
    const left = area.left, span = (area.right - area.left) || 1;
    const off = x => Math.max(0, Math.min(1, (xs.getPixelForValue(x) - left) / span));
    const g = chart.ctx.createLinearGradient(area.left, 0, area.right, 0);
    let curW = rp[0].wlc;
    g.addColorStop(0, wlcColor(curW));
    for (let i = 1; i < rp.length; i++) {
      if (rp[i].wlc !== curW) {
        const o = off((rp[i - 1].x + rp[i].x) / 2);
        g.addColorStop(o, wlcColor(curW));      // old state ends here
        g.addColorStop(o, wlcColor(rp[i].wlc)); // new state starts here
        curW = rp[i].wlc;
      }
    }
    g.addColorStop(1, wlcColor(curW));
    return g;
  }

  function filterWindow(all) {
    if (customMin !== null && customMax !== null) {
      return { pts: all.filter(p => p.x >= customMin && p.x <= customMax), minX: customMin, lastX: customMax };
    }
    const lastPt = all.length > 0 ? all[all.length - 1].x : Date.now();
    // minX stays anchored to the newest sample, so the span of visible data is
    // unchanged; a dark device only adds empty axis on the right, room for the
    // trailing break insertGaps appends. Without it the axis ends on the last
    // sample and the break is clipped away.
    const minX = lastPt - currentDuration;
    const lastX = lastPt + trailingSilenceMs(lastPt);
    return { pts: all.filter(p => p.x >= minX), minX, lastX };
  }

  // Light EMA over battery check-ins so the line reads as a trend without
  // over-flattening. Keeps timestamp + WLC per point (so the segment colour
  // stays precise to the check-in), and — crucially — preserves the null gap
  // markers inserted by insertGaps so offline windows render as breaks.
  function smoothBatterySeries(points, alpha) {
    if (!points || points.length < 3) return points;
    const a = (typeof alpha === 'number' ? alpha : 0.35);
    let prev = null;
    return points.map(function (p) {
      if (p.y == null) { prev = null; return p; }   // gap break — reset the filter
      if (prev === null) { prev = p.y; return p; }
      prev = (a * p.y) + ((1 - a) * prev);
      return Object.assign({}, p, { y: Math.max(0, Math.min(100, Number(prev.toFixed(2)))) });
    });
  }

  // One chart at a time, picked with the Battery / Temp / RAM tabs in the card header.
  window.setChartTab = function setChartTab(tab) {
    if (tab === 'all' || CHART_TABS.indexOf(tab) < 0) tab = HAS_BATTERY ? 'battery' : 'temp';
    activeTab = tab;
    CHART_TABS.forEach(function (t) {
      var el = document.getElementById('chart-panel-' + t);
      if (el) el.style.display = t === tab ? '' : 'none';
      var btn = document.querySelector('.vt-tab[data-chart-tab="' + t + '"]');
      if (btn) { btn.classList.toggle('active', t === tab); btn.setAttribute('aria-selected', t === tab ? 'true' : 'false'); }
    });
    function nav() { if (window.vtNavSync) window.vtNavSync(); }
    if (tab === 'temp') loadMetricLazy('temp', false, nav);
    if (tab === 'ram') loadMetricLazy('ram', false, nav);
    nav();
    var c = tab === 'temp' ? tempChart : tab === 'ram' ? ramChart : batteryChart;
    if (c) setTimeout(function () { try { c.resize(); } catch (e) {} }, 0);
    document.querySelectorAll('#vt-legend .vt-lg').forEach(function (l) { l.hidden = l.getAttribute('data-lg') !== tab; });
  };

  function applyData(chart, all) {
    const { pts, minX, lastX } = filterWindow(all);
    // Battery: resample the dense integer window to ~220 fractional bucket-averages
    // so the spline flows smoothly; other metrics vary enough to draw raw.
    if (chart === batteryChart) chart.$wlcPts = pts;  // raw per-sample wlc for the stroke gradient
    chart.data.datasets[0].data = (chart === batteryChart)
      ? toChangePoints(insertGaps(pts))
      : insertGaps(pts);
    chart.options.scales.x.min = minX;
    chart.options.scales.x.max = lastX;
    chart.update('none'); // no animation: live telemetry re-renders often; animating each = a visible flash
  }

  function fmtDate(ms) {
    return new Date(ms).toLocaleDateString([], tzArg({month: "short", day: "numeric"})) + " " + fmtTime(ms);
  }

  function applyUnifiedData() {
    if (!unifiedChart) return;
    const { pts: bPts, minX, lastX } = filterWindow(srcBattery());
    unifiedChart.data.datasets[0].data = toChangePoints(insertGaps(bPts));
    unifiedChart.data.datasets[1].data = insertGaps(filterWindow(srcTemp()).pts);
    unifiedChart.data.datasets[2].data = insertGaps(filterWindow(srcRam()).pts);
    unifiedChart.options.scales.x.min = minX;
    unifiedChart.options.scales.x.max = lastX;
    unifiedChart.update('none');
  }

  // A custom window, short enough for the range button: "Oct 14, 09:52–15:52" within a
  // day, "Oct 13 09:06 – Oct 14 09:06" across days, dates only for long spans.
  function vtRangeLabel(a, b) {
    var day = function (ms) { return new Date(ms).toLocaleDateString([], tzArg({ month: 'short', day: 'numeric' })); };
    var hm = function (ms) { return new Date(ms).toLocaleTimeString([], tzArg({ hour: '2-digit', minute: '2-digit', hour12: false })); };
    if (day(a) === day(b)) return day(a) + ', ' + hm(a) + '\u2013' + hm(b);
    if (b - a >= 3 * 86400e3) return day(a) + ' \u2013 ' + day(b);
    return day(a) + ' ' + hm(a) + ' \u2013 ' + day(b) + ' ' + hm(b);
  }

  function updateCharts() {
    const { pts } = filterWindow(srcBattery());
    const labels = {3600000: "Last hour", 21600000: "Last 6 hours", 43200000: "Last 12 hours", 86400000: "Last 24 hours", 172800000: "Last 48 hours", 604800000: "Last 7 days"};
    // The window label renders on the graph (#chart-window-label), not in the control
    // row, so a wide custom/shifted range can't wrap the toolbar. The old #chart-info
    // slot is kept empty to hold the row's layout stable.
    var _wtext = (customMin !== null && customMax !== null)
      ? vtRangeLabel(customMin, customMax)
      : (labels[currentDuration] || "Last " + Math.round(currentDuration / 3600000) + " hours");
    var _wl = document.getElementById("chart-window-label");
    if (_wl) { _wl.textContent = _wtext; _wl.title = _wtext; }
    var _ci = document.getElementById("chart-info");
    if (_ci) _ci.textContent = "";

    if (window.mdmSyncShift) window.mdmSyncShift();
    if (batteryChart) applyData(batteryChart, srcBattery());
    if (tempChart) applyData(tempChart, srcTemp());
    if (ramChart) applyData(ramChart, srcRam());
    applyUnifiedData();
    syncMarkerToggle();
    refreshTabVals();
    updateFooter(null);
    if (window.vtNavSync) window.vtNavSync();
  }

  // Live per-tab value badges (Design-1 style): show each metric's latest reading on
  // its tab so all numbers are visible without switching. Newest point is last.
  function refreshTabVals() {
    function set(id, arr, suffix) {
      var el = document.getElementById(id);
      if (!el || !arr || !arr.length) return;
      var v = arr[arr.length - 1].y;
      if (v == null || isNaN(v)) return;
      el.textContent = (Math.round(v * 10) / 10) + suffix;
    }
    if (DV.showBattery) set('tabval-battery', allBattery, '%');
    set('tabval-temp', allTemp, '°C');
    set('tabval-ram', allRam, '%');
  }

  function nearestInArray(arr, ms) {
    if (!arr || !arr.length) return null;
    let best = null, bestDist = Infinity;
    for (let i = 0; i < arr.length; i++) {
      const d = Math.abs(arr[i].x - ms);
      if (d < bestDist) { bestDist = d; best = arr[i]; }
    }
    return best;
  }

  // Battery cycles inside the window on screen. A cycle is a real excursion — the
  // pack charged to CYCLE_FULL or above, then later run down to CYCLE_EMPTY or below;
  // it is counted at the empty crossing. A dip to 35% and back is not a cycle. This is
  // a different measure from the "Battery cycles" in About, which totals lifetime
  // discharge ÷ 100 (wear), and the two are not expected to agree.
  const CYCLE_FULL = 95, CYCLE_EMPTY = 10;

  // The battery reading just before the window, which arms the walk below. Without it a
  // pack charged to full before the window opened never arms, so its discharge inside the
  // window went uncounted: a 48 h view read zero while the month held four cycles.
  function cycleAnchor() {
    return viewData && viewData.battery_before ? viewData.battery_before.y : null;
  }

  function cyclesIn(arr, carry) {
    if (!arr || !arr.length) return 0;
    let n = 0, armed = carry != null && !isNaN(carry) && carry >= CYCLE_FULL;
    for (let i = 0; i < arr.length; i++) {
      const y = arr[i].y;
      if (y == null || isNaN(y)) continue;
      if (y >= CYCLE_FULL) armed = true;
      else if (armed && y <= CYCLE_EMPTY) { n++; armed = false; }
    }
    return n;
  }

  // Lifetime wear by day: [[epochMs, cumulativeDischargePct], …]. This is the series
  // behind the "Battery cycles" card (discharge ÷ 100), so the graph and the card
  // finally answer the same question. Empty when the device has no rolled-up days yet.
  var CYCLE_SERIES = Array.isArray(CYCLE_SERIES_RAW) ? CYCLE_SERIES_RAW : [];

  // Index of the last rolled-up day at or before ms, or -1 before the series starts.
  function cycleDayIndex(ms) {
    if (!CYCLE_SERIES.length || ms == null || ms < CYCLE_SERIES[0][0]) return -1;
    var lo = 0, hi = CYCLE_SERIES.length - 1, best = -1;
    while (lo <= hi) {
      var mid = (lo + hi) >> 1;
      if (CYCLE_SERIES[mid][0] <= ms) { best = mid; lo = mid + 1; } else { hi = mid - 1; }
    }
    return best;
  }

  // Cycles this pack had done by the hovered moment.
  //
  // The rolled-up series is per DAY, so on its own the readout sat still for hours and
  // then jumped — useless for reading a graph whose x axis is hours. So the day total is
  // only the floor: whole days before the hovered one come from the rollup, and the part
  // of the current day that has already happened is measured off the battery line
  // itself, by summing its downward steps. That moves with the pointer at the same
  // resolution as the battery readout beside it, and still lands on the lifetime number
  // the Battery cycles card shows.
  function lifetimeCyclesAt(ms, batt) {
    if (!CYCLE_SERIES.length || ms == null) return null;
    var i = cycleDayIndex(ms);
    if (i < 0) return 0;
    // Whole days strictly before the hovered day.
    var base = i > 0 ? CYCLE_SERIES[i - 1][1] : 0;
    var dayStart = CYCLE_SERIES[i][0];
    // Discharge inside the hovered day, as far as the pointer. Only the plotted series
    // is available, so a window that opens mid-day misses the hours before it — the
    // number can only be short, never inflated, and it is exact once the day is in view.
    var within = 0;
    if (batt && batt.length) {
      for (var k = 1; k < batt.length; k++) {
        var x = batt[k].x;
        if (x <= dayStart) continue;
        if (x > ms) break;
        var prev = batt[k - 1].y, cur = batt[k].y;
        if (prev == null || cur == null || isNaN(prev) || isNaN(cur)) continue;
        if (cur < prev) within += prev - cur;
      }
    }
    return (base + within) / 100;
  }

  // The window-local fallback: completed 95%→10% excursions up to the hovered point,
  // used only when there is no rolled-up wear history to read.
  function cyclesUpTo(arr, carry, ms) {
    if (!arr || !arr.length) return 0;
    if (ms == null) return cyclesIn(arr, carry);
    let n = 0, armed = carry != null && !isNaN(carry) && carry >= CYCLE_FULL;
    for (let i = 0; i < arr.length; i++) {
      if (arr[i].x > ms) break;
      const y = arr[i].y;
      if (y == null || isNaN(y)) continue;
      if (y >= CYCLE_FULL) armed = true;
      else if (armed && y <= CYCLE_EMPTY) { n++; armed = false; }
    }
    return n;
  }

  // The readout panel beside the plot. One check-in — the nearest to the pointer —
  // not a nearest-per-metric mix (FW-2026-000053), and its own timestamp.
  function setReadout(id, text, unit) {
    const el = document.getElementById(id);
    if (!el) return;
    el.textContent = text;
    if (unit) { const u = document.createElement('small'); u.textContent = unit; el.appendChild(u); }
  }

  function updateFooter(ms, markerText) {
    const panel = document.getElementById('chart-readout');
    if (panel) return updateReadout(ms, markerText);
    const el = document.getElementById('chart-footer-text');
    if (!el) return;
    if (ms == null) { el.textContent = '—'; return; }
    // Read from the series the graph is actually drawing (srcX()), not the live
    // arrays: a custom window (7d, 30d, a dragged range) is served by its own fetch,
    // and reading allBattery there returned the nearest point of the default 48h
    // window for every hover — the same handful of values wherever you pointed, on
    // every device (FW-2026-000053).
    const bPt = nearestInArray(srcBattery(), ms);
    const tPt = nearestInArray(srcTemp(), ms);
    const rPt = nearestInArray(srcRam(), ms);
    const parts = [fmtDate(ms)];
    if (bPt != null && bPt.y != null) {
      // WLC text comes from the raw nearest reading (not the smoothed line), so the
      // label is the exact wireless-charging state recorded at that checkin.
      let s = 'Battery: ' + bPt.y + '%';
      if (bPt.wlc === 1) s += ' · ⚡ Wireless charging in use';
      else if (bPt.wlc === 0) s += ' · Wireless pad vacant';
      else if (bPt.wlc === 2) s += ' · ⚠ Wireless pad misplaced — please place it again';
      else if (bPt.wlc !== undefined && bPt.wlc !== null) s += ' · ⚠ Pad Unreadable';
      if (srcCharge().length) {
        const c = chargeAt(ms);
        if (c === 'flap') s += ' · ⚠ Charger flapping';
        else if (c === true) s += ' · Charging';
        else if (c === false) s += ' · Not charging';
      }
      parts.push(s);
    }
    if (tPt != null && tPt.y != null) parts.push('Temp: ' + tPt.y.toFixed(1) + '°C');
    if (rPt != null && rPt.y != null) parts.push('RAM: ' + rPt.y.toFixed(1) + '%');
    if (markerText) parts.push(markerText);
    el.textContent = parts.join('  ·  ');
  }

  function updateReadout(ms, markerText) {
    const batt = srcBattery(), temp = srcTemp(), ram = srcRam();
    // At rest (no pointer) the panel shows the newest check-in in view rather than
    // going blank — the column is always worth reading.
    if (ms == null) {
      const last = (batt && batt.length ? batt : (temp && temp.length ? temp : ram));
      ms = last && last.length ? last[last.length - 1].x : null;
      if (ms == null) { setReadout('cr-when', '—'); return; }
    }
    const bPt = nearestInArray(batt, ms);
    const tPt = nearestInArray(temp, ms);
    const rPt = nearestInArray(ram, ms);
    // The check-in the panel is describing is the nearest battery reading when there is
    // one (the line people hover), else whichever series answered.
    const anchor = bPt || tPt || rPt;
    let when = anchor ? fmtDate(anchor.x) : fmtDate(ms);
    if (anchor) {
      const off = Math.round(Math.abs(anchor.x - ms) / 1000);
      if (off >= 5) when += '  ±' + (off < 90 ? off + 's' : Math.round(off / 60) + 'm');
    }
    setReadout('cr-when', when + (markerText ? '  ·  ' + markerText : ''));
    if (bPt && bPt.y != null) setReadout('cr-batt', String(bPt.y), '%'); else setReadout('cr-batt', '—');
    if (tPt && tPt.y != null) setReadout('cr-temp', tPt.y.toFixed(1), '°C'); else setReadout('cr-temp', '—');
    if (rPt && rPt.y != null) setReadout('cr-ram', rPt.y.toFixed(1), '%'); else setReadout('cr-ram', '—');
    // Charging only. The pad state (placed / vacant / misplaced) is already the colour
    // of the battery line and the strip underneath it; repeating it here spent the
    // panel's narrowest line on something the plot says better.
    let note = '';
    if (bPt && srcCharge().length) {
      const c = chargeAt(anchor ? anchor.x : ms);
      if (c === 'flap') note = 'charger flapping';
      else if (c === true) note = 'charging';
      else if (c === false) note = 'not charging';
    }
    setReadout('cr-charge', note);
    // Cycles this pack had done by the hovered moment, counted from the start of its
    // life — the number the "Battery cycles" card shows, so dragging the pointer along
    // the line walks up towards it instead of reporting a window-local count that reads
    // 0 on a pack with fourteen cycles behind it.
    var at = anchor ? anchor.x : ms;
    var life = lifetimeCyclesAt(at, batt);
    if (life != null) setReadout('cr-cycles', life.toFixed(2));
    else setReadout('cr-cycles', String(cyclesUpTo(batt, cycleAnchor(), at)));
  }

  function setHoverInfo(data) { /* replaced by updateReadout */ }

  function syncMarkerToggle() {
    var btn = document.getElementById('toggle-markers-btn');
    if (btn) { btn.classList.toggle('active', showMarkers); btn.setAttribute('aria-pressed', showMarkers ? 'true' : 'false'); }
    var bb = document.getElementById('toggle-builds-btn');
    if (bb) {
      bb.classList.toggle('active', showBuilds);
      bb.setAttribute('aria-pressed', showBuilds ? 'true' : 'false');
      if (!buildMarkers.length) { bb.disabled = true; bb.title = 'No build changes recorded for this device'; }
    }
  }

  function makeGradient(color) {
    return (ctx) => {
      const chart = ctx.chart;
      const {ctx: c, chartArea} = chart;
      if (!chartArea) return "transparent";
      const gradient = c.createLinearGradient(0, chartArea.top, 0, chartArea.bottom);
      gradient.addColorStop(0,   color.replace(")", ",0.18)").replace("rgb", "rgba"));
      gradient.addColorStop(0.5, color.replace(")", ",0.08)").replace("rgb", "rgba"));
      gradient.addColorStop(1,   color.replace(")", ",0.02)").replace("rgb", "rgba"));
      return gradient;
    };
  }

  // Evenly-spaced, human time ticks. Chart.js's linear auto-ticks land on "nice"
  // round MILLISECOND values, which are not nice CLOCK values — so the gaps between
  // ticks (and the days) end up unevenly spaced across the axis (FW-2026-000030
  // follow-up). Replace them with ticks at a single fixed time step chosen from the
  // visible span, so the interval between every tick is the same amount of time.
  const TICK_STEPS_MS = [60e3, 5*60e3, 10*60e3, 15*60e3, 30*60e3, 3600e3, 2*3600e3,
                         3*3600e3, 6*3600e3, 12*3600e3, 86400e3, 2*86400e3, 7*86400e3, 14*86400e3];
  function evenTimeTicks(axis) {
    const min = axis.min, max = axis.max;
    if (min == null || max == null || max <= min) return;
    // About one label per 95px of chart, at most 7, so a narrow chart doesn't overlap them.
    const span = max - min, target = Math.max(3, Math.min(7, Math.floor(((axis.chart && axis.chart.width) || 700) / 95)));
    // Spans beyond what a 14-day step can cover with ~7 ticks switch to calendar
    // months (1st of the month, every 1/2/3/6 months), which a fixed millisecond
    // step can't express. fmtTimeAxis reads axis._tickStep to pick the label form.
    if (span / (14 * 86400e3) > target) {
      const months = span / (30.44 * 86400e3);
      const mStep = months / target <= 1 ? 1 : months / target <= 2 ? 2 : months / target <= 3 ? 3 : 6;
      const d0 = new Date(min); d0.setDate(1); d0.setHours(0, 0, 0, 0);
      d0.setMonth(Math.floor(d0.getMonth() / mStep) * mStep);
      const ticks = [];
      for (let d = new Date(d0); d.getTime() <= max && ticks.length < 64; d.setMonth(d.getMonth() + mStep)) {
        if (d.getTime() >= min) ticks.push({ value: d.getTime() });
      }
      axis._tickStep = mStep * 30.44 * 86400e3;
      if (ticks.length) axis.ticks = ticks;
      return;
    }
    let step = TICK_STEPS_MS[TICK_STEPS_MS.length - 1];
    for (let i = 0; i < TICK_STEPS_MS.length; i++) {
      if (span / TICK_STEPS_MS[i] <= target) { step = TICK_STEPS_MS[i]; break; }
    }
    axis._tickStep = step;
    // Anchor the tick grid to local midnight (not the UTC epoch) so a tick lands
    // exactly on 12 AM — every step here divides a day — which is where fmtTimeAxis
    // prints the date. Falls back cleanly for sub-hour steps within a single day.
    const base0 = new Date(min); base0.setHours(0, 0, 0, 0);
    const base = base0.getTime();
    const first = base + Math.ceil((min - base) / step) * step;
    const ticks = [];
    for (let t = first; t <= max && ticks.length < 64; t += step) ticks.push({ value: t });
    if (ticks.length) axis.ticks = ticks;
  }

  // The battery chart's x axis with its tick labels pushed down to make room for the
  // charging strip drawn between the plot and the labels.
  function chargeStripXScale() {
    const sc = sharedXScale();
    sc.ticks.padding += CHARGE_STRIP_GAP + CHARGE_STRIP_H;
    return sc;
  }
  function sharedXScale() {
    return {
      type: "linear",
      afterBuildTicks: evenTimeTicks,
      ticks: { color: TICK, maxRotation: 0, minRotation: 0, callback: fmtTimeAxis, autoSkip: false, padding: 8 },
      grid: { display: false },
      border: { display: false },
    };
  }

  function getVisibleMarkers(chart) {
    if (!chart._commandMarkers || !chart._commandMarkers.length) return [];
    const xScale = chart.scales.x;
    return chart._commandMarkers
      .filter(function (marker) {
        return marker.x >= xScale.min && marker.x <= xScale.max;
      })
      .map(function (marker) {
        return {
          x: marker.x,
          px: xScale.getPixelForValue(marker.x),
          label: marker.label,
          status: marker.status,
        };
      });
  }

  function clusterMarkers(chart) {
    const visible = getVisibleMarkers(chart);
    if (!visible.length) return [];

    const clusters = [];
    const thresholdPx = 28;
    visible.forEach(function (marker) {
      const current = clusters[clusters.length - 1];
      if (!current || Math.abs(marker.px - current.lastPx) > thresholdPx) {
        clusters.push({
          markers: [marker],
          lastPx: marker.px,
        });
        return;
      }
      current.markers.push(marker);
      current.lastPx = marker.px;
    });

    return clusters.map(function (cluster) {
      const sumPx = cluster.markers.reduce(function (sum, marker) { return sum + marker.px; }, 0);
      return {
        markers: cluster.markers,
        px: sumPx / cluster.markers.length,
      };
    });
  }

  function markerClusterText(cluster) {
    const labels = cluster.markers.map(function (marker) { return marker.label; });
    const unique = Array.from(new Set(labels));
    return unique.join(', ');
  }

  function markerHoverText(chart) {
    if (!showMarkers || chart._crosshairX == null) return '';
    const clusters = clusterMarkers(chart);
    if (!clusters.length) return '';

    let closest = null;
    let closestDist = Infinity;
    clusters.forEach(function (cluster) {
      const dist = Math.abs(cluster.px - chart._crosshairX);
      if (dist < closestDist) {
        closestDist = dist;
        closest = cluster;
      }
    });
    if (!closest || closestDist > 24) return '';

    const prefix = closest.markers.length === 1 ? 'Command' : (closest.markers.length + ' commands');
    return prefix + ': ' + markerClusterText(closest);
  }

  // ── Build-change markers ──
  // Some clients report the build id JSON-quoted ("v2.0.7l"); show it bare.
  function buildName(s) { return String(s == null ? '' : s).replace(/^"+|"+$/g, ''); }
  function getVisibleBuilds(chart) {
    if (!showBuilds || !chart._buildMarkers || !chart._buildMarkers.length || !chart.scales || !chart.scales.x) return [];
    const xScale = chart.scales.x;
    return chart._buildMarkers
      .filter(function (b) { return b.x >= xScale.min && b.x <= xScale.max; })
      .map(function (b) { return { x: b.x, px: xScale.getPixelForValue(b.x), from: b.from, to: b.to }; });
  }
  function buildHoverText(chart) {
    if (chart._crosshairX == null) return '';
    const vis = getVisibleBuilds(chart);
    let closest = null, dist = Infinity;
    vis.forEach(function (b) { var d = Math.abs(b.px - chart._crosshairX); if (d < dist) { dist = d; closest = b; } });
    if (!closest || dist > 24) return '';
    return 'Build ' + buildName(closest.from) + ' → ' + buildName(closest.to);
  }
  function overlayHoverText(chart) {
    return [markerHoverText(chart), buildHoverText(chart)].filter(Boolean).join('  ·  ');
  }
  const BUILD_COLOR = 'rgba(168,85,247,0.9)', BUILD_LINE = 'rgba(168,85,247,0.45)';
  const buildMarkerPlugin = {
    id: 'buildMarkers',
    afterDraw(chart) {
      const vis = getVisibleBuilds(chart);
      if (!vis.length || !chart.chartArea) return;
      const ca = chart.chartArea, ctx = chart.ctx;
      ctx.save();
      ctx.font = '600 10.5px system-ui, sans-serif';
      ctx.textAlign = 'left';
      ctx.textBaseline = 'middle';
      let lastRight = -Infinity, row = 0;
      vis.forEach(function (b) {
        var x = b.px;
        ctx.setLineDash([]);
        ctx.strokeStyle = BUILD_LINE; ctx.lineWidth = 1.5;
        ctx.beginPath(); ctx.moveTo(x, ca.top); ctx.lineTo(x, ca.bottom); ctx.stroke();
        // small triangle flag at the bottom
        ctx.fillStyle = BUILD_COLOR;
        ctx.beginPath(); ctx.moveTo(x, ca.bottom - 7); ctx.lineTo(x - 4, ca.bottom); ctx.lineTo(x + 4, ca.bottom); ctx.closePath(); ctx.fill();
        // build pill; stagger rows when neighbours overlap
        var text = '→ ' + buildName(b.to);
        var w = ctx.measureText(text).width + 12, h = 16;
        var bx = Math.min(x + 6, ca.right - w);
        if (bx < lastRight + 4) { row = (row + 1) % 2; } else { row = 0; }
        lastRight = bx + w;
        var by = ca.bottom - 12 - h - (row * (h + 3));
        ctx.fillStyle = 'rgba(8,11,16,0.92)';
        ctx.strokeStyle = BUILD_LINE; ctx.lineWidth = 1;
        ctx.beginPath(); ctx.roundRect ? ctx.roundRect(bx, by, w, h, 5) : ctx.rect(bx, by, w, h); ctx.fill(); ctx.stroke();
        ctx.fillStyle = '#e9d5ff';
        ctx.fillText(text, bx + 6, by + h / 2);
      });
      ctx.restore();
    },
  };

  // The stacked charts, top to bottom. They share one time axis, so a pointer on any
  // of them is one moment on all of them.
  const VT_COLOR = { battery: '#f9674e', temp: '#e8a13a', ram: '#7a80f6' };
  function vtStack() { return [batteryChart, tempChart, ramChart].filter(Boolean); }
  // Same y-axis width on every chart, so their plots (and the pointer line) line up.
  function vtYFit(scale) { scale.width = 46; }
  function vtCss(name, fb) { return (getComputedStyle(document.documentElement).getPropertyValue(name) || '').trim() || fb; }
  function vtNearestIdx(data, ms) {
    let best = -1, bd = Infinity;
    for (let i = 0; i < data.length; i++) {
      const p = data[i]; if (!p || p.y == null) continue;
      const d = Math.abs(p.x - ms); if (d < bd) { bd = d; best = i; }
    }
    return best;
  }
  function vtNearest(data, ms) { const i = vtNearestIdx(data, ms); return i < 0 ? null : data[i]; }
  function vtRound(ctx, x, y, w, h, r) {
    ctx.beginPath();
    if (ctx.roundRect) ctx.roundRect(x, y, w, h, r); else ctx.rect(x, y, w, h);
  }
  // The value bubble (static/device-graph-hover-demos.html, V2): a dot where the line is
  // read and the value beside the pointer in the line's colour; the bottom chart also
  // carries the time on its axis.
  function vtBubble(chart) {
    const ca = chart.chartArea, xs = chart.scales.x, ys = chart.scales.y;
    if (!ca || !xs || !ys) return;
    const px = chart._crosshairX, ms = xs.getValueForPixel(px);
    // Read the raw check-ins, not the drawn line: the battery line is drawn from the
    // points where the value changed, which can be hours apart.
    const data = chart._accent === 'battery' ? (chart.$wlcPts || []) : ((chart.data.datasets[0] && chart.data.datasets[0].data) || []);
    const idx = vtNearestIdx(data, ms), pt = idx < 0 ? null : data[idx];
    const ctx = chart.ctx, col = VT_COLOR[chart._accent] || '#7a80f6';
    ctx.save();
    // Shown wherever there is a reading nearby; hidden in an offline gap.
    if (pt && Math.abs(pt.x - ms) <= (xs.max - xs.min) * 0.04) {
      let txt;
      if (chart._accent === 'battery') {
        txt = Math.round(pt.y) + '%';
        // Not every check-in carries the pad state: use the last one reported.
        let w = null;
        for (let k = idx; k >= 0 && k > idx - 500; k--) { const v = data[k] && data[k].wlc; if (v !== undefined && v !== null) { w = v; break; } }
        if (w === 1) txt += ' · phone on pad';
        else if (w === 0) txt += ' · pad vacant';
        else if (w === 2) txt += ' · pad misplaced';
      } else if (chart._accent === 'temp') txt = pt.y.toFixed(1) + '°C';
      else txt = Math.round(pt.y) + '%';
      const py = ys.getPixelForValue(pt.y);
      ctx.beginPath(); ctx.arc(px, py, 4, 0, Math.PI * 2);
      ctx.fillStyle = col; ctx.fill();
      ctx.lineWidth = 2; ctx.strokeStyle = vtCss('--surface', '#fff'); ctx.stroke();
      ctx.font = '700 11.5px ' + (Chart.defaults.font.family || 'system-ui, sans-serif');
      ctx.textBaseline = 'middle';
      const w = ctx.measureText(txt).width + 14, h = 20;
      let bx = px + 8; if (bx + w > ca.right) bx = px - 8 - w;
      const by = ca.top + 2;
      vtRound(ctx, bx, by, w, h, 7);
      ctx.fillStyle = vtCss('--bg', '#fff'); ctx.fill();
      ctx.lineWidth = 1.2; ctx.strokeStyle = col; ctx.stroke();
      ctx.fillStyle = vtCss('--text', '#111'); ctx.fillText(txt, bx + 7, by + h / 2 + 0.5);
    }
    // The time of the pointer, on the chart's time axis.
    {
      const span = xs.max - xs.min;
      const t = span > 36 * 3600e3 ? fmtDate(ms) : fmtTime(ms);
      ctx.font = '700 10.5px ' + (Chart.defaults.font.family || 'system-ui, sans-serif');
      ctx.textBaseline = 'middle';
      const w = ctx.measureText(t).width + 12, h = 17;
      const bx = Math.max(ca.left, Math.min(ca.right - w, px - w / 2)), by = ca.bottom + 5;
      vtRound(ctx, bx, by, w, h, 5);
      ctx.fillStyle = vtCss('--text', '#111'); ctx.fill();
      ctx.fillStyle = vtCss('--bg', '#fff'); ctx.fillText(t, bx + 6, by + h / 2 + 0.5);
    }
    ctx.restore();
  }

  // Crosshair plugin — the pointer line, drawn on every stacked chart at the same moment.
  const crosshairPlugin = {
    id: 'crosshair',
    afterEvent(chart, args) {
      const evt = args.event;
      if (evt.type === 'mousemove' && chart.chartArea) {
        const x = evt.x;
        const ca = chart.chartArea;
        chart._crosshairX = (x >= ca.left && x <= ca.right) ? x : null;
      } else if (evt.type === 'mouseout') {
        chart._crosshairX = null;
      } else return;
      args.changed = true;
      if (chart._isUnified) return;
      const v = chart._crosshairX == null ? null : chart.scales.x.getValueForPixel(chart._crosshairX);
      vtStack().forEach(function (c) {
        if (c === chart || !c.scales || !c.scales.x) return;
        c._crosshairX = v == null ? null : c.scales.x.getPixelForValue(v);
        c.draw();
      });
    },
    afterDraw(chart) {
      if (chart._crosshairX == null) return;
      const ctx = chart.ctx;
      const ca = chart.chartArea;
      ctx.save();
      ctx.beginPath();
      ctx.setLineDash([3, 3]);
      ctx.moveTo(chart._crosshairX, ca.top);
      ctx.lineTo(chart._crosshairX, ca.bottom);
      ctx.lineWidth = 1;
      ctx.strokeStyle = 'rgba(122,127,154,0.6)';
      ctx.stroke();
      ctx.restore();
      if (!chart._isUnified) vtBubble(chart);
    },
  };

  // Find nearest data point by x pixel position
  function nearestByPixel(chart, datasetIndex) {
    if (chart._crosshairX == null) return null;
    const idx = datasetIndex || 0;
    const data = chart.data.datasets[idx] ? chart.data.datasets[idx].data : [];
    if (!data.length) return null;
    const xScale = chart.scales.x;
    const cursorVal = xScale.getValueForPixel(chart._crosshairX);
    let best = null, bestDist = Infinity;
    for (let i = 0; i < data.length; i++) {
      const d = Math.abs(data[i].x - cursorVal);
      if (d < bestDist) { bestDist = d; best = data[i]; }
    }
    return best;
  }

  const staticLabelPlugin = {
    id: 'staticLabel',
    afterEvent(chart) {
      if (chart._crosshairX == null) { updateFooter(null); return; }
      const xScale = chart.scales.x;
      const ms = xScale.getValueForPixel(chart._crosshairX);
      updateFooter(ms, overlayHoverText(chart));
    },
  };

  const commandMarkerPlugin = {
    id: 'commandMarkers',
    afterDraw(chart) {
      if (!showMarkers) return;
      if (!chart._commandMarkers || !chart._commandMarkers.length) return;
      const ca = chart.chartArea;
      const visible = getVisibleMarkers(chart);
      if (!visible.length) return;
      const clusters = clusterMarkers(chart);

      const ctx = chart.ctx;
      ctx.save();
      ctx.setLineDash([4, 4]);
      visible.forEach(function (marker) {
        var x = marker.px;
        ctx.strokeStyle = 'rgba(255,255,255,0.22)';
        ctx.beginPath();
        ctx.moveTo(x, ca.top + 22);
        ctx.lineTo(x, ca.bottom);
        ctx.stroke();
      });

      ctx.setLineDash([]);
      ctx.font = '11px system-ui, sans-serif';
      ctx.textAlign = 'center';
      ctx.textBaseline = 'middle';
      clusters.forEach(function (cluster, idx) {
        var count = String(cluster.markers.length);
        var badgeWidth = Math.max(18, ctx.measureText(count).width + 10);
        var badgeHeight = 14;
        var x = Math.max(ca.left + (badgeWidth / 2), Math.min(ca.right - (badgeWidth / 2), cluster.px));
        var y = ca.top + 5 + ((idx % 2) * 16);
        var boxX = x - (badgeWidth / 2);

        ctx.fillStyle = 'rgba(8,11,16,0.92)';
        ctx.strokeStyle = cluster.markers.length > 1 ? 'rgba(91,110,245,0.55)' : 'rgba(255,255,255,0.18)';
        ctx.fillRect(boxX, y, badgeWidth, badgeHeight);
        ctx.strokeRect(boxX, y, badgeWidth, badgeHeight);
        ctx.fillStyle = cluster.markers.length > 1 ? '#9eb0ff' : '#dfe6e9';
        ctx.fillText(count, x, y + (badgeHeight / 2));
      });
      ctx.restore();
    },
  };

  // Reference bands shade the "bad" zone of each metric so a crossing reads at a
  // glance (low battery, hot temp, high RAM). Drawn under the line.
  const TH = { battLow: 20, tempWarn: 40, tempHot: 45, ramHigh: 85 };
  const BAND_LOW = 'rgba(224,86,86,0.06)', BAND_WARN = 'rgba(232,161,58,0.07)', BAND_HOT = 'rgba(224,86,86,0.09)';
  function bandsFor(chart) {
    if (chart._isUnified) return [
      { from: 0, to: TH.battLow, color: BAND_LOW, axis: 'yPct' },
      { from: TH.tempHot, to: null, color: BAND_HOT, axis: 'yTemp' },
    ];
    if (chart._accent === 'battery') return [{ from: 0, to: TH.battLow, color: BAND_LOW, axis: 'y' }];
    if (chart._accent === 'temp') return [
      { from: TH.tempWarn, to: TH.tempHot, color: BAND_WARN, axis: 'y' },
      { from: TH.tempHot, to: null, color: BAND_HOT, axis: 'y' },
    ];
    if (chart._accent === 'ram') return [{ from: TH.ramHigh, to: 100, color: BAND_HOT, axis: 'y' }];
    return [];
  }
  const bandsPlugin = {
    id: 'bands',
    beforeDatasetsDraw(chart) {
      const ca = chart.chartArea; if (!ca) return;
      const ctx = chart.ctx, scales = chart.scales;
      ctx.save();
      const bands = bandsFor(chart);
      if (!bands || !bands.length) { ctx.restore(); return; }
      bands.forEach(function (b) {
        const ys = scales[b.axis || 'y']; if (!ys) return;
        const yTo = b.to != null ? ys.getPixelForValue(b.to) : ca.top;
        const yFrom = b.from != null ? ys.getPixelForValue(b.from) : ca.bottom;
        const top = Math.max(ca.top, Math.min(yTo, yFrom));
        const bot = Math.min(ca.bottom, Math.max(yTo, yFrom));
        if (bot <= top) return;
        ctx.fillStyle = b.color;
        ctx.fillRect(ca.left, top, ca.right - ca.left, bot - top);
      });
      ctx.restore();
    },
  };
  // Offline spans (1 Oct; static/offline-queue-plan.html, option C). Grey where the device
  // was out of contact but kept readings and sent them later; hatched where there are no
  // readings at all (a gap in the line: an older client, or its queue was full). Late
  // stretches of every line are dashed, set here so no dataset definition has to know.
  const LATE_SEGMENT = { borderDash: function (c) { return (inLate(c.p0.parsed.x) && inLate(c.p1.parsed.x)) ? [5, 4] : undefined; } };
  let offlineHatch = null;
  const offlinePlugin = {
    id: 'offline',
    beforeUpdate(chart) {
      (chart.data.datasets || []).forEach(function (ds) { if (!ds.segment) ds.segment = LATE_SEGMENT; });
    },
    beforeDatasetsDraw(chart) {
      const ca = chart.chartArea, xs = chart.scales.x;
      if (!ca || !xs) return;
      const ctx = chart.ctx;
      const clip = function (a, b) { return [Math.max(ca.left, xs.getPixelForValue(a)), Math.min(ca.right, xs.getPixelForValue(b))]; };
      const muted = (getComputedStyle(document.documentElement).getPropertyValue('--muted') || '').trim() || '#888';
      const label = function (text, x1, x2) {
        ctx.font = '600 10.5px system-ui, sans-serif';
        if (x2 - x1 < ctx.measureText(text).width + 12) return; // only where it fits
        ctx.fillStyle = muted;
        ctx.fillText(text, x1 + 6, ca.top + 13);
      };
      ctx.save();
      // Grey: late spans.
      lateSpans.forEach(function (s) {
        const p = clip(s.from, s.to);
        if (p[1] - p[0] < 1) return;
        ctx.fillStyle = 'rgba(128,128,128,0.13)';
        ctx.fillRect(p[0], ca.top, p[1] - p[0], ca.bottom - ca.top);
        label('Sent later', p[0], p[1]);
      });
      // Hatch: breaks in the first line that no late span covers.
      const ds = chart.data.datasets && chart.data.datasets[0];
      const data = (ds && ds.data) || [];
      if (!offlineHatch) {
        const pc = document.createElement('canvas'); pc.width = 8; pc.height = 8;
        const g = pc.getContext('2d');
        g.strokeStyle = 'rgba(128,128,128,0.35)'; g.lineWidth = 2;
        g.beginPath(); g.moveTo(0, 8); g.lineTo(8, 0); g.moveTo(-2, 2); g.lineTo(2, -2); g.moveTo(6, 10); g.lineTo(10, 6); g.stroke();
        offlineHatch = ctx.createPattern(pc, 'repeat');
      }
      for (let i = 0; i < data.length; i++) {
        if (!data[i] || data[i].y !== null) continue;
        const prev = i > 0 ? data[i - 1] : null, next = i + 1 < data.length ? data[i + 1] : null;
        const a = prev && prev.y !== null ? prev.x : data[i].x;
        const b = next && next.y !== null ? next.x : (xs.max || data[i].x);
        if (inLate(a) && inLate(b)) continue;
        const p = clip(a, b);
        if (p[1] - p[0] < 1) continue;
        ctx.fillStyle = offlineHatch;
        ctx.fillRect(p[0], ca.top, p[1] - p[0], ca.bottom - ca.top);
        label('No data', p[0], p[1]);
      }
      ctx.restore();
    },
  };
  // Draws the blue drag-select rectangle while a zoom drag is in progress.
  const selectionPlugin = {
    id: 'selection',
    afterDraw(chart) {
      const r = chart._selRect; if (!r || !chart.chartArea) return;
      const ca = chart.chartArea, ctx = chart.ctx;
      const x0 = Math.min(r.x0, r.x1), w = Math.abs(r.x1 - r.x0);
      ctx.save();
      ctx.fillStyle = 'rgba(91,110,245,0.12)';
      ctx.strokeStyle = 'rgba(91,110,245,0.55)';
      ctx.lineWidth = 1;
      ctx.fillRect(x0, ca.top, w, ca.bottom - ca.top);
      ctx.strokeRect(x0, ca.top, w, ca.bottom - ca.top);
      ctx.restore();
    },
  };

  // Battery chart — only built when the device has a battery; kiosks omit the
  // #batteryChart canvas entirely, so creating it here would throw on null.
  if (DV.showBattery) {
  batteryChart = new Chart(document.getElementById("batteryChart").getContext("2d"), {
    type: "line",
    data: {
      datasets: [{
        label: "Battery %",
        data: [],
        tension: 0.55,
        cubicInterpolationMode: "default",
        fill: true,
        pointRadius: 0,
        pointHoverRadius: 0,
        borderWidth: 2.5,
        // Stroke is painted by an x-axis gradient keyed to charging-pad state, so the
        // line stays smooth (no extra vertices) while still showing placed/vacant spans.
        borderColor: ctx => buildBatteryGradient(ctx.chart),
        borderCapStyle: "round",
        borderJoinStyle: "round",
        spanGaps: false,
        backgroundColor: (ctx) => {
          const chart = ctx.chart;
          const {ctx: c, chartArea} = chart;
          if (!chartArea) return "transparent";
          const gradient = c.createLinearGradient(0, chartArea.top, 0, chartArea.bottom);
          gradient.addColorStop(0, "rgba(249,103,78,0.28)");
          gradient.addColorStop(1, "rgba(249,103,78,0)");
          return gradient;
        },
      }],
    },
    options: {
      animation: false, responsive: true, maintainAspectRatio: false, clip: false,
      events: ['mousemove', 'mouseout'],
      interaction: { mode: "nearest", intersect: false, axis: "x" },
      plugins: {
        legend: { display: false },
        tooltip: { enabled: false },
      },
      scales: {
        x: DV.hasCharging ? chargeStripXScale() : sharedXScale(),
        y: { min: 0, max: 100, afterFit: vtYFit, ticks: { color: TICK, callback: v => v + "%", stepSize: 50, padding: 6 }, grid: { color: GRID }, border: { display: false } },
      },
    },
    plugins: [offlinePlugin, bandsPlugin, crosshairPlugin, staticLabelPlugin, commandMarkerPlugin, buildMarkerPlugin, selectionPlugin].concat(DV.hasCharging ? [chargeStripPlugin] : []),
  });
  batteryChart._labelFn = pt => {
    const line = 'Battery: ' + pt.y + '%';
    if (pt.wlc === 1) return line + ' ⚡ Wireless charging in use';
    if (pt.wlc === 0) return line + ' — Charging pad vacant';
    if (pt.wlc === 2) return line + ' ⚠ Charging pad misplaced — please place it again';
    if (pt.wlc !== undefined && pt.wlc !== null) return line + ' ⚠ Pad unreadable';
    return line;
  };
  batteryChart._commandMarkers = commandMarkers;
  batteryChart._buildMarkers = buildMarkers;
  batteryChart._accent = 'battery';
  }

  // Temperature chart
  tempChart = new Chart(document.getElementById("tempChart").getContext("2d"), {
    type: "line",
    data: {
      datasets: [{
        label: "Temperature",
        data: [],
        tension: 0.4,
        fill: true,
        borderColor: "#e8a13a",
        pointBackgroundColor: "#e8a13a",
        pointRadius: 0,
        pointHoverRadius: 0,
        borderWidth: 2.5,
        borderCapStyle: "round",
        borderJoinStyle: "round",
        spanGaps: false,
        backgroundColor: (ctx) => {
          const chart = ctx.chart;
          const {ctx: c, chartArea} = chart;
          if (!chartArea) return "transparent";
          const gradient = c.createLinearGradient(0, chartArea.top, 0, chartArea.bottom);
          gradient.addColorStop(0, "rgba(232,161,58,0.28)");
          gradient.addColorStop(1, "rgba(232,161,58,0)");
          return gradient;
        },
      }],
    },
    options: {
      animation: false, responsive: true, maintainAspectRatio: false, clip: false,
      events: ['mousemove', 'mouseout'],
      interaction: { mode: "nearest", intersect: false, axis: "x" },
      plugins: {
        legend: { display: false },
        tooltip: { enabled: false },
      },
      scales: {
        x: sharedXScale(),
        y: { afterFit: vtYFit, ticks: { color: TICK, callback: v => v + "°C", padding: 6, maxTicksLimit: 4 }, grid: { color: GRID }, border: { display: false } },
      },
    },
    plugins: [offlinePlugin, bandsPlugin, crosshairPlugin, staticLabelPlugin, commandMarkerPlugin, buildMarkerPlugin, selectionPlugin],
  });
  tempChart._labelFn = pt => 'Temp: ' + pt.y.toFixed(1) + '°C';
  tempChart._commandMarkers = commandMarkers;
  tempChart._buildMarkers = buildMarkers;
  tempChart._accent = 'temp';

  // RAM chart
  ramChart = new Chart(document.getElementById("ramChart").getContext("2d"), {
    type: "line",
    data: {
      datasets: [{
        label: "RAM %",
        data: [],
        tension: 0.4,
        fill: true,
        borderColor: "#7a80f6",
        pointBackgroundColor: "#7a80f6",
        pointRadius: 0,
        pointHoverRadius: 0,
        borderWidth: 2.5,
        borderCapStyle: "round",
        borderJoinStyle: "round",
        spanGaps: false,
        backgroundColor: (ctx) => {
          const chart = ctx.chart;
          const {ctx: c, chartArea} = chart;
          if (!chartArea) return "transparent";
          const gradient = c.createLinearGradient(0, chartArea.top, 0, chartArea.bottom);
          gradient.addColorStop(0, "rgba(122,128,246,0.28)");
          gradient.addColorStop(1, "rgba(122,128,246,0)");
          return gradient;
        },
      }],
    },
    options: {
      animation: false, responsive: true, maintainAspectRatio: false, clip: false,
      events: ['mousemove', 'mouseout'],
      interaction: { mode: "nearest", intersect: false, axis: "x" },
      plugins: {
        legend: { display: false },
        tooltip: { enabled: false },
      },
      scales: {
        x: sharedXScale(),
        y: { min: 0, max: 100, afterFit: vtYFit, ticks: { color: TICK, callback: v => v + "%", stepSize: 50, padding: 6 }, grid: { color: GRID }, border: { display: false } },
      },
    },
    plugins: [offlinePlugin, bandsPlugin, crosshairPlugin, staticLabelPlugin, commandMarkerPlugin, buildMarkerPlugin, selectionPlugin],
  });
  ramChart._labelFn = pt => 'RAM: ' + pt.y.toFixed(1) + '%';
  ramChart._commandMarkers = commandMarkers;
  ramChart._buildMarkers = buildMarkers;
  ramChart._accent = 'ram';

  // ── Unified chart ────────────────────────────────────────────────────────
  // Reuse the same linear ms-timestamp x-axis the individual charts use
  const unifiedXCfg = {
    type: 'linear',
    afterBuildTicks: evenTimeTicks,
    grid: { color: GRID, drawBorder: false },
    ticks: {
      color: '#7a7f9a', font: { size: 11 }, autoSkip: false,
      callback: v => fmtDate(v),
    },
    border: { display: false },
  };

  unifiedChart = new Chart(document.getElementById('unifiedChart').getContext('2d'), {
    type: 'line',
    data: {
      datasets: [
        {
          label: 'Battery %', data: [], yAxisID: 'yPct',
          borderColor: '#f9674e', backgroundColor: 'rgba(249,103,78,0.10)',
          borderWidth: 2, pointRadius: 0, spanGaps: false,
          tension: 0.55,
          borderCapStyle: 'round', borderJoinStyle: 'round', fill: true,
        },
        {
          label: 'Temp °C', data: [], yAxisID: 'yTemp',
          borderColor: '#e8a13a', backgroundColor: 'transparent',
          borderWidth: 2, pointRadius: 0, spanGaps: false,
          tension: 0.4,
          borderCapStyle: 'round', borderJoinStyle: 'round',
        },
        {
          label: 'RAM %', data: [], yAxisID: 'yPct',
          borderColor: '#7a80f6', backgroundColor: 'transparent',
          borderWidth: 2, pointRadius: 0, spanGaps: false,
          tension: 0.4,
          borderCapStyle: 'round', borderJoinStyle: 'round',
        },
      ],
    },
    options: {
      animation: false, responsive: true, maintainAspectRatio: false, clip: false,
      interaction: { mode: 'index', intersect: false },
      plugins: {
        legend: { display: false },
        tooltip: { enabled: false },
        decimation: { enabled: true, algorithm: 'lttb', samples: 250 },
      },
      scales: {
        x: unifiedXCfg,
        yPct: {
          position: 'left', min: 0, max: 100,
          grid: { color: GRID, drawBorder: false },
          ticks: { color: '#7a7f9a', font: { size: 11 }, callback: v => v + '%', maxTicksLimit: 5 },
          border: { display: false },
        },
        yTemp: {
          position: 'right',
          grid: { drawOnChartArea: false },
          ticks: { color: '#ff9800', font: { size: 11 }, callback: v => v + '°', maxTicksLimit: 5 },
          border: { display: false },
        },
      },
    },
    plugins: [offlinePlugin, bandsPlugin, crosshairPlugin, staticLabelPlugin, commandMarkerPlugin, buildMarkerPlugin, selectionPlugin],
  });
  unifiedChart._isUnified = true;
  unifiedChart._commandMarkers = commandMarkers;
  unifiedChart._buildMarkers = buildMarkers;
  unifiedChart._accent = 'battery';

  function redrawAllCharts() {
    if (batteryChart) batteryChart.update();
    tempChart.update();
    ramChart.update();
    unifiedChart.update();
  }
  var _markBtn = document.getElementById("toggle-markers-btn");
  if (_markBtn) _markBtn.addEventListener("click", function () {
    showMarkers = !showMarkers;
    try { localStorage.setItem(markerStorageKey, showMarkers ? 'visible' : 'hidden'); } catch (e) {}
    syncMarkerToggle();
    redrawAllCharts();
  });
  var _buildBtn = document.getElementById("toggle-builds-btn");
  if (_buildBtn) _buildBtn.addEventListener("click", function () {
    showBuilds = !showBuilds;
    try { localStorage.setItem(buildStorageKey, showBuilds ? 'visible' : 'hidden'); } catch (e) {}
    syncMarkerToggle();
    redrawAllCharts();
  });
  syncMarkerToggle();

  document.querySelectorAll(".dur-btn[data-ms]").forEach(btn => {
    btn.addEventListener("click", () => {
      customMin = null;
      customMax = null;
      closeCustomPop();
      showRangeError("");
      document.querySelectorAll(".dur-btn").forEach(b => b.classList.remove("active"));
      btn.classList.add("active");
      currentDuration = +btn.dataset.ms;
      // Back to the rolling window: the live arrays are what SSE appends to, so the
      // chart keeps updating in place.
      window.mdmClearViewData();
      // The page only bakes in the default 6h window; 12h/24h/48h pull their extra
      // history from /chart-data on click (loadOlderWithSpinner shows the same
      // "Loading graphs…" overlay used on first paint while that's in flight, so a
      // wider window never just renders empty).
      loadOlderWithSpinner(Date.now() - currentDuration, updateCharts);
    });
  });

  function closeCustomPop() {
    var inputs = document.getElementById("custom-range-inputs");
    if (inputs) inputs.setAttribute("hidden", "");
  }
  function openCustomPop() {
    var inputs = document.getElementById("custom-range-inputs");
    if (!inputs) return;
    showRangeError("");
    // Pre-fill with the currently shown window so the inputs start from context.
    var hasCustom = customMin !== null && customMax !== null;
    var endMs = hasCustom ? customMax : Date.now();
    var startMs = hasCustom ? customMin : (endMs - currentDuration);
    document.getElementById("range-start").value = toLocalISO(new Date(startMs));
    document.getElementById("range-end").value = toLocalISO(new Date(endMs));
    if (window.mdmCalSync) window.mdmCalSync();
    inputs.removeAttribute("hidden");
  }
  document.getElementById("custom-range-btn").addEventListener("click", function (e) {
    e.stopPropagation();
    var inputs = document.getElementById("custom-range-inputs");
    if (inputs.hasAttribute("hidden")) openCustomPop(); else closeCustomPop();
  });
  document.getElementById("cr-close").addEventListener("click", closeCustomPop);
  // Click outside the popover (and not on the Custom button) closes it.
  document.addEventListener("click", function (e) {
    var inputs = document.getElementById("custom-range-inputs");
    if (!inputs || inputs.hasAttribute("hidden")) return;
    if (inputs.contains(e.target) || e.target.closest("#custom-range-btn")) return;
    closeCustomPop();
  });
  // Auto-apply as the From/To fields change — no separate Apply button.
  document.getElementById("range-start").addEventListener("change", applyCustomRange);
  document.getElementById("range-end").addEventListener("change", applyCustomRange);

  function showRangeError(msg) {
    var el = document.getElementById("range-error");
    if (!el) return;
    if (msg) { el.textContent = msg; el.hidden = false; }
    else { el.textContent = ""; el.hidden = true; }
  }

  // Validate + apply the two datetime inputs. Empty → error; reversed → auto-swap.
  function applyCustomRange() {
    var s = document.getElementById("range-start").value;
    var e = document.getElementById("range-end").value;
    if (!s || !e) { showRangeError("Pick both a start and end time."); return false; }
    var a = new Date(s).getTime(), b = new Date(e).getTime();
    if (a === b) { showRangeError("Start and end can't be the same time."); return false; }
    if (a > b) { // reversed — swap silently and reflect it back into the inputs
      var t = a; a = b; b = t;
      document.getElementById("range-start").value = toLocalISO(new Date(a));
      document.getElementById("range-end").value = toLocalISO(new Date(b));
    }
    showRangeError("");
    customMin = a; customMax = b;
    document.querySelectorAll(".dur-btn").forEach(b => b.classList.remove("active"));
    document.getElementById("custom-range-btn").classList.add("active");
    // Fetch this window, not everything back to it: the server thins to ~2500 points
    // per request, so a month-wide request drawn as one day arrives nearly empty.
    fetchWindow(customMin, customMax, updateCharts);
    return true;
  }

  function presetRange(preset) {
    var now = new Date(), start = new Date(now), end = now;
    if (preset === "1h") { start = new Date(now.getTime() - 3600000); }
    else if (preset === "6h") { start = new Date(now.getTime() - 6 * 3600000); }
    else if (preset === "24h") { start = new Date(now.getTime() - 24 * 3600000); }
    else if (preset === "today") { start.setHours(0, 0, 0, 0); }
    else if (preset === "yesterday") {
      start.setDate(start.getDate() - 1); start.setHours(0, 0, 0, 0);
      end = new Date(start); end.setHours(23, 59, 59, 999);
    } else if (preset === "7d") { start = new Date(now.getTime() - 7 * 86400000); }
    else if (preset === "30d") { start = new Date(now.getTime() - 30 * 86400000); }
    document.getElementById("range-start").value = toLocalISO(start);
    document.getElementById("range-end").value = toLocalISO(new Date(end));
    if (window.mdmCalSync) window.mdmCalSync();
    if (applyCustomRange()) closeCustomPop();
  }

  // Slide the active custom window left/right by its own width.
  function nudgeRange(dir) {
    if (customMin === null || customMax === null) return;
    var span = customMax - customMin;
    customMin += dir * span; customMax += dir * span;
    document.getElementById("range-start").value = toLocalISO(new Date(customMin));
    document.getElementById("range-end").value = toLocalISO(new Date(customMax));
    loadOlderWithSpinner(customMin, updateCharts);
  }

  function resetCustomRange() {
    customMin = null; customMax = null;
    showRangeError("");
    closeCustomPop();
    document.querySelectorAll(".dur-btn").forEach(b => b.classList.remove("active"));
    var def = document.querySelector('.dur-btn[data-ms="21600000"]');
    if (def) def.classList.add("active");
    currentDuration = 6 * 3600 * 1000;
    // Drop any ?focus=/?window= deep-link params so the custom window can't be
    // re-applied by applyFocusFromURL on a reload or back/forward navigation.
    try {
      var u = new URL(location.href);
      if (u.searchParams.has("focus") || u.searchParams.has("window")) {
        u.searchParams.delete("focus"); u.searchParams.delete("window");
        history.replaceState(null, "", u);
      }
    } catch (e) {}
    updateCharts();
  }

  document.getElementById("reset-range-btn").addEventListener("click", resetCustomRange);
  document.getElementById("range-prev").addEventListener("click", () => nudgeRange(-1));
  document.getElementById("range-next").addEventListener("click", () => nudgeRange(1));
  // ── Range calendar ──────────────────────────────────────────────────────────
  // One grid picks both ends: first click is the start, second the end (reversed
  // clicks swap). It writes the same two values the hidden inputs always held, so the
  // presets, the shift arrows and applyCustomRange are untouched — a start covers from
  // 00:00 and an end covers to 23:59:59.999, because the picker chooses days.
  var calView = new Date();            // month on screen
  var calStart = null, calEnd = null;  // chosen days, as ms at local midnight
  function dayStart(ms) { var d = new Date(ms); d.setHours(0, 0, 0, 0); return d.getTime(); }
  function dayEnd(ms) { var d = new Date(ms); d.setHours(23, 59, 59, 999); return d.getTime(); }
  function sameDay(a, b) { return a !== null && b !== null && dayStart(a) === dayStart(b); }

  function calRender() {
    var grid = document.getElementById('cal-grid'), title = document.getElementById('cal-title');
    if (!grid || !title) return;
    title.textContent = calView.toLocaleDateString([], { month: 'long', year: 'numeric' });
    var first = new Date(calView.getFullYear(), calView.getMonth(), 1);
    // Monday-first: getDay() is 0 for Sunday, so Sunday starts the last column.
    var lead = (first.getDay() + 6) % 7;
    var startCell = new Date(first);
    startCell.setDate(1 - lead);
    var todayEnd = dayEnd(Date.now());

    // Build the 42 cells once, then only update them. Replacing innerHTML here would
    // detach the very button being clicked before the document-level "click outside
    // closes this popover" handler runs, so contains(e.target) came back false and the
    // first click on a date shut the picker.
    if (grid.children.length !== 42) {
      var frag = document.createDocumentFragment();
      for (var k = 0; k < 42; k++) {
        var cell = document.createElement('button');
        cell.type = 'button';
        frag.appendChild(cell);
      }
      grid.innerHTML = '';
      grid.appendChild(frag);
    }
    for (var i = 0; i < 42; i++) {
      var d = new Date(startCell);
      d.setDate(startCell.getDate() + i);
      var ms = d.getTime(), cls = 'cr-day';
      if (d.getMonth() !== calView.getMonth()) cls += ' mut';
      if (sameDay(ms, Date.now())) cls += ' today';
      // Outside what this device can have data for: after today, or before it joined.
      if (ms > todayEnd || ms < dayStart(CHART_MIN_X)) cls += ' off';
      if (calStart !== null && calEnd !== null && ms > calStart && ms < calEnd) cls += ' in';
      if (sameDay(ms, calStart)) cls += ' edge start';
      if (sameDay(ms, calEnd)) cls += ' edge end';
      var btn = grid.children[i];
      btn.className = cls;
      btn.dataset.ms = String(ms);
      btn.textContent = String(d.getDate());
    }
    // Stop the month arrow at the device's first month for the same reason.
    var prevBtn = document.getElementById('cal-prev');
    if (prevBtn) {
      var floor = new Date(CHART_MIN_X);
      var atFloor = calView.getFullYear() < floor.getFullYear()
        || (calView.getFullYear() === floor.getFullYear() && calView.getMonth() <= floor.getMonth());
      prevBtn.disabled = atFloor;
      prevBtn.title = atFloor ? 'This device was onboarded ' + floor.toLocaleDateString([], { day: 'numeric', month: 'short', year: 'numeric' }) : 'Previous month';
    }
    var sel = document.getElementById('cal-sel');
    if (sel) {
      var f = function (ms) { return new Date(ms).toLocaleDateString([], { day: 'numeric', month: 'short' }); };
      sel.textContent = calStart === null ? 'Pick a start date'
        : calEnd === null ? f(calStart) + ' — pick an end date'
        : f(calStart) + ' to ' + f(calEnd);
    }
  }

  // Reflect whatever the inputs hold (a preset, the shift arrows, a drag-zoom) back
  // into the calendar, so what is highlighted is always what is on the chart.
  function calSync() {
    var si = document.getElementById('range-start'), ei = document.getElementById('range-end');
    if (!si || !ei) return;
    var a = si.value ? new Date(si.value).getTime() : NaN;
    var b = ei.value ? new Date(ei.value).getTime() : NaN;
    calStart = isNaN(a) ? null : dayStart(a);
    calEnd = isNaN(b) ? null : dayStart(b);
    if (calStart !== null) calView = new Date(calStart);
    calRender();
  }
  window.mdmCalSync = calSync;

  function calCommit() {
    if (calStart === null || calEnd === null) return;
    var si = document.getElementById('range-start'), ei = document.getElementById('range-end');
    si.value = toLocalISO(new Date(calStart));
    ei.value = toLocalISO(new Date(dayEnd(calEnd)));
    applyCustomRange();
  }

  var _calGrid = document.getElementById('cal-grid');
  if (_calGrid) _calGrid.addEventListener('click', function (e) {
    var btn = e.target.closest('.cr-day');
    if (!btn || btn.classList.contains('off')) return;
    var ms = +btn.dataset.ms;
    if (calStart === null || calEnd !== null) {        // start a fresh range
      calStart = ms; calEnd = null;
    } else if (ms < calStart) {                        // clicked before the start: swap
      calEnd = calStart; calStart = ms;
    } else {
      calEnd = ms;
    }
    calRender();
    if (calStart !== null && calEnd !== null) { calCommit(); closeCustomPop(); }
  });
  var _calPrev = document.getElementById('cal-prev'), _calNext = document.getElementById('cal-next');
  if (_calPrev) _calPrev.addEventListener('click', function () { calView.setMonth(calView.getMonth() - 1); calRender(); });
  if (_calNext) _calNext.addEventListener('click', function () { calView.setMonth(calView.getMonth() + 1); calRender(); });
  calRender();

  document.querySelectorAll(".cr-preset").forEach(function (btn) {
    btn.addEventListener("click", function () { presetRange(btn.dataset.preset); });
  });

  // Timezone segment: your zone, the device's, or UTC. Each option re-renders axes,
  // tooltips and the range label; the active one is the current mode. A device that
  // never reported a usable zone has no Device option (server-side), but You/UTC stay
  // — UTC is what the CSV export prints, so it must be reachable everywhere.
  var tzSeg = document.getElementById("tz-seg");
  if (tzSeg) {
    var devBtn = tzSeg.querySelector('.gc-tzb[data-tz="device"]');
    if (devBtn && !window.DEVICE_TZ) devBtn.remove();   // reported zone Intl won't take
    tzSeg.querySelectorAll(".gc-tzb").forEach(function (b) {
      b.addEventListener("click", function () {
        window.TZ_MODE = b.getAttribute("data-tz");
        tzSeg.querySelectorAll(".gc-tzb").forEach(function (x) { x.classList.toggle("active", x === b); });
        updateCharts();
      });
    });
  }

  // ── Direct-manipulation zoom + PNG export ──────────────────────────────────
  function activeChartObj() {
    if (activeTab === 'temp') return tempChart;
    if (activeTab === 'ram') return ramChart;
    if (activeTab === 'all') return unifiedChart;
    return batteryChart;
  }

  // Set a custom window from an interaction (drag / wheel), reusing the same
  // customMin/customMax path as the manual range inputs so the chip + nudge work.
  function applyZoomWindow(a, b) {
    a = Math.round(a); b = Math.round(b);
    if (b - a < 60000) return;          // floor at 1 min so a stray drag can't over-zoom
    var cl = clampToOnboard(a, b); a = cl[0]; b = cl[1];
    customMin = a; customMax = b;
    document.querySelectorAll(".dur-btn").forEach(x => x.classList.remove("active"));
    var cb = document.getElementById("custom-range-btn"); if (cb) cb.classList.add("active");
    var si = document.getElementById("range-start"), ei = document.getElementById("range-end");
    if (si) si.value = toLocalISO(new Date(a));
    if (ei) ei.value = toLocalISO(new Date(b));
    // Fetch the window being drawn, at that window's own resolution. Asking for
    // [a, oldest-loaded] instead — which is what this used to do — returns the same
    // 2500 points spread over everything back to the chosen start, so a single day out
    // of a month-wide answer arrives nearly empty.
    fetchWindow(a, b, updateCharts);
  }
  function windowBounds(chart) {
    if (customMin !== null && customMax !== null) return [customMin, customMax];
    var xs = chart.scales.x; return [xs.min, xs.max];
  }
  // Toolbar +/- zoom: zoom the active chart's window around its centre, mirroring the
  // Ctrl+scroll / trackpad-pinch zoom.
  window.chartZoom = function (dir) {
    var chart = activeChartObj(); if (!chart) return;
    var b = windowBounds(chart); if (!b) return;
    var a = b[0], z = b[1]; if (a == null || z == null) return;
    var pivot = (a + z) / 2, f = dir > 0 ? 0.8 : 1.25; // + zooms in, - zooms out
    applyZoomWindow(pivot - (pivot - a) * f, pivot + (z - pivot) * f);
  };
  // Flash the "Zoom with Ctrl + scroll" overlay when the user scrolls without Ctrl.
  var _zoomHintTimer = null;
  function showZoomHint(msg) {
    var el = document.getElementById('chart-zoom-hint'); if (!el) return;
    el.textContent = msg || 'Zoom with Ctrl + scroll';
    el.classList.add('show');
    clearTimeout(_zoomHintTimer);
    _zoomHintTimer = setTimeout(function () { el.classList.remove('show'); }, 1100);
  }

  // Drag across the plot to zoom to that span, Ctrl+scroll to zoom around the cursor,
  // double-click to reset to the default window.
  function installZoom(chart) {
    if (!chart) return;
    var canvas = chart.canvas, dragging = false;
    var panning = false, panStartX = 0, panFrom = 0, panTo = 0, panPlotW = 1;
    canvas.style.cursor = 'crosshair';
    canvas.addEventListener('mousedown', function (e) {
      if (e.button !== 0) return;
      var ca = chart.chartArea, rect = canvas.getBoundingClientRect();
      var x = e.clientX - rect.left;
      if (x < ca.left || x > ca.right) return;
      if (e.ctrlKey) {
        // Ctrl + drag = pan the current window left/right.
        var b = windowBounds(chart);
        if (b[0] == null || b[1] == null) return;
        panning = true; panStartX = x; panFrom = b[0]; panTo = b[1];
        panPlotW = Math.max(1, ca.right - ca.left);
        canvas.style.cursor = 'grabbing'; e.preventDefault();
        return;
      }
      dragging = true; chart._selRect = { x0: x, x1: x }; e.preventDefault();
    });
    canvas.addEventListener('mousemove', function (e) {
      var ca = chart.chartArea, rect = canvas.getBoundingClientRect();
      if (panning) {
        var span = panTo - panFrom;
        var dv = (e.clientX - rect.left - panStartX) * span / panPlotW; // drag right → earlier
        applyZoomWindow(panFrom - dv, panTo - dv);
        return;
      }
      if (!dragging) return;
      chart._selRect.x1 = Math.max(ca.left, Math.min(ca.right, e.clientX - rect.left));
      chart.draw();
    });
    window.addEventListener('mouseup', function () {
      if (panning) { panning = false; canvas.style.cursor = 'crosshair'; return; }
      if (!dragging) return; dragging = false;
      var r = chart._selRect; chart._selRect = null;
      if (!r) return;
      if (Math.abs(r.x1 - r.x0) < 8) { chart.draw(); return; }  // a click, not a drag
      var xs = chart.scales.x;
      applyZoomWindow(xs.getValueForPixel(Math.min(r.x0, r.x1)), xs.getValueForPixel(Math.max(r.x0, r.x1)));
    });
    canvas.addEventListener('dblclick', function () { resetCustomRange(); });
    canvas.addEventListener('wheel', function (e) {
      var ca = chart.chartArea, rect = canvas.getBoundingClientRect();
      var x = e.clientX - rect.left;
      if (x < ca.left || x > ca.right) return;
      // Plain scroll is left to the page; zoom is Ctrl+scroll — and trackpad pinch,
      // which the browser delivers as ctrl+wheel — so a normal two-finger scroll (no
      // ctrl) still pages as usual and never gets trapped by the chart. Works inline
      // and full screen alike.
      if (!e.ctrlKey) { showZoomHint(); return; }
      e.preventDefault();
      var bounds = windowBounds(chart), a = bounds[0], b = bounds[1];
      if (a == null || b == null) return;
      var pivot = chart.scales.x.getValueForPixel(x);
      var f = e.deltaY < 0 ? 0.8 : 1.25;   // scroll up = zoom in
      applyZoomWindow(pivot - (pivot - a) * f, pivot + (b - pivot) * f);
    }, { passive: false });
  }
  [batteryChart, tempChart, ramChart, unifiedChart].forEach(installZoom);

  // Keep the canvases sharp when the browser page is zoomed. Chart.js sizes each
  // canvas's backing store to devicePixelRatio, but only re-samples on its
  // ResizeObserver — which measures CSS pixels. Page zoom changes the DPR without
  // changing the CSS-pixel size, so the observer never fires: the old-resolution
  // bitmap gets stretched by the browser and looks pixelated next to the crisp DOM.
  // Watch the DPR with a resolution media query (the standard re-arming pattern) and
  // force a resize() on each change so the backing store is re-rendered at the new
  // ratio. resize() re-reads devicePixelRatio in Chart.js v4.
  (function watchDevicePixelRatio() {
    if (!window.matchMedia) return;
    function onChange() {
      [batteryChart, tempChart, ramChart, unifiedChart].forEach(function (c) { if (c) c.resize(); });
      var mq = window.matchMedia('(resolution: ' + window.devicePixelRatio + 'dppx)');
      if (mq.addEventListener) mq.addEventListener('change', onChange, { once: true });
      else if (mq.addListener) mq.addListener(onChange); // Safari < 14 fallback
    }
    onChange();
  })();

  var _resetBtn = document.getElementById('chart-reset-btn');
  if (_resetBtn) _resetBtn.addEventListener('click', function () { resetCustomRange(); });

  // Full-screen graphs: toggle the browser Fullscreen API on the chart card, and
  // resize the charts to fill it (Chart.js watches the container, but nudge to be safe).
  var _fsBtn = document.getElementById('chart-fs-btn');
  var _chartCard = document.querySelector('.chart-card');
  if (_fsBtn && _chartCard) {
    _fsBtn.addEventListener('click', function () {
      if (document.fullscreenElement) {
        if (document.exitFullscreen) document.exitFullscreen();
      } else if (_chartCard.requestFullscreen) {
        _chartCard.requestFullscreen().catch(function () {});
      }
    });
    document.addEventListener('fullscreenchange', function () {
      var fs = document.fullscreenElement === _chartCard;
      _fsBtn.textContent = fs ? '⤡' : '⤢';
    _fsBtn.title = fs ? 'Exit full screen' : 'View graphs full screen';
      // Resize a couple of times: the fullscreen enter/exit transition isn't finished at
      // 80ms, so a single early resize can leave a chart canvas sized for full screen —
      // which then overflows the card on exit and pushes the toolbar's right-side buttons
      // (PNG / reset / full-screen) off the edge. A second pass after the transition settles
      // restores the correct size; .chart-wrap also clips so a transient oversize can't
      // widen the card in the meantime.
      var reflow = function () {
        [batteryChart, tempChart, ramChart, unifiedChart].forEach(function (c) { if (c) { try { c.resize(); } catch (e) {} } });
        if (typeof updateCharts === 'function') updateCharts();
      };
      setTimeout(reflow, 80);
      setTimeout(reflow, 350);
    });
  }

  // Shift the visible window earlier/later by its own width — works on a preset
  // window too (it converts the current view to a custom range, then slides it).
  function shiftWindow(dir) {
    var c = activeChartObj(); if (!c) return;
    var b = windowBounds(c);
    if (b[0] == null || b[1] == null) return;
    var span = b[1] - b[0];
    if (dir < 0 && b[0] <= CHART_MIN_X) return;   // already at the device's first day
    applyZoomWindow(b[0] + dir * span, b[1] + dir * span);
  }
  // Grey the arrow out when there is nothing earlier to show, so the limit is visible
  // rather than something you discover by clicking and nothing happening.
  function syncShiftButtons() {
    var prev = document.getElementById('chart-shift-prev');
    if (!prev) return;
    // Mirror filterWindow's own arithmetic rather than reading chart.scales: on the
    // first render the scale is not populated yet, and its zero start read as "already
    // at the device's first day", which greyed the arrow out on page load.
    var start;
    if (customMin !== null) {
      start = customMin;
    } else {
      var b = srcBattery();
      var lastPt = b.length ? b[b.length - 1].x : Date.now();
      start = lastPt - currentDuration;
    }
    prev.disabled = start <= CHART_MIN_X;
    prev.title = prev.disabled ? 'Nothing before this device was onboarded' : 'Shift window earlier';
  }
  window.mdmSyncShift = syncShiftButtons;
  var _shiftPrev = document.getElementById('chart-shift-prev');
  var _shiftNext = document.getElementById('chart-shift-next');
  if (_shiftPrev) _shiftPrev.addEventListener('click', function () { shiftWindow(-1); });
  if (_shiftNext) _shiftNext.addEventListener('click', function () { shiftWindow(1); });

  var _exportBtn = document.getElementById('chart-export-btn');
  if (_exportBtn) _exportBtn.addEventListener('click', function () {
    var cs = vtStack(); if (!cs.length) return;
    // The whole stack, top to bottom, composited onto a solid theme background — the
    // canvases themselves are transparent, which would export as a see-through PNG.
    var tmp = document.createElement('canvas');
    tmp.width = Math.max.apply(null, cs.map(function (c) { return c.canvas.width; }));
    tmp.height = cs.reduce(function (h, c) { return h + c.canvas.height; }, 0);
    var tctx = tmp.getContext('2d');
    tctx.fillStyle = getComputedStyle(document.body).backgroundColor || '#ffffff';
    tctx.fillRect(0, 0, tmp.width, tmp.height);
    var oy = 0;
    cs.forEach(function (c) { tctx.drawImage(c.canvas, 0, oy); oy += c.canvas.height; });
    var a = document.createElement('a');
    a.href = tmp.toDataURL('image/png');
    a.download = DV.serial + '-vitals-' + new Date().toISOString().slice(0, 10) + '.png';
    document.body.appendChild(a); a.click(); a.remove();
  });

  function toLocalISO(d) {
    var y = d.getFullYear(), m = String(d.getMonth()+1).padStart(2,"0"),
        day = String(d.getDate()).padStart(2,"0"), h = String(d.getHours()).padStart(2,"0"),
        min = String(d.getMinutes()).padStart(2,"0");
    return y+"-"+m+"-"+day+"T"+h+":"+min;
  }

  // When opened via a deep link like ?focus=temp (a heat call-out), zoom the chart to
  // a 1-hour window around that metric's peak and switch to its tab. ?window=<min>
  // overrides the width. Returns the chart tab to show, or null when no focus applies.
  function applyFocusFromURL() {
    var params = new URLSearchParams(location.search);
    var f = params.get("focus");
    if (!f) return null;
    var byMetric = { temp: allTemp, battery: allBattery, ram: allRam };
    var data = byMetric[f];
    if (!data || !data.length) return null;
    var peak = null;
    for (var i = 0; i < data.length; i++) {
      if (data[i].y == null) continue;
      if (peak === null || data[i].y > peak.y) peak = data[i];
    }
    if (!peak) return null;
    var win = parseInt(params.get("window"), 10);
    if (!(win > 0)) win = 60; // minutes
    var half = win * 60 * 1000 / 2;
    customMin = peak.x - half;
    customMax = peak.x + half;
    document.querySelectorAll(".dur-btn").forEach(b => b.classList.remove("active"));
    document.getElementById("custom-range-btn").classList.add("active");
    // Reveal the selector primed with the focused window so it's easy to widen/shift.
    document.getElementById("custom-range-inputs").removeAttribute("hidden");
    document.getElementById("range-start").value = toLocalISO(new Date(customMin));
    document.getElementById("range-end").value = toLocalISO(new Date(customMax));
    return f === "battery" ? "battery" : f === "ram" ? "ram" : "temp";
  }

  // ── Navigator strip (static/device-graph-demos.html, T4) ─────────────────────
  // Under the charts: the last two days (or more, around an older window) as one thin
  // line, with the window on screen drawn over it. Drag the window to move through
  // time, pull an edge to widen or narrow it, click elsewhere to jump there. It replaces
  // the ‹ › and − + buttons; drag-to-zoom on the charts still works.
  (function () {
    var nav = document.getElementById('vt-nav'), win = document.getElementById('vt-nav-win'), line = document.getElementById('vt-nav-line');
    if (!nav || !win) return;
    var navA = 0, navB = 1, curA = 0, curB = 1, drag = null, navFloor = CHART_MIN_X;
    function place(a, b) {
      var span = (navB - navA) || 1;
      var l = Math.max(0, (a - navA) / span * 100), r = Math.min(100, (b - navA) / span * 100);
      win.style.left = l + '%';
      win.style.width = Math.max(0.8, r - l) + '%';
    }
    // The line draws itself in: a clip sweeps out to the points that just arrived (older
    // history loads in chunks), rather than the line jumping a chunk at a time.
    var clipR = document.getElementById('vt-nav-clip-r'), shown = { l: 0, r: 0, n: 0, tab: null }, anim = 0;
    function setClip(l, r) { if (clipR) { clipR.setAttribute('x', l.toFixed(1)); clipR.setAttribute('width', Math.max(0, r - l).toFixed(1)); } }
    function navReveal(l, r, n) {
      var fresh = shown.tab !== activeTab || shown.n === 0;
      var grew = n > shown.n;
      var from = fresh ? { l: l, r: l } : { l: shown.l, r: shown.r };
      shown.tab = activeTab; shown.n = n; shown.l = l; shown.r = r;
      cancelAnimationFrame(anim);
      if (!fresh && !grew) { setClip(l, r + 2); return; }
      var t0 = performance.now(), dur = fresh ? 1400 : 900;
      (function step(now) {
        var k = Math.min(1, (now - t0) / dur), e = 1 - Math.pow(1 - k, 3);
        setClip(from.l + (l - from.l) * e, from.r + (r + 2 - from.r) * e);
        if (k < 1) anim = requestAnimationFrame(step);
      })(t0);
    }
    window.vtNavSync = function () {
      var c = vtStack()[0]; if (!c) return;
      var a = c.options.scales.x.min, b = c.options.scales.x.max;
      if (a == null || b == null) return;
      curA = a; curB = b;
      var now = Date.now(), w = curB - curA;
      navB = Math.max(now, curB);
      navA = Math.min(curA - w * 0.5, navB - 48 * 3600e3);
      if (navA < CHART_MIN_X) navA = Math.min(CHART_MIN_X, curA);
      navFloor = Math.min(CHART_MIN_X, curA);
      // The strip draws the metric on screen: battery and RAM on 0–100, temperature
      // scaled to what it covers.
      var src = activeTab === 'ram' ? allRam : activeTab === 'temp' ? allTemp : allBattery;
      var pct = activeTab !== 'temp';
      var pts = src.filter(function (p) { return p.y != null && p.x >= navA && p.x <= navB; });
      var lo = pct ? 0 : Infinity, hi = pct ? 100 : -Infinity;
      if (!pct) pts.forEach(function (p) { lo = Math.min(lo, p.y); hi = Math.max(hi, p.y); });
      if (!(hi - lo >= 1)) hi = lo + 1;
      var step = Math.max(1, Math.floor(pts.length / 300)), d = '';
      for (var i = 0; i < pts.length; i += step) {
        var x = (pts[i].x - navA) / (navB - navA) * 1000, y = 31 - (pts[i].y - lo) / (hi - lo) * 28;
        d += (d ? ' L' : 'M') + x.toFixed(1) + ',' + y.toFixed(1);
      }
      if (line) line.setAttribute('d', d);
      if (pts.length) navReveal((pts[0].x - navA) / (navB - navA) * 1000, (pts[pts.length - 1].x - navA) / (navB - navA) * 1000, pts.length);
      if (!drag) place(curA, curB);
      var f = document.getElementById('vt-nav-from'), t = document.getElementById('vt-nav-to');
      if (f) f.textContent = fmtDate(navA);
      if (t) t.textContent = navB - now < 60000 ? 'now' : fmtDate(navB);
    };
    function move(e) {
      if (!drag) return;
      var dv = (e.clientX - drag.x) * drag.per, now = Date.now(), a = drag.a0, b = drag.b0;
      if (drag.mode === 'move') {
        var w = b - a; a += dv; b += dv;
        if (b > now) { b = now; a = b - w; }
        if (a < navFloor) { a = navFloor; b = a + w; }
      } else if (drag.mode === 'l') a = Math.max(navFloor, Math.min(a + dv, b - 60000));
      else b = Math.min(now, Math.max(b + dv, a + 60000));
      drag.a = a; drag.b = b; place(a, b);
    }
    nav.addEventListener('pointerdown', function (e) {
      if (e.button !== 0) return;
      var rect = nav.getBoundingClientRect(), per = (navB - navA) / Math.max(1, rect.width);
      var a0 = curA, b0 = curB, jumped = false;
      if (!e.target.closest('#vt-nav-win')) {          // jump: centre the window on the click
        var cx = navA + (e.clientX - rect.left) * per, w = b0 - a0;
        a0 = cx - w / 2; b0 = cx + w / 2; jumped = true;
      }
      drag = { mode: e.target.getAttribute('data-edge') || 'move', x: e.clientX, a0: a0, b0: b0, per: per, a: a0, b: b0, jumped: jumped };
      try { nav.setPointerCapture(e.pointerId); } catch (err) {}
      nav.classList.add('drag');
      move(e);
      e.preventDefault();
    });
    nav.addEventListener('pointermove', move);
    function end() {
      if (!drag) return;
      var d = drag; drag = null; nav.classList.remove('drag');
      if (!d.jumped && Math.abs(d.a - curA) < 1000 && Math.abs(d.b - curB) < 1000) { place(curA, curB); return; }
      applyZoomWindow(d.a, d.b);
    }
    nav.addEventListener('pointerup', end);
    nav.addEventListener('pointercancel', end);
  })();

  var focusTab = applyFocusFromURL();
  // Populate/render after a paint so the "Loading graphs…" overlay is visible while
  // Chart.js lays out the data (which can take a beat on devices with lots of points).
  requestAnimationFrame(function () {
    updateCharts();
    syncMarkerToggle();
    setChartTab(focusTab || activeTab);
    var cl = document.getElementById('chart-loading');
    if (cl) { cl.classList.add('hide'); setTimeout(function () { cl.style.display = 'none'; }, 300); }
  });
  // setOnline is how the presence SSE handler (a separate <script> block) reaches
  // deviceOnline — going through the export instead of relying on cross-script
  // script-scope resolution for a top-level `let`.
  function setOnline(v) {
    if (deviceOnline === v) return;
    deviceOnline = v;
    updateCharts();
  }
  // lastPlottedMs — newest check-in time already drawn, so the SSE push dedups
  // against it directly instead of inferring it from a single series (which fails
  // for battery-less kiosk devices). Seeded lazily on the first push from whatever
  // history rendered; reset here because hx-boost re-runs this boot on every visit.
  window.mdmChart = { allBattery, allTemp, allRam, allCharge, pushCharge, updateCharts, setOnline, lastPlottedMs: null, tempLoaded, ramLoaded };

  // The trailing break turns true purely by the passage of time — the device is gone,
  // so no check-in, SSE frame or presence event will arrive to trigger a redraw. The
  // presence event fires when the socket read deadline expires, about a minute in and
  // so a quarter-hour early. Poll the boundary instead, and redraw only when it flips,
  // so an open dashboard grows the break on its own rather than needing a reload.
  //
  // Kept on window and cleared first: hx-boost re-injects this script on every
  // navigation back to a device page, so mdmBootCharts runs again. Without the clear,
  // each visit leaves a live timer holding the previous run's charts.
  if (window.mdmTrailingTick) clearInterval(window.mdmTrailingTick);
  let trailingBreakShown = false;
  window.mdmTrailingTick = setInterval(function () {
    const shown = allBattery.length > 0
      && trailingSilenceMs(allBattery[allBattery.length - 1].x) > 0;
    if (shown === trailingBreakShown) return;
    trailingBreakShown = shown;
    updateCharts();
  }, 60000);

  // Recolor chart grid/ticks live when the theme is toggled.
  window.addEventListener('mdm:themechange', function () {
    const cs = getComputedStyle(document.documentElement);
    const grid = (cs.getPropertyValue('--chart-grid') || '').trim() || GRID;
    const tick = (cs.getPropertyValue('--chart-tick') || '').trim() || TICK;
    [batteryChart, tempChart, ramChart, unifiedChart].forEach(function (ch) {
      if (!ch || !ch.options || !ch.options.scales) return;
      Object.keys(ch.options.scales).forEach(function (k) {
        const sc = ch.options.scales[k];
        if (!sc) return;
        if (sc.grid) sc.grid.color = grid;
        if (sc.ticks) sc.ticks.color = tick;
      });
      ch.update('none');
    });
  });
}

// Wait for Chart.js. Failures surface in the overlay instead of leaving it
// spinning — a stuck spinner reads as "still working" and costs the user a
// refresh to find out otherwise.
window.mdmChartReady.then(mdmBootCharts).then(function () {
  // First paint is done; widen the loaded window in the background so the range
  // buttons don't have to fetch. Idle-scheduled so it never competes with the
  // first render.
  var go = function () { if (window.mdmChartWarm) window.mdmChartWarm(); };
  if (window.requestIdleCallback) requestIdleCallback(go, { timeout: 3000 });
  else setTimeout(go, 1200);
}).catch(function (err) {
  console.error('[mdm] chart bootstrap failed:', err);
  var cl = document.getElementById('chart-loading');
  if (cl) {
    var t = cl.querySelector('.chart-loading-t');
    if (t) t.textContent = 'Charts unavailable — reload the page';
    var sp = cl.querySelector('.chart-spin');
    if (sp) sp.style.display = 'none';
  }
});
