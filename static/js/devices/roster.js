// Loaded by templates/devices.html.
// Every roster parameter, read from the URL (the chips and "+ Filter" write the URL, so it
// is the single source of truth), with the live search box and any overrides on top.
var FLEET_PARAMS = ['q', 'sort', 'dir', 'page', 'group', 'restaurant', 'production', 'product', 'status', 'build',
  'battery', 'ram', 'temp', 'kiosk', 'charging', 'timezone', 'hidden', 'hygiene', 'enrolled_by', 'kind', 'class',
  'onboarding', 'lifecycle'];
function getFilterVals(overrides) {
  var p = new URLSearchParams(location.search);
  var vals = {};
  FLEET_PARAMS.forEach(function (k) { vals[k] = p.get(k) || ''; });
  if (!vals.page) vals.page = '1';
  var sq = document.getElementById('search-input');
  if (sq) vals.q = sq.value;
  if (overrides) {
    for (var k in overrides) vals[k] = overrides[k];
  }
  return vals;
}

// The roster URL for a set of values: empty ones are left out, so the address bar (and a
// view saved from it) carries only what is actually set.
function fleetURL(vals) {
  var q = new URLSearchParams();
  Object.keys(vals).forEach(function (k) { if (vals[k] !== '' && vals[k] != null && !(k === 'page' && vals[k] === '1')) q.set(k, vals[k]); });
  var qs = q.toString();
  return '/devices' + (qs ? '?' + qs : '');
}

// Signature of the active filters, excluding page/page_size — used to tell a
// plain page turn (keep the "select all matching" set) apart from an actual
// filter/search change (the old set no longer matches, so drop it).
function filterSig(vals) {
  vals = vals || getFilterVals();
  var o = Object.assign({}, vals);
  delete o.page; delete o.page_size;
  return JSON.stringify(o);
}

function navigateDevices(overrides) {
  var vals = getFilterVals(overrides);
  if (selectAllMatchingSet && filterSig(vals) !== selectAllMatchingSig) clearSelectAllMatching();
  var url = fleetURL(vals);
  // Explicit innerHTML swap: a bare target string would inherit the <body>'s
  // hx-swap="outerHTML" (hx-boost), which replaces the container itself and
  // spills the fragment into the flex .dv-split as a broken horizontal row.
  htmx.ajax('GET', url, { target: '#device-table-container', swap: 'innerHTML' }).then(function() {
    history.pushState(null, '', url);
    syncFilterUI();
    document.body.dispatchEvent(new CustomEvent('fleet:scoped'));
  });
}

function applyFilters() {
  navigateDevices({page: '1'});
}
// Set (or with '' clear) one filter.
function setFilter(k, v) {
  var o = {page: '1'}; o[k] = v;
  navigateDevices(o);
}

// Skeleton rows for the device list, shown while a rail collection switch loads.
function mdmDevSkeleton() {
  var row = '<div class="skel-row"><div class="skel skel-av"></div><div class="g"><div class="skel skel-line skel-w40"></div><div class="skel skel-line skel-w60"></div></div><div class="skel skel-pill"></div></div>';
  var s = ''; for (var i = 0; i < 7; i++) s += row;
  return '<div class="skel-list skel-card">' + s + '</div>';
}

// Re-scope the roster to a rail collection IN PLACE: only the device list (skeleton
// → real rows) and the heading/quick-view counts (OOB) change; the rail, search bar
// and inspector stay put. Switching collection resets the search + popover filters.
function scopeRoster(anchor, u) {
  clearSelectAllMatching();
  setSel('search-input', '');
  var ov = { page: '1', group: '', restaurant: '', build: '', production: '', product: '', status: '', battery: '', ram: '', temp: '', kiosk: '', charging: '', timezone: '', q: '', hidden: '', kind: '', class: '', onboarding: '', enrolled_by: '', lifecycle: '', hygiene: '' };
  if (u.searchParams.get('restaurant')) ov.restaurant = u.searchParams.get('restaurant');
  else if (u.searchParams.get('group')) ov.group = u.searchParams.get('group');
  else if (u.searchParams.get('build')) ov.build = u.searchParams.get('build');
  else if (u.searchParams.get('product')) { ov.product = u.searchParams.get('product'); }
  // The product tab stacks with the collection: keep it across rail clicks.
  var curClass = new URLSearchParams(location.search).get('class');
  if (curClass) ov.class = curClass;

  // Instant heading + rail highlight from the clicked item (before data lands).
  var item = anchor.closest('.fleet-rail-item');
  if (item) {
    document.querySelectorAll('.fleet-rail-item').forEach(function (x) { x.classList.remove('on'); });
    item.classList.add('on');
    var nm = item.querySelector('.nm'), ct = item.querySelector('.ct');
    var rt = document.getElementById('roster-title'), rc = document.getElementById('roster-count');
    if (rt && nm) rt.textContent = nm.textContent.trim();
    if (rc && ct) { var n = ct.textContent.trim(); rc.textContent = n + ' device' + (n === '1' ? '' : 's'); }
  }

  var tc = document.getElementById('device-table-container');
  if (tc) tc.innerHTML = mdmDevSkeleton();
  var url = fleetURL(getFilterVals(ov));
  htmx.ajax('GET', url, { target: '#device-table-container', swap: 'innerHTML', headers: { 'X-Roster-Meta': '1' } }).then(function () {
    history.pushState(null, '', url);
    syncFilterUI();
    applyCols();
    document.body.dispatchEvent(new CustomEvent('fleet:scoped'));
  });
}

function setSel(id, val) { var el = document.getElementById(id); if (el) el.value = val; }

// Clear one filter by its old select id ('filter-build'); kept for callers outside the chips.
function clearOne(id) {
  setFilter(String(id).replace(/^filter-/, '').replace(/-/g, '_'), '');
}

function clearSearch() {
  setSel('search-input', '');
  navigateDevices({page: '1', q: ''});
}

function clearFilters() {
  // Clears the filter chips, but keeps the rail collection (group/restaurant), the
  // product tab and the search in scope — Clear sits with the chips, so it clears those.
  ffClose();
  navigateDevices({page: '1', production: '', product: '', status: '', build: '', battery: '', ram: '', temp: '', kiosk: '', charging: '', timezone: '', hidden: '', kind: '', onboarding: '', enrolled_by: '', lifecycle: '', hygiene: ''});
}

