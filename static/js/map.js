// Loaded by templates/map.html.
(function () {
  var pts = [];
  try { pts = JSON.parse(document.getElementById('fm-data').textContent || '[]'); } catch (e) {}
  var state = { q: '', status: 'all', product: 'all', site: 'all', cluster: true, labels: false, style: 'auto', only: null };
  // ?serials=A,B,C from the fleet selection panel: show only those devices.
  (function () {
    var raw = new URLSearchParams(location.search).get('serials'); if (!raw) return;
    var set = {}; raw.split(',').forEach(function (x) { x = x.trim(); if (x) set[x] = true; });
    state.only = set;
    var n = Object.keys(set).length, have = pts.filter(function (p) { return set[p.serial]; }).length;
    var host = document.querySelector('.fm-opts'); if (!host) return;
    var chip = document.createElement('div'); chip.className = 'fm-only';
    chip.innerHTML = '<span>Showing <b>' + have + '</b> of ' + n + ' selected device' + (n === 1 ? '' : 's') + (have < n ? ' · ' + (n - have) + ' without a location' : '') + '</span><a href="/map">Show whole fleet</a>';
    host.insertBefore(chip, host.firstChild);
  })();
  var map = null, markers = [], clusterer = null, iw = null, byId = {};
  function esc(s){ return String(s==null?'':s).replace(/[&<>"']/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c];}); }
  function ago(iso){ var d=(Date.now()-new Date(iso).getTime())/1000; if(!isFinite(d)) return ''; if(d<90) return 'just now'; if(d<3600) return Math.round(d/60)+'m ago'; if(d<86400) return Math.round(d/3600)+'h ago'; return Math.round(d/86400)+'d ago'; }

  // Facet controls from the data.
  var prods = {}; pts.forEach(function (p) { prods[p.product || '—'] = (prods[p.product || '—'] || 0) + 1; });
  var ph = document.getElementById('fm-product');
  Object.keys(prods).sort().forEach(function (k) {
    var b = document.createElement('button'); b.type = 'button'; b.setAttribute('data-v', k);
    b.innerHTML = esc(k) + ' <span class="n">' + prods[k] + '</span>'; ph.appendChild(b);
  });
  var sites = {}; pts.forEach(function (p) { var k = p.restaurant || ''; if (k) sites[k] = (sites[k] || 0) + 1; });
  var ss = document.getElementById('fm-site');
  Object.keys(sites).sort().forEach(function (k) { var o = document.createElement('option'); o.value = k; o.textContent = k + ' (' + sites[k] + ')'; ss.appendChild(o); });
  if (!Object.keys(sites).length) ss.disabled = true;
  var on = pts.filter(function (p) { return p.online; }).length;
  document.querySelector('[data-count=all]').textContent = pts.length;
  document.querySelector('[data-count=online]').textContent = on;
  document.querySelector('[data-count=offline]').textContent = pts.length - on;

  function visible() {
    var q = state.q.trim().toLowerCase();
    return pts.filter(function (p) {
      if (state.only && !state.only[p.serial]) return false;
      if (state.status === 'online' && !p.online) return false;
      if (state.status === 'offline' && p.online) return false;
      if (state.product !== 'all' && (p.product || '—') !== state.product) return false;
      if (state.site !== 'all' && (p.restaurant || '') !== state.site) return false;
      if (q && [p.serial, p.name, p.restaurant, p.address, p.build].join(' ').toLowerCase().indexOf(q) < 0) return false;
      return true;
    });
  }

  function renderList(list) {
    var host = document.getElementById('fm-list');
    host.innerHTML = '';
    document.getElementById('fm-shown').textContent = list.length + ' shown';
    var c = document.getElementById('fm-count'); if (c) c.textContent = list.length + ' of ' + pts.length + ' devices';
    if (!list.length) { host.innerHTML = '<div class="fm-empty"><b>No devices match</b>Loosen the filters or clear the search.</div>'; return; }
    var groups = {};
    list.forEach(function (p) { var k = p.restaurant || 'Unassigned'; (groups[k] = groups[k] || []).push(p); });
    Object.keys(groups).sort(function (a, b) { return (a === 'Unassigned') - (b === 'Unassigned') || a.localeCompare(b); }).forEach(function (k) {
      var h = document.createElement('div'); h.className = 'fm-site'; h.innerHTML = esc(k) + '<span class="n">' + groups[k].length + '</span>'; host.appendChild(h);
      groups[k].sort(function (a, b) { return (b.online - a.online) || a.serial.localeCompare(b.serial); }).forEach(function (p) {
        var d = document.createElement('div'); d.className = 'fm-dev'; d.setAttribute('data-id', p.serial);
        var tag = p.battery > 0 && p.battery < 20 ? '<span class="tag warn">' + p.battery + '%</span>' : (p.kiosk ? '' : '<span class="tag">unlocked</span>');
        d.innerHTML = '<span class="st ' + (p.online ? 'ok' : '') + '"></span><div class="main"><div class="sn">' + esc(p.serial) + '</div><div class="meta">' + esc(p.product || '') + (p.address ? ' · ' + esc(p.address) : '') + ' · ' + (p.online ? 'online' : 'seen ' + ago(p.last_seen)) + '</div></div>' + tag;
        d.addEventListener('click', function () { focus(p, d); });
        host.appendChild(d);
      });
    });
  }

  function focus(p, row) {
    Array.prototype.forEach.call(document.querySelectorAll('.fm-dev.on'), function (e) { e.classList.remove('on'); });
    if (row) row.classList.add('on');
    if (!map) return;
    map.panTo({ lat: p.lat, lng: p.lon });
    if (map.getZoom() < 15) map.setZoom(15);
    var mk = byId[p.serial]; if (mk) openInfo(p, mk);
  }
  function openInfo(p, mk) {
    iw.setContent('<div style="min-width:170px;font-size:13px"><b>' + esc(p.name) + '</b>'
      + '<div style="opacity:.7;font-family:monospace">' + esc(p.serial) + '</div>'
      + (p.address ? '<div>' + esc(p.address) + '</div>' : '')
      + '<div style="margin:4px 0">' + (p.online ? '🟢 online' : '⚪ offline · seen ' + ago(p.last_seen)) + (p.battery ? ' · 🔋 ' + p.battery + '%' : '') + '</div>'
      + '<a href="/devices/' + encodeURIComponent(p.serial) + '">Open device →</a></div>');
    iw.open(map, mk);
  }

  var DARK = [
    {elementType:'geometry',stylers:[{color:'#212a37'}]},{elementType:'labels.text.fill',stylers:[{color:'#9ca5b3'}]},
    {elementType:'labels.text.stroke',stylers:[{color:'#1b2431'}]},{featureType:'administrative',elementType:'geometry',stylers:[{color:'#4b5563'}]},
    {featureType:'poi',stylers:[{visibility:'off'}]},{featureType:'road',elementType:'geometry',stylers:[{color:'#2c3644'}]},
    {featureType:'road',elementType:'labels.text.fill',stylers:[{color:'#9ca5b3'}]},{featureType:'road.highway',elementType:'geometry',stylers:[{color:'#3a4757'}]},
    {featureType:'transit',stylers:[{visibility:'off'}]},{featureType:'water',elementType:'geometry',stylers:[{color:'#17212f'}]},
    {featureType:'water',elementType:'labels.text.fill',stylers:[{color:'#51607a'}]}
  ];
  function applyStyle() {
    if (!map) return;
    var s = state.style, dark = window.mdmTheme && window.mdmTheme() === 'dark';
    if (s === 'satellite' || s === 'hybrid') { map.setMapTypeId(s); map.setOptions({ styles: [] }); return; }
    map.setMapTypeId('roadmap');
    map.setOptions({ styles: (s === 'dark' || (s === 'auto' && dark)) ? DARK : [] });
  }

  function renderMarkers(list) {
    if (!map) return;
    if (clusterer) { clusterer.clearMarkers(); clusterer = null; }
    markers.forEach(function (m) { m.setMap(null); }); markers = []; byId = {};
    var bounds = new google.maps.LatLngBounds();
    list.forEach(function (p) {
      var mk = new google.maps.Marker({
        position: { lat: p.lat, lng: p.lon }, map: map, title: p.serial,
        label: state.labels ? { text: p.serial, fontSize: '10px', fontWeight: '700', className: 'fm-mlabel' } : null,
        icon: { path: google.maps.SymbolPath.CIRCLE, scale: 8, fillColor: p.online ? '#22c55e' : '#94a3b8', fillOpacity: 1, strokeColor: '#fff', strokeWeight: 2, labelOrigin: new google.maps.Point(0, 3) }
      });
      mk.addListener('click', function () { openInfo(p, mk); });
      markers.push(mk); byId[p.serial] = mk; bounds.extend(mk.getPosition());
    });
    if (state.cluster && window.markerClusterer && markers.length > 1) {
      var accent = getComputedStyle(document.body).getPropertyValue('--accent').trim() || '#f9674e';
      clusterer = new markerClusterer.MarkerClusterer({ map: map, markers: markers, renderer: { render: function (c) {
        var size = c.count < 10 ? 34 : c.count < 100 ? 42 : 50, r = size / 2;
        var svg = '<svg xmlns="http://www.w3.org/2000/svg" width="' + size + '" height="' + size + '"><circle cx="' + r + '" cy="' + r + '" r="' + (r - 2) + '" fill="' + accent + '" stroke="#fff" stroke-width="2.5"/></svg>';
        return new google.maps.Marker({ position: c.position, icon: { url: 'data:image/svg+xml;charset=UTF-8,' + encodeURIComponent(svg), scaledSize: new google.maps.Size(size, size), anchor: new google.maps.Point(r, r) }, label: { text: String(c.count), color: '#fff', fontSize: '12.5px', fontWeight: '700' }, zIndex: 1000 + c.count });
      } } });
    }
    return bounds;
  }
  function fit(bounds) {
    if (!map || !markers.length) return;
    if (markers.length === 1) { map.setCenter(markers[0].getPosition()); map.setZoom(13); return; }
    google.maps.event.addListenerOnce(map, 'bounds_changed', function () { if (map.getZoom() > 15) map.setZoom(15); });
    map.fitBounds(bounds, 40);
  }
  function refresh(refit) {
    var list = visible();
    renderList(list);
    var b = renderMarkers(list);
    if (refit) fit(b);
  }

  window.fmInitMap = function () {
    var el = document.getElementById('fm-map');
    if (!el || !window.google) return;
    map = new google.maps.Map(el, { zoom: 2, center: { lat: 20, lng: 0 }, mapTypeControl: false, streetViewControl: false, fullscreenControl: true, keyboardShortcuts: false, gestureHandling: 'greedy' });
    iw = new google.maps.InfoWindow();
    window.addEventListener('mdm:themechange', applyStyle);
    applyStyle();
    refresh(true);
  };

  // Controls.
  function seg(id, key) {
    var host = document.getElementById(id);
    host.addEventListener('click', function (e) {
      var b = e.target.closest('button'); if (!b) return;
      Array.prototype.forEach.call(host.querySelectorAll('button'), function (x) { x.classList.remove('on'); });
      b.classList.add('on'); state[key] = b.getAttribute('data-v'); refresh(true);
    });
  }
  seg('fm-status', 'status'); seg('fm-product', 'product');
  document.getElementById('fm-site').addEventListener('change', function () { state.site = this.value; refresh(true); });
  document.getElementById('fm-style').addEventListener('change', function () { state.style = this.value; applyStyle(); });
  document.getElementById('fm-cluster').addEventListener('change', function () { state.cluster = this.checked; refresh(false); });
  document.getElementById('fm-labels').addEventListener('change', function () { state.labels = this.checked; refresh(false); });
  var qt; document.getElementById('fm-q').addEventListener('input', function () { clearTimeout(qt); var v = this.value; qt = setTimeout(function () { state.q = v; refresh(true); }, 120); });
  document.getElementById('fm-fit').addEventListener('click', function () { refresh(true); });

  renderList(visible());
})();
