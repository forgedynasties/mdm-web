// Loaded by templates/layout.html.
      // Preview navigation router: dock/nav links to real app pages are remapped to
      // their in-preview dummy equivalents; anything without a dummy falls back to
      // /login (sign in to explore for real) rather than dead-ending mid-click.
      (function () {
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
      })();
    