// Collections rail: scope the roster to one collection, clearing the opposite.
// Product tabs: one product at a time; keeps the Filters panel's Product select in sync.
function setProductTab(cls) {
  navigateDevices({page: '1', class: cls});
}

function selectCollection(type, id) {
  var ov = {page: '1', group: '', restaurant: ''};
  if (type === 'group') ov.group = id;
  else if (type === 'restaurant') ov.restaurant = id;
  navigateDevices(ov);
}


// Keep an open popover inside the window, above the floating bottom nav: cap its
// height to the room below its top edge and let it scroll inside.
function fitFleetPop(pop) {
  var dock = document.querySelector('.dock, nav.dock, #dock');
  var bottom = dock ? dock.getBoundingClientRect().top - 12 : window.innerHeight - 16;
  var room = bottom - pop.getBoundingClientRect().top;
  pop.style.maxHeight = Math.max(220, room) + 'px';
  pop.style.overflowY = 'auto';
}
window.addEventListener('resize', function () {
  document.querySelectorAll('.fleet-pop:not([hidden])').forEach(fitFleetPop);
});

// Sort control: a popover next to Filters that sets sort/dir (replaces the
// old sortable column headers). Opening Sort closes Filters.
function toggleSort(e) {
  if (e) { e.preventDefault(); e.stopPropagation(); }
  var pop = document.getElementById('sort-pop');
  var btn = document.getElementById('sort-btn');
  if (!pop) return;
  var willOpen = pop.hasAttribute('hidden');
  if (willOpen) { ffClose(); selMenuClose(); }
  if (willOpen) { pop.removeAttribute('hidden'); fitFleetPop(pop); } else { pop.setAttribute('hidden', ''); }
  if (btn) { btn.setAttribute('aria-expanded', willOpen ? 'true' : 'false'); btn.classList.toggle('open', willOpen); }
}
function closeSort() {
  var pop = document.getElementById('sort-pop');
  if (!pop || pop.hasAttribute('hidden')) return;
  pop.setAttribute('hidden', '');
  var btn = document.getElementById('sort-btn');
  if (btn) { btn.setAttribute('aria-expanded', 'false'); btn.classList.remove('open'); }
}
function sortBy(key) {
  var c = curSort();
  // Re-selecting the active field flips direction; a new field defaults to a
  // sensible direction (serial A→Z, everything else high→low).
  var nextDir = (c.sort === key) ? (c.dir === 'desc' ? 'asc' : 'desc') : (key === 'serial' ? 'asc' : 'desc');
  navigateDevices({page: '1', sort: key, dir: nextDir});
  closeSort();
}
function sortDir(dir) {
  var c = curSort();
  if (c.dir !== dir) navigateDevices({page: '1', sort: c.sort, dir: dir});
  closeSort();
}

// Bind the outside-click / Escape closers once. This script re-runs on every htmx
// table swap, so guard against stacking duplicate document listeners.
if (!window.__fleetDocBound) {
  window.__fleetDocBound = true;
  document.addEventListener('click', function(e) {
    var fw = document.getElementById('fl-fchips');
    if (fw && !fw.contains(e.target) && !(e.target.closest && e.target.closest('#fl-map-chips'))) ffClose();
    var slw = document.getElementById('sel-wrap');
    if (slw && !slw.contains(e.target)) selMenuClose();
    var sw = document.getElementById('sort-wrap');
    if (sw && !sw.contains(e.target)) closeSort();
  });
  document.addEventListener('keydown', function(e) { if (e.key === 'Escape') { ffClose(); closeSort(); selMenuClose(); } });

  // Rail collection switch → re-scope in place instead of a full boosted reload.
  // Capture phase so we preempt hx-boost on the anchor. Only intercept collection
  // SCOPE links (All devices / a restaurant / group / release) while already in
  // roster mode; view=grid switches and grid/detail→roster transitions fall through
  // to the normal boosted navigation.
  document.addEventListener('click', function (e) {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var a = e.target.closest && e.target.closest('.fleet-rail a[href^="/devices"]');
    if (!a) return;
    if (!document.getElementById('device-table-container')) return;
    var u; try { u = new URL(a.href, location.origin); } catch (_) { return; }
    if (u.searchParams.get('view')) return; // grid switch → full nav
    e.preventDefault(); e.stopPropagation();
    scopeRoster(a, u);
  }, true);
}

// ── Menus on the toolbar (F2 filters, B1 selection) ─────────────────────────────
// Both are menu-kit panels with panes: a list, and a second step (a field's values, the
// checks) that replaces it with a back row, so nothing nests or flies out.
function flPane(menuId, name) {
  var m = document.getElementById(menuId); if (!m) return;
  m.querySelectorAll('.fl-mpane').forEach(function (p) { p.hidden = p.dataset.pane !== name; });
  var first = m.querySelector('.fl-mpane[data-pane="' + name + '"] input, .fl-mpane[data-pane="' + name + '"] .cc-mi:not(.fl-mback)');
  if (first && first.focus) first.focus({preventScroll: true});
}
function selMenuToggle(e) {
  if (e) { e.preventDefault(); e.stopPropagation(); }
  var m = document.getElementById('sel-menu'), b = document.getElementById('sel-more-btn');
  if (!m) return;
  if (!m.hidden) { selMenuClose(); return; }
  ffClose(); closeSort();
  var rm = document.getElementById('sel-rm-group');
  if (rm) rm.hidden = !getFilterVals().group;
  m.hidden = false; flPane('sel-menu', 'main'); fitFleetPop(m);
  if (b) { b.setAttribute('aria-expanded', 'true'); b.classList.add('open'); }
}
function selMenuClose() {
  var m = document.getElementById('sel-menu'), b = document.getElementById('sel-more-btn');
  if (m && !m.hidden) m.hidden = true;
  if (b) { b.setAttribute('aria-expanded', 'false'); b.classList.remove('open'); }
}

