// Loaded by templates/_lost_mode.html (lost-report-card) and on the device page.
// Draws the "Since it went missing" trail: the placed timeline points, oldest first, joined
// by a dashed line, on the same Google map the Location tab uses. Loads the Maps API only
// when nothing on the page has, and draws nothing without a key or a point.
(function () {
  function styles() { return typeof devMapStyles === 'function' ? devMapStyles() : null; }
  function draw() {
    var el = document.getElementById('lr-map');
    if (!el || el.dataset.drawn === '1' || !window.google || !google.maps) return;
    var pts = [];
    try { pts = JSON.parse(el.dataset.points || '[]'); } catch (e) { return; }
    if (!pts.length) return;
    el.dataset.drawn = '1';
    el.innerHTML = '';
    var css = getComputedStyle(document.documentElement);
    var red = (css.getPropertyValue('--danger') || '#b42318').trim();
    var blue = (css.getPropertyValue('--accent-2') || '#7a80f6').trim();
    var green = (css.getPropertyValue('--ok') || '#0f6d33').trim();
    var map = new google.maps.Map(el, { mapTypeControl: false, streetViewControl: false, fullscreenControl: false, styles: styles() });
    window.addEventListener('mdm:themechange', function () { map.setOptions({ styles: styles() }); });
    var bounds = new google.maps.LatLngBounds(), path = [];
    pts.forEach(function (p, i) {
      var pos = { lat: p.lat, lng: p.lon };
      path.push(pos); bounds.extend(pos);
      var last = i === pts.length - 1;
      new google.maps.Marker({
        position: pos, map: map, title: p.title,
        icon: { path: google.maps.SymbolPath.CIRCLE, scale: last ? 7 : 5, fillOpacity: 1, strokeColor: '#fff', strokeWeight: 2,
          fillColor: p.kind === 'heard' ? green : (last ? red : blue) }
      });
    });
    if (path.length > 1) {
      new google.maps.Polyline({ path: path, map: map, strokeOpacity: 0,
        icons: [{ icon: { path: 'M 0,-1 0,1', strokeOpacity: 1, strokeColor: red, scale: 3 }, offset: '0', repeat: '12px' }] });
      map.fitBounds(bounds, 40);
    } else {
      map.setCenter(path[0]); map.setZoom(16);
    }
  }
  function load() {
    var el = document.getElementById('lr-map');
    if (!el) return;
    if (window.google && google.maps) { draw(); return; }
    // Another map on the page may still be loading the API: wait for it a little.
    if (document.querySelector('script[src*="maps.googleapis.com/maps/api/js"]')) {
      var n = 0, t = setInterval(function () { if ((window.google && google.maps) || ++n > 40) { clearInterval(t); draw(); } }, 250);
      return;
    }
    if (!el.dataset.key) return;
    window.lostTrailInit = draw;
    var s = document.createElement('script');
    s.async = true;
    s.src = 'https://maps.googleapis.com/maps/api/js?key=' + encodeURIComponent(el.dataset.key) + '&callback=lostTrailInit';
    document.head.appendChild(s);
  }
  // Location lives on the About tab (26 Oct 2026); draw on load and when About is shown.
  load();
  document.addEventListener('click', function (e) { if (e.target.closest && e.target.closest('#dd-tab-about')) setTimeout(load, 50); });
  document.body.addEventListener('htmx:afterSettle', load);
})();

// ?lost=1 (from the "left the venue" alert's "Mark as lost…"): open the mark-as-lost dialog
// once, then drop the parameter so a reload doesn't reopen it.
(function () {
  var q = new URLSearchParams(location.search);
  if (q.get('lost') !== '1') return;
  var d = document.getElementById('lost-dlg');
  if (d && !d.open) { try { d.showModal(); } catch (e) {} }
  q.delete('lost');
  history.replaceState(history.state, '', location.pathname + (q.toString() ? '?' + q : '') + location.hash);
})();
