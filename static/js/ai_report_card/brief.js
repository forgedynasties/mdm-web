// Loaded by templates/ai_report_card.html.
  // Collapse to the first three call-outs; the toggle appears only when there are more.
  (function () {
    var card = document.getElementById('ai-summary-card'), full = document.getElementById('ai-sum-full'), tg = document.getElementById('ov3-brief-toggle');
    if (!card || !full || !tg) return;
    function sync() {
      var n = full.querySelectorAll('.rep-row').length;
      tg.hidden = n <= 3;
      tg.textContent = card.classList.contains('open') ? 'Show less' : 'Show ' + (n - 3) + ' more';
    }
    tg.addEventListener('click', function () { card.classList.toggle('open'); sync(); });
    new MutationObserver(sync).observe(full, { childList: true, subtree: true });
    sync();
  })();
  
