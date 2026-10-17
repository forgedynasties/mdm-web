// Loaded by templates/export.html.
function setRange(btn, hours) {
  var now = new Date();
  var start = new Date(now.getTime() - hours * 3600 * 1000);
  document.getElementById('export-start').value = toLocalISO(start);
  document.getElementById('export-end').value = toLocalISO(now);
  document.getElementById('export-range-btns').querySelectorAll('.dur-btn').forEach(function(b) {
    b.classList.remove('active');
  });
  if (btn) btn.classList.add('active');
  document.getElementById('range-summary').textContent = humanRange(hours);
  document.getElementById('summary-range').textContent = humanRange(hours);
}

function humanRange(hours) {
  if (hours < 24) return 'Last ' + hours + ' hour' + (hours === 1 ? '' : 's');
  var days = hours / 24;
  if (days < 7) return 'Last ' + days + ' day' + (days === 1 ? '' : 's');
  if (days % 7 === 0) return 'Last ' + (days/7) + ' week' + (days/7 === 1 ? '' : 's');
  return 'Last ' + days + ' days';
}

function onTimeChange() {
  // User edited inputs directly — clear quick-pick highlight and show "Custom".
  document.getElementById('export-range-btns').querySelectorAll('.dur-btn').forEach(function(b) {
    b.classList.remove('active');
  });
  var s = document.getElementById('export-start').value;
  var e = document.getElementById('export-end').value;
  var label = 'Custom range';
  if (s && e) {
    var diffMs = new Date(e).getTime() - new Date(s).getTime();
    if (diffMs > 0) {
      var hours = Math.round(diffMs / 3600000);
      label = humanRange(hours);
    }
  }
  document.getElementById('range-summary').textContent = label;
  document.getElementById('summary-range').textContent = label;
}

function toLocalISO(d) {
  var pad = function(n) { return n < 10 ? '0' + n : n; };
  return d.getFullYear() + '-' + pad(d.getMonth()+1) + '-' + pad(d.getDate()) + 'T' + pad(d.getHours()) + ':' + pad(d.getMinutes());
}

function toggleSerials(btn) {
  var wrap = document.getElementById('export-serials');
  var collapsed = wrap.getAttribute('data-collapsed') === '1';
  wrap.setAttribute('data-collapsed', collapsed ? '0' : '1');
  wrap.querySelectorAll('.serial-chip').forEach(function(chip, i) {
    if (collapsed) {
      chip.classList.remove('serial-chip-hidden');
    } else if (i >= 10) {
      chip.classList.add('serial-chip-hidden');
    }
  });
  btn.textContent = collapsed ? 'Show fewer' : 'Show all';
}

// The Download CSV button intentionally does NOT disable itself or show a "preparing…"
// state on submit. A file download fires no JS completion event, so any such state has
// to be guessed back off (a cookie round-trip, a timeout, etc.) — every version of that
// has ended up with the button stuck disabled on some real-world timing it didn't
// account for. Left static/always-clickable instead: the browser's own download
// indicator (and, for a huge export, "why is nothing happening" is on the browser, not
// this page) is the only completion signal, and clicking it twice just requests the
// file twice — harmless.

// Default: last 48 hours. Columns and sampling are fixed, so there is nothing else
// to initialise.
setRange(document.querySelector('#export-range-btns .dur-btn[data-hours="48"]'), 48);
