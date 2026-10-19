// Loaded by templates/_lock_host.html (device page, New view).
// Lock locks to the device's host app by itself. With one build installed the button acts
// straight away; with several (or none, or locked to another app) it opens #lk-menu.
// The button holds its busy state until the server has heard back from the device.
(function () {
  if (window.lockHostClick) return;

  // The island clips what overflows it, so an open menu is moved to <body> and placed
  // under the button; a page swap drops it.
  function menu() { return document.querySelector('.lk-menu[data-portal]') || document.getElementById('lk-menu'); }
  function closeMenu() {
    var m = menu(), b = document.getElementById('lk-btn');
    if (m) m.hidden = true;
    if (b && b.hasAttribute('aria-expanded')) b.setAttribute('aria-expanded', 'false');
  }
  function place(m, btn) {
    if (m.parentElement !== document.body) { m.setAttribute('data-portal', ''); document.body.appendChild(m); }
    var r = btn.getBoundingClientRect();
    m.style.top = (r.bottom + 8) + 'px';
    m.style.right = Math.max(8, document.documentElement.clientWidth - r.right) + 'px';
  }
  document.addEventListener('htmx:beforeSwap', function () {
    document.querySelectorAll('.lk-menu[data-portal]').forEach(function (m) { m.remove(); });
  });
  window.addEventListener('resize', closeMenu);
  window.addEventListener('scroll', closeMenu, true);

  function setState(btn, on, label, sub, pkg) {
    btn.setAttribute('data-on', on ? '1' : '0');
    btn.setAttribute('data-kout', '0');
    btn.classList.toggle('on', on);
    btn.classList.remove('warn');
    btn.querySelector('.lk-label').textContent = label;
    btn.querySelector('.lk-sub').textContent = sub || '';
    var m = menu();
    if (!m) return;
    m.querySelectorAll('.lk-v').forEach(function (r) { r.classList.toggle('active', on && r.getAttribute('data-pkg') === pkg); });
    m.querySelectorAll('.lk-off, .lk-off-sep').forEach(function (el) { el.hidden = !on; });
    var h = m.querySelector('.cc-mh');
    if (h && h.firstChild && h.firstChild.nodeType === 3 && m.querySelector('.lk-v')) {
      h.firstChild.textContent = on ? 'Locked to' : 'Lock to which ' + btn.getAttribute('data-name') + '?';
    }
  }

  function send(btn, on, pkg) {
    if (btn.getAttribute('aria-busy') === 'true') return;
    closeMenu();
    var prev = { on: btn.getAttribute('data-on') === '1', label: btn.querySelector('.lk-label').textContent, sub: btn.querySelector('.lk-sub').textContent };
    btn.setAttribute('aria-busy', 'true');
    btn.disabled = true;
    btn.querySelector('.lk-label').textContent = on ? 'Locking…' : 'Unlocking…';
    btn.querySelector('.lk-sub').textContent = 'waiting for the device';
    if (window.mdmBeginBusy) window.mdmBeginBusy();
    var body = new URLSearchParams();
    body.append('on', on ? '1' : '0');
    if (pkg) body.append('package', pkg);
    fetch('/devices/' + encodeURIComponent(btn.getAttribute('data-serial')) + '/lock', {
      method: 'POST', body: body, credentials: 'same-origin', headers: { 'HX-Request': 'true' }
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (!r.ok) throw new Error(j.error || ('HTTP ' + r.status));
        return j;
      });
    }).then(function (j) {
      setState(btn, j.on, j.label, j.sub, j.package);
      // The lock form further down the page and the SSE handler read this.
      if (window.__kioskOrig) { window.__kioskOrig.on = !!j.on; window.__kioskOrig.pkg = j.package || ''; }
      var what = j.on ? 'Locked to ' + j.sub : 'Unlocked';
      if (window.showToast) {
        window.showToast(j.delivered ? what + '. The device has it.'
          : what + '. The device is offline and picks it up when it next checks in.', j.delivered ? 'success' : 'info');
      }
    }).catch(function (e) {
      setState(btn, prev.on, prev.label, prev.sub, null);
      if (window.showToast) window.showToast(e.message || 'Could not save. Try again.', 'error');
    }).finally(function () {
      btn.removeAttribute('aria-busy');
      btn.disabled = false;
      if (window.mdmEndBusy) window.mdmEndBusy();
    });
  }

  function relock(btn) {
    btn.setAttribute('aria-busy', 'true');
    btn.disabled = true;
    btn.querySelector('.lk-label').textContent = 'Locking…';
    fetch('/devices/' + encodeURIComponent(btn.getAttribute('data-serial')) + '/kiosk/relock', {
      method: 'POST', credentials: 'same-origin', headers: { 'HX-Request': 'true' }
    }).finally(function () { location.reload(); });
  }

  window.lockHostClick = function (btn) {
    if (!btn || btn.classList.contains('role-locked') || btn.getAttribute('aria-busy') === 'true') return;
    if (btn.getAttribute('data-kout') === '1') { relock(btn); return; }
    var m = menu();
    if (m) {
      var open = m.hidden;
      if (open) place(m, btn);
      m.hidden = !open;
      btn.setAttribute('aria-expanded', open ? 'true' : 'false');
      if (open) { var first = m.querySelector('.cc-mi:not([hidden])'); if (first) first.focus(); }
      return;
    }
    var on = btn.getAttribute('data-on') === '1';
    send(btn, !on, on ? '' : btn.getAttribute('data-pkg'));
  };

  document.addEventListener('click', function (e) {
    var m = menu();
    if (!m || m.hidden) return;
    var btn = document.getElementById('lk-btn');
    var row = e.target.closest('.lk-menu .cc-mi');
    if (row) {
      if (row.classList.contains('lk-off')) { send(btn, false, ''); return; }
      if (row.classList.contains('active')) { closeMenu(); return; }
      send(btn, true, row.getAttribute('data-pkg'));
      return;
    }
    if (!e.target.closest('.lk-wrap') && !e.target.closest('.lk-menu')) closeMenu();
  });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') closeMenu(); });

  // A lock changed somewhere else (another tab, a rule, the device): mdmSyncKiosk
  // (tabs-live.js) calls this.
  window.lockHostSync = function (enabled) {
    var btn = document.getElementById('lk-btn');
    if (!btn || btn.getAttribute('aria-busy') === 'true') return;
    if ((btn.getAttribute('data-on') === '1') !== !!enabled) {
      setState(btn, !!enabled, enabled ? 'Locked' : 'Lock', enabled ? btn.getAttribute('data-name') : '', null);
    }
  };
})();
