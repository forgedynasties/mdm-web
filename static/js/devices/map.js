// Loaded by templates/devices.html. Fleet's map (MP1 "Canvas", picked 5 Nov 2026): our own
// MapLibre map (static/js/map-canvas.js) instead of Google. A venue is one pill (online ring,
// device count, name); pills that would overlap merge into a count; zoomed out to a continent
// they become one chip per country. A floating card lists what is on screen and names the
// venues off it. The List · Map switch sits beside the search.
(function () {
  var pane = document.getElementById('fl-map-pane');
  if (!pane) return;
  var map = null, marks = [], popup = null, last = null, awayOnly = false, awayLimit = 300,
      loading = false, pending = false, fitted = false, REGION_ZOOM = 4.2;
  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function active() { return !pane.hidden; }
  function cv(n) { return getComputedStyle(document.body).getPropertyValue(n).trim(); }
  function ago(iso) {
    var t = Date.parse(iso); if (!t) return '';
    var m = Math.max(0, Math.round((Date.now() - t) / 60000));
    if (m < 2) return 'now'; if (m < 60) return m + ' min';
    var h = Math.round(m / 60); if (h < 48) return h + ' h';
    return Math.round(h / 24) + ' d';
  }
  function fmtDist(m) { return m < 1000 ? m + ' m' : m < 100000 ? (m / 1000).toFixed(1) + ' km' : Math.round(m / 1000).toLocaleString() + ' km'; }
  // One place per site: devices that share p.site (a restaurant, or a ~30 m cell).
  function groupSites(pts) {
    var by = {}, order = [];
    pts.forEach(function (p) {
      var k = p.site || p.serial, s = by[k];
      if (!s) { s = by[k] = { key: k, devs: [], lat: 0, lon: 0, on: 0 }; order.push(k); }
      s.devs.push(p); s.lat += p.lat; s.lon += p.lon; if (p.online) s.on++;
    });
    return order.map(function (k) {
      var s = by[k], f = s.devs[0];
      s.n = s.devs.length; s.lat /= s.n; s.lon /= s.n;
      s.name = f.restaurant || (s.n === 1 ? f.serial : 'Lab' + (f.address ? ' · ' + f.address.split(',')[0] : '')); s.rid = f.restaurant_id || ''; s.addr = f.address || '';
      s.away = s.n === 1 && !!f.away;
      return s;
    });
  }
  function groupRegions(pts) {
    var by = {}, order = [];
    pts.forEach(function (p) {
      var k = p.region || 'Elsewhere', r = by[k];
      if (!r) { r = by[k] = { name: k, n: 0, on: 0, lat: 0, lon: 0, b: [[p.lon, p.lat], [p.lon, p.lat]] }; order.push(k); }
      r.n++; r.lat += p.lat; r.lon += p.lon; if (p.online) r.on++;
      r.b[0][0] = Math.min(r.b[0][0], p.lon); r.b[0][1] = Math.min(r.b[0][1], p.lat);
      r.b[1][0] = Math.max(r.b[1][0], p.lon); r.b[1][1] = Math.max(r.b[1][1], p.lat);
    });
    return order.map(function (k) { var r = by[k]; r.lat /= r.n; r.lon /= r.n; return r; });
  }
  function bboxOf(list) {
    var b = [[180, 90], [-180, -90]];
    list.forEach(function (s) { b[0][0] = Math.min(b[0][0], s.lon); b[0][1] = Math.min(b[0][1], s.lat); b[1][0] = Math.max(b[1][0], s.lon); b[1][1] = Math.max(b[1][1], s.lat); });
    return b;
  }
  // The ring is the share online; under 60 % it turns red.
  function ring(n, on) { var p = n ? Math.round(on / n * 100) : 0; return '<span class="mm-ring' + (n && on / n < 0.6 ? ' bad' : '') + '" style="--p:' + p + '%"></span>'; }
  function cut(s, n) { s = String(s || ''); return s.length > n ? s.slice(0, n - 1) + '…' : s; }
  function popupHtml(s) {
    if (s.n === 1) {
      var p = s.devs[0], pct = Math.max(0, Math.min(100, p.battery || 0));
      return '<div class="fm-card"><div class="fm-h"><div><b class="fm-sn" style="font-size:13px">' + esc(p.serial) + '</b><div class="fm-sub">' + esc(p.name) + (p.address ? ' · ' + esc(p.address) : '') + '</div></div><span class="fm-pill' + (p.online ? '' : ' off') + '">' + (p.online ? 'Online' : 'Offline') + '</span></div>'
        + '<div class="fm-body">' + (p.battery ? '<div class="fm-meter"><i style="width:' + pct + '%"></i></div><div class="fm-row">Battery<b>' + pct + '%</b></div>' : '')
        + (p.product ? '<div class="fm-row">Model<b>' + esc(p.product) + '</b></div>' : '') + (p.build ? '<div class="fm-row">Build<b>' + esc(p.build) + '</b></div>' : '')
        + '<div class="fm-row">Last seen<b>' + esc(ago(p.last_seen)) + '</b></div>'
        + (p.away ? '<div class="fm-row" style="color:var(--warn)">Away from venue<b style="color:var(--warn)">' + fmtDist(p.away_m) + '</b></div><div class="fm-sub">from ' + esc(p.restaurant || 'its venue') + '</div>'
          : p.away_m != null ? '<div class="fm-row">From venue<b>' + fmtDist(p.away_m) + '</b></div>' : '') + '</div>'
        + '<div class="fm-acts"><a class="p" href="/devices/' + encodeURIComponent(p.serial) + '">Open device</a><button type="button" data-fm="close">Close</button></div></div>';
    }
    var devs = s.devs.slice().sort(function (a, b) { return (a.online - b.online) || a.serial.localeCompare(b.serial); });
    var rows = devs.slice(0, 8).map(function (p) {
      return '<a href="/devices/' + encodeURIComponent(p.serial) + '"><i class="' + (p.online ? '' : 'off') + '"></i><span class="fm-sn">' + esc(p.serial) + '</span><span class="fm-m">' + (p.battery ? p.battery + '% · ' : '') + esc(ago(p.last_seen)) + '</span></a>';
    }).join('');
    var more = devs.length > 8 ? '<div class="fm-more">+ ' + (devs.length - 8) + ' more</div>' : '';
    var acts = s.rid ? '<a class="p" href="/restaurants/' + encodeURIComponent(s.rid) + '">Open venue</a><button type="button" data-fm="filter" data-rid="' + esc(s.rid) + '">Filter list to this</button>' : '<button type="button" data-fm="close">Close</button>';
    return '<div class="fm-card"><div class="fm-h"><div><b>' + esc(s.name) + '</b><div class="fm-sub">' + (s.addr ? esc(s.addr) + ' · ' : '') + s.n + ' devices · ' + s.on + ' online</div></div><button type="button" class="fm-x" data-fm="close" aria-label="Close">✕</button></div><div class="fm-list">' + rows + '</div>' + more + '<div class="fm-acts">' + acts + '</div></div>';
  }
  function openPopup(s) {
    if (popup) popup.remove();
    popup = new maplibregl.Popup({ closeButton: false, maxWidth: '300px', offset: 16, className: 'fm-pop' })
      .setLngLat([s.lon, s.lat]).setHTML(popupHtml(s)).addTo(map);
  }
  pane.addEventListener('click', function (e) {
    var t = e.target.closest && e.target.closest('[data-fm]'); if (!t) return;
    if (t.dataset.fm === 'close' && popup) popup.remove();
    if (t.dataset.fm === 'filter' && typeof navigateDevices === 'function') { if (popup) popup.remove(); navigateDevices({ page: '1', restaurant: t.dataset.rid }); }
  });
  function mark(lon, lat, html, cls, onClick, z) {
    var el = document.createElement('button');
    el.type = 'button'; el.className = cls; el.innerHTML = html;
    if (z) el.style.zIndex = z;
    el.addEventListener('click', function (e) { e.stopPropagation(); onClick(); });
    var m = new maplibregl.Marker({ element: el, anchor: 'center' }).setLngLat([lon, lat]).addTo(map);
    marks.push(m);
    return m;
  }
  function sitesNow() {
    if (!last) return [];
    var pts = awayOnly ? last.pts.filter(function (p) { return p.away; }) : last.pts;
    return groupSites(pts);
  }
  // Pills that would overlap on screen merge into one count, biggest first.
  function cluster(sites) {
    var out = [];
    sites.slice().sort(function (a, b) { return b.n - a.n; }).forEach(function (s) {
      var p = map.project([s.lon, s.lat]), hit = null;
      for (var i = 0; i < out.length; i++) { var c = out[i]; if (Math.abs(c.px.x - p.x) < 70 && Math.abs(c.px.y - p.y) < 30) { hit = c; break; } }
      if (hit) { hit.sites.push(s); hit.n += s.n; hit.on += s.on; }
      else out.push({ px: p, sites: [s], n: s.n, on: s.on, lat: s.lat, lon: s.lon });
    });
    return out;
  }
  function draw() {
    marks.forEach(function (m) { m.remove(); }); marks = [];
    if (!map || !last) return;
    var sites = sitesNow();
    if (map.getZoom() <= REGION_ZOOM) {
      groupRegions(awayOnly ? last.pts.filter(function (p) { return p.away; }) : last.pts).forEach(function (r) {
        mark(r.lon, r.lat, ring(r.n, r.on) + '<b>' + r.n + '</b><span>' + esc(r.name) + '</span>', 'mm-pill', function () {
          map.fitBounds(r.b, { padding: 80, maxZoom: 13 });
        });
      });
    } else {
      cluster(sites).forEach(function (c) {
        if (c.sites.length > 1) {
          mark(c.lon, c.lat, ring(c.n, c.on) + '<b>' + c.n + '</b><span>' + c.sites.length + ' places</span>', 'mm-pill', function () {
            map.fitBounds(bboxOf(c.sites), { padding: 90, maxZoom: 15 });
          }, 2);
          return;
        }
        var s = c.sites[0];
        if (s.n === 1) {
          var p = s.devs[0];
          mark(s.lon, s.lat, '', 'mm-dot' + (s.away ? ' away' : p.online ? '' : ' off'), function () { openPopup(s); }, s.away ? 3 : 1)
            .getElement().title = p.serial + (s.away ? ' · away from its venue' : '');
          return;
        }
        mark(s.lon, s.lat, ring(s.n, s.on) + '<b>' + s.n + '</b><span>' + esc(cut(s.name, 22)) + '</span>', 'mm-pill', function () { openPopup(s); }, 2);
      });
    }
    listCard(sites);
  }
  // The floating card: places on screen (most devices first), then the ones off it.
  function listCard(sites) {
    var rows = document.getElementById('fl-mapl-rows'), away = document.getElementById('fl-mapl-away');
    if (!rows || !map) return;
    var b = map.getBounds(), inV = [], outV = [];
    sites.forEach(function (s) { (b.contains([s.lon, s.lat]) ? inV : outV).push(s); });
    inV.sort(function (a, b2) { return b2.n - a.n; }); outV.sort(function (a, b2) { return b2.n - a.n; });
    var head = document.getElementById('fl-mapl-h');
    if (head) head.textContent = inV.length ? 'On the map · ' + inV.length + ' place' + (inV.length === 1 ? '' : 's') : 'Nothing on this part of the map';
    rows.innerHTML = inV.slice(0, 5).map(function (s, i) {
      return '<button type="button" class="r" data-i="' + i + '">' + ring(s.n, s.on) + '<b>' + esc(cut(s.name, 26)) + '</b><small>' + s.n + (s.n > 1 ? ' · ' + s.on + ' on' : s.on ? ' · online' : ' · offline') + '</small></button>';
    }).join('') + (inV.length > 5 ? '<div class="more">+ ' + (inV.length - 5) + ' more here</div>' : '');
    away.innerHTML = outV.slice(0, 2).map(function (s, i) {
      return '<button type="button" data-o="' + i + '">' + esc(cut(s.name, 18)) + ' ' + s.n + ' ›</button>';
    }).join('') + (outV.length > 2 ? '<button type="button" data-all="1">+' + (outV.length - 2) + '</button>' : '');
    away.hidden = !outV.length;
    rows.onclick = function (e) { var t = e.target.closest('[data-i]'); if (!t) return; var s = inV[+t.dataset.i]; map.flyTo({ center: [s.lon, s.lat], zoom: Math.max(map.getZoom(), 13) }); openPopup(s); };
    away.onclick = function (e) {
      var t = e.target.closest('button'); if (!t) return;
      if (t.dataset.all) { fitAll(); return; }
      var s = outV[+t.dataset.o]; map.flyTo({ center: [s.lon, s.lat], zoom: 13 });
    };
  }
  // A dashed line from an away device to its venue, and a square where the venue is.
  function drawAway() {
    if (!map || !map.isStyleLoaded()) return;
    var feats = [];
    (last ? last.pts : []).forEach(function (p) {
      if (!p.away || p.venue_lat == null) return;
      feats.push({ type: 'Feature', geometry: { type: 'LineString', coordinates: [[p.lon, p.lat], [p.venue_lon, p.venue_lat]] } });
      feats.push({ type: 'Feature', geometry: { type: 'Point', coordinates: [p.venue_lon, p.venue_lat] } });
    });
    var data = { type: 'FeatureCollection', features: feats };
    if (map.getSource('away')) { map.getSource('away').setData(data); return; }
    map.addSource('away', { type: 'geojson', data: data });
    map.addLayer({ id: 'away-line', type: 'line', source: 'away', filter: ['==', '$type', 'LineString'],
      paint: { 'line-color': cv('--warn') || '#d97706', 'line-width': 2, 'line-dasharray': [2, 2] } });
    map.addLayer({ id: 'away-venue', type: 'circle', source: 'away', filter: ['==', '$type', 'Point'],
      paint: { 'circle-radius': 6, 'circle-color': cv('--text') || '#111', 'circle-stroke-color': '#fff', 'circle-stroke-width': 2 } });
    // Zoomed out to countries the lines would cross oceans; they show with the venue pills.
    ['away-line', 'away-venue'].forEach(function (id) { map.setLayerZoomRange(id, REGION_ZOOM, 24); });
  }
  window.flAwayToggle = function () { awayOnly = !awayOnly; syncChips(); draw(); };
  // The list's filter chips (plus the rail's venue/group) above the map.
  function syncChips() {
    var bar = document.getElementById('fl-map-bar'), box = document.getElementById('fl-map-chips');
    var src = document.getElementById('fl-chip-list');
    var html = '';
    var v = (typeof getFilterVals === 'function') ? getFilterVals() : {}, kind = v.restaurant ? 'restaurant' : v.group ? 'group' : '', id = v.restaurant || v.group || '';
    var a = kind ? document.querySelector('.fleet-rail-list[data-sec="' + kind + 's"] a[href*="' + kind + '=' + id + '"]') : null;
    var nm = a && a.closest('.fleet-rail-item') && a.closest('.fleet-rail-item').querySelector('.nm');
    if (nm) html += '<span class="fchip"><span class="k">' + (kind === 'group' ? 'Group' : 'Restaurant') + '</span> ' + esc(nm.textContent.trim()) + ' <button type="button" class="x" onclick="navigateDevices({page:\'1\',restaurant:\'\',group:\'\'})" aria-label="Clear collection">×</button></span>';
    if (src) html += src.innerHTML;
    var na = last ? last.pts.filter(function (p) { return p.away; }).length : 0;
    if (na || awayOnly) html += '<button type="button" class="fchip fl-away has' + (awayOnly ? ' on' : '') + '" onclick="flAwayToggle()" title="Devices further than ' + awayLimit + ' m from their venue"><span class="k">Away from venue</span> <b>' + na + '</b>' + (awayOnly ? ' <span class="x" aria-hidden="true">×</span>' : '') + '</button>';
    box.innerHTML = html;
    box.hidden = !html;
    var un = document.getElementById('fl-map-un');
    bar.hidden = !html && !(un && un.textContent);
  }
  function fitAll() {
    var sites = sitesNow();
    if (!sites.length) return;
    var pts = sites.slice();
    (last ? last.pts : []).forEach(function (p) { if (p.away && p.venue_lat != null) pts.push({ lon: p.venue_lon, lat: p.venue_lat }); });
    if (pts.length === 1) { map.jumpTo({ center: [pts[0].lon, pts[0].lat], zoom: 13 }); return; }
    map.fitBounds(bboxOf(pts), { padding: { top: 70, bottom: 50, left: 320, right: 60 }, maxZoom: 13, duration: 0 });
  }
  function render(pts, total, keepView) {
    last = { pts: pts, total: total };
    var missing = Math.max(0, total - pts.length), noVenue = pts.filter(function (p) { return !p.restaurant_id; }).length;
    var un = document.getElementById('fl-map-un');
    un.innerHTML = (missing ? missing + ' without a location' : '') + (noVenue ? (missing ? ' · ' : '') + noVenue + ' not at a venue' : '');
    document.getElementById('fl-map-count').textContent = pts.length + ' of ' + total + ' located';
    syncChips();
    if (popup) popup.remove();
    var empty = pane.querySelector('.fl-map-empty'); if (empty) empty.remove();
    if (!pts.length) {
      var e = document.createElement('div'); e.className = 'fl-map-empty';
      e.innerHTML = '<b>No located devices in this view</b>Devices appear once their Wi-Fi gives a position.';
      pane.querySelector('.fl-map').appendChild(e);
    }
    drawAway();
    if (!keepView || !fitted) { fitAll(); fitted = true; }
    draw();
  }
  // Open, the map fills the window down to the dock and the page does not scroll (5 Nov 2026,
  // user: "the map shouldnt be hidden by nav dock. and it should not be scrollable").
  function fitPane() {
    var root = document.documentElement, box = pane.querySelector('.fl-map');
    if (!active()) { root.classList.remove('fl-map-on'); box.style.height = ''; return; }
    root.classList.add('fl-map-on');
    if (window.innerWidth <= 720) { box.style.height = ''; return; }
    window.scrollTo(0, 0);
    var dock = document.querySelector('nav.dock'), dr = dock && dock.getBoundingClientRect();
    var bottom = dr && dr.height ? dr.top - 14 : window.innerHeight - 16;
    box.style.height = Math.max(320, Math.floor(bottom - box.getBoundingClientRect().top)) + 'px';
    if (map) map.resize();
  }
  window.addEventListener('resize', function () { if (active()) fitPane(); });
  // The loading veil over the map area: a line saying what is happening, gone once drawn.
  function loadState(t) {
    var el = document.getElementById('fl-map-load'); if (!el) return;
    if (t) { document.getElementById('fl-map-load-t').textContent = t; el.classList.remove('done'); }
    else el.classList.add('done');
  }
  var waiters = null;
  function initMap(cb) {
    if (map && !waiters) { cb(); return; }
    if (waiters) { waiters.push(cb); return; }
    waiters = [cb];
    map = mdmMap.create(document.getElementById('fl-map'), { nav: true });
    map.on('load', function () { var q = waiters; waiters = null; q.forEach(function (f) { f(); }); });
    map.on('style.load', function () { drawAway(); });
    map.on('moveend', draw);
  }
  function load() {
    if (!active()) return;
    syncChips();
    if (loading) { pending = true; return; }
    loading = true;
    var vals = (typeof getFilterVals === 'function') ? getFilterVals() : {};
    var qs = new URLSearchParams(vals); qs.delete('page'); qs.delete('page_size');
    var keep = !!map, ready = null, data = null;
    if (!keep) loadState('Loading the map…');
    function go() {
      if (!ready || !data) return;
      if (!keep) loadState('Placing ' + (data.points || []).length + ' devices…');
      render(data.points || [], data.total || 0, keep);
      map.once('idle', function () { loadState(''); });
    }
    // The map draws while the devices load, rather than after.
    mdmMap.load(function () { initMap(function () { map.resize(); ready = true; go(); }); });
    fetch('/devices/map.json?' + qs.toString(), { credentials: 'same-origin', headers: { 'Accept': 'application/json' } })
      .then(function (r) { return r.json(); })
      .then(function (d) { awayLimit = d.away_limit_m || 300; data = d; if (!ready && !keep) loadState('Drawing the map…'); go(); })
      .catch(function () { document.getElementById('fl-map-count').textContent = 'Could not load map data'; loadState(''); })
      .then(function () { loading = false; if (pending) { pending = false; load(); } });
  }
  window.flToggleMap = function (force) {
    var on = (typeof force === 'boolean') ? force : pane.hidden;
    pane.hidden = !on;
    var split = document.getElementById('dv-split'); if (split) { split.hidden = on; split.style.display = on ? 'none' : ''; }
    document.querySelectorAll('[data-fl-view]').forEach(function (b) {
      var me = (b.dataset.flView === 'map') === on;
      b.classList.toggle('on', me); b.setAttribute('aria-pressed', String(me));
    });
    try { localStorage.setItem('mdm-fleet-view', on ? 'map' : 'list'); } catch (e) {}
    fitPane();
    if (on) { fitted = false; load(); }
  };
  // Leaving Fleet (htmx swap) frees the page again.
  document.addEventListener('htmx:beforeSwap', function (e) {
    var t = e.detail && e.detail.target; if (t && t.contains(pane)) document.documentElement.classList.remove('fl-map-on');
  });
  // MapLibre is big: fetch it while the list is on screen, so the map opens quickly.
  (window.requestIdleCallback || function (f) { setTimeout(f, 1500); })(function () { if (window.mdmMap) mdmMap.load(function () {}); });
  // Re-fetch when the roster re-renders (filter, quick view, rail scope, live poll).
  document.body.addEventListener('htmx:afterSwap', function (evt) {
    if (evt.detail && evt.detail.target && evt.detail.target.id === 'device-table-container') load();
  });
  document.body.addEventListener('fleet:scoped', function () { fitted = false; load(); });
  try { if (localStorage.getItem('mdm-fleet-view') === 'map') window.flToggleMap(true); } catch (e) {}
})();
