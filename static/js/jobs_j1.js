// Loaded by templates/jobs.html (J1, 21 Oct 2026).
// Filter pills: State, Type and Sent by filter the job rows on the page. The live list
// re-renders every few seconds, so the filter is kept here and applied again after each
// swap. Group and day headers with no row left are hidden too.
(function () {
  if (!window.__jzFilter) window.__jzFilter = { state: '', type: '', by: '' };
  var f = window.__jzFilter;

  function apply() {
    var list = document.getElementById('cmd-timeline');
    if (!list) return;
    var on = f.state || f.type || f.by, shown = 0, total = 0;
    var kids = Array.prototype.slice.call(list.children);
    kids.forEach(function (el) {
      if (!el.classList.contains('htrow')) return;
      total++;
      var ok = (!f.state || el.dataset.bucket === f.state) &&
               (!f.type || el.dataset.cmdType === f.type) &&
               (!f.by || el.dataset.by === f.by);
      el.hidden = !ok;
      if (ok) shown++;
    });
    // A header stays when a visible row follows it before the next header of its kind
    // (a day header ends at the next day or group header; a group header at the next group).
    kids.forEach(function (el, i) {
      var grp = el.classList.contains('hgrp-h'), day = el.classList.contains('hist-day');
      var more = el.classList.contains('hmore');
      if (!grp && !day && !more) return;
      if (more) { el.hidden = !!on; return; }
      var any = false;
      for (var j = i + 1; j < kids.length; j++) {
        var k = kids[j];
        if (k.classList.contains('hgrp-h') || (day && k.classList.contains('hist-day'))) break;
        if (k.classList.contains('htrow') && !k.hidden) { any = true; break; }
      }
      el.hidden = on ? !any : false;
    });
    var none = document.getElementById('jz-none');
    if (none) none.hidden = !(on && total > 0 && shown === 0);
    var cnt = document.getElementById('jz-count');
    if (cnt) {
      cnt.hidden = !on;
      cnt.innerHTML = '<b>' + shown + '</b> of ' + total + ' jobs on this page';
    }
  }

  function syncPills() {
    document.querySelectorAll('[data-jz-pill]').forEach(function (b) {
      var key = b.dataset.jzPill, val = f[key];
      var item = document.querySelector('[data-jz-set="' + key + '"][data-val="' + (window.CSS && CSS.escape ? CSS.escape(val) : val) + '"]');
      var lbl = b.querySelector('b');
      if (lbl && item) lbl.textContent = item.textContent;
      b.classList.toggle('set', !!val);
      document.querySelectorAll('[data-jz-set="' + key + '"]').forEach(function (i) {
        i.classList.toggle('active', i.dataset.val === val);
      });
    });
  }

  if (!window.__jzBound) {
    window.__jzBound = true;
    document.addEventListener('click', function (e) {
      var b = e.target.closest('[data-jz-set]');
      if (!b) return;
      f[b.dataset.jzSet] = b.dataset.val;
      syncPills();
      apply();
      if (window.ocCloseMenus) window.ocCloseMenus();
    });
    document.addEventListener('htmx:afterSwap', function (e) {
      var t = e.detail && e.detail.target;
      if (t && (t.id === 'cmd-timeline' || t.tagName === 'MAIN')) { syncPills(); apply(); }
    });
  }
  syncPills();
  apply();
})();
