// New venue N1 (picked 4 Nov 2026): one search over Google Maps and the AIO backend
// (GET /restaurants/new/search). A row found in both carries both badges; picking it fills
// the venue's fields (the hidden inputs and the form, which stays folded unless someone
// edits by hand), offers the devices the backend lists there, and links the venue to the
// backend restaurant on create. A row that is already a venue opens it instead.
(function () {
  var box = document.getElementById('vn1');
  if (!box) return;
  var q = document.getElementById('vn-q'), res = document.getElementById('vn-res'), hint = document.getElementById('vn-hint');
  var card = document.getElementById('vn-card'), manual = document.getElementById('rf-manual');
  var name = document.getElementById('rf-name'), addr = document.getElementById('rf-address');
  var url = box.dataset.search, hits = [], act = -1, timer = 0, ctl = null, picked = null;
  function $(id) { return document.getElementById(id); }
  function esc(x) { return String(x == null ? '' : x).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function initial(n) { var m = String(n || '?').match(/[A-Za-z0-9]/); return m ? m[0].toUpperCase() : '?'; }
  function badges(h) {
    var b = '';
    if (h.backend) b += '<span class="vn-bdg aio"><i></i>AIO backend · ' + esc(h.backend.env_label) + ' #' + h.backend.id + (h.backend.total ? ' · ' + h.backend.total + ' device' + (h.backend.total > 1 ? 's' : '') : '') + '</span>';
    if (h.google) b += '<span class="vn-bdg gm"><i></i>Google Maps' + (h.google.rating ? ' · ★ ' + h.google.rating.toFixed(1) : '') + '</span>';
    if (h.test) b += '<span class="vn-bdg test">test restaurant</span>';
    if (h.venue) b += '<span class="vn-bdg ven">already a venue</span>';
    return b;
  }
  function row(h, i) {
    var inner = '<span class="vn-ic" style="background:' + esc(h.color) + '">' + initial(h.name) + '</span><span class="vn-tx"><span class="vn-nm">' + esc(h.name) + '</span><span class="vn-ad" style="display:block">' + esc(h.address || 'No address') + '</span><span style="display:block">' + badges(h) + '</span>' + (h.why ? '<span class="vn-why" style="display:block">' + esc(h.why) + '</span>' : '') + '</span>';
    if (h.venue) return '<a class="vn-ri" role="option" id="vn-o' + i + '" data-i="' + i + '" href="/restaurants/' + esc(h.venue.id) + '">' + inner + '<span class="vn-open">Open ' + esc(h.venue.name) + ' →</span></a>';
    return '<button type="button" class="vn-ri' + (picked === h ? ' on' : '') + '" role="option" id="vn-o' + i + '" data-i="' + i + '">' + inner + '<span class="vn-go">›</span></button>';
  }
  function render(google) {
    res.hidden = false; q.setAttribute('aria-expanded', 'true');
    res.innerHTML = hits.length ? hits.map(row).join('') : '<div class="vn-empty">Nothing in Google Maps or the AIO backend for that. Try the street or the city, or add it manually.</div>';
    hint.textContent = google === 'off' ? 'Google Maps search is not set up on this server: only the AIO backend was searched.'
      : google === 'error' ? 'Google Maps did not answer: only the AIO backend was searched.'
      : google === 'busy' ? 'Too many searches in a minute: only the AIO backend was searched.'
      : 'Searches Google Maps and the AIO backend together.';
    act = -1;
  }
  function search() {
    var v = q.value.trim();
    if (v.length < 2) { res.hidden = true; q.setAttribute('aria-expanded', 'false'); return; }
    if (ctl) ctl.abort();
    ctl = window.AbortController ? new AbortController() : null;
    hint.textContent = 'Searching…';
    fetch(url + '?q=' + encodeURIComponent(v), { credentials: 'same-origin', signal: ctl ? ctl.signal : undefined })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function (d) { hits = d.results || []; render(d.google); })
      .catch(function (e) { if (e && e.name === 'AbortError') return; hint.textContent = 'The search failed. Try again, or add it manually.'; });
  }
  q.addEventListener('input', function () { clearTimeout(timer); timer = setTimeout(search, 300); });
  q.addEventListener('keydown', function (e) {
    var rows = res.querySelectorAll('.vn-ri');
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      if (!rows.length) return; e.preventDefault();
      act = e.key === 'ArrowDown' ? Math.min(act + 1, rows.length - 1) : Math.max(act - 1, 0);
      rows.forEach(function (r, i) { r.classList.toggle('act', i === act); });
      q.setAttribute('aria-activedescendant', rows[act].id); rows[act].scrollIntoView({ block: 'nearest' });
    } else if (e.key === 'Enter') {
      e.preventDefault(); // never submit the form from the search box
      if (act >= 0 && rows[act]) rows[act].click();
    }
  });
  res.addEventListener('click', function (e) {
    var b = e.target.closest('button.vn-ri'); if (!b) return;
    pick(hits[+b.dataset.i]);
    res.querySelectorAll('.vn-ri').forEach(function (r) { r.classList.toggle('on', r === b); });
  });
  // S1: a backend restaurant waiting for a venue fills the card like a search pick.
  box.querySelectorAll('.vn-add').forEach(function (b) {
    b.addEventListener('click', function () { try { var h = JSON.parse(b.getAttribute('data-hit')); q.value = h.name; pick(h); } catch (e) {} });
  });
  $('vn-hand').addEventListener('click', function () { pick(null); box.classList.add('manual'); openForm(); if (name) name.focus(); });

  function set(id, v) { var el = $(id); if (el) el.value = v == null ? '' : v; }
  // Leaves the search-only start: the card (a pick) or the form (Add manually), the steps and
  // the Deploy devices card come in.
  function unfold() { box.classList.remove('start'); if (q.form) q.form.classList.remove('vn-start'); }
  function openForm() { unfold(); if (manual) manual.hidden = false; if (window.rfPrev) rfPrev(); }
  function pick(h) {
    picked = h;
    set('vn-lat', h && h.lat); set('vn-lng', h && h.lng); set('vn-tz', h && h.tz);
    set('vn-benv', h && h.backend ? h.backend.env : ''); set('vn-bid', h && h.backend ? h.backend.id : '');
    set('vn-web', h && h.google ? h.google.website : '');
    if (name) name.value = h ? h.name : name.value;
    if (addr) addr.value = h ? h.address : addr.value;
    if (window.rfPrev) rfPrev();
    if (!h) { card.innerHTML = '<div class="vn-ph">Adding it manually: fill in the form below. You can link it to the AIO backend later in Venue settings.</div>'; return; }
    unfold(); box.classList.remove('manual');
    drawCard(h);
  }
  function src(s) { return '<span class="vn-src">' + s + '</span>'; }
  function drawCard(h) {
    var b = h.backend, html = '<div class="vn-head"><span class="vn-ic" style="background:' + esc(h.color) + '">' + initial(h.name) + '</span><div><div class="vn-title">' + esc(h.name) + '</div><div>' + badges(h) + '</div></div></div>';
    html += '<div class="vn-fld"><span class="k">Address</span><span>' + esc(h.address || '—') + src(h.google ? 'Google' : 'backend') + '</span></div>';
    html += '<div class="vn-fld"><span class="k">Pin</span><span>' + (h.lat != null ? 'Set' + src(h.google ? 'Google' : 'backend') : 'Not known yet') + '</span></div>';
    html += '<div class="vn-fld"><span class="k">Timezone</span><span>' + esc(h.tz || 'From its devices') + (h.tz ? src(h.google ? 'Google' : 'backend') : '') + '</span></div>';
    html += '<div class="vn-fld"><span class="k">Hours</span><span>' + (b ? 'The backend\'s, kept in step' + src('backend') : 'Set them in Venue settings') + '</span></div>';
    html += '<div class="vn-fld"><span class="k">Icon</span><span>' + (b ? 'The backend\'s logo' + (h.google && h.google.website ? ', else the website\'s icon' : '') : h.google && h.google.website ? 'The website\'s icon' : 'Its initial') + '</span></div>';
    html += '<div class="vn-fld"><span class="k">AIO backend</span><span>' + (b ? 'Linked to ' + esc(b.env_label) + ' #' + b.id + (b.name !== h.name ? ' ("' + esc(b.name) + '")' : '') + (h.google ? ' <button type="button" class="vn-link" id="vn-unlink" style="font-size:12px">Not the same place?</button>' : '') : 'Not listed there · link it later in Venue settings') + '</span></div>';
    if (b && b.total) {
      // Devices are placed from Setup (4 Nov 2026), not here: say what is waiting.
      var inMdm = (b.devices || []).length;
      html += '<div class="vn-fld"><span class="k">Devices</span><span>The backend lists ' + b.total + (inMdm ? ', ' + inMdm + ' in the MDM' : '') + '. Place them from Setup once the venue exists.</span></div>';
    }
    html += '<div class="vn-actions"><button type="button" class="cc-btn" id="vn-edit">Edit details</button><span class="sp"></span><button type="submit" class="cc-btn primary" id="vn-create"></button></div>';
    card.innerHTML = html;
    $('vn-create').textContent = 'Create venue';
    $('vn-edit').addEventListener('click', function () { openForm(); if (name) name.focus(); });
    var ul = $('vn-unlink');
    if (ul) ul.addEventListener('click', function () {
      // Google's place only: drop the backend link and its devices.
      var g = Object.assign({}, h, { backend: null, why: '' });
      if (!g.google.website) g.google = h.google;
      pick(g);
    });
  }
  // The search box sits inside the create form: an Enter there must never submit it.
  q.form && q.form.addEventListener('submit', function (e) {
    if (!name || !name.value.trim()) { e.preventDefault(); q.focus(); }
  });
  if (q.value.trim().length >= 2) search();
})();
