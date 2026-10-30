// Loaded by templates/layout.html.
  // Top-bar dropdowns (alerts bell + account menu). One open at a time; close on
  // outside-click / Escape. The alerts list lazy-loads from /alerts/recent each open.
  (function () {
    // .appbar carries its own backdrop-filter (the sticky header's frosted look),
    // which makes it a "backdrop root" for descendants — a dropdown's OWN
    // backdrop-filter can then only sample the header's already-composited layer,
    // not the real page behind it, so its glass blur renders as if it weren't
    // there at all. Portal the popup out to <body> (position:fixed, reachable via
    // its own id regardless of DOM parent) so its blur samples the actual page.
    // Portal both popups to <body> up front (not lazily on first open) — moving a
    // backdrop-filter element to a new parent makes the browser drop and re-promote
    // its compositing layer, which shows up as a one-frame flicker (blur cuts out
    // then back in) if done while the popup is becoming visible. Doing it once,
    // hidden, before any open avoids that entirely.
    ['alerts-pop', 'acct-pop'].forEach(function (id) {
      var p = document.getElementById(id);
      if (p && p.parentElement !== document.body) document.body.appendChild(p);
    });
    function pop(id, btnSel, open) {
      var p = document.getElementById(id);
      if (!p) return;
      if (open) {
        if (p.parentElement !== document.body) document.body.appendChild(p);
        var b = document.querySelector(btnSel);
        if (b) {
          var r = b.getBoundingClientRect();
          p.style.position = 'fixed';
          // Both remaining popups hang off the top bar, so they open downward
          // and right-aligned. (The dock's More used to open upward from here;
          // it opens the command palette now and needs no placement.)
          // Open away from the edge the button is nearest. The account chip moved into
          // the rail foot, where below-and-right put the menu off the bottom of the
          // screen — it opened, you just could not see it.
          var up = r.bottom > window.innerHeight * 0.6;
          var leftish = r.left < window.innerWidth * 0.5;
          p.style.top = up ? 'auto' : (r.bottom + 9) + 'px';
          p.style.bottom = up ? (window.innerHeight - r.top + 9) + 'px' : 'auto';
          p.style.left = leftish ? Math.max(8, r.left) + 'px' : 'auto';
          p.style.right = leftish ? 'auto' : (window.innerWidth - r.right) + 'px';
        }
        p.removeAttribute('hidden');
      } else {
        p.setAttribute('hidden', '');
      }
      var b = document.querySelector(btnSel);
      if (b) b.setAttribute('aria-expanded', open ? 'true' : 'false');
    }
    window.navAcct = function (e) {
      if (e) { e.preventDefault(); e.stopPropagation(); }
      var willOpen = document.getElementById('acct-pop').hasAttribute('hidden');
      pop('alerts-pop', '#alerts-indicator', false);
      pop('acct-pop', '.appbar-acct', willOpen);
    };
    // Load the alerts list in the background (page load + whenever the count
    // changes elsewhere), never at the moment of opening — refreshAlertsList
    // refuses to touch #alerts-pop-body while .nbar-pop is visible. The
    // account dropdown never flickered because it never mutates its content
    // after open; fetching-on-open was exactly what made the alerts one
    // different (and the only one that flickered). Matching that behavior —
    // the popover's content is always already there before you click — fixes
    // it at the source instead of fighting the resulting repaint.
    var alertsListReady = false;
    // Sneak peek has no session: /alerts/recent would answer with a redirect to /login.
    var preview = location.pathname.indexOf('/sneak-peek') === 0;
    window.refreshAlertsList = function () {
      var p = document.getElementById('alerts-pop');
      if (!p || preview) return;
      if (!p.hasAttribute('hidden')) return; // never mutate while visible
      if (!window.htmx) return;
      htmx.ajax('GET', '/alerts/recent', { target: '#alerts-pop-body', swap: 'innerHTML' }).then(function () {
        alertsListReady = true;
      });
    };
    window.navAlerts = function (e) {
      if (e) { e.preventDefault(); e.stopPropagation(); }
      var willOpen = document.getElementById('alerts-pop').hasAttribute('hidden');
      pop('acct-pop', '.appbar-acct', false);
      pop('alerts-pop', '#alerts-indicator', willOpen);
      // Fallback only: the background load (below) should have already
      // populated this before anyone clicks. If it somehow hasn't (fired
      // before htmx was ready, or the very first paint), fetch now rather
      // than show a permanently empty list.
      if (willOpen && !alertsListReady && !preview && window.htmx) {
        htmx.ajax('GET', '/alerts/recent', { target: '#alerts-pop-body', swap: 'innerHTML' }).then(function () {
          alertsListReady = true;
        });
      }
    };
    // htmx.min.js loads synchronously in <head>, so it's already available here.
    refreshAlertsList();
    document.addEventListener('click', function (e) {
      // The popup itself now lives outside its trigger's wrap (portaled to <body>
      // on open — see pop() above), so "outside" means outside BOTH the trigger
      // wrap and the popup, not just the wrap.
      var aw = document.getElementById('acct-wrap'), ap = document.getElementById('acct-pop');
      if (!(aw && aw.contains(e.target)) && !(ap && ap.contains(e.target))) pop('acct-pop', '.appbar-acct', false);
      var lw = document.getElementById('alerts-wrap'), lp = document.getElementById('alerts-pop');
      if (!(lw && lw.contains(e.target)) && !(lp && lp.contains(e.target))) pop('alerts-pop', '#alerts-indicator', false);
    });
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') {
        pop('acct-pop', '.appbar-acct', false); pop('alerts-pop', '#alerts-indicator', false);
      }
    });
  })();

  // ── Recently visited pages ───────────────────────────────────────────────────
  // Only the palette's empty state uses this, and only as a hint, so per-browser
  // is fine. It is namespaced by account: two people on one browser must not read
  // each other's history back out of the palette.
  //
  // The dock's three shortcuts are NOT this. They come from the server, computed
  // per account from the page-view audit — see #dock-frequent above and
  // frequentPaths() in handlers.go. A browser-local counter was the first attempt
  // and it was wrong twice over: localStorage is per browser, not per account, so
  // two users sharing a machine shared one dock; and a fresh browser had no
  // history at all, so the feature showed nothing on the day it shipped.
  (function () {
    var who = document.body.getAttribute('data-user') || '-';
    var RECENT_KEY = 'mdm-recent-pages:' + who;
    window.mdmRecentPages = function () {
      try { return JSON.parse(localStorage.getItem(RECENT_KEY) || '[]') || []; } catch (e) { return []; }
    };
    window.mdmNoteVisit = function (url, label) {
      if (!url || url === '/') return;
      try {
        var list = window.mdmRecentPages().filter(function (r) { return r.url !== url; });
        list.unshift({ url: url, label: label });
        localStorage.setItem(RECENT_KEY, JSON.stringify(list.slice(0, 10)));
      } catch (e) {}
    };
    window.mdmNoteVisit(location.pathname, (document.title || '').split(' \u2014 ')[0].trim());
  })();

  // The dock is outside <main>, and a boosted navigation only swaps <main> — so
  // nothing re-renders it and its active state (and the admin entry promoted out of
  // More) stayed on whatever the last full page load produced. Re-derive both from the
  // URL after every swap and on back/forward.
  (function () {
    // Pages that belong to a dock entry without sharing its path.
    var GROUPS = {
      // Venues has its own dock entry now, so /restaurants is not Fleet's.
      '/devices': ['/groups', '/enrollment'],
      '/releases': ['/rollouts', '/updates', '/packages'],
    };
    function owns(href, path) {
      href = (href || '').split('?')[0]; // Engineers' Lab is /devices?state=lab
      if (href === '/') return path === '/';
      if (path === href || path.indexOf(href + '/') === 0) return true;
      var extra = GROUPS[href] || [];
      for (var i = 0; i < extra.length; i++) {
        if (path === extra[i] || path.indexOf(extra[i] + '/') === 0) return true;
      }
      return false;
    }
    // ── Your three ────────────────────────────────────────────────────────────
    // The dock's four are the four everyone needs. These are the three THIS
    // ACCOUNT opens most, computed server-side from the page-view audit and
    // handed over in #dock-frequent, rendered just before More.
    //
    // Two things keep it from being a slot machine. The four fixed destinations
    // and the currently-promoted page are excluded, so the dock never shows one
    // place twice. And the label and href are resolved through #page-index —
    // which carries the server's role guards — so a path this role cannot open
    // renders nothing, whatever the history says.
    // ── Icons for the injected dock links ─────────────────────────────────────
    // The page index is text-only (it was never meant to be seen), so the links
    // this file injects into the dock — the three shortcuts and the promoted
    // admin page — have no icon to clone. Each index entry carries a data-ic key
    // instead, and the shapes live here once rather than as 28 inline SVGs in
    // markup that ships on every request.
    var DOCK_ICONS = {
      grid:     '<rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/>',
      building: '<path d="M3 21h18"/><path d="M5 21V7l7-4 7 4v14"/><path d="M10 21v-6h4v6"/>',
      layers:   '<path d="M12 3 3 8l9 5 9-5z"/><path d="M3 13l9 5 9-5"/>',
      pin:      '<path d="M20 10c0 6-8 12-8 12s-8-6-8-12a8 8 0 0 1 16 0z"/><circle cx="12" cy="10" r="3"/>',
      pulse:    '<path d="M3 12h4l3 8 4-16 3 8h4"/>',
      send:     '<path d="M21 3 10.2 13.8"/><path d="M21 3l-6.5 18-4.3-7.2L3 9.5z"/>',
      phone:    '<rect x="7" y="2" width="10" height="20" rx="2"/><path d="M10.5 18.5h3"/>',
      download: '<path d="M12 4v12"/><path d="M7 11l5 5 5-5"/><path d="M5 20h14"/>',
      tag:      '<path d="M20.6 13.4 13.4 20.6a2 2 0 0 1-2.8 0L2 12V2h10l8.6 8.6a2 2 0 0 1 0 2.8z"/><circle cx="7" cy="7" r="1.4"/>',
      image:    '<rect x="3" y="5" width="18" height="14" rx="2"/><circle cx="8.5" cy="10" r="1.5"/><path d="M21 16l-5-4-4 3"/>',
      clock:    '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
      lock:     '<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/>',
      shield:   '<path d="M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6z"/><path d="M9 12l2 2 4-4"/>',
      wifi:     '<path d="M5 12.5a10 10 0 0 1 14 0"/><path d="M8.5 16a5.5 5.5 0 0 1 7 0"/><path d="M12 19.5h.01"/>',
      sliders:  '<path d="M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3"/><path d="M1 14h6M9 8h6M17 16h6"/>',
      users:    '<path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 0 0-3-3.87"/>',
      monitor:  '<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/>',
      server:   '<rect x="3" y="4" width="18" height="7" rx="2"/><rect x="3" y="13" width="18" height="7" rx="2"/><path d="M7 8h.01M7 17h.01"/>',
      gear:     '<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.2a1.7 1.7 0 0 0-1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.2a1.7 1.7 0 0 0 1.5-1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3h.1a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.2a1.7 1.7 0 0 0 1 1.5h.1a1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8z"/>',
      bell:     '<path d="M18 8a6 6 0 1 0-12 0c0 7-3 9-3 9h18s-3-2-3-9"/><path d="M13.7 21a2 2 0 0 1-3.4 0"/>'
    };
    // The generic fallback is a plain page, so a new index entry that forgets its
    // data-ic still renders something rather than a bare label in an icon dock.
    DOCK_ICONS._ = '<path d="M6 3h8l4 4v14H6z"/><path d="M14 3v5h4"/>';
    function dockIcon(src) {
      var d = DOCK_ICONS[src.getAttribute('data-ic')] || DOCK_ICONS._;
      return '<span class="ui-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" ' +
        'stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + d + '</svg></span>';
    }

    var FREQ_PATHS = (function () {
      try { return JSON.parse(document.getElementById('dock-frequent').textContent) || []; }
      catch (e) { return []; }
    })();
    function syncFrequent(dock, path) {
      dock.querySelectorAll('.js-freq, .dock-sep').forEach(function (el) { el.remove(); });
      if (!FREQ_PATHS.length) return;
      var index = document.getElementById('page-index');
      if (!index) return;
      var byUrl = {};
      index.querySelectorAll('a[href]').forEach(function (a) { byUrl[a.getAttribute('href')] = a; });
      // Exclude every destination the dock already carries — the fixed items, the one
      // promoted for this route, and an admin entry the server rendered in — or it
      // shows twice ("Users" next to "Users").
      var inDock = {};
      dock.querySelectorAll('a.nav-link[href]').forEach(function (a) { inDock[a.getAttribute('href')] = true; });
      // Frequent pages go before the search field (the dock's last item).
      var more = dock.querySelector('.dock-search'), first = true, shown = 0;
      FREQ_PATHS.forEach(function (url) {
        // The server sends ten candidates; take the first three that survive.
        // A path drops out here when it is a detail page rather than a navigation
        // destination (no page-index entry), or when this role cannot open it.
        if (shown >= 3) return;
        if (inDock[url]) return;
        var src = byUrl[url];
        if (!src) return;
        shown++;
        if (first) {
          var sep = document.createElement('span');
          sep.className = 'dock-sep';
          sep.setAttribute('aria-hidden', 'true');
          dock.insertBefore(sep, more || null);
          first = false;
        }
        var a = document.createElement('a');
        a.className = 'nav-link js-dest js-freq' + (owns(url, path) ? ' active' : '');
        a.href = url;
        var label = (src.getAttribute('data-cmdk-label') || src.textContent).trim();
        a.setAttribute('data-cmdk-label', label);
        a.title = label + ' — one of your most-visited pages';
        a.innerHTML = dockIcon(src) + '<span class="dock-lbl">' + label + '</span>';
        dock.insertBefore(a, more || null);
        if (window.htmx) htmx.process(a);
      });
    }

    function syncNav() {
      var path = location.pathname;
      var dock = document.querySelector('nav.dock');
      // Source of the promotion is the (never-rendered) page index.
      var pop = document.getElementById('page-index');
      if (!dock) return;
      // A previously promoted entry is a clone, not part of the rendered dock.
      var old = dock.querySelector('.js-promoted');
      if (old) old.remove();
      // The server already renders the active admin entry into the dock for a full
      // page load. Promoting a second copy on top of it is what showed "Clients"
      // twice — only clone when the dock does not already carry this destination.
      var already = false;
      dock.querySelectorAll('a.nav-link[href]').forEach(function (a) {
        if (!a.classList.contains('js-promoted') && owns(a.getAttribute('href'), path)) already = true;
      });
      if (pop && !already) {
        var hit = null;
        pop.querySelectorAll('a[href]').forEach(function (a) {
          var on = owns(a.getAttribute('href'), path);
          a.classList.toggle('active', on);
          if (on) hit = a;
        });
        if (hit) {
          var clone = hit.cloneNode(true);
          clone.className = 'nav-link js-dest js-promoted active';
          // Index entries are text-only (they were never meant to be seen), so
          // rebuild the link the way a real dock entry is built: icon + label.
          clone.innerHTML = dockIcon(hit) +
            '<span class="dock-lbl">' + (hit.getAttribute('data-cmdk-label') || hit.textContent).trim() + '</span>';
          clone.setAttribute('data-cmdk-label', hit.getAttribute('data-cmdk-label') || '');
          var more = dock.querySelector('.dock-search');
          dock.insertBefore(clone, more || null);
          if (window.htmx) htmx.process(clone);
        }
      }
      dock.querySelectorAll('a.nav-link[href]').forEach(function (a) {
        if (a.classList.contains('js-promoted')) return;
        a.classList.toggle('active', owns(a.getAttribute('href'), path));
      });
      // After the promotion, so the two can never render the same destination.
      // Guarded: localStorage throws outright in some privacy modes, and a dock
      // convenience must never be able to break the dock.
      try { syncFrequent(dock, path); } catch (e) {}
      if (window.mdmSyncDockActive) window.mdmSyncDockActive();
    }
    document.addEventListener('htmx:afterSettle', syncNav);
    window.addEventListener('popstate', syncNav);
    syncNav();
  })();

  // Background-warm the other primary dock destinations so clicking between them
  // feels instant. Fires the exact same request htmx's own boosted click would
  // (HX-Request/HX-Boosted headers) at the same URL; the server (see prefetchableTemplates
  // in handlers.go) briefly makes that specific response variant cacheable
  // (Cache-Control: private, max-age=10, Vary'd on those headers so it can never be
  // served for a non-boosted full-page load). htmx's real fetch on click then hits
  // the browser's own HTTP cache and resolves near-instantly — htmx never knows,
  // so its normal swap/title/script/history handling all still runs unchanged.
  //
  // Fired only after window 'load' (never requestIdleCallback, which can run BEFORE
  // load) — an outstanding fetch kicked off while the page is still "loading" keeps
  // the browser tab's own loading indicator spinning until it resolves; one that
  // starts after load already finished doesn't.
  (function () {
    var WARM_DELAY_MS = 600;
    function warmDockLinks() {
      document.querySelectorAll('.dock .nav-link[href]').forEach(function (a) {
        var href = a.getAttribute('href');
        if (!href || href === location.pathname) return;
        fetch(href, {
          headers: { 'HX-Request': 'true', 'HX-Boosted': 'true' },
          credentials: 'same-origin',
        }).catch(function () {});
      });
    }
    function scheduleWarm() { setTimeout(warmDockLinks, WARM_DELAY_MS); }
    if (document.readyState === 'complete') scheduleWarm();
    else window.addEventListener('load', scheduleWarm);
    // Re-warm the other destinations each time a boosted nav settles into a new
    // page — the 10s cache window is short by design (this is live fleet data), so
    // keep it topped up as the user browses instead of only warming once at boot.
    document.body.addEventListener('htmx:afterSettle', function (evt) {
      var t = evt.detail && evt.detail.target;
      if (t && t.tagName === 'MAIN') scheduleWarm();
    });
  })();
  
