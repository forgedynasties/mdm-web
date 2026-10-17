// Loaded by templates/overview.html.
      /* Under hx-boost, htmx injects this fragment's <script> tags and an external
           `<script src>` resolves asynchronously — unlike a full page load, which
           blocks on it — so the bootstrap below could run with Chart still undefined
           and silently no-op. window.mdmChartReady (same pattern as device.html)
           makes the dependency explicit and awaits it instead of racing it. */
      window.mdmChartReady = window.mdmChartReady || (window.Chart
        ? Promise.resolve()
        : new Promise(function (resolve, reject) {
            var s = document.createElement('script');
            s.src = '/static/vendor/chart.umd.min.js';
            s.onload = resolve;
            s.onerror = function () { reject(new Error('Chart.js failed to load')); };
            document.head.appendChild(s);
          }));
      window.mdmChartReady.then(function(){
        var el = document.getElementById('ov-activity');
        if (!el || typeof Chart === 'undefined') return;
        var rows = window.OV_ACTIVITY_ROWS;
        var cs = getComputedStyle(document.body);
        var accent = (cs.getPropertyValue('--accent') || '#f9674e').trim();
        var warn = (cs.getPropertyValue('--warn') || '#8f4209').trim();
        var tick = (cs.getPropertyValue('--chart-tick') || '#687078').trim();
        var grid = (cs.getPropertyValue('--chart-grid') || 'rgba(16,24,40,.08)').trim();
        var ctx = el.getContext('2d');
        var grad = ctx.createLinearGradient(0, 0, 0, 200);
        grad.addColorStop(0, accent + '44'); grad.addColorStop(1, accent + '00');
        new Chart(ctx, {
          type: 'line',
          data: { labels: rows.map(function(r){ return r.label; }),
            datasets: [
              { label: 'Active', data: rows.map(function(r){ return r.val; }),
                borderColor: accent, backgroundColor: grad, fill: true, tension: 0.35,
                borderWidth: 2, pointRadius: 3, pointHoverRadius: 5,
                pointBackgroundColor: accent, pointBorderColor: '#fff', pointBorderWidth: 1 },
              { label: 'Low battery', data: rows.map(function(r){ return r.low; }),
                borderColor: warn, borderDash: [4,4], fill: false, tension: 0.35,
                borderWidth: 1.6, pointRadius: 2, pointHoverRadius: 4, pointBackgroundColor: warn }
            ] },
          options: {
            responsive: true, maintainAspectRatio: false,
            interaction: { intersect: false, mode: 'index' },
            plugins: { legend: { display: false } },
            scales: {
              x: { grid: { display: false }, ticks: { color: tick } },
              y: { beginAtZero: true, grid: { color: grid }, ticks: { color: tick, precision: 0 } }
            }
          }
        });
      }).catch(function (err) { console.error('[mdm] fleet activity chart failed:', err); });
      
