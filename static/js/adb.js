// Loaded by templates/adb.html (A1, picked 17 Oct). Three parts: the device picker of
// "Connect devices", the live progress of the devices being connected (polled from
// /adb/progress every 2 s until each is ready or failed; the serials ride the URL so a
// reload keeps watching), and the batch bar of "Open now".
(function () {
  var S = window.AdbSteps, picks = (window.ADB_PAGE && window.ADB_PAGE.picks) || [];

  // ── Picker ──────────────────────────────────────────────────────────────────
  var form = document.getElementById('adb-connect');
  if (form) {
    var q = document.getElementById('adb-search'), sug = document.getElementById('adb-sug'),
      chips = document.getElementById('adb-chips'), go = document.getElementById('adb-go'),
      chosen = [], act = 0, shown = [];
    var sync = function () {
      chips.innerHTML = chosen.map(function (s) {
        return '<span class="adb-chip">' + S.esc(s) + '<button type="button" data-rm="' + S.esc(s) + '" aria-label="Remove ' + S.esc(s) + '">×</button>' +
          '<input type="hidden" name="serial" value="' + S.esc(s) + '"></span>';
      }).join('');
      go.disabled = chosen.length === 0;
      go.textContent = chosen.length > 1 ? 'Connect ' + chosen.length + ' devices' : 'Connect';
    };
    var draw = function () {
      var t = q.value.trim().toLowerCase();
      shown = picks.filter(function (p) {
        return chosen.indexOf(p.s) < 0 && (!t || p.s.toLowerCase().indexOf(t) >= 0 || (p.v || '').toLowerCase().indexOf(t) >= 0);
      }).slice(0, 40);
      if (act >= shown.length) act = 0;
      sug.innerHTML = shown.length ? shown.map(function (p, i) {
        return '<div class="cc-mi' + (i === act ? ' act' : '') + '" role="option" data-s="' + S.esc(p.s) + '"><span class="dot' + (p.o ? ' on' : '') + '"></span>' +
          '<span class="mono">' + S.esc(p.s) + '</span>' + (p.a ? '<span class="cc-tag warn">adb on</span>' : '') +
          '<span class="sub">' + S.esc((p.v || 'Lab') + ' · ' + p.k + ' · ' + (p.o ? 'online' : 'offline')) + '</span></div>';
      }).join('') : '<div class="cc-mi is-static">No device matches</div>';
      sug.hidden = false; q.setAttribute('aria-expanded', 'true');
    };
    var hide = function () { sug.hidden = true; q.setAttribute('aria-expanded', 'false'); };
    // After a pick the list closes, so it never covers Connect; typing or ↓ opens it again.
    var pick = function (s) { if (s && chosen.indexOf(s) < 0) chosen.push(s); q.value = ''; sync(); hide(); q.focus(); };
    var quiet = false; // Escape clears a search box, which fires input: don't reopen on that one
    q.addEventListener('focus', function () { if (!chosen.length) draw(); });
    q.addEventListener('input', function () { if (quiet) { quiet = false; return; } act = 0; draw(); });
    q.addEventListener('keydown', function (e) {
      if (e.key === 'ArrowDown') { if (sug.hidden) { draw(); } else { act = Math.min(act + 1, shown.length - 1); draw(); } e.preventDefault(); }
      else if (e.key === 'ArrowUp') { act = Math.max(act - 1, 0); draw(); e.preventDefault(); }
      else if (e.key === 'Enter') { e.preventDefault(); if (!sug.hidden && shown[act]) pick(shown[act].s); }
      else if (e.key === 'Backspace' && !q.value && chosen.length) { chosen.pop(); sync(); }
      else if (e.key === 'Escape') { quiet = !!q.value; hide(); }
    });
    sug.addEventListener('mousedown', function (e) { var o = e.target.closest('[data-s]'); if (o) { e.preventDefault(); pick(o.getAttribute('data-s')); } });
    chips.addEventListener('click', function (e) { var b = e.target.closest('[data-rm]'); if (b) { chosen = chosen.filter(function (s) { return s !== b.getAttribute('data-rm'); }); sync(); } });
    document.getElementById('adb-q').addEventListener('click', function () { q.focus(); });
    document.addEventListener('click', function (e) { if (!e.target.closest('.adb-pick')) hide(); });
    form.addEventListener('submit', function () { go.disabled = true; go.textContent = 'Sending…'; });
    sync();
  }

  // ── Progress ────────────────────────────────────────────────────────────────
  var watch = document.getElementById('adb-watch');
  if (watch && watch.dataset.serials) {
    var serials = watch.dataset.serials.split(','), lan = false, last = [], timer = null;
    var devs = document.getElementById('adb-devs'), meta = document.getElementById('adb-watch-m'),
      box = document.getElementById('adb-cmds'), list = document.getElementById('adb-cmd-list'),
      cnt = document.getElementById('adb-cmds-n'), lanT = document.getElementById('adb-lan-t');
    var lines = function () {
      return last.map(function (d) { return lan ? d.lan : d.anywhere; }).filter(Boolean);
    };
    var render = function (ds) {
      last = ds;
      devs.innerHTML = ds.map(function (d) {
        return '<div class="adb-dev"><div class="hd"><span class="s">' + S.esc(d.serial) + '</span><span class="v">' + S.esc(d.venue || 'Lab') +
          (d.online ? '' : ' · offline') + '</span>' + S.tag(d) + '</div>' + S.steps(d) + '</div>';
      }).join('');
      var ready = ds.filter(function (d) { return d.state === 'ready'; }).length,
        failed = ds.filter(function (d) { return d.state === 'failed'; }).length,
        working = ds.filter(function (d) { return d.state === 'working'; }).length;
      var parts = [ready + ' of ' + ds.length + ' ready'];
      if (working) parts.push(working + ' on the way');
      if (failed) parts.push(failed + ' failed');
      meta.textContent = parts.join(' · ');
      var anyLan = ds.some(function (d) { return d.lan; }), anyAny = ds.some(function (d) { return d.anywhere; });
      if (!anyAny && anyLan) lan = true;
      lanT.style.display = anyLan && anyAny ? '' : 'none'; // .cc-btn's display beats [hidden]
      lanT.setAttribute('aria-pressed', lan ? 'true' : 'false');
      lanT.textContent = lan ? 'From anywhere' : 'Same network';
      var ls = ds.filter(function (d) { return lan ? d.lan : d.anywhere; });
      list.innerHTML = ls.map(function (d) { return S.cmd(lan ? d.lan : d.anywhere, d.serial); }).join('');
      cnt.textContent = ls.length ? ls.length + ' device' + (ls.length === 1 ? '' : 's') + (lan ? ' · same network' : ' · from anywhere') : '';
      box.hidden = ls.length === 0;
    };
    // "Open now" is drawn with the page; when a device turns ready it belongs there, so the
    // card is fetched again and swapped in (once per newly ready device).
    var readySeen = {}, first = true;
    var refreshOpen = function () {
      fetch(location.href, { credentials: 'same-origin' }).then(function (r) { return r.text(); }).then(function (html) {
        var fresh = new DOMParser().parseFromString(html, 'text/html').getElementById('adb-open-card'),
          old = document.getElementById('adb-open-card');
        if (fresh && old) { old.replaceWith(fresh); if (window.htmx) window.htmx.process(fresh); wireOpen(); }
      }).catch(function () {});
    };
    var tick = function () {
      if (!document.body.contains(watch)) return; // navigated away (boosted)
      S.fetch(serials).then(function (r) {
        render(r.devices || []);
        var newly = (r.devices || []).filter(function (d) { return d.state === 'ready' && !readySeen[d.serial]; });
        newly.forEach(function (d) { readySeen[d.serial] = true; });
        if (newly.length && !first) refreshOpen(); // the first look is what the page was drawn with
        first = false;
        var done = (r.devices || []).every(S.settled);
        timer = setTimeout(tick, done ? 30000 : 2000); // once done, only keep the tunnel's state fresh
      }).catch(function () { timer = setTimeout(tick, 5000); });
    };
    lanT.addEventListener('click', function () { lan = !lan; render(last); });
    document.getElementById('adb-copy-all').addEventListener('click', function () { S.copy(lines().join('\n'), this); });
    tick();
  }

  // ── Open now: select several, turn off or copy in batch ─────────────────────
  function wireOpen() {
    var off = document.getElementById('adb-off-form');
    if (!off) return;
    var boxes = function () { return Array.prototype.slice.call(off.querySelectorAll('td input[name=serial]:not(:disabled)')); },
      bar = document.getElementById('adb-bar'), count = document.getElementById('adb-count'), all = document.getElementById('adb-all');
    var upd = function () { var k = boxes().filter(function (c) { return c.checked; }).length; count.textContent = k; bar.hidden = k === 0; };
    boxes().forEach(function (c) { c.addEventListener('change', upd); });
    all.addEventListener('change', function () { var on = this.checked; boxes().forEach(function (c) { c.checked = on; }); upd(); });
    document.getElementById('adb-clear').addEventListener('click', function () { boxes().forEach(function (c) { c.checked = false; }); all.checked = false; upd(); });
    document.getElementById('adb-copy-sel').addEventListener('click', function () {
      var ls = boxes().filter(function (c) { return c.checked; }).map(function (c) { var tr = c.closest('tr'); return tr.dataset.any || tr.dataset.lan; }).filter(Boolean);
      S.copy(ls.join('\n'), this);
    });
    // A row's own Turn off / End tunnel / Allow acts on that row alone, not on what is ticked.
    off.addEventListener('click', function (e) {
      var b = e.target.closest('button[type=submit]');
      if (b && (b.dataset.one || b.getAttribute('formaction'))) boxes().forEach(function (c) { c.checked = false; });
    });
    upd();
  }
  wireOpen();
})();
