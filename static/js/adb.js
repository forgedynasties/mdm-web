// Loaded by templates/adb.html (W2, picked 3 Nov 2026). The list is Fleet's twin
// (coll_list.js filters it); this file is the connect pane: the ticked rows (Connect, Turn off,
// Copy commands), and the live progress of the devices being connected (polled from
// /adb/progress every 2 s until each is ready or failed; the serials ride the URL so a reload
// keeps watching). When a device turns ready the list is fetched again and swapped in, keeping
// the view, filters and ticks.
(function () {
  var S = window.AdbSteps;
  var form = document.getElementById('adb-act');
  if (!form) return;
  var picked = document.getElementById('adb-picked'), go = document.getElementById('adb-go'),
    off = document.getElementById('adb-off'), copySel = document.getElementById('adb-copy-sel'),
    clear = document.getElementById('adb-clear'), head = document.getElementById('adb-act-h'),
    meta = document.getElementById('adb-act-m'), metaDefault = meta.textContent;

  function rows() { return Array.prototype.slice.call(document.querySelectorAll('#adb-rows tr.ft-row')); }
  function ticked() { return rows().filter(function (r) { var c = r.querySelector('.adb-tick'); return c && c.checked; }); }

  function sync() {
    var t = ticked();
    form.querySelectorAll('input[name=serial]').forEach(function (i) { i.remove(); });
    t.forEach(function (r) {
      var i = document.createElement('input'); i.type = 'hidden'; i.name = 'serial'; i.value = r.dataset.serial; form.appendChild(i);
    });
    picked.innerHTML = t.map(function (r) {
      return '<span>' + S.esc(r.dataset.serial) + '<button type="button" data-unpick="' + S.esc(r.dataset.serial) + '" aria-label="Untick ' + S.esc(r.dataset.serial) + '">×</button></span>';
    }).join('');
    var openN = t.filter(function (r) { return r.dataset.group === 'open' && r.dataset.switch; }).length,
      cmdN = t.filter(function (r) { return r.dataset.any || r.dataset.lan; }).length;
    head.textContent = t.length ? (t.length === 1 ? 'Connect 1 device' : 'Connect ' + t.length + ' devices') : 'Connect devices';
    meta.textContent = t.length ? (openN ? openN + ' of them already open: Connect again sets a new time.' : 'They stay open for the time you pick, then close on their own.') : metaDefault;
    go.disabled = !t.length;
    off.disabled = !openN;
    off.textContent = openN ? 'Turn off ' + openN : 'Turn off';
    copySel.disabled = !cmdN;
    clear.hidden = !t.length;
  }
  document.addEventListener('change', function (e) {
    if (e.target.classList && e.target.classList.contains('adb-tick')) sync();
    if (e.target.id === 'adb-all') {
      var on = e.target.checked;
      rows().forEach(function (r) { var c = r.querySelector('.adb-tick'); if (c && !c.disabled && !r.hidden) c.checked = on; });
      sync();
    }
  });
  document.addEventListener('click', function (e) {
    var b = e.target.closest('[data-unpick]');
    if (b) { rows().forEach(function (r) { if (r.dataset.serial === b.dataset.unpick) r.querySelector('.adb-tick').checked = false; }); sync(); return; }
    b = e.target.closest('[data-pick-one]');
    if (b) {
      rows().forEach(function (r) { var c = r.querySelector('.adb-tick'); if (c) c.checked = r.dataset.serial === b.dataset.pickOne; });
      sync(); go.focus(); return;
    }
    // One ⋯ menu open at a time; a click elsewhere closes it.
    document.querySelectorAll('details.adb-more[open]').forEach(function (d) { if (!d.contains(e.target)) d.open = false; });
  });
  clear.addEventListener('click', function () {
    rows().forEach(function (r) { var c = r.querySelector('.adb-tick'); if (c) c.checked = false; });
    var all = document.getElementById('adb-all'); if (all) all.checked = false;
    sync();
  });
  copySel.addEventListener('click', function () {
    S.copy(ticked().map(function (r) { return r.dataset.any || r.dataset.lan; }).filter(Boolean).join('\n'), this);
  });
  form.addEventListener('submit', function (e) {
    var b = e.submitter;
    if (b === off && !confirm('Turn wireless adb off on ' + ticked().filter(function (r) { return r.dataset.group === 'open' && r.dataset.switch; }).length + ' device(s)?')) { e.preventDefault(); return; }
    if (b === go) { go.disabled = true; go.textContent = 'Sending…'; }
  });
  sync();

  // ── Progress ────────────────────────────────────────────────────────────────
  var watch = document.getElementById('adb-watch');
  if (!watch || !watch.dataset.serials) return;
  var serials = watch.dataset.serials.split(','), lan = false, last = [];
  var devs = document.getElementById('adb-devs'), wmeta = document.getElementById('adb-watch-m'),
    box = document.getElementById('adb-cmds'), list = document.getElementById('adb-cmd-list'),
    cnt = document.getElementById('adb-cmds-n'), lanT = document.getElementById('adb-lan-t');
  function lines() { return last.map(function (d) { return lan ? d.lan : d.anywhere; }).filter(Boolean); }
  function render(ds) {
    last = ds;
    devs.innerHTML = ds.map(function (d) {
      return '<div class="adb-dev"><div class="hd"><span class="s">' + S.esc(d.serial) + '</span><span class="v">' + S.esc(d.venue || 'Lab') +
        (d.online ? '' : ' · offline') + '</span>' + S.tag(d) + '</div>' + (S.settled(d) ? '' : S.steps(d)) + '</div>';
    }).join('');
    var ready = ds.filter(function (d) { return d.state === 'ready'; }).length,
      failed = ds.filter(function (d) { return d.state === 'failed'; }).length,
      working = ds.filter(function (d) { return d.state === 'working'; }).length;
    var parts = [ready + ' of ' + ds.length + ' ready'];
    if (working) parts.push(working + ' on the way');
    if (failed) parts.push(failed + ' failed');
    wmeta.textContent = parts.join(' · ');
    var anyLan = ds.some(function (d) { return d.lan; }), anyAny = ds.some(function (d) { return d.anywhere; });
    if (!anyAny && anyLan) lan = true;
    lanT.style.display = anyLan && anyAny ? '' : 'none'; // .cc-btn's display beats [hidden]
    lanT.setAttribute('aria-pressed', lan ? 'true' : 'false');
    lanT.textContent = lan ? 'From anywhere' : 'Same network';
    var ls = ds.filter(function (d) { return lan ? d.lan : d.anywhere; });
    list.innerHTML = ls.map(function (d) { return S.cmd(lan ? d.lan : d.anywhere, d.serial); }).join('');
    cnt.textContent = ls.length ? ls.length + (lan ? ' · same network' : ' · from anywhere') : '';
    box.hidden = ls.length === 0;
  }
  // A device that turned ready belongs in Open now: fetch the page again and swap the list in.
  var readySeen = {}, first = true;
  function refreshList() {
    fetch(location.href, { credentials: 'same-origin' }).then(function (r) { return r.text(); }).then(function (html) {
      var fresh = new DOMParser().parseFromString(html, 'text/html').getElementById('adb-root'),
        old = document.getElementById('adb-root');
      if (!fresh || !old) return;
      var keep = ticked().map(function (r) { return r.dataset.serial; });
      fresh._clCarry = old._clState;
      old.replaceWith(fresh);
      if (window.htmx) window.htmx.process(fresh);
      if (window.collListBoot) window.collListBoot();
      rows().forEach(function (r) { var c = r.querySelector('.adb-tick'); if (c && keep.indexOf(r.dataset.serial) >= 0) c.checked = true; });
      sync();
    }).catch(function () {});
  }
  function tick() {
    if (!document.body.contains(watch)) return; // navigated away (boosted)
    S.fetch(serials).then(function (r) {
      var ds = r.devices || [];
      render(ds);
      var newly = ds.filter(function (d) { return d.state === 'ready' && !readySeen[d.serial]; });
      newly.forEach(function (d) { readySeen[d.serial] = true; });
      if (newly.length && !first) refreshList(); // the first look is what the page was drawn with
      first = false;
      setTimeout(tick, ds.every(S.settled) ? 30000 : 2000); // once done, only keep the tunnel's state fresh
    }).catch(function () { setTimeout(tick, 5000); });
  }
  lanT.addEventListener('click', function () { lan = !lan; render(last); });
  document.getElementById('adb-copy-all').addEventListener('click', function () { S.copy(lines().join('\n'), this); });
  tick();
})();
