// Loaded by templates/user_access.html.
(function(){
  var UID = window.USER_ACCESS_PAGE.id;
  var NAME = window.USER_ACCESS_PAGE.name;
  var BASE = '/users/' + UID + '/access';
  var ICON = {
    all:'<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a15 15 0 0 1 0 18M12 3a15 15 0 0 0 0 18"/></svg>',
    restaurant:'<svg viewBox="0 0 24 24"><path d="M3 21h18M5 21V7l7-4 7 4v14"/><path d="M9 21v-6h6v6"/></svg>',
    group:'<svg viewBox="0 0 24 24"><rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/></svg>',
    device:'<svg viewBox="0 0 24 24"><rect x="5" y="2" width="14" height="20" rx="2"/><path d="M12 18h.01"/></svg>'
  };
  function $(id){ return document.getElementById(id); }
  function esc(s){ return String(s == null ? '' : s).replace(/[&<>"']/g, function(c){ return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]; }); }
  function go(msg, type){ location.href = BASE + '?flash=' + encodeURIComponent(msg) + '&flash_type=' + (type || 'ok'); }
  function post(url, body){
    // FormData → urlencoded: the handlers read r.Form (ParseForm), which does not parse multipart.
    var enc = new URLSearchParams(); if (body) body.forEach(function(v, k){ enc.append(k, v); });
    return fetch(url, { method:'POST', credentials:'same-origin', headers:{'HX-Request':'true','Content-Type':'application/x-www-form-urlencoded'}, body: enc }).then(function(r){
      if (r.ok) { var t = ''; try { var tr = JSON.parse(r.headers.get('HX-Trigger') || '{}'); t = tr.toast ? tr.toast.msg : ''; } catch(e){} return { ok:true, msg:t }; }
      return r.text().then(function(t){ return { ok:false, msg: t.trim() || ('Request failed (' + r.status + ')') }; });
    });
  }

  /* ── starting point: auto-save ── */
  var baseForm = $('ua-base');
  if (baseForm) baseForm.addEventListener('change', function(){
    var st = $('ua-base-state'); st.textContent = 'saving…';
    post(baseForm.action, new FormData(baseForm)).then(function(res){
      if (res.ok) { st.textContent = 'saved'; if (window.showToast) showToast('Starting point saved', 'ok'); refreshWords(); refreshCheck(); }
      else { st.textContent = 'not saved'; if (window.showToast) showToast(res.msg, 'error'); }
    });
  });

  /* ── scope search (shared by the drawer and the checker) ── */
  function typeahead(input, dd, opts){
    var timer, items = [], hi = -1, closed = false;
    function render(){
      if (closed) return;
      if (!items.length) { dd.innerHTML = '<div class="ua-opt none">' + (input.value.trim() ? 'Nothing matches' : 'Start typing…') + '</div>'; dd.hidden = false; return; }
      dd.innerHTML = items.map(function(o, i){
        return '<div class="ua-opt' + (i === hi ? ' hi' : '') + '" data-i="' + i + '"><span class="ty ' + o.type + '">' + ICON[o.type] + '</span><span class="nm' + (o.type === 'device' ? ' mono' : '') + '">' + esc(o.name) + '</span><span class="mt">' + esc(o.meta || '') + '</span></div>';
      }).join('');
      dd.hidden = false;
    }
    function load(){
      var q = input.value.trim();
      if (opts.minLen && q.length < opts.minLen) { dd.hidden = true; return; }
      fetch('/users/access/scope-search?q=' + encodeURIComponent(q), { credentials:'same-origin' }).then(function(r){ return r.json(); }).then(function(list){
        items = (list || []).filter(opts.filter || function(){ return true; }); hi = items.length ? 0 : -1; render();
      });
    }
    input.addEventListener('input', function(){ closed = false; clearTimeout(timer); timer = setTimeout(load, 140); });
    input.addEventListener('focus', function(){ if (!opts.minLen || input.value.trim().length >= opts.minLen) load(); });
    input.addEventListener('keydown', function(e){
      if (dd.hidden) return;
      if (e.key === 'ArrowDown') { hi = Math.min(items.length - 1, hi + 1); render(); e.preventDefault(); }
      else if (e.key === 'ArrowUp') { hi = Math.max(0, hi - 1); render(); e.preventDefault(); }
      else if (e.key === 'Enter') { if (hi >= 0 && items[hi]) { opts.pick(items[hi]); dd.hidden = true; closed = true; clearTimeout(timer); } e.preventDefault(); }
      else if (e.key === 'Escape') { dd.hidden = true; }
    });
    dd.addEventListener('mousedown', function(e){ var o = e.target.closest('.ua-opt'); if (o && o.dataset.i != null) { opts.pick(items[+o.dataset.i]); dd.hidden = true; closed = true; clearTimeout(timer); e.preventDefault(); } });
    document.addEventListener('click', function(e){ if (!input.contains(e.target) && !dd.contains(e.target)) dd.hidden = true; });
  }

  /* ── drawer ── */
  var chips = {}; // key -> option
  var editing = null;
  function chipsRender(){
    var host = $('ua-chips'); host.innerHTML = '';
    Object.keys(chips).forEach(function(k){
      var o = chips[k], el = document.createElement('span');
      el.className = 'ua-sc ' + o.type; el.innerHTML = ICON[o.type] + '<span' + (o.type === 'device' ? ' class="mono"' : '') + '>' + esc(o.name) + '</span>' + (editing ? '' : '<span class="x" title="Remove">×</span>');
      el.querySelector('svg').style.cssText = 'width:12px;height:12px;fill:none;stroke:currentColor;stroke-width:2';
      if (!editing) el.querySelector('.x').onclick = function(){ delete chips[k]; chipsRender(); preview(); };
      host.appendChild(el);
    });
    preview();
  }
  window.uaPick = function(o){ if (o.type === 'all') chips = {}; else delete chips['all']; chips[o.type === 'all' ? 'all' : o.type + ':' + o.id] = o; $('ua-q').value = ''; chipsRender(); };
  typeahead($('ua-q'), $('ua-dd'), { pick: uaPick });

  function checkedActions(){ return Array.prototype.slice.call(document.querySelectorAll('#ua-form input[name=actions]:checked')); }
  function syncActions(){
    var any = $('ua-any').checked;
    document.querySelectorAll('#ua-form .ua-ck').forEach(function(l){
      var ck = l.querySelector('input'); var sens = ck.dataset.sensitive === 'true';
      if (any && !sens) { ck.checked = false; ck.disabled = true; l.classList.add('dim'); } else { ck.disabled = false; l.classList.remove('dim'); }
    });
    var effect = document.querySelector('#ua-form input[name=effect]:checked').value;
    var hot = checkedActions().filter(function(c){ return c.dataset.sensitive === 'true'; }).map(function(c){ return c.dataset.label.toLowerCase(); });
    var conf = $('ua-confirm'); conf.hidden = !(effect === 'allow' && hot.length);
    if (!conf.hidden) $('ua-confirm-txt').textContent = 'I understand this gives ' + NAME + ' ' + hot.join(' and ') + ' on every device this rule covers.';
    $('ua-effhelp').textContent = effect === 'allow' ? 'Grants the actions below.' : 'Takes the actions below away, even if another rule allows them.';
    preview();
  }
  document.querySelectorAll('#ua-form input[name=actions], #ua-form input[name=effect]').forEach(function(i){ i.addEventListener('change', syncActions); });
  $('ua-note').addEventListener('input', preview);
  window.uaExpChange = function(){
    var v = $('ua-exp').value, c = $('ua-exp-custom'); c.hidden = v !== 'custom';
    $('ua-expires').value = v === 'custom' ? (c.value || '') : v; preview();
  };
  $('ua-exp-custom').addEventListener('change', uaExpChange);

  function preview(){
    var effect = document.querySelector('#ua-form input[name=effect]:checked').value;
    var acts = checkedActions().map(function(c){ return c.value === '*' ? 'take any action' : c.dataset.label.toLowerCase(); });
    var where = Object.keys(chips).map(function(k){ var o = chips[k]; return o.type === 'all' ? 'the whole fleet' : (o.type === 'restaurant' ? 'venue ' : o.type === 'group' ? 'group ' : 'device ') + o.name; });
    var pasted = ($('ua-form').serials.value.match(/[A-Za-z0-9]{6,}/g) || []).length;
    if (pasted) where.push(pasted + ' pasted serial' + (pasted > 1 ? 's' : ''));
    var p = $('ua-preview');
    if (!acts.length || !where.length) { p.innerHTML = 'Pick where and which actions to see the rule in plain words.'; return; }
    var s = '<b>' + esc(NAME) + '</b> ' + (effect === 'allow' ? 'can ' : '<b class="warn">cannot</b> ') + esc(join(acts)) + ' in ' + esc(join(where));
    var ex = $('ua-expires').value;
    if (ex && ex !== 'never') s += ex.indexOf('d') === ex.length - 1 && ex.length <= 3 ? ' for ' + ex.replace('d', ' day') + (ex !== '1d' ? 's' : '') : ' until ' + ex.replace('T', ' ');
    p.innerHTML = s + '.' + (where.length > 1 && !editing ? ' <span class="muted">(' + where.length + ' rules, one per place)</span>' : '');
  }
  function join(xs){ return xs.length < 2 ? xs.join('') : xs.slice(0, -1).join(', ') + ' and ' + xs[xs.length - 1]; }

  function reset(){
    $('ua-form').reset(); chips = {}; editing = null; $('ua-gid').value = '';
    $('ua-dtitle').textContent = 'New rule'; $('ua-save').textContent = 'Save rule'; $('ua-err').textContent = '';
    $('ua-step-scope').querySelectorAll('.ua-search, .ua-paste, [onclick], a').forEach(function(el){ el.hidden = false; });
    $('ua-paste').hidden = true; $('ua-exp-custom').hidden = true; $('ua-expires').value = 'never';
    chipsRender(); syncActions();
  }
  window.uaOpen = function(){ reset(); var d = $('ua-drawer'); d.hidden = false; d.scrollIntoView({ behavior:'smooth', block:'start' }); setTimeout(function(){ $('ua-q').focus(); }, 250); };
  window.uaClose = function(){ $('ua-drawer').hidden = true; };
  window.uaEdit = function(btn){
    var row = btn.closest('.ua-g'); reset();
    editing = row.dataset.id; $('ua-gid').value = editing;
    $('ua-dtitle').textContent = 'Edit rule'; $('ua-save').textContent = 'Save changes';
    document.querySelector('#ua-form input[name=effect][value=' + row.dataset.effect + ']').checked = true;
    chips[row.dataset.scopeType === 'all' ? 'all' : row.dataset.scopeType + ':x'] = { type: row.dataset.scopeType, id: 'x', name: row.dataset.scopeName || 'Whole fleet' };
    $('ua-step-scope').querySelectorAll('.ua-search, .ua-paste, [onclick], a').forEach(function(el){ el.hidden = true; });
    row.dataset.actions.split(',').forEach(function(a){ var ck = document.querySelector('#ua-form input[name=actions][value="' + a + '"]'); if (ck) ck.checked = true; });
    $('ua-note').value = row.dataset.note || '';
    if (row.dataset.expires) { $('ua-exp').value = 'custom'; $('ua-exp-custom').value = row.dataset.expires; $('ua-exp-custom').hidden = false; $('ua-expires').value = row.dataset.expires; }
    chipsRender(); syncActions();
    var d = $('ua-drawer'); d.hidden = false; d.scrollIntoView({ behavior:'smooth', block:'start' });
  };
  window.uaRemove = function(btn){
    var row = btn.closest('.ua-g');
    if (!confirm('Remove this rule?\n\n' + row.querySelector('.ua-scope').textContent.trim() + ' · ' + row.querySelector('.ua-acts').textContent.trim())) return;
    post(BASE + '/grants/' + row.dataset.id + '/delete', new FormData()).then(function(res){ if (res.ok) go('Rule removed'); else if (window.showToast) showToast(res.msg, 'error'); });
  };
  window.uaSubmit = function(e){
    e.preventDefault();
    var f = $('ua-form'), fd = new FormData(f), err = $('ua-err'); err.textContent = '';
    if (!checkedActions().length) { err.textContent = 'Pick at least one action.'; return false; }
    if (!editing) {
      Object.keys(chips).forEach(function(k){ fd.append('scopes', k === 'all' ? 'all' : k); });
      if (!Object.keys(chips).length && !fd.get('serials').trim()) { err.textContent = 'Pick where the rule applies.'; return false; }
    }
    $('ua-save').disabled = true;
    post(editing ? BASE + '/grants/' + editing + '/edit' : BASE + '/grants', fd).then(function(res){
      $('ua-save').disabled = false;
      if (res.ok) go(res.msg || 'Saved'); else err.textContent = res.msg;
    });
    return false;
  };

  /* ── checker ── */
  var curSerial = '';
  typeahead($('ua-cq'), $('ua-cdd'), { minLen: 2, filter: function(o){ return o.type === 'device'; }, pick: function(o){ $('ua-cq').value = o.name; curSerial = o.name; refreshCheck(); } });
  $('ua-cq').addEventListener('change', function(){ curSerial = $('ua-cq').value.trim(); refreshCheck(); });
  function refreshCheck(){
    fetch(BASE + '/check?serial=' + encodeURIComponent(curSerial), { credentials:'same-origin' }).then(function(r){ return r.json(); }).then(function(d){
      var dev = $('ua-cdev'), res = $('ua-cres');
      if (d.error) { dev.innerHTML = '<span style="color:var(--danger)">' + esc(d.error) + '</span>'; return; }
      dev.innerHTML = d.device ? '<b class="mono">' + esc(d.device.serial) + '</b>' + (d.device.nickname ? ' · ' + esc(d.device.nickname) : '') + (d.device.venue ? ' · ' + esc(d.device.venue) : ' · no venue') : '';
      var groups = {}; var order = [];
      (d.actions || []).forEach(function(a){ if (!groups[a.group]) { groups[a.group] = []; order.push(a.group); } groups[a.group].push(a); });
      res.innerHTML = order.map(function(g){
        return '<div class="ua-cr-h">' + esc(d.device ? g : 'Fleet-wide actions') + '</div>' + groups[g].map(function(a){
          return '<div class="ua-cr ' + (a.allowed ? 'ok' : 'no') + '"><span class="st">' + (a.allowed ? '<svg viewBox="0 0 24 24"><path d="M20 6 9 17l-5-5"/></svg>' : '<svg viewBox="0 0 24 24"><path d="M18 6 6 18M6 6l12 12"/></svg>') + '</span><span>' + esc(a.label) + '</span><span class="why" title="' + esc(a.rule || '') + '">' + esc(a.rule || a.reason) + '</span></div>';
        }).join('');
      }).join('') || '<div class="muted small">Type a serial to see what ' + esc(NAME) + ' could do on it.</div>';
    });
  }
  function refreshWords(){
    fetch(location.pathname, { credentials:'same-origin', headers:{'HX-Request':'true','HX-Boosted':'true'} }).then(function(r){ return r.text(); }).then(function(html){
      var doc = new DOMParser().parseFromString(html, 'text/html'); var w = doc.querySelector('.ua-words'); if (w) document.querySelector('.ua-words').innerHTML = w.innerHTML;
    }).catch(function(){});
  }
  refreshCheck();

  /* ── the two questions ── */
  var sf = $('ua-simple');
  if (sf) {
    var places = {};
    (window.USER_ACCESS_PAGE.places || []).forEach(function(o){ places[o.type + ':' + o.id] = o; });
    function placesRender(){
      var host = $('ua-s-chips'); host.innerHTML = '';
      Object.keys(places).forEach(function(k){
        var o = places[k], el = document.createElement('span');
        el.className = 'ua-sc ' + o.type; el.title = o.meta || '';
        el.innerHTML = ICON[o.type] + '<span' + (o.type === 'device' ? ' class="mono"' : '') + '>' + esc(o.name) + '</span><span class="x" title="Remove">×</span>';
        el.querySelector('svg').style.cssText = 'width:12px;height:12px;fill:none;stroke:currentColor;stroke-width:2';
        el.querySelector('.x').onclick = function(){ delete places[k]; placesRender(); sPreview(); };
        host.appendChild(el);
      });
      if (!Object.keys(places).length) host.innerHTML = '<span class="muted small">Nothing yet. With no places they see no devices.</span>';
    }
    typeahead($('ua-s-q'), $('ua-s-dd'), { filter: function(o){ return o.type !== 'all'; }, pick: function(o){ places[o.type + ':' + o.id] = o; $('ua-s-q').value = ''; placesRender(); sPreview(); } });
    function sForm(){
      var fd = new FormData(sf);
      if (fd.get('see') === 'only') Object.keys(places).forEach(function(k){ fd.append('scopes', k); });
      if (fd.get('do') !== 'custom') fd.delete('actions');
      return fd;
    }
    function sSync(){
      var see = (sf.querySelector('input[name=see]:checked') || {}).value;
      $('ua-s-places').hidden = see !== 'only';
      var dO = (sf.querySelector('input[name=do]:checked') || {}).value;
      $('ua-s-custom').hidden = dO !== 'custom';
      var hot = Array.prototype.slice.call(sf.querySelectorAll('#ua-s-custom input[name=actions]:checked')).filter(function(c){ return c.dataset.sensitive === 'true'; }).map(function(c){ return c.dataset.label.toLowerCase(); });
      var conf = $('ua-s-confirm'); conf.hidden = !(dO === 'custom' && hot.length);
      if (!conf.hidden) $('ua-s-confirm-txt').textContent = 'I understand this gives ' + NAME + ' ' + hot.join(' and ') + ' on every device they can see.';
      var ex = $('ua-s-exp').value, c = $('ua-s-exp-custom'); c.hidden = ex !== 'custom';
      $('ua-s-expires').value = ex === 'custom' ? (c.value || '') : ex;
    }
    var pvTimer, pvSeq = 0;
    function sPreview(){
      sSync(); clearTimeout(pvTimer);
      pvTimer = setTimeout(function(){
        var seq = ++pvSeq; $('ua-pv-state').textContent = 'checking…';
        var enc = new URLSearchParams(); sForm().forEach(function(v, k){ enc.append(k, v); });
        fetch(BASE + '/simple/preview', { method:'POST', credentials:'same-origin', headers:{'HX-Request':'true','Content-Type':'application/x-www-form-urlencoded'}, body: enc }).then(function(r){ return r.json(); }).then(function(d){
          if (seq !== pvSeq) return;
          $('ua-pv-state').textContent = '';
          if (d.devices != null) {
            $('ua-pv-n').innerHTML = esc(NAME.split(' ')[0]) + ' sees ' + d.devices + ' <small>of ' + d.total + ' device' + (d.total === 1 ? '' : 's') + '</small>';
            $('ua-pv-can').textContent = d.devices ? (d.can && d.can.length ? join(d.can) + '.' : 'Only look: no commands.') : 'Nothing: no devices in view.';
            $('ua-pv-cannot').textContent = d.cannot && d.cannot.length ? join(d.cannot) + '.' : 'Nothing held back.';
          }
          var e = $('ua-pv-err'); e.hidden = !d.error; e.textContent = d.error ? 'Can\'t save: ' + d.error : '';
          $('ua-s-save').disabled = !!d.error;
        }).catch(function(){ $('ua-pv-state').textContent = ''; });
      }, 180);
    }
    sf.addEventListener('change', sPreview);
    $('ua-s-note').addEventListener('input', sSync);
    window.uaSimpleSave = function(e){
      e.preventDefault(); sSync();
      var err = $('ua-s-err'); err.textContent = '';
      if (!sf.querySelector('input[name=do]:checked')) { err.textContent = 'Pick what they can do.'; return false; }
      $('ua-s-save').disabled = true;
      post(BASE + '/simple', sForm()).then(function(res){
        $('ua-s-save').disabled = false;
        if (res.ok) go(res.msg || 'Access saved'); else err.textContent = res.msg;
      });
      return false;
    };
    placesRender(); sPreview();
  }
})();
