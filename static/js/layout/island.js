// The top island's live moments (D3, picked 3 Nov 2026). The island at rest is the page's
// breadcrumb, numbers and buttons (layout.html fills it); on top of that:
//  - what a click did ("Sent to 3 devices") shows in the island for 4 s (6 s for an error),
//    instead of a toast in the corner;
//  - a new critical alert takes the island over until Open or Later, instead of the dock;
//  - commands this person started that are still running keep a small progress slot
//    (GET /island/jobs) that opens the job.
// On a phone, or a page with no island, the old toast stack and dock alert stay.
(function () {
  var slot = document.getElementById('appbar-island');
  if (!slot) return;
  function visible() { return slot.offsetParent !== null && slot.getBoundingClientRect().width > 200; }
  function esc(x) { return String(x == null ? '' : x).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }

  // ── A moment over the island ─────────────────────────────────────────────────
  var live = null, timer = null, sticky = false;
  function clear() {
    clearTimeout(timer);
    if (live) { var l = live; live = null; l.classList.remove('show'); setTimeout(function () { l.remove(); }, 200); }
    sticky = false;
  }
  function moment(html, cls, ms) {
    if (sticky && cls.indexOf('alert') < 0) return false; // an alert waits for Open / Later
    clear();
    live = document.createElement('div');
    live.className = 'isl-live ' + cls;
    live.setAttribute('role', cls.indexOf('err') >= 0 || cls.indexOf('alert') >= 0 ? 'alert' : 'status');
    live.innerHTML = html;
    slot.appendChild(live);
    requestAnimationFrame(function () { if (live) live.classList.add('show'); });
    if (ms) {
      timer = setTimeout(clear, ms);
      live.addEventListener('mouseenter', function () { clearTimeout(timer); });
      live.addEventListener('mouseleave', function () { timer = setTimeout(clear, 1500); });
    }
    var x = live.querySelector('[data-isl-x]'); if (x) x.addEventListener('click', clear);
    return true;
  }
  var ICON = {
    success: '<path d="M5 13l4 4L19 7"/>', info: '<circle cx="12" cy="12" r="9"/><path d="M12 8h.01M11 12h1v5h1"/>',
    error: '<path d="M12 8v5M12 16h.01"/><circle cx="12" cy="12" r="9"/>', warning: '<path d="M12 9v4M12 17h.01"/><path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"/>'
  };
  // showToast (chrome.js) now lands here when the island is on screen.
  var toast = window.showToast;
  window.showToast = function (msg, type) {
    type = ICON[type] ? type : (type === 'ok' ? 'success' : 'info');
    if (!visible()) return toast && toast(msg, type);
    var tone = type === 'error' ? 'err' : type === 'warning' ? 'warn' : type === 'success' ? 'ok' : 'info';
    if (!moment('<span class="il-ic ' + tone + '"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round">' + ICON[type] + '</svg></span><span class="il-m">' + esc(msg) + '</span><button type="button" class="il-x" data-isl-x aria-label="Dismiss">×</button>',
      'flash ' + tone, type === 'error' ? 6000 : 4000)) return toast && toast(msg, type);
  };
  // A new critical alert (chrome.js polls /dock/status and calls mdmIsland).
  var dockIsland = window.mdmIsland;
  window.mdmIsland = function (a) {
    if (!visible()) return dockIsland ? dockIsland(a) : false;
    moment('<span class="il-dot" aria-hidden="true"></span><span class="il-m"><b>' + esc(a.headline || 'New alert') + '</b>' + (a.sentence ? '<small>' + esc(a.sentence) + '</small>' : '') + '</span>' +
      '<a class="cc-btn sm primary" href="' + esc(a.href || '/alerts') + '">Open</a><button type="button" class="cc-btn sm" data-isl-x>Later</button>', 'alert', 0);
    sticky = true;
    if (live) live.querySelector('a').addEventListener('click', clear);
    return true;
  };

  // ── Jobs this person started ─────────────────────────────────────────────────
  var jobsEl = null, jobsTimer = null, busy = false;
  function drawJobs(js) {
    if (!js.length) { if (jobsEl) { jobsEl.remove(); jobsEl = null; } return false; }
    var j = js[0], pct = j.total ? Math.round(j.done * 100 / j.total) : 0;
    if (!jobsEl || !slot.contains(jobsEl)) {
      jobsEl = document.createElement('a'); jobsEl.className = 'isl-job';
      var acts = slot.querySelector('.isl-acts');
      slot.insertBefore(jobsEl, acts || null);
    }
    jobsEl.href = j.href;
    jobsEl.title = j.label + ': ' + j.done + ' of ' + j.total + ' done' + (j.failed ? ', ' + j.failed + ' failed' : '') + (js.length > 1 ? ' · and ' + (js.length - 1) + ' more' : '');
    jobsEl.innerHTML = '<span class="ij-ring" style="--p:' + pct + '%"><i></i></span><span class="ij-t">' + esc(j.label) + '</span><span class="ij-n">' + j.done + '/' + j.total + '</span>';
    return true;
  }
  function pollJobs() {
    clearTimeout(jobsTimer);
    if (busy || document.hidden || !visible()) { jobsTimer = setTimeout(pollJobs, 30000); return; }
    busy = true;
    fetch('/island/jobs', { credentials: 'same-origin', headers: { Accept: 'application/json' } })
      .then(function (r) { return r.ok ? r.json() : { jobs: [] }; })
      .then(function (d) { busy = false; jobsTimer = setTimeout(pollJobs, drawJobs(d.jobs || []) ? 8000 : 60000); })
      .catch(function () { busy = false; jobsTimer = setTimeout(pollJobs, 60000); });
  }
  // A command was just sent: look sooner.
  document.addEventListener('htmx:afterRequest', function (e) {
    var p = (e.detail && e.detail.pathInfo && e.detail.pathInfo.requestPath) || '';
    if (/\/commands|\/adb\/|\/reboot|\/lock|\/installs|\/follows/.test(p)) { clearTimeout(jobsTimer); jobsTimer = setTimeout(pollJobs, 2500); }
  });
  // The island is refilled on every navigation: put the slot back.
  var refill = window.mdmIslandRefresh;
  window.mdmIslandRefresh = function () { if (refill) refill(); jobsEl = null; pollJobs(); };
  document.addEventListener('htmx:afterSwap', function (e) {
    var t = e.detail && e.detail.target;
    if (t && t.tagName === 'MAIN') { jobsEl = null; clearTimeout(jobsTimer); jobsTimer = setTimeout(pollJobs, 300); if (!sticky) clear(); }
  });
  document.addEventListener('visibilitychange', function () { if (!document.hidden) pollJobs(); });
  setTimeout(pollJobs, 1200);
})();
