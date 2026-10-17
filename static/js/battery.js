// Loaded by templates/battery.html.
(function () {
  var el = document.getElementById('bt-chart'), src = document.getElementById('bt-data');
  if (!el || !src) return;
  var R; try { R = JSON.parse(src.textContent); } catch (e) { return; }
  if (!R.length) return;
  // [ms, ev, mv, pct, status, plug, temp, screen]
  function draw() {
    var W = el.clientWidth || 800, H = el.clientHeight || 280, top = 14, bot = 22, h = H - top - bot;
    var t0 = R[0][0], t1 = R[R.length - 1][0]; if (t1 === t0) t1 = t0 + 1;
    var mv = R.filter(function (r) { return r[2] > 0; }).map(function (r) { return r[2]; });
    var lo = Math.min.apply(null, mv) - 15, hi = Math.max.apply(null, mv) + 15;
    function x(t) { return (t - t0) / (t1 - t0) * W; }
    function y(v) { return top + h - (v - lo) / (hi - lo) * h; }
    function yp(p) { return top + h - p / 100 * h; }
    function yt(c) { return top + h - (c - 15) / 35 * h; }
    var s = '';
    for (var i = 0; i < R.length - 1; i++) {
      var a = x(R[i][0]), b = Math.max(x(R[i + 1][0]) - a, 0.6);
      if (R[i][4] === 2) s += '<rect x="' + a + '" y="' + top + '" width="' + b + '" height="' + h + '" fill="rgba(34,197,94,.13)"/>';
      if (R[i][7] === 1) s += '<rect x="' + a + '" y="' + (H - bot + 4) + '" width="' + b + '" height="6" fill="rgba(168,85,247,.6)"/>';
    }
    function line(f, get, color, w, dash) {
      var pts = R.filter(f).map(function (r) { return x(r[0]).toFixed(1) + ',' + get(r).toFixed(1); }).join(' ');
      return '<polyline fill="none" stroke="' + color + '" stroke-width="' + w + '"' + (dash ? ' stroke-dasharray="' + dash + '"' : '') + ' points="' + pts + '"/>';
    }
    s += line(function (r) { return r[3] >= 0; }, function (r) { return yp(r[3]); }, '#60a5fa', 1.4, '4 3');
    s += line(function (r) { return r[6] > -100; }, function (r) { return yt(r[6]); }, '#facc15', 1.1);
    s += line(function (r) { return r[2] > 0; }, function (r) { return y(r[2]); }, '#f97316', 1.8);
    var span = t1 - t0, fmtT = function (t) { var d = new Date(t); return span > 36e5 * 30 ? d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }); };
    for (var k = 0; k <= 4; k++) { var tv = t0 + span * k / 4; s += '<text x="' + Math.min(Math.max(x(tv) - 20, 0), W - 70) + '" y="' + (H - 2) + '" font-size="11" fill="currentColor" opacity=".55">' + fmtT(tv) + '</text>'; }
    s += '<text x="4" y="' + (top + 10) + '" font-size="11" fill="#f97316">' + hi + ' mV</text><text x="4" y="' + (top + h - 4) + '" font-size="11" fill="#f97316">' + lo + ' mV</text>';
    s += '<line id="bt-cur" x1="0" x2="0" y1="' + top + '" y2="' + (top + h) + '" stroke="currentColor" stroke-opacity=".35" stroke-dasharray="3 3" visibility="hidden"/>';
    el.setAttribute('viewBox', '0 0 ' + W + ' ' + H);
    el.innerHTML = s;
    var tip = document.getElementById('bt-tip'), cur = document.getElementById('bt-cur');
    el.onmousemove = function (e) {
      var bx = el.getBoundingClientRect(), t = t0 + (e.clientX - bx.left) / bx.width * span, best = 0;
      for (var j = 1; j < R.length; j++) { if (Math.abs(R[j][0] - t) < Math.abs(R[best][0] - t)) best = j; }
      var r = R[best], st = { 2: 'charging', 3: 'discharging', 4: 'not charging', 5: 'full' }[r[4]] || ('status ' + r[4]);
      tip.innerHTML = new Date(r[0]).toLocaleString() + '<br><b>' + r[2] + ' mV</b> · ' + r[3] + '% · ' + st + (r[6] > -100 ? ' · ' + r[6].toFixed(1) + ' °C' : '') + (r[7] === 1 ? ' · screen on' : '');
      tip.style.display = 'block';
      var px = (r[0] - t0) / span * bx.width;
      tip.style.left = Math.min(px + 12, bx.width - tip.offsetWidth - 4) + 'px'; tip.style.top = '8px';
      cur.setAttribute('x1', x(r[0])); cur.setAttribute('x2', x(r[0])); cur.setAttribute('visibility', 'visible');
    };
    el.onmouseleave = function () { tip.style.display = 'none'; cur.setAttribute('visibility', 'hidden'); };
  }
  draw();
  window.addEventListener('resize', function () { requestAnimationFrame(draw); });
})();
