// Loaded by templates/devices.html.
(function () {
  var pane = document.getElementById('fl-map-pane'), btn = document.getElementById('fl-map-btn');
  if (!pane || !btn) return;
  var map = null, markers = [], clusterMarks = [], regionMarks = [], awayLayers = [], awayOnly = false, awayLimit = 300, REGION_ZOOM = 5, clusterer = null, iw = null, loading = false, pending = false;
  var DARK = [{elementType:'geometry',stylers:[{color:'#212a37'}]},{elementType:'labels.text.fill',stylers:[{color:'#9ca5b3'}]},{elementType:'labels.text.stroke',stylers:[{color:'#1b2431'}]},{featureType:'poi',stylers:[{visibility:'off'}]},{featureType:'road',elementType:'geometry',stylers:[{color:'#2c3644'}]},{featureType:'road',elementType:'labels.text.fill',stylers:[{color:'#9ca5b3'}]},{featureType:'transit',stylers:[{visibility:'off'}]},{featureType:'water',elementType:'geometry',stylers:[{color:'#17212f'}]}];
  function styles(){ return (window.mdmTheme && window.mdmTheme()==='dark') ? DARK : []; }
  function esc(s){ return String(s==null?'':s).replace(/[&<>"']/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c];}); }
  function active(){ return !pane.hidden; }
  function ensureMaps(cb) {
    if (window.google && google.maps) { cb(); return; }
    if (window.__flMapsCb) { window.__flMapsCb.push(cb); return; }
    window.__flMapsCb = [cb];
    window.flMapsReady = function(){ var q = window.__flMapsCb || []; window.__flMapsCb = null; q.forEach(function(f){ f(); }); };
    if (!window.markerClusterer) { var c = document.createElement('script'); c.src = '/static/vendor/markerclusterer.min.js'; document.head.appendChild(c); }
    var s = document.createElement('script'); s.async = true;
    s.src = 'https://maps.googleapis.com/maps/api/js?key=' + window.FLEET_MAPS_KEY + '&callback=flMapsReady';
    document.head.appendChild(s);
  }
  function initMap() {
    if (map) return;
    map = new google.maps.Map(document.getElementById('fl-map'), { zoom: 2, center: {lat:20,lng:0}, mapTypeControl:false, streetViewControl:false, fullscreenControl:true, keyboardShortcuts:false, gestureHandling:'greedy', styles: styles() });
    iw = new google.maps.InfoWindow({ headerDisabled:true, maxWidth:300 });
    map.addListener('zoom_changed', applyZoomLayer);
    window.addEventListener('mdm:themechange', function(){ map.setOptions({styles: styles()}); if (last) render(last.pts, last.total, true); });
  }
  function cv(n){ return getComputedStyle(document.body).getPropertyValue(n).trim(); }
  function ago(iso){
    var t = Date.parse(iso); if (!t) return '';
    var m = Math.max(0, Math.round((Date.now() - t) / 60000));
    if (m < 2) return 'now'; if (m < 60) return m + ' min';
    var h = Math.round(m / 60); if (h < 48) return h + ' h';
    return Math.round(h / 24) + ' d';
  }
  // One blip per site: devices that share p.site (a restaurant, or a ~30 m cell) are one place.
  function groupSites(pts){
    var by = {}, order = [];
    pts.forEach(function(p){
      var k = p.site || p.serial, s = by[k];
      if (!s) { s = by[k] = { key:k, devs:[], lat:0, lon:0, on:0 }; order.push(k); }
      s.devs.push(p); s.lat += p.lat; s.lon += p.lon; if (p.online) s.on++;
    });
    return order.map(function(k){
      var s = by[k], f = s.devs[0];
      s.n = s.devs.length; s.lat /= s.n; s.lon /= s.n;
      s.name = f.restaurant || f.name; s.rid = f.restaurant_id || ''; s.addr = f.address || '';
      s.away = s.n === 1 && !!f.away;
      return s;
    });
  }
  // Ring = share online (red under 60%), number = devices. A lone device is a plain dot.
  function blipSvg(n, on, away){
    var ok = cv('--ok') || '#22c55e', bad = cv('--danger') || '#ef4444', grey = cv('--text-faint') || '#94a3b8', face = cv('--surface') || '#fff';
    var share = n ? on / n : 0;
    if (n === 1) {
      // Away from its venue: amber and a little bigger, so it reads as an exception.
      if (away) return { size:26, svg:'<svg xmlns="http://www.w3.org/2000/svg" width="26" height="26"><circle cx="13" cy="13" r="10" fill="'+(cv('--warn') || '#d97706')+'" stroke="'+face+'" stroke-width="2.5"/><rect x="11.8" y="7" width="2.4" height="8" rx="1.2" fill="#fff"/><circle cx="13" cy="18" r="1.4" fill="#fff"/></svg>' };
      return { size:20, svg:'<svg xmlns="http://www.w3.org/2000/svg" width="20" height="20"><circle cx="10" cy="10" r="7.5" fill="'+(on?ok:grey)+'" stroke="'+face+'" stroke-width="2.5"/></svg>' };
    }
    var size = n < 10 ? 46 : n < 100 ? 52 : 58, r = (size - 6) / 2, c = 2 * Math.PI * r, col = share < 0.6 ? bad : ok;
    return { size:size, svg:'<svg xmlns="http://www.w3.org/2000/svg" width="'+size+'" height="'+size+'" viewBox="0 0 '+size+' '+size+'"><circle cx="'+(size/2)+'" cy="'+(size/2)+'" r="'+(r+3)+'" fill="'+face+'"/><circle cx="'+(size/2)+'" cy="'+(size/2)+'" r="'+r+'" fill="none" stroke="'+grey+'" stroke-width="5"/><circle cx="'+(size/2)+'" cy="'+(size/2)+'" r="'+r+'" fill="none" stroke="'+col+'" stroke-width="5" stroke-dasharray="'+(c*share)+' '+c+'" transform="rotate(-90 '+(size/2)+' '+(size/2)+')"/></svg>' };
  }
  function blipIcon(n, on, label, away){
    var b = blipSvg(n, on, away), r = b.size / 2;
    var o = { icon:{ url:'data:image/svg+xml;charset=UTF-8,' + encodeURIComponent(b.svg), scaledSize:new google.maps.Size(b.size, b.size), anchor:new google.maps.Point(r, r) } };
    if (label) o.label = { text:String(n), color:cv('--text') || '#111', fontSize:'14px', fontWeight:'700' };
    return o;
  }
  function fmtDist(m){ return m < 1000 ? m + ' m' : m < 100000 ? (m / 1000).toFixed(1) + ' km' : Math.round(m / 1000).toLocaleString() + ' km'; }
  // Region chips, for when the map is zoomed out past where a site blip makes sense: one pill
  // per country with its device count, tinted by the share online.
  function groupRegions(pts){
    var by = {}, order = [];
    pts.forEach(function(p){
      var k = p.region || 'Elsewhere', r = by[k];
      if (!r) { r = by[k] = { name:k, n:0, on:0, away:0, lat:0, lon:0, b:new google.maps.LatLngBounds() }; order.push(k); }
      r.n++; r.lat += p.lat; r.lon += p.lon; if (p.online) r.on++; if (p.away) r.away++;
      r.b.extend({ lat:p.lat, lng:p.lon });
    });
    return order.map(function(k){ var r = by[k]; r.lat /= r.n; r.lon /= r.n; return r; });
  }
  function regionIcon(r){
    var ok = cv('--ok') || '#22c55e', bad = cv('--danger') || '#ef4444', face = cv('--surface') || '#fff', line = cv('--border') || '#ccc', txt = cv('--text') || '#111', warn = cv('--warn') || '#d97706';
    var col = r.on / r.n < 0.6 ? bad : ok, w = Math.round(52 + r.name.length * 6.8), h = 34;
    var svg = '<svg xmlns="http://www.w3.org/2000/svg" width="'+w+'" height="'+h+'"><rect x="1" y="1" width="'+(w-2)+'" height="'+(h-2)+'" rx="16" fill="'+face+'" stroke="'+line+'" stroke-width="1.5"/>'
      + '<circle cx="17" cy="17" r="12" fill="'+col+'" fill-opacity=".38"/><text x="17" y="21.5" text-anchor="middle" font-family="Poppins,Inter,Arial,sans-serif" font-size="12.5" font-weight="700" fill="'+txt+'">'+r.n+'</text>'
      + '<text x="36" y="21.5" font-family="Poppins,Inter,Arial,sans-serif" font-size="12.5" font-weight="600" fill="'+txt+'">'+esc(r.name)+'</text>'
      + (r.away ? '<circle cx="'+(w-5)+'" cy="6" r="5.5" fill="'+warn+'" stroke="'+face+'" stroke-width="2"/>' : '') + '</svg>';
    return { url:'data:image/svg+xml;charset=UTF-8,' + encodeURIComponent(svg), scaledSize:new google.maps.Size(w, h), anchor:new google.maps.Point(w / 2, h / 2) };
  }
  // Region chips replace the site blips while zoomed out; the away-from-venue lines show with the blips.
  function applyZoomLayer(){
    if (!map) return;
    var reg = regionMarks.length > 0 && map.getZoom() <= REGION_ZOOM;
    regionMarks.forEach(function(m){ m.setMap(reg ? map : null); });
    if (clusterer) clusterer.setMap(reg ? null : map); else markers.forEach(function(m){ m.setMap(reg ? null : map); });
    awayLayers.forEach(function(l){ l.setMap(reg ? null : map); });
  }
  // A dashed line from an away device to its venue, and a square where the venue is.
  function drawAway(p){
    var warn = cv('--warn') || '#d97706', face = cv('--surface') || '#fff', txt = cv('--text') || '#111';
    var from = { lat:p.lat, lng:p.lon }, to = { lat:p.venue_lat, lng:p.venue_lon };
    awayLayers.push(new google.maps.Polyline({ path:[from, to], strokeOpacity:0, clickable:false, map:map,
      icons:[{ icon:{ path:'M 0,-1 0,1', strokeOpacity:1, strokeColor:warn, strokeWeight:2, scale:3 }, offset:'0', repeat:'11px' }] }));
    var sq = '<svg xmlns="http://www.w3.org/2000/svg" width="18" height="18"><rect x="2" y="2" width="14" height="14" rx="3.5" fill="'+txt+'" stroke="'+face+'" stroke-width="2"/></svg>';
    awayLayers.push(new google.maps.Marker({ position:to, map:map, clickable:false, title:(p.restaurant || 'Venue') + ' (venue)', zIndex:1,
      icon:{ url:'data:image/svg+xml;charset=UTF-8,' + encodeURIComponent(sq), scaledSize:new google.maps.Size(18, 18), anchor:new google.maps.Point(9, 9) } }));
  }
  window.flAwayToggle = function(){ awayOnly = !awayOnly; if (last) render(last.pts, last.total, false); };
  function popup(s){
    if (s.n === 1) {
      var p = s.devs[0], pct = Math.max(0, Math.min(100, p.battery || 0));
      return '<div class="fm-card"><div class="fm-h"><div><b class="fm-sn" style="font-size:13px">'+esc(p.serial)+'</b><div class="fm-sub">'+esc(p.name)+(p.address?' · '+esc(p.address):'')+'</div></div><span class="fm-pill'+(p.online?'':' off')+'">'+(p.online?'Online':'Offline')+'</span></div>'
        + '<div class="fm-body">'+(p.battery?'<div class="fm-meter"><i style="width:'+pct+'%"></i></div><div class="fm-row">Battery<b>'+pct+'%</b></div>':'')
        + (p.product?'<div class="fm-row">Model<b>'+esc(p.product)+'</b></div>':'') + (p.build?'<div class="fm-row">Build<b>'+esc(p.build)+'</b></div>':'')
        + '<div class="fm-row">Last seen<b>'+esc(ago(p.last_seen))+'</b></div>'
        + (p.away ? '<div class="fm-row" style="color:var(--warn)">Away from venue<b style="color:var(--warn)">'+fmtDist(p.away_m)+'</b></div><div class="fm-sub">from '+esc(p.restaurant || 'its venue')+'</div>'
          : p.away_m != null ? '<div class="fm-row">From venue<b>'+fmtDist(p.away_m)+'</b></div>' : '') + '</div>'
        + '<div class="fm-acts"><a class="p" href="/devices/'+encodeURIComponent(p.serial)+'">Open device</a><button type="button" data-fm="close">Close</button></div></div>';
    }
    var devs = s.devs.slice().sort(function(a, b){ return (a.online - b.online) || a.serial.localeCompare(b.serial); });
    var rows = devs.slice(0, 8).map(function(p){
      return '<a href="/devices/'+encodeURIComponent(p.serial)+'"><i class="'+(p.online?'':'off')+'"></i><span class="fm-sn">'+esc(p.serial)+'</span><span class="fm-m">'+(p.battery?p.battery+'% · ':'')+esc(ago(p.last_seen))+'</span></a>';
    }).join('');
    var more = devs.length > 8 ? '<div class="fm-more">+ '+(devs.length - 8)+' more</div>' : '';
    var acts = (s.rid ? '<a class="p" href="/restaurants/'+encodeURIComponent(s.rid)+'">Open restaurant</a><button type="button" data-fm="filter" data-rid="'+esc(s.rid)+'">Filter list to this</button>' : '<button type="button" data-fm="close">Close</button>');
    return '<div class="fm-card"><div class="fm-h"><div><b>'+esc(s.name)+'</b><div class="fm-sub">'+(s.addr?esc(s.addr)+' · ':'')+s.n+' devices · '+s.on+' online</div></div><button type="button" class="fm-x" data-fm="close" aria-label="Close">✕</button></div><div class="fm-list">'+rows+'</div>'+more+'<div class="fm-acts">'+acts+'</div></div>';
  }
  pane.addEventListener('click', function(e){
    var t = e.target.closest && e.target.closest('[data-fm]'); if (!t) return;
    if (t.dataset.fm === 'close' && iw) iw.close();
    if (t.dataset.fm === 'filter' && typeof navigateDevices === 'function') { if (iw) iw.close(); navigateDevices({ page:'1', restaurant:t.dataset.rid }); }
  });
  // Mirror the list's filter chips (plus the rail's restaurant/group, which has no chip there)
  // above the map; their buttons call the same clearOne/clearFilters the list uses.
  function syncChips(){
    var bar = document.getElementById('fl-map-bar'), box = document.getElementById('fl-map-chips');
    var src = document.getElementById('fl-chip-list');
    var html = '';
    var v = (typeof getFilterVals === 'function') ? getFilterVals() : {}, kind = v.restaurant ? 'restaurant' : v.group ? 'group' : '', id = v.restaurant || v.group || '';
    var a = kind ? document.querySelector('.fleet-rail-list[data-sec="'+kind+'s"] a[href*="'+kind+'='+id+'"]') : null;
    var nm = a && a.closest('.fleet-rail-item') && a.closest('.fleet-rail-item').querySelector('.nm');
    if (nm) html += '<span class="fchip"><span class="k">'+(kind === 'group' ? 'Group' : 'Restaurant')+'</span> '+esc(nm.textContent.trim())+' <button type="button" class="x" onclick="navigateDevices({page:\'1\',restaurant:\'\',group:\'\'})" aria-label="Clear collection">×</button></span>';
    if (src) html += src.innerHTML;
    var na = last ? last.pts.filter(function(p){ return p.away; }).length : 0;
    if (last) html += '<button type="button" class="fchip fl-away'+(na ? ' has' : '')+(awayOnly ? ' on' : '')+'" onclick="flAwayToggle()" title="Devices further than '+awayLimit+' m from their venue"><span class="k">Away from venue</span> <b>'+na+'</b>'+(awayOnly ? ' <span class="x" aria-hidden="true">×</span>' : '')+'</button>';
    box.innerHTML = html;
    box.hidden = !html;
    bar.hidden = !html && !document.getElementById('fl-map-un').textContent;
  }
  function syncUnlocated(missing){
    var un = document.getElementById('fl-map-un');
    var noVenue = last ? last.pts.filter(function(p){ return !p.restaurant_id; }).length : 0;
    un.innerHTML = (missing > 0 ? missing + ' without a location <button type="button" onclick="flToggleMap(false)">Back to list</button>' : '') + (noVenue ? (missing > 0 ? ' · ' : '') + noVenue + ' not at a venue' : '');
    syncChips();
  }
  var last = null;
  function render(pts, total, keepView) {
    last = { pts:pts, total:total };
    syncUnlocated(Math.max(0, total - pts.length));
    var nAway = pts.filter(function(p){ return p.away; }).length;
    if (awayOnly) pts = pts.filter(function(p){ return p.away; });
    // Tear the old clusterer down completely: clearMarkers() alone leaves its drawn cluster
    // markers on the map, and a live refresh then stacks a new count over the old one ("35" on "36").
    if (clusterer) { clusterer.clearMarkers(); clusterer.setMap(null); clusterer = null; }
    clusterMarks.forEach(function(m){ m.setMap(null); }); clusterMarks = [];
    markers.forEach(function(m){ m.setMap(null); }); markers = [];
    regionMarks.forEach(function(m){ m.setMap(null); }); regionMarks = [];
    awayLayers.forEach(function(l){ l.setMap(null); }); awayLayers = [];
    if (iw) iw.close();
    var old = pane.querySelector('.fl-map-empty'); if (old) old.remove();
    var sites = groupSites(pts);
    document.getElementById('fl-map-count').textContent = pts.length + ' of ' + total + ' located' + (sites.length < pts.length ? ' · ' + sites.length + ' sites' : '') + (nAway && !awayOnly ? ' · ' + nAway + ' away' : '');
    if (!pts.length) {
      var e = document.createElement('div'); e.className = 'fl-map-empty';
      e.innerHTML = awayOnly ? '<b>No devices are away from their venue</b>Every located device is within the limit of its venue, or has no venue.' : '<b>No located devices in this view</b>Devices appear once the geolocation pipeline resolves their position.';
      pane.querySelector('.fl-map').appendChild(e); return;
    }
    var bounds = new google.maps.LatLngBounds();
    sites.forEach(function(s){
      var o = blipIcon(s.n, s.on, s.n > 1, s.away);
      var mk = new google.maps.Marker({ position:{lat:s.lat,lng:s.lon}, map:map, title:s.name + (s.n > 1 ? ' · ' + s.n + ' devices' : s.away ? ' · away from venue' : ''), icon:o.icon, label:o.label, optimized:false, zIndex:s.away ? 900 : undefined });
      mk.nDev = s.n; mk.nOn = s.on;
      mk.addListener('click', function(){ iw.setContent(popup(s)); iw.open(map, mk); });
      markers.push(mk); bounds.extend(mk.getPosition());
      if (s.away) { var d0 = s.devs[0]; drawAway(d0); bounds.extend({ lat:d0.venue_lat, lng:d0.venue_lon }); }
    });
    groupRegions(pts).forEach(function(r){
      var rm = new google.maps.Marker({ position:{lat:r.lat,lng:r.lon}, icon:regionIcon(r), title:r.name + ' · ' + r.n + ' devices', zIndex:2000 });
      rm.addListener('click', function(){ google.maps.event.addListenerOnce(map, 'bounds_changed', function(){ if (map.getZoom() > 15) map.setZoom(15); }); map.fitBounds(r.b, 70); });
      regionMarks.push(rm);
    });
    if (window.markerClusterer && markers.length > 1) {
      // A cluster of sites is still devices: sum them so the number never changes with zoom.
      clusterer = new markerClusterer.MarkerClusterer({ map:map, markers:markers, renderer:{ render:function(c){
        var n = 0, on = 0; c.markers.forEach(function(m){ n += m.nDev; on += m.nOn; });
        var o = blipIcon(n, on, true);
        var cm = new google.maps.Marker({ position:c.position, icon:o.icon, label:o.label, zIndex:1000 + n });
        clusterMarks.push(cm);
        return cm;
      } } });
    }
    if (keepView) { applyZoomLayer(); return; }
    if (markers.length === 1 && !awayLayers.length) { map.setCenter(markers[0].getPosition()); map.setZoom(13); }
    else { google.maps.event.addListenerOnce(map, 'bounds_changed', function(){ if (map.getZoom() > 15) map.setZoom(15); applyZoomLayer(); }); map.fitBounds(bounds, 40); }
    applyZoomLayer();
  }
  function load() {
    if (!active()) return;
    syncChips();
    if (loading) { pending = true; return; }
    loading = true;
    var vals = (typeof getFilterVals === 'function') ? getFilterVals() : {};
    var qs = new URLSearchParams(vals); qs.delete('page'); qs.delete('page_size');
    fetch('/devices/map.json?' + qs.toString(), { credentials:'same-origin', headers:{ 'Accept':'application/json' } })
      .then(function(r){ return r.json(); })
      .then(function(d){ awayLimit = d.away_limit_m || 300; ensureMaps(function(){ initMap(); render(d.points || [], d.total || 0); }); })
      .catch(function(){ document.getElementById('fl-map-count').textContent = 'Could not load map data'; })
      .then(function(){ loading = false; if (pending) { pending = false; load(); } });
  }
  window.flToggleMap = function (force) {
    var on = (typeof force === 'boolean') ? force : pane.hidden;
    pane.hidden = !on;
    var split = document.getElementById('dv-split'); if (split) { split.hidden = on; split.style.display = on ? 'none' : ''; }
    btn.setAttribute('aria-pressed', String(on));
    btn.querySelector('.lbl').textContent = on ? 'Back to list' : 'Map view';
    btn.querySelector('.i-map').hidden = on; btn.querySelector('.i-list').hidden = !on;
    btn.title = on ? 'Show the device list again' : 'Show the devices in this view on a map';
    document.querySelectorAll('#appbar-island [data-fl-map]').forEach(function (b) { b.textContent = on ? 'Back to list' : 'Map view'; b.setAttribute('aria-pressed', String(on)); });
    try { localStorage.setItem('mdm-fleet-view', on ? 'map' : 'list'); } catch (e) {}
    if (on) load();
  };
  // Re-fetch when the roster re-renders (filter, quick view, rail scope, live poll).
  document.body.addEventListener('htmx:afterSwap', function (evt) {
    if (evt.detail && evt.detail.target && evt.detail.target.id === 'device-table-container') load();
  });
  // The roster swaps before history.pushState, so the afterSwap load above can read the
  // previous scope from location.search; this one runs once the URL is the new one.
  document.body.addEventListener('fleet:scoped', load);
  try { if (localStorage.getItem('mdm-fleet-view') === 'map') window.flToggleMap(true); } catch (e) {}
})();
