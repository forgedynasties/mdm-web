// Loaded by templates/layout.html.
    // ── No-reload navigation plumbing ──────────────────────────────────────────
    // The whole app is hx-boosted (see <body>): links and form submits are AJAX
    // and only the <main> region is swapped, so the header/dock and the persistent
    // footer scripts (theme, alerts SSE, modals) are never re-run. Page-scoped
    // live resources DO live inside <main>, so register them here and we tear them
    // down before each boosted swap — otherwise EventSources/WebSockets would leak
    // and hit the browser's per-domain connection cap.
    window.__pageStreams = [];
    window.__pageTimers = [];
    // mdmStream: an EventSource that auto-closes on the next in-app navigation.
    window.mdmStream = function (url, cfg) {
      var es = new EventSource(url, cfg);
      window.__pageStreams.push(es);
      return es;
    };
    // mdmTrackStream / mdmTrackTimer: register a resource created elsewhere
    // (e.g. a WebSocket, or a setInterval id) for the same cleanup.
    window.mdmTrackStream = function (s) { window.__pageStreams.push(s); return s; };
    window.mdmInterval = function (fn, ms) { var id = setInterval(fn, ms); window.__pageTimers.push(id); return id; };
    function mdmTeardownPage() {
      window.__pageStreams.forEach(function (s) { try { s.close(); } catch (e) {} });
      window.__pageStreams = [];
      window.__pageTimers.forEach(function (id) { clearInterval(id); });
      window.__pageTimers = [];
    }
    // A boosted page navigation always swaps <main>, whatever hx-target the link
    // inherited. Links inside live regions (hx-target="this" polling panes such
    // as the device Queue, the command detail, release rows) used to inherit that
    // target and render the whole destination page INSIDE the pane. htmx honours
    // detail.target / swapOverride set here.
    // Capture phase: this must run before the in-place morph listener on <body>
    // (which would otherwise morph the page into the pane and cancel the swap).
    document.addEventListener('htmx:beforeSwap', function (e) {
      var d = e.detail; if (!d || !d.boosted) return;
      // Only boosted <a> navigations: htmx also flags a pane's own hx-get poll as
      // "boosted" when it sits under hx-boost, and that must keep its own target.
      // detail.elt here is the swap target, so look at what triggered the request.
      var rc = d.requestConfig; if (rc && rc.verb && rc.verb !== 'get') return;
      var te = rc && rc.triggeringEvent, src = te && te.target && te.target.closest ? te.target.closest('a[href]') : null;
      if (!src || (te.type !== 'click' && te.type !== 'keydown')) return;
      var main = document.querySelector('main'); if (!main || d.target === main) return;
      d.target = main; d.swapOverride = 'outerHTML'; d.selectOverride = 'main';
    }, true);
    // Safety net for polling regions: an hx-get element with no hx-target of its
    // own inherits the body's hx-target="main" and would replace the whole page
    // with its fragment every tick. Only a boosted navigation may swap <main>;
    // anything else that resolved to <main> by inheritance is redirected to the
    // element that made the request.
    document.addEventListener('htmx:beforeSwap', function (e) {
      var d = e.detail; if (!d || d.boosted) return;
      var main = document.querySelector('main'); if (!main || d.target !== main) return;
      var el = d.elt; if (!el || el === main || el === document.body) return;
      var own = el.closest('[hx-target]'); if (own && own !== document.body) return; // explicit target
      d.target = el;
      if (!el.hasAttribute('hx-swap')) d.swapOverride = 'innerHTML';
      try { console.warn('[mdm] hx-get without hx-target retargeted to itself:', el.id || el.tagName); } catch (e2) {}
    }, true);
    // "← Back" links return to the page the user actually came from (tracked
    // per tab across boosted navigations); the href stays the sensible default
    // for a fresh tab or a deep link.
    (function () {
      var K = 'mdm-nav-prev', C = 'mdm-nav-cur';
      function here() { return location.pathname + location.search; }
      function track() {
        try {
          var cur = sessionStorage.getItem(C), now = here();
          if (cur && cur !== now) sessionStorage.setItem(K, cur);
          sessionStorage.setItem(C, now);
        } catch (e) {}
      }
      track();
      document.addEventListener('htmx:pushedIntoHistory', track);
      window.addEventListener('popstate', function () { setTimeout(track, 0); });
      document.addEventListener('click', function (e) {
        var a = e.target.closest && e.target.closest('a.back-link[href]');
        if (!a || e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
        var prev = null; try { prev = sessionStorage.getItem(K); } catch (e2) {}
        if (!prev || prev === here() || prev.indexOf('/login') === 0) return;
        e.preventDefault(); e.stopPropagation();
        var t = document.createElement('a'); t.href = prev; t.setAttribute('hx-boost', 'true'); t.setAttribute('hx-target', 'main'); t.setAttribute('hx-swap', 'outerHTML'); t.setAttribute('hx-select', 'main'); t.hidden = true;
        document.body.appendChild(t); if (window.htmx) htmx.process(t); t.click(); setTimeout(function () { t.remove(); }, 0);
      }, true);
    })();
    // hx-confirm through an in-app dialog, not window.confirm: browsers silently
    // suppress native dialogs after "don't show again", which made every
    // confirm-guarded button look dead.
    document.addEventListener('htmx:confirm', function (e) {
      var q = e.detail && e.detail.question; if (!q) return;
      e.preventDefault();
      var m = document.getElementById('mdm-hxconfirm');
      if (!m) {
        m = document.createElement('div'); m.id = 'mdm-hxconfirm'; m.className = 'modal-backdrop';
        m.innerHTML = '<div class="modal-box" role="alertdialog" aria-modal="true" aria-labelledby="mdm-hxconfirm-msg"><div class="modal-body" style="padding:22px 22px 8px;font-size:14px;line-height:1.5;" id="mdm-hxconfirm-msg"></div><div class="modal-footer" style="display:flex;gap:8px;justify-content:flex-end;padding:12px 22px 18px;"><button type="button" class="btn-outline" data-x>Cancel</button><button type="button" class="cc-btn primary" data-ok>OK</button></div></div>';
        document.body.appendChild(m);
      }
      m.querySelector('#mdm-hxconfirm-msg').textContent = q;
      var ok = m.querySelector('[data-ok]'), no = m.querySelector('[data-x]');
      var src = e.detail && (e.detail.elt || e.target), form = src && src.closest ? src.closest('form') : null;
      function close(cancelled) {
        m.classList.remove('modal-open'); ok.onclick = no.onclick = m.onclick = null; document.removeEventListener('keydown', esc);
        // The submit-lock already fired on the click; put the button back if the
        // user backs out, otherwise it stays disabled with its spinner forever.
        if (cancelled && form && window.mdmResetSubmitting) window.mdmResetSubmitting(form);
        if (cancelled && window.mdmEndBusy) window.mdmEndBusy();
      }
      function esc(ev) { if (ev.key === 'Escape') close(true); }
      ok.onclick = function () { close(false); e.detail.issueRequest(true); };
      no.onclick = function () { close(true); }; m.onclick = function (ev) { if (ev.target === m) close(true); };
      document.addEventListener('keydown', esc);
      m.classList.add('modal-open'); setTimeout(function () { ok.focus(); }, 30);
    });
    // Tear down when a boosted navigation is about to replace <main>.
    document.addEventListener('htmx:beforeSwap', function (e) {
      var t = e.detail && e.detail.target;
      if (t && t.tagName === 'MAIN') mdmTeardownPage();
    });
    // Belt-and-suspenders: also on a hard unload.
    window.addEventListener('pagehide', mdmTeardownPage);

    // ── Focus and announce the new page ───────────────────────────────────────
    // A boosted navigation replaces <main> but leaves focus wherever the click
    // left it — on the dock link that was pressed. Tabbing from there walks the
    // dock again instead of entering the page just loaded, and a screen reader
    // hears nothing at all: the <title> swap is silent, and no live region
    // carries the route. Both are fixed here, on the same event the teardown
    // above already uses.
    //
    // <main tabindex="-1"> is focusable by script but not by Tab, so moving
    // focus to it does not add a stop to the tab order; the next Tab continues
    // from the top of the page content. This is also what finally makes the
    // "Skip to content" link work — its target had never been focused before.
    document.addEventListener('htmx:afterSettle', function (e) {
      var d = e.detail; if (!d || !d.boosted) return;
      var t = d.target; if (!t || t.tagName !== 'MAIN') return;
      var main = document.getElementById('main') || document.querySelector('main');
      if (!main) return;
      try { main.focus({ preventScroll: true }); } catch (e2) { try { main.focus(); } catch (e3) {} }
      window.scrollTo(0, 0);
      // Announce the page name. document.title is "<Page> — <Brand>"; the brand
      // is the same on every page, so announcing it each time is noise.
      var an = document.getElementById('route-announcer');
      var name = (document.title || '').split(' — ')[0].trim();
      // Feed the palette's recents from the same signal, so "where you have been"
      // needs no extra bookkeeping anywhere else.
      if (window.mdmNoteVisit) window.mdmNoteVisit(location.pathname, name);
      if (an) {
        // Re-assigning the same string does not re-announce in some readers, so
        // clear first and set on the next frame.
        an.textContent = '';
        requestAnimationFrame(function () { an.textContent = name || 'Page loaded'; });
      }
    });

    // ── Instant, page-shaped skeleton on navigation ────────────────────────────
    // The moment a boosted page navigation starts, clone the skeleton <template>
    // that matches the destination into <main>, so the page appears instantly in
    // roughly its final shape while data loads; htmx then swaps in real content.
    // Only for GET navigations that target <main> (not in-place polling partials,
    // which target their own regions, nor form POSTs). A tiny delay avoids a flash
    // on already-fast responses.
    function mdmSkeletonFor(path) {
      path = (path || '').split('?')[0].split('#')[0];
      if (path === '/') return 'skel-overview';
      if (path === '/settings') return 'skel-settings';
      if (path === '/commands') return 'skel-actions';
      // Detail pages: a device serial, a numeric release/deployment id, or a
      // group/restaurant/production id — stat-cards + content blocks.
      if (/^\/devices\/[^/]+/.test(path)) return 'skel-detail';
      if (/^\/releases\/\d+/.test(path)) return 'skel-detail';
      if (/^\/commands\/[0-9a-f-]{8,}/.test(path)) return 'skel-detail';
      if (/^\/(groups|restaurants|productions)\/[^/]+/.test(path)) return 'skel-detail';
      if (path === '/devices') return 'skel-fleet'; // two-column: collections rail + roster
      return 'skel-list'; // history, alerts, users, packages, setup, …
    }
    // ── Page-shaped skeletons from the page itself ─────────────────────────────
    // Each time a page settles, remember where its cards, tables and headings sit
    // (per path, with ids folded: /devices/*, /releases/*). The next navigation to
    // it shimmers in exactly that shape instead of a generic template, so the
    // skeleton is right for every page without one being drawn by hand.
    function mdmShapeKey(path) {
      path = (path || '').split('?')[0].split('#')[0];
      return 'mdm-shape:' + path.split('/').map(function (seg, i) {
        if (i < 2) return seg;
        return /^\d+$|^[0-9a-f-]{8,}$|^[A-Z0-9][A-Z0-9-]{5,}$/i.test(seg) && /\d/.test(seg) ? '*' : seg;
      }).join('/');
    }
    function mdmCaptureShape() {
      var main = document.querySelector('main');
      if (!main || main.querySelector('.skel, .skel-shape') || !main.textContent.trim()) return;
      var mr = main.getBoundingClientRect();
      if (mr.width < 240) return;
      var bodyBg = getComputedStyle(document.body).backgroundColor, boxes = [];
      function cardLike(cs) {
        var border = parseFloat(cs.borderTopWidth) > 0 && cs.borderTopStyle !== 'none' && parseFloat(cs.borderLeftWidth) > 0;
        var bg = cs.backgroundColor, filled = bg && bg !== 'transparent' && bg !== 'rgba(0, 0, 0, 0)' && bg !== bodyBg;
        return border || filled;
      }
      (function walk(el, depth) {
        for (var c = el.firstElementChild; c && boxes.length < 60; c = c.nextElementSibling) {
          if (/^(SCRIPT|STYLE|TEMPLATE|LINK|DIALOG)$/.test(c.tagName)) continue;
          var r = c.getBoundingClientRect();
          if (r.width < 8 || r.height < 6 || r.top - mr.top > 1800) continue;
          var cs = getComputedStyle(c);
          // Floating things (popovers, map labels, dialogs) are not part of the page's shape.
          if (cs.display === 'none' || cs.visibility === 'hidden' || cs.position === 'fixed' || cs.position === 'absolute' || c.getAttribute('role') === 'dialog') continue;
          if (/^H[1-3]$/.test(c.tagName)) {
            var rg = document.createRange(); rg.selectNodeContents(c);
            var t = rg.getBoundingClientRect();
            if (t.width > 4) boxes.push([t.left - mr.left, t.top - mr.top, t.width, t.height, 1, 6]);
            continue;
          }
          if (r.width * r.height > 1600 && cardLike(cs)) {
            boxes.push([r.left - mr.left, r.top - mr.top, r.width, r.height, 0, parseFloat(cs.borderTopLeftRadius) || 8]);
            continue;
          }
          if (depth < 7) walk(c, depth + 1);
        }
      })(main, 0);
      if (boxes.length < 2) return;
      var h = 0; boxes.forEach(function (b) { h = Math.max(h, b[1] + b[3]); });
      try {
        localStorage.setItem(mdmShapeKey(location.pathname), JSON.stringify({ w: Math.round(mr.width), h: Math.round(h),
          b: boxes.map(function (b) { return b.map(function (v) { return Math.round(v); }); }) }));
      } catch (e) {}
    }
    // The remembered shape as a skeleton, or null when there is none or it was taken
    // at a width this layout would reflow away from.
    function mdmShapeSkeleton(path, main) {
      var s = null;
      try { s = JSON.parse(localStorage.getItem(mdmShapeKey(path)) || 'null'); } catch (e) {}
      if (!s || !s.b || !main) return null;
      var w = main.getBoundingClientRect().width;
      if (!w || Math.abs(w - s.w) / s.w > 0.12) return null;
      var k = w / s.w, wrap = document.createElement('div');
      wrap.className = 'skel-shape';
      wrap.setAttribute('aria-hidden', 'true');
      wrap.style.height = Math.min(s.h, Math.round(window.innerHeight * 1.5)) + 'px';
      s.b.forEach(function (b) {
        var d = document.createElement('div');
        d.className = b[4] ? 'skel' : 'skel-block';
        d.style.cssText = 'position:absolute;margin:0;left:' + Math.round(b[0] * k) + 'px;top:' + b[1] + 'px;width:' +
          Math.round(b[2] * k) + 'px;height:' + b[3] + 'px;border-radius:' + b[5] + 'px';
        wrap.appendChild(d);
      });
      return wrap;
    }
    // Capture once the page has settled, after its lazy parts had a moment to load.
    var _shapeT = null;
    function mdmScheduleCapture() { clearTimeout(_shapeT); _shapeT = setTimeout(mdmCaptureShape, 1200); }
    window.addEventListener('load', mdmScheduleCapture);
    document.addEventListener('htmx:afterSettle', function (e) {
      var t = e.detail && e.detail.target;
      if (t && t.tagName === 'MAIN') mdmScheduleCapture();
    });

    // ── Nothing on screen without its CSS (29 Oct 2026) ─────────────────────────
    // Pages keep their <style> blocks and stylesheets inside <main>, but some of what they
    // style lives outside it: the context island in the top bar is copied from the page's
    // own template. So when a navigation emptied <main> (the skeleton) or swapped it, the
    // island's buttons lost their styles until the next page arrived ("buttons lose css
    // when I click Remote"). The outgoing page's styles are held in <head> until the new
    // page is in, and anything swapped in that brings a stylesheet stays hidden — the
    // island with it — until that stylesheet has loaded (at most 2.5 s).
    function mdmHoldStyles(main) {
      var held = document.getElementById('mdm-held-css');
      if (!held) {
        held = document.createElement('div');
        held.id = 'mdm-held-css';
        held.hidden = true;
        document.head.appendChild(held);
      }
      Array.prototype.forEach.call(main.querySelectorAll('style, link[rel="stylesheet"]'), function (el) {
        held.appendChild(el.cloneNode(true));
      });
    }
    function mdmReleaseStyles() {
      var held = document.getElementById('mdm-held-css');
      if (held) held.remove();
    }
    // The outgoing page's styles, captured as the swap begins (the skeleton may have
    // taken them already), kept until the new page has settled.
    document.addEventListener('htmx:beforeSwap', function (e) {
      var t = e.detail && e.detail.target, main = document.querySelector('main');
      if (t && t === main && !document.getElementById('mdm-held-css')) mdmHoldStyles(main);
    });
    function mdmWaitForCSS(el, alsoHide) {
      var pending = Array.prototype.filter.call(el.querySelectorAll('link[rel="stylesheet"]'), function (l) { return !l.sheet; });
      if (!pending.length) return false;
      var targets = [el].concat(alsoHide || []);
      var left = pending.length;
      var show = function () { targets.forEach(function (t) { t._mdmCssWait = false; t.classList.remove('css-wait'); }); };
      var done = function () { if (--left <= 0) show(); };
      targets.forEach(function (t) { t._mdmCssWait = true; t.classList.add('css-wait'); });
      pending.forEach(function (l) { l.addEventListener('load', done, { once: true }); l.addEventListener('error', done, { once: true }); });
      setTimeout(show, 2500);
      return true;
    }
    document.addEventListener('htmx:afterSwap', function (e) {
      var t = e.detail && e.detail.target, main = document.querySelector('main');
      if (!t) return;
      if (t === main || t.tagName === 'MAIN') {
        var island = document.getElementById('appbar-island');
        mdmWaitForCSS(main, island ? [island] : []);
        // The island now holds the new page's controls: the old styles can go.
        mdmReleaseStyles();
        return;
      }
      if (t.querySelectorAll) mdmWaitForCSS(t);
    });
    // htmx's settle step puts the swapped-in element's own attributes back (class="container"
    // on <main>), which dropped css-wait while the stylesheet was still loading: Lab → Apps
    // showed the list unstyled for ~0.4 s (30 Oct 2026). Put it back until the sheet is in.
    document.addEventListener('htmx:afterSettle', function () {
      [document.querySelector('main'), document.getElementById('appbar-island')].forEach(function (t) {
        if (t && t._mdmCssWait) t.classList.add('css-wait');
      });
    });
    var _skelTimer = null, _navWatchdog = null, _navDest = '', _navActive = false;
    function mdmCancelSkeleton() { if (_skelTimer) { clearTimeout(_skelTimer); _skelTimer = null; } }
    function mdmClearWatchdog() { if (_navWatchdog) { clearTimeout(_navWatchdog); _navWatchdog = null; } }
    // A boosted <main> GET nav that RECOVERS to real content (beforeSwap/afterSettle):
    // clear the skeleton timer + watchdog, nav is done.
    function mdmNavOk() { mdmCancelSkeleton(); mdmClearWatchdog(); _navActive = false; }
    // A boosted <main> GET nav that FAILED (aborted/errored/timed out — Firefox fires
    // htmx:abort instead of afterRequest): the page is stuck on a skeleton, so recover
    // immediately with a full load of the destination. Guarded by _navActive so an
    // aborted polling partial doesn't trigger a reload.
    // Is <main> currently stuck on a skeleton or blank? Only then is a hard reload
    // the right recovery — a transient error on an already-rendered page must not
    // blow the good content away.
    function mdmMainStuck() {
      var main = document.querySelector('main');
      return !!(main && (!main.textContent.trim() || main.querySelector('.skel, .skel-shape')));
    }
    function mdmNavFail() {
      mdmCancelSkeleton(); mdmClearWatchdog();
      mdmReleaseStyles();
      if (!_navActive) return;
      _navActive = false;
      if (mdmMainStuck()) window.location.href = _navDest || window.location.href;
    }
    document.addEventListener('htmx:beforeRequest', function (e) {
      var d = e.detail || {};
      var verb = (d.requestConfig && d.requestConfig.verb || '').toLowerCase();
      var main = document.querySelector('main');
      if (verb !== 'get' || !main || d.target !== main) return; // only main-targeted GET nav
      var path = (d.requestConfig && d.requestConfig.path) ||
                 (d.elt && d.elt.getAttribute && d.elt.getAttribute('href')) || '';
      // Highlight the destination in the dock immediately (don't wait for the
      // page to load) — mdmSyncDockActive is defined in the footer script.
      var dest = path.split('?')[0].split('#')[0];
      if (window.mdmSyncDockActive) window.mdmSyncDockActive(dest);
      // Only reveal the skeleton once the response is clearly slow. On a LAN/local
      // server most pages return in well under this, so a fast nav goes straight
      // content→content (crossfaded) and never flashes a blank/skeleton frame.
      _skelTimer = setTimeout(function () {
        // The page's own remembered shape when there is one, else the template.
        var shape = mdmShapeSkeleton(path, main);
        var tpl = shape ? null : document.getElementById(mdmSkeletonFor(path));
        // The island outside <main> still shows this page's controls: keep their styles.
        mdmHoldStyles(main);
        main.innerHTML = '';
        if (shape) main.appendChild(shape);
        else if (tpl && tpl.content) main.appendChild(tpl.content.cloneNode(true));
        window.scrollTo(0, 0);
      }, 150);
      // Watchdog: if the nav neither succeeds nor fails within 12s (genuinely hung)
      // AND left <main> stuck on a skeleton/blank, force a full reload so the page
      // can never stay blank — but never reload away good, already-rendered content.
      _navActive = true; _navDest = path;
      mdmClearWatchdog();
      _navWatchdog = setTimeout(function () {
        if (!_navActive) return;
        _navActive = false;
        if (mdmMainStuck()) window.location.href = _navDest || window.location.href;
      }, 12000);
    });
    document.addEventListener('htmx:beforeSwap', mdmNavOk);
    document.addEventListener('htmx:afterSettle', mdmNavOk);
    // htmx:abort is intentionally NOT here: an abort is almost always benign — the
    // user started a newer navigation that superseded this one, or clicked away.
    // Treating it as a failure was reloading the whole page mid-navigation.
    ['htmx:responseError', 'htmx:sendError', 'htmx:timeout'].forEach(function (ev) {
      document.addEventListener(ev, mdmNavFail);
    });
    // A refused action (403/400/404 on a boosted form) used to fail silently:
    // surface the server's one-line reason as a toast.
    document.addEventListener('htmx:responseError', function (e) {
      var x = e.detail && e.detail.xhr; if (!x || x.status < 400 || x.status >= 500) return;
      var t = (x.responseText || '').trim();
      if (!t || t.length > 240 || /^\s*</.test(t)) t = x.status === 403 ? 'You are not allowed to do that.' : x.status === 404 ? 'Not found.' : 'Request failed (' + x.status + ').';
      if (window.showToast) showToast(t, 'error');
    });
  
