// Loaded by templates/deployment_detail.html.
// Live device-status section: the 3s poll only swaps when something actually
// changed, and the "Updated" column ticks locally so an idle poll stays byte-stable.
(function () {
  var box = document.getElementById('deployment-targets');
  if (!box) return;

  // Live progress via server push (SSE) instead of constant polling: the server
  // fires "deployment-update" when OTA progress ticks or anyone acts on the
  // deployment; we dispatch a body event that the targets div refetches on
  // (there's a slow 30s poll as a fallback if the stream drops).
  var DP = window.DEPLOYMENT_PAGE;
  if (DP.active && window.EventSource) {
    var des = window.mdmStream('/releases/' + DP.release + '/deployments/' + DP.deployment + '/events');
    des.addEventListener('deployment-update', function () {
      document.body.dispatchEvent(new CustomEvent('deployment-updated'));
    });
    window.addEventListener('beforeunload', function () { des.close(); });
  }

  // Skip the swap when the polled HTML is identical to what's on screen — otherwise
  // the table would re-render every 3s (and the browser can mask the server's 304).
  // Fail open: only cancel on a real, non-empty response that matches the last one,
  // so a missing/empty body can never suppress swaps and hide the table/progress bar.
  var lastHtml = null;
  document.body.addEventListener('htmx:beforeSwap', function (e) {
    if (!e.target || e.target.id !== 'deployment-targets') return;
    var html = e.detail && e.detail.serverResponse;
    if (typeof html !== 'string' || html === '') return; // let htmx swap as usual
    if (html === lastHtml) { e.detail.shouldSwap = false; }
    else { lastHtml = html; }
  });

  // A per-device action posts from inside the live region and its redirect (the whole page) is
  // swapped in there too; fetch the region again at once so it is the partial, not a page in a page.
  // A swap that brought a whole page into the region (the redirect after an action) is
  // replaced at once by the region's own partial.
  document.body.addEventListener('htmx:afterSwap', function (e) {
    if (!e.target || e.target.id !== 'deployment-targets' || !e.target.querySelector('.cc-top')) return;
    lastHtml = null;
    setTimeout(function () { document.body.dispatchEvent(new CustomEvent('deployment-updated')); }, 50);
  });
  var acting = false;
  document.body.addEventListener('htmx:beforeRequest', function (e) {
    var el = e.detail && e.detail.elt;
    acting = !!(el && el.closest && el.id !== 'deployment-targets' && el.closest('#deployment-targets') && (el.tagName === 'FORM' || el.tagName === 'BUTTON'));
  });
  document.body.addEventListener('htmx:afterRequest', function (e) {
    var el = e.detail && e.detail.elt;
    if (!acting || (el && el.id === 'deployment-targets')) return;
    acting = false; lastHtml = null;
    setTimeout(function () { document.body.dispatchEvent(new CustomEvent('deployment-updated')); }, 150);
  });

  // Relative "x ago" for the .reltime[data-ts] cells is rendered by the global
  // ticker (window.mdmTickRel in layout.html), which owns every .reltime span —
  // a second page-local ticker here would fight it over the same nodes.
})();