var ffState = { k: null };
function ffPlace(anchor) {
  var m = document.getElementById('ff-menu'), row = document.getElementById('fl-fchips');
  if (!m || !row || !anchor) return;
  var left = 0;
  if (row.contains(anchor)) {
    var a = anchor.getBoundingClientRect(), r = row.getBoundingClientRect();
    left = Math.max(0, Math.min(a.left - r.left, r.width - 280));
  }
  m.style.left = left + 'px';
}
function ffOpen(e, anchor, field) {
  if (e) { e.preventDefault(); e.stopPropagation(); }
  var m = document.getElementById('ff-menu'); if (!m) return;
  var add = document.getElementById('ff-add');
  // A second click on whatever opened the menu closes it — the "More" button and the
  // Release / Lock chips alike.
  if (!m.hidden && ffState.anchor === anchor) { ffClose(); return; }
  closeSort(); selMenuClose();
  ffState.anchor = anchor;
  m.hidden = false;
  ffPlace(anchor);
  if (add) add.setAttribute('aria-expanded', 'true');
  if (field) { ffShowField(field); }
  else {
    var q = document.getElementById('ff-find'); if (q) q.value = '';
    ffFind('');
    flPane('ff-menu', 'fields');
  }
  fitFleetPop(m);
}
function ffChip(e, btn, k) { ffOpen(e, btn.closest('#fl-fchips') ? btn : document.getElementById('ff-add'), k); }
function ffClose() {
  var m = document.getElementById('ff-menu'); if (m) m.hidden = true;
  var add = document.getElementById('ff-add'); if (add) add.setAttribute('aria-expanded', 'false');
  ffState.anchor = null;
}
function ffBack() { ffFind((document.getElementById('ff-find') || {}).value || ''); flPane('ff-menu', 'fields'); }
// The field list, or — while something is typed — the matching fields plus the values
// that match, so "v2.1.03" or "offline" is one Enter away.
function ffFind(q) {
  var box = document.getElementById('ff-fields'); if (!box) return;
  var p = new URLSearchParams(location.search), html = '', grp = '';
  q = (q || '').trim().toLowerCase();
  var fields = (window.FLEET_FIELDS || []).filter(function (f) { return f.vals && f.vals.length; });
  var quick = window.FLEET_QUICK_KEYS || [];
  fields.forEach(function (f) {
    // Status, Release and Lock have their own chips (F3); "More" lists the rest. Typed
    // searches still reach their values below.
    if (quick.indexOf(f.k) >= 0) return;
    if (q && f.label.toLowerCase().indexOf(q) < 0) return;
    if (!q && f.grp !== grp) { grp = f.grp; html += '<div class="cc-msec">' + ffEsc(grp) + '</div>'; }
    var cur = p.get(f.k);
    html += '<button type="button" class="cc-mi" role="menuitem" data-ff="' + f.k + '" onclick="ffShowField(\'' + f.k + '\')">' + ffEsc(f.label)
      + '<span class="rt">' + (cur ? '<b class="fl-cur">' + ffEsc(ffValLabel(f, cur)) + '</b>' : '') + '›</span></button>';
  });
  if (q) {
    var hits = [];
    fields.forEach(function (f) { f.vals.forEach(function (v) { if (String(v[1]).toLowerCase().indexOf(q) >= 0) hits.push([f, v]); }); });
    if (hits.length) {
      html += '<div class="cc-msec">Values</div>';
      hits.slice(0, 10).forEach(function (h) {
        html += '<button type="button" class="cc-mi" role="menuitem" data-ffk="' + h[0].k + '" data-ffv="' + ffEsc(h[1][0]) + '" onclick="ffPick(this.dataset.ffk, this.dataset.ffv)">' + ffEsc(h[1][1]) + '<span class="rt">' + ffEsc(h[0].label) + '</span></button>';
      });
    }
    if (!html) html = '<div class="cc-mi is-off is-static">Nothing matches “' + ffEsc(q) + '”</div>';
  }
  box.innerHTML = html;
}
function ffFindKey(e) {
  if (e.key === 'Enter') {
    e.preventDefault();
    var b = document.querySelector('#ff-fields .cc-mi:not(.is-off)'); if (b) b.click();
  } else if (e.key === 'ArrowDown') {
    e.preventDefault();
    var f = document.querySelector('#ff-fields .cc-mi:not(.is-off)'); if (f) f.focus();
  }
}
// A field's values, each with how many devices it would show in the current scope.
// Counts come from the server once per opening; a value that would show nothing is
// greyed out rather than offered as a dead end.
function ffShowField(k) {
  var f = ffField(k); if (!f) return;
  ffState.k = k;
  var p = new URLSearchParams(location.search), cur = p.get(k) || '';
  document.getElementById('ff-vtitle').textContent = f.label;
  var box = document.getElementById('ff-values');
  var rows = (f.vals || []).map(function (v) {
    return '<button type="button" class="cc-mi' + (v[0] === cur ? ' active' : '') + '" role="menuitemradio" aria-checked="' + (v[0] === cur) + '" data-v="' + ffEsc(v[0]) + '" onclick="ffPick(\'' + k + '\', this.dataset.v)">' + ffEsc(v[1]) + '<small class="ff-n">…</small></button>';
  }).join('');
  var any = '<button type="button" class="cc-mi' + (cur ? '' : ' active') + '" role="menuitemradio" aria-checked="' + (!cur) + '" onclick="ffPick(\'' + k + '\', \'\')">' + ffEsc(f.any || ('Any ' + f.label.toLowerCase())) + '</button>';
  box.innerHTML = '<div class="fl-mlist">' + rows + '</div><div class="cc-msep"></div>' + any;
  flPane('ff-menu', 'values');
  var q = new URLSearchParams(getFilterVals());
  q.delete(k); q.delete('page'); q.delete('sort'); q.delete('dir');
  q.set('field', k); q.set('vals', (f.vals || []).map(function (v) { return v[0]; }).join(','));
  fetch('/devices/filter-counts?' + q.toString(), { headers: { 'HX-Request': 'true' } })
    .then(function (r) { return r.ok ? r.json() : {}; })
    .then(function (n) {
      if (ffState.k !== k) return;
      box.querySelectorAll('.cc-mi[data-v]').forEach(function (b) {
        var c = n[b.dataset.v], el = b.querySelector('.ff-n');
        if (el) el.textContent = c == null ? '' : c;
        if (c === 0 && b.dataset.v !== cur) {
          // A release no device here runs is left out; other fields grey it.
          if (k === 'build') b.remove(); else { b.classList.add('is-off'); b.disabled = true; }
        }
      });
      // Releases: the one most devices here are on first.
      if (k === 'build') {
        var list = box.querySelector('.fl-mlist');
        [].slice.call(list.children).sort(function (a, b) { return (n[b.dataset.v] || 0) - (n[a.dataset.v] || 0); })
          .forEach(function (b) { list.appendChild(b); });
      }
    })
    .catch(function () { box.querySelectorAll('.ff-n').forEach(function (el) { el.textContent = ''; }); });
}
function ffPick(k, v) {
  ffClose();
  setFilter(k, v);
}
window.addEventListener('resize', function () { var m = document.getElementById('ff-menu'); if (m && !m.hidden && ffState.anchor) ffPlace(ffState.anchor); });

