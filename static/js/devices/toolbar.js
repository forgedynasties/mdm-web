// Loaded by templates/devices.html.
// ── Roster toolbar: inline rename (group·venue) in place of the title ───────────
function collRenameOpen() {
  var title = document.getElementById('roster-title');
  var tools = document.querySelector('.coll-tools');
  var form = document.getElementById('coll-rename');
  if (!form || !title) return;
  if (tools) tools.style.display = 'none';
  title.style.display = 'none';
  title.insertAdjacentElement('afterend', form);
  form.style.display = 'inline-flex';
  var inp = form.querySelector('input[name="name"]');
  if (inp) { inp.focus(); inp.select(); }
}
function collRenameClose() {
  var title = document.getElementById('roster-title');
  var form = document.getElementById('coll-rename');
  var tools = document.querySelector('.coll-tools');
  if (form) form.style.display = 'none';
  if (title) title.style.display = '';
  if (tools) tools.style.display = 'inline-flex';
}

// ── Fleet-page devices popup: load a group/venue's add-devices+members section ──
function openFleetDevicesModal(url, title) {
  var m = document.getElementById('fleet-devices-modal');
  var body = document.getElementById('fleet-devices-modal-body');
  var head = document.getElementById('fleet-devices-modal-title');
  if (!m || !body) return;
  if (head && title) head.textContent = title;
  body.innerHTML = '<div class="muted small" style="padding:20px;">Loading…</div>';
  m.classList.add('modal-open');
  if (window.htmx) {
    htmx.ajax('GET', url, { target: '#fleet-devices-modal-body', swap: 'innerHTML' });
  } else {
    window.location.href = url;
  }
}
function closeFleetDevicesModal(event) {
  if (event && event.target !== document.getElementById('fleet-devices-modal')) return;
  var m = document.getElementById('fleet-devices-modal');
  if (!m) return;
  m.classList.remove('modal-open');
  document.getElementById('fleet-devices-modal-body').innerHTML = '';
}

// ── Roster toolbar kebab: open group/venue, rename, or manage devices ───────────
function collMenuClose() {
  var m = document.querySelector('.coll-menu:not([hidden])');
  if (!m) return;
  m.setAttribute('hidden', '');
  var btn = m.previousElementSibling;
  if (btn) btn.setAttribute('aria-expanded', 'false');
}
function collMenuToggle(e) {
  e.preventDefault();
  e.stopPropagation();
  var btn = e.currentTarget;
  var menu = btn.nextElementSibling;
  if (!menu) return;
  var willOpen = menu.hasAttribute('hidden');
  collMenuClose();
  if (willOpen) {
    menu.removeAttribute('hidden');
    btn.setAttribute('aria-expanded', 'true');
  }
}
document.addEventListener('click', function (e) {
  if (!e.target.closest('.coll-menu-wrap')) collMenuClose();
});

document.addEventListener('keydown', function (e) {
  if (e.key === 'Escape') {
    var m = document.getElementById('fleet-devices-modal');
    if (m && m.classList.contains('modal-open')) closeFleetDevicesModal();
    collRenameClose();
    collMenuClose();
  }
});
