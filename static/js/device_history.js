// Loaded by templates/device.html.
function toggleSection(header) {
  var body = header.nextElementSibling;
  var open = body.style.display !== 'none';
  if (open) {
    body.style.display = 'none';
    header.classList.remove('open');
    header.setAttribute('aria-expanded', 'false');
  } else {
    body.style.display = '';
    header.classList.add('open');
    header.setAttribute('aria-expanded', 'true');
    var card = header.closest('.card');
    if (card && window.htmx && card.hasAttribute('hx-trigger')) {
      window.htmx.trigger(card, 'load');
    }
  }
}

function showCheckin(row) {
  var checkinTime = new Date(row.dataset.time);
  document.getElementById('modal-time').textContent    = isNaN(checkinTime.getTime()) ? row.dataset.time : checkinTime.toLocaleString([], window.tzArg({}));
  document.getElementById('modal-battery').textContent = row.dataset.battery + '%';
  document.getElementById('modal-build').textContent   = row.dataset.build;
  try {
    document.getElementById('modal-extra').textContent = JSON.stringify(JSON.parse(row.dataset.extra), null, 2);
  } catch(_) {
    document.getElementById('modal-extra').textContent = row.dataset.extra;
  }
  document.getElementById('checkin-modal').classList.add('modal-open');
}
function closeModal(e) {
  if (e.target === document.getElementById('checkin-modal')) {
    document.getElementById('checkin-modal').classList.remove('modal-open');
  }
}
document.addEventListener('keydown', function (e) {
  if (e.key === 'Escape') document.getElementById('checkin-modal').classList.remove('modal-open');
});
