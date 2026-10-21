// Loaded by templates/users.html (U1, picked 20 Oct): AIO team / Customers tabs, search,
// role filter, sortable columns, rows that open the person, and the Invite dialog.
(function () {
  var card = document.getElementById('up-card');
  if (card) {
    var q = document.getElementById('up-q');
    var role = { team: '', customers: '' };
    // P1 (21 Oct 2026): Role and Activity are pills (oc-menus.js opens them); ?activity=
    // (the banner's Review / See them links) sets the Activity one on arrival.
    var act = card.dataset.activity || '';
    var tab = (card.querySelector('[role=tab].on') || {}).dataset ? card.querySelector('[role=tab].on').dataset.tab : 'team';

    function pane() { return card.querySelector('.up-pane[data-pane="' + tab + '"]'); }
    function apply() {
      var p = pane(); if (!p) return;
      var t = q.value.trim().toLowerCase(), n = 0, rows = p.querySelectorAll('tr.ft-row');
      rows.forEach(function (r) {
        var ok = (!t || r.dataset.q.toLowerCase().indexOf(t) !== -1) && (!role[tab] || r.dataset.role === role[tab]) && (!act || r.dataset.act === act);
        r.hidden = !ok; if (ok) n++;
      });
      var none = p.querySelector('.up-none'); if (none) none.hidden = n > 0 || !rows.length;
    }
    card.querySelectorAll('[role=tab]').forEach(function (b) {
      b.addEventListener('click', function () {
        tab = b.dataset.tab;
        card.querySelectorAll('[role=tab]').forEach(function (x) { var on = x === b; x.classList.toggle('on', on); x.setAttribute('aria-selected', on); });
        card.querySelectorAll('.up-pane').forEach(function (p) { p.hidden = p.dataset.pane !== tab; });
        card.querySelectorAll('[data-for]').forEach(function (s) { s.hidden = s.dataset.for !== tab; });
        q.placeholder = tab === 'customers' ? 'Search name, email or venue…' : 'Search name or email…';
        try { history.replaceState(null, '', tab === 'customers' ? '/users?tab=customers' : '/users'); } catch (e) {}
        apply();
      });
    });
    function pill(key, label) {
      card.querySelectorAll('[data-up-pill="' + key + '"]').forEach(function (p) {
        if (key === 'role' && p.closest('[data-for]') && p.closest('[data-for]').dataset.for !== tab) return;
        p.querySelector('b').textContent = label; p.classList.toggle('set', label !== 'Any');
      });
    }
    card.addEventListener('click', function (e) {
      var mi = e.target.closest('[data-up-role],[data-up-act]');
      if (!mi) return;
      var menu = mi.parentNode;
      menu.querySelectorAll('.cc-mi').forEach(function (x) { x.classList.toggle('active', x === mi); });
      if (mi.hasAttribute('data-up-role')) { role[tab] = mi.dataset.upRole; pill('role', mi.textContent); }
      else { act = mi.dataset.upAct; pill('act', mi.textContent); }
      if (window.ocCloseMenus) window.ocCloseMenus();
      apply();
    });
    if (act) {
      var first = card.querySelector('[data-up-act="' + act + '"]');
      if (first) { first.parentNode.querySelectorAll('.cc-mi').forEach(function (x) { x.classList.toggle('active', x === first); }); pill('act', first.textContent); }
      apply();
    }
    q.addEventListener('input', apply);

    // Sorting: Name A→Z, Role and Last active highest / newest first; a second click flips.
    card.querySelectorAll('.ft-sort').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var th = btn.closest('th'), table = btn.closest('table'), tb = table.tBodies[0], key = btn.dataset.sort;
        var asc = th.classList.contains('on') ? th.dataset.dir !== 'asc' : key === 'name';
        table.querySelectorAll('th').forEach(function (x) { x.classList.remove('on'); });
        th.classList.add('on'); th.dataset.dir = asc ? 'asc' : 'desc';
        btn.querySelector('.ft-dir').textContent = asc ? '↑' : '↓';
        var rows = Array.prototype.slice.call(tb.rows);
        rows.sort(function (a, b) {
          var x, y;
          if (key === 'rank') { x = +a.dataset.rank; y = +b.dataset.rank; }
          else if (key === 'seen') { x = a.dataset.seen ? Date.parse(a.dataset.seen) : 0; y = b.dataset.seen ? Date.parse(b.dataset.seen) : 0; }
          else { x = a.dataset.name.toLowerCase(); y = b.dataset.name.toLowerCase(); }
          var c = x < y ? -1 : x > y ? 1 : a.dataset.name.localeCompare(b.dataset.name);
          return asc ? c : -c;
        });
        rows.forEach(function (r) { tb.appendChild(r); });
      });
    });

    // A click anywhere on a row opens the person, except on its own links and menu.
    card.addEventListener('click', function (e) {
      var r = e.target.closest('tr.ft-row');
      if (!r || e.target.closest('a,button,summary,details,form,input')) return;
      if (window.htmx) htmx.ajax('GET', r.dataset.href, { target: 'body', pushUrl: true }); else location.href = r.dataset.href;
    });
    document.addEventListener('click', function (e) {
      document.querySelectorAll('.up-more[open]').forEach(function (d) { if (!d.contains(e.target)) d.open = false; });
    });
  }

  var modal = document.getElementById('invite-modal');
  window.upInviteOpen = function () { if (!modal) return; modal.classList.add('modal-open'); var e = document.getElementById('iv-email'); if (e) e.focus(); };
  window.upInviteClose = function () { if (modal) modal.classList.remove('modal-open'); };
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { window.upInviteClose(); document.querySelectorAll('.up-more[open]').forEach(function (d) { d.open = false; }); } });
  var form = document.getElementById('iv-form');
  if (form) {
    var ven = document.getElementById('iv-venues');
    var customer = { owner: 1, staff: 1 };
    function sync() {
      var r = form.querySelector('input[name=role]:checked'), on = !!(r && customer[r.value]);
      ven.hidden = !on;
    }
    form.addEventListener('change', sync); sync();
    // A customer needs at least one venue: the first box carries the browser's message.
    form.addEventListener('submit', function (e) {
      var r = form.querySelector('input[name=role]:checked'), first = form.querySelector('input[name=restaurants]');
      if (!first) return;
      var need = r && customer[r.value] && !form.querySelector('input[name=restaurants]:checked');
      first.setCustomValidity(need ? 'Pick the venue(s) they belong to.' : '');
      if (need) { e.preventDefault(); first.reportValidity(); }
    });
    form.addEventListener('change', function (e) { if (e.target.name === 'restaurants') { var f = form.querySelector('input[name=restaurants]'); if (f) f.setCustomValidity(''); } });
  }
})();
