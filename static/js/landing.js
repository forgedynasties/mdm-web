// Loaded by templates/landing.html.
    (function () {
      var nav = document.getElementById('nav'), hero = document.getElementById('top');
      // Pill goes light once the hero has scrolled away.
      function onScroll() { nav.classList.toggle('scrolled', window.scrollY > hero.offsetHeight - 100); }
      window.addEventListener('scroll', onScroll, { passive: true }); onScroll();

      // Theme switch (shared key with the dashboard).
      function setTheme(t) { document.documentElement.setAttribute('data-theme', t); try { localStorage.setItem('mdm-theme', t); } catch (e) {} }
      document.querySelectorAll('#theme-btn, .js-theme').forEach(function (b) { b.addEventListener('click', function () { setTheme(document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark'); }); });

      document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { closeSheet(); ldClose(); } });

      // Mobile sheet
      var sheet = document.getElementById('sheet');
      function closeSheet() { sheet.classList.remove('open'); }
      document.getElementById('burger').addEventListener('click', function () { sheet.classList.add('open'); });
      document.getElementById('sheet-close').addEventListener('click', closeSheet);
      sheet.addEventListener('click', function (e) { if (e.target === sheet || e.target.closest('a.item')) closeSheet(); });
      window.closeSheet = closeSheet;
    })();
    function ldPlay(btn) {
      var p = document.getElementById('ld-player'), v = document.getElementById('ld-player-video'); if (!p) return;
      document.getElementById('ld-player-title').textContent = btn.getAttribute('data-title');
      v.src = btn.getAttribute('data-src'); p.hidden = false; v.play().catch(function () {});
    }
    function ldClose() { var p = document.getElementById('ld-player'), v = document.getElementById('ld-player-video'); if (!p || p.hidden) return; v.pause(); v.removeAttribute('src'); v.load(); p.hidden = true; }
  
