// Loaded by templates/compliance_override.html.
(function () {
  var box = document.getElementById('co-scope');
  function sync() {
    var on = box.querySelector('input:checked'), v = on ? on.value : '';
    document.querySelectorAll('.co-pick').forEach(function (s) { s.hidden = s.getAttribute('data-for') !== v; });
  }
  box.addEventListener('change', sync); sync();
})();
