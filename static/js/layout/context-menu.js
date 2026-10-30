// Right-click menus (R1, picked 30 Oct 2026): right-click a device, venue, group, app or
// person anywhere in <main> and our menu opens; Shift + right-click, and right-click in a
// field or on selected text, keep the browser's. The ContextMenu key or Shift+F10 opens it
// on the focused row. One panel (#ctx-menu, layout.html) for every page.
//
// What a thing is comes from data-ctx on it (Fleet rows and cards, the rail, Venues,
// Groups, Users), else from the row's own link (/devices/<serial>, /restaurants/<id>,
// /groups/<id>, /apps/<package>, /users/<id>/profile), else a row with data-href gets
// Open / Open in new tab / Copy link. Actions use the routes the pages already use, and
// only what data-ctx-caps says this viewer may do is offered; each route still checks.
(function () {
  if (window.mdmCtxMenu) return;
  window.mdmCtxMenu = true;

  var menu = null, caps = {}, opener = null, marked = [], items = [];
  function panel() {
    if (menu && document.body.contains(menu)) return menu;
    menu = document.getElementById('ctx-menu');
    try { caps = JSON.parse(menu.getAttribute('data-ctx-caps') || '{}'); } catch (e) { caps = {}; }
    return menu;
  }

  var ic = {
    open: '<path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/>',
    tab: '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="M3 9h18"/>',
    copy: '<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V5a1 1 0 0 0-1-1H5a1 1 0 0 0-1 1v10a1 1 0 0 0 1 1h3"/>',
    link: '<path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1"/>',
    shot: '<rect x="3" y="6" width="18" height="14" rx="2"/><circle cx="12" cy="13" r="3.5"/><path d="M8 6l1.5-2h5L16 6"/>',
    remote: '<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/>',
    reboot: '<path d="M20 12a8 8 0 1 1-2.3-5.7M20 4v5h-5"/>',
    lock: '<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>',
    unlock: '<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 7.5-2"/>',
    group: '<rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><path d="M17.5 14v7M14 17.5h7"/>',
    venue: '<path d="M12 21s-7-6.2-7-11a7 7 0 0 1 14 0c0 4.8-7 11-7 11z"/><circle cx="12" cy="10" r="2.5"/>',
    role: '<path d="M4 7h16M4 12h10M4 17h7"/>',
    list: '<path d="M8 6h13M8 12h13M8 18h13M3.5 6h.01M3.5 12h.01M3.5 18h.01"/>',
    edit: '<path d="M4 20h4L19 9l-4-4L4 16v4z"/>',
    hide: '<path d="M3 3l18 18M10.6 10.6a2 2 0 0 0 2.8 2.8M9.9 5.1A9.7 9.7 0 0 1 12 5c5 0 9 5 9 7a10 10 0 0 1-2.2 3.3M6.6 6.6C4.3 8 3 10.4 3 12c0 2 4 7 9 7a9.3 9.3 0 0 0 4.3-1"/>',
    show: '<path d="M3 12s3.5-7 9-7 9 7 9 7-3.5 7-9 7-9-7-9-7z"/><circle cx="12" cy="12" r="3"/>',
    trash: '<path d="M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3"/>',
    settings: '<circle cx="12" cy="12" r="3"/><path d="M12 2v3M12 19v3M2 12h3M19 12h3M4.9 4.9l2.1 2.1M17 17l2.1 2.1M4.9 19.1 7 17M17 7l2.1-2.1"/>',
    plus: '<path d="M12 5v14M5 12h14"/>',
    back: '<path d="M15 18l-6-6 6-6"/>',
    padlock: '<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>'
  };
  function svg(name) { return name ? '<svg viewBox="0 0 24 24" aria-hidden="true">' + (ic[name] || '') + '</svg>' : ''; }
  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function toast(msg, type) { if (window.showToast) window.showToast(msg, type || 'success'); }
  function abs(path) { return new URL(path, location.origin).href; }

  // ── What was right-clicked ────────────────────────────────────────────────
  var ROW = 'tr, .dl-card, .dl-lean-row, .fleet-rail-item, li, [data-href]';
  function classify(href) {
    var u;
    try { u = new URL(href, location.href); } catch (e) { return null; }
    if (u.origin !== location.origin) return null;
    var p = u.pathname, m;
    if ((m = p.match(/^\/devices\/([^/]+)\/?$/))) return { kind: 'device', serial: decodeURIComponent(m[1]) };
    if ((m = p.match(/^\/restaurants\/([0-9a-f-]{36})\/?$/))) return { kind: 'venue', id: m[1], page: p };
    if ((m = p.match(/^\/groups\/([0-9a-f-]{36})\/?$/))) return { kind: 'group', id: m[1], page: p };
    if ((m = p.match(/^\/apps\/([A-Za-z][\w.]*\.[\w.]+)\/?$/))) return { kind: 'app', pkg: m[1], page: p };
    if ((m = p.match(/^\/users\/([^/]+)\/profile\/?$/))) return { kind: 'user', page: p };
    return null;
  }
  function textOf(el) { return el ? (el.textContent || '').replace(/\s+/g, ' ').trim() : ''; }
  function find(el) {
    if (!el || !el.closest) return null;
    var main = document.getElementById('main');
    if (!main || !main.contains(el) || el.closest('.cc-menu, dialog, .modal-box')) return null;
    if (el.closest('input, textarea, select, [contenteditable=""], [contenteditable="true"]')) return null;
    var x = el.closest('[data-ctx]');
    if (x) {
      if (x.dataset.ctx === 'none') return null;
      var d = x.dataset;
      return { kind: d.ctx, el: x, serial: d.serial, id: d.id, name: d.name, page: d.page, pkg: d.package, email: d.email };
    }
    var row = el.closest(ROW);
    var link = el.closest('a[href]');
    var t = link && classify(link.getAttribute('href'));
    if (t) { t.el = (row && main.contains(row)) ? row : link; t.name = textOf(link); return t; }
    if (!row || !main.contains(row)) return null;
    var href = row.getAttribute('data-href');
    if (href) {
      t = classify(href) || { kind: 'link', page: href };
      t.el = row; t.name = textOf(row.querySelector('b, .nm, a')) || '';
      return t;
    }
    // A row whose own link names a device, venue, group, app or person.
    var links = row.querySelectorAll('a[href]');
    for (var i = 0; i < links.length; i++) {
      t = classify(links[i].getAttribute('href'));
      if (t) { t.el = row; t.name = textOf(links[i]); return t; }
    }
    return null;
  }

  // ── Doing things ──────────────────────────────────────────────────────────
  function go(url) {
    var a = document.createElement('a');
    a.href = url;
    a.style.display = 'none';
    document.body.appendChild(a);
    if (window.htmx) window.htmx.process(a);
    a.click();
    a.remove();
  }
  function newTab(url) { window.open(abs(url), '_blank', 'noopener'); }
  function copy(text, what) {
    var done = function () { toast('Copied ' + what); };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(done, function () { fallbackCopy(text); done(); });
    } else { fallbackCopy(text); done(); }
  }
  function fallbackCopy(text) {
    var ta = document.createElement('textarea');
    ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
    document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); } catch (e) {}
    ta.remove();
  }
  function reloadView() { go(location.pathname + location.search + location.hash); }
  // post sends a form the way htmx would, so routes answer with their toast (HX-Trigger)
  // instead of a redirect; a route that redirects anyway is followed.
  function post(url, fields, after) {
    var body = new URLSearchParams();
    Object.keys(fields).forEach(function (k) {
      [].concat(fields[k]).forEach(function (v) { body.append(k, v); });
    });
    return fetch(url, { method: 'POST', body: body, credentials: 'same-origin', headers: { 'HX-Request': 'true' } })
      .then(function (res) {
        var trig = res.headers.get('HX-Trigger'), shown = false;
        if (trig) {
          try { var t = JSON.parse(trig).toast; if (t && t.msg) { toast(t.msg, t.type); shown = true; } } catch (e) {}
        }
        if (!res.ok) {
          return res.text().then(function (txt) {
            var msg = txt;
            try { msg = JSON.parse(txt).error || txt; } catch (e) {}
            toast((msg || 'That did not work').trim().slice(0, 200), 'error');
          });
        }
        var to = res.headers.get('HX-Redirect') || (res.redirected ? res.url : '');
        if (after) return after(res, to, shown);
        if (to) go(to);
      })
      .catch(function () { toast('Could not reach the server', 'error'); });
  }
  // Commands go through POST /commands like the Fleet selection bar (roster.js bulkRun),
  // which lands on the command's page.
  function runCommand(type, serials, ask) {
    if (ask && !window.confirm(ask + '\n\n' + serials.slice(0, 8).join('\n') + (serials.length > 8 ? '\n…and ' + (serials.length - 8) + ' more' : ''))) return;
    var f = document.createElement('form');
    f.method = 'POST'; f.action = '/commands'; f.style.display = 'none';
    [['type', type], ['target_type', 'devices'], ['target_serials', serials.join('\n')]].forEach(function (kv) {
      var i = document.createElement('input'); i.type = 'hidden'; i.name = kv[0]; i.value = kv[1]; f.appendChild(i);
    });
    document.body.appendChild(f);
    f.submit();
  }
  var targets = null;
  function loadTargets() {
    if (targets) return Promise.resolve(targets);
    return fetch('/ctx/targets', { credentials: 'same-origin' }).then(function (r) {
      if (!r.ok) throw new Error('targets');
      return r.json();
    }).then(function (j) { targets = j; setTimeout(function () { targets = null; }, 60000); return j; });
  }

  // ── Menus ─────────────────────────────────────────────────────────────────
  // An item: {label, icon, kbd, run, ro (read only here), danger, keep (stays open)}.
  function deviceMenu(t) {
    var rows = [t.el], cb = t.el.querySelector && t.el.querySelector('.device-cb');
    if (cb && cb.checked) {
      var ticked = Array.prototype.map.call(document.querySelectorAll('.device-cb:checked'), function (b) { return b.closest('[data-serial]'); }).filter(Boolean);
      if (ticked.length > 1) rows = ticked;
    }
    var serials = rows.length > 1 ? rows.map(function (r) { return r.dataset.serial; }) : [t.serial];
    var n = serials.length, one = n === 1, el = t.el, d = el.dataset || {};
    var atVenue = rows.some(function (r) { return r.dataset && r.dataset.atv === '1'; });
    var ro = !!(caps.venueReadOnly && atVenue);
    var page = '/devices/' + encodeURIComponent(t.serial);
    var out = { head: one ? t.serial : n + ' devices', sub: one ? (d.type || '') : 'selected', items: [] };
    var it = out.items;
    if (one) {
      it.push({ label: 'Open', icon: 'open', kbd: '↵', run: function () { go(page); } });
      it.push({ label: 'Open in new tab', icon: 'tab', run: function () { newTab(page); } });
      it.push({ label: 'Copy serial', icon: 'copy', run: function () { copy(t.serial, t.serial); } });
      it.push({ label: 'Copy link', icon: 'link', run: function () { copy(abs(page), 'link'); } });
    } else {
      it.push({ label: 'Copy serials', icon: 'copy', run: function () { copy(serials.join('\n'), n + ' serials'); } });
    }
    it.push('sep');
    if (caps.screenshot) it.push({ label: one ? 'Screenshot' : 'Screenshot ' + n, icon: 'shot', run: function () { runCommand('screenshot', serials); } });
    if (caps.remote && one) it.push({ label: 'Remote', icon: 'remote', ro: ro, run: function () { location.href = page + '/remote'; } });
    if (caps.reboot) it.push({ label: one ? 'Reboot…' : 'Reboot ' + n + '…', icon: 'reboot', ro: ro, run: function () {
      runCommand('reboot', serials, 'Reboot ' + (one ? t.serial : n + ' devices') + '? Whatever is on screen restarts.');
    } });
    if (caps.lock && one) {
      var locked = d.locked === '1';
      it.push({ label: locked ? 'Unlock' : 'Lock', icon: locked ? 'unlock' : 'lock', ro: ro, run: function () {
        toast(locked ? 'Unlocking ' + t.serial + '…' : 'Locking ' + t.serial + '…', 'info');
        post(page + '/lock', { on: locked ? '0' : '1' }, function (res) {
          if (!res.ok) return;
          return res.json().then(function (j) {
            toast((locked ? 'Unlocked' : 'Locked') + (j.delivered ? ' · the device has it' : ' · at its next check-in'));
            if (el.dataset) el.dataset.locked = locked ? '' : '1';
          }, function () {});
        });
      } });
    }
    var tail = [];
    if (caps.group) tail.push({ label: 'Add to group…', icon: 'group', ro: ro, keep: true, run: function () { groupPicker(serials); } });
    if (caps.venue) tail.push({ label: 'Move to venue…', icon: 'venue', ro: ro, keep: true, run: function () { venuePicker(serials); } });
    if (caps.role && one && d.agent === 'dpc') tail.push({ label: 'Role…', icon: 'role', ro: ro, keep: true, rt: d.type, run: function () { rolePicker(t.serial, d.class); } });
    if (tail.length) { it.push('sep'); it.push.apply(it, tail); }
    return out;
  }
  function venueMenu(t) {
    var devs = '/devices?restaurant=' + t.id;
    var out = { head: t.name || 'Venue', sub: 'venue', items: [] }, it = out.items;
    if (t.page) {
      it.push({ label: 'Open', icon: 'open', run: function () { go(t.page); } });
      it.push({ label: 'Open in new tab', icon: 'tab', run: function () { newTab(t.page); } });
    }
    it.push({ label: 'Show its devices', icon: 'list', run: function () { go(devs); } });
    it.push({ label: 'Copy link', icon: 'link', run: function () { copy(abs(t.page || devs), 'link'); } });
    if (t.page && caps.venue) { it.push('sep'); it.push({ label: 'Venue settings', icon: 'settings', run: function () { go(t.page + '?open=profile'); } }); }
    return out;
  }
  function groupMenu(t) {
    var devs = '/devices?group=' + t.id;
    var out = { head: t.name || 'Group', sub: 'group', items: [] }, it = out.items;
    if (t.page) {
      it.push({ label: 'Open', icon: 'open', run: function () { go(t.page); } });
      it.push({ label: 'Open in new tab', icon: 'tab', run: function () { newTab(t.page); } });
    }
    it.push({ label: 'Show its devices', icon: 'list', run: function () { go(devs); } });
    it.push({ label: 'Copy link', icon: 'link', run: function () { copy(abs(t.page || devs), 'link'); } });
    var tail = [];
    if (caps.group) tail.push({ label: 'Rename…', icon: 'edit', run: function () {
      var name = window.prompt('New name for ' + t.name + ':', t.name || '');
      if (name === null || !name.trim() || name.trim() === t.name) return;
      post('/groups/' + t.id, { name: name.trim() }, function (res) { if (res.ok) { toast('Renamed to ' + name.trim()); reloadView(); } });
    } });
    if (caps.admin) {
      var hidden = t.el && t.el.dataset && t.el.dataset.hidden === '1';
      tail.push({ label: hidden ? 'Show to everyone' : 'Hide (super admins only)', icon: hidden ? 'show' : 'hide', run: function () {
        post('/groups/' + t.id + '/hidden', { hidden: hidden ? '0' : '1' }, function (res) { if (res.ok) reloadView(); });
      } });
    }
    if (caps.group) tail.push({ label: 'Delete…', icon: 'trash', danger: true, run: function () {
      var typed = window.prompt('Delete the group ' + t.name + '? Its devices stay; only the group goes.\n\nType the group\'s name to confirm:', '');
      if (typed === null) return;
      if (typed.trim() !== t.name) { toast('The name typed does not match', 'error'); return; }
      post('/groups/' + t.id + '/delete', { confirm_name: typed.trim() }, function (res) {
        if (!res.ok) return;
        toast('Deleted ' + t.name);
        if (location.pathname.indexOf(t.id) >= 0 || location.search.indexOf(t.id) >= 0) go('/devices'); else reloadView();
      });
    } });
    if (tail.length) { it.push('sep'); it.push.apply(it, tail); }
    return out;
  }
  function simpleMenu(t, head, sub, extra) {
    var out = { head: head, sub: sub, items: [] }, it = out.items;
    it.push({ label: 'Open', icon: 'open', run: function () { go(t.page); } });
    it.push({ label: 'Open in new tab', icon: 'tab', run: function () { newTab(t.page); } });
    (extra || []).forEach(function (x) { it.push(x); });
    it.push({ label: 'Copy link', icon: 'link', run: function () { copy(abs(t.page), 'link'); } });
    return out;
  }
  function menuFor(t) {
    switch (t.kind) {
      case 'device': return deviceMenu(t);
      case 'venue': return venueMenu(t);
      case 'group': return groupMenu(t);
      case 'app': return simpleMenu(t, t.name || t.pkg, 'app', [{ label: 'Copy package', icon: 'copy', run: function () { copy(t.pkg, t.pkg); } }]);
      case 'user': return simpleMenu(t, t.name || 'Person', 'person', t.email ? [{ label: 'Copy email', icon: 'copy', run: function () { copy(t.email, t.email); } }] : []);
      case 'link': return simpleMenu(t, t.name || 'Link', '', []);
    }
    return null;
  }

  // ── Pickers (Add to group…, Move to venue…, Role…): the menu swaps to a list ──
  function picker(title, sub, rows, choose, extra) {
    var m = panel();
    var h = '<div class="cc-mh"><span>' + esc(title) + '</span><span class="sub">' + esc(sub) + '</span></div>';
    if (rows.length > 7) h += '<div class="cc-mfield"><input type="search" placeholder="Search" aria-label="Search ' + esc(title) + '"></div>';
    h += '<div class="ctx-list">';
    rows.forEach(function (r, i) {
      h += '<button type="button" class="cc-mi' + (r.active ? ' active' : '') + '" role="menuitem" data-i="' + i + '" data-q="' + esc(r.label.toLowerCase()) + '">' + esc(r.label) + '</button>';
    });
    if (!rows.length) h += '<div class="ctx-empty">Nothing to pick</div>';
    h += '</div>';
    if (extra) h += '<hr class="cc-msep"><button type="button" class="cc-mi" role="menuitem" data-extra="1">' + svg(extra.icon) + esc(extra.label) + '</button>';
    m.innerHTML = h;
    items = rows;
    m.onclick = function (e) {
      var b = e.target.closest('.cc-mi');
      if (!b) return;
      close();
      if (b.dataset.extra) extra.run(); else choose(rows[+b.dataset.i]);
    };
    var q = m.querySelector('input[type=search]');
    if (q) {
      q.addEventListener('input', function () {
        var v = q.value.trim().toLowerCase();
        m.querySelectorAll('.ctx-list .cc-mi').forEach(function (b) { b.hidden = v && b.dataset.q.indexOf(v) < 0; });
      });
      q.focus();
    } else {
      var f = m.querySelector('.cc-mi'); if (f) f.focus();
    }
    clamp(m);
  }
  function groupPicker(serials) {
    var n = serials.length, what = n === 1 ? serials[0] : n + ' devices';
    loadTargets().then(function (t) {
      picker('Add to group', what, t.groups.map(function (g) { return { label: g.name, id: g.id }; }), function (g) {
        post('/groups/' + g.id + '/devices', { serials: serials.join('\n') }, function (res, to, shown) {
          if (res.ok && !shown) toast('Added to ' + g.label);
          if (res.ok) reloadView();
        });
      }, { label: 'New group…', icon: 'plus', run: function () {
        var name = window.prompt('Name of the new group for ' + what + ':', '');
        if (name === null || !name.trim()) return;
        post('/groups', { name: name.trim(), serials: serials.join('\n') }, function (res, to) {
          if (res.ok) { toast('Made the group ' + name.trim()); if (to) go(to); }
        });
      } });
    }, function () { toast('Could not load the groups', 'error'); close(); });
  }
  function venuePicker(serials) {
    var n = serials.length, what = n === 1 ? serials[0] : n + ' devices';
    loadTargets().then(function (t) {
      picker('Move to venue', what, t.venues.map(function (v) { return { label: v.name, id: v.id }; }), function (v) {
        if (!window.confirm('Move ' + what + ' to ' + v.label + '? It leaves the Lab and is set up there.')) return;
        post('/devices/bulk-restaurant', { serials: serials.join('\n'), restaurant_id: v.id }, function (res, to) {
          if (res.ok) { toast('Moved to ' + v.label); if (to) go(to); else reloadView(); }
        });
      });
    }, function () { toast('Could not load the venues', 'error'); close(); });
  }
  function rolePicker(serial, current) {
    var rows = (caps.roles || []).map(function (r) { return { label: r[1], key: r[0], active: r[0] === current }; });
    picker('Role', serial, rows, function (r) {
      if (r.key === current) return;
      if (!window.confirm('Make ' + serial + ' a ' + r.label + '? Its role app and lock rules follow.')) return;
      post('/devices/' + encodeURIComponent(serial) + '/role', { class: r.key }, function (res) { if (res.ok) reloadView(); });
    });
  }

  // ── Drawing, placing, closing ─────────────────────────────────────────────
  function draw(spec) {
    var m = panel(), h = '<div class="cc-mh"><span>' + esc(spec.head) + '</span>' + (spec.sub ? '<span class="sub">' + esc(spec.sub) + '</span>' : '') + '</div>';
    items = [];
    var lastSep = true;
    spec.items.forEach(function (it) {
      if (it === 'sep') { if (!lastSep) h += '<hr class="cc-msep">'; lastSep = true; return; }
      lastSep = false;
      var i = items.push(it) - 1;
      var right = it.ro ? '<span class="ctx-ro">' + svg('padlock') + 'read only</span>'
        : it.kbd ? '<kbd>' + esc(it.kbd) + '</kbd>' : it.rt ? '<span class="rt">' + esc(it.rt) + '</span>' : '';
      h += '<button type="button" class="cc-mi' + (it.ro ? ' is-off' : '') + (it.danger ? ' danger' : '') + '" role="menuitem" data-i="' + i + '"' +
        (it.ro ? ' aria-disabled="true" title="Read only: this device is at a venue"' : '') + '>' + svg(it.icon) + esc(it.label) + right + '</button>';
    });
    h += '<div class="cc-mfoot">Shift + right-click: browser menu</div>';
    m.innerHTML = h;
    m.onclick = function (e) {
      var b = e.target.closest('.cc-mi');
      if (!b || b.classList.contains('is-off')) return;
      var it = items[+b.dataset.i];
      if (!it) return;
      if (!it.keep) close();
      it.run();
    };
  }
  function clamp(m) {
    var r = m.getBoundingClientRect(), vw = document.documentElement.clientWidth, vh = window.innerHeight;
    var x = parseFloat(m.style.left) || 0, y = parseFloat(m.style.top) || 0;
    if (x + r.width > vw - 8) x = Math.max(8, vw - r.width - 8);
    if (y + r.height > vh - 8) y = Math.max(8, vh - r.height - 8);
    m.style.left = x + 'px'; m.style.top = y + 'px';
  }
  function open(t, x, y) {
    var spec = menuFor(t);
    if (!spec) return false;
    close();
    var m = panel();
    if (!m) return false;
    draw(spec);
    m.hidden = false;
    m.style.left = x + 'px'; m.style.top = y + 'px';
    clamp(m);
    marked = [t.el];
    if (t.kind === 'device' && t.el.querySelector) {
      var cb = t.el.querySelector('.device-cb');
      if (cb && cb.checked) marked = Array.prototype.map.call(document.querySelectorAll('.device-cb:checked'), function (b) { return b.closest('[data-serial]'); }).filter(Boolean);
    }
    marked.forEach(function (r) { if (r && r.classList) r.classList.add('ctx-on'); });
    var first = m.querySelector('.cc-mi:not(.is-off)');
    if (first) first.focus({ preventScroll: true });
    return true;
  }
  function close() {
    var m = panel();
    if (m && !m.hidden) {
      m.hidden = true;
      m.innerHTML = '';
      if (opener && document.body.contains(opener) && opener.focus) opener.focus({ preventScroll: true });
    }
    marked.forEach(function (r) { if (r && r.classList) r.classList.remove('ctx-on'); });
    marked = []; opener = null;
  }

  document.addEventListener('contextmenu', function (e) {
    if (e.defaultPrevented) return; // the remote screen's own right-click (Back)
    if (e.shiftKey) { close(); return; }
    var sel = window.getSelection && String(window.getSelection());
    if (sel && sel.trim()) return;
    var t = find(e.target);
    if (!t) { close(); return; }
    opener = null;
    if (open(t, e.clientX, e.clientY)) e.preventDefault();
  });
  document.addEventListener('keydown', function (e) {
    var m = panel();
    if (m && !m.hidden && m.contains(document.activeElement)) {
      var list = Array.prototype.filter.call(m.querySelectorAll('.cc-mi'), function (b) { return !b.hidden && !b.classList.contains('is-off'); });
      var i = list.indexOf(document.activeElement);
      if (e.key === 'ArrowDown') { e.preventDefault(); if (list.length) list[(i + 1) % list.length].focus(); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); if (list.length) list[(i - 1 + list.length) % list.length].focus(); }
      else if (e.key === 'Home') { e.preventDefault(); if (list.length) list[0].focus(); }
      else if (e.key === 'End') { e.preventDefault(); if (list.length) list[list.length - 1].focus(); }
      else if (e.key === 'Escape') { e.preventDefault(); close(); }
      else if (e.key === 'Tab') { close(); }
      return;
    }
    if (m && !m.hidden && e.key === 'Escape') { close(); return; }
    if (e.key === 'ContextMenu' || (e.shiftKey && e.key === 'F10')) {
      var a = document.activeElement, t = find(a);
      if (!t) return;
      e.preventDefault();
      var r = (t.el || a).getBoundingClientRect();
      opener = a;
      open(t, Math.min(r.left + 24, r.right), r.top + Math.min(r.height, 32));
    }
  });
  document.addEventListener('mousedown', function (e) {
    var m = panel();
    if (m && !m.hidden && !m.contains(e.target)) close();
  }, true);
  window.addEventListener('scroll', function (e) {
    var m = panel();
    if (m && !m.hidden && !m.contains(e.target)) close();
  }, true);
  window.addEventListener('resize', close);
  window.addEventListener('blur', function () { var m = panel(); if (m && !m.hidden && !m.contains(document.activeElement)) close(); });
  document.addEventListener('htmx:beforeRequest', function (e) {
    var el = e.detail && e.detail.elt;
    if (el && el.tagName === 'A' || (e.detail && e.detail.boosted)) close();
  });
})();