// Column-count control (1/2/3, persisted). The chosen count is remembered, but
// only takes effect when the list is wide enough (container queries) — a wider
// choice gracefully falls back to fewer columns when space is tight. The active
// highlight reflects the *rendered* column count, not just the saved preference.
function applyColsClass(n) {
  var list = document.querySelector('.dl-list');
  if (!list) return;
  list.classList.remove('cols-2', 'cols-3');
  if (n === 2) list.classList.add('cols-2');
  else if (n === 3) list.classList.add('cols-3');
}
function renderedCols() {
  var list = document.querySelector('.dl-list');
  if (!list) return 1;
  var gtc = getComputedStyle(list).gridTemplateColumns;
  if (!gtc || gtc === 'none') return 1;
  return gtc.split(' ').filter(Boolean).length;
}
function syncColsUI() {
  var n = renderedCols();
  document.querySelectorAll('.cols-seg button').forEach(function (b) { b.classList.toggle('on', String(n) === b.dataset.cols); });
}
function setCols(n) {
  applyColsClass(n);
  try { localStorage.setItem('mdm-cols', String(n)); } catch (e) {}
  syncColsUI();
}
function applyCols() {
  var n = 1;
  try { n = parseInt(localStorage.getItem('mdm-cols') || '1', 10) || 1; } catch (e) {}
  applyColsClass(n);
  syncColsUI();
}
document.addEventListener('DOMContentLoaded', applyCols);
window.addEventListener('resize', syncColsUI);
document.body.addEventListener('htmx:afterSwap', function (evt) {
  if (evt.detail && evt.detail.target && evt.detail.target.id === 'device-table-container') {
    applyCols();
    // A "select all matching" set survives plain page turns — reflect it on this
    // page's freshly-rendered checkboxes and refresh the bulk bar/count.
    if (selectAllMatchingSet) {
      document.querySelectorAll('.device-cb').forEach(function (box) {
        box.checked = selectAllMatchingSet.has(box.value);
      });
    }
    if (typeof updateBulkBar === 'function') updateBulkBar();
  }
});

// Search with debounce
var searchTimer;
document.getElementById('search-input').addEventListener('input', function() {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(function() {
    navigateDevices({page: '1'});
  }, 350);
});

// When set, a "Select all N matching" click resolved every serial matching the
// current roster filter (not just this page) — getSelectedSerials returns this
// instead of just the checked-on-this-page boxes. Cleared by unchecking Select
// All, navigating to a fresh filter/page, or unchecking any individual box.
var selectAllMatchingSet = null;
var selectAllMatchingSig = null; // filter signature (no page/page_size) the set was built for

function getSelectedSerials() {
  if (selectAllMatchingSet) return Array.from(selectAllMatchingSet);
  return Array.from(document.querySelectorAll('.device-cb:checked')).map(function(cb) { return cb.value; });
}

function clearSelectAllMatching() {
  selectAllMatchingSet = null;
  var btn = document.getElementById('select-all-matching');
  if (btn) { btn.hidden = true; btn.disabled = false; btn.textContent = ''; }
}

function selectAllMatching() {
  var btn = document.getElementById('select-all-matching');
  var allCb = document.getElementById('select-all');
  var total = allCb ? parseInt(allCb.dataset.filterTotal || '0', 10) : 0;
  if (btn) { btn.disabled = true; btn.textContent = 'Selecting…'; }
  fetch('/devices/select-serials' + location.search, { headers: { 'HX-Request': 'true' } })
    .then(function (r) { return r.json(); })
    .then(function (data) {
      selectAllMatchingSet = new Set(data.serials || []);
      selectAllMatchingSig = filterSig();
      if (btn) { btn.disabled = false; btn.hidden = true; }
      // Tick this page's boxes too, so the rows match the count.
      document.querySelectorAll('.device-cb').forEach(function (box) { box.checked = selectAllMatchingSet.has(box.value); });
      updateBulkBar();
    })
    .catch(function () {
      if (btn) { btn.disabled = false; btn.textContent = 'Select all ' + total; }
    });
}

function clearSelection() {
  selectAllMatchingSet = null;
  document.querySelectorAll('.device-cb').forEach(function (b) { b.checked = false; });
  var all = document.getElementById('select-all'); if (all) { all.checked = false; all.indeterminate = false; }
  updateBulkBar();
}
function updateBulkBar() {
  var serials = getSelectedSerials();
  try { syncPushUpdateBtn(); } catch (_) {}
  // Export lives in the page header and only makes sense with a selection.
  var exportBtn = document.getElementById('export-btn');
  if (exportBtn) {
    exportBtn.disabled = serials.length === 0;
    exportBtn.title = serials.length ? 'Export ' + serials.length + ' selected device' + (serials.length === 1 ? '' : 's') : 'Select devices to export them';
  }
  var allCb = document.getElementById('select-all');
  var allBoxes = document.querySelectorAll('.device-cb');
  var matchBtn = document.getElementById('select-all-matching');

  // If a matching-set selection is active, per-row checkboxes stay a mirror of it
  // (so a page you land on still shows the right boxes checked) — unchecking one
  // removes just that serial from the set rather than resetting to page-only.
  if (selectAllMatchingSet) {
    allBoxes.forEach(function (box) {
      if (!box.checked) selectAllMatchingSet.delete(box.value);
    });
    serials = getSelectedSerials();
  }

  // The selection bar takes the toolbar row while anything is ticked (B1).
  var bar = document.querySelector('.fleet-bar');
  if (bar) bar.classList.toggle('is-sel', serials.length > 0);
  if (!serials.length) selMenuClose();
  var count = document.getElementById('bulk-count');
  if (count) count.textContent = serials.length + ' selected' + (selectAllMatchingSet ? ' · all matching' : '');

  if (allCb) {
    if (selectAllMatchingSet || (allBoxes.length > 0 && serials.length === allBoxes.length)) {
      allCb.checked = true; allCb.indeterminate = false;
    } else if (serials.length > 0) {
      allCb.checked = false; allCb.indeterminate = true;
    } else {
      allCb.checked = false; allCb.indeterminate = false;
    }
  }

  // "Select all N" reaches past this page: offered while the filter matches more
  // devices than are ticked, and not once the whole matching set is selected.
  if (matchBtn) {
    var filterTotal = allCb ? parseInt(allCb.dataset.filterTotal || '0', 10) : 0;
    var showIt = !selectAllMatchingSet && serials.length > 0 && filterTotal > serials.length;
    matchBtn.hidden = !showIt;
    if (showIt && !matchBtn.disabled) matchBtn.textContent = 'Select all ' + filterTotal;
  }
}

