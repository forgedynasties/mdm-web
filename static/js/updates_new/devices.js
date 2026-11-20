// Loaded by templates/updates_new.html (NU1: stepper + live coral summary).
// The device list is the shared picker (static/picker.js); the count, the offline count and
// the Send button follow its basket (pk:change carries online / offline / full counts).
(function () {
  var detail = { serials: [], online: 0, offline: 0, full: 0 };
  var step = 1;

  function $(id) { return document.getElementById(id); }
  function form() { return $('push-form'); }
  function plural(n, w) { return n + ' ' + w + (n === 1 ? '' : 's'); }
  function val(id, d) { var e = $(id); return e ? e.value : d; }
  function serialCount() {
    var f = $('pk-serials');
    return f && f.value ? f.value.split('\n').filter(Boolean).length : 0;
  }
  function hasRelease() {
    var r = document.querySelector('#push-form input[name="release_id"]');
    return !!(r && r.value);
  }
  // Full image + a wiping release + no consent: the server refuses it, so Send waits too.
  function wipeBlocked() {
    var f = form();
    if (!f || !f.getAttribute('data-wipes')) return false;
    var w = $('push-allow-wipe');
    return val('push-delivery', 'smart') === 'full' && !(w && w.checked);
  }
  // Smart delivery, a wiping release, no consent, and EVERY chosen device only fits the wiping full
  // image: the server would leave them all out ("No eligible devices"), so Send waits too.
  function allLeftOut() {
    var f = form(), n = serialCount();
    if (!f || !f.getAttribute('data-wipes') || n === 0) return false;
    var w = $('push-allow-wipe');
    return val('push-delivery', 'smart') !== 'full' && !(w && w.checked) && detail.full >= n;
  }

  function setList(items) {
    var ul = $('nu-sum-l'); if (!ul) return;
    ul.innerHTML = '';
    items.forEach(function (t) { var li = document.createElement('li'); li.textContent = t; ul.appendChild(li); });
  }

  function update() {
    var f = form(); if (!f) return;
    var n = serialCount(), rel = hasRelease();
    var off = detail.offline, on = detail.online;
    var unknown = Math.max(0, n - on - off);
    var wipes = !!f.getAttribute('data-wipes');
    var allowed = !!($('push-allow-wipe') && $('push-allow-wipe').checked);
    var blocked = wipeBlocked(), noneFit = allLeftOut();
    var delivery = val('push-delivery', 'smart');
    var manual = val('push-reboot', 'immediate') === 'manual';

    var lbl = $('push-lbl');
    if (lbl) lbl.textContent = 'Push to ' + plural(n, 'device');
    var btn = $('push-btn');
    if (btn) btn.disabled = !rel || n === 0 || blocked || noneFit;

    var big = $('nu-sum-big');
    if (big) big.textContent = !rel ? 'Choose a release' : n === 0 ? 'Choose the devices' : 'Will update ' + plural(n, 'device');

    var items = [];
    if (rel && n > 0) {
      if (on) items.push(plural(on, 'device') + ' online now: the update starts at once');
      if (off) items.push(off + ' offline: ' + (off === 1 ? 'waits' : 'wait') + ' and update' + (off === 1 ? 's' : '') + ' when back');
      if (unknown) items.push(unknown + ' not in the list yet: checked when sent');
      items.push(delivery === 'full' ? 'Full image to every device' : 'Smart: an incremental where one matches');
      items.push(manual ? 'Reboot waits for your command' : 'Reboots once it has installed');
      if (wipes && delivery !== 'full' && !allowed && detail.full > 0) {
        items.push(plural(detail.full, 'device') + ' left out: only a wiping full image fits ' + (detail.full === 1 ? 'it' : 'them'));
      }
    }
    setList(items);

    var w = $('nu-sum-w');
    if (w) {
      var msg = '';
      if (blocked) msg = 'The full image of this release wipes the device. Allow the factory reset in Options to send it.';
      else if (noneFit) msg = 'None of the chosen devices has an incremental for its build, and the full image wipes the device. Allow the factory reset in Options (and pick Full image) to send it.';
      else if (wipes && allowed && rel && n > 0) msg = 'Factory reset allowed: devices that get the full image are wiped.';
      w.textContent = msg; w.hidden = !msg;
      w.classList.toggle('stop', blocked || noneFit);
    }
    var sum = $('nu-sum');
    if (sum) sum.classList.toggle('is-blocked', blocked || noneFit);

    var s2 = $('nu-s2');
    if (s2 && rel) s2.textContent = n === 0 ? 'None chosen yet' : plural(n, 'device') + ' chosen' + (off ? ' · ' + off + ' offline' : '');
    var s3 = $('nu-s3');
    if (s3 && rel) s3.textContent = (manual ? 'Manual reboot' : 'Reboot immediately') + (f.querySelector('#push-delivery') ? ' · ' + (delivery === 'full' ? 'Full image' : 'Smart') : '');
  }

  function showStep(n) {
    var f = form(); if (!f) return;
    step = n;
    f.querySelectorAll('[data-pane]').forEach(function (p) { p.hidden = p.getAttribute('data-pane') !== String(n); });
    f.querySelectorAll('.nu-step').forEach(function (b) { b.classList.toggle('on', b.getAttribute('data-step') === String(n)); });
    var prev = $('nu-prev'), next = $('nu-next');
    if (prev) prev.hidden = n === 1;
    if (next) {
      next.hidden = n === 3;
      next.textContent = n === 1 ? 'Next: devices' : 'Next: options';
      next.disabled = n === 1 && !hasRelease();
    }
    f.setAttribute('data-step', n);
    if (n === 1) { var i = $('rel-search'); if (i && !hasRelease()) { /* leave the list open, no focus steal */ } }
  }

  function init() {
    var f = form(); if (!f) return;
    detail = { serials: [], online: 0, offline: 0, full: 0 };
    f.querySelectorAll('.nu-step').forEach(function (b) {
      b.addEventListener('click', function () { if (!b.disabled) showStep(parseInt(b.getAttribute('data-step'), 10)); });
    });
    var prev = $('nu-prev'), next = $('nu-next');
    if (prev) prev.addEventListener('click', function () { showStep(Math.max(1, step - 1)); });
    if (next) next.addEventListener('click', function () { showStep(Math.min(3, step + 1)); });
    var w = $('push-allow-wipe'); if (w) w.addEventListener('change', update);
    showStep(parseInt(f.getAttribute('data-start') || '1', 10));
    update();
  }

  // The document listener is bound once (a swapped-in page runs this script again).
  if (!window.__nuBound) {
    window.__nuBound = true;
    document.addEventListener('pk:change', function (e) {
      if (e.detail) detail = { serials: e.detail.serials || [], online: e.detail.online || 0, offline: e.detail.offline || 0, full: e.detail.full || 0 };
      update();
    });
  }

  window.pushCount = update;
  window.pushConfirm = function (e) {
    var n = serialCount();
    if (n === 0 || wipeBlocked()) { e.preventDefault(); return false; }
    var w = $('push-allow-wipe');
    if (w && w.checked) {
      return mdmConfirmSubmit(e, { title: 'Push update and allow factory reset?', body: 'This pushes the update to ' + plural(n, 'selected device') + '. Any of them without an incremental for its current build gets the full image, which WIPES the device: all apps, settings and data are erased.', confirmLabel: 'Push and allow reset' });
    }
    return mdmConfirmSubmit(e, { title: 'Push update?', body: 'This pushes the update to ' + plural(n, 'selected device') + '.', confirmLabel: 'Push update' });
  };
  function seg(btn) { btn.parentElement.querySelectorAll('.seg-btn').forEach(function (s) { s.classList.toggle('on', s === btn); }); }
  window.pushReboot = function (v, btn) { $('push-reboot').value = v; seg(btn); update(); };
  window.pushDelivery = function (v, btn) { var el = $('push-delivery'); if (el) el.value = v; seg(btn); update(); };

  init();
})();
