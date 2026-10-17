// Loaded by templates/wrapped.html.
(function () {
  var root = document.getElementById('wr');
  if (!root) return;
  var reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  // Progress bar tracks scroll through the deck.
  var prog = document.getElementById('wr-prog');
  root.addEventListener('scroll', function () {
    var max = root.scrollHeight - root.clientHeight;
    prog.style.width = (max > 0 ? (root.scrollTop / max) * 100 : 0) + '%';
  }, { passive: true });

  // Format a number with thousands separators and optional decimals.
  function fmt(n, dec) {
    if (dec) return n.toLocaleString(undefined, { minimumFractionDigits: dec, maximumFractionDigits: dec });
    return Math.round(n).toLocaleString();
  }

  // Count up every [data-count] inside a panel once, when it first appears.
  function runCounts(panel) {
    panel.querySelectorAll('[data-count]').forEach(function (el) {
      if (el.dataset.done) return; el.dataset.done = '1';
      var target = parseFloat(el.dataset.count) || 0;
      var dec = parseInt(el.dataset.dec || '0', 10);
      var unit = el.querySelector('.u'); // preserve a trailing unit span (days, °C)
      var setText = function (v) {
        if (unit) { el.firstChild.nodeValue = fmt(v, dec); }
        else { el.textContent = fmt(v, dec); }
      };
      if (reduce || target === 0) { setText(target); return; }
      var dur = 1300, t0 = null;
      function step(ts) {
        if (t0 === null) t0 = ts;
        var p = Math.min((ts - t0) / dur, 1);
        var e = 1 - Math.pow(1 - p, 3); // easeOutCubic
        setText(target * e);
        if (p < 1) requestAnimationFrame(step); else setText(target);
      }
      requestAnimationFrame(step);
    });
  }

  var panels = root.querySelectorAll('[data-panel]');
  if (!reduce) root.classList.add('anim'); // enable entrance animation only with JS

  // Reveal any panel whose center is within the viewport. A plain scroll check is
  // used instead of IntersectionObserver, which proved unreliable inside a fixed,
  // scroll-snapped container — a missed observer would leave a panel blank.
  function reveal() {
    var vh = root.clientHeight;
    panels.forEach(function (p) {
      if (p.classList.contains('in')) return;
      var r = p.getBoundingClientRect();
      if (r.top < vh * 0.62 && r.bottom > vh * 0.38) {
        p.classList.add('in');
        runCounts(p);
      }
    });
  }
  var ticking = false;
  root.addEventListener('scroll', function () {
    if (ticking) return; ticking = true;
    requestAnimationFrame(function () { reveal(); ticking = false; });
  }, { passive: true });
  reveal(); // reveal whatever's on screen at load
  // Safety net: the scroll event alone can miss a panel if layout settles late.
  var revealTimer = setInterval(function () {
    reveal();
    var all = Array.prototype.every.call(panels, function (p) { return p.classList.contains('in'); });
    if (all) { clearInterval(revealTimer); }
  }, 200);

  // Esc closes the experience.
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') { var a = root.querySelector('.wr-close'); if (a) a.click(); }
  });
})();
