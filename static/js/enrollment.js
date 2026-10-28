// Loaded by templates/enrollment.html.
(function () {
  function el(id) { return document.getElementById(id); }
  window.enCopy = function (btn, text) {
    var done = function () { var o = btn.textContent; btn.textContent = 'Copied'; setTimeout(function () { btn.textContent = o; }, 1200); };
    if (navigator.clipboard && navigator.clipboard.writeText) { navigator.clipboard.writeText(text).then(done, function () { fallback(); }); } else fallback();
    function fallback() { var t = document.createElement('textarea'); t.value = text; document.body.appendChild(t); t.select(); try { document.execCommand('copy'); } catch (e) {} t.remove(); done(); }
  };
  // Tabs. The open one rides in ?tab= (replaceState, no history spam); a tab the URL
  // names that does not exist falls back to the page's default for its current state.
  (function () {
    var strip = document.querySelector('.en-tabs'); if (!strip) return;
    var tabs = [].slice.call(strip.querySelectorAll('[data-tab]')), panes = [].slice.call(document.querySelectorAll('.en-tabpane'));
    function show(t, push) {
      tabs.forEach(function (b) { var on = b.dataset.tab === t; b.classList.toggle('on', on); b.setAttribute('aria-selected', on ? 'true' : 'false'); });
      panes.forEach(function (p) { p.classList.toggle('on', p.dataset.tab === t); });
      if (push) { try { var u = new URL(location.href); u.searchParams.set('tab', t); history.replaceState(null, '', u); } catch (e) {} }
    }
    var want = new URLSearchParams(location.search).get('tab');
    var fallback = strip.dataset.default || (tabs[0] && tabs[0].dataset.tab);
    show(tabs.some(function (b) { return b.dataset.tab === want; }) ? want : fallback, false);
    tabs.forEach(function (b) { b.addEventListener('click', function () { show(b.dataset.tab, true); }); });
    // One ⋯ menu open at a time; a click anywhere else closes it.
    document.addEventListener('click', function (e) {
      document.querySelectorAll('details.en-kebab[open]').forEach(function (d) { if (!d.contains(e.target)) d.removeAttribute('open'); });
    });
    document.addEventListener('toggle', function (e) {
      var d = e.target; if (!d || !d.classList || !d.classList.contains('en-kebab') || !d.open) return;
      document.querySelectorAll('details.en-kebab[open]').forEach(function (o) { if (o !== d) o.removeAttribute('open'); });
    }, true);
  })();
  // Profile selection: the toolbar's buttons wake up with the count; "all" ticks what
  // the search currently shows; delete asks.
  (function () {
    var form = el('en-prof-bulk'); if (!form) return;
    var all = el('en-prof-all'), n = el('en-prof-n');
    function picks() { return [].slice.call(document.querySelectorAll('.en-pick')); }
    function sync() {
      var on = picks().filter(function (c) { return c.checked; }).length;
      n.textContent = on ? on + ' selected' : 'none selected';
      form.querySelectorAll('button').forEach(function (b) { b.disabled = !on; });
    }
    document.addEventListener('change', function (e) { if (e.target.classList && e.target.classList.contains('en-pick')) sync(); });
    all.addEventListener('change', function () {
      picks().forEach(function (c) { var row = c.closest('.lt-row'); if (!row || !row.classList.contains('lt-out')) c.checked = all.checked; });
      sync();
    });
    window.enBulkConfirm = function (f) {
      var a = document.activeElement && document.activeElement.value;
      if (a === 'delete') { var k = picks().filter(function (c) { return c.checked; }).length; return confirm('Delete ' + k + ' profile' + (k === 1 ? '' : 's') + '? Devices they enrolled keep working.'); }
      return true;
    };
    sync();
  })();
  window.enToggleQR = function (id) {
    var box = el('en-qr-' + id); if (!box) return;
    var img = box.querySelector('img[data-src]');
    if (img && !img.src) img.src = img.getAttribute('data-src');
    var opening = !box.classList.contains('open');
    // One at a time: a second open QR just pushes the page around.
    document.querySelectorAll('.en-qr.open').forEach(function (o) { if (o !== box) o.classList.remove('open'); });
    box.classList.toggle('open', opening);
    if (opening) box.scrollIntoView({ behavior: 'smooth', block: 'center' });
  };
  // create / edit modal
  window.enOpenModal = function (btn) {
    var m = el('en-modal'), f = el('en-modal-form');
    var d = btn && btn.dataset && btn.dataset.id ? btn.dataset : null;
    f.action = d ? '/enrollment/profiles/' + d.id + '/update' : '/enrollment/profiles';
    el('en-modal-title').textContent = d ? 'Edit profile' : 'New enrollment profile';
    el('en-f-submit').textContent = d ? 'Save' : 'Create profile';
    el('en-f-name').value = d ? d.name : '';
    el('en-f-site').value = d ? d.site : '';
    el('en-f-group').value = d ? d.group : '';
    el('en-f-max').value = d ? d.max : '';
    el('en-f-exp').value = '';
    el('en-f-exp-hint').textContent = d ? 'Blank keeps it open-ended; a number sets a new expiry from today.' : 'Leave blank for no expiry.';
    el('en-f-notes').value = d ? d.notes : '';
    m.classList.add('modal-open');
    setTimeout(function () { el('en-f-name').focus(); }, 50);
  };
  window.enCloseModal = function (e) {
    var m = el('en-modal');
    if (m && (!e || e.target === m)) m.classList.remove('modal-open');
  };
  // how-to tabs
  var tabs = el('en-how-tabs');
  if (tabs) tabs.addEventListener('click', function (e) {
    var b = e.target.closest('button'); if (!b) return;
    tabs.querySelectorAll('button').forEach(function (x) { x.classList.toggle('on', x === b); });
    document.querySelectorAll('.en-pane').forEach(function (p) { p.classList.toggle('on', p.dataset.pane === b.dataset.pane); });
  });
  // Live inbox: any device event may be an enrollment or a first check-in.
  // Re-fetch the inbox fragment (throttled) and patch the counters.
  // Every device's telemetry fires a refresh, and swapping the rows closed an open site
  // dropdown and dropped a site picked but not yet sent with Done (28 Oct 2026). So: skip
  // a fragment that has not changed, hold one while the inbox has focus (an open select
  // does) until focus leaves it, and carry picked sites across the swap by row id.
  var lastInbox = null, heldInbox = null;
  function inboxFocused(box) {
    var a = document.activeElement;
    return !!a && a !== document.body && box.contains(a);
  }
  function applyInbox(html) {
    var box = el('en-inbox'); if (!box) return;
    if (html === lastInbox) return;
    lastInbox = html;
    if (inboxFocused(box)) { heldInbox = html; return; }
    heldInbox = null;
    var picked = {};
    box.querySelectorAll('select.en-sel').forEach(function (s) {
      var o = s.selectedOptions && s.selectedOptions[0];
      var row = s.closest('.en-row');
      if (o && !o.defaultSelected && row && row.id) picked[row.id] = s.value;
    });
    box.innerHTML = html;
    Object.keys(picked).forEach(function (id) {
      var s = document.querySelector('#' + CSS.escape(id) + ' select.en-sel');
      if (s) s.value = picked[id];
    });
    if (window.ltInit) ltInit();
    var n = parseInt((box.firstElementChild && box.firstElementChild.dataset.count) || '0', 10) || 0;
    var m = el('en-inbox-m'); if (m) m.textContent = n ? n + ' waiting' : 'nothing waiting';
    var k = el('en-kpi-inbox'); if (k) { k.textContent = n; k.classList.toggle('warn', n > 0); }
    var h = el('en-h1'); if (h) h.innerHTML = n ? '<span class="hl warn">' + n + '</span> new device' + (n === 1 ? '' : 's') + ' to onboard' : 'Every device is <span class="hl ok">placed</span>';
    if (window.htmx) htmx.process(box);
  }
  var pending = null;
  function refreshInbox() {
    if (pending) return;
    pending = setTimeout(function () {
      pending = null;
      fetch('/enrollment?partial=inbox', { headers: { 'Accept': 'text/html' } }).then(function (r) { return r.ok ? r.text() : null; }).then(function (t) { if (t != null) applyInbox(t); }).catch(function () {});
    }, 900);
  }
  window.enRefreshInbox = refreshInbox;
  var inboxBox = el('en-inbox');
  if (inboxBox) inboxBox.addEventListener('focusout', function () {
    setTimeout(function () {
      if (heldInbox == null || inboxFocused(inboxBox)) return;
      var h = heldInbox; lastInbox = null; applyInbox(h);
    }, 0);
  });
  // A successful Done (204 + toast) drops the row at once; the throttled re-fetch
  // then reconciles counters and anything that arrived meanwhile.
  document.body.addEventListener('htmx:afterRequest', function (e) {
    var f = e.detail && e.detail.elt;
    if (!f || !f.classList || !f.classList.contains('en-onboard')) return;
    if (!e.detail.successful) return;
    var row = f.closest('.en-row'); if (row) row.remove();
    refreshInbox();
  });
  if (window.EventSource && window.mdmStream) {
    var es = window.mdmStream('/events/devices');
    es.addEventListener('device-update', refreshInbox);
  }
})();
