// Loaded by templates/device.html.
// The hero's Online/Offline badge is the live one (updated over the socket); the island
// repeats it, so keep the island's dot and word in step with it.
(function () {
  var b = document.getElementById('ws-badge'); if (!b) return;
  function sync() {
    var on = b.classList.contains('on');
    var d = document.querySelector('#appbar-island .isl-dot'), w = document.querySelector('#appbar-island .isl-on');
    if (d) { d.classList.toggle('on', on); d.classList.toggle('off', !on); }
    if (w) w.textContent = on ? 'online' : 'offline';
  }
  new MutationObserver(sync).observe(b, { attributes: true, attributeFilter: ['class'] });
  setTimeout(sync, 0);
})();