function toggleSelectAll(cb) {
  if (!cb.checked) clearSelectAllMatching();
  document.querySelectorAll('.device-cb').forEach(function(box) { box.checked = cb.checked; });
  updateBulkBar();
}

// Open the Actions composer targeting the selected devices — one entry point for
// every action (reboot, kiosk, install, screenshot, …) instead of separate bulk buttons.
// Pager buttons live inside the live-refreshed list; delegate so a re-render never
// loses the click.
document.addEventListener('click', function (e) {
  var b = e.target.closest && e.target.closest('.page-nav[data-page]');
  if (!b || !b.closest('#device-table-container')) return;
  e.preventDefault();
  navigateDevices({ page: b.dataset.page });
});

function bulkExport() {
  var serials = getSelectedSerials();
  if (serials.length === 0) return;
  window.location.href = '/export?serials=' + encodeURIComponent(serials.join(','))
    + '&from=' + encodeURIComponent(location.pathname + location.search);
}

// Open another page with the selection carried along (?serials=A,B,C plus extras).
// Push update: our firmware OTA targets one model at a time, so the selection must be
// MDM Firmware devices of a single model; that model is passed on so the Deploy page
// opens with it selected. (The server re-checks, e.g. for "select all N matching".)
function pushUpdateCheck() {
  var cards = Array.from(document.querySelectorAll('.device-cb:checked')).map(function (cb) { return cb.closest('.dl-card'); }).filter(Boolean);
  var models = {}, notFw = 0;
  cards.forEach(function (c) {
    if (c.dataset.agent !== 'firmware') { notFw++; return; }
    models[c.dataset.model] = c.dataset.modelLabel || c.dataset.model;
  });
  var keys = Object.keys(models);
  if (notFw) return { ok: false, why: 'Push update is only for Firmware MDM devices — ' + notFw + ' selected ' + (notFw === 1 ? 'is' : 'are') + ' Standard MDM / MDM Lite.' };
  if (keys.length > 1) return { ok: false, why: 'Select devices of one model to push an update (selected: ' + keys.map(function (k) { return models[k]; }).join(', ') + ').' };
  return { ok: true, model: keys[0] || '' };
}
function pushUpdateSel() {
  var c = pushUpdateCheck();
  if (!c.ok) { if (window.showToast) showToast(c.why, 'error'); return; }
  bulkGo('/updates/new', c.model ? { product: c.model } : {});
}
function syncPushUpdateBtn() {
  var b = document.getElementById('push-update-btn'); if (!b) return;
  var c = pushUpdateCheck();
  b.classList.toggle('is-disabled', !c.ok);
  b.setAttribute('aria-disabled', c.ok ? 'false' : 'true');
  b.title = c.ok ? 'Open Deploy with these devices pre-ticked' + (c.model ? ' and their model selected' : '') : c.why;
}

function bulkGo(path, extra) {
  var serials = getSelectedSerials();
  if (serials.length === 0) return;
  var q = new URLSearchParams(); q.set('serials', serials.join(','));
  Object.keys(extra || {}).forEach(function (k) { q.set(k, extra[k]); });
  window.location.href = path + '?' + q.toString();
}
// Run an action on the selection, here, rather than handing the selection to the Actions
// console. The set IS the noun: you filter the fleet down to the devices you mean, select
// them, and act — so there is no page whose whole job is "pick a verb, then pick targets".
//
// It posts to the same POST /commands every other send uses, so nothing is reimplemented:
// the per-type role allowlist, skip-already-installed, the APK size/ETag lookup, batching
// and the audit entry all still apply, and the response redirects to the action's page
// where the per-device timeline lives.
function bulkRun(type, fields, confirmMsg) {
  var serials = getSelectedSerials();
  if (serials.length === 0) return;
  var n = serials.length;
  if (confirmMsg && !window.confirm(confirmMsg.replace('{n}', n) +
      '\n\n' + serials.slice(0, 8).join('\n') + (n > 8 ? '\n…and ' + (n - 8) + ' more' : ''))) return;
  var f = document.createElement('form');
  f.method = 'POST'; f.action = '/commands'; f.style.display = 'none';
  var add = function (k, v) {
    var i = document.createElement('input');
    i.type = 'hidden'; i.name = k; i.value = v; f.appendChild(i);
  };
  add('type', type);
  add('target_type', 'devices');
  add('target_serials', serials.join('\n'));
  Object.keys(fields || {}).forEach(function (k) { add(k, fields[k]); });
  document.body.appendChild(f);
  f.submit();
}

// Shell needs its line before it can run, so it gets a prompt rather than a sheet: the
// bulk bar lives inside a scrollable panel, where an absolutely-positioned popover is
// clipped (the same reason More expands in place).
function bulkShell() {
  var serials = getSelectedSerials();
  if (serials.length === 0) return;
  var cmd = window.prompt('Shell command to run on ' + serials.length + ' device' +
    (serials.length === 1 ? '' : 's') + ':\n\nOutput comes back per device on the action page.', '');
  if (cmd === null) return;
  cmd = cmd.trim();
  if (!cmd) return;
  bulkRun('shell', { shell_cmd: cmd });
}