function dplOpen(id) { var m = document.getElementById(id); if (m) m.classList.add('modal-open'); }
function dplClose(id) { var m = document.getElementById(id); if (m) m.classList.remove('modal-open'); }
document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { dplClose('dpl-reboot-modal'); dplClose('dpl-push-modal'); } });
function setReboot(mode) {
  document.getElementById('reboot-input').value = mode;
  document.getElementById('scheduled-time-row').style.display = mode === 'scheduled' ? '' : 'none';
  document.querySelectorAll('#reboot-seg .seg-btn').forEach(function (b) {
    b.classList.toggle('active', b.dataset.mode === mode);
  });
}
function setDelivery(mode) {
  document.getElementById('addt-delivery').value = mode;
  document.querySelectorAll('#addt-delivery-seg .seg-btn').forEach(function (b) {
    b.classList.toggle('active', b.textContent.toLowerCase().indexOf(mode === 'full' ? 'full' : 'smart') !== -1);
  });
}
function setTargetTab(tab) {
  document.getElementById('tab-devices').style.display = tab === 'devices' ? '' : 'none';
  document.getElementById('tab-groups').style.display  = tab === 'groups'  ? '' : 'none';
  document.querySelectorAll('#target-seg .seg-btn').forEach(function(b) {
    b.classList.toggle('active', b.dataset.tab === tab);
  });
}
function filterSendList(ctrl) {
  var el = ctrl;
  while (el && !el.classList.contains('send-tab-panel')) el = el.parentElement;
  if (!el) return;
  var qEl = el.querySelector('input[type="search"]'), q = qEl ? qEl.value.toLowerCase() : '';
  var sEl = el.querySelector('.send-filter-status'), s = sEl ? sEl.value : '';
  var bEl = el.querySelector('.send-filter-batt'), b = bEl ? bEl.value : '';
  var visible = 0;
  el.querySelectorAll('.send-item').forEach(function(item) {
    var okQ = !q || item.dataset.search.toLowerCase().indexOf(q) !== -1;
    var okS = !s || item.dataset.online === (s === 'online' ? '1' : '0');
    var okB = !b || item.dataset.batt === b;
    var match = okQ && okS && okB;
    item.style.display = match ? '' : 'none';
    if (match) visible++;
  });
  var countEl = el.querySelector('.send-count');
  if (countEl) countEl.textContent = (q || s || b) ? visible + ' match' + (visible !== 1 ? 'es' : '') : '';
}
function toggleAll(master, name) {
  var panel = master.closest('.send-tab-panel');
  panel.querySelectorAll('input[name="' + name + '"]').forEach(function(cb) {
    if (cb.closest('.send-item').style.display !== 'none') cb.checked = master.checked;
  });
}

