// Loaded by templates/export.html.
// Headline + summary follow the picker's basket, and a ?serials= arrival starts
// with those devices already chosen.
(function () {
  var head = document.getElementById('ex-head-count');
  var sum = document.getElementById('summary-devices');
  document.addEventListener('pk:change', function (e) {
    var n = (e.detail && e.detail.serials ? e.detail.serials.length : 0);
    if (head) head.textContent = n ? (n + ' device' + (n === 1 ? '' : 's')) : 'no devices yet';
    if (sum) {
      sum.textContent = n;
      var tail = sum.nextSibling;
      if (tail && tail.nodeType === 3) tail.nodeValue = n === 1 ? ' device' : ' devices';
    }
  });
})();
