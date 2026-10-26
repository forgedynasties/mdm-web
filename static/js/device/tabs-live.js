// Loaded by templates/device.html.
  // Device-detail tab switching. The active tab is remembered per device so a
  // server-triggered reload (e.g. after sending a reboot from Controls) re-opens
  // the same tab instead of snapping back to the summary.
  function setDeviceTab(name, btn) {
    document.querySelectorAll('.dd-tab').forEach(function (t) { var sel = t === btn; t.classList.toggle('active', sel); t.setAttribute('aria-selected', sel ? 'true' : 'false'); });
    document.querySelectorAll('.dd-panel').forEach(function (p) {
      p.hidden = p.getAttribute('data-dd-panel') !== name;
      if (!p.hidden && window.htmx) {
        // Lazy parts (Health's compliance checklist and alert history, History) fetch
        // their content on first open; the "once" in their hx-trigger makes repeat opens
        // a no-op. A panel can hold more than one (Health holds two).
        var lazies = p.hasAttribute('hx-get') ? [p] : [].slice.call(p.querySelectorAll(':scope > [hx-trigger^="dd-show"]'));
        // process() first: on a deep-link or remembered tab this runs before htmx has wired
        // the element up, and the trigger would go to nothing. It is a no-op when already done.
        lazies.forEach(function (lazy) { window.htmx.process(lazy); window.htmx.trigger(lazy, 'dd-show'); if (window.watchLazy) window.watchLazy(p, lazy); });
      }
    });
    // Charts may have been laid out while hidden; nudge a resize when summary shows.
    if (name === 'summary' && window.mdmChart) {
      requestAnimationFrame(function () { window.dispatchEvent(new Event('resize')); });
    }
  }
  window.setDeviceTab = setDeviceTab;
  // "Reset its key…" on an impersonation alert lands here: open the dialog.
  if (location.hash === '#reset-key') {
    var kd = document.getElementById('key-reset-dlg');
    if (kd) kd.showModal();
  }

  // Client pill popover: open on click, close on the next click outside or Escape.
  // The hero clips its overflow, so the wrapper carries .open for the :has() rule
  // that lets the panel hang below the card.
  window.toggleClientPill = function (e) {
    var btn = e.currentTarget, pop = document.getElementById('cpill-pop');
    if (!pop) return;
    var open = pop.hidden;
    pop.hidden = !open;
    if (open) window.ddFactsPop(pop, btn);
    btn.setAttribute('aria-expanded', open ? 'true' : 'false');
    btn.closest('.cpill-wrap').classList.toggle('open', open);
    if (!open) return;
    setTimeout(function () {
      function away(ev) {
        if (ev.target.closest && ev.target.closest('.cpill-wrap')) return;
        close();
      }
      function key(ev) { if (ev.key === 'Escape') { close(); btn.focus(); } }
      function close() {
        pop.hidden = true;
        btn.setAttribute('aria-expanded', 'false');
        btn.closest('.cpill-wrap').classList.remove('open');
        document.removeEventListener('click', away);
        document.removeEventListener('keydown', key);
      }
      document.addEventListener('click', away);
      document.addEventListener('keydown', key);
    }, 0);
  };

  // A lazy panel that fails to load used to sit on "Loading…" for ever: the request
  // is a "once" trigger, so nothing retries it and nothing says what went wrong.
  // Turn any failed/aborted fetch into a message with a Retry that re-issues it.
  (function () {
    function lazyPanelError(e) {
      var el = e.detail && e.detail.elt; if (!el) return;
      var panel = el.closest ? el.closest('.dd-panel') : null;
      if (!panel || !panel.hasAttribute('data-dd-panel')) return;
      var url = el.getAttribute('hx-get'); if (!url) return;
      var st = (e.detail.xhr && e.detail.xhr.status) || 0;
      var why = st ? 'The server answered ' + st + '.' : 'The request did not complete.';
      var box = el === panel ? panel : el;
      box.innerHTML = '<div class="muted small" style="padding:26px;text-align:center;">'
        + 'Couldn\'t load this tab. ' + why
        + ' <button type="button" class="cc-btn sm" style="margin-left:8px;">Retry</button></div>';
      var btn = box.querySelector('button');
      if (btn) btn.addEventListener('click', function () {
        box.innerHTML = '<div class="muted small" style="padding:26px;text-align:center;">Loading…</div>';
        window.htmx.ajax('GET', url, { target: box, swap: el === panel ? 'innerHTML' : 'outerHTML' });
      });
    }
    ['htmx:responseError', 'htmx:sendError', 'htmx:timeout', 'htmx:swapError'].forEach(function (evt) {
      document.body.addEventListener(evt, lazyPanelError);
    });
    // A request that never fires at all (a JS error before the trigger, a fetch the
    // browser drops) leaves the same dead placeholder, and no htmx event ever
    // arrives to catch. Watch the panel instead: still on the placeholder after 20s
    // means it failed, whatever the reason.
    window.watchLazy = function (panel, el) {
      // Per lazy element, not per panel: Health holds two.
      if (el.__lazyWatch) return;
      el.__lazyWatch = setTimeout(function () {
        if (!panel.isConnected || !el.isConnected) return;
        var txt = (el.textContent || '').trim();
        if (txt.indexOf('Loading') !== 0) return;
        lazyPanelError({ detail: { elt: el, xhr: null } });
      }, 20000);
    };
  })();

  // On load, honor a ?tab= deep-link (a crash link from the fleet views points at
  // ?tab=alerts). Nothing else: the page opens on Overview every time.
  (function () {
    var want = null;
    try { var qp = new URLSearchParams(location.search); if (qp.get('tab')) want = qp.get('tab'); } catch (e) {}
    // A device page always opens on Overview; only a link that names a tab opens another.
    if (want === 'queue' || want === 'history') want = 'activity'; // the two tabs merged
    // T1 (19 Oct): Host app is in Apps; Alerts and Compliance are Health.
    if (want === 'hostapp') want = 'apps';
    if (want === 'alerts' || want === 'compliance') want = 'health';
    if (want === 'wifi') want = 'network';
    if (want === 'location') want = 'about'; // Location moved into About (26 Oct 2026)
    if (!want || want === 'summary') return;
    var btn = document.getElementById('dd-tab-' + want);
    if (btn && btn.hidden) return;   // About lives in Overview in the split view
    if (btn) setDeviceTab(want, btn);
  })();

  // Remove this device from a group inline (no navigation).
  window.ddRemoveGroup = function (btn, gid, serial) {
    if (!confirm('Remove this device from the group?')) return;
    if (window.mdmBeginBusy) window.mdmBeginBusy();
    fetch('/groups/' + gid + '/devices/' + encodeURIComponent(serial) + '/remove', { method: 'POST' })
      .then(function () { var chip = btn.closest('.ddc-gtag'); if (chip) chip.remove(); })
      .catch(function () {})
      .finally(function () { if (window.mdmEndBusy) window.mdmEndBusy(); });
  };

  // Placement popovers (change restaurant / add group).
  var DD_SERIAL = window.DEVICE_PAGE.serial;
  window.plTogglePop = function (id, btn) {
    var pop = document.getElementById(id);
    if (!pop) return;
    var opening = pop.hasAttribute('hidden');
    document.querySelectorAll('.pl-pop').forEach(function (p) { p.setAttribute('hidden', ''); });
    if (opening) { pop.removeAttribute('hidden'); window.ddFactsPop(pop, btn); }
  };
  window.ddAddGroup = function (gid, btn) {
    // URLSearchParams -> application/x-www-form-urlencoded. Must NOT be FormData: that
    // sends multipart, which the handler's r.ParseForm() does not read, so the server
    // would see no serials and add nothing (the page reloads unchanged).
    var body = new URLSearchParams(); body.append('serials', DD_SERIAL);
    btn.disabled = true;
    if (window.mdmBeginBusy) window.mdmBeginBusy();
    fetch('/groups/' + gid + '/devices', { method: 'POST', body: body, credentials: 'same-origin' })
      .then(function () { location.reload(); })
      .catch(function () { btn.disabled = false; if (window.mdmEndBusy) window.mdmEndBusy(); });
  };
  document.addEventListener('click', function (e) {
    if (!e.target.closest('.pl-pop-wrap')) {
      document.querySelectorAll('.pl-pop').forEach(function (p) { p.setAttribute('hidden', ''); });
    }
  });

  // Kiosk config popover, opened from the hero "Kiosk" pill.
  window.kioskModalOpen = function () {
    var m = document.getElementById('kiosk-modal');
    if (!m) return;
    // Discard any selection that was never applied: reset the picker to the app that
    // is actually locked (and the mode select to the applied mode), so reopening
    // always reflects the applied state.
    var hid = document.getElementById('kiosk-pkg-value');
    if (hid && window.__kioskOrig) hid.value = window.__kioskOrig.pkg;
    window.__kioskDirty = false;
    var q = document.getElementById('kpk-q');
    if (q) q.value = '';
    if (window.kioskPickerRender) window.kioskPickerRender();
    m.classList.add('modal-open');
    if (window.kioskRefreshToggle) window.kioskRefreshToggle();
    if (q) setTimeout(function () { q.focus(); }, 40);
  };
  window.kioskModalClose = function () {
    var m = document.getElementById('kiosk-modal');
    if (m) m.classList.remove('modal-open');
  };
  window.kioskModalBackdrop = function (e) { if (e.target.id === 'kiosk-modal') window.kioskModalClose(); };

  // Nearby Wi-Fi popover, opened from the vitals Wi-Fi pill.
  window.wifiModalOpen = function () {
    var m = document.getElementById('wifi-modal');
    if (m) m.classList.add('modal-open');
  };
  window.wifiModalClose = function () {
    var m = document.getElementById('wifi-modal');
    if (m) m.classList.remove('modal-open');
  };
  window.wifiModalBackdrop = function (e) { if (e.target.id === 'wifi-modal') window.wifiModalClose(); };
  // Show a transient status line inside the popover.
  function kioskNote(msg, isErr) {
    var n = document.getElementById('kiosk-note');
    if (!n) return;
    n.textContent = msg || '';
    n.classList.toggle('err', !!isErr);
  }
  // Persist the kiosk config via fetch (no navigation, popover stays open) and update
  // the pill/toggle optimistically. The pill flips instantly; the real device applies
  // it on its next check-in (and SSE will confirm/settle the state either way).
  var kioskBusy = false;
  // App lock is only supported for the AIO app on our firmware: lock another app and
  // the device does go into kiosk, but it keeps reporting kiosk off, so the dashboard
  // says "Kiosk off" over a locked device (FW-2026-000025). Warn before that happens.
  // A hard restriction is available to admins as the kiosk allowlist in
  // Settings › Devices & Kiosk.
  window.kioskIsAioApp = function (p) { return /^com\.aioapp(\.|$)/.test((p || '').trim()); };
  window.kioskSubmit = function (enabled) {
    var form = document.getElementById('kiosk-form');
    var val = document.getElementById('kiosk-enabled-val');
    var pkg = document.getElementById('kiosk-pkg-value');
    if (!form || !val) return;
    if (enabled && pkg && pkg.value && !window.kioskIsAioApp(pkg.value)) {
      if (!window.confirm('Only the AIO app is fully supported in locked mode.\n\n'
          + pkg.value + ' will be locked on the device, but the device keeps reporting kiosk as off, '
          + 'so this page will show "Kiosk off" while the device is actually locked.\n\nLock it anyway?')) {
        return;
      }
    }
    val.value = enabled ? '1' : '0';
    // URLSearchParams -> application/x-www-form-urlencoded. Must NOT be FormData:
    // that sends multipart, which the handler's r.ParseForm() does not read, so the
    // server would see kiosk_enabled empty and silently store kiosk OFF.
    var body = new URLSearchParams();
    body.append('kiosk_enabled', enabled ? '1' : '0');
    body.append('kiosk_package', pkg ? pkg.value : '');
    body.append('kiosk_mode', 'app');
    // Optimistic: reflect the new state in the header pill right away. A server-side
    // kiosk change also ends any local exit — the client clears its suspension when the
    // config arrives — so the "exited on device" label goes with it.
    if (window.mdmSyncKiosk) window.mdmSyncKiosk(enabled, false);
    kioskBusy = true;
    if (window.mdmBeginBusy) window.mdmBeginBusy();
    fetch(form.action, {
      method: 'POST', body: body, credentials: 'same-origin',
      // Ask for the 204 no-content response (like htmx) so the server doesn't render
      // and return a whole device page we'd only throw away.
      headers: { 'HX-Request': 'true' }
    }).then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
    }).catch(function () {
      // Roll back the optimistic flip on failure.
      if (window.mdmSyncKiosk) window.mdmSyncKiosk(!enabled);
      kioskNote('Could not save — try again', true);
    }).finally(function () { kioskBusy = false; if (window.mdmEndBusy) window.mdmEndBusy(); });
  };
  // The enable toggle stays disabled until a locked app is chosen — so it can never
  // silently "snap back". Turning kiosk OFF is always allowed.
  // The currently-applied kiosk state (what's on the server), distinct from the
  // in-modal selection. Seeded from the server-rendered values; updated after a
  // successful apply/turn-off and by live SSE flips.
  // Seed from the form's dedicated data-locked-* attributes (server truth), NOT the
  // mutable #kiosk-pkg-value — otherwise a re-render/re-run would re-seed the applied
  // package from the in-progress selection and make "changed" wrong.
  (function () {
    var f = document.getElementById('kiosk-form');
    window.__kioskOrig = f
      ? { pkg: f.getAttribute('data-locked-pkg') || '', on: f.getAttribute('data-locked-on') === '1', mode: f.getAttribute('data-locked-mode') || 'app' }
      : { pkg: '', on: false, mode: 'app' };
    // Set when any mode-specific field (mode select, extra apps, browser URL/prefixes)
    // is edited, so the Apply button shows even when the locked app itself is unchanged.
    window.__kioskDirty = false;
  })();

  // Reflect state on the footer buttons:
  //  - kiosk OFF        → [Enable kiosk] (disabled until an app is chosen)
  //  - kiosk ON, same   → [Turn off kiosk]
  //  - kiosk ON, changed→ [Turn off kiosk] [Switch locked app]
  // Named kioskRefreshToggle for continuity with its callers (mdmSyncKiosk, SSE).
  window.kioskRefreshToggle = function () {
    var applyBtn = document.getElementById('kiosk-apply-btn');
    var offBtn = document.getElementById('kiosk-off-btn');
    if (!applyBtn) return;
    var pkg = document.getElementById('kiosk-pkg-value');
    var hasApp = pkg ? !!pkg.value.trim() : true; // plain-input fallback: don't gate
    var orig = window.__kioskOrig || { pkg: '', on: false, mode: 'app' };
    var changed = (pkg && pkg.value !== orig.pkg) || orig.mode !== 'app' || window.__kioskDirty;
    var n = document.getElementById('kiosk-note');
    if (!orig.on) {
      if (offBtn) offBtn.hidden = true;
      applyBtn.hidden = false;
      applyBtn.textContent = 'Lock';
      applyBtn.disabled = !hasApp;
      if (n && !n.classList.contains('err')) {
        var nonAio = pkg && pkg.value && window.kioskIsAioApp && !window.kioskIsAioApp(pkg.value);
        kioskNote(hasApp ? (nonAio ? 'Only the AIO app reports its kiosk state — this page will show "Kiosk off" while the device is locked' : '')
                         : 'Select a locked app to enable');
      }
    } else {
      if (offBtn) offBtn.hidden = false;
      if (changed && hasApp) {
        applyBtn.hidden = false;
        applyBtn.textContent = orig.mode === 'app' ? 'Switch locked app' : 'Apply changes';
        applyBtn.disabled = false;
      } else {
        applyBtn.hidden = true;
      }
    }
  };
  // Primary button: enable kiosk (when off) or apply the newly-chosen app (when on),
  // then close. The pill flips immediately via kioskSubmit -> mdmSyncKiosk.
  window.kioskApply = function () {
    var pkg = document.getElementById('kiosk-pkg-value');
    if (pkg && !pkg.value.trim()) {
      var s = document.getElementById('kpk-q');
      if (s) { s.focus(); s.classList.add('kiosk-need'); setTimeout(function () { s.classList.remove('kiosk-need'); }, 1200); }
      kioskNote('Pick a locked app first', true);
      return;
    }
    window.kioskSubmit(true);
    var appliedPkg = pkg ? pkg.value : '';
    var f = document.getElementById('kiosk-form');
    if (f) { f.setAttribute('data-locked-on', '1'); f.setAttribute('data-locked-pkg', appliedPkg); f.setAttribute('data-locked-mode', 'app'); }
    if (window.__kioskOrig) { window.__kioskOrig.on = true; window.__kioskOrig.pkg = appliedPkg; window.__kioskOrig.mode = 'app'; }
    window.__kioskDirty = false;
    window.kioskModalClose();
  };
  // Danger button: turn kiosk off, then close.
  window.kioskTurnOff = function () {
    window.kioskSubmit(false);
    var f = document.getElementById('kiosk-form');
    if (f) f.setAttribute('data-locked-on', '0');
    if (window.__kioskOrig) window.__kioskOrig.on = false;
    window.kioskModalClose();
  };
  // Picking a different app just updates the buttons (no auto-apply); an explicit
  // Enable / Change locked app press commits it.
  document.addEventListener('change', function (e) {
    if (!e.target) return;
    if (e.target.id === 'kiosk-pkg-value') {
      window.kioskRefreshToggle();
      return;
    }
    if (e.target.closest && e.target.closest('#kiosk-form')) {
      window.__kioskDirty = true;
      window.kioskRefreshToggle();
    }
  });
  window.kioskRefreshToggle();
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') { var m = document.getElementById('kiosk-modal'); if (m && m.classList.contains('modal-open')) window.kioskModalClose(); }
  });

  // Opening a device always lands on Overview (no last-tab restore).

  // Live charging-pad occupancy: repaint the sidebar card's status dot + label from
  // the SSE wlc_status (1 placed · 0 vacant · 2 flapping — gpio27 kept disagreeing
  // instead of settling, a real hardware symptom · else unreadable).
  window.mdmSyncPadStatus = function (wlc) {
    var el = document.getElementById('pad-status');
    if (!el) return;
    var warn = document.getElementById('pad-warn');
    // wlc_status is only meaningful while charging is ON. If the pad's charging toggle is
    // off, keep "Not available" — don't let a device-reported vacant/placed override it.
    var card = document.getElementById('charging-pad-card');
    if (card && card.getAttribute('data-charging') === '0') {
      el.className = 'pad-stat pad-stat-na';
      var tn = el.querySelector('.pad-txt'); if (tn) tn.textContent = 'Not available';
      if (warn) warn.hidden = true;
      if (window.ddPadChip) window.ddPadChip('na');
      return;
    }
    var map = { 1: ['on', 'Phone on it'], 0: ['vacant', 'Vacant'], 2: ['flapping', 'Misplaced'] };
    var m = map[wlc] || ['bad', 'Unreadable'];
    el.className = 'pad-stat pad-stat-' + m[0];
    var t = el.querySelector('.pad-txt'); if (t) t.textContent = m[1];
    if (warn) warn.hidden = (wlc !== 2);
    if (window.ddPadChip) window.ddPadChip(m[0]);
  };

  window.chShowAll = function () {
    var l = document.getElementById('ch-list'); if (l) l.classList.remove('ch-collapsed');
    var b = document.getElementById('ch-more'); if (b) b.style.display = 'none';
  };
  // Command-history type filters (client-side, in the History tab).
  // Filter pills from the types present, in first-seen order, named like the rows.
  (function () {
    var bar = document.getElementById('ch-filters'); if (!bar) return;
    var seen = {};
    document.querySelectorAll('#ch-list .ddc-tl[data-ct]').forEach(function (r) {
      var ct = r.getAttribute('data-ct'); if (!ct || seen[ct]) return; seen[ct] = true;
      var b = document.createElement('button'); b.type = 'button'; b.className = 'ch-pill'; b.setAttribute('data-ct', ct);
      b.textContent = (r.getAttribute('data-ctl') || ct).replace(/^Update reboot$/, 'Reboot');
      if (ct === 'reboot') b.textContent = 'Reboot';
      b.addEventListener('click', function () { cmdHistFilter(ct, b); });
      bar.appendChild(b);
    });
  })();
  window.cmdHistFilter = function (ct, btn) {
    document.querySelectorAll('#ch-filters .ch-pill').forEach(function (p) { p.classList.toggle('on', p === btn); });
    if (ct) chShowAll(); // filtering by a type looks across all commands, not just the first 10
    var vis = 0;
    document.querySelectorAll('#ch-list .ddc-tl').forEach(function (r) {
      var show = !ct || r.getAttribute('data-ct') === ct;
      r.style.display = show ? '' : 'none'; if (show) vis++;
    });
    var e = document.getElementById('ch-empty'); if (e) e.style.display = vis ? 'none' : '';
  };

  // Single source of truth for reflecting kiosk on/off in the header, updated in
  // place (no fragment swap) so the popover/combo aren't disturbed. Covers every
  // chip variant: the operator+ ribbon pill (.ddc-ctl) and the viewer's
  // static chip both carry a .ddc-kstate label; the popover also has the toggle +
  // hidden input. Driven by the SSE device payload (kiosk_enabled) so an on-device
  // offline exit flips it live for whoever is watching, whatever their role.
  // suspended = the device unlocked itself with the exit PIN. The stored config is
  // still "on" (it re-locks on reboot), so the label says so instead of lying either way.
  // window.__kioskSuspended starts from the config script before this file's tag.
  window.mdmSyncKiosk = function (enabled, suspended) {
    if (suspended !== undefined && suspended !== null) window.__kioskSuspended = !!suspended;
    if (enabled === undefined || enabled === null) return;
    var susp = !!window.__kioskSuspended && !!enabled;
    document.querySelectorAll('.ddc-kstate').forEach(function (el) {
      if (el.closest('#wlc-chip')) return; // the charging pill shares this class — leave it alone
      el.textContent = susp ? 'exited on device' : (enabled ? 'on' : 'off');
    });
    var btn = document.querySelector('.ddc-ctl');
    if (btn) btn.classList.toggle('on', !!enabled && !susp);
    var tog = document.getElementById('kiosk-toggle');
    if (tog) tog.checked = !!enabled;
    var hid = document.getElementById('kiosk-enabled-val');
    if (hid) hid.value = enabled ? '1' : '0';
    var f = document.getElementById('kiosk-form');
    if (f) f.setAttribute('data-locked-on', enabled ? '1' : '0');
    if (window.__kioskOrig) window.__kioskOrig.on = !!enabled;
    if (window.kioskRefreshToggle) window.kioskRefreshToggle();
    if (window.lockHostSync) window.lockHostSync(enabled && !susp);
  };

  (function () {
    var DP = window.DEVICE_PAGE;
    var serial = DP.serial;
    if (!serial || !window.EventSource) return;
    var es = window.mdmStream('/devices/' + encodeURIComponent(serial) + '/events');
    // A reconnect means frames were missed while the stream was down.
    es.addEventListener('open', function () { if (window.mdmChartCatchUp) window.mdmChartCatchUp(); });

    es.addEventListener('device', function (e) {
      var d = {};
      try { d = JSON.parse(e.data); } catch (_) {}

      // Refresh every htmx-bound fragment listening for this event: the hero
      // vitals row (battery / charging ⚡ / temp / RAM / uptime / Wi-Fi), the
      // stats block and the command history. The vitals fragment re-renders the
      // charging bolt server-side, so it tracks d.charging without a reload.
      document.body.dispatchEvent(new CustomEvent('device-updated'));

      // Keep the hero meta live without a reload: build id (in case the device just
      // updated) and the "Last seen" clock, which the presence handler reveals when
      // the device drops.
      if (d.build_id) {
        var _b = document.querySelector('.ddc-build');
        if (_b && _b.textContent.trim() !== d.build_id) _b.textContent = d.build_id;
        var _bk = document.querySelector('.ddc-build-kv');
        if (_bk && _bk.textContent.trim() !== d.build_id) _bk.textContent = d.build_id;
        document.querySelectorAll('.isl-build').forEach(function (el) { if (el.textContent.trim() !== d.build_id) el.textContent = d.build_id; });
      }
      if (d.ts_ms) {
        window.__lastCheckinSec = Math.floor(d.ts_ms / 1000);
        var _ls = document.querySelector('.ddc-lastseen .reltime');
        if (_ls) { _ls.dataset.ts = window.__lastCheckinSec; if (window.mdmTickRel) window.mdmTickRel(); }
      }

      // Kiosk state travels in the SSE payload — flip the chip/toggle live when the
      // device reports an on-device offline exit (kiosk turned off), no reload.
      if (typeof d.kiosk_suspended === 'boolean') window.__kioskSuspended = d.kiosk_suspended;
      if (typeof d.kiosk_enabled === 'boolean' && window.mdmSyncKiosk) {
        window.mdmSyncKiosk(d.kiosk_enabled, d.kiosk_suspended);
      }

      // Charging-pad occupancy (wlc_status) also rides the payload — keep the sidebar
      // card's live status dot in sync without a reload.
      if (d.wlc !== undefined && d.wlc !== null && window.mdmSyncPadStatus) {
        window.mdmSyncPadStatus(d.wlc);
      }

      // push new point into charts
      if (d.ts_ms && window.mdmChart) {
        // insertGaps measures the distance between neighbouring points, so it needs
        // the series strictly increasing in x. A device can emit two frames for one
        // poll (WS telemetry plus the HTTP safety net fire in the same pass), and
        // those can also land out of order. Either one hides a gap: a duplicate makes
        // the delta 0, an out-of-order point makes it negative. Drop anything not
        // newer than the last point already plotted.
        //
        // Dedup off the last plotted CHECK-IN time, not the battery series: a device
        // in locked mode with no battery (dedicated kiosk hardware) never populates
        // allBattery, so keying off it left newest = -Infinity forever. Every SSE
        // frame then re-passed the guard — and frames re-fire on any device-update,
        // e.g. a command-status change re-sends the same latest check-in — so temp/RAM
        // got double-plotted while the chart's live edge never advanced cleanly. That
        // is the "graphs not updating in locked mode" report. Track the check-in time
        // across all series so the guard is source-agnostic.
        var last = window.mdmChart.lastPlottedMs;
        if (last === undefined || last === null) {
          last = Math.max(
            window.mdmChart.allBattery.length ? window.mdmChart.allBattery[window.mdmChart.allBattery.length - 1].x : -Infinity,
            window.mdmChart.allTemp.length ? window.mdmChart.allTemp[window.mdmChart.allTemp.length - 1].x : -Infinity,
            window.mdmChart.allRam.length ? window.mdmChart.allRam[window.mdmChart.allRam.length - 1].x : -Infinity
          );
        }
        if (d.ts_ms > last) {
          window.mdmChart.lastPlottedMs = d.ts_ms;
          var wlc = d.wlc !== undefined && d.wlc !== null ? d.wlc : undefined;
          if (DP.showBattery) window.mdmChart.allBattery.push({x: d.ts_ms, y: d.battery_pct, wlc: wlc});
          if (DP.hasCharging) window.mdmChart.pushCharge(d.ts_ms, d.charger_flapping ? 'flap' : typeof d.charging === 'boolean' ? d.charging : null);
          // Skip a series the page has not fetched yet (temp/RAM load lazily): one
          // pushed point makes a one-point series that the crosshair rail reports at
          // every cursor position. The lazy load picks the live point up anyway.
          if (window.mdmChart.tempLoaded && d.temp_c !== null && d.temp_c !== undefined)
            window.mdmChart.allTemp.push({x: d.ts_ms, y: d.temp_c});
          if (window.mdmChart.ramLoaded && d.ram_pct !== null && d.ram_pct !== undefined)
            window.mdmChart.allRam.push({x: d.ts_ms, y: d.ram_pct});
          if (window.mdmChart.netLoaded && d.wifi_rssi !== null && d.wifi_rssi !== undefined)
            window.mdmChart.allNet.push({x: d.ts_ms, y: d.wifi_rssi});
          window.mdmChart.updateCharts();
        }
      }

    });

    es.addEventListener('presence', function (event) {
      var badge = document.getElementById('ws-badge');
      if (!badge) return;
      var online = event.data === 'online';
      if (window.mdmChart) window.mdmChart.setOnline(online);
      badge.className = 'ddc-status ' + (online ? 'on' : 'off');
      badge.innerHTML = '<i></i>' + (online ? (badge.dataset.labelOn || 'Online') : (badge.dataset.labelOff || 'Offline'));
      badge.setAttribute('title', online ? 'WebSocket connected' : 'No WebSocket connection');
      // "Last seen" belongs only to the offline state. It's rendered server-side but
      // was never toggled here, so a device that came back online kept a stale "Last
      // seen …" until a manual refresh. Hide it when online; reveal (freshened to the
      // latest check-in) when offline.
      var lastSeen = document.querySelector('.ddc-lastseen');
      if (lastSeen) {
        if (online) {
          lastSeen.setAttribute('hidden', '');
        } else {
          if (window.__lastCheckinSec) {
            var rt = lastSeen.querySelector('.reltime');
            if (rt) rt.dataset.ts = window.__lastCheckinSec;
          }
          lastSeen.removeAttribute('hidden');
          if (window.mdmTickRel) window.mdmTickRel();
        }
      }
    });
    window.addEventListener('beforeunload', function () { es.close(); });
  })();

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

