// Loaded by templates/device.html.
  // Keep the Queue tab's count badge in sync as the queue drains/morphs live.
  document.body.addEventListener('htmx:afterSwap', function (e) {
    if (!e.target || e.target.id !== 'device-queue') return;
    var n = e.target.querySelectorAll('.dq-row:not(.dq-done)').length;
    var tab = document.getElementById('dd-tab-activity');
    var badge = document.getElementById('dd-queue-n');
    if (n > 0) {
      if (!badge && tab) { badge = document.createElement('span'); badge.className = 'dd-tab-n'; badge.id = 'dd-queue-n'; tab.appendChild(document.createTextNode(' ')); tab.appendChild(badge); }
      if (badge) badge.textContent = n;
    } else if (badge) { badge.remove(); }
  });
