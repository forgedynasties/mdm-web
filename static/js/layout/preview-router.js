// Loaded by templates/layout.html.
      // Preview navigation router: dock/nav links to real app pages are remapped to
      // their in-preview dummy equivalents; anything without a dummy falls back to
      // /login (sign in to explore for real) rather than dead-ending mid-click.
      (function () {
        // No session, so no live streams: a page script's mdmStream (the Fleet list's
        // /events/devices, for one) gets an inert stand-in instead of a 401.
        window.mdmStream = function () {
          return { addEventListener: function () {}, removeEventListener: function () {}, close: function () {} };
        };
        var MAP = { '/': '/sneak-peek', '/devices': '/sneak-peek/fleet', '/alerts': '/sneak-peek/alerts', '/fleet-health': '/sneak-peek' };
        document.addEventListener('click', function (e) {
          var a = e.target.closest && e.target.closest('a[href]');
          if (!a) return;
          var href = a.getAttribute('href') || '';
          if (href.charAt(0) !== '/') return;                 // external / anchor
          if (href.indexOf('/sneak-peek') === 0 || href === '/login' || a.hasAttribute('data-preview-exit')) return; // already fine
          e.preventDefault(); e.stopPropagation();
          var path = href.split('?')[0].split('#')[0];
          window.location.href = MAP[path] || '/login';
        }, true);
        // The dock marks the page it is on from location, which is /sneak-peek…, so
        // mark the dock entry whose preview page this is (Overview, Fleet) instead.
        window.addEventListener('load', function () {
          document.querySelectorAll('nav.dock a.nav-link[href]').forEach(function (a) {
            a.classList.toggle('active', MAP[a.getAttribute('href')] === location.pathname && a.getAttribute('href') !== '/fleet-health');
          });
        });
      })();
    
