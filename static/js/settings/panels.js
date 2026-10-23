// Loaded by templates/settings.html.
// Settings tabs.
(function () {
  var nav = document.getElementById('settings-nav');
  if (!nav) return;
  var btns = nav.querySelectorAll('button');
  var panels = document.querySelectorAll('.settings-tab');
  function show(key) {
    var found = false;
    panels.forEach(function (p) { var on = p.getAttribute('data-tab') === key; p.classList.toggle('active', on); if (on) found = true; });
    btns.forEach(function (b) { b.classList.toggle('active', b.getAttribute('data-tab') === key); });
    return found;
  }
  btns.forEach(function (b) {
    b.addEventListener('click', function () { var k = b.getAttribute('data-tab'); if (show(k)) history.replaceState(null, '', '#' + k); });
  });
  var initial = (location.hash || '').replace('#', '');
  // The App library tab moved out (23 Oct 2026): /apps is the one library.
  if (initial === 'applibrary') { location.replace('/apps'); return; }
  // Enrollment's cards (QR provisioning APK, adb keys) moved to Enrollment (23 Oct 2026).
  if (initial === 'enrollment' || initial === 'fleet-adb-key') { location.replace('/enrollment?tab=how' + (initial === 'fleet-adb-key' ? '#fleet-adb-key' : '')); return; }
  // legacy deep-links from older tabs
  var alias = { security: 'people', sessions: 'people', tools: 'devices', testing: 'general', display: 'general', signin: 'people' };
  // Cards that moved to their own pages (S1, 24 Oct 2026).
  var moved = { ota: '/releases/settings', alerts: '/alert-config', channels: '/alert-config#channels', commands: '/jobs#job-settings', integrations: '/server#integrations', ai: '/server#integrations', google: '/server#integrations' };
  if (moved[initial]) { location.replace(moved[initial]); return; }
  if (alias[initial]) initial = alias[initial];
  // A hash may name a card inside a tab (#fleet-adb-key): open its tab, then scroll to it.
  function showFor(k) {
    if (!k) return false;
    if (show(k)) return true;
    var el = document.getElementById(k);
    var tab = el && el.closest('.settings-tab');
    if (!tab) return false;
    show(tab.getAttribute('data-tab'));
    setTimeout(function () { el.scrollIntoView({ block: 'start' }); }, 0);
    return true;
  }
  if (!showFor(initial)) show('people');
  // in-page links to /settings#tab (and back/forward) switch tabs too
  window.addEventListener('hashchange', function () { var k = (location.hash || '').replace('#', ''); if (alias[k]) k = alias[k]; showFor(k); });
})();

// Alert rules: switch = save immediately; thresholds show a Save button when edited.
// Both post with fetch (the server answers 204 for X-Requested-With: fetch), so
// the page never reloads and you stay on the tab.
function ruleData(form) {
  var fd = new URLSearchParams(); // the POST wrapper parses urlencoded bodies, not multipart
  fd.append('type', form.querySelector('input[name=type]').value);
  if (form.querySelector('.sw-toggle').classList.contains('on')) fd.append('enabled', 'on');
  form.querySelectorAll('.rule-fields input, .rule-fields select').forEach(function (el) { fd.append(el.name, el.value); });
  return fd;
}
function ruleStatus(form, text, err) {
  var st = form.querySelector('.rule-status'); if (!st) return;
  st.textContent = text; st.classList.toggle('err', !!err); st.style.opacity = 1;
  clearTimeout(st._t); if (!err) st._t = setTimeout(function () { st.style.opacity = 0; }, 2200);
}
function ruleCounts() {
  document.querySelectorAll('.alert-cat-panel').forEach(function (panel) {
    var cat = panel.getAttribute('data-cat'), on = panel.querySelectorAll('.rule-item:not(.off)').length, all = panel.querySelectorAll('.rule-item').length;
    var pill = document.querySelector('.alert-cat-tab[data-cat="' + cat + '"] .pill'); if (pill) pill.textContent = on + '/' + all;
  });
}
function ruleSave(form, ev) {
  if (ev) ev.preventDefault();
  var name = form.getAttribute('data-name') || 'Rule';
  fetch(form.action, { method: 'POST', body: ruleData(form), headers: { 'X-Requested-With': 'fetch' }, credentials: 'same-origin' })
    .then(function (r) { if (!r.ok) throw new Error(r.status); ruleStatus(form, 'Saved'); var b = form.querySelector('.rule-save'); if (b) b.hidden = true; if (window.showToast) showToast(name + ' saved', 'success'); })
    .catch(function () { ruleStatus(form, 'Not saved', true); if (window.showToast) showToast('Could not save ' + name, 'error'); });
  return false;
}
function ruleToggle(btn) {
  var form = btn.closest('form'); var on = !btn.classList.contains('on');
  btn.classList.toggle('on', on); btn.setAttribute('aria-pressed', on ? 'true' : 'false'); btn.title = on ? 'Disable' : 'Enable';
  form.classList.toggle('off', !on); ruleCounts(); ruleSave(form);
}
function ruleDirty(el) { var b = el.closest('form').querySelector('.rule-save'); if (b) b.hidden = false; }
// Every other settings form: submit natively (no htmx boost — boosted requests drop
// the #tab fragment) and come back to the tab you were on after saving.
document.querySelectorAll('.settings-panels form:not([hx-post]):not([hx-boost])').forEach(function (f) { f.setAttribute('hx-boost', 'false'); });
document.addEventListener('submit', function (e) {
  var form = e.target; if (!form.closest || !form.closest('.settings-panels') || form.hasAttribute('hx-post') || form.classList.contains('rule-item')) return;
  var inp = form.querySelector('input[name=redirect]'); if (!inp) { inp = document.createElement('input'); inp.type = 'hidden'; inp.name = 'redirect'; form.appendChild(inp); }
  // A card shown on another page (S1) comes back to that page, at the card.
  if (form.closest('.st-moved')) { var card = form.closest('[id]'); inp.value = location.pathname + location.search + (card ? '#' + card.id : ''); return; }
  var active = document.querySelector('.settings-nav button.active'); var tab = active ? active.getAttribute('data-tab') : 'general';
  if (!inp.value || inp.value.indexOf('/settings') === 0) inp.value = '/settings#' + tab;
}, true);

// Alert-rule category sub-tabs (within the Alerts tab).
(function () {
  var tabs = document.getElementById('alert-cat-tabs');
  if (!tabs) return;
  var btns = tabs.querySelectorAll('.alert-cat-tab');
  var panels = document.querySelectorAll('.alert-cat-panel');
  btns.forEach(function (b) {
    b.addEventListener('click', function () {
      var k = b.getAttribute('data-cat');
      btns.forEach(function (x) { x.classList.toggle('active', x === b); });
      panels.forEach(function (p) { p.classList.toggle('active', p.getAttribute('data-cat') === k); });
    });
  });
})();

// People & sign-in (S1): a switch row's On/Off word follows its switch after the
// in-place toggle swaps the button.
document.body.addEventListener('htmx:afterSettle', function () {
  document.querySelectorAll('.s1-sw').forEach(function (row) {
    var st = row.querySelector('.s1-st'); if (st) st.textContent = row.querySelector('.sw-toggle.on') ? 'On' : 'Off';
  });
});
