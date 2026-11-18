// Users list UL1 (18 Nov 2026): Team / Customers switch, search, the By role rows, the
// housekeeping tiles and the filter pills all filter the one table in place; unverified
// sign-ups swap the table for the waiting list. ?activity= (invited | quiet | week) and
// ?tab=customers set the starting view. The page fits between header and dock on a desktop
// window (the table scrolls inside its card).
(function () {
  var page = document.getElementById('ul1');
  if (!page) return;
  var card = document.getElementById('ul1-card');
  var table = document.getElementById('ul1-tb');
  var rows = Array.prototype.slice.call(table.tBodies[0].rows);
  var wait = document.getElementById('waiting-signups');
  var wrap = card.querySelector('[data-ul1-table]');
  var none = card.querySelector('[data-ul1-none]');
  var q = card.querySelector('[data-ul1-q]');
  var S = { who: card.dataset.tab === 'customers' ? 'cust' : 'team', role: '', flt: '', q: '' };
  var A = { invited: 'inv', quiet: 'quiet', week: 'wk' };
  if (A[card.dataset.activity]) S.flt = A[card.dataset.activity];

  function match(r) {
    var d = r.dataset;
    if (d.who !== S.who) return false;
    if (S.role && d.role !== S.role) return false;
    if (S.q && d.text.toLowerCase().indexOf(S.q) === -1) return false;
    switch (S.flt) {
      case 'wk': return d.act === 'week';
      case 'inv': return d.act === 'invited';
      case 'quiet': return d.act === 'quiet';
      case 'own': return d.own === '1';
    }
    return true;
  }
  function apply() {
    var unv = S.flt === 'unv' && wait;
    var n = 0;
    rows.forEach(function (r) { var ok = match(r); r.hidden = !ok; if (ok) n++; });
    if (wait) wait.hidden = !unv;
    wrap.hidden = !!unv;
    none.hidden = n > 0;
    page.querySelectorAll('[data-ul1-who]').forEach(function (b) { b.classList.toggle('on', b.dataset.ul1Who === S.who); });
    page.querySelectorAll('.ul1-fp').forEach(function (b) { b.classList.toggle('on', b.dataset.ul1Flt === S.flt); });
    page.querySelectorAll('.ul1-tile').forEach(function (b) {
      var on = b.dataset.ul1Flt === S.flt; b.classList.toggle('on', on); b.setAttribute('aria-pressed', on);
    });
    page.querySelectorAll('.ul1-rrow').forEach(function (b) {
      var on = b.dataset.ul1Role === S.role; b.classList.toggle('on', on); b.setAttribute('aria-pressed', on);
    });
    var clr = page.querySelector('.ul1-lk'); if (clr) clr.hidden = !S.role;
    q.placeholder = S.who === 'cust' ? 'Search name, email or venue…' : 'Search name or email…';
    try { history.replaceState(null, '', S.who === 'cust' ? '/users?tab=customers' : '/users'); } catch (e) {}
  }

  page.addEventListener('click', function (e) {
    var t = e.target.closest('[data-ul1-who],[data-ul1-flt],[data-ul1-role]');
    if (!t || !page.contains(t)) return;
    if (t.hasAttribute('data-ul1-who')) { S.who = t.dataset.ul1Who; S.role = ''; }
    else if (t.hasAttribute('data-ul1-flt')) { S.flt = S.flt === t.dataset.ul1Flt && t.classList.contains('ul1-tile') ? '' : t.dataset.ul1Flt; }
    else {
      var v = t.dataset.ul1Role;
      S.role = S.role === v ? '' : v;
      if (S.role) { var r = rows.filter(function (x) { return x.dataset.role === S.role; })[0]; if (r) S.who = r.dataset.who; }
    }
    apply();
  });
  q.addEventListener('input', function () { S.q = q.value.trim().toLowerCase(); apply(); });

  // Sorting: Name A→Z, Role and Last active highest / newest first; a second click flips.
  var tb = table.tBodies[0];
  table.querySelectorAll('.ul1-sort').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var th = btn.closest('th'), key = btn.dataset.sort;
      var asc = th.classList.contains('on') ? th.dataset.dir !== 'asc' : key === 'name';
      table.querySelectorAll('th').forEach(function (x) { x.classList.remove('on'); });
      th.classList.add('on'); th.dataset.dir = asc ? 'asc' : 'desc';
      table.querySelectorAll('.ul1-sort i').forEach(function (i) { i.textContent = ''; });
      btn.querySelector('i').textContent = asc ? '↑' : '↓';
      rows.sort(function (a, b) {
        var x, y;
        if (key === 'name') { x = a.dataset.sName.toLowerCase(); y = b.dataset.sName.toLowerCase(); }
        else { x = +a.dataset['s' + key.charAt(0).toUpperCase() + key.slice(1)]; y = +b.dataset['s' + key.charAt(0).toUpperCase() + key.slice(1)]; }
        var c = x < y ? -1 : x > y ? 1 : a.dataset.sName.localeCompare(b.dataset.sName);
        return asc ? c : -c;
      });
      rows.forEach(function (r) { tb.appendChild(r); });
    });
  });

  // A click anywhere on a row opens the person, except on its own links.
  card.addEventListener('click', function (e) {
    var r = e.target.closest('tr.ft-row');
    if (!r || e.target.closest('a,button,summary,details,form,input')) return;
    if (window.htmx) htmx.ajax('GET', r.dataset.href, { target: 'body', pushUrl: true }); else location.href = r.dataset.href;
  });

  function fit() {
    var html = document.documentElement;
    if (!html.classList.contains('shell') || innerWidth < 1101 || innerHeight < 640) {
      page.classList.remove('fit'); page.style.removeProperty('--ul1-h'); return;
    }
    page.classList.remove('fit');
    var main = document.getElementById('main');
    var bottom = main ? main.getBoundingClientRect().bottom : innerHeight - 16;
    var dock = document.querySelector('.dock');
    if (dock && dock.getBoundingClientRect().top > 200) bottom = Math.min(bottom, dock.getBoundingClientRect().top - 12);
    var top = page.getBoundingClientRect().top + (main ? main.scrollTop : 0);
    var h = Math.floor(bottom - top - 8);
    if (h >= 380) { page.style.setProperty('--ul1-h', h + 'px'); page.classList.add('fit'); }
  }
  addEventListener('resize', fit);
  apply();
  fit();
  setTimeout(fit, 150);
  setTimeout(fit, 700);
  addEventListener('load', fit);
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(fit);
  var mainEl = document.getElementById('main');
  if (window.ResizeObserver && mainEl) new ResizeObserver(function () { fit(); }).observe(mainEl);
})();
