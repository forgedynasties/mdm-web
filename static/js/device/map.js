// Loaded by templates/device.html. The Location card's map (MP1 "Canvas", picked 5 Nov 2026):
// our own MapLibre map (static/js/map-canvas.js) with the device as a pin named by its serial
// and venue.
(function () {
  var el = document.getElementById('dev-map'), d = window.DEVICE_MAP;
  if (!el || !d) return;
  var lat = parseFloat(d.lat), lng = parseFloat(d.lng);
  if (isNaN(lat) || isNaN(lng)) return;
  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  // The map lives in the Location tab, hidden until opened: it is built the first time it has
  // a size, and follows the card's size after that.
  var map = null;
  function build() {
    if (map || !el.clientWidth) return;
    map = true;
    mdmMap.load(function () {
      map = mdmMap.create(el, { center: [lng, lat], zoom: 15 });
      var pin = document.createElement('div');
      pin.className = 'mm-pin' + (d.online ? '' : ' off');
      pin.innerHTML = '<b>' + esc(d.serial) + (d.venue ? ' · ' + esc(d.venue) : '') + '</b><i></i>';
      new maplibregl.Marker({ element: pin, anchor: 'bottom' }).setLngLat([lng, lat]).addTo(map);
    });
  }
  if (window.ResizeObserver) new ResizeObserver(function () { if (map && map.resize) map.resize(); else build(); }).observe(el);
  else build();
})();
