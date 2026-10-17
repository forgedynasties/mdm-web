// Loaded by templates/commands.html.
  // "Show admin actions" (admins only): the server hides actions sent by admin
  // accounts — from the lists and the counts — unless this cookie is "1".
  (function () {
    var on = /(?:^|; )mdm_show_admin_actions=1(?:;|$)/.test(document.cookie);
    function sync() { document.querySelectorAll('.adm-toggle input').forEach(function (i) { i.checked = on; }); }
    window.mdmAdminActions = function (v) {
      document.cookie = 'mdm_show_admin_actions=' + (v ? '1' : '0') + '; path=/; max-age=31536000; samesite=lax';
      location.reload();
    };
    sync();
  })();

  // Live activity: subscribe to the global command feed and, on any command-update,
  // nudge the timeline to re-fetch + morph in place (no reload, no flash). The named
  // SSE event is "command-update" (see CommandsFeedEvents). mdmStream auto-closes on nav.
  (function () {
    if (!window.EventSource || !window.mdmStream) return;
    var es = window.mdmStream('/commands/events');
    es.addEventListener('command-update', function () {
      document.body.dispatchEvent(new CustomEvent('commands-feed-update'));
    });
  })();

  // Client-side filter over the live activity rows (search + type + target).
  function filterCommands() {
    var q = (document.getElementById('cmd-search').value || '').toLowerCase();
    var ft = document.getElementById('filter-type').value;
    var fg = document.getElementById('filter-target').value;
    var rows = document.querySelectorAll('#cmd-history-table .htrow[data-cmd-type]');
    rows.forEach(function (r) {
      var typeMatch = !ft || r.dataset.cmdType === ft;
      var targetMatch = !fg || r.dataset.cmdTarget === fg;
      var searchMatch = !q || (r.dataset.cmdSearch || '').toLowerCase().indexOf(q) !== -1;
      r.style.display = typeMatch && targetMatch && searchMatch ? '' : 'none';
    });
    document.querySelectorAll('#cmd-history-table .hist-day').forEach(function (h) {
      var vis = false, n = h.nextElementSibling;
      while (n && (n.classList.contains('htrow') || n.classList.contains('hist-day'))) {
        if (n.classList.contains('htrow') && n.style.display !== 'none') { vis = true; break; }
        if (n.classList.contains('hist-day')) break;
        n = n.nextElementSibling;
      }
      h.style.display = vis ? '' : 'none';
    });
    document.querySelectorAll('#cmd-history-table .hgrp-h').forEach(function (h) {
      var vis = false, n = h.nextElementSibling;
      while (n && (n.classList.contains('htrow') || n.classList.contains('hist-day'))) {
        if (n.classList.contains('htrow') && n.style.display !== 'none') { vis = true; break; }
        n = n.nextElementSibling;
      }
      h.style.display = vis ? '' : 'none';
    });
  }

  // Show toast on a feed row's resend form (replaced across boosted swaps).
  if (window.__coResendToast) document.removeEventListener('submit', window.__coResendToast);
  window.__coResendToast = function (evt) {
    var form = evt.target;
    if (!form || form.tagName !== 'FORM') return;
    if (form.action.indexOf('/resend') !== -1) {
      setTimeout(function () { if (window.showToast) showToast('Action resent', 'success'); }, 100);
    }
  };
  document.addEventListener('submit', window.__coResendToast);

  // Pin (recipe) modal + delete — pins are shared server-side recipes so the
  // Schedules page can run them on a timer.
  function openRecipeModal() {
    var t = document.getElementById('f-type');
    if (!t || !t.value) { if (window.showToast) showToast('Choose an action and targets first, then pin it', 'info'); return; }
    document.getElementById('recipe-name').value = '';
    document.getElementById('recipe-modal-error').style.display = 'none';
    document.getElementById('recipe-modal').classList.add('modal-open');
    document.getElementById('recipe-name').focus();
  }
  function closeRecipeModal(e) {
    var m = document.getElementById('recipe-modal');
    if (m && (!e || e.target === m)) m.classList.remove('modal-open');
  }
  async function saveRecipe() {
    var name = document.getElementById('recipe-name').value.trim();
    var errEl = document.getElementById('recipe-modal-error');
    if (!name) { errEl.textContent = 'Give the pin a name.'; errEl.style.display = ''; return; }
    var fd = new FormData(document.getElementById('cmd-form'));
    fd.set('name', name);
    var res = await fetch('/recipes', { method: 'POST', body: new URLSearchParams(fd) });
    if (!res.ok && res.status !== 200) {
      var t = await res.text();
      errEl.textContent = t || 'Failed to save the pin.'; errEl.style.display = ''; return;
    }
    window.location = '/commands';
  }
  function deleteRecipe(e, id) {
    e.preventDefault(); e.stopPropagation();
    if (!confirm('Delete this pin?')) return;
    var f = document.createElement('form');
    f.method = 'POST'; f.action = '/recipes/' + id + '/delete';
    document.body.appendChild(f); f.submit();
  }

