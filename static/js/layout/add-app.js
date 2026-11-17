// Add app on many devices (D1, 17 Nov 2026): mdmAddApp({serials}|{group}|{venue}) loads the
// dialog from GET /apps/add-to and posts the pick to POST /apps/add-to. The app is added to
// every device that lacks it; online ones install now, offline ones when they're back.
(function () {
  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function toast(m, t) { if (window.showToast) showToast(m, t || 'info'); }
  window.mdmAddApp = function (t) {
    var q = new URLSearchParams();
    if (t.serials) q.append('serials', t.serials.join(','));
    if (t.group) q.append('group', t.group);
    if (t.venue) q.append('venue', t.venue);
    fetch('/apps/add-to?' + q.toString(), { credentials: 'same-origin' }).then(function (r) {
      if (!r.ok) return r.text().then(function (x) { throw new Error((x || 'Could not open Add app').trim()); });
      return r.text();
    }).then(function (html) {
      var old = document.getElementById('add-app-dlg'); if (old) old.remove();
      var holder = document.createElement('div'); holder.innerHTML = html;
      var m = holder.firstElementChild; if (!m) return;
      document.body.appendChild(m);
      wire(m);
    }).catch(function (e) { toast(e.message, 'error'); });
  };
  function wire(m) {
    var back = document.activeElement, pane = m.querySelector('.aa-r'), say = m.querySelector('.aa-say'), add = m.querySelector('[data-aa-add]');
    var n = +m.dataset.n, online = +m.dataset.online, offline = +m.dataset.offline, pick = null;
    function close() { m.remove(); if (back && back.focus) back.focus(); }
    m.addEventListener('click', function (e) { if (e.target === m || e.target.closest('[data-aa-close]')) close(); });
    m.addEventListener('keydown', function (e) { if (e.key === 'Escape') close(); });
    var f = m.querySelector('[data-aa-filter]');
    if (f) { f.focus(); f.addEventListener('input', function () { var v = f.value.toLowerCase(); m.querySelectorAll('.aa-app').forEach(function (b) { b.hidden = v && (b.dataset.name || '').toLowerCase().indexOf(v) < 0; }); }); }
    function sentence() {
      if (!pick) { say.textContent = 'Pick an app'; add.disabled = true; return; }
      var lack = n - pick.ch.have;
      add.disabled = lack <= 0;
      if (lack <= 0) { say.innerHTML = 'All ' + n + ' already have <b>' + esc(pick.name) + '</b>.'; add.textContent = 'Add app'; return; }
      var label = esc(pick.name) + (pick.ch.label ? ' · ' + esc(pick.ch.label) : '');
      if (n === 1) {
        say.innerHTML = 'Adds <b>' + label + '</b>. It installs the newest build' + (pick.ch.newest ? ' (' + esc(pick.ch.newest) + ')' : '') + (online ? ' now.' : ' when the device is back online.');
        add.textContent = 'Add app';
      } else {
        say.innerHTML = 'Adds <b>' + label + '</b> to the ' + lack + ' that don\'t have it' + (offline ? '. Online ones install now, ' + offline + ' offline when they\'re back.' : ': they install now.');
        add.textContent = 'Add to ' + lack;
      }
    }
    m.querySelectorAll('.aa-app').forEach(function (b) {
      b.addEventListener('click', function () {
        m.querySelectorAll('.aa-app.on').forEach(function (x) { x.classList.remove('on'); }); b.classList.add('on');
        var chs = []; try { chs = JSON.parse(b.dataset.channels || '[]'); } catch (e) {}
        var name = b.dataset.name;
        pane.innerHTML = '<div class="aa-sep" style="padding-left:6px">' + (chs.length > 1 ? 'Channel' : esc(name)) + '</div>' + chs.map(function (c, i) {
          return '<div class="aa-ch" role="radio" tabindex="0" data-i="' + i + '"><span class="aa-rd"></span><span><b>' + esc(c.label || name) + '</b><small>' + esc(c.pkg) + '</small></span>' +
            '<span class="aa-have">' + (c.have ? (n === 1 ? 'on the device' : 'on ' + c.have + ' of them') : esc(c.newest || '')) + '</span></div>';
        }).join('');
        function choose(i) {
          pane.querySelectorAll('.aa-ch').forEach(function (r) { var on = +r.dataset.i === i; r.classList.toggle('on', on); r.setAttribute('aria-checked', on); });
          pick = { name: name, ch: chs[i] }; sentence();
        }
        pane.querySelectorAll('.aa-ch').forEach(function (r) {
          r.addEventListener('click', function () { choose(+r.dataset.i); });
          r.addEventListener('keydown', function (e) { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); choose(+r.dataset.i); } });
        });
        choose(0);
      });
    });
    add.addEventListener('click', function () {
      if (!pick) return;
      add.disabled = true;
      var body = new URLSearchParams(m.dataset.q); body.append('package', pick.ch.pkg);
      fetch('/apps/add-to', { method: 'POST', body: body, credentials: 'same-origin', headers: { 'HX-Request': 'true' } }).then(function (r) {
        return r.ok ? r.json() : r.text().then(function (x) { throw new Error((x || 'Could not add it').trim()); });
      }).then(function (j) {
        close(); toast(j.message, 'success');
        if (window.htmx) htmx.trigger(document.body, 'device-updated');
      }).catch(function (e) { add.disabled = false; toast(e.message, 'error'); });
    });
    sentence();
  }
})();
