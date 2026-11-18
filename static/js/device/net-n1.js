// Network sheet (N1): the last 24 h of Wi-Fi signal under the connection facts. Bento view only;
// the card is swapped by htmx while a Wi-Fi command runs, so it is redrawn after every swap.
(function () {
  function draw(el, pts, online) {
    var W = 320, H = 120, P = 6, lo = -95, hi = -30;
    var t0 = pts[0].x, t1 = pts[pts.length - 1].x, span = Math.max(1, t1 - t0);
    var xs = function (t) { return P + (W - 2 * P) * (t - t0) / span; };
    var ys = function (v) { v = Math.max(lo, Math.min(hi, v)); return P + (H - 2 * P) * (hi - v) / (hi - lo); };
    var d = pts.map(function (p, i) { return (i ? 'L' : 'M') + xs(p.x).toFixed(1) + ' ' + ys(p.y).toFixed(1); }).join(' ');
    var weak = ys(-80).toFixed(1);
    var last = pts[pts.length - 1].y, min = Math.min.apply(null, pts.map(function (p) { return p.y; }));
    el.innerHTML = '<div class="wf-sig-h"><b>Signal · last 24 h</b><span>now ' + last + ' dBm · weakest ' + min + ' dBm</span></div>' +
      '<svg viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none" role="img" aria-label="Wi-Fi signal over the last 24 hours">' +
      '<rect x="0" y="' + weak + '" width="' + W + '" height="' + (H - weak) + '" class="wf-sig-weak"/>' +
      '<line x1="0" x2="' + W + '" y1="' + weak + '" y2="' + weak + '" class="wf-sig-line"/>' +
      '<path d="' + d + '" class="wf-sig-p"/></svg>' +
      '<div class="wf-sig-f"><span>24 h ago</span><span>weak below −80 dBm</span><span>now</span></div>';
    el.hidden = false;
  }
  function load() {
    document.querySelectorAll('.dd-bento .wf-sigcard[data-serial]').forEach(function (el) {
      var until = Date.now(), from = until - 86400000;
      fetch('/devices/' + el.getAttribute('data-serial') + '/chart-data?from=' + from + '&until=' + until, { credentials: 'same-origin' })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (j) {
          var w = j && (j.wifi || []).filter(function (p) { return p && typeof p.y === 'number'; });
          if (w && w.length > 1) draw(el, w); else { el.hidden = true; }
        }).catch(function () {});
    });
  }
  document.addEventListener('DOMContentLoaded', load);
  document.body && document.body.addEventListener('htmx:afterSwap', function (e) { if (e.target && (e.target.id === 'dd-wifi' || (e.target.querySelector && e.target.querySelector('#dd-wifi')))) load(); });
  document.addEventListener('htmx:afterSettle', function (e) { if (e.target && e.target.id === 'dd-wifi') load(); });
})();