// ── The target-first console ───────────────────────────────────────────────────
// Left rail builds the device selection (scopes, quick filters, search, pasted
// serials); the right-hand action grid adapts to it via the /commands/impact
// counts (incl. data-dpc), and a one-row payload strip composes + sends. The
// hidden #cmd-form carries the exact field contract CommandCreate expects —
// identical to the previous palette implementation.
(function () {
  'use strict';
  var form = document.getElementById('cmd-form');
  if (!form || form.dataset.coInit) return;
  form.dataset.coInit = '1';
  var DS = window.CO_DS || {};
  DS.actions = DS.actions || []; DS.apps = DS.apps || []; DS.groups = DS.groups || [];
  DS.restaurants = DS.restaurants || []; DS.queries = DS.queries || []; DS.packages = DS.packages || [];
  DS.shellRecent = DS.shellRecent || []; DS.shellPopular = DS.shellPopular || [];
  var CAN = !!window.CO_CAN_OPERATE;

  var ICONS = {
    install_apk: '<path d="M12 3v10"/><path d="m8 9 4 4 4-4"/><path d="M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2"/>',
    uninstall: '<path d="M3 6h18"/><path d="M8 6V4h8v2"/><path d="M6 6l1 14h10l1-14"/>',
    reboot: '<path d="M4 12a8 8 0 0 1 14-5"/><path d="M18 3v4h-4"/><path d="M20 12a8 8 0 0 1-14 5"/><path d="M6 21v-4h4"/>',
    set_kiosk: '<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/>',
    update_splash: '<rect x="3" y="5" width="18" height="14" rx="2"/><circle cx="9" cy="10" r="1.6"/><path d="m5 17 4-4 3 3 3-3 4 4"/>',
    screenshot: '<rect x="3" y="7" width="18" height="12" rx="2"/><circle cx="12" cy="13" r="3"/><path d="M8 7l1.5-2h5L16 7"/>',
    app_reload: '<path d="M20 11a8 8 0 1 0-2.3 5.7"/><path d="M20 4v7h-7"/>',
    app_restart: '<path d="M12 3v8"/><path d="M7.5 6.3a7 7 0 1 0 9 0"/>',
    app_clear_cache: '<path d="m7 21-4-4 11-11 4 4z"/><path d="M14 6 18 2l4 4-4 4"/><path d="M11 21h10"/>',
    app_update_check: '<path d="M12 3v12"/><path d="m7 10 5 5 5-5"/><path d="M5 21h14"/>',
    app_update: '<rect x="6" y="2" width="12" height="20" rx="2"/><path d="M12 7v7"/><path d="m9 11 3 3 3-3"/><path d="M11 18.5h2"/>',
    shell: '<path d="m4 7 5 5-5 5"/><path d="M12 17h8"/>',
    query: '<circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/>',
    collect_logs: '<path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5M9 13h6M9 17h6"/>',
    wipe: '<path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"/><path d="M12 9v4"/><path d="M12 17h.01"/>',
    all: '<circle cx="12" cy="12" r="9"/><path d="M12 3v18M3 12h18"/>',
    place: '<path d="M3 21h18"/><path d="M5 21V7l7-4 7 4v14"/>',
    group: '<circle cx="9" cy="8" r="3"/><circle cx="16" cy="10" r="2.4"/><path d="M4 19c0-2.8 2.2-5 5-5s5 2.2 5 5"/><path d="M14.5 15.6c2 .3 3.5 1.7 3.5 3.4"/>',
    device: '<rect x="7" y="3" width="10" height="18" rx="2"/><path d="M11 18h2"/>',
    dot: '<circle cx="12" cy="12" r="8"/>'
  };
  // Fleet-wide console offers only the broad-safe actions. Locked mode, boot splash,
  // shell and factory wipe are per-device decisions and live on the device page.
  var GRID_GROUPS = [
    ['Apps', ['install_apk', 'uninstall']],
    ['Device', ['reboot']],
    ['Diagnostics', ['screenshot', 'query', 'collect_logs']]
  ];
  // Not offered from the fleet console at all (they would otherwise land in "Other").
  var GRID_HIDDEN = { set_kiosk: true, update_splash: true, shell: true, wipe: true, unenroll: true,
                      app_update: true, app_reload: true, app_restart: true,
                      app_clear_cache: true, app_update_check: true };

  // ── State ──
  var S = { action: null, apps: [], pkgs: [], shell: '', query: null, kiosk: null, splash: '',
            targets: [], refine: { status: '', battery: '', kiosk: '' }, reason: '' };
  var QF = { online: false, offline: false, kiosk: false };
  var lastImpact = { total: 0, effective: 0, online: 0, offline: 0, dpc: 0, verb: '' };
  var lastWarn = '';
  var armed = false, armTimer = null;

  var el = function (id) { return document.getElementById(id); };
  var qEl = el('co-q'), menuEl = el('co-menu'), stripEl = el('co-strip');
  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function svg(k) { return '<svg viewBox="0 0 24 24">' + (ICONS[k] || ICONS.dot) + '</svg>'; }
  function tail(u) { var i = u.lastIndexOf('/'); return i >= 0 && i < u.length - 1 ? u.slice(i + 1) : u; }
  function toast(m, t) { if (window.showToast) showToast(m, t || 'info'); }
  function setHint(m) {
    var n = el('co-note'); if (!n) return;
    if (m) { n.innerHTML = '<span class="hint">' + esc(m) + '</span>'; if (stripEl.hidden) { stripEl.hidden = false; } }
    else updateNote();
  }

  // ── Form sync: rebuild the hidden inputs to CommandCreate's exact contract ──
  function syncForm() {
    el('f-type').value = S.action ? S.action.t : '';
    var dyn = el('f-dyn'); dyn.innerHTML = '';
    function add(name, value) { var i = document.createElement('input'); i.type = 'hidden'; i.name = name; i.value = value; dyn.appendChild(i); }
    if (S.action) switch (S.action.p) {
      case 'apps': S.apps.forEach(function (a) { add('apk_url', a.url); }); break;
      case 'pkgs': S.pkgs.forEach(function (p) { add('package', p.pkg); }); break;
      case 'shell': if (S.shell.trim()) add('shell_cmd', S.shell.trim()); break;
      case 'query': if (S.query) add('query_id', S.query.id); break;
      case 'kiosk': if (S.kiosk) { add('kiosk_enabled', S.kiosk.enabled); if (S.kiosk.enabled === '1' && S.kiosk.pkg) add('kiosk_package', S.kiosk.pkg); } break;
      case 'splash': if (S.splash.trim()) add('splash_url', S.splash.trim()); break;
    }
    el('f-reason').value = S.reason.trim();
    var tt = '', smode = '', sid = '', serials = [];
    var kinds = S.targets.map(function (t) { return t.kind; });
    if (S.targets.length) {
      if (kinds[0] === 'all') { tt = 'scope'; smode = 'all'; }
      else if (S.targets.length === 1 && kinds[0] === 'restaurant') { tt = 'scope'; smode = 'restaurant'; sid = S.targets[0].id; }
      else if (kinds.every(function (k) { return k === 'group'; })) {
        if (S.targets.length === 1) { tt = 'scope'; smode = 'group'; sid = S.targets[0].id; }
        else { tt = 'groups'; S.targets.forEach(function (t) { add('target_groups', t.id); }); }
      } else if (kinds.every(function (k) { return k === 'device'; })) { tt = 'devices'; serials = S.targets.map(function (t) { return t.serial; }); }
      else {
        // restaurants + groups + devices together: the server unions them by device
        tt = 'mixed';
        S.targets.forEach(function (t) {
          if (t.kind === 'restaurant') add('target_restaurants', t.id);
          else if (t.kind === 'group') add('target_groups', t.id);
          else if (t.kind === 'device') serials.push(t.serial);
        });
      }
    }
    el('f-ttype').value = tt;
    el('f-smode').value = smode;
    el('f-sid').value = sid;
    var scoped = tt === 'scope';
    el('f-sstatus').value = scoped ? S.refine.status : '';
    el('f-sbattery').value = scoped ? S.refine.battery : '';
    el('f-skiosk').value = scoped ? S.refine.kiosk : '';
    el('f-serials').value = serials.join('\n');
    document.body.dispatchEvent(new CustomEvent('impact')); // debounced 200ms by the hx-trigger
  }

  // ── Validation: what's still missing before a send can go out ──
  function firstMissing() {
    if (!S.targets.length) return { msg: 'Pick devices first — search, tick a scope or paste serials on the left.' };
    if (!S.action) return { msg: 'Pick an action from the grid.' };
    switch (S.action.p) {
      case 'apps': if (!S.apps.length) return { msg: 'Choose at least one app to install.' }; break;
      case 'pkgs': if (!S.pkgs.length) return { msg: 'Choose at least one package to uninstall.' }; break;
      case 'shell': if (!S.shell.trim()) return { msg: 'Enter a shell command.' }; break;
      case 'query': if (!S.query) return { msg: 'Pick a query to run.' }; break;
      case 'kiosk': if (!S.kiosk || (S.kiosk.enabled === '1' && !S.kiosk.pkg)) return { msg: 'Pick locked mode — lock to an app, or unlock.' }; break;
      case 'splash': if (!S.splash.trim()) return { msg: 'Paste the splash image URL.' }; break;
    }
    if (S.action.destr && DS.requireReason && !S.reason.trim()) return { msg: 'A reason is required for this action.', focus: 'co-reason' };
    return null;
  }

  // ── Capability math (from the impact fragment's counts) ──
  // cap "system app": the privileged AOSP client only — DPC devices can't run it.
  // cap "DPC only": the Device-Owner agent only — system-app devices can't run it.
  // The server counts, per action, how many of the resolved targets cannot run it
  // (from each device's advertised capabilities). The DPC split is only a fallback
  // for a stale fragment without that data.
  function incompatCount(a) {
    if (!lastImpact.total) return 0;
    if (lastImpact.unsup) return lastImpact.unsup[a.t] || 0;
    if (a.cap === 'DPC only') return lastImpact.effective - lastImpact.dpc;
    if (a.cap === 'system app') return lastImpact.dpc;
    return 0;
  }
  function incompatWhy(a, n) {
    var eff = lastImpact.total || lastImpact.effective;
    return n + ' of ' + eff + ' can’t run ' + a.n.toLowerCase() + ' on their agent — named in the grid, not queued';
  }

  // ── Rail: scopes ──
  function scopeList() {
    var list = [{ kind: 'all', name: 'All devices', meta: 'the whole fleet — asks to confirm', icon: 'all' }];
    DS.restaurants.forEach(function (r) { list.push({ kind: 'restaurant', id: r.id, name: r.name, count: r.count, meta: r.count + ' device' + (r.count === 1 ? '' : 's'), icon: 'place' }); });
    DS.groups.forEach(function (g) { list.push({ kind: 'group', id: g.id, name: g.name, count: g.count, meta: g.count + ' device' + (g.count === 1 ? '' : 's'), icon: 'group' }); });
    return list;
  }
  function selKey(t) { return t.kind + ':' + (t.id || t.serial || 'all'); }
  function isSel(kind, id) { return S.targets.some(function (t) { return t.kind === kind && (t.id || 'all') === (id || 'all'); }); }
  var scopeTab = 'all'; // all | restaurant | group — which scope kinds the list shows
  function renderScopes() {
    var q = (qEl.value || '').trim().toLowerCase();
    var html = '';
    scopeList().forEach(function (s) {
      // "All devices" stays on every tab; the tabs narrow the rest by kind.
      if (scopeTab !== 'all' && s.kind !== 'all' && s.kind !== scopeTab) return;
      if (q && (s.name + ' ' + s.kind).toLowerCase().indexOf(q) < 0) return;
      var on = isSel(s.kind, s.id);
      html += '<button type="button" class="co-scope' + (on ? ' on' : '') + '" data-kind="' + s.kind + '" data-id="' + esc(s.id || '') + '">'
        + '<span class="ic">' + svg(s.icon) + '</span>'
        + '<span class="bd"><span class="n">' + esc(s.name) + '</span><span class="m">' + (s.kind === 'all' ? '' : '<span class="kt">' + s.kind + '</span>') + esc(s.meta) + '</span></span>'
        + '<span class="add">' + (on ? 'Added' : '+ Add') + '</span></button>';
    });
    el('co-scopes').innerHTML = html || '<div class="co-listempty">No scope matches.</div>';
  }

  // ── Rail: device list (via the /commands/browse-devices partial) ──
  var devCache = {}, devMeta = {}, devLoading = false, devTimer = null;
  var qfCounts = null; // {online, offline} from the unfiltered base list
  function parseRows(html) {
    var doc = new DOMParser().parseFromString(html, 'text/html');
    var out = [];
    doc.querySelectorAll('.cb-row').forEach(function (row) {
      var cb = row.querySelector('.cmd-browse-cb'); if (!cb) return;
      var bat = row.querySelector('.dp-bchip'); var sub = row.querySelector('.cb-sub');
      out.push({ serial: cb.value, online: !!row.querySelector('.cb-status i.on'),
                 dpc: row.getAttribute('data-dpc') === '1',
                 product: row.getAttribute('data-product') || '',
                 model: row.getAttribute('data-model') || '',
                 bat: bat && bat.textContent !== '—' ? bat.textContent.trim() : '',
                 sub: sub ? sub.textContent.trim() : '' });
    });
    return out;
  }
  function noteMeta(rows) { rows.forEach(function (d) { devMeta[d.serial] = d; }); }
  function devKey() { return (qEl.value || '').trim() + '|' + (QF.kiosk ? 'k' : ''); }
  function ensureDevices(now) {
    var key = devKey();
    if (devCache[key]) { renderDevs(); return; }
    clearTimeout(devTimer);
    devTimer = setTimeout(function () {
      devLoading = true; renderDevs();
      var p = new URLSearchParams();
      var q = (qEl.value || '').trim();
      // A single pasted serial keeps its bullet ("-AT070…"); search the clean one.
      if (q) p.set('q', cleanToken(q) || q);
      if (QF.kiosk) p.set('kiosk', 'enabled');
      fetch('/commands/browse-devices?' + p.toString()).then(function (r) { return r.text(); }).then(function (html) {
        var rows = parseRows(html);
        noteMeta(rows);
        devCache[key] = rows; devLoading = false;
        if (key === '|' && !qfCounts) {
          qfCounts = { online: 0, offline: 0 };
          rows.forEach(function (d) { d.online ? qfCounts.online++ : qfCounts.offline++; });
          renderQuick();
        }
        renderDevs();
      }).catch(function () { devLoading = false; renderDevs(); });
    }, now ? 0 : 250);
  }
  function renderQuick() {
    document.querySelectorAll('#co-quick .co-qf').forEach(function (b) {
      var k = b.dataset.qf;
      b.classList.toggle('on', !!QF[k]);
      var n = b.querySelector('.n');
      if (n && qfCounts && k !== 'kiosk') n.textContent = qfCounts[k];
    });
  }
  // Pasted serials often carry list decoration ("-AT070…", "• AT070…", "1. AT070…",
  // a trailing comma, quotes). Strip it off the ends only, never inside the token.
  function cleanToken(t) {
    return String(t || '')
      .replace(/^[\s\-–—*•·>#"'`([{]+/, '')
      .replace(/^\d+[.)]\s*/, '')
      .replace(/[\s,;:."'`)\]}]+$/, '')
      .trim();
  }
  function pasteTokens() {
    var q = (qEl.value || '').trim();
    if (!q || !/[\s,;]/.test(q)) return null;
    var toks = q.split(/[\s,;]+/).map(cleanToken).filter(Boolean);
    return toks.length ? toks : null;
  }
  function renderDevs() {
    var rows = devCache[devKey()] || [];
    var picked = {};
    S.targets.forEach(function (t) { if (t.kind === 'device') picked[t.serial] = true; });
    rows.forEach(function (d) { if (!picked[d.serial] && coveredBy(d.serial)) picked[d.serial] = 'via'; });
    var shown = rows.filter(function (d) {
      // Devices already covered by a chosen scope leave the list: the scope row
      // in the Selected well stands for them.
      if (coveredBy(d.serial)) return false;
      if (QF.online && !d.online) return false;
      if (QF.offline && d.online) return false;
      return true;
    });
    // Picked devices stay where they are (checked) and are listed again in the
    // Selected pane above; moving them to the top made the list jump on click.
    shown = shown.slice(0, 60);
    var html = '';
    var toks = pasteTokens();
    if (toks) {
      html += '<button type="button" class="co-paste" id="co-addpaste"><svg viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg>Add ' + toks.length + ' pasted serials</button>';
    }
    shown.forEach(function (d) {
      var via = coveredBy(d.serial);
      var on = !!picked[d.serial];
      html += '<button type="button" class="co-dev' + (on ? ' on' : '') + (via ? ' via' : '') + '" data-serial="' + esc(d.serial) + '"' + (via ? ' title="Included via ' + esc(via) + '"' : '') + '>'
        + '<span class="cb"><svg viewBox="0 0 24 24"><path d="m5 13 4 4L19 7"/></svg></span>'
        + '<span class="bd"><span class="sn">' + esc(d.serial) + '</span>' + (d.product || d.sub || d.bat ? '<span class="mt">' + esc([d.dpc && d.model ? d.model : d.product, d.sub, d.bat].filter(Boolean).join(' · ')) + '</span>' : '') + '</span>'
        + '<span class="st ' + (d.online ? 'on' : 'off') + '"></span></button>';
    });
    if (!shown.length && !toks) {
      var q = (qEl.value || '').trim();
      if (devLoading) html += '<div class="co-listempty">Searching devices…</div>';
      else if (isSel('all')) html += '<div class="co-listempty">All devices are selected.</div>';
      else if (rows.length && S.targets.length) html += '<div class="co-listempty">Every matching device is already covered by the selected scopes.</div>';
      else if (q) html += '<button type="button" class="co-paste" id="co-addpaste" data-one="1"><svg viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg>Add serial &ldquo;' + esc(q) + '&rdquo;</button><div class="co-listempty">No device matches this search — Enter checks it against the fleet.</div>';
      else html += '<div class="co-listempty">No devices match these filters.</div>';
    }
    el('co-devs').innerHTML = html;
  }

  // ── Selection mutations (same semantics + hints as the palette) ──
  function hasKind(k) { return S.targets.some(function (t) { return t.kind === k; }); }
  // Scopes and devices combine freely: any number of restaurants, groups and
  // picked devices. "All devices" is the one exclusive choice. The server unions
  // the selection and counts each device once (target_type=mixed).
  function toggleScope(kind, id) {
    disarm();
    if (kind === 'all') {
      S.targets = isSel('all') ? [] : [{ kind: 'all' }];
    } else if (isSel(kind, id)) {
      S.targets = S.targets.filter(function (t) { return !(t.kind === kind && t.id === id); });
    } else {
      S.targets = S.targets.filter(function (t) { return t.kind !== 'all'; });
      if (kind === 'restaurant') {
        var r = DS.restaurants.find(function (x) { return x.id === id; });
        S.targets.push({ kind: 'restaurant', id: id, name: r ? r.name : 'Restaurant', count: r ? r.count : null });
      } else {
        var g = DS.groups.find(function (x) { return x.id === id; });
        S.targets.push({ kind: 'group', id: id, name: g ? g.name : 'Group', count: g ? g.count : null });
      }
      loadScopeMembers({ kind: kind, id: id });
    }
    updateUI();
  }
  function toggleDevice(serial) {
    disarm();
    if (coveredBy(serial)) { toast('Already included via ' + coveredBy(serial), 'info'); return; }
    if (S.targets.some(function (t) { return t.kind === 'device' && t.serial === serial; })) {
      S.targets = S.targets.filter(function (t) { return !(t.kind === 'device' && t.serial === serial); });
    } else {
      S.targets = S.targets.filter(function (t) { return t.kind !== 'all'; });
      S.targets.push({ kind: 'device', serial: serial });
    }
    updateUI();
  }
  // Which devices each chosen scope contains, so the device list can show them
  // as included (checked, "via Harbor Grill") and duplicates don't double up.
  var scopeMembers = {};
  function loadScopeMembers(t) {
    var key = t.kind + ':' + t.id;
    if (scopeMembers[key]) return Promise.resolve(scopeMembers[key]);
    return fetchScopeRows(t).then(function (rows) {
      var set = {}; rows.forEach(function (r) { set[r.serial] = 1; });
      scopeMembers[key] = set; renderDevs(); return set;
    }).catch(function () { return {}; });
  }
  function coveredBy(serial) {
    for (var i = 0; i < S.targets.length; i++) {
      var t = S.targets[i];
      if (t.kind === 'all') return 'All devices';
      if (t.kind !== 'restaurant' && t.kind !== 'group') continue;
      var set = scopeMembers[t.kind + ':' + t.id];
      if (set && set[serial]) return t.name;
    }
    return '';
  }
  function addSerials(raw) {
    S.targets = S.targets.filter(function (t) { return t.kind !== 'all'; });
    raw.split(/[\s,;]+/).forEach(function (s) {
      s = cleanToken(s);
      if (s && !S.targets.some(function (t) { return t.serial === s; })) S.targets.push({ kind: 'device', serial: s });
    });
  }
  // Paste-a-list: each pasted serial is checked against the fleet first. Matches are
  // added as individual device targets (canonical serial from the server); the rest
  // are shown in a popup instead of riding along and being dropped silently at send.
  var resolving = false;
  function addSerialsValidated(raw) {
    if (resolving) return;
    var toks = raw.split(/[\s,;]+/).map(cleanToken).filter(Boolean);
    if (!toks.length) return;
    resolving = true;
    fetch('/commands/resolve-serials?serials=' + encodeURIComponent(toks.join(',')))
      .then(function (r) { return r.json(); })
      .then(function (res) {
        resolving = false;
        var found = res.found || [], missing = res.missing || [];
        if (found.length) {
          disarm();
          if (hasKind('group') || hasKind('all') || hasKind('restaurant')) S.targets = [];
          addSerials(found.join(' '));
        }
        qEl.value = ''; ensureDevices(true); updateUI();
        if (missing.length) showMissingSerials(missing, found.length);
        else if (found.length) toast('Added ' + found.length + ' device' + (found.length === 1 ? '' : 's'), 'success');
      })
      .catch(function () { resolving = false; toast('Could not check the pasted serials — try again', 'error'); });
  }
  function showMissingSerials(missing, added) {
    var m = el('co-missing-modal'); if (!m) return;
    el('co-missing-sum').textContent = added
      ? 'Added ' + added + ' matching device' + (added === 1 ? '' : 's') + '. ' + missing.length + ' serial' + (missing.length === 1 ? ' is not in the fleet and was' : 's are not in the fleet and were') + ' left out:'
      : 'None of the ' + missing.length + ' pasted serial' + (missing.length === 1 ? '' : 's') + ' match a device in the fleet:';
    el('co-missing-list').innerHTML = missing.map(function (s) { return '<li>' + esc(s) + '</li>'; }).join('');
    m.classList.add('modal-open');
  }
  function closeMissingModal(e) {
    var m = el('co-missing-modal');
    if (m && (!e || e.target === m)) m.classList.remove('modal-open');
  }
  window.closeMissingModal = closeMissingModal;

  // ── Selected pane: the chosen scopes and devices, above the lists ──
  function renderSelected() {
    var html = '';
    S.targets.forEach(function (t) {
      if (t.kind === 'device') {
        var d = devMeta[t.serial] || {};
        html += '<button type="button" class="co-dev on" data-serial="' + esc(t.serial) + '" title="Remove">'
          + '<span class="cb"><svg viewBox="0 0 24 24"><path d="m5 13 4 4L19 7"/></svg></span>'
          + '<span class="bd"><span class="sn">' + esc(t.serial) + '</span>' + (d.product || d.sub ? '<span class="mt">' + esc([d.dpc && d.model ? d.model : d.product, d.sub, d.bat].filter(Boolean).join(' · ')) + '</span>' : '') + '</span>'
          + '<span class="st ' + (d.online ? 'on' : 'off') + '"></span></button>';
      } else {
        var icon = t.kind === 'all' ? 'all' : t.kind === 'restaurant' ? 'place' : 'group';
        html += '<button type="button" class="co-scope on" data-kind="' + t.kind + '" data-id="' + esc(t.id || '') + '" title="Remove">'
          + '<span class="ic">' + svg(icon) + '</span>'
          + '<span class="bd"><span class="n">' + esc(targetLabel(t).replace(/ ×\d+$/, '')) + '</span><span class="m">' + (t.kind === 'all' ? 'the whole fleet' : '<span class="kt">' + t.kind + '</span>' + (t.count != null ? t.count + ' device' + (t.count === 1 ? '' : 's') : '')) + '</span></span>'
          + '<span class="add">Remove</span></button>';
      }
    });
    if (html) html = '<div class="co-sel-top"><span>' + S.targets.length + ' selected</span><span><button type="button" class="co-bk-clear" data-copy-serials="#co-sel" title="Copy the selected serials, one per line"><span class="cs-lbl">Copy</span></button> <button type="button" class="co-bk-clear" id="co-sel-clear">Clear</button></span></div>' + html;
    el('co-sel').innerHTML = html || '<div class="co-what-empty" id="co-what-empty">Nothing selected yet. Pick restaurants, groups or devices on the left and they collect here.</div>';
  }
  // ── Basket ──
  function targetLabel(t) {
    if (t.kind === 'all') return 'All devices';
    if (t.count != null && t.kind !== 'device') return t.name + ' ×' + t.count;
    return t.name || t.serial;
  }
  function renderBasket() {
    el('co-count').textContent = S.targets.length ? lastImpact.effective : 0;
    el('co-count-t').textContent = 'device' + (lastImpact.effective === 1 && S.targets.length ? '' : 's') + ' selected';
    var pills = '';
    S.targets.forEach(function (t) {
      pills += '<span class="co-bk-pill' + (t.kind === 'device' ? ' mono' : '') + '"><span class="lbl">' + esc(targetLabel(t)) + '</span>'
        + '<button type="button" class="x" data-rm="' + esc(selKey(t)) + '" title="Remove" aria-label="Remove ' + esc(targetLabel(t)) + '">&times;</button></span>';
    });
    el('co-pills').innerHTML = pills || '<span class="co-listempty" style="padding:2px 0 0">Nothing selected yet.</span>';
    // Refine chips only make sense for a scope selection (all / restaurant / one group).
    var scoped = el('f-ttype').value === 'scope';
    var ref = el('co-refine');
    ref.hidden = !scoped;
    if (scoped) {
      ref.innerHTML = refBtn('status', 'online', 'Online only') + refBtn('battery', 'low', 'Low battery') + refBtn('kiosk', 'enabled', 'Locked');
    }
    renderMix();
  }
  function refBtn(k, v, label) {
    var on = S.refine[k] === v;
    return '<button type="button" class="co-ref' + (on ? ' on' : '') + '" data-k="' + k + '" data-v="' + v + '" aria-pressed="' + on + '">' + label + '</button>';
  }
  function renderMix() {
    var m = el('co-mix');
    if (!S.targets.length) { m.innerHTML = ''; el('co-warn').hidden = true; return; }
    var k = lastImpact.kinds || {};
    var kindBits = KINDS.filter(function (x) { return (k[x[0]] || 0) > 0; }).map(function (x) {
      return '<span><span class="dot kd-' + x[0] + '"></span><b>' + k[x[0]] + '</b> ' + esc(x[2].replace('firmware client', 'firmware')) + '</span>';
    }).join('');
    m.innerHTML =
      '<span><span class="dot" style="background:var(--ok)"></span><b>' + lastImpact.online + '</b> online</span>'
      + '<span><span class="dot" style="background:var(--rule)"></span><b>' + lastImpact.offline + '</b> queued</span>'
      + kindBits;
    el('co-warn').hidden = !lastWarn;
    if (lastWarn) el('co-warn-t').textContent = lastWarn;
  }

  // ── Action grid ──
  // ── Agent badges ──
  // FW / DPC / LT on every card, lit for the agents that can run the action. What a
  // kind can do is read off the live selection when devices of that kind are in it —
  // the standard client's powers depend on which adb grants are in place, so a fixed table
  // would be wrong — and falls back to KIND_FALLBACK when none is selected.
  var KINDS = [['fw', 'FW', 'Firmware MDM'], ['dpc', 'STD', 'Standard MDM'], ['lite', 'LT', 'MDM Lite']];
  var KIND_FALLBACK = {
    install_apk: { fw: 1, dpc: 1, lite: 0 }, uninstall: { fw: 1, dpc: 1, lite: 0 },
    reboot: { fw: 1, dpc: 1, lite: 0 }, screenshot: { fw: 1, dpc: 1, lite: 1 },
    query: { fw: 1, dpc: 1, lite: 0 }, shell: { fw: 1, dpc: 1, lite: 0 },
    set_kiosk: { fw: 1, dpc: 1, lite: 0 }, update_splash: { fw: 1, dpc: 0, lite: 0 },
    wipe: { fw: 0, dpc: 1, lite: 0 }, unenroll: { fw: 0, dpc: 1, lite: 0 }
  };
  function kindRuns(t, k) {
    var sup = lastImpact.kindSup && lastImpact.kindSup[t];
    var have = (lastImpact.kinds || {})[k] || 0;
    if (sup && have) return sup[k] > 0;           // measured against what is selected
    var fb = KIND_FALLBACK[t];                    // nothing of this kind selected
    return fb ? !!fb[k] : true;
  }
  function kindBadges(t) {
    return '<span class="kd">' + KINDS.map(function (k) {
      var runs = kindRuns(t, k[0]);
      // MDM Lite runs a handful of actions; a dead LT pill on every other card is
      // noise, so it is left off entirely rather than shown struck through.
      if (!runs && k[0] === 'lite') return '';
      return '<i class="' + (runs ? k[0] : 'no') + '" title="' + esc(k[2]) + (runs ? ' runs this' : ' cannot run this') + '">' + k[1] + '</i>';
    }).join('') + '</span>';
  }

  function actionCard(a) {
    var n = incompatCount(a);
    var eff = lastImpact.total || lastImpact.effective;
    var none = eff > 0 && n >= eff;
    var on = S.action && S.action.t === a.t;
    var danger = a.t === 'wipe';
    var cls = 'co-act' + (on ? ' on' : '') + (n > 0 && !none ? ' dim' : '') + (none ? ' off' : '') + (danger ? ' danger' : '');
    // The count replaces the old sentence: the grid heading already says how much of
    // the selection an action covers, and the names of the devices it misses are in
    // the skip list under the selected card, where they can be acted on.
    var tag = (n > 0 && !none && eff > 0) ? '<span class="captag">' + (eff - n) + ' of ' + eff + '</span>' : '';
    return '<button type="button" class="' + cls + '" data-t="' + a.t + '">'
      + '<span class="ic">' + svg(a.t) + '</span>'
      + '<span class="nm">' + esc(a.n) + '</span><span class="ds">' + esc(a.d) + '</span>' + tag + kindBadges(a.t) + '</button>';
  }

  // The devices the selected action will not reach, by name. The old page said only
  // "4 of 7 can't run this" and dropped them silently at send; with three agents in
  // one fleet that is most mixed selections, so it is worth the row.
  function skipPanel() {
    if (!S.action) return '';
    var rows = lastImpact.skip || [];
    if (!rows.length) return '';
    var byKind = {};
    rows.forEach(function (r) { (byKind[r.k] = byKind[r.k] || []).push(r.s); });
    var parts = KINDS.filter(function (k) { return byKind[k[0]]; }).map(function (k) {
      var list = byKind[k[0]];
      var shown = list.slice(0, 6).map(function (x) { return '<span class="k">' + esc(x) + '</span>'; }).join(' ');
      if (list.length > 6) shown += ' <span class="snote">+' + (list.length - 6) + ' more</span>';
      return '<div class="skip-l"><b>' + list.length + ' on ' + esc(k[2]) + '</b> ' + shown + '</div>';
    }).join('');
    return '<div class="skip"><b>' + rows.length + ' device' + (rows.length === 1 ? '' : 's') + ' will be skipped</b>'
      + ' — their agent cannot run ' + esc(S.action.n.toLowerCase()) + ', so nothing is queued for them.' + parts + '</div>';
  }

  function coverageGroups() {
    var eff = lastImpact.total || lastImpact.effective;
    var all = [], some = [], none = [];
    DS.actions.forEach(function (a) {
      if (GRID_HIDDEN[a.t]) return;
      var n = incompatCount(a);
      if (!eff || n === 0) all.push(a);
      else if (n >= eff) none.push(a);
      else some.push(a);
    });
    return [all, some, none];
  }

  function renderGrid() {
    // Nothing selected yet: coverage is unknowable, so keep the familiar
    // by-purpose grid rather than claiming everything "runs on all 0".
    if (!lastImpact.total && !lastImpact.effective) { renderGridByPurpose(); return; }
    var g = coverageGroups(), eff = lastImpact.total || lastImpact.effective;
    var html = '';
    if (g[0].length) {
      html += cap('Runs on all ' + eff, 100, false) + '<div class="co-agrid">' + g[0].map(actionCard).join('') + '</div>';
    }
    if (g[1].length) {
      html += cap('Runs on some', Math.round(100 * (eff - incompatCount(g[1][0])) / eff), true)
        + '<div class="co-agrid">' + g[1].map(actionCard).join('') + '</div>';
      if (S.action && incompatCount(S.action) > 0 && incompatCount(S.action) < eff) html += skipPanel();
    }
    if (g[2].length) {
      html += '<div class="co-gcap">Runs on none of this selection · ' + g[2].length + '<span class="ln"></span></div>'
        + '<div class="co-agrid">' + g[2].map(actionCard).join('') + '</div>';
    }
    el('co-grid').innerHTML = html;
  }
  function cap(label, pct, part) {
    return '<div class="co-gcap">' + esc(label)
      + ' <span class="cov"><span class="cbar' + (part ? ' part' : '') + '"><i style="width:' + pct + '%"></i></span></span><span class="ln"></span></div>';
  }
  function renderGridByPurpose() {
    var byType = {};
    DS.actions.forEach(function (a) { byType[a.t] = a; });
    var placed = {};
    var html = '';
    GRID_GROUPS.forEach(function (g) {
      var acts = g[1].filter(function (t) { placed[t] = true; return byType[t]; });
      if (!acts.length) return;
      html += '<div class="co-gcap' + (g[0] === 'Danger zone' ? ' danger' : '') + '">' + g[0] + '<span class="ln"></span></div><div class="co-agrid">'
        + acts.map(function (t) { return actionCard(byType[t]); }).join('') + '</div>';
    });
    var extra = DS.actions.filter(function (a) { return !placed[a.t] && !GRID_HIDDEN[a.t]; });
    if (extra.length) html += '<div class="co-gcap">Other<span class="ln"></span></div><div class="co-agrid">' + extra.map(actionCard).join('') + '</div>';
    el('co-grid').innerHTML = html;
  }

  // ── Payload strip ──
  function verbFor(a, n) {
    switch (a.t) {
      case 'install_apk': return 'push to ' + n + ' device' + (n === 1 ? '' : 's');
      case 'app_update': return 'update the app on ' + n + ' device' + (n === 1 ? '' : 's');
      case 'uninstall': return 'remove from ' + n + ' device' + (n === 1 ? '' : 's');
      case 'reboot': return 'restart ' + n + ' device' + (n === 1 ? '' : 's');
      case 'set_kiosk': return 'kiosk config on ' + n + ' device' + (n === 1 ? '' : 's');
      case 'screenshot': return 'capture ' + n + ' screen' + (n === 1 ? '' : 's');
      case 'shell': return 'run on ' + n + ' device' + (n === 1 ? '' : 's');
      case 'query': return 'query ' + n + ' device' + (n === 1 ? '' : 's');
      case 'collect_logs': return 'collect logs from ' + n + ' device' + (n === 1 ? '' : 's');
      case 'update_splash': return 'reflash ' + n + ' splash image' + (n === 1 ? '' : 's');
      case 'wipe': return 'ERASE ' + n + ' device' + (n === 1 ? '' : 's') + ' completely';
    }
    return a.d;
  }
  function chip(lbl, rmKind, rmKey) {
    return '<span class="co-chip"><span class="lbl">' + esc(lbl) + '</span><button type="button" class="x" data-chip-rm="' + rmKind + '" data-key="' + esc(rmKey || '') + '" aria-label="Remove ' + esc(lbl) + '">&times;</button></span>';
  }
  function renderStrip() {
    stripEl.hidden = false;
    stripEl.classList.toggle('empty', !S.action);
    if (!S.action) {
      el('co-st-ic').innerHTML = svg('dot');
      el('co-st-t').textContent = 'No action yet';
      el('co-st-s').textContent = 'pick one from the tiles below — it lands here with its options and Send';
      el('co-pay').innerHTML = ''; el('co-note').textContent = '';
      closeMenu(); return;
    }
    el('co-st-ic').innerHTML = svg(S.action.t);
    el('co-st-t').textContent = S.action.n;
    renderPay();
    var rs = el('co-reason');
    rs.style.display = (S.action.destr && DS.requireReason) ? '' : 'none';
    rs.placeholder = 'Reason — recorded in the audit log' + (DS.requireReason ? ' (required)' : '');
    if (rs.value !== S.reason) rs.value = S.reason;
    updateArmedUI();
    updateStripMeta();
  }
  function renderPay() {
    var pay = el('co-pay');
    if (!S.action) { pay.innerHTML = ''; return; }
    var html = '';
    switch (S.action.p) {
      case 'apps':
        S.apps.forEach(function (a) { html += chip(a.name, 'app', a.url); });
        html += '<button type="button" class="co-addbtn" data-menu="apps"><svg viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg>' + (S.apps.length ? 'Add another app' : 'Pick apps from the library') + '</button>';
        break;
      case 'pkgs':
        S.pkgs.forEach(function (p) { html += chip(p.name || p.pkg, 'pkg', p.pkg); });
        html += '<button type="button" class="co-addbtn" data-menu="pkgs"><svg viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg>' + (S.pkgs.length ? 'Add another package' : 'Pick installed packages') + '</button>';
        break;
      case 'shell':
        html += '<input class="co-in mono" id="co-shell" placeholder="Shell command — e.g. df -h" value="' + esc(S.shell) + '">';
        break;
      case 'query':
        if (S.query) html += chip(S.query.label, 'query', String(S.query.id));
        html += '<button type="button" class="co-addbtn" data-menu="query"><svg viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg>' + (S.query ? 'Change query' : 'Pick a vetted query') + '</button>';
        break;
      case 'kiosk':
        var lockOn = S.kiosk && S.kiosk.enabled === '1';
        var unlockOn = S.kiosk && S.kiosk.enabled === '0';
        html += '<div class="co-seg"><button type="button" data-kiosk="lock" class="' + (lockOn ? 'on' : '') + '">Lock to an app</button><button type="button" data-kiosk="unlock" class="' + (unlockOn ? 'on' : '') + '">Unlock</button></div>';
        if (lockOn) {
          html += S.kiosk.pkg ? chip('Lock to ' + (S.kiosk.name || S.kiosk.pkg), 'kioskpkg', '')
            : '<button type="button" class="co-addbtn" data-menu="kioskpkg"><svg viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg>Choose the pinned app</button>';
        }
        if (unlockOn) html += '<span class="co-paynote">exit kiosk — home, recents and status bar return</span>';
        break;
      case 'splash':
        html += '<input class="co-in mono" id="co-splash" placeholder="Image URL — BMP/PNG/JPEG, wrapped into splash.img server-side" value="' + esc(S.splash) + '">';
        break;
      default:
        html += '<span class="co-paynote">No parameters — review the counts and send.</span>';
    }
    pay.innerHTML = html;
    var sh = el('co-shell');
    if (sh) { sh.addEventListener('input', function () { S.shell = sh.value; syncFormQuiet(); }); sh.addEventListener('focus', function () { openMenu('shell', sh); }); sh.addEventListener('input', function () { if (menu.kind === 'shell') renderMenu(); }); bindEnter(sh); }
    var sp = el('co-splash');
    if (sp) { sp.addEventListener('input', function () { S.splash = sp.value; syncFormQuiet(); }); bindEnter(sp); }

  }
  // Payload keystrokes shouldn't re-render the strip (focus loss) — only sync + label.
  var quietTimer = null;
  function syncFormQuiet() { disarm(); clearTimeout(quietTimer); quietTimer = setTimeout(function () { syncForm(); updateStripMeta(); }, 250); }
  function bindEnter(inp) { inp.addEventListener('keydown', function (e) { if (e.key === 'Enter') { e.preventDefault(); closeMenu(); send(); } }); }

  function updateStripMeta() {
    if (!S.action) return;
    var n = lastImpact.effective;
    el('co-st-s').textContent = verbFor(S.action, n);
    updateSendLabel();
    updateNote();
  }
  function updateSendLabel() {
    var lbl = el('co-send-label'); if (!lbl) return;
    var btn = el('co-send');
    var n = lastImpact.effective;
    if (btn) btn.classList.toggle('arm', armed);
    if (armed) { lbl.textContent = S.action && S.action.t === 'wipe' ? 'Confirm wipe' : 'Confirm ×' + (n || '?'); return; }
    lbl.textContent = n > 0 ? 'Send to ' + n + ' device' + (n === 1 ? '' : 's') : 'Send';
  }
  function updateNote() {
    var note = el('co-note'); if (!note || !S.action) return;
    var n = incompatCount(S.action);
    if (n > 0) {
      var total = lastImpact.total || lastImpact.effective;
      var compat = total - n;
      note.innerHTML = '<b>' + n + ' of ' + total + '</b> ' + esc(incompatWhy(S.action, n).replace(/^\d+ of \d+ /, ''))
        + (compat > 0 && CAN ? ' <button type="button" class="co-subset" id="co-subset">Send to the ' + compat + ' compatible only</button>' : ' — nothing in the selection can run it.');
    } else if (S.targets.length && lastImpact.effective > 0) {
      note.innerHTML = esc(lastImpact.online + ' delivered live over WebSocket · ' + lastImpact.offline + ' queued for next check-in')
        + (S.targets.length && S.targets[0].kind === 'all' ? ' · targets the whole fleet' : '');
    } else {
      note.innerHTML = 'Pick devices on the left — the counts fill in live.';
    }
  }
  function updateArmedUI() {
    var w = el('co-wipe');
    var showWipe = armed && S.action && S.action.t === 'wipe';
    w.style.display = showWipe ? '' : 'none';
    if (!showWipe) w.value = '';
    updateSendLabel();
  }

  // ── Strip dropdown menu (apps / packages / locked app / shell suggestions) ──
  var menu = { kind: null, anchor: null };
  function closeMenu() { menu.kind = null; menu.anchor = null; menuEl.classList.remove('on'); }

  // ── App picker popup: one tile per app family, variant/version chooser under a
  // selected tile, multi-select, one Add. Built from DS.apps (ordered family,
  // prod first, newest first; each row carries family/variant/latest). ──
  // Selection is per variant, not per app: a family can have several variants chosen at once
  // (Nugget prod + internal + qa), each with its own version. AM.sel maps "family||variant"
  // to the chosen build's url; AM.ver remembers the version picked for a variant even while it
  // is unticked. A one-build app is a plain tile (one click selects it); anything with several
  // variants or versions opens a panel under its row of tiles.
  var AM = { fams: [], sel: {}, ver: {}, open: null, focus: null };
  function amKey(f, v) { return f.key + '||' + v; }
  function amFamilies() {
    var map = {}, out = [];
    DS.apps.forEach(function (a) {
      var key = a.family || a.name || a.pkg; var f = map[key];
      if (!f) { f = { key: key, name: a.family || a.name, icon: '', entries: [], variants: {}, order: [] }; map[key] = f; out.push(f); }
      if (!f.icon && a.icon) f.icon = a.icon;
      var v = a.variant || 'prod'; if (!f.variants[v]) { f.variants[v] = []; f.order.push(v); }
      f.variants[v].push(a); f.entries.push(a);
    });
    out.forEach(function (f) { f.def = (f.variants.prod && f.variants.prod[0]) || f.entries[0]; });
    return out;
  }
  function amLatest(list) { return list.filter(function (a) { return a.latest; })[0] || list[0]; }
  function amCurrent(f, v) {
    var k = amKey(f, v), list = f.variants[v], u = AM.ver[k];
    return (u && list.filter(function (a) { return a.url === u; })[0]) || amLatest(list);
  }
  window.openAppModal = function () {
    if (!CAN) return;
    AM.fams = amFamilies(); AM.sel = {}; AM.ver = {}; AM.open = null; AM.focus = null;
    AM.fams.forEach(function (f) { f.entries.forEach(function (a) { if (S.apps.some(function (x) { return x.url === a.url; })) { var k = amKey(f, a.variant || 'prod'); AM.sel[k] = a.url; AM.ver[k] = a.url; } }); });
    var q = el('cam-q'); if (q) q.value = '';
    el('co-app-modal').classList.add('modal-open');
    amRender(); if (q) setTimeout(function () { q.focus(); }, 30);
  };
  window.closeAppModal = function (e) { if (e && e.target && e.target !== el('co-app-modal')) return; el('co-app-modal').classList.remove('modal-open'); };
  function amPanel(f) {
    var p = document.createElement('div'); p.className = 'cam-panel'; p.setAttribute('role', 'group'); p.setAttribute('aria-label', f.name + ' variants');
    var ic = f.icon ? '<img src="data:image/png;base64,' + f.icon + '" alt="">' : '<span class="cam-mono">' + esc((f.name || '?').slice(0, 1)) + '</span>';
    var h = '<div class="cam-ph">' + ic + esc(f.name) + '<span class="hint">tick any variants · choose a version for each</span></div>';
    f.order.forEach(function (v) {
      var k = amKey(f, v), list = f.variants[v], cur = amCurrent(f, v), on = !!AM.sel[k];
      h += '<div class="cam-pr' + (on ? ' on' : '') + '" data-k="' + esc(k) + '"><label class="cam-pl"><input type="checkbox" class="cam-cb"' + (on ? ' checked' : '') + '><span class="cam-vn">' + esc(v) + '</span><span class="cam-pk">' + esc(cur.pkg || tail(cur.url)) + '</span>' + (cur.latest ? '<span class="cam-lat">latest</span>' : '') + '</label>';
      if (list.length > 1) {
        h += '<select class="form-input cam-pick" aria-label="Version of ' + esc(v) + '">';
        list.forEach(function (a) { h += '<option value="' + esc(a.url) + '"' + (a.url === cur.url ? ' selected' : '') + '>' + esc(a.ver ? 'v' + a.ver : 'unversioned') + (a.latest ? ' (latest)' : '') + '</option>'; });
        h += '</select>';
      } else {
        h += '<span class="cam-one">' + esc(cur.ver ? 'v' + cur.ver : 'unversioned') + '</span>';
      }
      h += '</div>';
    });
    p.innerHTML = h;
    f.order.forEach(function (v) {
      var k = amKey(f, v), row = p.querySelector('.cam-pr[data-k="' + k.replace(/"/g, '\\"') + '"]');
      row.querySelector('.cam-cb').addEventListener('change', function () {
        if (AM.sel[k]) delete AM.sel[k]; else AM.sel[k] = amCurrent(f, v).url;
        AM.focus = { k: k, kind: 'input' }; amRender();
      });
      var sl = row.querySelector('.cam-pick');
      if (sl) sl.addEventListener('change', function () { AM.ver[k] = sl.value; if (AM.sel[k]) AM.sel[k] = sl.value; AM.focus = { k: k, kind: 'select' }; amRender(); });
    });
    return p;
  }
  function amRender() {
    var q = (el('cam-q').value || '').toLowerCase(), grid = el('cam-grid'), shown = [];
    grid.innerHTML = '';
    AM.fams.forEach(function (f) {
      var hay = (f.name + ' ' + f.entries.map(function (a) { return a.pkg + ' ' + (a.variant || ''); }).join(' ')).toLowerCase();
      if (!q || hay.indexOf(q) >= 0) shown.push(f);
    });
    // Tiles per row, so the panel can open under the clicked tile's whole row.
    var cols = Math.max(1, Math.floor((grid.clientWidth + 8) / 140));
    var tiles = [], openAt = -1;
    shown.forEach(function (f, i) {
      var nv = f.order.length, ne = f.entries.length, grouped = nv > 1 || ne > 1;
      var nsel = f.order.filter(function (v) { return AM.sel[amKey(f, v)]; }).length, on = nsel > 0, isOpen = AM.open === f.key;
      var pick = grouped ? amCurrent(f, f.order.filter(function (v) { return AM.sel[amKey(f, v)]; })[0] || (f.variants.prod ? 'prod' : f.order[0])) : f.def;
      var w = document.createElement('div'); w.className = 'cam-tile' + (on ? ' on' : '') + (grouped ? ' is-family' : '') + (isOpen ? ' open' : '');
      var ic = f.icon ? '<img src="data:image/png;base64,' + f.icon + '" alt="">' : '<span class="cam-mono">' + esc((f.name || '?').slice(0, 1)) + '</span>';
      var sub = grouped
        ? (on ? nsel + ' of ' + nv + ' variant' + (nv === 1 ? '' : 's') + ' chosen' : (nv > 1 ? nv + ' variants' : '') + (nv > 1 && ne > 1 ? ' · ' : '') + (ne > 1 ? ne + ' versions' : ''))
        : (pick.variant && pick.variant !== 'prod' ? pick.variant + ' · ' : '') + (pick.ver ? 'v' + pick.ver : '');
      var tick = nsel > 1 ? '<span class="cam-tick cam-tickn">' + nsel + '</span>' : '<span class="cam-tick"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3"><path d="M5 13l4 4L19 7"/></svg></span>';
      w.innerHTML = '<button type="button" class="cam-btn"' + (grouped ? ' aria-expanded="' + isOpen + '"' : '') + '>' + ic + '<span class="cam-name">' + esc(f.name) + '</span><span class="cam-ver">' + esc(sub) + '</span>' + (grouped ? '<span class="cam-caret" aria-hidden="true">' + (isOpen ? '▴' : '▾') + '</span>' : '') + tick + '</button>';
      w.querySelector('.cam-btn').addEventListener('click', function () {
        if (grouped) { AM.open = isOpen ? null : f.key; }
        else { var k = amKey(f, f.def.variant || 'prod'); if (AM.sel[k]) delete AM.sel[k]; else AM.sel[k] = f.def.url; }
        amRender();
      });
      tiles.push(w);
      if (isOpen) openAt = i;
    });
    var panelAfter = openAt < 0 ? -1 : Math.min(tiles.length - 1, (Math.floor(openAt / cols) + 1) * cols - 1);
    tiles.forEach(function (w, i) {
      grid.appendChild(w);
      if (i === panelAfter) grid.appendChild(amPanel(shown[openAt]));
    });
    var n = Object.keys(AM.sel).length;
    el('cam-count').textContent = n ? n + ' selected' : 'Pick one or more apps';
    el('cam-add').disabled = !n; el('cam-add').textContent = n ? 'Add ' + n + ' app' + (n === 1 ? '' : 's') : 'Add';
    el('cam-empty').hidden = shown.length > 0;
    if (AM.focus) {
      var row = grid.querySelector('.cam-pr[data-k="' + AM.focus.k.replace(/"/g, '\\"') + '"]'), tgt = row && row.querySelector(AM.focus.kind === 'select' ? 'select' : 'input');
      if (tgt) tgt.focus();
      AM.focus = null;
    }
  }
  window.addEventListener('resize', function () { if (el('co-app-modal').classList.contains('modal-open')) amRender(); });
  window.amApply = function () {
    var picked = [];
    AM.fams.forEach(function (f) {
      f.order.forEach(function (v) {
        var u = AM.sel[amKey(f, v)]; if (!u) return;
        var a = f.entries.filter(function (x) { return x.url === u; })[0];
        if (a) picked.push({ url: a.url, name: f.name + (a.variant && a.variant !== 'prod' ? ' · ' + a.variant : '') + (a.ver ? ' v' + a.ver : '') });
      });
    });
    S.apps = picked; updateUI(); el('co-app-modal').classList.remove('modal-open');
  };
  window.amSearch = function () { amRender(); };
  function openMenu(kind, anchor) {
    if (!CAN) return;
    menu.kind = kind; menu.anchor = anchor || null;
    if (kind === 'pkgs') ensurePkgs();
    renderMenu();
  }
  function positionMenu() {
    var a = menu.anchor;
    if (!a) { menuEl.style.left = '16px'; menuEl.style.top = '100%'; return; }
    var sr = stripEl.getBoundingClientRect(), ar = a.getBoundingClientRect();
    var left = Math.max(4, Math.min(ar.left - sr.left, sr.width - 404));
    menuEl.style.left = left + 'px';
    menuEl.style.top = (ar.bottom - sr.top + 6) + 'px';
  }
  function optHtml(o) {
    var icon = o.img ? '<img src="' + o.img + '" alt="">' : svg(o.icon || S.action.t);
    return '<button type="button" class="co-opt" data-oi="' + o.i + '">'
      + '<span class="ic">' + icon + '</span>'
      + '<span class="tx"><span class="n' + (o.mono ? ' mono' : '') + '">' + esc(o.title) + '</span>' + (o.meta ? '<span class="m">' + esc(o.meta) + '</span>' : '') + '</span>'
      + (o.sel ? '<span class="k">added</span>' : '') + '</button>';
  }
  var menuOpts = [];
  function match(hay, q) { return !q || hay.toLowerCase().indexOf(q.toLowerCase()) >= 0; }
  function buildMenuOpts(q) {
    var opts = [], empty = 'Nothing here.';
    if (menu.kind === 'apps') {
      DS.apps.forEach(function (a) {
        if (!match(a.name + ' ' + (a.family || '') + ' ' + (a.variant || '') + ' ' + (a.pkg || ''), q)) return;
        // Older versions stay out of the list until searched for, so the catalogue
        // shows one row per variant.
        if (!a.latest && !q) return;
        var sel = S.apps.some(function (x) { return x.url === a.url; });
        var title = a.family && a.family !== a.name ? a.family + ' · ' + (a.variant || 'prod') : (a.name + (a.variant && a.variant !== 'prod' ? ' · ' + a.variant : ''));
        title += (a.ver ? ' v' + a.ver : '') + (a.latest ? '' : ' (older)');
        opts.push({ img: a.icon ? 'data:image/png;base64,' + a.icon : '', title: title, meta: a.pkg || tail(a.url), sel: sel,
          pick: function () { if (sel) S.apps = S.apps.filter(function (x) { return x.url !== a.url; }); else S.apps.push({ url: a.url, name: a.name }); } });
      });
      empty = DS.apps.length ? 'No app matches.' : 'No apps in the library yet — add one in Settings → App library.';
    } else if (menu.kind === 'pkgs') {
      if (pkgsLoading) { empty = 'Loading apps installed on the target…'; }
      (pkgsList || []).forEach(function (p) {
        if (!match(p.name + ' ' + p.pkg, q)) return;
        var sel = S.pkgs.some(function (x) { return x.pkg === p.pkg; });
        opts.push({ img: p.img || '', title: p.name, meta: p.pkg + (p.count ? ' · ' + p.count : ''), sel: sel, icon: 'uninstall',
          pick: function () { if (sel) S.pkgs = S.pkgs.filter(function (x) { return x.pkg !== p.pkg; }); else S.pkgs.push({ pkg: p.pkg, name: p.name }); } });
      });
      if (!pkgsLoading && !(pkgsList || []).length) empty = S.targets.length ? 'No uninstallable apps reported on the selection.' : 'Pick a target first — the list shows what is actually installed there.';
    } else if (menu.kind === 'query') {
      DS.queries.forEach(function (x) {
        if (!match(x.label + ' ' + (x.cat || ''), q)) return;
        var sel = !!(S.query && S.query.id === x.id);
        opts.push({ icon: 'query', title: x.label, meta: x.cat || 'vetted read-only diagnostic', sel: sel,
          pick: function () { S.query = { id: x.id, label: x.label }; } });
      });
      empty = DS.queries.length ? 'No query matches.' : 'No vetted queries defined — add them in Settings.';
    } else if (menu.kind === 'kioskpkg') {
      DS.packages.forEach(function (p) {
        if (!match(p.name + ' ' + p.pkg, q)) return;
        if (opts.length >= 40) return;
        opts.push({ img: p.icon ? 'data:image/png;base64,' + p.icon : '', title: p.name, meta: p.pkg + ' · ' + p.count + ' device' + (p.count === 1 ? '' : 's'), icon: 'set_kiosk',
          pick: function () { S.kiosk = { enabled: '1', pkg: p.pkg, name: p.name }; } });
      });
      empty = q ? 'No app matches — it must be installed somewhere on the fleet.' : 'No packages reported yet.';
    } else if (menu.kind === 'shell') {
      DS.shellRecent.forEach(function (c) { if (match(c, q)) opts.push({ icon: 'shell', title: c, mono: true, meta: 'recent', pick: function () { S.shell = c; var sh = el('co-shell'); if (sh) sh.value = c; } }); });
      DS.shellPopular.forEach(function (c) { if (match(c, q) && DS.shellRecent.indexOf(c) < 0) opts.push({ icon: 'shell', title: c, mono: true, meta: 'frequent', pick: function () { S.shell = c; var sh = el('co-shell'); if (sh) sh.value = c; } }); });
      empty = '';
    }
    opts.forEach(function (o, i) { o.i = i; });
    return { opts: opts, empty: empty };
  }
  function renderMenu() {
    if (!menu.kind) return;
    var withSearch = menu.kind !== 'shell';
    var prev = menuEl.querySelector('.msrch');
    var q = menu.kind === 'shell' ? (el('co-shell') ? el('co-shell').value.trim() : '') : (prev ? prev.value : '');
    var built = buildMenuOpts(q);
    menuOpts = built.opts;
    if (menu.kind === 'shell' && !menuOpts.length) { closeMenu(); return; }
    menuEl.innerHTML = (withSearch ? '<input class="msrch" type="text" placeholder="Search…" value="' + esc(q) + '">' : '')
      + '<div class="mlist">' + (menuOpts.length ? menuOpts.map(optHtml).join('') : '<div class="co-mempty">' + built.empty + '</div>') + '</div>';
    menuEl.classList.add('on');
    positionMenu();
    var srch = menuEl.querySelector('.msrch');
    if (srch) {
      srch.addEventListener('input', function () { renderMenuKeepFocus(); });
      if (prev) { srch.focus(); srch.setSelectionRange(srch.value.length, srch.value.length); }
    }
  }
  function renderMenuKeepFocus() {
    var srch = menuEl.querySelector('.msrch');
    var q = srch ? srch.value : '';
    var built = buildMenuOpts(q);
    menuOpts = built.opts;
    menuEl.querySelector('.mlist').innerHTML = menuOpts.length ? menuOpts.map(optHtml).join('') : '<div class="co-mempty">' + built.empty + '</div>';
  }

  // Uninstall packages, scoped to the current target via /commands/target-packages.
  var pkgsList = null, pkgsLoading = false, pkgsKey = '';
  function ensurePkgs() {
    var key = el('f-ttype').value + '|' + el('f-smode').value + '|' + el('f-sid').value + '|' + el('f-serials').value + '|' + JSON.stringify(S.refine);
    if (pkgsList && pkgsKey === key) return;
    pkgsKey = key; pkgsLoading = true; pkgsList = null;
    var p = new URLSearchParams();
    new FormData(form).forEach(function (v, k) { p.append(k, v); });
    fetch('/commands/target-packages?' + p.toString()).then(function (r) { return r.text(); }).then(function (html) {
      var doc = new DOMParser().parseFromString(html, 'text/html');
      var out = [];
      doc.querySelectorAll('.pkg').forEach(function (b) {
        var img = b.querySelector('.ic img');
        out.push({ pkg: b.getAttribute('data-pkg'), name: b.getAttribute('data-name') || b.getAttribute('data-pkg'),
                   count: (b.querySelector('.cnt') || { textContent: '' }).textContent.trim(), img: img ? img.getAttribute('src') : '' });
      });
      pkgsList = out; pkgsLoading = false;
      if (menu.kind === 'pkgs') renderMenuKeepFocus();
    }).catch(function () { pkgsLoading = false; pkgsList = []; });
  }

  // ── Subset to compatible: turn the selection into explicit serials that CAN run
  // the chosen action (resolving scopes via the browse partial). ──
  function fetchScopeRows(t) {
    var p = new URLSearchParams();
    if (t.kind === 'restaurant') p.set('restaurant', t.id);
    if (t.kind === 'group') p.set('group', t.id);
    if (el('f-ttype').value === 'scope') {
      if (S.refine.status) p.set('status', S.refine.status);
      if (S.refine.battery) p.set('battery', S.refine.battery);
      if (S.refine.kiosk) p.set('kiosk', S.refine.kiosk);
    }
    return fetch('/commands/browse-devices?' + p.toString()).then(function (r) { return r.text(); }).then(parseRows);
  }
  function subsetCompatible() {
    if (!S.action) return;
    var needDpc = S.action.cap === 'DPC only';
    var unsup = {};
    if (lastImpact.unsupSerials) lastImpact.unsupSerials.split(',').forEach(function (s) { if (s) unsup[s] = 1; });
    var haveUnsup = !!lastImpact.unsupSerials || lastImpact.unsupported === 0;
    var jobs = S.targets.map(function (t) {
      if (t.kind === 'device') {
        var m = devMeta[t.serial];
        // Unknown serials count as system-app clients (the server treats missing
        // agent_type the same way).
        return Promise.resolve([{ serial: t.serial, dpc: m ? m.dpc : false }]);
      }
      return fetchScopeRows(t);
    });
    Promise.all(jobs).then(function (lists) {
      var seen = {}, keep = [];
      lists.forEach(function (rows) {
        rows.forEach(function (r) {
          if (seen[r.serial]) return;
          seen[r.serial] = 1;
          noteMeta([r]);
          if (haveUnsup ? !unsup[r.serial] : (needDpc ? r.dpc : !r.dpc)) keep.push(r.serial);
        });
      });
      if (!keep.length) { toast('No compatible devices in the selection', 'error'); return; }
      S.targets = keep.map(function (s) { return { kind: 'device', serial: s }; });
      disarm(); updateUI();
      toast('Selection narrowed to ' + keep.length + ' compatible device' + (keep.length === 1 ? '' : 's'), 'success');
    }).catch(function () { toast('Could not resolve the selection', 'error'); });
  }

  // ── Send (same safety gates as before: two-step confirm for destructive /
  // fleet-wide, typed WIPE for wipe, reason blocking) ──
  function disarm() { if (!armed) return; armed = false; clearTimeout(armTimer); updateArmedUI(); updateNote(); }
  function send() {
    if (!CAN) return;
    var miss = firstMissing();
    if (miss) { setHint(miss.msg); if (miss.focus) { var f = el(miss.focus); if (f) f.focus(); } return; }
    closeMenu();
    var fleetWide = el('f-smode').value === 'all';
    var wipe = S.action.t === 'wipe';
    if ((S.action.destr || fleetWide) && !armed) {
      armed = true;
      updateArmedUI();
      var n = lastImpact.effective;
      setHint(wipe
        ? 'Factory wipe erases ' + (n || 'the targeted') + ' device(s) completely. Type WIPE in the box, then press Send again.'
        : (fleetWide ? 'This targets the whole fleet (' + n + ' devices, online and offline). ' : '') + 'Press Send again to confirm.');
      armTimer = setTimeout(disarm, 10000);
      if (wipe) el('co-wipe').focus();
      return;
    }
    if (wipe && el('co-wipe').value.trim() !== 'WIPE') { setHint('Type WIPE (all caps) in the box to confirm the factory wipe.'); el('co-wipe').focus(); return; }
    submitNow();
  }
  function submitNow() {
    var sendBtn = el('co-send');
    if (sendBtn) sendBtn.disabled = true;
    var body = new URLSearchParams();
    new FormData(form).forEach(function (v, k) { body.append(k, v); });
    fetch(form.action, { method: 'POST', body: body, headers: { 'Accept': 'text/html' } }).then(function (res) {
      if (sendBtn) sendBtn.disabled = false;
      armed = false; clearTimeout(armTimer); updateArmedUI();
      if (!res.ok) {
        return res.text().then(function (t) {
          var m = (t || 'Send failed').trim().split('\n')[0].slice(0, 200);
          setHint(m); toast(m, 'error');
        });
      }
      var flash = '', ftype = 'success', dest = null;
      try {
        var u = new URL(res.url, location.origin);
        flash = u.searchParams.get('flash') || ''; if (flash) ftype = u.searchParams.get('flash_type') || 'info';
        // One command (any number of targets) → the server redirects to its page.
        // fetch() followed that redirect silently; go there for real so the operator
        // lands on the live result instead of hunting for it in the feed.
        if (/^\/commands\/[0-9a-f-]{36}$/i.test(u.pathname)) dest = u.pathname + u.search;
      } catch (e) {}
      S.reason = ''; el('co-wipe').value = '';
      if (dest) { location.href = dest; return; }
      var adminHidden = document.getElementById('show-admin-actions') && !document.getElementById('show-admin-actions').checked;
      toast(flash || 'Action sent — tracking in Live activity below', ftype);
      if (adminHidden) toast('Your sends are hidden from Live activity — tick "Show admin actions" to see them', 'info');
      updateUI();
      document.body.dispatchEvent(new CustomEvent('commands-feed-update'));
    }).catch(function () {
      if (sendBtn) sendBtn.disabled = false; disarm();
      toast('Network error — action not sent', 'error');
    });
  }

  // ── Top-level render + wiring ──
  function updateUI() {
    syncForm();
    renderScopes();
    renderDevs();
    renderSelected();
    renderBasket();
    renderGrid();
    renderStrip();
  }
  function setAction(a) {
    var changed = !S.action || S.action.t !== a.t;
    S.action = a;
    if (changed) { S.apps = []; S.pkgs = []; S.shell = ''; S.query = null; S.kiosk = null; S.splash = ''; S.reason = ''; closeMenu(); }
    disarm();
  }

  if (CAN) {
    el('co-send') && el('co-send').addEventListener('click', function () { send(); });
    el('co-reason').addEventListener('input', function () { S.reason = el('co-reason').value; el('f-reason').value = S.reason.trim(); });
    bindEnter(el('co-reason'));
    bindEnter(el('co-wipe'));

    // Rail wiring.
    qEl.addEventListener('input', function () { renderScopes(); ensureDevices(false); });
    // Clipboard detection: pasting a list of serials adds them straight away. A
    // multi-word search ("flights vegas") must keep searching, so every token has to
    // look like a serial — long enough and carrying a digit.
    qEl.addEventListener('paste', function () {
      setTimeout(function () {
        var toks = pasteTokens();
        if (toks && toks.length > 1 && toks.every(function (t) { return t.length >= 8 && /\d/.test(t); })) {
          addSerialsValidated(qEl.value);
        } else {
          renderScopes(); ensureDevices(false);
        }
      }, 0);
    });
    qEl.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') { qEl.value = ''; renderScopes(); ensureDevices(true); qEl.blur(); }
      if (e.key === 'Enter') {
        e.preventDefault();
        var q = (qEl.value || '').trim();
        if (q) addSerialsValidated(q);
      }
    });
    el('co-quick').addEventListener('click', function (e) {
      var b = e.target.closest('.co-qf'); if (!b) return;
      var k = b.dataset.qf;
      QF[k] = !QF[k];
      if (k === 'online' && QF.online) QF.offline = false;
      if (k === 'offline' && QF.offline) QF.online = false;
      renderQuick();
      if (k === 'kiosk') ensureDevices(true); else renderDevs();
    });
    el('co-stabs').addEventListener('click', function (e) {
      var b = e.target.closest('.co-stab'); if (!b) return;
      scopeTab = b.dataset.stab;
      document.querySelectorAll('#co-stabs .co-stab').forEach(function (x) { x.classList.toggle('on', x === b); });
      renderScopes();
    });
    el('co-scopes').addEventListener('click', function (e) {
      var b = e.target.closest('.co-scope'); if (!b) return;
      toggleScope(b.dataset.kind, b.dataset.id || null);
    });
    el('co-sel').addEventListener('click', function (e) {
      if (e.target.closest('#co-sel-clear')) { S.targets = []; disarm(); updateUI(); return; }
      var sc = e.target.closest('.co-scope'); if (sc) { toggleScope(sc.dataset.kind, sc.dataset.id || null); return; }
      var d = e.target.closest('.co-dev'); if (d) toggleDevice(d.dataset.serial);
    });
    el('co-devs').addEventListener('click', function (e) {
      var paste = e.target.closest('#co-addpaste');
      if (paste) { addSerialsValidated(qEl.value); return; }
      var b = e.target.closest('.co-dev'); if (!b) return;
      toggleDevice(b.dataset.serial);
    });
    el('co-clear').addEventListener('click', function () { S.targets = []; disarm(); updateUI(); });
    el('co-pills').addEventListener('click', function (e) {
      var x = e.target.closest('[data-rm]'); if (!x) return;
      S.targets = S.targets.filter(function (t) { return selKey(t) !== x.dataset.rm; });
      disarm(); updateUI();
    });
    el('co-refine').addEventListener('click', function (e) {
      var b = e.target.closest('.co-ref'); if (!b) return;
      var k = b.dataset.k;
      S.refine[k] = S.refine[k] === b.dataset.v ? '' : b.dataset.v;
      pkgsKey = ''; disarm(); updateUI();
    });

    // Grid + strip wiring.
    el('co-grid').addEventListener('click', function (e) {
      var b = e.target.closest('.co-act'); if (!b) return;
      var a = DS.actions.find(function (x) { return x.t === b.dataset.t; }); if (!a) return;
      var n = incompatCount(a);
      setAction(a);
      updateUI();
      if (n > 0) toast(incompatWhy(a, n) + ' — use "Send to the compatible only" to narrow the selection', 'info');
      stripEl.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    });
    stripEl.addEventListener('click', function (e) {
      var sub = e.target.closest('#co-subset');
      if (sub) { subsetCompatible(); return; }
      var rm = e.target.closest('[data-chip-rm]');
      if (rm) {
        var kind = rm.dataset.chipRm, key = rm.dataset.key;
        if (kind === 'app') S.apps = S.apps.filter(function (x) { return x.url !== key; });
        else if (kind === 'pkg') S.pkgs = S.pkgs.filter(function (x) { return x.pkg !== key; });
        else if (kind === 'kioskpkg') S.kiosk = { enabled: '1' };
        else if (kind === 'query') S.query = null;
        disarm(); syncForm(); renderPay(); updateStripMeta();
        return;
      }
      var seg = e.target.closest('[data-kiosk]');
      if (seg) {
        S.kiosk = seg.dataset.kiosk === 'unlock' ? { enabled: '0' } : { enabled: '1', pkg: (S.kiosk && S.kiosk.pkg) || '', name: (S.kiosk && S.kiosk.name) || '' };
        disarm(); syncForm(); renderPay(); updateStripMeta();
        if (S.kiosk.enabled === '1' && !S.kiosk.pkg) openMenu('kioskpkg', stripEl.querySelector('[data-menu="kioskpkg"]'));
        return;
      }
      var add = e.target.closest('[data-menu]');
      if (add && add.dataset.menu === 'apps') { openAppModal(); return; }
      if (add) { openMenu(add.dataset.menu, add); return; }
    });
    menuEl.addEventListener('mousedown', function (e) { if (!e.target.closest('.msrch')) e.preventDefault(); });
    menuEl.addEventListener('click', function (e) {
      e.stopPropagation();
      var b = e.target.closest('.co-opt'); if (!b) return;
      var o = menuOpts[+b.dataset.oi]; if (!o) return;
      o.pick();
      disarm(); syncForm(); renderPay(); updateStripMeta();
      if (o.keepOpen) renderMenuKeepFocus(); else closeMenu();
      if (menu.kind) positionMenu();
    });

    // Global keys + click-away (replace previous handlers across boosted swaps).
    if (window.__coDocKey) document.removeEventListener('keydown', window.__coDocKey);
    if (window.__palDocKey) { document.removeEventListener('keydown', window.__palDocKey); window.__palDocKey = null; }
    window.__coDocKey = function (e) {
      if (e.key === 'Escape') { closeMenu(); if (typeof closeRecipeModal === 'function') closeRecipeModal(); return; }
      var tag = (e.target.tagName || '').toLowerCase();
      if (tag === 'input' || tag === 'textarea' || tag === 'select' || e.target.isContentEditable) return;
      if (e.key === '/') {
        var q = document.getElementById('co-q'); if (!q) return;
        e.preventDefault(); q.focus();
      }
    };
    document.addEventListener('keydown', window.__coDocKey);
    if (window.__coDocClick) document.removeEventListener('click', window.__coDocClick);
    if (window.__palDocClick) { document.removeEventListener('click', window.__palDocClick); window.__palDocClick = null; }
    window.__coDocClick = function (e) { if (!e.target.closest('.co-menu') && !e.target.closest('[data-menu]')) closeMenu(); };
    document.addEventListener('click', window.__coDocClick);
  }

  // Impact fragment reloaded → mirror the resolved counts (incl. DPC) onto the
  // basket, the capability grid and the Send button.
  if (window.__coImpact) document.body.removeEventListener('htmx:afterSwap', window.__coImpact);
  if (window.__palImpact) { document.body.removeEventListener('htmx:afterSwap', window.__palImpact); window.__palImpact = null; }
  window.__coImpact = function (e) {
    if (!e.target || e.target.id !== 'cmd-impact') return;
    var b = e.target.querySelector('.imp-body');
    if (!b) return;
    var unsupMap = null; try { unsupMap = b.dataset.unsup ? JSON.parse(b.dataset.unsup) : null; } catch (err) { unsupMap = null; }
    var jparse = function (v, dflt) { try { return v ? JSON.parse(v) : dflt; } catch (err) { return dflt; } };
    lastImpact = {
      effective: parseInt(b.dataset.effective || '0', 10),
      online: parseInt(b.dataset.online || '0', 10),
      offline: parseInt(b.dataset.offline || '0', 10),
      dpc: parseInt(b.dataset.dpc || '0', 10),
      unsupported: parseInt(b.dataset.unsupported || '0', 10),
      unsup: unsupMap,
      unsupSerials: b.dataset.unsupSerials || '',
      // Per agent kind: how many are selected, how many of each can run each action,
      // and the devices the selected action would skip. "DPC" on its own conflates the
      // Device Owner agent with MDM Lite, which is stored as a DPC kind.
      kinds: jparse(b.dataset.kinds, { fw: 0, dpc: 0, lite: 0 }),
      kindSup: jparse(b.dataset.kindsup, {}),
      skip: jparse(b.dataset.skip, []),
      verb: b.dataset.verb || ''
    };
    lastImpact.total = lastImpact.effective + lastImpact.unsupported;
    var w = e.target.querySelector('.imp-warn');
    lastWarn = w ? w.textContent.trim() : '';
    renderBasket();
    renderGrid();
    updateStripMeta();
  };
  document.body.addEventListener('htmx:afterSwap', window.__coImpact);

  // Fill the recent-send icons (server rows carry data-act-icon).
  document.querySelectorAll('#p-pinned .ic[data-act-icon]').forEach(function (ic) {
    ic.innerHTML = svg(ic.dataset.actIcon);
  });

  // ── Prefill: pinned/recent rows, ?clone= (Duplicate & edit), ?serials= (bulk
  // from the Devices page). Shape: {type, apk_url, apk_urls, package, packages,
  // shell_cmd, query_id, serials, groups, target}. ──
  function applyPrefill(pf) {
    if (!pf || !pf.type) return false;
    var act = DS.actions.find(function (a) { return a.t === pf.type; });
    if (!act) { toast('Your role cannot send “' + pf.type + '” actions', 'error'); return false; }
    setAction(act); S.action = act;
    var urls = pf.apk_urls || (pf.apk_url ? [pf.apk_url] : []);
    urls.forEach(function (u) {
      var app = DS.apps.find(function (a) { return a.url === u; });
      S.apps.push({ url: u, name: app ? app.name : tail(u) });
    });
    var pkgs = pf.packages || (pf.package ? [pf.package] : []);
    pkgs.forEach(function (p) { S.pkgs.push({ pkg: p, name: p }); });
    if (typeof pf.shell_cmd === 'string') S.shell = pf.shell_cmd;
    if (pf.query_id) { var x = DS.queries.find(function (o) { return o.id === pf.query_id; }); if (x) S.query = { id: x.id, label: x.label }; }
    S.targets = [];
    if (pf.serials && pf.serials.length) pf.serials.forEach(function (s) { S.targets.push({ kind: 'device', serial: s }); });
    else if (pf.groups && pf.groups.length) pf.groups.forEach(function (gid) {
      var g = DS.groups.find(function (o) { return o.id === gid; });
      if (g) S.targets.push({ kind: 'group', id: g.id, name: g.name, count: g.count });
    });
    else if (pf.target === 'all') S.targets = [{ kind: 'all' }];
    updateUI();
    return true;
  }
  window.coLoadRow = function (btn) {
    if (!CAN) return;
    var row = btn.closest('[data-prefill]');
    var pf; try { pf = JSON.parse(row.dataset.prefill); } catch (e) { return; }
    if (!applyPrefill(pf)) return;
    document.getElementById('co-main').scrollIntoView({ behavior: 'smooth', block: 'start' });
    toast('Loaded — review the selection and payload, then send', 'success');
  };
  window.coPinRow = function (btn) {
    if (!CAN) return;
    window.coLoadRow(btn);
    if (typeof openRecipeModal === 'function') openRecipeModal();
  };

  // Init: URL serials → selection; clone prefill; base device list for the rail.
  var qsp = new URLSearchParams(window.location.search), qs = qsp.get('serials');
  if (qs && CAN) addSerials(qs);
  if (window.CMD_PREFILL && CAN) applyPrefill(window.CMD_PREFILL);
  // ?type=reboot|screenshot|install_apk[&apk_url=] from the fleet selection panel:
  // pick the action too, so the page opens ready to send.
  if (qsp.get('type') && CAN && !window.CMD_PREFILL) {
    var pf0 = { type: qsp.get('type'), serials: qs ? qs.split(',').map(function (x) { return x.trim(); }).filter(Boolean) : [] };
    if (qsp.get('apk_url')) pf0.apk_url = qsp.get('apk_url');
    if (applyPrefill(pf0) && pf0.serials.length) {
      var main0 = document.getElementById('co-main'); if (main0) main0.scrollIntoView({ block: 'start' });
    }
  }
  updateUI();
  ensureDevices(true);
})();