// DP1 (18 Nov 2026): the State and Venue pills and the search filter the device list in the
// page, and the picked device's detail is shown beside it. The live region is re-rendered on
// every refresh, so the choices (and the list's scroll) are kept here and applied again after
// each swap.
(function () {
  if (window.__dp1Filters) return;
  window.__dp1Filters = true;
  var F = {state: '', venue: '', q: '', sel: '', dep: ''};
  var keep = null;

  function rows() { return document.querySelectorAll('.dp1-r'); }
  function select(serial) {
    F.sel = serial;
    document.querySelectorAll('.dp1-det').forEach(function (d) { d.hidden = d.dataset.serial !== serial; });
    rows().forEach(function (r) { r.classList.toggle('on', r.dataset.serial === serial); });
  }
  function apply() {
    var dp = window.DEPLOYMENT_PAGE;
    if (dp && F.dep !== String(dp.deployment)) { F.state = F.venue = F.q = F.sel = ''; F.dep = String(dp.deployment); }
    var all = rows(), shown = 0, firstShown = null, selShown = false;
    all.forEach(function (r) {
      var ok = (!F.state || r.dataset.state === F.state) && (!F.venue || r.dataset.venue === F.venue) &&
        (!F.q || r.dataset.serial.toLowerCase().indexOf(F.q) !== -1);
      r.hidden = !ok;
      if (ok) { shown++; if (!firstShown) firstShown = r; if (r.dataset.serial === F.sel) selShown = true; }
    });
    var none = document.querySelector('.dp1-none');
    if (none) none.hidden = shown > 0 || all.length === 0;
    ['state', 'venue'].forEach(function (k) {
      var pill = document.querySelector('[data-dp1-pill="' + k + '"]');
      if (!pill) return;
      var label = 'All';
      document.querySelectorAll('[data-dp1-set="' + k + '"]').forEach(function (i) {
        var on = i.dataset.val === F[k];
        i.classList.toggle('active', on);
        if (on) label = i.textContent.replace(/ · \d+$/, '');
      });
      if (F[k] && label === 'All') { F[k] = ''; } // the choice is gone from the list
      pill.querySelector('b').textContent = label;
      pill.classList.toggle('set', !!F[k]);
    });
    var q = document.getElementById('dp1-q');
    if (q && q.value !== F.q) q.value = F.q;
    // keep the picked device while it is listed, else the first one that is
    if (all.length) {
      var exists = !!document.querySelector('.dp1-det[data-serial="' + (window.CSS && CSS.escape ? CSS.escape(F.sel) : F.sel) + '"]');
      if (!exists || !selShown) { if (firstShown) select(firstShown.dataset.serial); }
      else select(F.sel);
    }
    fit();
  }

  // Fit the page between the header and the dock on a desktop window; elsewhere it stacks.
  function fit() {
    var p = document.querySelector('.dp1p');
    if (!p) return;
    var html = document.documentElement;
    if (!html.classList.contains('shell') || innerWidth < 1101 || innerHeight < 640) {
      p.classList.remove('fit'); p.style.removeProperty('--dp1-h'); return;
    }
    p.classList.remove('fit');
    var main = document.getElementById('main');
    var bottom = main ? main.getBoundingClientRect().bottom : innerHeight - 16;
    var top = p.getBoundingClientRect().top + (main ? main.scrollTop : 0);
    var h = Math.floor(bottom - top - 8);
    if (h >= 420) { p.style.setProperty('--dp1-h', h + 'px'); p.classList.add('fit'); }
  }
  addEventListener('resize', fit);

  document.addEventListener('click', function (e) {
    var i = e.target.closest('[data-dp1-set]');
    if (i) {
      F[i.dataset.dp1Set] = i.dataset.val;
      if (window.ocCloseMenus) window.ocCloseMenus();
      apply();
      return;
    }
    var r = e.target.closest('.dp1-r');
    if (r) select(r.dataset.serial);
  });
  document.addEventListener('input', function (e) {
    if (e.target && e.target.id === 'dp1-q') { F.q = e.target.value.trim().toLowerCase(); apply(); }
  });
  // Arrow keys move through the list while it has focus.
  document.addEventListener('keydown', function (e) {
    var r = e.target.closest && e.target.closest('.dp1-r');
    if (!r || (e.key !== 'ArrowDown' && e.key !== 'ArrowUp')) return;
    var vis = Array.prototype.filter.call(rows(), function (x) { return !x.hidden; });
    var n = vis[vis.indexOf(r) + (e.key === 'ArrowDown' ? 1 : -1)];
    if (n) { e.preventDefault(); n.focus(); select(n.dataset.serial); }
  });

  // A refresh replaces the list: remember where it was scrolled and what had focus.
  document.body.addEventListener('htmx:beforeSwap', function (e) {
    if (!e.target || e.target.id !== 'deployment-targets') return;
    var l = document.querySelector('.dp1-list'), c = document.querySelector('.dp1-l'), d = document.querySelector('.dp1-dets');
    var a = document.activeElement;
    keep = {
      list: l ? l.scrollTop : 0, left: c ? c.scrollTop : 0, det: d ? d.scrollTop : 0,
      q: a && a.id === 'dp1-q' ? a.selectionStart : -1,
      row: a && a.classList && a.classList.contains('dp1-r') ? a.dataset.serial : ''
    };
  });
  document.body.addEventListener('htmx:afterSwap', function (e) {
    if (!e.target || e.target.id !== 'deployment-targets') return;
    apply();
    if (!keep) return;
    var l = document.querySelector('.dp1-list'), c = document.querySelector('.dp1-l'), d = document.querySelector('.dp1-dets');
    if (l) l.scrollTop = keep.list;
    if (c) c.scrollTop = keep.left;
    if (d) d.scrollTop = keep.det;
    if (keep.q >= 0) { var q = document.getElementById('dp1-q'); if (q) { q.focus(); q.setSelectionRange(keep.q, keep.q); } }
    if (keep.row) { var r = document.querySelector('.dp1-r[data-serial="' + keep.row + '"]'); if (r) r.focus({preventScroll: true}); }
    keep = null;
  });
  document.addEventListener('htmx:load', apply);
  apply();
})();
