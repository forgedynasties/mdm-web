// Loaded by templates/overview_new.html.
// First-visit intro: shown once per browser, dismissed by either button.
(function () {
  var box = document.getElementById('ovd-intro');
  if (!box) return;
  var KEY = 'mdm-ov-new-intro';
  try { if (localStorage.getItem(KEY)) return; } catch (e) { return; }
  function done() { box.hidden = true; try { localStorage.setItem(KEY, '1'); } catch (e) {} }
  document.addEventListener('htmx:beforeSwap', function gone() { box.remove(); document.removeEventListener('htmx:beforeSwap', gone); });
  box.hidden = false;
  document.getElementById('ovd-intro-ok').addEventListener('click', done);
  document.addEventListener('keydown', function esc(e) { if (e.key === 'Escape' && !box.hidden) { done(); document.removeEventListener('keydown', esc); } });
})();

(function () {
  var t = document.getElementById('ovd-at');
  if (!t) return;
  var d = new Date(t.getAttribute('datetime'));
  if (!isNaN(d)) t.textContent = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  if (window.mdmTickRel) window.mdmTickRel();
})();

// Wall tooltips: one floating label, instead of a title per square.
(function () {
  var wall = document.querySelector('.ovd-wall');
  if (!wall) return;
  // The label lives on <body>, outside the page that navigation swaps. Clicking a square
  // left it showing on the next page and in the history snapshot, so coming back added a
  // second one next to the stuck one: start from none, and remove it on the way out.
  var old = document.getElementById('ovd-tip');
  if (old) old.remove();
  var tip = document.createElement('div');
  tip.id = 'ovd-tip';
  document.body.appendChild(tip);
  function hide() { tip.classList.remove('on'); }
  // Leaving the page (a boosted link saves a history snapshot first) removes it; any
  // other swap, like the cards refreshing, only hides it.
  function gone() {
    tip.remove();
    document.removeEventListener('htmx:beforeSwap', hide);
    document.removeEventListener('htmx:beforeHistorySave', gone);
    window.removeEventListener('pagehide', gone);
    window.removeEventListener('scroll', hide);
  }
  wall.addEventListener('click', hide);
  window.addEventListener('scroll', hide, { passive: true });
  document.addEventListener('htmx:beforeSwap', hide);
  document.addEventListener('htmx:beforeHistorySave', gone);
  window.addEventListener('pagehide', gone);
  wall.addEventListener('mousemove', function (e) {
    var t = e.target.closest('[data-tip]');
    if (!t) { tip.classList.remove('on'); return; }
    tip.textContent = t.getAttribute('data-tip');
    var x = Math.min(e.clientX + 12, window.innerWidth - tip.offsetWidth - 8);
    tip.style.left = x + 'px'; tip.style.top = (e.clientY + 16) + 'px';
    tip.classList.add('on');
  });
  wall.addEventListener('mouseleave', function () { tip.classList.remove('on'); });
})();

