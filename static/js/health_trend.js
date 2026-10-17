// Loaded by templates/health.html: the Fleet health page's "Last 14 days" charts —
// online devices, low battery, running hot and crashes per day, from #fh-series.
// Chart.js is loaded once (window.mdmChartReady).
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
      })();
