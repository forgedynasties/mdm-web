// Loaded by templates/restaurant_form.html.
(function () {
  var lat = document.getElementById('rest-lat');
  var lng = document.getElementById('rest-lng');
  var paste = document.getElementById('rest-coord-paste');
  var link = document.getElementById('rest-map-link');
  if (!lat || !lng) return;
  function syncLink() {
    if (lat.value !== '' && lng.value !== '') {
      link.href = 'https://www.google.com/maps?q=' + encodeURIComponent(lat.value + ',' + lng.value);
      link.style.display = '';
    } else {
      link.style.display = 'none';
    }
  }
  if (paste) {
    paste.addEventListener('input', function () {
      var m = paste.value.split(',');
      if (m.length === 2) {
        var a = parseFloat(m[0].trim()), b = parseFloat(m[1].trim());
        if (!isNaN(a) && !isNaN(b)) { lat.value = a; lng.value = b; syncLink(); }
      }
    });
  }
  lat.addEventListener('input', syncLink);
  lng.addEventListener('input', syncLink);
  syncLink();
})();

// Searchable type-ahead timezone combobox. Filters the list as you type; stores the
// IANA value in the hidden #tz-value field that the form submits.
(function () {
  var combo = document.getElementById('tz-combo');
  if (!combo) return;
  var input = document.getElementById('tz-search');
  var hidden = document.getElementById('tz-value');
  var opts = [].slice.call(combo.querySelectorAll('.tz-opt'));
  var active = -1;

  function labelFor(val) {
    for (var i = 0; i < opts.length; i++) {
      if (opts[i].getAttribute('data-value') === val) return opts[i].textContent.trim();
    }
    return '';
  }
  input.value = labelFor(hidden.value); // prefill current selection

  function open() { combo.classList.add('open'); input.setAttribute('aria-expanded', 'true'); }
  function close() { combo.classList.remove('open'); input.setAttribute('aria-expanded', 'false'); active = -1; paintActive(); }
  function visible() { return opts.filter(function (o) { return !o.classList.contains('hidden'); }); }
  function paintActive() {
    var vis = visible();
    opts.forEach(function (o) { o.classList.remove('active'); });
    if (active >= 0 && active < vis.length) { vis[active].classList.add('active'); vis[active].scrollIntoView({ block: 'nearest' }); }
  }
  function filter(q) {
    q = q.trim().toLowerCase();
    opts.forEach(function (o) {
      var s = o.getAttribute('data-search').toLowerCase();
      o.classList.toggle('hidden', q !== '' && s.indexOf(q) === -1);
    });
    active = -1; paintActive();
  }
  function choose(o) { hidden.value = o.getAttribute('data-value'); input.value = o.textContent.trim(); close(); if (window.rfPrev) rfPrev(); }

  input.addEventListener('focus', function () { filter(''); open(); input.select(); });
  input.addEventListener('input', function () { filter(input.value); open(); if (window.rfPrev) rfPrev(); });
  input.addEventListener('keydown', function (e) {
    var vis = visible();
    if (e.key === 'ArrowDown') { e.preventDefault(); open(); active = Math.min(active + 1, vis.length - 1); paintActive(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); active = Math.max(active - 1, 0); paintActive(); }
    else if (e.key === 'Enter') { if (combo.classList.contains('open')) { e.preventDefault(); if (active >= 0) choose(vis[active]); } }
    else if (e.key === 'Escape') { input.value = labelFor(hidden.value); close(); }
  });
  opts.forEach(function (o) { o.addEventListener('mousedown', function (e) { e.preventDefault(); choose(o); }); });
  document.addEventListener('click', function (e) {
    if (!combo.contains(e.target)) { input.value = labelFor(hidden.value); close(); }
  });
})();

// Live venue preview + name-gated create button (create flow only).
function rfPrev() {
  var name = document.getElementById('rf-name');
  if (!name || !document.getElementById('pv-name')) return;
  var n = name.value.trim();
  document.getElementById('pv-name').textContent = n || 'New venue';
  var addr = (document.getElementById('rf-address') || {}).value || '';
  document.getElementById('pv-addr').textContent = addr.trim() || 'No address yet';
  var tz = (document.getElementById('tz-search') || {}).value || '';
  document.getElementById('pv-tz').textContent = tz.trim() || 'Per-device timezone';
  var sum = document.getElementById('rf-sum-name'); if (sum) sum.textContent = n || 'a new venue';
  var btn = document.getElementById('rf-create-btn'); if (btn) btn.disabled = n === '';
}
rfPrev();
