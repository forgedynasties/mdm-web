// Loaded by templates/users_access.html: Access AC1 (picked 18 Nov 2026). A matrix cell opens
// a small card saying what the cell means, with "Add a grant" (the Grants tab on that role)
// or "Open person page". The data is on the button (data-ax-*); nothing is fetched.
(function () {
  var mx = document.getElementById('ac1-mx');
  var pop = document.getElementById('ac1-pop');
  if (!mx || !pop) return;
  var cur = null;

  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function close() { pop.hidden = true; if (cur) { cur.classList.remove('sel'); cur.setAttribute('aria-expanded', 'false'); } cur = null; }

  function open(btn) {
    if (cur === btn) { close(); return; }
    close();
    cur = btn; btn.classList.add('sel'); btn.setAttribute('aria-expanded', 'true');
    var lines = (btn.dataset.axLines || '').split('|').filter(Boolean);
    var h = '<div class="h">' + esc(btn.dataset.axWho) + ' · ' + esc(btn.dataset.axCol) + '</div>' +
      '<div class="p">' + (lines.map(esc).join('<br>') || 'Nothing here.') + '</div>';
    if (btn.dataset.axRole) h += '<a class="b" href="/users/access?role=' + encodeURIComponent(btn.dataset.axRole) + '">Add a grant for ' + esc(btn.dataset.axWho) + '…</a>';
    if (btn.dataset.axOpen) h += '<a class="b" href="/users/' + encodeURIComponent(btn.dataset.axOpen) + '/profile">Open person page</a>';
    if (!btn.dataset.axRole && !btn.dataset.axOpen) h += '<div class="p mut">Fixed by the role: no grant changes it.</div>';
    pop.innerHTML = h; pop.hidden = false;
    var host = pop.offsetParent || pop.parentNode;
    var hr = host.getBoundingClientRect(), br = btn.getBoundingClientRect();
    var w = pop.offsetWidth;
    var left = Math.max(8, Math.min(br.left - hr.left + host.scrollLeft, host.clientWidth - w - 8));
    pop.style.left = left + 'px';
    pop.style.top = (br.bottom - hr.top + host.scrollTop + 6) + 'px';
    var pr = pop.getBoundingClientRect();
    if (pr.bottom > window.innerHeight - 8) pop.style.top = (br.top - hr.top + host.scrollTop - pop.offsetHeight - 6) + 'px';
  }

  mx.addEventListener('click', function (e) {
    var b = e.target.closest('.ac1-cell');
    if (b) { e.stopPropagation(); open(b); }
  });
  document.addEventListener('click', function (e) { if (cur && !pop.contains(e.target)) close(); });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && cur) { var b = cur; close(); b.focus(); } });
})();
