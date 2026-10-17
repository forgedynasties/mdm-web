// Loaded by templates/command_detail.html.
(function () {
  if (!window.EventSource) return;
  var es = window.mdmStream('/commands/' + window.COMMAND_PAGE.id + '/events');
  es.addEventListener('command-update', function () { document.body.dispatchEvent(new CustomEvent('command-updated')); });
  // Keep the headline percent / segment bar in step with the deliveries card.
  document.body.addEventListener('htmx:afterSwap', function (e) {
    var t = e.target; if (!t || t.id !== 'deliveries-card') return;
    var src = t.querySelector('#ad-stats-src') || document.getElementById('ad-stats-src');
    var dst = document.getElementById('ad-stats');
    if (src && dst) dst.innerHTML = src.innerHTML;
  });
  window.addEventListener('beforeunload', function () { es.close(); });
})();
(function () {
  function fmt(ms){var d=(Date.now()-ms)/1000;if(d<60)return 'just now';if(d<3600)return Math.floor(d/60)+'m ago';if(d<86400)return Math.floor(d/3600)+'h ago';return Math.floor(d/86400)+'d ago';}
  function tick(){document.querySelectorAll('[data-since-ms]').forEach(function(el){var ms=parseInt(el.dataset.sinceMs,10);if(!isNaN(ms))el.textContent=fmt(ms);});}
  tick();setInterval(tick,30000);
})();
