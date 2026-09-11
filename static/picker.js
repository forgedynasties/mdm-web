/* Shared device picker — the Actions-page rail, standalone.
 *
 * Markup contract: templates/_picker.html. One picker per page.
 *   #pk-rail[data-field,data-scopes,data-exclude-group,data-exclude-restaurant]
 *   #pk-q #pk-quick #pk-stabs #pk-scopes #pk-devs #pk-pills #pk-count #pk-clear
 *   #pk-serials            hidden input the form submits (newline separated)
 *   [data-dp-count]        elements showing the chosen count
 *   [data-dp-submit]       buttons disabled while nothing is chosen
 *
 * Rows come from /commands/browse-devices, the same endpoint the Actions page
 * uses, so both stay in step. A scope (restaurant or group) expands to its
 * devices: the picker stores concrete serials, never a scope reference, because
 * the pages using it export or assign an explicit list.
 */
(function () {
  var rail = document.getElementById('pk-rail');
  if (!rail) return;

  var ENDPOINT = '/commands/browse-devices';
  var picked = {};          // serial -> {serial, sub, online, bat}
  var meta = {};            // serial -> row, for pill labels
  var cache = {};           // request key -> rows
  var QF = {};              // active quick filters
  var stab = 'all';
  var timer = null, loading = false, scopeBusy = '';

  function el(id) { return document.getElementById(id); }
  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function scopes() { try { return JSON.parse(el('pk-scope-data').textContent || '[]') || []; } catch (e) { return []; } }

  // ── data ────────────────────────────────────────────────────────────────────
  function baseParams() {
    var p = new URLSearchParams();
    if (rail.dataset.excludeGroup) p.set('exclude_group', rail.dataset.excludeGroup);
    if (rail.dataset.excludeRestaurant) p.set('exclude_restaurant', rail.dataset.excludeRestaurant);
    return p;
  }
  function parseRows(html) {
    var doc = new DOMParser().parseFromString(html, 'text/html'), out = [];
    doc.querySelectorAll('.cb-row').forEach(function (row) {
      var cb = row.querySelector('.cmd-browse-cb'); if (!cb) return;
      var bat = row.querySelector('.dp-bchip'), sub = row.querySelector('.cb-sub');
      out.push({
        serial: cb.value,
        online: !!row.querySelector('.cb-status i.on'),
        dpc: row.getAttribute('data-dpc') === '1',
        bat: bat && bat.textContent !== '—' ? bat.textContent.trim() : '',
        sub: sub ? sub.textContent.trim() : ''
      });
    });
    return out;
  }
  function query() { return (el('pk-q').value || '').trim(); }
  function key() { return query() + '|' + (QF.kiosk ? 'k' : ''); }
  function load(now) {
    var k = key();
    if (cache[k]) { render(); return; }
    clearTimeout(timer);
    timer = setTimeout(function () {
      loading = true; render();
      var p = baseParams();
      if (query() && !pasteTokens()) p.set('q', query());
      if (QF.kiosk) p.set('kiosk', 'enabled');
      fetch(ENDPOINT + '?' + p.toString(), { credentials: 'same-origin' })
        .then(function (r) { return r.text(); })
        .then(function (html) {
          var rows = parseRows(html);
          rows.forEach(function (d) { meta[d.serial] = d; });
          cache[k] = rows; loading = false;
          renderQuick(rows);
          render();
        })
        .catch(function () { loading = false; render(); });
    }, now ? 0 : 220);
  }
  // A pasted list (spaces/commas/newlines) adds every serial at once instead of
  // searching for the blob.
  function pasteTokens() {
    var q = query();
    return q && /[\s,;]/.test(q) ? q.split(/[\s,;]+/).filter(Boolean) : null;
  }

  // ── rendering ───────────────────────────────────────────────────────────────
  var counts = null;
  function renderQuick(rows) {
    if (!counts && rows && key() === '|') {
      counts = { online: 0, offline: 0, dpc: 0 };
      rows.forEach(function (d) { d.online ? counts.online++ : counts.offline++; if (d.dpc) counts.dpc++; });
    }
    rail.querySelectorAll('#pk-quick .co-qf').forEach(function (b) {
      var k = b.dataset.qf;
      b.classList.toggle('on', !!QF[k]);
      var n = b.querySelector('.n');
      if (n && counts && k !== 'kiosk') n.textContent = counts[k];
      if (k === 'dpc' && counts) b.hidden = !counts.dpc && !QF.dpc;
    });
  }
  function visible() {
    var rows = cache[key()] || [];
    if (QF.online) rows = rows.filter(function (d) { return d.online; });
    if (QF.offline) rows = rows.filter(function (d) { return !d.online; });
    if (QF.dpc) rows = rows.filter(function (d) { return d.dpc; });
    return rows;
  }
  function render() {
    var host = el('pk-devs'), rows = visible(), html = '';
    var tokens = pasteTokens();
    if (tokens) {
      html += '<button type="button" class="co-paste" id="pk-paste"><svg viewBox="0 0 24 24"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>Add ' + tokens.length + ' pasted serial' + (tokens.length === 1 ? '' : 's') + '</button>';
    }
    if (loading) html += '<div class="co-listempty">Loading devices…</div>';
    else if (!rows.length) html += '<div class="co-listempty">No device matches.</div>';
    rows.forEach(function (d) {
      var on = !!picked[d.serial];
      html += '<button type="button" class="co-dev' + (on ? ' on' : '') + '" data-serial="' + esc(d.serial) + '">' +
        '<span class="cb">' + (on ? '<svg viewBox="0 0 24 24"><path d="M4 12l5 5L20 6"/></svg>' : '') + '</span>' +
        '<span class="bd"><span class="sn">' + esc(d.serial) + '</span><span class="mt">' + esc(d.sub) + '</span></span>' +
        '<span class="st ' + (d.online ? 'on' : 'off') + '"></span></button>';
    });
    host.innerHTML = html;
    renderScopes();
    renderBasket();
  }
  function renderScopes() {
    var host = el('pk-scopes'); if (!host) return;
    var list = scopes().filter(function (s) { return stab === 'all' || s.kind === stab; });
    host.innerHTML = list.map(function (s) {
      var busy = scopeBusy === s.kind + ':' + s.id;
      return '<button type="button" class="co-scope" data-kind="' + s.kind + '" data-id="' + esc(s.id) + '" data-name="' + esc(s.name) + '">' +
        '<span class="ic">' + (s.kind === 'restaurant' ? '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 21V8l9-5 9 5v13"/><path d="M9 21v-6h6v6"/></svg>' : '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><circle cx="9" cy="8" r="3"/><path d="M3 19a6 6 0 0 1 12 0"/></svg>') + '</span>' +
        '<span class="bd"><span class="nm">' + esc(s.name) + '</span><span class="kt">' + (s.kind === 'restaurant' ? 'restaurant' : 'group') + '</span> <span class="ct">' + (busy ? 'adding…' : s.count + ' devices') + '</span></span></button>';
    }).join('') || '<div class="co-listempty">Nothing here yet.</div>';
  }
  function renderBasket() {
    var serials = Object.keys(picked);
    el('pk-count').textContent = serials.length;
    el('pk-count-t').textContent = serials.length === 1 ? 'device selected' : 'devices selected';
    el('pk-pills').innerHTML = serials.map(function (s) {
      return '<span class="co-bk-pill mono" data-serial="' + esc(s) + '"><span class="lbl">' + esc(s) + '</span><span class="x" data-x="' + esc(s) + '">×</span></span>';
    }).join('');
    var f = el('pk-serials'); if (f) f.value = serials.join('\n');
    document.querySelectorAll('[data-dp-count]').forEach(function (e) { e.textContent = serials.length; });
    document.querySelectorAll('[data-dp-submit]').forEach(function (e) { e.disabled = serials.length === 0; });
    document.dispatchEvent(new CustomEvent('pk:change', { detail: { serials: serials } }));
  }

  // ── interaction ─────────────────────────────────────────────────────────────
  function add(serial) { if (serial) picked[serial] = meta[serial] || { serial: serial }; }
  function toggle(serial) { if (picked[serial]) delete picked[serial]; else add(serial); render(); }

  rail.addEventListener('click', function (e) {
    var dev = e.target.closest('.co-dev');
    if (dev) { toggle(dev.dataset.serial); return; }
    var paste = e.target.closest('#pk-paste');
    if (paste) { (pasteTokens() || []).forEach(add); el('pk-q').value = ''; load(true); return; }
    var qf = e.target.closest('.co-qf');
    if (qf) {
      var k = qf.dataset.qf;
      if (k === 'online' || k === 'offline') { var other = k === 'online' ? 'offline' : 'online'; delete QF[other]; }
      QF[k] = !QF[k]; if (!QF[k]) delete QF[k];
      if (k === 'kiosk') load(true); else { renderQuick(); render(); }
      return;
    }
    var tab = e.target.closest('.co-stab');
    if (tab) {
      stab = tab.dataset.stab;
      rail.querySelectorAll('.co-stab').forEach(function (t) { t.classList.toggle('on', t === tab); });
      renderScopes();
      return;
    }
    var scope = e.target.closest('.co-scope');
    if (scope) { addScope(scope.dataset.kind, scope.dataset.id, scope.dataset.name); return; }
    var x = e.target.closest('[data-x]');
    if (x) { delete picked[x.dataset.x]; render(); return; }
    if (e.target.closest('#pk-clear')) { picked = {}; render(); }
  });
  el('pk-q').addEventListener('input', function () { load(false); });

  // A scope adds every device it holds, then the basket is a plain serial list.
  function addScope(kind, id, name) {
    var k = kind + ':' + id;
    if (scopeBusy) return;
    scopeBusy = k; renderScopes();
    var p = baseParams();
    p.set(kind === 'restaurant' ? 'restaurant' : 'group', id);
    fetch(ENDPOINT + '?' + p.toString(), { credentials: 'same-origin' })
      .then(function (r) { return r.text(); })
      .then(function (html) {
        parseRows(html).forEach(function (d) { meta[d.serial] = d; add(d.serial); });
      })
      .catch(function () {})
      .then(function () { scopeBusy = ''; render(); });
  }

  // Pages can preselect (e.g. arriving with ?serials=).
  window.pkPreselect = function (serials) { (serials || []).forEach(add); render(); };
  window.pkSerials = function () { return Object.keys(picked); };
  window.pkClear = function () { picked = {}; render(); };
  window.pkReload = function () { cache = {}; counts = null; load(true); };

  // Serials the page arrived with (rendered into the hidden field server-side).
  var seed = (el('pk-serials').value || '').split('\n').map(function (x) { return x.trim(); }).filter(Boolean);
  seed.forEach(add);

  renderQuick();
  load(true);
})();
