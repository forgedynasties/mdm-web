// Loaded by templates/export.html.
  function toggleDevicePicker(force) {
    var body = document.getElementById('ex-picker-body');
    var btn = document.getElementById('ex-picker-toggle');
    if (!body || !btn) return;
    var open = typeof force === 'boolean' ? force : body.hidden;
    body.hidden = !open;
    btn.setAttribute('aria-expanded', open ? 'true' : 'false');
    btn.querySelector('svg').style.transform = open ? 'rotate(180deg)' : '';
    document.getElementById('ex-picker-label').textContent = open ? 'Hide device list' : 'Choose devices';
  }
  // Nothing preselected (a bare /export) → open it, that is the only thing to do here.
  if (!window.EXPORT_PAGE.serials) toggleDevicePicker(true);
