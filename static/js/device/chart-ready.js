// Loaded by templates/device.html.
window.mdmChartReady = window.mdmChartReady || (window.Chart
  ? Promise.resolve()
  : new Promise(function (resolve, reject) {
      var s = document.createElement('script');
      s.src = '/static/vendor/chart.umd.min.js';
      s.onload = resolve;
      s.onerror = function () { reject(new Error('Chart.js failed to load')); };
      // head, not the swapped <main>: this survives boosted navigation, so
      // returning to a device page reuses the already-parsed library.
      document.head.appendChild(s);
    }));
