// Maps MP1 "Canvas" (picked 5 Nov 2026; user: "make design lean maybe if we dont use google
// maps"): MapLibre (static/vendor/maplibre, BSD) drawing OpenFreeMap's vector tiles (no key,
// no quota) in our own style: the canvas grey, white water and roads, only cities and main
// roads, with a dark twin that follows the theme. The data credit is the compact ⓘ, folded
// (user: "make it not show openfreemap (c) if possible"; OSM's licence needs it reachable).
(function () {
  if (window.mdmMap) return;
  var VER = '4.7.1', loaded = false, waiting = [];
  function dark() { return !!(window.mdmTheme && window.mdmTheme() === 'dark'); }
  function style(isDark) {
    var c = isDark
      ? { bg: '#17171d', water: '#0d0d12', road: '#26262e', road2: '#202027', park: '#1b1f1c', bld: '#1f1f26', text: '#8a8a96', halo: '#17171d', bound: '#3a3a44' }
      : { bg: '#ececf0', water: '#ffffff', road: '#ffffff', road2: '#f7f7f9', park: '#e3e8e2', bld: '#e2e2e8', text: '#6d6d78', halo: '#ececf0', bound: '#c9c9d2' };
    var font = ['Noto Sans Regular'];
    return {
      version: 8,
      glyphs: 'https://tiles.openfreemap.org/fonts/{fontstack}/{range}.pbf',
      sources: { o: { type: 'vector', url: 'https://tiles.openfreemap.org/planet' } },
      layers: [
        { id: 'bg', type: 'background', paint: { 'background-color': c.bg } },
        { id: 'park', type: 'fill', source: 'o', 'source-layer': 'park', paint: { 'fill-color': c.park } },
        { id: 'water', type: 'fill', source: 'o', 'source-layer': 'water', paint: { 'fill-color': c.water } },
        { id: 'bld', type: 'fill', source: 'o', 'source-layer': 'building', minzoom: 14, paint: { 'fill-color': c.bld } },
        { id: 'r2', type: 'line', source: 'o', 'source-layer': 'transportation', minzoom: 11,
          filter: ['in', 'class', 'secondary', 'tertiary', 'minor'],
          paint: { 'line-color': c.road2, 'line-width': ['interpolate', ['linear'], ['zoom'], 11, 0.5, 16, 6] } },
        { id: 'r1', type: 'line', source: 'o', 'source-layer': 'transportation',
          filter: ['in', 'class', 'motorway', 'trunk', 'primary'],
          paint: { 'line-color': c.road, 'line-width': ['interpolate', ['linear'], ['zoom'], 6, 0.6, 10, 1.6, 16, 9] } },
        { id: 'bd', type: 'line', source: 'o', 'source-layer': 'boundary', filter: ['in', 'admin_level', 2, 4],
          paint: { 'line-color': c.bound, 'line-dasharray': [2, 2], 'line-width': 1 } },
        { id: 'road-lbl', type: 'symbol', source: 'o', 'source-layer': 'transportation_name', minzoom: 14,
          layout: { 'symbol-placement': 'line', 'text-field': ['get', 'name'], 'text-font': font, 'text-size': 10.5 },
          paint: { 'text-color': c.text, 'text-halo-color': c.halo, 'text-halo-width': 1.5 } },
        { id: 'place', type: 'symbol', source: 'o', 'source-layer': 'place',
          filter: ['in', 'class', 'country', 'state', 'city', 'town'],
          layout: { 'text-field': ['get', 'name'], 'text-font': font,
            'text-size': ['interpolate', ['linear'], ['zoom'], 3, 10, 12, 13] },
          paint: { 'text-color': c.text, 'text-halo-color': c.halo, 'text-halo-width': 1.5 } }
      ]
    };
  }
  // MapLibre is ~800 KB, so it loads on the first map, once per page.
  function load(cb) {
    if (window.maplibregl) { cb(); return; }
    waiting.push(cb);
    if (loaded) return;
    loaded = true;
    var css = document.createElement('link');
    css.rel = 'stylesheet'; css.href = '/static/vendor/maplibre/maplibre-gl.css?v=' + VER;
    document.head.appendChild(css);
    var s = document.createElement('script');
    s.src = '/static/vendor/maplibre/maplibre-gl.js?v=' + VER;
    s.onload = function () { var q = waiting; waiting = []; q.forEach(function (f) { f(); }); };
    document.head.appendChild(s);
  }
  function fold(el) {
    var a = el.querySelector('.maplibregl-ctrl-attrib');
    if (a) { a.classList.remove('maplibregl-compact-show'); a.removeAttribute('open'); }
  }
  // create(el, {center:[lon,lat], zoom, nav}) → a map in the Canvas style that follows the theme.
  function create(el, o) {
    o = o || {};
    var map = new maplibregl.Map({
      container: el, style: style(dark()), center: o.center || [0, 20], zoom: o.zoom == null ? 1.5 : o.zoom,
      attributionControl: false, dragRotate: false, pitchWithRotate: false, touchPitch: false, fadeDuration: 0
    });
    map.touchZoomRotate.disableRotation();
    map.addControl(new maplibregl.AttributionControl({ compact: true }), 'bottom-right');
    if (o.nav) map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-right');
    map.on('load', function () { fold(el); });
    map.on('styledata', function () { fold(el); });
    window.addEventListener('mdm:themechange', function () { map.setStyle(style(dark())); });
    return map;
  }
  window.mdmMap = { load: load, create: create, style: style };
})();
