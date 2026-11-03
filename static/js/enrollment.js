// The Enrollment views of the Setup page (M1, 3 Nov 2026: Enrollment moved into Setup). Each
// view's pane is swapped in by htmx (#oh-pane), so the per-pane wiring runs again after every
// swap; the page-wide pieces (helpers, document listeners) run once.
(function () {
  function el(id) { return document.getElementById(id); }

  // Per pane: profile selection, the How to enroll tabs, Enrolled's "this week" flag.
  function init() {
    // Profile selection: the toolbar's buttons wake up with the count; "all" ticks what
    // the search currently shows; delete asks.
    var form = el('en-prof-bulk');
    if (form && !form.dataset.enReady) {
      form.dataset.enReady = '1';
      var all = el('en-prof-all');
      if (all) all.addEventListener('change', function () {
        picks().forEach(function (c) { var row = c.closest('.lt-row'); if (!row || !row.classList.contains('lt-out')) c.checked = all.checked; });
        syncProfiles();
      });
      syncProfiles();
    }
    // How to enroll: its own small tab strip.
    var tabs = el('en-how-tabs');
    if (tabs && !tabs.dataset.enReady) {
      tabs.dataset.enReady = '1';
      tabs.addEventListener('click', function (e) {
        var b = e.target.closest('button'); if (!b) return;
        tabs.querySelectorAll('button').forEach(function (x) { x.classList.toggle('on', x === b); });
        document.querySelectorAll('.en-pane').forEach(function (p) { p.classList.toggle('on', p.dataset.pane === b.dataset.pane); });
      });
    }
    // Enrolled › This week: the chip filters on data-f-week, set here from each row's time.
    var weekAgo = Date.now() / 1000 - 7 * 86400;
    document.querySelectorAll('#en-recent tr[data-ts]').forEach(function (r) { r.setAttribute('data-f-week', (+r.dataset.ts || 0) >= weekAgo ? '1' : '0'); });
    if (window.ltInit) ltInit();
  }
  function picks() { return [].slice.call(document.querySelectorAll('.en-pick')); }
  function syncProfiles() {
    var form = el('en-prof-bulk'), n = el('en-prof-n'); if (!form) return;
    var on = picks().filter(function (c) { return c.checked; }).length;
    if (n) n.textContent = on ? on + ' selected' : 'none selected';
    form.querySelectorAll('button').forEach(function (b) { b.disabled = !on; });
  }

  if (window.__enOnce) { init(); return; }
  window.__enOnce = true;

  window.enCopy = function (btn, text) {
    var done = function () { var o = btn.textContent; btn.textContent = 'Copied'; setTimeout(function () { btn.textContent = o; }, 1200); };
    if (navigator.clipboard && navigator.clipboard.writeText) { navigator.clipboard.writeText(text).then(done, function () { fallback(); }); } else fallback();
    function fallback() { var t = document.createElement('textarea'); t.value = text; document.body.appendChild(t); t.select(); try { document.execCommand('copy'); } catch (e) {} t.remove(); done(); }
  };
  window.enBulkConfirm = function () {
    var a = document.activeElement && document.activeElement.value;
    if (a === 'delete') { var k = picks().filter(function (c) { return c.checked; }).length; return confirm('Delete ' + k + ' profile' + (k === 1 ? '' : 's') + '? Devices they enrolled keep working.'); }
    return true;
  };
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
    var m = el('en-modal'), f = el('en-modal-form'); if (!m || !f) return;
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
  // One ⋯ menu open at a time; a click anywhere else closes it.
  document.addEventListener('click', function (e) {
    document.querySelectorAll('details.en-kebab[open]').forEach(function (d) { if (!d.contains(e.target)) d.removeAttribute('open'); });
  });
  document.addEventListener('toggle', function (e) {
    var d = e.target; if (!d || !d.classList || !d.classList.contains('en-kebab') || !d.open) return;
    document.querySelectorAll('details.en-kebab[open]').forEach(function (o) { if (o !== d) o.removeAttribute('open'); });
  }, true);
  document.addEventListener('change', function (e) { if (e.target.classList && e.target.classList.contains('en-pick')) syncProfiles(); });
  // A pane swapped in (Setup's left list) gets its wiring.
  document.addEventListener('htmx:afterSettle', function (e) {
    var t = e.detail && e.detail.target;
    if (t && (t.id === 'oh-pane' || t.tagName === 'MAIN')) init();
  });
  init();
})();
