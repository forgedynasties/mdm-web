// Loaded by templates/production_form.html.
document.addEventListener('DOMContentLoaded', refreshPreview);
var nameEdited = false;
var B36 = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ';
function encodeBatch(month, year) {
  var val = month * 100 + (year % 100);
  return B36[Math.floor(val / 36)] + B36[val % 36];
}
function pad5(n) { return String(n).padStart(5, '0'); }

function refreshPreview() {
  var p     = document.getElementById('f-product').value.trim().toUpperCase();
  var m     = document.getElementById('f-model').value.trim();
  var v     = document.getElementById('f-variant').value.trim() || '0';
  var s     = document.getElementById('f-sku').value.trim().toUpperCase() || 'AA';
  var month = parseInt(document.getElementById('f-month').value, 10);
  var year  = parseInt(document.getElementById('f-year').value, 10);
  var start = parseInt(document.getElementById('f-start').value, 10);
  var qty   = parseInt(document.getElementById('f-qty').value, 10);
  var el    = document.getElementById('serial-preview');

  if (!p || !m || isNaN(month) || month < 1 || month > 12 ||
      isNaN(year) || year < 0 || isNaN(start) || start < 1 || isNaN(qty) || qty < 1) {
    el.innerHTML = '<span class="muted">Fill in the fields above to see a preview.</span>';
    return;
  }

  // An explicit code wins over the month/year encoding — that is the whole point of
  // the field: an early run whose serials predate the convention.
  var batch  = document.getElementById('f-batch').value.trim().toUpperCase();
  if (batch.length !== 2) batch = encodeBatch(month, year);
  var prefix = p + m + v + s + batch;
  var first  = prefix + pad5(start);
  var last   = prefix + pad5(start + qty - 1);

  if (!nameEdited) {
    var mo = String(month).padStart(2, '0');
    var yr = String(year % 100).padStart(2, '0');
    document.getElementById('f-name').value = p + m + '-prod-' + mo + '-' + yr + '-' + qty + 'pc';
  }

  el.innerHTML =
    '<code class="serial-preview">' + first + '</code>' +
    ' <span class="muted">→</span> ' +
    '<code class="serial-preview">' + last + '</code>' +
    ' &nbsp;<span class="muted small">batch <strong>' + batch + '</strong> &middot; ' + qty.toLocaleString() + ' unit' + (qty !== 1 ? 's' : '') + '</span>';
}
