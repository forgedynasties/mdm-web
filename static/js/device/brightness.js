// Loaded by the "dev-brightness" row (device.html, shared with device_classic.html):
// Screen brightness, B1 (21 Oct 2026). The pencil opens the editor in place; Save sets a
// level for this device, "Back to default" puts it back on 122 (Tableside AI) or stops
// managing it (other devices). After a save the row is re-read from the page, so the chip
// says what the server now holds.
(function () {
  function row(el) { return el.closest('.bri'); }
  window.briEdit = function (el) {
    var r = row(el), form = r.querySelector('.bri-edit'), pen = r.querySelector('.bri-pen');
    if (!form) return;
    var open = form.hidden;
    form.hidden = !open;
    if (pen) pen.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) { var s = form.querySelector('input[type=range]'); if (s) s.focus(); }
  };
  window.briLock = function (btn) {
    var seg = btn.parentNode, form = btn.closest('form');
    seg.querySelectorAll('button').forEach(function (b) { b.setAttribute('aria-pressed', b === btn ? 'true' : 'false'); });
    form.elements.screen_brightness_locked.value = btn.getAttribute('data-lock') === '1' ? '1' : '';
  };
  function send(r, body) {
    var st = r.querySelector('.bri-st'), btns = r.querySelectorAll('.bri-edit button');
    btns.forEach(function (b) { b.disabled = true; });
    if (st) st.textContent = 'saving…';
    if (window.mdmBeginBusy) window.mdmBeginBusy();
    return fetch('/devices/' + encodeURIComponent(r.getAttribute('data-serial')) + '/brightness', {
      method: 'POST', body: body, credentials: 'same-origin', headers: { 'HX-Request': 'true' }
    }).then(function (res) {
      if (!res.ok) return res.text().then(function (t) { throw new Error(t || ('HTTP ' + res.status)); });
      // Re-read this row from the page so the chip shows what the server holds now.
      return fetch(location.href, { credentials: 'same-origin' }).then(function (p) { return p.text(); }).then(function (html) {
        var fresh = new DOMParser().parseFromString(html, 'text/html').querySelector('.bri[data-serial="' + r.getAttribute('data-serial') + '"]');
        if (fresh) r.replaceWith(fresh); else if (st) st.textContent = 'saved, sent to the device';
      });
    }).catch(function (e) {
      if (st) st.textContent = String(e.message || e).trim();
      btns.forEach(function (b) { b.disabled = false; });
    }).finally(function () { if (window.mdmEndBusy) window.mdmEndBusy(); });
  }
  window.briSave = function (form) {
    var body = new URLSearchParams();
    body.append('screen_brightness', form.elements.screen_brightness.value);
    if (form.elements.screen_brightness_locked.value === '1') body.append('screen_brightness_locked', '1');
    send(row(form), body);
    return false;
  };
  window.briReset = function (btn) {
    var body = new URLSearchParams();
    body.append('reset', '1');
    send(row(btn), body);
  };
})();
