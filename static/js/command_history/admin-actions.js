// Loaded by templates/command_history.html.
  // "Show admin actions" (admins only): actions sent by admin accounts are hidden
  // from the history lists unless switched on. Remembered per browser; default off.
  (function () {
    // "Show admin actions" (admins only): the server hides actions sent by admin
    // accounts — from the lists and the counts — unless this cookie is "1".
    var on = /(?:^|; )mdm_show_admin_actions=1(?:;|$)/.test(document.cookie);
    function sync() { document.querySelectorAll('.adm-toggle input').forEach(function (i) { i.checked = on; }); }
    window.mdmAdminActions = function (v) {
      document.cookie = 'mdm_show_admin_actions=' + (v ? '1' : '0') + '; path=/; max-age=31536000; samesite=lax';
      location.reload();
    };
    sync();
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', sync);
  })();
