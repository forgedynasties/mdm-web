// Loaded by templates/settings.html.
  // Per-channel alert-type filter: keep each category's master checkbox in sync with
  // its children (checked / unchecked / indeterminate), and let it toggle them.
  function chanTypeKids(group) { return group.querySelectorAll('input[name="alert_types"]'); }
  function chanRefreshCat(group) {
    var cat = group.querySelector('.chan-type-cat');
    if (!cat) return;
    var kids = chanTypeKids(group), on = 0;
    kids.forEach(function (c) { if (c.checked) on++; });
    cat.checked = on === kids.length;
    cat.indeterminate = on > 0 && on < kids.length;
  }
  function chanToggleCat(cat) {
    var group = cat.closest('.chan-type-group');
    chanTypeKids(group).forEach(function (c) { c.checked = cat.checked; });
    cat.indeterminate = false;
  }
  function chanSyncCat(kid) { chanRefreshCat(kid.closest('.chan-type-group')); }
  document.addEventListener('DOMContentLoaded', function () {
    document.querySelectorAll('.chan-type-group').forEach(chanRefreshCat);
  });