// Restaurant map: a circle per restaurant, sized by devices, coloured by state.
window.ovdInitMap = function () {
  var el = document.getElementById('ovd-map'), raw = document.getElementById('ovd-map-data');
  if (!el || !raw || !window.google) return;
  var sites = JSON.parse(raw.textContent || '[]');
  if (!sites.length) return;
  // Google's own labels are dimmed so the restaurants' names are what stands out.
  var MUTED = [{ stylers: [{ saturation: -70 }] }, { elementType: 'labels', stylers: [{ lightness: 35 }] }, { featureType: 'poi', stylers: [{ visibility: 'off' }] }, { featureType: 'transit', stylers: [{ visibility: 'off' }] }];
  var DARK = [
    {elementType:'geometry',stylers:[{color:'#212a37'}]},{elementType:'labels.text.fill',stylers:[{color:'#6b7486'}]},
    {elementType:'labels.text.stroke',stylers:[{color:'#1b2431'}]},{featureType:'administrative',elementType:'geometry',stylers:[{color:'#4b5563'}]},
    {featureType:'poi',stylers:[{visibility:'off'}]},{featureType:'road',elementType:'geometry',stylers:[{color:'#2c3644'}]},
    {featureType:'road',elementType:'labels.text.fill',stylers:[{color:'#5d6678'}]},{featureType:'road.highway',elementType:'geometry',stylers:[{color:'#3a4757'}]},
    {featureType:'transit',stylers:[{visibility:'off'}]},{featureType:'water',elementType:'geometry',stylers:[{color:'#17212f'}]},
    {featureType:'water',elementType:'labels.text.fill',stylers:[{color:'#51607a'}]}
  ];
  var map = new google.maps.Map(el, { zoom: 10, center: { lat: sites[0].lat, lng: sites[0].lng }, mapTypeControl: false, streetViewControl: false, fullscreenControl: true, keyboardShortcuts: false, gestureHandling: 'cooperative' });
  var markers = [];
  function esc(v) { return String(v).replace(/[&<>"]/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]; }); }
  function color(h) {
    var cs = getComputedStyle(document.body);
    return cs.getPropertyValue(h === 'bad' ? '--danger' : h === 'warn' ? '--warn' : '--ok').trim();
  }
  // Each restaurant is a dot with its device count inside and its name underneath.
  // Worst first, so when names collide the ones at risk keep theirs.
  var rank = { ok: 2, warn: 1, bad: 0 };
  sites.forEach(function (s) { s.r = Math.round(9 + Math.sqrt(s.total) * 1.8); });
  var order = sites.slice().sort(function (a, b) { return rank[a.health] - rank[b.health] || b.total - a.total; });

  // Names: an overlay under each dot; one that would overlap a dot or another name
  // is hidden and shows while its dot is hovered.
  function Names() {}
  Names.prototype = new google.maps.OverlayView();
  Names.prototype.onAdd = function () {
    var pane = this.getPanes().overlayLayer;
    sites.forEach(function (s) {
      s.lbl = document.createElement('div');
      s.lbl.className = 'ovd-mlbl ' + s.health;
      s.lbl.textContent = s.name;
      pane.appendChild(s.lbl);
    });
  };
  Names.prototype.draw = function () {
    var proj = this.getProjection(), boxes = [];
    if (!proj) return;
    sites.forEach(function (s) {
      s.px = proj.fromLatLngToDivPixel(new google.maps.LatLng(s.lat, s.lng));
      boxes.push({ x: s.px.x - s.r, y: s.px.y - s.r, w: 2 * s.r, h: 2 * s.r });
    });
    function free(b) { return !boxes.some(function (o) { return b.x < o.x + o.w && b.x + b.w > o.x && b.y < o.y + o.h && b.y + b.h > o.y; }); }
    order.forEach(function (s) {
      var w = s.lbl.offsetWidth, h = s.lbl.offsetHeight, x = s.px.x, y = s.px.y, r = s.r;
      // Under the dot first, then right, left, above.
      var spots = [{ x: x - w / 2, y: y + r + 3 }, { x: x + r + 4, y: y - h / 2 }, { x: x - r - 4 - w, y: y - h / 2 }, { x: x - w / 2, y: y - r - 3 - h }];
      var b = null;
      for (var i = 0; i < spots.length && !b; i++) {
        var c = { x: spots[i].x, y: spots[i].y, w: w, h: h };
        if (free(c)) b = c;
      }
      var at = b || { x: spots[0].x, y: spots[0].y };
      s.lbl.style.left = at.x + 'px';
      s.lbl.style.top = at.y + 'px';
      s.lbl.classList.toggle('off', !b);
      if (b) boxes.push(b);
    });
  };
  Names.prototype.onRemove = function () { sites.forEach(function (s) { if (s.lbl) s.lbl.remove(); }); };

  function draw() {
    var dark = window.mdmTheme && window.mdmTheme() === 'dark';
    var font = getComputedStyle(document.body).fontFamily;
    map.setOptions({ styles: dark ? DARK : MUTED });
    markers.forEach(function (m) { m.setMap(null); });
    markers = [];
    var bounds = new google.maps.LatLngBounds();
    order.slice().reverse().forEach(function (s) {
      var mk = new google.maps.Marker({
        position: { lat: s.lat, lng: s.lng }, map: map, title: s.name + ' · ' + s.total + ' device' + (s.total === 1 ? '' : 's'), zIndex: 10 + (2 - rank[s.health]),
        icon: { path: google.maps.SymbolPath.CIRCLE, scale: s.r, fillColor: color(s.health), fillOpacity: 1, strokeColor: dark ? '#16161d' : '#ffffff', strokeWeight: 2 },
        // Dark-theme status colours are light, so the count goes dark there.
        label: { text: String(s.total), color: dark ? '#0d0d12' : '#ffffff', fontSize: (s.r > 14 ? 12 : 11) + 'px', fontWeight: '700', fontFamily: font }
      });
      mk.addListener('click', function () { card.show(s); });
      mk.addListener('mouseover', function () { if (s.lbl) s.lbl.classList.add('peek'); });
      mk.addListener('mouseout', function () { if (s.lbl) s.lbl.classList.remove('peek'); });
      markers.push(mk);
      bounds.extend(mk.getPosition());
    });
    return bounds;
  }
  new Names().setMap(map);

  // The restaurant card: one at a time, above its dot. Closed by ×, Escape, or a click
  // on the map; opening it pans the map so the whole card is in view.
  function Card() { this.s = null; }
  Card.prototype = new google.maps.OverlayView();
  Card.prototype.onAdd = function () {
    var el = this.el = document.createElement('div');
    el.className = 'ovd-mcard';
    el.hidden = true;
    el.setAttribute('role', 'dialog');
    google.maps.OverlayView.preventMapHitsAndGesturesFrom(el);
    this.getPanes().floatPane.appendChild(el);
    var self = this;
    el.addEventListener('click', function (e) { if (e.target.closest('.x')) self.hide(); });
  };
  Card.prototype.draw = function () {
    if (!this.s || !this.el) return;
    var p = this.getProjection().fromLatLngToDivPixel(new google.maps.LatLng(this.s.lat, this.s.lng));
    this.el.style.left = p.x + 'px';
    this.el.style.top = (p.y - this.s.r - 8) + 'px';
  };
  Card.prototype.onRemove = function () { if (this.el) this.el.remove(); };
  Card.prototype.show = function (s) {
    this.s = s;
    var el = this.el, on = s.online, off = s.total - s.online;
    var dot = s.health === 'bad' ? 'danger' : s.health;
    var h = '<div class="h"><span class="ovd-dot ' + esc(dot) + '"></span><b>' + esc(s.name) + '</b>'
      + '<button type="button" class="x" aria-label="Close">×</button></div><div class="b">'
      + '<div class="mt"><i style="width:' + (s.total ? Math.round(on * 100 / s.total) : 0) + '%"></i></div>'
      + '<div class="rw"><span><b>' + on + '</b> of ' + s.total + ' online</span>' + (off ? '<span>' + off + ' silent</span>' : '<span>all reporting</span>') + '</div>';
    if (s.silent && s.silent.length) {
      var shown = s.silent.slice(0, 8);
      h += '<div class="sl">Silent: ' + shown.map(function (x) {
        return '<a href="/devices/' + encodeURIComponent(x) + '" title="' + esc(x) + '">' + esc(x.length > 7 ? x.slice(-5) : x) + '</a>';
      }).join('') + (s.silent.length > shown.length ? ' +' + (s.silent.length - shown.length) : '') + '</div>';
    }
    if (s.uptime && s.uptime.length) {
      h += '<div class="up">' + s.uptime.map(function (c) { return '<i class="' + esc(c) + '"></i>'; }).join('') + '<small>uptime, 7 days</small></div>';
    }
    h += '</div><div class="f"><a class="cc-btn sm primary" href="/restaurants/' + encodeURIComponent(s.id) + '">Open restaurant</a>'
      + '<a class="cc-btn sm" href="/devices?restaurant=' + encodeURIComponent(s.id) + '">Its devices</a></div>';
    el.innerHTML = h;
    el.setAttribute('aria-label', s.name);
    el.hidden = false;
    this.draw();
    // Pan so the card fits inside the map with a small margin.
    var mr = map.getDiv().getBoundingClientRect(), cr = el.getBoundingClientRect(), m = 12, dx = 0, dy = 0;
    if (cr.top < mr.top + m) dy = cr.top - mr.top - m;
    if (cr.left < mr.left + m) dx = cr.left - mr.left - m;
    else if (cr.right > mr.right - m) dx = cr.right - mr.right + m;
    if (dx || dy) map.panBy(dx, dy);
    var x = el.querySelector('.x'); if (x) x.focus({ preventScroll: true });
  };
  Card.prototype.hide = function () { this.s = null; if (this.el) this.el.hidden = true; };
  var card = new Card();
  card.setMap(map);
  map.addListener('click', function () { card.hide(); });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && card.s) card.hide(); });
  // Frame the restaurants: one is centred at street level, several are fitted.
  var b = draw();
  if (sites.length === 1) {
    map.setZoom(14);
  } else {
    google.maps.event.addListenerOnce(map, 'bounds_changed', function () { if (map.getZoom() > 15) map.setZoom(15); });
    map.fitBounds(b, 48);
  }
  window.addEventListener('mdm:themechange', function () { draw(); });
};
