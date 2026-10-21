// Loaded by templates/users_access.html: Access control AC1 with the powers matrix M2
// (picked 21 Oct 2026). Sees / Role pills filter the matrix in the page; the Role cell
// changes the role; clicking a person opens the two-question editor under their row,
// which saves through the same routes as the person's page (/users/{id}/access/simple,
// with /simple/preview for the "If you save" line); "Who can see a device?" asks
// /users/access/who.
(function () {
  var card = document.getElementById('ac1-matrix');
  if (!card) return;
  var F = { sees: '', role: '' };
  var rows = function () { return card.querySelectorAll('tr.ac1-r'); };
  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }

  function apply() {
    rows().forEach(function (r) {
      var s = F.sees, ok = true;
      if (s === 'whole' || s === 'part') ok = r.dataset.sees === s;
      else if (s) ok = (' ' + r.dataset.flags + ' ').indexOf(' ' + s + ' ') !== -1;
      if (F.role && r.dataset.role !== F.role) ok = false;
      r.hidden = !ok;
      var ed = card.querySelector('tr.ac1-edr[data-for="' + r.dataset.id + '"]');
      if (ed && !ok) { ed.hidden = true; r.classList.remove('open'); }
    });
  }
  function setPill(key, val) {
    var label = 'Any';
    card.querySelectorAll('[data-ac1-set="' + key + '"]').forEach(function (i) {
      var on = i.dataset.val === val; i.classList.toggle('active', on); if (on) label = i.textContent;
    });
    var p = card.querySelector('[data-ac1-pill="' + key + '"]');
    if (p) { p.querySelector('b').textContent = label; p.classList.toggle('set', !!val); }
  }
  card.addEventListener('click', function (e) {
    var mi = e.target.closest('[data-ac1-set]');
    if (mi) { F[mi.dataset.ac1Set] = mi.dataset.val; setPill(mi.dataset.ac1Set, mi.dataset.val); if (window.ocCloseMenus) window.ocCloseMenus(); apply(); return; }
    var r = e.target.closest('tr.ac1-r');
    if (!r || r.dataset.edit !== '1' || e.target.closest('select,form,a,button,input')) return;
    toggle(r);
  });
  if (card.dataset.show) { F.sees = card.dataset.show; setPill('sees', F.sees); apply(); }

  // The Role cell: confirm, then submit the row's form (POST /users/{id}/role).
  window.ac1Role = function (sel) {
    var r = sel.closest('tr'), name = r ? r.dataset.name : 'them';
    var label = sel.options[sel.selectedIndex].textContent;
    if (!confirm('Make ' + name + ' ' + label + '? Their rules stay; the role sets the most they can do.')) { sel.value = sel.dataset.was; return; }
    sel.form.submit();
  };

  // ── The row editor ──
  function toggle(r) {
    var ed = card.querySelector('tr.ac1-edr[data-for="' + r.dataset.id + '"]');
    if (!ed) return;
    var open = ed.hidden;
    card.querySelectorAll('tr.ac1-edr').forEach(function (x) { x.hidden = true; });
    card.querySelectorAll('tr.ac1-r.open').forEach(function (x) { x.classList.remove('open'); });
    if (!open) return;
    ed.hidden = false; r.classList.add('open');
    var box = ed.querySelector('.ac1-ed');
    if (!box.dataset.built) build(box, JSON.parse(ed.querySelector('.ac1-data').textContent));
  }

  function build(box, d) {
    box.dataset.built = '1';
    var id = box.dataset.id, name = box.dataset.name, first = name.split(' ')[0];
    var base = '/users/' + id + '/access';
    if (!d.ok) {
      box.innerHTML = '<div class="ac1-odd"><b>' + esc(name) + ' has a setup the two questions can\'t show.</b> ' + esc(d.why || '') +
        ' Read or change it on their page.</div><div class="ac1-foot"><button type="button" class="cc-btn sm" data-x="close">Close</button><a href="/users/' + id + '/profile?open=rules#ua-adv">Open ' + esc(first) + '\'s page →</a></div>';
      box.querySelector('[data-x=close]').onclick = function () { toggle(card.querySelector('tr.ac1-r[data-id="' + id + '"]')); };
      return;
    }
    var places = {};
    (d.places || []).forEach(function (o) { places[o.type + ':' + o.id] = o; });
    var has = {}; (d.actions || []).forEach(function (k) { has[k] = true; });
    var ex = d.expires ? 'custom' : 'never';
    var h = '';
    h += '<div class="ac1-q"><span class="n">1</span><div><h4>Which devices can ' + esc(first) + ' see?</h4><div class="ac1-opts">' +
      '<label class="ac1-opt"><input type="radio" name="see" value="all"' + (d.every ? ' checked' : '') + '>Every device</label>' +
      '<label class="ac1-opt"><input type="radio" name="see" value="only"' + (d.every ? '' : ' checked') + '>Only these…</label></div>' +
      '<div class="ac1-places"' + (d.every ? ' hidden' : '') + '><input type="text" class="form-input" placeholder="Add a venue, a group, or type a serial…" autocomplete="off"><div class="ac1-dd" hidden></div><div class="ac1-chips"></div></div></div></div>';
    h += '<div class="ac1-q"><span class="n">2</span><div><h4>What can ' + esc(first) + ' do there?</h4><div class="ac1-opts">';
    (d.choices || []).forEach(function (c) {
      h += '<label class="ac1-opt' + (c.fits ? '' : ' off') + '" title="' + esc(c.desc || '') + (c.fits ? '' : ' More than ' + esc(d.article) + ' can have.') + '"><input type="radio" name="do" value="' + esc(c.value) + '"' + (d.do === c.value ? ' checked' : '') + (c.fits ? '' : ' disabled') + '>' + esc(c.name) + '</label>';
    });
    h += '<label class="ac1-opt"><input type="radio" name="do" value="custom"' + (d.do === 'custom' ? ' checked' : '') + '>Custom…</label></div>' +
      '<div class="ac1-opts ac1-custom" style="margin-top:8px"' + (d.do === 'custom' ? '' : ' hidden') + '>';
    (d.custom || []).forEach(function (a) {
      h += '<label class="ac1-opt' + (a.sensitive ? ' hot' : '') + '"><input type="checkbox" name="actions" value="' + esc(a.key) + '" data-sensitive="' + (a.sensitive ? '1' : '') + '"' + (has[a.key] ? ' checked' : '') + '>' + esc(a.label) + '</label>';
    });
    h += '</div></div></div>';
    h += '<label class="ac1-conf" hidden><input type="checkbox" name="confirm_sensitive" value="1"><span>I understand this opens live sessions on these devices.</span></label>';
    h += '<div class="ac1-ends">Ends <select class="form-input" data-x="exp"><option value="never">Never</option><option value="7d">In 7 days</option><option value="30d">In 30 days</option><option value="custom">Pick a date…</option></select>' +
      '<input type="datetime-local" class="form-input" data-x="expc" value="' + esc(d.expires || '') + '"' + (d.expires ? '' : ' hidden') + '>' +
      '<input type="text" class="form-input" name="note" maxlength="200" placeholder="Why (optional)" value="' + esc(d.note || '') + '" style="flex:1;min-width:180px"></div>';
    h += '<div class="ac1-pv">If you save: …</div>';
    h += '<div class="ac1-foot"><button type="button" class="cc-btn sm primary" data-x="save">Save access</button><button type="button" class="cc-btn sm" data-x="close">Cancel</button><a href="/users/' + id + '/profile">Open ' + esc(first) + '\'s page →</a></div>';
    box.innerHTML = h;
    box.querySelector('[data-x=exp]').value = ex;

    var q = box.querySelector('.ac1-places input'), dd = box.querySelector('.ac1-dd'), chips = box.querySelector('.ac1-chips');
    function renderChips() {
      chips.innerHTML = Object.keys(places).map(function (k) { return '<span class="ac1-chip">' + esc(places[k].name) + '<button type="button" data-k="' + esc(k) + '" aria-label="Remove">×</button></span>'; }).join('') ||
        '<span class="muted small">Nothing yet. With no places they see no devices.</span>';
    }
    chips.addEventListener('click', function (e) { var b = e.target.closest('button[data-k]'); if (b) { delete places[b.dataset.k]; renderChips(); preview(); } });
    var st;
    q.addEventListener('input', function () {
      clearTimeout(st); var v = q.value.trim();
      if (!v) { dd.hidden = true; return; }
      st = setTimeout(function () {
        fetch('/users/access/scope-search?q=' + encodeURIComponent(v), { credentials: 'same-origin' }).then(function (r) { return r.json(); }).then(function (list) {
          list = (list || []).filter(function (o) { return o.type !== 'all'; }).slice(0, 12);
          dd.innerHTML = list.map(function (o, i) { return '<button type="button" data-i="' + i + '">' + esc(o.name) + '<small>' + esc(o.meta || o.type) + '</small></button>'; }).join('') || '<div class="muted small" style="padding:8px">Nothing matches.</div>';
          dd.hidden = false;
          dd.onclick = function (e) { var b = e.target.closest('button[data-i]'); if (!b) return; var o = list[+b.dataset.i]; places[o.type + ':' + o.id] = o; q.value = ''; dd.hidden = true; renderChips(); preview(); };
        }).catch(function () { dd.hidden = true; });
      }, 180);
    });
    renderChips();

    function form() {
      var fd = new URLSearchParams();
      var see = (box.querySelector('input[name=see]:checked') || {}).value || 'all';
      fd.append('see', see);
      if (see === 'only') Object.keys(places).forEach(function (k) { fd.append('scopes', k); });
      var dO = (box.querySelector('input[name=do]:checked') || {}).value || '';
      fd.append('do', dO);
      if (dO === 'custom') box.querySelectorAll('input[name=actions]:checked').forEach(function (c) { fd.append('actions', c.value); });
      var conf = box.querySelector('input[name=confirm_sensitive]'); if (conf.checked) fd.append('confirm_sensitive', '1');
      fd.append('note', box.querySelector('input[name=note]').value);
      var e = box.querySelector('[data-x=exp]').value;
      fd.append('expires', e === 'custom' ? box.querySelector('[data-x=expc]').value : e);
      return fd;
    }
    function sync() {
      var see = (box.querySelector('input[name=see]:checked') || {}).value;
      box.querySelector('.ac1-places').hidden = see !== 'only';
      var dO = (box.querySelector('input[name=do]:checked') || {}).value;
      box.querySelector('.ac1-custom').hidden = dO !== 'custom';
      var hot = dO === 'custom' && box.querySelector('.ac1-custom input[data-sensitive="1"]:checked');
      box.querySelector('.ac1-conf').hidden = !hot;
      box.querySelector('[data-x=expc]').hidden = box.querySelector('[data-x=exp]').value !== 'custom';
    }
    var pt, seq = 0, pv = box.querySelector('.ac1-pv'), save = box.querySelector('[data-x=save]');
    function preview() {
      sync(); clearTimeout(pt);
      pt = setTimeout(function () {
        var n = ++seq;
        fetch(base + '/simple/preview', { method: 'POST', credentials: 'same-origin', headers: { 'HX-Request': 'true', 'Content-Type': 'application/x-www-form-urlencoded' }, body: form() })
          .then(function (r) { return r.json(); }).then(function (res) {
            if (n !== seq) return;
            pv.classList.toggle('err', !!res.error);
            if (res.error) { pv.textContent = 'Can\'t save: ' + res.error; save.disabled = true; return; }
            save.disabled = false;
            var can = res.can && res.can.length ? res.can.join(', ') : 'only look';
            pv.innerHTML = '<b>If you save:</b> ' + esc(first) + ' sees ' + res.devices + ' of ' + res.total + ' devices and can ' + esc(can) + '.';
          }).catch(function () {});
      }, 200);
    }
    box.addEventListener('change', preview);
    box.querySelector('[data-x=close]').onclick = function () { toggle(card.querySelector('tr.ac1-r[data-id="' + id + '"]')); };
    save.onclick = function () {
      save.disabled = true;
      fetch(base + '/simple', { method: 'POST', credentials: 'same-origin', headers: { 'HX-Request': 'true', 'Content-Type': 'application/x-www-form-urlencoded' }, body: form() })
        .then(function (r) {
          if (r.ok) { location.href = '/users/access?flash=' + encodeURIComponent('Access saved for ' + name) + '&flash_type=ok'; return; }
          return r.text().then(function (t) { pv.classList.add('err'); pv.textContent = 'Not saved: ' + (t.trim() || r.status); save.disabled = false; });
        }).catch(function () { save.disabled = false; });
    };
    preview();
  }

  // ── Who can see a device? ──
  window.ac1Who = function (f) {
    var out = document.getElementById('ac1-who-r'), serial = f.serial.value.trim();
    if (!serial) return false;
    out.textContent = 'Checking…';
    fetch('/users/access/who?serial=' + encodeURIComponent(serial), { credentials: 'same-origin' }).then(function (r) { return r.json(); }).then(function (d) {
      if (d.error) { out.textContent = d.error; return; }
      out.innerHTML = '<b class="mono">' + esc(d.serial) + '</b>' + (d.venue ? ' · ' + esc(d.venue) : ' · no venue') + ': <b>' + d.count + ' of ' + d.total + '</b> people can see it.' +
        '<ul>' + (d.people || []).map(function (p) { return '<li>' + esc(p.name) + ' <small>' + esc(p.role) + ' · ' + esc(p.why) + '</small></li>'; }).join('') + '</ul>';
    }).catch(function () { out.textContent = 'Could not check.'; });
    return false;
  };
})();
