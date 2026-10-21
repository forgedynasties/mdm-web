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

// D1 (21 Oct 2026): the State and Venue pills filter the device rows in the page. The
// live region is re-rendered on every refresh, so the choice is kept here and applied
// again after each swap.
(function () {
  if (window.__dp1Filters) return;
  window.__dp1Filters = true;
  var F = {state: '', venue: ''};
  function apply() {
    var rows = document.querySelectorAll('.dp1-r'), shown = 0;
    rows.forEach(function (r) {
      var ok = (!F.state || r.dataset.state === F.state) && (!F.venue || r.dataset.venue === F.venue);
      r.hidden = !ok;
      if (ok) shown++;
    });
    var none = document.querySelector('.dp1-none');
    if (none) none.hidden = shown > 0 || rows.length === 0;
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
  }
  document.addEventListener('click', function (e) {
    var i = e.target.closest('[data-dp1-set]');
    if (!i) return;
    F[i.dataset.dp1Set] = i.dataset.val;
    if (window.ocCloseMenus) window.ocCloseMenus();
    apply();
  });
  document.body.addEventListener('htmx:afterSwap', function (e) {
    if (e.target && e.target.id === 'deployment-targets') apply();
  });
  document.addEventListener('htmx:load', apply);
})();