var deviceActionConfig = {
  install: {
    title: 'Install App',
    copy: 'Choose one or more apps from your library and install them on this device.',
    meta: ''
  },
  poll: {
    title: 'Set Poll Interval',
    copy: 'Tune how often this device checks in for commands and telemetry.',
    meta: 'Device setting'
  },
  kiosk: {
    title: 'Kiosk Mode',
    copy: 'Lock the device to a single package or release it back to the launcher.',
    meta: 'Device setting'
  }
};

// Picker is scoped to its container (.device-actions) so the Controls and Settings
// tabs can each run an independent picker without colliding.
function selectDeviceAction(key, btn) {
  var root = btn.closest('.device-actions');
  if (!root) return;
  root.querySelectorAll('.device-action-tile').forEach(function(tile) {
    if (tile.dataset.deviceAction) tile.classList.toggle('active', tile.dataset.deviceAction === key);
  });
  root.querySelectorAll('.device-action-body').forEach(function(panel) {
    panel.style.display = panel.dataset.action === key ? '' : 'none';
  });
  var cfg = deviceActionConfig[key];
  if (!cfg) return;
  var title = root.querySelector('.js-da-title');
  var copy = root.querySelector('.js-da-copy');
  var meta = root.querySelector('.js-da-meta');
  if (title) title.textContent = cfg.title;
  if (copy) copy.textContent = cfg.copy;
  if (meta) meta.textContent = cfg.meta;
}

// Sync each picker's header to its initially-active tile on load.
document.addEventListener('DOMContentLoaded', function() {
  document.querySelectorAll('.device-actions').forEach(function(root) {
    var active = root.querySelector('.device-action-tile.active[data-device-action]');
    if (active) selectDeviceAction(active.dataset.deviceAction, active);
  });
});
