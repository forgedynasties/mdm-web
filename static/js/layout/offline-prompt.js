// R1 (17 Nov 2026; user: "Reboot can only be sent to a device which is online. if a reboot
// is sent and a device is offline show that prompt"). The server sends nothing to an
// offline device and answers with the event mdm-reboot-offline {serials, ago}; this opens
// the prompt saying so. Loaded by layout.html.
(function () {
  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function open(d) {
    var serials = (d && d.serials) || [];
    var one = serials.length === 1;
    var title = one ? esc(serials[0]) + ' is offline' : 'All ' + serials.length + ' devices are offline';
    var body = one
      ? 'A reboot needs the device online, so nothing was sent.' + (d.ago ? ' It was last heard from ' + esc(d.ago) + ' ago.' : '')
      : 'A reboot needs the device online, so nothing was sent: ' + serials.map(esc).join(', ') + '.';
    var old = document.getElementById('offline-prompt'); if (old) old.remove();
    var m = document.createElement('div');
    m.className = 'modal-backdrop modal-open'; m.id = 'offline-prompt';
    m.innerHTML = '<div class="modal-box" role="alertdialog" aria-modal="true" aria-labelledby="op-t" aria-describedby="op-b" style="max-width:440px">' +
      '<div class="install-modal-head"><span class="modal-title" id="op-t">' + title + '</span></div>' +
      '<p id="op-b" style="margin:0;padding:4px 20px 6px;line-height:1.5">' + body + '</p>' +
      '<div class="install-modal-foot"><span style="flex:1"></span><button type="button" class="btn-primary btn-sm" id="op-ok">OK</button></div></div>';
    var back = document.activeElement;
    function close() { m.remove(); if (back && back.focus) back.focus(); }
    m.addEventListener('click', function (e) { if (e.target === m) close(); });
    m.addEventListener('keydown', function (e) { if (e.key === 'Escape' || e.key === 'Enter') { e.preventDefault(); close(); } });
    document.body.appendChild(m);
    var ok = m.querySelector('#op-ok'); ok.addEventListener('click', close); ok.focus();
  }
  document.body.addEventListener('mdm-reboot-offline', function (e) { open(e.detail); });
  window.mdmRebootOffline = open;
})();
