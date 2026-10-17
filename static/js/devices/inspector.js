// Loaded by templates/devices.html.
  // Manual refresh (used by refresh button and visibility change).
  (function () {
    var lastRefresh = Date.now();
    window.mdmRefreshDevices = function () {
      lastRefresh = Date.now();
      document.body.dispatchEvent(new CustomEvent('refresh-devices'));
    };
    document.addEventListener('visibilitychange', function () {
      if (document.hidden) return;
      if (Date.now() - lastRefresh > 120000) window.mdmRefreshDevices();
    });
  })();

  // motion: when the list re-fetches (refresh-devices), a card whose serial was not in
  // the previous render drops in with a green wash (motion.css .m-arrive) — a device
  // that enrolled while you were looking. First render is silent: everything is new.
  (function () {
    var seen = null;
    function serials() { var o = {}; document.querySelectorAll('#device-table-container .dl-card[data-serial]').forEach(function (c) { o[c.getAttribute('data-serial')] = 1; }); return o; }
    document.addEventListener('htmx:afterSwap', function (e) {
      if (!e.target || e.target.id !== 'device-table-container') return;
      var now = serials();
      if (seen) { for (var k in now) if (!seen[k]) { var c = document.querySelector('#device-table-container .dl-card[data-serial="' + k.replace(/"/g, '') + '"]'); if (c) c.classList.add('m-arrive'); } }
      seen = now;
    });
    document.addEventListener('DOMContentLoaded', function () { seen = serials(); });
    if (document.readyState !== 'loading') seen = serials();
  })();

  // Real-time row patching via SSE.
  (function () {
    if (!window.EventSource) return;

    function patchRow(d) {
      var card = document.querySelector('.dl-card[data-serial="' + d.serial + '"]');
      if (!card) return;

      // A device that just became hidden should leave the (hidden-excluded) fleet view.
      if (d.hidden) { card.remove(); return; }

      // Update only the build-id text node, NOT the whole .dl-rel — that span also
      // holds the .dl-product pill, and setting .dl-rel.textContent would wipe it
      // (the pill would vanish on live patch and only return on a full reload).
      var rel = card.querySelector('.dl-buildid');
      if (rel) rel.textContent = d.build_id;

      // Classic layout: the old battery chip (see device-cards-classic).
      var chip = card.querySelector('.dl-chip');
      if (chip && d.battery_missing) {
        // Pack absent on a product that has one (NTC fault / unplugged). Distinct from
        // the kiosk mains chip below, and never repainted as a percentage.
        chip.className = 'dl-chip battery-low';
        chip.setAttribute('aria-label', 'No battery detected');
        chip.setAttribute('title', 'No battery detected — the charger reports the pack absent (NTC thermistor open/out of range, or unplugged). Charging is suspended; any percentage would be read off the rail.');
        chip.innerHTML = '<span class="bat-glyph"><svg viewBox="0 0 30 15" width="18" height="11" aria-hidden="true"><rect x="1" y="1" width="24" height="13" rx="3" fill="none" stroke="currentColor" stroke-width="1.5"/><rect x="26.5" y="4.5" width="2.5" height="6" rx="1" fill="currentColor"/><path d="M6 4l12 8M18 4L6 12" stroke="currentColor" stroke-width="1.5"/></svg></span><span class="battery-label">None</span>';
      } else if (chip && chip.classList.contains('dl-tv')) {
        // TV box / dongle: the chip names the device, it carries no live reading.
      } else if (chip && !d.has_battery) {
        // Wall-powered kiosk: keep the mains socket chip; never repaint it as a battery
        // (which would show a bogus 0%). Mirrors the server-rendered dl-mains chip.
        chip.className = 'dl-chip dl-mains';
        chip.setAttribute('aria-label', 'AC powered');
        chip.setAttribute('title', 'AC powered — no battery');
        chip.innerHTML = '<span class="bat-glyph"><svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><circle cx="12" cy="12" r="9.5"/><circle cx="8.5" cy="12" r="1.4" fill="currentColor" stroke="none"/><circle cx="15.5" cy="12" r="1.4" fill="currentColor" stroke="none"/></svg></span>';
      } else if (chip) {
        chip.className = 'dl-chip ' + d.battery_class;
        chip.setAttribute('aria-label', 'Battery ' + d.battery_pct + '%');
        var glyph = d.flapping
          ? '<span class="bat-glyph dl-flap" title="Charger flapping — charging toggling ~' + d.flap_rate + '×/min; faulty charger, dock, or cable suspected" aria-label="Charger flapping — faulty charger suspected"><svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 2v5M15 2v5M7 7h10v3a5 5 0 0 1-10 0zM12 15v7"/><path d="M3 3l18 18"/></svg></span>'
          : d.charging
          ? '<span class="bat-glyph" title="Charging"><svg viewBox="0 0 24 24" width="12" height="12" fill="currentColor" aria-hidden="true"><path d="M13 2L4 14h7l-1 8 9-12h-7z"/></svg></span>'
          : '<span class="bat-glyph"><svg viewBox="0 0 30 15" width="18" height="11" aria-hidden="true"><rect x="1" y="1" width="24" height="13" rx="3" fill="none" stroke="currentColor" stroke-width="1.5"/><rect x="26.5" y="4.5" width="2.5" height="6" rx="1" fill="currentColor"/><rect x="3" y="3" width="' + Math.max(2, Math.round(d.battery_pct * 18 / 100)) + '" height="9" rx="1.5" fill="currentColor"/></svg></span>';
        chip.innerHTML = glyph + '<span class="battery-label">' + d.battery_pct + '%</span>';
      }

      // Power glyph (mirrors the device-cards template). TV boxes have no battery by
      // design, so they never repaint as a pack reading or a missing pack.
      var dongle = card.dataset.kind === 'dongle';
      var pv = card.querySelector('[data-m="power"] .pv');
      if (pv) {
        if (d.battery_missing && !dongle) {
          pv.innerHTML = '<span class="pw danger" role="img" aria-label="No battery detected" title="No battery detected — the charger reports the pack absent (NTC thermistor open/out of range, or unplugged). Charging is suspended; any percentage would be read off the rail."><svg class="g-bat" viewBox="0 0 26 14" aria-hidden="true"><use href="#dk-batx"/></svg></span>';
        } else if (d.has_battery && !dongle) {
          var fill = Math.max(2, Math.min(18, Math.round(d.battery_pct * 18 / 100)));
          var mk = d.flapping ? '<svg class="mk flap" viewBox="0 0 24 24" aria-hidden="true"><use href="#dk-flap"/></svg>'
            : d.charging ? '<svg class="mk chg" viewBox="0 0 24 24" aria-hidden="true"><use href="#dk-bolt"/></svg>' : '';
          pv.innerHTML = '<span class="pw pw-bat ' + d.battery_class + '" role="img" aria-label="Battery ' + d.battery_pct + '%' +
            (d.flapping ? ', charger flapping" title="Charger flapping — charging toggling ~' + d.flap_rate + '×/min; faulty charger, dock, or cable suspected"' : d.charging ? ', charging" title="Charging"' : '"') + '>' +
            '<span class="v"><span class="pc">' + d.battery_pct + '%</span><span class="mk-s">' + mk + '</span></span><span class="gb">' +
            '<svg class="g-bat" viewBox="0 0 26 14" aria-hidden="true"><rect x="1" y="1" width="21" height="12" rx="3" fill="none" stroke="currentColor" stroke-width="1.5"/><rect x="23.3" y="4.5" width="2" height="5" rx="1" fill="currentColor"/><rect x="3" y="3" width="' + fill + '" height="8" rx="1.5" fill="currentColor" transform="scale(.94 1)"/></svg>' +
            '</span></span>';
        } else {
          pv.innerHTML = '<span class="pw mains" role="img" aria-label="AC powered" title="AC powered — no battery"><svg class="g-plug" viewBox="0 0 24 24" aria-hidden="true"><use href="#dk-plug"/></svg></span>';
        }
      }

      var ram = card.querySelector('[data-m="ram"] .v');
      if (ram) {
        if (d.has_ram) { ram.className = 'v ' + (d.ram_pct >= 80 ? 'danger' : d.ram_pct >= 60 ? 'warn' : 'ok'); ram.textContent = d.ram_pct + '%'; }
        else { ram.className = 'v muted'; ram.textContent = '—'; }
      }

      var temp = card.querySelector('[data-m="temp"] .v');
      if (temp) {
        if (d.temp_str) { temp.className = 'v ' + d.temp_class; temp.textContent = d.temp_str; }
        else { temp.className = 'v muted'; temp.textContent = '—'; }
      }

      var st = card.querySelector('.dl-status');
      if (st) {
        // Legacy firmware never holds a WebSocket; a recent HTTP check-in shows yellow.
        var ci = !d.online && st.dataset.legacy === '1' && d.last_seen_iso &&
          (Date.now() - Date.parse(d.last_seen_iso)) < (+st.dataset.legacyWindow || 180) * 1000;
        var lbl = d.online ? (st.dataset.labelOn || 'Online') : ci ? 'Checking in' : (st.dataset.labelOff || 'Offline');
        // Keep the shape the server rendered: layouts that show a bare dot (New,
        // Serial, Column — the label is in the tooltip) must not grow a label the
        // first time a row ticks, which is what keying this off .dl-l1 alone did.
        var dotOnly = !st.textContent.trim() || !!st.closest('.dl-l1');
        // motion: a card that just came back online glows once (motion.css .m-glow).
        if (d.online && !st.classList.contains('on')) { card.classList.remove('m-glow'); void card.offsetWidth; card.classList.add('m-glow'); }
        st.className = 'dl-status ' + (d.online ? 'on' : ci ? 'checkin' : 'off');
        if (dotOnly) { st.title = lbl; st.setAttribute('aria-label', lbl); st.innerHTML = '<i></i>'; }
        else st.innerHTML = '<i></i>' + lbl;
      }
      var sy = card.querySelector('.dl-synced');
      if (sy) {
        // Heartbeat text: hidden while the WebSocket is live (the Online badge says it all),
        // shown (as "3 min ago") only when the device is offline.
        // MDM Lite (data-always) has no live connection: its last check-in is always shown.
        var always = sy.dataset.always === '1';
        sy.textContent = d.time_since;
        sy.className = 'dl-synced' + (d.online && !always ? ' hidden' : '');
      }

      // Preserve inspector selection, which lives in a class the patch would clobber.
      var keepSel = window.mdmInspectorSelected && window.mdmInspectorSelected() === d.serial;
      card.className = 'dl-card ' + d.row_classes + (keepSel ? ' row-selected' : '');
    }

    var es = window.mdmStream('/events/devices');
    es.addEventListener('device-update', function (e) {
      try { patchRow(JSON.parse(e.data)); } catch (_) {}
    });
    es.onerror = function () {
      // browser will auto-reconnect; no action needed
    };
  })();

  // ── Split-pane inspector ─────────────────────────────────────────────────
  // Clicking a device opens a compact panel beside the roster instead of
  // navigating away, so you never lose your place in the list. Deep work still
  // lives on the full /devices/{serial} page, linked from the panel.
  (function () {
    var split = document.getElementById('dv-split');
    var aside = document.getElementById('device-inspector');
    var body = document.getElementById('device-inspector-body');
    if (!split || !aside || !body) return;
    var selected = null;

    function esc(s) { return window.CSS && CSS.escape ? CSS.escape(s) : s.replace(/"/g, '\\"'); }

    function highlight(serial) {
      document.querySelectorAll('#device-table-container .dl-card.row-selected')
        .forEach(function (el) { el.classList.remove('row-selected'); });
      if (serial) {
        var card = document.querySelector('#device-table-container .dl-card[data-serial="' + esc(serial) + '"]');
        if (card) card.classList.add('row-selected');
      }
    }

    function reflectUrl(serial) {
      try {
        var u = new URL(location.href);
        if (serial) u.searchParams.set('device', serial); else u.searchParams.delete('device');
        history.replaceState(null, '', u);
      } catch (e) {}
    }

    window.mdmInspectorSelected = function () { return selected; };

    window.mdmOpenInspector = function (serial) {
      if (!serial) return;
      selected = serial;
      split.classList.add('inspector-open');
      aside.hidden = false;
      highlight(serial);
      reflectUrl(serial);
      body.innerHTML = '<div class="dvp-loading muted small">Loading…</div>';
      if (window.htmx) {
        htmx.ajax('GET', '/devices/' + encodeURIComponent(serial) + '/panel',
          { target: '#device-inspector-body', swap: 'innerHTML' })
          .then(function () {
            var c = body.querySelector('.dvp-close');
            if (c) c.focus();
          });
      }
    };

    window.mdmCloseInspector = function () {
      selected = null;
      split.classList.remove('inspector-open');
      aside.hidden = true;
      body.innerHTML = '';
      highlight(null);
      reflectUrl(null);
    };

    // Intercept clicks on a device's serial / View link → open panel in place.
    // Modifier- and middle-clicks fall through so "open in new tab" still works.
    document.addEventListener('click', function (e) {
      if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      var a = e.target.closest && e.target.closest('#device-table-container a.link-detail[href^="/devices/"]');
      if (!a) return;
      // Links marked data-full open the full device page directly (the row's
      // "View" action), bypassing the inspector.
      if (a.dataset.full === '1') return;
      var m = a.getAttribute('href').match(/^\/devices\/([^\/?#]+)$/);
      if (!m) return;
      e.preventDefault();
      window.mdmOpenInspector(decodeURIComponent(m[1]));
    });

    // Esc closes the inspector — unless a modal or the command palette owns Esc.
    document.addEventListener('keydown', function (e) {
      if (e.key !== 'Escape' || !split.classList.contains('inspector-open')) return;
      if (document.querySelector('.modal-backdrop.modal-open')) return;
      var cmdk = document.getElementById('cmdk');
      if (cmdk && !cmdk.hidden) return;
      window.mdmCloseInspector();
    });

    // Re-apply the selection highlight after the table is swapped (filter / sort
    // / paginate). The panel itself stays open and independent of the list.
    document.body.addEventListener('htmx:afterSwap', function (evt) {
      if (evt.detail && evt.detail.target && evt.detail.target.id === 'device-table-container') {
        highlight(selected);
      }
    });

    // Guarded quick-actions exposed for the panel's Reboot / Hide buttons.
    window.mdmInspectorReboot = function (serial) {
      if (!serial || !window.mdmConfirm) return;
      window.mdmConfirm({
        title: 'Reboot device',
        body: 'Reboot ' + serial + '?',
        scope: 'The device reboots.',
        tier: 'danger', confirmLabel: 'Reboot'
      }).then(function (ok) {
        if (ok) window.mdmPostForm('/commands', { type: 'reboot', target_type: 'devices', target_serials: serial, apk_url: '' });
      });
    };

    // Deep link: ?device=SERIAL opens the inspector on load.
    var initial = new URLSearchParams(location.search).get('device');
    if (initial) window.mdmOpenInspector(initial);
  })();
