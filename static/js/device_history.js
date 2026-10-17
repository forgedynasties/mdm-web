// Loaded by templates/device.html.
function toggleSection(header) {
  var body = header.nextElementSibling;
  var open = body.style.display !== 'none';
  if (open) {
    body.style.display = 'none';
    header.classList.remove('open');
    header.setAttribute('aria-expanded', 'false');
  } else {
    body.style.display = '';
    header.classList.add('open');
    header.setAttribute('aria-expanded', 'true');
    var card = header.closest('.card');
    if (card && window.htmx && card.hasAttribute('hx-trigger')) {
      window.htmx.trigger(card, 'load');
    }
  }
}

function showCheckin(row) {
  var checkinTime = new Date(row.dataset.time);
  document.getElementById('modal-time').textContent    = isNaN(checkinTime.getTime()) ? row.dataset.time : checkinTime.toLocaleString([], window.tzArg({}));
  document.getElementById('modal-battery').textContent = row.dataset.battery + '%';
  document.getElementById('modal-build').textContent   = row.dataset.build;
  try {
    document.getElementById('modal-extra').textContent = JSON.stringify(JSON.parse(row.dataset.extra), null, 2);
  } catch(_) {
    document.getElementById('modal-extra').textContent = row.dataset.extra;
  }
  document.getElementById('checkin-modal').classList.add('modal-open');
}
function closeModal(e) {
  if (e.target === document.getElementById('checkin-modal')) {
    document.getElementById('checkin-modal').classList.remove('modal-open');
  }
}
document.addEventListener('keydown', function (e) {
  if (e.key === 'Escape') document.getElementById('checkin-modal').classList.remove('modal-open');
});

// Live offline-exit unlock code + countdown ring. The code is fetched from the server
// (works over plain HTTP, no browser crypto); the ring depletes locally each second and
// a fresh code is fetched only when the window rolls over. Re-initialises on htmx swaps.
(function () {
  function initOfflineCode() {
    var card = document.getElementById('oe-card');
    if (!card || card.dataset.oeInit === '1') return;
    card.dataset.oeInit = '1';
    var serial = card.dataset.serial;
    var period = parseInt(card.dataset.period, 10) || 60;
    var digits = parseInt(card.dataset.digits, 10) || 6;
    var codeEl = document.getElementById('oe-code');
    var ring = document.getElementById('oe-ring');
    var R = 15.5, C = 2 * Math.PI * R;
    ring.style.strokeDasharray = C.toFixed(2);
    var boundary = 0; // wall-clock secs when the current code expires
    var fetching = false;

    function group(c) { return (digits === 6 && c.length === 6) ? c.slice(0, 3) + ' ' + c.slice(3) : c; }

    // Reflect the server's kiosk on/off state in the chip pill + toggle live, so an
    // on-device offline exit (which flips kiosk off server-side) updates here without
    // a page reload. Updated in place — no fragment swap — so the popover/combo aren't
    // disturbed.
    function syncKiosk(enabled) { if (window.mdmSyncKiosk) window.mdmSyncKiosk(enabled); }

    function refresh() {
      if (fetching) return;
      fetching = true;
      fetch('/devices/' + encodeURIComponent(serial) + '/offline-code', { credentials: 'same-origin' })
        .then(function (r) { return r.json(); })
        .then(function (d) {
          fetching = false;
          if (!d) return;
          syncKiosk(d.kiosk_enabled);
          if (!d.enabled) return;
          if (d.code) codeEl.textContent = group(String(d.code));
          boundary = Date.now() / 1000 + (d.seconds || period);
        })
        .catch(function () { fetching = false; });
    }

    // Re-fetch immediately on each check-in (SSE), not just on window rollover, so the
    // kiosk state flips the moment the device reports its offline exit.
    if (!window._oeKioskSync) {
      window._oeKioskSync = true;
      document.body.addEventListener('device-updated', function () { refresh(); });
    }

    function tick() {
      var now = Date.now() / 1000;
      var remaining = boundary - now;
      if (remaining <= 0) { refresh(); remaining = period; }
      if (remaining > period) remaining = period;
      ring.style.transition = remaining > period - 1.2 ? 'none' : '';
      ring.style.strokeDashoffset = (C * (1 - remaining / period)).toFixed(2);
      card.classList.toggle('expiring', remaining <= 6);
    }

    // Stop the old timer if the card is swapped away, then start fresh.
    if (window._oeTimer) clearInterval(window._oeTimer);
    refresh();
    tick();
    window._oeTimer = setInterval(function () {
      if (!document.getElementById('oe-card')) { clearInterval(window._oeTimer); return; }
      tick();
    }, 1000);
  }
  document.addEventListener('DOMContentLoaded', initOfflineCode);
  document.body.addEventListener('htmx:load', initOfflineCode);
  document.body.addEventListener('htmx:afterSettle', initOfflineCode);
  initOfflineCode();
})();
