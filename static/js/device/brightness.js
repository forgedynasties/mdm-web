// Loaded by templates/device.html and device_classic.html: the Screen brightness form.
// An empty level stops managing brightness (and unlocks); the device applies the change
// on the config push.
window.briSave = function (form) {
  var btn = form.querySelector('button[type=submit]');
  var st = form.querySelector('.bri-st');
  var body = new URLSearchParams();
  body.append('screen_brightness', form.elements.screen_brightness.value.trim());
  if (form.elements.screen_brightness_locked.checked) body.append('screen_brightness_locked', '1');
  btn.disabled = true;
  if (st) st.textContent = 'saving…';
  if (window.mdmBeginBusy) window.mdmBeginBusy();
  fetch('/devices/' + encodeURIComponent(form.getAttribute('data-serial')) + '/brightness', {
    method: 'POST', body: body, credentials: 'same-origin', headers: { 'HX-Request': 'true' }
  }).then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error(t || ('HTTP ' + r.status)); });
      if (st) st.textContent = 'saved, sent to the device';
    })
    .catch(function (e) { if (st) st.textContent = String(e.message || e).trim(); })
    .finally(function () { btn.disabled = false; if (window.mdmEndBusy) window.mdmEndBusy(); });
  return false;
};
