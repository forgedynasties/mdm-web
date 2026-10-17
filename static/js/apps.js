// Loaded by templates/apps.html.
// Search filters every card and tile in place; a shelf with no matches says so rather than
// collapsing to a mystery gap.
function stFilter(q) {
  q = (q || '').trim().toLowerCase();
  document.querySelectorAll('[data-card]').forEach(function (c) {
    var hay = (c.getAttribute('data-search') || '').toLowerCase();
    c.hidden = q !== '' && hay.indexOf(q) === -1;
  });
  document.querySelectorAll('[data-shelf]').forEach(function (s) {
    var any = Array.prototype.some.call(s.querySelectorAll('[data-card]'), function (c) { return !c.hidden; });
    var empty = s.querySelector('[data-shelf-empty]');
    if (empty) empty.hidden = any;
  });
  // The library grid below carries its own searchable tiles (data-app).
  document.querySelectorAll('.lib-tile').forEach(function (t) {
    var hay = (t.getAttribute('data-app') || '').toLowerCase();
    t.style.display = (q === '' || hay.indexOf(q) !== -1) ? '' : 'none';
  });
}
document.querySelectorAll('.st-chip').forEach(function (a) {
  a.addEventListener('click', function () {
    document.querySelectorAll('.st-chip').forEach(function (x) { x.classList.remove('on'); });
    a.classList.add('on');
  });
});
