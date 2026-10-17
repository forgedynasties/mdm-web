// Loaded by templates/ai_report_card.html.
      // Evidence charts: fleet-wide series (above) and, under each report call-out,
      // the 14-day series for the site or device it names, on the metric that
      // matches the call-out's area. Chart.js is loaded once (window.mdmChartReady).
      (function () {
        window.mdmChartReady = window.mdmChartReady || (window.Chart ? Promise.resolve() : new Promise(function (res, rej) {
          var s = document.createElement('script'); s.src = '/static/vendor/chart.umd.min.js'; s.onload = res; s.onerror = function () { rej(new Error('Chart.js failed')); }; document.head.appendChild(s);
        }));
        var cs = getComputedStyle(document.body);
        var tok = function (n, d) { return (cs.getPropertyValue(n) || d).trim(); };
        var C = { ok: tok('--ok', '#0f6d33'), warn: tok('--warn', '#8f4209'), bad: tok('--danger', '#b42318'), accent: tok('--accent', '#f9674e'), v2: tok('--accent-2', '#7a80f6'), tick: tok('--chart-tick', '#687078'), grid: tok('--chart-grid', 'rgba(16,24,40,.08)') };
        function line(el, labels, sets, opts) {
          var ctx = el.getContext('2d');
          return new Chart(ctx, { type: 'line', data: { labels: labels, datasets: sets.map(function (d) {
              var g = ctx.createLinearGradient(0, 0, 0, el.parentNode.offsetHeight || 90); g.addColorStop(0, d.color + '3a'); g.addColorStop(1, d.color + '00');
              return { data: d.data, borderColor: d.color, backgroundColor: d.fill === false ? 'transparent' : g, fill: d.fill !== false, tension: .35, borderWidth: d.dash ? 1.2 : 1.8, borderDash: d.dash || [], pointRadius: d.dash ? 0 : 2, pointHoverRadius: 4, pointBackgroundColor: d.color, spanGaps: true, label: d.label || '' };
            }) },
            options: Object.assign({ responsive: true, maintainAspectRatio: false, animation: false, interaction: { intersect: false, mode: 'index' },
              plugins: { legend: { display: false }, tooltip: { callbacks: { label: function (c) { return (c.dataset.label ? c.dataset.label + ': ' : '') + (c.parsed.y == null ? '—' : c.parsed.y + (opts && opts.unit || '')); } } } },
              scales: { x: { grid: { display: false }, ticks: { color: C.tick, maxTicksLimit: 5, font: { size: 10 } } }, y: { beginAtZero: true, grid: { color: C.grid }, ticks: { color: C.tick, precision: 0, maxTicksLimit: 4, font: { size: 10 } }, suggestedMax: opts && opts.max } } }, {}) });
        }
        function fmt(v, unit) { return v == null ? '—' : (Math.round(v * 10) / 10) + (unit || ''); }
        // Fleet-wide
        window.mdmChartReady.then(function () {
          var S; try { S = JSON.parse(document.getElementById('fh-series').textContent); } catch (e) { return; }
          var days = S.days || [], labels = days.map(function (d) { return d.day; });
          function last(a) { return a.length ? a[a.length - 1] : null; }
          function put(id, text) { var e = document.getElementById(id); if (e) e.textContent = text; }
          var act = days.map(function (d) { return d.active; }), low = days.map(function (d) { return d.low; }), hot = days.map(function (d) { return d.hot; });
          line(document.getElementById('fh-c-active'), labels, [{ data: act, color: C.ok, label: 'online' }], { max: S.total || undefined });
          put('fh-ev-active', 'today ' + fmt(last(act)) + ' of ' + (S.total || '?'));
          line(document.getElementById('fh-c-low'), labels, [{ data: low, color: C.warn, label: 'low battery' }]);
          put('fh-ev-low', 'today ' + fmt(last(low)) + ' · peak ' + fmt(Math.max.apply(null, low.concat([0]))));
          line(document.getElementById('fh-c-hot'), labels, [{ data: hot, color: C.bad, label: 'hot' }]);
          put('fh-ev-hot', 'today ' + fmt(last(hot)) + ' · peak ' + fmt(Math.max.apply(null, hot.concat([0]))));
          var cr = S.crashes || [], crl = cr.map(function (_, i) { var d = new Date(); d.setDate(d.getDate() - (cr.length - 1 - i)); return d.toLocaleDateString([], { month: 'short', day: 'numeric' }); });
          line(document.getElementById('fh-c-crash'), crl, [{ data: cr, color: C.accent, label: 'crashes' }]);
          put('fh-ev-crash', 'today ' + fmt(last(cr)) + ' · 7-day total ' + cr.reduce(function (a, b) { return a + b; }, 0));
        }).catch(function () {});

        // Per call-out
        var METRIC = {
          heat: { dev: 'temp_max', site: 'temp_max', label: 'Peak temperature', unit: '°C', color: C.bad, ref: 45, refLabel: 'hot cutoff' },
          battery: { dev: 'battery_avg', site: 'battery_avg', label: 'Battery average', unit: '%', color: C.warn, ref: 20, refLabel: 'low cutoff', max: 100, extra: 'battery_min', extraLabel: 'daily low' },
          charging: { dev: 'charging_frac', site: 'charging_frac', label: 'Time on charger', unit: '%', color: C.v2, scale: 100, max: 100 },
          memory: { dev: 'ram_pct_peak', site: null, label: 'Peak RAM', unit: '%', color: C.v2, ref: 90, refLabel: 'pressure', max: 100 },
          connectivity: { dev: 'online_minutes', site: 'online_min_avg', label: 'Online minutes per day', unit: ' min', color: C.ok },
          crash: { dev: null, site: null }
        };
        function readJSON(id) { try { return JSON.parse(document.getElementById(id).textContent); } catch (e) { return []; } }
        function resolve(scope) {
          var s = String(scope || '').trim(); if (!s) return null;
          var serials = readJSON('ai-sum-serials'), hit = null;
          serials.forEach(function (x) { if (!hit && s.indexOf(x) >= 0) hit = { kind: 'dev', id: x, name: x }; });
          if (hit) return hit;
          var rests = readJSON('ai-sum-restaurants'), ls = s.toLowerCase();
          rests.forEach(function (r) { if (!hit && r.name && ls.indexOf(r.name.toLowerCase()) >= 0) hit = { kind: 'site', id: r.id, name: r.name }; });
          return hit;
        }
        function attach(row) {
          if (row.dataset.evDone) return; row.dataset.evDone = '1';
          var slot = row.querySelector('.rep-ev'); if (!slot) return;
          var m = METRIC[row.dataset.area] || METRIC.battery, tgt = resolve(row.dataset.scope);
          if (!tgt) return;
          var field = tgt.kind === 'dev' ? m.dev : m.site; if (!field) return;
          var url = tgt.kind === 'dev' ? '/devices/' + encodeURIComponent(tgt.id) + '/daily-stats?days=14' : '/restaurants/' + tgt.id + '/daily-stats?days=14';
          fetch(url, { credentials: 'same-origin', headers: { Accept: 'application/json' } }).then(function (r) { return r.json(); }).then(function (rows) {
            rows = Array.isArray(rows) ? rows : []; if (!rows.length) return;
            var labels = rows.map(function (r) { return new Date(r.day).toLocaleDateString([], { month: 'short', day: 'numeric' }); });
            var vals = rows.map(function (r) { var v = r[field]; return v == null ? null : (m.scale ? v * m.scale : v); });
            var sets = [{ data: vals, color: m.color, label: m.label }];
            if (m.extra) { var ex = rows.map(function (r) { return r[m.extra] == null ? null : r[m.extra]; }); if (ex.some(function (v) { return v != null; })) sets.push({ data: ex, color: m.color, dash: [3, 3], fill: false, label: m.extraLabel }); }
            if (m.ref != null) sets.push({ data: vals.map(function () { return m.ref; }), color: C.tick, dash: [4, 4], fill: false, label: m.refLabel });
            var present = vals.filter(function (v) { return v != null; }); if (!present.length) return;
            var latest = present[present.length - 1], hi = Math.max.apply(null, present), lo = Math.min.apply(null, present);
            slot.innerHTML = '<div class="h"><span>' + m.label + ' · ' + tgt.name + '</span><b>latest ' + fmt(latest, m.unit) + ' · range ' + fmt(lo, m.unit) + '–' + fmt(hi, m.unit) + '</b></div><div class="c"><canvas></canvas></div>';
            slot.hidden = false;
            window.mdmChartReady.then(function () { line(slot.querySelector('canvas'), labels, sets, { unit: m.unit, max: m.max }); });
          }).catch(function () {});
        }
        function scan() { document.querySelectorAll('#ai-sum-full .rep-row[data-scope]').forEach(attach); }
        var full = document.getElementById('ai-sum-full');
        if (full) { new MutationObserver(scan).observe(full, { childList: true }); scan(); }
      })();
      
