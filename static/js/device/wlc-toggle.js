// Loaded by templates/device.html.
    window.wlcToggle = function (btn) {
      var cur = btn.getAttribute('data-on') === '1';
      var next = !cur;
      var st = btn.querySelector('.ddc-kstate');
      var apply = function (v) {
        btn.setAttribute('data-on', v ? '1' : '0');
        btn.setAttribute('aria-checked', v ? 'true' : 'false');
        btn.classList.toggle('on', v);
        if (st) st.textContent = v ? 'enabled' : 'disabled';
        // Keep the card's charging flag in sync so the live SSE updater (mdmSyncPadStatus)
        // knows to hold "Not available" while disabled.
        var card = document.getElementById('charging-pad-card');
        if (card) card.setAttribute('data-charging', v ? '1' : '0');
        // Reflect the status immediately: charging off => "Not available" (gpio27 is
        // meaningless then). When re-enabled, the next telemetry refresh fills in the real
        // placed/vacant.
        var ps = document.getElementById('pad-status');
        if (ps && !v) { ps.className = 'pad-stat pad-stat-na'; var t = ps.querySelector('.pad-txt'); if (t) t.textContent = 'Not available'; }
        if (!v && window.ddPadChip) window.ddPadChip('na');
        document.querySelectorAll('.wlc-seg button').forEach(function (b) { b.setAttribute('aria-pressed', (b.value === '1') === v ? 'true' : 'false'); });
        var wt = document.getElementById('wlc-row-st'); if (wt) wt.textContent = v ? 'pad is on' : 'pad is off';
      };
      apply(next); // optimistic
      btn.disabled = true;
      if (window.mdmBeginBusy) window.mdmBeginBusy();
      var body = new URLSearchParams();
      body.append('wlc_charging_enabled', next ? '1' : '0');
      fetch('/devices/' + encodeURIComponent(btn.getAttribute('data-serial')) + '/wlc', {
        method: 'POST', body: body, credentials: 'same-origin', headers: { 'HX-Request': 'true' }
      }).then(function (r) {
          if (!r.ok) throw new Error('HTTP ' + r.status);
          // The Pad column's words (On / Off · charging off) come from the server.
          var cols = document.getElementById('device-vitals-cols');
          if (cols && window.htmx) htmx.ajax('GET', cols.getAttribute('hx-get'), { target: cols, swap: 'innerHTML' });
        })
        .catch(function () { apply(cur); }) // roll back on failure
        .finally(function () { btn.disabled = false; if (window.mdmEndBusy) window.mdmEndBusy(); });
    };
    