// Wipe and unenroll cannot be undone and a selection can carry hundreds of devices, so
// these ask for the word rather than for a click.
function bulkDestructive(type, word) {
  var serials = getSelectedSerials();
  if (serials.length === 0) return;
  var n = serials.length;
  var answer = window.prompt(
    (type === 'wipe' ? 'Factory-reset' : 'Unenroll') + ' ' + n + ' device' + (n === 1 ? '' : 's') +
    '. This cannot be undone.\n\n' + serials.slice(0, 6).join('\n') + (n > 6 ? '\n…and ' + (n - 6) + ' more' : '') +
    '\n\nType ' + word + ' to confirm:', '');
  if (answer === null || answer.trim().toUpperCase() !== word) return;
  bulkRun(type);
}

function bulkCopySerials() {
  var serials = getSelectedSerials();
  if (serials.length === 0) return;
  var text = serials.join('\n');
  var done = function () { if (window.showToast) showToast('Copied ' + serials.length + ' serial' + (serials.length === 1 ? '' : 's'), 'success'); };
  if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(done, function () { ssCopyFallback(text); done(); });
  else { ssCopyFallback(text); done(); }
}
// Copy one serial off the card (the serial-leads and column layouts). Delegated, so it
// survives the htmx re-renders; capture phase + stopPropagation keeps the row's own
// click-to-open from firing behind it.
function fleetCopySerial(btn) {
  var v = btn.getAttribute('data-copy');
  if (!v) return;
  var done = function () { if (window.showToast) showToast('Copied ' + v, 'success'); };
  if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(v).then(done, function () { ssCopyFallback(v); done(); });
  else { ssCopyFallback(v); done(); }
}
document.addEventListener('click', function (e) {
  var b = e.target.closest && e.target.closest('.dl-copy');
  if (!b) return;
  e.preventDefault(); e.stopPropagation();
  fleetCopySerial(b);
}, true);
function ssCopyFallback(text) {
  var ta = document.createElement('textarea'); ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
  document.body.appendChild(ta); ta.select(); try { document.execCommand('copy'); } catch (e) {} ta.remove();
}
// Put the current selection into a form as hidden serials inputs.
function ssFillSerials(form) {
  var serials = getSelectedSerials();
  form.querySelectorAll('input[name="serials"]').forEach(function (i) { i.remove(); });
  serials.forEach(function (s) { var i = document.createElement('input'); i.type = 'hidden'; i.name = 'serials'; i.value = s; form.appendChild(i); });
  return serials.length;
}
function ssOpen(id) { var m = document.getElementById(id); if (m) m.classList.add('modal-open'); }
function ssClose(id, event) { var m = document.getElementById(id); if (!m) return; if (!event || event.target === m) m.classList.remove('modal-open'); }
// Save as group: the Add-to-group modal, pre-set to "new group" with a suggested name.
function bulkSaveAsGroup() {
  var serials = getSelectedSerials();
  if (serials.length === 0) return;
  openBulkGroupModal();
  var sel = document.getElementById('bulk-group-select');
  if (sel) { sel.value = '__new__'; onBulkGroupSelect(); }
  var name = document.getElementById('bulk-group-name');
  if (name && !name.value) {
    var d = new Date(), mon = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'][d.getMonth()];
    name.value = 'Selection ' + mon + ' ' + d.getDate() + ' · ' + serials.length;
    name.select();
  }
}
// Rename: pattern with {n}, {serial}, {last4}; live preview of the first names.
function openBulkRenameModal() {
  var serials = getSelectedSerials();
  if (serials.length === 0) return;
  var c = document.getElementById('bulk-rename-count'); if (c) c.textContent = serials.length;
  ssPreviewRename();
  ssOpen('bulk-rename-modal');
  var inp = document.getElementById('bulk-rename-pattern'); if (inp) { inp.focus(); inp.select(); }
}
function ssPreviewRename() {
  var serials = getSelectedSerials();
  var pat = (document.getElementById('bulk-rename-pattern') || {}).value || '';
  var start = parseInt((document.getElementById('bulk-rename-start') || {}).value, 10); if (!(start > 0)) start = 1;
  var out = document.getElementById('bulk-rename-preview'); if (!out) return;
  if (!pat.trim()) { out.innerHTML = '<span class="muted">Empty pattern clears the nicknames.</span>'; return; }
  var rows = serials.slice(0, 4).map(function (s, i) {
    var n = pat.replace(/\{n\}/g, String(start + i)).replace(/\{serial\}/g, s).replace(/\{last4\}/g, s.slice(-4)).slice(0, 40);
    return '<div><code>' + ssEscape(s) + '</code> → <b>' + ssEscape(n) + '</b></div>';
  });
  if (serials.length > 4) rows.push('<div class="muted">… and ' + (serials.length - 4) + ' more</div>');
  out.innerHTML = rows.join('');
}
function ssEscape(x) { return String(x == null ? '' : x).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
function submitBulkRename(event) {
  var f = event.target;
  if (!ssFillSerials(f)) { event.preventDefault(); return false; }
  return true;
}
// Set type (admin): one class for the whole selection.
// Retire (admin): out of the active fleet, history kept; a check-in brings a device back.
function bulkRetire() {
  var serials = getSelectedSerials();
  if (serials.length === 0) return;
  if (!confirm('Retire ' + serials.length + ' device' + (serials.length === 1 ? '' : 's') + '?\n\nThey leave lists, alerts and policy coverage but keep their history. A later check-in brings a device back.')) return;
  var f = document.getElementById('bulk-retire-form'); if (!f) return;
  ssFillSerials(f); f.submit();
}

// Remove the selected devices from the group currently filtering the roster.
// Only wired up when a group filter is active (?group=<id>); reloads to reflect
// the shrunken roster.
function bulkRemoveFromGroup(gid) {
  var serials = getSelectedSerials();
  if (serials.length === 0) return;
  if (!confirm('Remove ' + serials.length + ' device(s) from this group?')) return;
  var body = new URLSearchParams();
  serials.forEach(function (s) { body.append('serials', s); });
  fetch('/groups/' + gid + '/devices/remove', { method: 'POST', body: body, headers: { 'HX-Request': 'true' } })
    .then(function (r) { if (r.ok) window.location.reload(); })
    .catch(function () {});
}

function openBulkGroupModal() {
  var serials = getSelectedSerials();
  if (serials.length === 0) return;
  var form = document.getElementById('bulk-group-form');
  form.querySelectorAll('input[name="serials"]').forEach(function(i) { i.remove(); });
  serials.forEach(function(s) {
    var inp = document.createElement('input');
    inp.type = 'hidden';
    inp.name = 'serials';
    inp.value = s;
    form.appendChild(inp);
  });
  document.getElementById('bulk-group-name').value = '';
  document.getElementById('bulk-group-count').textContent = serials.length;
  onBulkGroupSelect();
  document.getElementById('bulk-group-modal').classList.add('modal-open');
  document.getElementById('bulk-group-select').focus();
}

// Reveal the "new group name" field and adjust the submit label depending on
// whether an existing group or the "＋ New group…" option is chosen.
function onBulkGroupSelect() {
  var sel = document.getElementById('bulk-group-select');
  var isNew = sel && sel.value === '__new__';
  var newRow = document.getElementById('bulk-group-new-row');
  var nameInp = document.getElementById('bulk-group-name');
  var submit = document.getElementById('bulk-group-submit');
  if (newRow) newRow.style.display = isNew ? '' : 'none';
  if (nameInp) nameInp.required = !!isNew;
  if (submit) submit.textContent = isNew ? 'Create group & add' : 'Add to group';
  if (isNew && nameInp) nameInp.focus();
}

// Route the submit to the right endpoint: create a new group (/groups) or add the
// selection to an existing one (/groups/{id}/devices). A native submit is used so it
// works whether or not htmx boosting is active.
function submitBulkGroup(event) {
  if (event) event.preventDefault();
  var form = document.getElementById('bulk-group-form');
  var sel = document.getElementById('bulk-group-select');
  var val = sel ? sel.value : '__new__';
  if (val === '__new__') {
    var name = (document.getElementById('bulk-group-name').value || '').trim();
    if (!name) { document.getElementById('bulk-group-name').focus(); return false; }
    form.action = '/groups';
  } else {
    form.action = '/groups/' + encodeURIComponent(val) + '/devices';
  }
  form.submit();
  return false;
}

function closeBulkGroupModal(event) {
  if (!event || event.target === document.getElementById('bulk-group-modal')) {
    document.getElementById('bulk-group-modal').classList.remove('modal-open');
  }
}

function openBulkRestaurantModal() {
  var serials = getSelectedSerials();
  if (serials.length === 0) return;
  var form = document.getElementById('bulk-restaurant-form');
  form.querySelectorAll('input[name="serials"]').forEach(function(i) { i.remove(); });
  serials.forEach(function(s) {
    var inp = document.createElement('input');
    inp.type = 'hidden'; inp.name = 'serials'; inp.value = s;
    form.appendChild(inp);
  });
  document.getElementById('bulk-rest-count').textContent = serials.length;
  document.getElementById('bulk-restaurant-modal').classList.add('modal-open');
}

function closeBulkRestaurantModal(event) {
  if (!event || event.target === document.getElementById('bulk-restaurant-modal')) {
    document.getElementById('bulk-restaurant-modal').classList.remove('modal-open');
  }
}

// Apps installed across the currently-selected devices, fetched when the modal
// opens. total = number of selected devices; an app on fewer than total of them is
// "partial" (greyed, usable with a warning).
var bulkKioskTotal = 0;
var bulkKioskAppByPkg = {};

function buildBulkKioskList(apps, total) {
  var list = document.getElementById('bulk-kiosk-pkg-list');
  // Keep the static "None" option (first child); drop any previously built options.
  while (list.children.length > 1) list.removeChild(list.lastChild);
  bulkKioskAppByPkg = {};
  apps.forEach(function (a) {
    bulkKioskAppByPkg[a.package_name] = a;
    var partial = a.device_count < total;
    var opt = document.createElement('div');
    opt.className = 'pkg-opt' + (partial ? ' pkg-opt-partial' : '');
    opt.setAttribute('data-value', a.package_name);
    opt.setAttribute('data-search', (a.app_name || '') + ' ' + a.package_name);
    var name = document.createElement('span');
    name.className = 'pkg-opt-name';
    name.textContent = a.app_name ? a.app_name : a.package_name;
    var pkg = document.createElement('span');
    pkg.className = 'pkg-opt-pkg mono';
    pkg.textContent = a.package_name;
    opt.appendChild(name); opt.appendChild(pkg);
    if (partial) {
      var badge = document.createElement('span');
      badge.className = 'pkg-opt-badge';
      badge.textContent = a.device_count + '/' + total;
      badge.title = 'Installed on ' + a.device_count + ' of ' + total + ' selected devices';
      opt.appendChild(badge);
    }
    list.appendChild(opt);
  });
}

function updateBulkKioskWarn() {
  var warn = document.getElementById('bulk-kiosk-warn');
  var pkg = document.getElementById('bulk-kiosk-package').value.trim();
  if (!pkg) { warn.style.display = 'none'; return; }
  var a = bulkKioskAppByPkg[pkg];
  var total = bulkKioskTotal;
  if (!a) {
    warn.textContent = '"' + pkg + '" isn\'t installed on any selected device. Kiosk will be set on all ' + total + ', but won\'t lock until the app is installed.';
    warn.style.display = '';
  } else if (a.device_count < total) {
    var missing = total - a.device_count;
    warn.textContent = 'Installed on only ' + a.device_count + ' of ' + total + ' selected devices. Kiosk will be set on all ' + total + ', but won\'t lock on the other ' + missing + ' until the app is installed.';
    warn.style.display = '';
  } else {
    warn.style.display = 'none';
  }
}

function openBulkKioskModal() {
  var serials = getSelectedSerials();
  if (serials.length === 0) return;
  var form = document.getElementById('bulk-kiosk-form');
  form.querySelectorAll('input[name="serials"]').forEach(function(i) { i.remove(); });
  var fd = new FormData();
  serials.forEach(function(s) {
    var inp = document.createElement('input');
    inp.type = 'hidden';
    inp.name = 'serials';
    inp.value = s;
    form.appendChild(inp);
    fd.append('serials', s);
  });
  bulkKioskTotal = serials.length;
  document.getElementById('bulk-kiosk-count').textContent = serials.length;
  document.getElementById('bulk-kiosk-toggle').checked = true;
  document.getElementById('bulk-kiosk-enabled').value = '1';
  document.getElementById('bulk-kiosk-package').value = '';
  document.getElementById('bulk-kiosk-warn').style.display = 'none';
  var bs = document.getElementById('bulk-kiosk-pkg-search');
  if (bs) bs.value = '';
  buildBulkKioskList([], serials.length);
  onBulkKioskToggle();
  document.getElementById('bulk-kiosk-modal').classList.add('modal-open');
  if (bs) bs.focus();
  // Fetch the apps common to / present on the selected devices.
  fetch('/devices/bulk-kiosk-apps', { method: 'POST', body: fd })
    .then(function (r) { return r.json(); })
    .then(function (j) {
      bulkKioskTotal = j.total || serials.length;
      buildBulkKioskList(j.apps || [], bulkKioskTotal);
    })
    .catch(function () {});
}

// Mirror the device-detail kiosk dropdown: a switch drives the hidden enabled flag
// and hides the app picker when disabling.
function onBulkKioskToggle() {
  var enabled = document.getElementById('bulk-kiosk-toggle').checked;
  document.getElementById('bulk-kiosk-enabled').value = enabled ? '1' : '0';
  document.getElementById('bulk-kiosk-pkg-row').style.display = enabled ? '' : 'none';
}

// Searchable "Locked app" combo for the bulk modal. The option list is rebuilt per
// selection (see buildBulkKioskList), so the combo queries .pkg-opt live and uses
// event delegation. Free-typed text is committed too, so you can pin a package no
// selected device reports yet (it surfaces a warning).
(function () {
  var combo = document.getElementById('bulk-kiosk-pkg-combo');
  if (!combo) return;
  var input = document.getElementById('bulk-kiosk-pkg-search');
  var hidden = document.getElementById('bulk-kiosk-package');
  var list = document.getElementById('bulk-kiosk-pkg-list');
  var active = -1;

  function allOpts() { return [].slice.call(list.querySelectorAll('.pkg-opt')); }
  function open() { combo.classList.add('open'); input.setAttribute('aria-expanded', 'true'); }
  function close() { combo.classList.remove('open'); input.setAttribute('aria-expanded', 'false'); active = -1; paint(); }
  function visible() { return allOpts().filter(function (o) { return !o.classList.contains('hidden'); }); }
  function paint() {
    allOpts().forEach(function (o) { o.classList.remove('active'); });
    var vis = visible();
    if (active >= 0 && active < vis.length) { vis[active].classList.add('active'); vis[active].scrollIntoView({ block: 'nearest' }); }
  }
  function filter(q) {
    q = q.trim().toLowerCase();
    allOpts().forEach(function (o) {
      var s = (o.getAttribute('data-search') || '').toLowerCase();
      o.classList.toggle('hidden', q !== '' && s.indexOf(q) === -1);
    });
    active = -1; paint();
  }
  function choose(o) {
    hidden.value = o.getAttribute('data-value');
    input.value = (o.querySelector('.pkg-opt-name') || o).textContent.trim();
    updateBulkKioskWarn();
    close();
  }

  input.addEventListener('focus', function () { filter(''); open(); input.select(); });
  input.addEventListener('input', function () { hidden.value = input.value.trim(); updateBulkKioskWarn(); filter(input.value); open(); });
  input.addEventListener('keydown', function (e) {
    var vis = visible();
    if (e.key === 'ArrowDown') { e.preventDefault(); open(); active = Math.min(active + 1, vis.length - 1); paint(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); active = Math.max(active - 1, 0); paint(); }
    else if (e.key === 'Enter') { if (combo.classList.contains('open') && active >= 0) { e.preventDefault(); choose(vis[active]); } }
    else if (e.key === 'Escape') { close(); }
  });
  list.addEventListener('mousedown', function (e) {
    var o = e.target.closest('.pkg-opt');
    if (o) { e.preventDefault(); choose(o); }
  });
  document.addEventListener('click', function (e) { if (!combo.contains(e.target)) close(); });

  // The server rejects enabling kiosk with no package; block the submit and nudge
  // the picker instead of letting it 400 (mirrors the per-device toggle guard).
  var form = document.getElementById('bulk-kiosk-form');
  if (form) form.addEventListener('submit', function (e) {
    if (document.getElementById('bulk-kiosk-toggle').checked && !hidden.value.trim()) {
      e.preventDefault();
      input.focus();
      input.classList.add('kiosk-need');
      setTimeout(function () { input.classList.remove('kiosk-need'); }, 1200);
    }
  });
})();

function closeBulkKioskModal(event) {
  if (!event || event.target === document.getElementById('bulk-kiosk-modal')) {
    document.getElementById('bulk-kiosk-modal').classList.remove('modal-open');
  }
}

// The saved table density is applied on load by the inline script before this file's tag.
document.body.addEventListener('htmx:afterSwap', function(evt) {
  var density = localStorage.getItem('mdm-density') || window.FLEET_DENSITY;
  if (evt.detail.target.id === 'device-table-container' && density === 'compact') {
    evt.detail.target.classList.add('compact');
  }
});

var pendingMassAction = null;

function confirmMassAction(opts) {
  pendingMassAction = opts;
  document.getElementById('confirm-count').textContent = opts.count;
  document.getElementById('confirm-action-name').textContent = opts.action;
  document.getElementById('confirm-understand').checked = false;
  document.getElementById('confirm-input').value = '';
  document.getElementById('confirm-modal').classList.add('modal-open');
  document.getElementById('confirm-input').focus();
}

function closeConfirmModal(event) {
  if (!event || event.target === document.getElementById('confirm-modal')) {
    document.getElementById('confirm-modal').classList.remove('modal-open');
    pendingMassAction = null;
  }
}

function submitConfirmedMassAction() {
  if (!pendingMassAction) return;
  var checked = document.getElementById('confirm-understand').checked;
  var typed = document.getElementById('confirm-input').value.trim();
  if (!checked || typed !== String(pendingMassAction.count)) return;
  var action = pendingMassAction;
  closeConfirmModal();
  action.onConfirm();
}

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') closeConfirmModal();
  if (e.key === 'Escape') closeBulkGroupModal();
});
