/* Moves page (/moves, read-only). Draws the rail, the progress strip, the table and the detail
   pane from the JSON in #mv-data, and redraws from GET /moves/data every 30 s keeping the
   selection, filters, search and scroll. moves_page.go builds the document. */
(function () {
  'use strict';
  var root = document.getElementById('mv-root');
  if (!root || root.dataset.mvBound) return;
  root.dataset.mvBound = '1';
  var D = JSON.parse(document.getElementById('mv-data').textContent || '{}');
  var S = { st: 'all', place: '', kind: '', online: false, q: '', sort: 'state', sel: '', rq: '' };
  var $ = function (id) { return document.getElementById(id); };
  var rail = $('mv-rail'), strip = $('mv-strip'), body = $('mv-body'), none = $('mv-none'), detail = $('mv-detail'), tw = $('mv-tw');

  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function ago(iso) {
    if (!iso) return 'never';
    var s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
    if (s < 90) return 'now';
    if (s < 5400) return Math.round(s / 60) + ' min ago';
    if (s < 129600) return Math.round(s / 3600) + ' h ago';
    return Math.round(s / 86400) + ' d ago';
  }
  var STATES = [
    ['all', 'All moves', ''], ['attention', 'Needs attention', 'bad'], ['pending', 'Pending', ''],
    ['waiting_client', 'Waiting for client update', 'warn'], ['sent', 'Sent / moving now', ''],
    ['arrived', 'Arrived', 'ok'], ['came_back', 'Came back', 'bad'], ['failed', 'Failed', 'bad'], ['canceled', 'Canceled', '']
  ];
  var LABEL = { pending: 'Pending', waiting_client: 'Updating client', sent: 'Sent', arrived: 'Arrived', came_back: 'Came back', failed: 'Failed', canceled: 'Canceled' };
  var DOT = { pending: 'c-pending', waiting_client: 'c-waiting_client', sent: 'c-sent', arrived: 'c-arrived', came_back: 'c-came_back', failed: 'c-failed', canceled: 'c-canceled' };
  var ORDER = { came_back: 0, failed: 1, sent: 2, waiting_client: 3, pending: 4, canceled: 5, arrived: 6 };

  // The rail's two lists are drawn into containers, so the search box keeps focus on a refresh.
  rail.innerHTML = '<div class="fleet-rail-cap"><span>State</span></div><div id="mv-rs"></div>' +
    '<div class="fleet-rail-cap"><span>Where</span></div><div id="mv-rsearch"></div><div id="mv-rp"></div>';
  var rs = $('mv-rs'), rp = $('mv-rp'), rsearch = $('mv-rsearch');

  function attSet() { var m = {}; (D.attention || []).forEach(function (a) { m[a.serial] = a; }); return m; }
  function inState(m, att) {
    switch (S.st) {
      case 'all': return true;
      case 'attention': return !!att[m.serial];
      default: return m.bucket === S.st;
    }
  }
  function visible() {
    var att = attSet(), q = S.q.trim().toLowerCase();
    return (D.moves || []).filter(function (m) {
      if (!inState(m, att)) return false;
      if (S.place && m.venue_key !== S.place) return false;
      if (S.kind && m.kind !== S.kind) return false;
      if (S.online && !m.online) return false;
      if (q && (m.serial + ' ' + (m.venue || (m.venue_key === 'toplace' ? 'to place' : ''))).toLowerCase().indexOf(q) < 0) return false;
      return true;
    }).sort(function (a, b) {
      var ta = a.last_seen ? Date.parse(a.last_seen) : 0, tb = b.last_seen ? Date.parse(b.last_seen) : 0;
      if (S.sort === 'seen') return tb - ta || (a.serial < b.serial ? -1 : 1);
      if (S.sort === 'venue') {
        var va = a.venue_key === 'toplace' ? '' : a.venue.toLowerCase(), vb = b.venue_key === 'toplace' ? '' : b.venue.toLowerCase();
        return va < vb ? -1 : va > vb ? 1 : (a.serial < b.serial ? -1 : 1);
      }
      var d = ORDER[a.bucket] - ORDER[b.bucket];
      return d || tb - ta || (a.serial < b.serial ? -1 : 1);
    });
  }

  function drawRail() {
    var c = D.counts || {};
    rs.innerHTML = STATES.map(function (s) {
      var n = s[0] === 'all' ? D.total : (c[s[0]] || 0);
      return '<button type="button" class="fleet-rail-item' + (S.st === s[0] ? ' on' : '') + '" data-st="' + s[0] + '" aria-pressed="' + (S.st === s[0]) + '"><span class="nm">' + esc(s[1]) + '</span><span class="ct">' + n + '</span></button>';
    }).join('');
    var places = D.places || [], rq = S.rq.trim().toLowerCase();
    // One search box for the venues, made once so it keeps focus through a redraw.
    if (places.length > 8 && !rsearch.firstChild) rsearch.innerHTML = '<input class="mv-rs" type="search" placeholder="Search venues" aria-label="Search venues" autocomplete="off">';
    var h = '<button type="button" class="fleet-rail-item' + (S.place === '' ? ' on' : '') + '" data-pl="" aria-pressed="' + (S.place === '') + '"><span class="nm">Everywhere</span></button>';
    places.forEach(function (p) {
      if (rq && p.name.toLowerCase().indexOf(rq) < 0) return;
      var pc = p.total ? Math.round(100 * p.arrived / p.total) : 0;
      h += '<button type="button" class="fleet-rail-item' + (S.place === p.key ? ' on' : '') + '" data-pl="' + esc(p.key) + '" aria-pressed="' + (S.place === p.key) + '" title="' + esc(p.name) + '"><span class="nm">' + esc(p.name) + (p.hidden ? '<small>hidden</small>' : '') + '</span><span class="mv-pb"><i style="width:' + pc + '%"></i></span><span class="ct">' + p.arrived + '/' + p.total + '</span></button>';
    });
    rp.innerHTML = h;
  }

  function drawStrip() {
    var tot = D.total || 0, arr = D.arrived || 0, pc = tot ? Math.round(100 * arr / tot) : 0, c = D.counts || {};
    var C = 2 * Math.PI * 28;
    var seg = [['arrived', 'Arrived'], ['sent', 'Sent'], ['waiting_client', 'Updating client'], ['pending', 'Pending'], ['canceled', 'Canceled']];
    var bad = (c.came_back || 0) + (c.failed || 0);
    var bar = seg.map(function (s) { var n = c[s[0]] || 0; return n ? '<i class="' + DOT[s[0]] + '" style="width:' + (100 * n / tot) + '%"></i>' : ''; }).join('') +
      (bad ? '<i class="c-came_back" style="width:' + (100 * bad / tot) + '%"></i>' : '');
    var leg = seg.filter(function (s) { return s[0] !== 'canceled' || c.canceled; }).map(function (s) { return '<span><b class="' + DOT[s[0]] + '"></b>' + s[1] + ' ' + (c[s[0]] || 0) + '</span>'; }).join('') +
      '<span><b class="c-came_back"></b>Came back / failed ' + bad + '</span>';
    var att = (D.attention || []);
    var al = att.length ? '<ul>' + att.map(function (a) {
      return '<li data-sn="' + esc(a.serial) + '" tabindex="0"><i class="' + (a.kind === 'dark' || a.kind === 'tooold' ? 'y' : '') + '"></i><div><b>' + esc(a.title) + '</b><span><code>' + esc(a.serial) + '</code>' + (a.text ? ' · ' + esc(a.text) : '') + '</span></div></li>';
    }).join('') + '</ul>' : '<div class="mv-ok">Nothing needs a person right now.</div>';
    strip.innerHTML = '<div><div class="mv-prog"><svg viewBox="0 0 70 70" role="img" aria-label="' + pc + ' percent arrived"><circle class="bg" cx="35" cy="35" r="28"/><circle class="fg" cx="35" cy="35" r="28" stroke-dasharray="' + (C * pc / 100) + ' ' + C + '"/><text class="pc" x="35" y="40" text-anchor="middle">' + pc + '%</text></svg>' +
      '<div class="mv-big">' + arr + '<span>of ' + tot + ' arrived</span></div></div><div class="mv-stack">' + bar + '</div><div class="mv-leg">' + leg + '</div></div>' +
      '<div class="mv-att"><h3>Needs attention · ' + att.length + '</h3>' + al + '</div>';
  }

  function cvOf(m) {
    if (m.kind === 'fw' && !m.have) return '<span class="mut">unknown</span>';
    if (!m.have) return '<span class="mut">unknown</span>';
    switch (m.standing) {
      case 'current': return esc(m.have) + ' <span class="mut">current</span>';
      case 'behind': return esc(m.have) + ' <span class="warn">&rarr; ' + esc(m.want) + '</span>';
      case 'tooold': return '<span class="bad">' + esc(m.have) + '</span> <span class="mut">too old</span>';
    }
    return esc(m.have) + ' <span class="mut">unknown</span>';
  }
  function seenOf(m) {
    var t = m.dark_days ? (m.dark_days === 9999 ? 'never seen' : 'dark ' + m.dark_days + ' days') : ago(m.last_seen);
    return '<span class="ls"><i class="od' + (m.online ? '' : ' off') + '"></i>' + esc(m.online ? 'now' : t) + '</span>';
  }

  function drawTable() {
    var rows = visible(), top = tw.scrollTop;
    body.innerHTML = rows.map(function (m) {
      var venue = m.venue_key === 'toplace' ? '<span class="tp">To place</span>' : esc(m.venue) + (m.venue_hidden ? '<span class="hid">hidden</span>' : '');
      var bad = m.bucket === 'came_back' || m.bucket === 'failed' || m.standing === 'tooold';
      return '<tr tabindex="0" data-sn="' + esc(m.serial) + '"' + (S.sel === m.serial ? ' class="sel"' : '') + '>' +
        '<td><span class="sn">' + esc(m.serial) + '</span><span class="kt">' + (m.kind === 'fw' ? 'FW' : 'STD') + '</span></td>' +
        '<td>' + venue + '</td><td class="cv">' + cvOf(m) + '</td><td>' + seenOf(m) + '</td>' +
        '<td><span class="sp"><i class="' + DOT[m.bucket] + '"></i>' + LABEL[m.bucket] + '</span><div class="nt2' + (bad ? ' bad' : '') + '">' + esc(m.next) + '</div></td>' +
        '<td class="ntc"><div class="nt' + (bad ? ' bad' : '') + '" title="' + esc(m.next) + '">' + esc(m.next) + '</div></td></tr>';
    }).join('');
    none.hidden = rows.length > 0;
    tw.scrollTop = top;
  }

  function drawDetail() {
    var m = (D.moves || []).filter(function (x) { return x.serial === S.sel; })[0];
    if (!m) {
      detail.innerHTML = '<h2 style="font-family:inherit">Move detail</h2><p class="mv-help" style="margin-top:12px">Pick a row to see where it is in the move: filed, imported on prod, client updated, sent, arrived.</p><p class="mv-help">' + visible().length + ' of ' + (D.total || 0) + ' moves shown.</p>';
      return;
    }
    var venue = m.venue_key === 'toplace' ? 'To place' : m.venue;
    detail.innerHTML = '<h2>' + esc(m.serial) + '</h2>' +
      '<div class="mv-sub"><span class="sp"><i class="' + DOT[m.bucket] + '"></i>' + LABEL[m.bucket] + '</span><span>&middot; ' + esc(venue) + (m.venue_hidden ? ' (hidden venue)' : '') + '</span></div>' +
      '<div class="mv-facts"><div><small>Kind</small><b>' + (m.kind === 'fw' ? 'Firmware client' : 'Standard client') + '</b></div>' +
      '<div><small>Client</small><b>' + cvOf(m) + '</b></div>' +
      '<div><small>Last seen</small><b><span class="ls"><i class="od' + (m.online ? '' : ' off') + '"></i>' + esc(m.online ? 'now' : (m.dark_days ? (m.dark_days === 9999 ? 'never seen' : 'dark ' + m.dark_days + ' days') : ago(m.last_seen))) + '</span></b></div>' +
      '<div><small>Attempts</small><b>' + m.attempts + '</b></div>' +
      '<div><small>Filed by</small><b>' + esc(m.created_by || '—') + '</b></div>' +
      '<div><small>Device</small><b>' + (m.device_hidden ? 'hidden' : 'listed') + '</b></div></div>' +
      (m.note ? '<div class="mv-note">' + esc(m.note) + '</div>' : '') +
      '<ol class="mv-tl">' + (m.steps || []).map(function (s) {
        return '<li class="' + s.state + '"><span class="nd">' + (s.state === 'done' ? '&#10003;' : s.state === 'bad' ? '!' : s.state === 'now' ? '&hellip;' : '') + '</span><b>' + esc(s.title) + '</b>' + (s.sub ? '<span>' + esc(s.sub) + '</span>' : '') + '</li>';
      }).join('') + '</ol>' +
      '<div class="mv-act"><a href="/devices/' + encodeURIComponent(m.serial) + '">Open device</a><button type="button" id="mv-copy">Copy serial</button></div>';
  }

  function drawAll() { drawRail(); drawStrip(); drawTable(); drawDetail(); }

  // Events (delegated: the pieces are redrawn).
  rail.addEventListener('click', function (e) {
    var b = e.target.closest('button[data-st],button[data-pl]');
    if (!b) return;
    if (b.hasAttribute('data-st')) S.st = b.getAttribute('data-st'); else S.place = b.getAttribute('data-pl');
    drawRail(); drawTable(); drawDetail();
  });
  rail.addEventListener('input', function (e) {
    if (!e.target.classList.contains('mv-rs')) return;
    S.rq = e.target.value; drawRail();
  });
  function pick(sn) {
    S.sel = sn; drawTable(); drawDetail();
    if (window.matchMedia('(max-width:1100px)').matches) detail.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  }
  body.addEventListener('click', function (e) { var r = e.target.closest('tr[data-sn]'); if (r) pick(r.getAttribute('data-sn')); });
  body.addEventListener('keydown', function (e) {
    var r = e.target.closest('tr[data-sn]');
    if (!r) return;
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); pick(r.getAttribute('data-sn')); }
    else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault(); var n = e.key === 'ArrowDown' ? r.nextElementSibling : r.previousElementSibling;
      if (n) { var sn = n.getAttribute('data-sn'); pick(sn); var t = body.querySelector('tr[data-sn="' + CSS.escape(sn) + '"]'); if (t) t.focus(); }
    }
  });
  strip.addEventListener('click', function (e) {
    var l = e.target.closest('li[data-sn]'); if (!l) return;
    S.st = 'all'; S.place = ''; S.q = l.getAttribute('data-sn'); $('mv-q').value = S.q; drawRail(); pick(l.getAttribute('data-sn'));
  });
  $('mv-kind').addEventListener('click', function (e) {
    var b = e.target.closest('button[data-k]'); if (!b) return;
    S.kind = b.getAttribute('data-k');
    [].forEach.call(this.children, function (c) { c.classList.toggle('on', c === b); });
    drawTable(); drawDetail();
  });
  $('mv-online').addEventListener('click', function () { S.online = !S.online; this.setAttribute('aria-pressed', S.online); drawTable(); drawDetail(); });
  $('mv-q').addEventListener('input', function () { S.q = this.value; drawTable(); drawDetail(); });
  $('mv-sort').addEventListener('change', function () { S.sort = this.value; drawTable(); });
  detail.addEventListener('click', function (e) {
    if (e.target.id !== 'mv-copy') return;
    var b = e.target, t = S.sel;
    var done = function () { b.textContent = 'Copied'; setTimeout(function () { b.textContent = 'Copy serial'; }, 1500); };
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(t).then(done, function () { });
    else { var ta = document.createElement('textarea'); ta.value = t; document.body.appendChild(ta); ta.select(); try { document.execCommand('copy'); done(); } catch (x) { } ta.remove(); }
  });

  // Refresh every 30 s without losing the selection, filters, search or scroll.
  var busy = false;
  function refresh() {
    if (busy || document.hidden) return;
    busy = true;
    fetch('/moves/data', { credentials: 'same-origin', headers: { Accept: 'application/json' } })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function (j) {
        D = j; var top = tw.scrollTop, rtop = rail.scrollTop;
        drawRail(); drawStrip(); drawTable(); drawDetail(); tw.scrollTop = top; rail.scrollTop = rtop;
      }).catch(function () { }).then(function () { busy = false; });
  }
  var timer = setInterval(function () { if (!document.body.contains(root)) { clearInterval(timer); return; } refresh(); }, 30000);
  window.mvRefresh = refresh;
  drawAll();
})();
