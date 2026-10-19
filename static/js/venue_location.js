// Loaded by templates/_venue_profile.html (venue page, Profile and Location cards).
// The Location card: search an address (the server asks Google), drag the pin, or take
// where the venue's devices are; the timezone can follow what they report. Everything
// lands in the form's hidden fields and is saved with the card's Save.
(function () {
  // Open a card from the header's Edit button (or ?open=), scroll to it, focus it.
  window.vnOpenCard = function (id) {
    var d = document.getElementById(id);
    if (!d) return;
    d.open = true;
    d.scrollIntoView({ block: 'start', behavior: 'smooth' });
    var f = d.querySelector('input[type=text]:not([hidden]), input[type=email], textarea');
    if (f) setTimeout(function () { f.focus({ preventScroll: true }); }, 250);
  };

  function init() {
    // A save sends you back with ?open=<card>; the card is already open, so drop the
    // parameter and bring it into view.
    try {
      var u = new URL(location.href), open = u.searchParams.get('open');
      if (open) {
        u.searchParams.delete('open');
        history.replaceState(null, '', u.pathname + (u.searchParams.toString() ? '?' + u.searchParams : '') + u.hash);
        var c = document.getElementById('vn-' + open);
        if (c) c.scrollIntoView({ block: 'start' });
      }
    } catch (e) {}

    var form = document.querySelector('.vl-form');
    if (!form || form.dataset.init === '1') return;
    form.dataset.init = '1';
    var lat = form.querySelector('input[name=latitude]'), lon = form.querySelector('input[name=longitude]');
    var q = form.querySelector('#vl-q'), box = form.querySelector('#vl-sugg');
    var pinned = form.querySelector('.vl-pinned'), card = form.closest('details');
    var map = null, marker = null;

    function setPin(la, lo, pan) {
      lat.value = la.toFixed(6); lon.value = lo.toFixed(6);
      if (pinned) pinned.innerHTML = 'Pinned at <span class="mono">' + la.toFixed(5) + ', ' + lo.toFixed(5) + '</span>.' + (form.dataset.key ? ' Drag the pin to adjust.' : '');
      if (map && window.google) {
        var p = { lat: la, lng: lo };
        if (!marker) marker = newMarker(p); else marker.setPosition(p);
        if (pan) { map.panTo(p); if (map.getZoom() < 16) map.setZoom(17); }
      }
    }

    // ── search ────────────────────────────────────────────────────────────────
    var timer = null, seq = 0, items = [], hi = -1;
    function close() { box.hidden = true; q.setAttribute('aria-expanded', 'false'); hi = -1; }
    function render() {
      box.innerHTML = items.length ? items.map(function (p, i) {
        var parts = p.address.split(', ');
        return '<button type="button" role="option" data-i="' + i + '"' + (i === hi ? ' class="on"' : '') + '>' + esc(parts[0]) + (parts.length > 1 ? '<br><small class="muted">' + esc(parts.slice(1).join(', ')) + '</small>' : '') + '</button>';
      }).join('') : '<div class="vl-none">No match. Try the street and city.</div>';
      box.hidden = false; q.setAttribute('aria-expanded', 'true');
    }
    function pick(i) {
      var p = items[i]; if (!p) return;
      q.value = p.address; close(); setPin(p.lat, p.lon, true);
    }
    if (form.dataset.geocode && q.getAttribute('role') === 'combobox') {
      q.addEventListener('input', function () {
        clearTimeout(timer);
        var v = q.value.trim();
        if (v.length < 4) { close(); return; }
        timer = setTimeout(function () {
          var my = ++seq;
          fetch(form.dataset.geocode + '?q=' + encodeURIComponent(v), { credentials: 'same-origin' })
            .then(function (r) { return r.json(); })
            .then(function (j) { if (my === seq) { items = j.places || []; hi = items.length ? 0 : -1; render(); } })
            .catch(function () {});
        }, 350);
      });
      q.addEventListener('keydown', function (e) {
        if (box.hidden) return;
        if (e.key === 'ArrowDown') { e.preventDefault(); hi = Math.min(hi + 1, items.length - 1); render(); }
        else if (e.key === 'ArrowUp') { e.preventDefault(); hi = Math.max(hi - 1, 0); render(); }
        else if (e.key === 'Enter') { e.preventDefault(); pick(hi < 0 ? 0 : hi); }
        else if (e.key === 'Escape') { close(); }
      });
      box.addEventListener('mousedown', function (e) {
        var b = e.target.closest('button[data-i]'); if (!b) return;
        e.preventDefault(); pick(+b.dataset.i);
      });
      q.addEventListener('blur', function () { setTimeout(close, 150); });
    }

    // ── devices' position and timezone ───────────────────────────────────────
    var useC = form.querySelector('[data-use-centre]');
    if (useC && form.dataset.centre) useC.addEventListener('click', function () {
      var c = form.dataset.centre.split(',');
      setPin(parseFloat(c[0]), parseFloat(c[1]), true);
    });
    var tzIn = form.querySelector('input[name=timezone]'), tzName = form.querySelector('.vl-tzname'), sel = form.querySelector('.vl-tzsel');
    function setTZ(v) {
      tzIn.value = v;
      var o = sel && [].find.call(sel.options, function (x) { return x.value === v; });
      tzName.textContent = v ? (o ? o.textContent : v) : "each device's own";
      if (sel) sel.value = v;
      var hint = form.querySelector('.vl-tzhint'); if (hint) hint.hidden = true;
    }
    var useTZ = form.querySelector('[data-use-tz]');
    if (useTZ) useTZ.addEventListener('click', function () { setTZ(useTZ.dataset.useTz); });
    var chg = form.querySelector('.vl-tzchange');
    if (chg && sel) {
      chg.addEventListener('click', function () { sel.hidden = !sel.hidden; if (!sel.hidden) sel.focus(); });
      sel.addEventListener('change', function () { setTZ(sel.value); sel.hidden = true; });
    }

    // ── map ──────────────────────────────────────────────────────────────────
    var pts = [];
    try { pts = JSON.parse(form.dataset.points || '[]'); } catch (e) {}
    function newMarker(p) {
      var m = new google.maps.Marker({ position: p, map: map, draggable: true, title: 'The venue' });
      m.addListener('dragend', function () { var x = m.getPosition(); setPin(x.lat(), x.lng(), false); });
      return m;
    }
    function draw() {
      var el = document.getElementById('vl-map');
      if (!el || map || !window.google || !google.maps) return;
      var css = getComputedStyle(document.documentElement);
      var blue = (css.getPropertyValue('--accent-2') || '#7a80f6').trim();
      var has = lat.value !== '' && lon.value !== '';
      var c = has ? { lat: +lat.value, lng: +lon.value } : (pts[0] ? { lat: pts[0].lat, lng: pts[0].lon } : { lat: 37.4, lng: -122.0 });
      map = new google.maps.Map(el, { center: c, zoom: has || pts.length ? 17 : 3, mapTypeControl: false, streetViewControl: false, fullscreenControl: false, clickableIcons: false });
      pts.forEach(function (p) {
        new google.maps.Marker({ position: { lat: p.lat, lng: p.lon }, map: map, title: p.serial, clickable: false,
          icon: { path: google.maps.SymbolPath.CIRCLE, scale: 5, fillColor: blue, fillOpacity: 1, strokeColor: '#fff', strokeWeight: 2 } });
      });
      if (has) marker = newMarker(c);
      // Click the map to drop the pin there.
      map.addListener('click', function (e) { setPin(e.latLng.lat(), e.latLng.lng(), false); });
    }
    function loadMap() {
      if (!form.dataset.key) return;
      if (window.google && google.maps) { draw(); return; }
      if (document.querySelector('script[src*="maps.googleapis.com/maps/api/js"]')) {
        var n = 0, t = setInterval(function () { if ((window.google && google.maps) || ++n > 40) { clearInterval(t); draw(); } }, 250);
        return;
      }
      window.vnLocationMapInit = draw;
      var s = document.createElement('script');
      s.async = true;
      s.src = 'https://maps.googleapis.com/maps/api/js?key=' + encodeURIComponent(form.dataset.key) + '&callback=vnLocationMapInit';
      document.head.appendChild(s);
    }
    // The map only loads once the card is opened: most visits never edit the location.
    if (card.open) loadMap();
    card.addEventListener('toggle', function () { if (card.open) loadMap(); });
  }

  function esc(s) { return String(s || '').replace(/[&<>"]/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]; }); }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init); else init();
  document.body.addEventListener('htmx:afterSettle', init);
})();
