// Loaded by templates/layout.html.
  // Keep the dock's active highlight in sync after a boosted (main-only) swap:
  // the dock lives outside <main>, so it isn't re-rendered on navigation.
  // One highlight only (15 Oct): an entry for the page itself (Enrollment in "your three",
  // say) wins over the one that merely owns it (Fleet owns /enrollment).
  function mdmSyncDockActive(path) {
    var p = path || location.pathname, direct = [], grouped = [];
    document.querySelectorAll('.dock .nav-link').forEach(function (a) {
      var h = a.getAttribute('href'), d, g = false;
      if (h === '/') d = (p === '/');
      else d = (p === h || p.indexOf(h + '/') === 0);
      if (!d && h === '/devices') g = /^\/(groups|enrollment)(\/|$)/.test(p);
      if (!d && h === '/releases') g = /^\/(rollouts|packages|updates)(\/|$)/.test(p);
      a.classList.toggle('active', d || g);
      if (d) direct.push(a); else if (g) grouped.push(a);
    });
    if (direct.length) grouped.forEach(function (a) { a.classList.remove('active'); });
  }
  // Call with no arg so it reads the (now-current) location.pathname — not the event.
  // URL first, content later: only the highlights move now, so the layout holds still
  // while the next page loads.
  document.body.addEventListener('htmx:pushedIntoHistory', function () { mdmSyncDockActive(); });
  document.body.addEventListener('htmx:historyRestore', function () { mdmSyncDockActive(); });
  window.addEventListener('popstate', function () { setTimeout(function () { mdmSyncDockActive(); }, 0); });
  // First paint, and after every swap: the dock is outside <main>, so it is never
  // re-rendered and has to be told when the page under it changed.
  mdmSyncDockActive();
  document.body.addEventListener('htmx:afterSwap', function () { mdmSyncDockActive(); });
  // ── Role-locked controls ────────────────────────────────────────────────────
  // A control the viewer's role can't use is still rendered, so people can see
  // what the product does and know what to ask for. Three things have to be true
  // for that to be honest rather than just broken-looking:
  //
  //   1. it must read as locked, not as still-loading (see .role-locked in
  //      style.css: a padlock and muted colour, not a blanket opacity);
  //   2. it must ANNOUNCE as locked — aria-disabled, stamped here so every
  //      current and future .role-locked gets it from one place rather than 28
  //      call sites having to remember;
  //   3. the reason must be reachable without clicking, because the click is
  //      exactly what we swallow. It appears on focus as well as on click, and
  //      the toast is a live region so it is spoken, not just drawn.
  function mdmLockReason(el) {
    var need = el.getAttribute('data-role-need') || 'operator';
    var label = {support: 'Support', operator: 'Operator', fleet_manager: 'Fleet manager', fleet_admin: 'Admin', admin: 'Super admin'}[need] || need;
    return el.getAttribute('data-lock-msg') ||
      (/rule/.test(need)
        ? 'Needs a ' + need + ' — ask a fleet manager to add one for you.'
        : 'Needs the ' + label + ' role — ask a fleet manager to raise your access.');
  }
  function mdmShowLockReason(el) {
    var t = document.getElementById('role-lock-toast');
    if (!t) {
      t = document.createElement('div'); t.id = 'role-lock-toast';
      t.setAttribute('role', 'status'); t.setAttribute('aria-live', 'polite');
      document.body.appendChild(t);
    }
    t.textContent = mdmLockReason(el);
    t.classList.add('show'); clearTimeout(t._h);
    t._h = setTimeout(function () { t.classList.remove('show'); }, 2600);
  }
  // Mark up whatever is on the page now, and whatever htmx swaps in later.
  function mdmStampLocks(root) {
    var scope = (root && root.querySelectorAll) ? root : document;
    scope.querySelectorAll('.role-locked').forEach(function (el) {
      if (el.getAttribute('aria-disabled') === 'true') return;
      el.setAttribute('aria-disabled', 'true');
      // The reason is the accessible description; without it the padlock is the
      // only clue and a reader gets "dimmed" with no way to find out why.
      if (!el.hasAttribute('title')) el.setAttribute('title', mdmLockReason(el));
    });
  }
  mdmStampLocks(document);
  document.body.addEventListener('htmx:afterSettle', function (e) {
    mdmStampLocks(e.detail && e.detail.target);
  });
  // Swallow the click in the capture phase, so inline handlers and form submits
  // never run, and say which role unlocks it.
  document.addEventListener('click', function (e) {
    var el = e.target && e.target.closest ? e.target.closest('.role-locked') : null;
    if (!el) return;
    e.preventDefault(); e.stopPropagation();
    mdmShowLockReason(el);
  }, true);
  // Keyboard: Enter/Space would otherwise fire the control, and tabbing onto it
  // gave no hint at all that it was locked.
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' && e.key !== ' ' && e.key !== 'Spacebar') return;
    var el = e.target && e.target.closest ? e.target.closest('.role-locked') : null;
    if (!el) return;
    e.preventDefault(); e.stopPropagation();
    mdmShowLockReason(el);
  }, true);
  document.addEventListener('focusin', function (e) {
    var el = e.target && e.target.closest ? e.target.closest('.role-locked') : null;
    if (el) mdmShowLockReason(el);
  });
  // Theme toggle: flip html[data-theme], persist, and notify listeners
  // (e.g. charts) so they can recolor without a reload.
  function toggleTheme() {
    var cur = document.documentElement.getAttribute('data-theme') === 'dark' ? 'dark' : 'light';
    var next = cur === 'dark' ? 'light' : 'dark';
    document.documentElement.setAttribute('data-theme', next);
    try { localStorage.setItem('mdm-theme', next); } catch (e) {}
    window.dispatchEvent(new CustomEvent('mdm:themechange', { detail: { theme: next } }));
  }
  window.toggleTheme = toggleTheme;
  // Current resolved theme, for scripts that need it at init time.
  window.mdmTheme = function () {
    return document.documentElement.getAttribute('data-theme') === 'dark' ? 'dark' : 'light';
  };

  // ── Motion (static/motion.css). Two runtimes that need no per-page code:
  //  · busy buttons: any submit button, and any hx-* trigger, shows a spinner while its
  //    request is in flight, so a press is seen to do something before the answer lands.
  //  · coach marks: one element per page may carry data-coach="id" with data-coach-title /
  //    data-coach-text. The first time this person sees it, it gets a pulsing ring and a
  //    tip; "Got it" remembers that, per person and per mark, in localStorage.
  (function () {
    function busy(el, on) { if (!el || el.tagName !== 'BUTTON') return; el.classList.toggle('is-busy', on); el.toggleAttribute('aria-busy', on); }
    document.addEventListener('submit', function (e) {
      var f = e.target; if (!(f instanceof HTMLFormElement) || f.hasAttribute('hx-post') || f.hasAttribute('hx-get')) return;
      var b = e.submitter || f.querySelector('button[type=submit],button:not([type])'); busy(b, true);
      setTimeout(function () { busy(b, false); }, 15000); // a full-page submit navigates; this is the fallback
    }, true);
    document.addEventListener('htmx:beforeRequest', function (e) { busy(e.detail && e.detail.elt && e.detail.elt.closest('button'), true); busy(e.detail && e.detail.requestConfig && e.detail.requestConfig.triggeringEvent && e.detail.requestConfig.triggeringEvent.submitter, true); });
    document.addEventListener('htmx:afterRequest', function (e) { document.querySelectorAll('button.is-busy').forEach(function (b) { busy(b, false); }); });
    var who = document.body.getAttribute('data-user') || '-';
    function seen(id) { try { return localStorage.getItem('mdm.coach.' + who + '.' + id) === '1'; } catch (e) { return true; } }
    function mark(id) { try { localStorage.setItem('mdm.coach.' + who + '.' + id, '1'); } catch (e) {} }
    function coach() {
      var el = document.querySelector('[data-coach]'); if (!el || el.classList.contains('m-coach')) return;
      var id = el.getAttribute('data-coach'); if (!id || seen(id)) return;
      if (window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches) { /* still show the tip, no ring loop */ }
      el.classList.add('m-coach');
      var tip = document.createElement('div'); tip.className = 'm-tip below'; tip.setAttribute('role', 'note');
      tip.innerHTML = '<b></b><span></span><button type="button">Got it</button>';
      tip.querySelector('b').textContent = el.getAttribute('data-coach-title') || '';
      tip.querySelector('span').textContent = el.getAttribute('data-coach-text') || '';
      function done() { mark(id); el.classList.remove('m-coach'); tip.remove(); }
      tip.querySelector('button').addEventListener('click', function (ev) { ev.stopPropagation(); done(); });
      el.addEventListener('click', done, { once: true });
      var host = el.parentElement; if (host && getComputedStyle(host).position === 'static') host.style.position = 'relative';
      el.insertAdjacentElement('afterend', tip);
      // Keep it on screen when the anchor sits near the right edge (phones).
      var over = tip.getBoundingClientRect().right - (window.innerWidth - 16); if (over > 0) tip.style.left = -over + 'px';
    }
    coach(); document.addEventListener('htmx:afterSwap', coach); document.addEventListener('htmx:load', coach);
  })();
  // ── Appbar pill: past 40px of scroll the bar gets .is-scrolled and style.css runs one
  // 700ms ease-out-expo transition to the floating pill; back again under 40px. Passive
  // listener, one class toggle at most per frame.
  (function () {
    var bar = document.querySelector('.appbar'); if (!bar) return;
    var queued = false;
    function size() { bar.style.setProperty('--pw', Math.min(document.documentElement.clientWidth - 48, 1140) + 'px'); }
    window.addEventListener('resize', size, { passive: true }); size();
    function apply() { queued = false; var on = (window.scrollY || document.documentElement.scrollTop || 0) > 40; if (on !== bar.classList.contains('is-scrolled')) bar.classList.toggle('is-scrolled', on); }
    window.addEventListener('scroll', function () { if (!queued) { queued = true; requestAnimationFrame(apply); } }, { passive: true });
    document.addEventListener('htmx:afterSwap', apply);
    apply();
  })();
  // The context island is filled by an inline script right after </main> (layout.html
  // footer), so it is in place at the first paint instead of after this file loads.
  // ── List tools: one search box, filter chips and a "Show all N" cap for long lists.
  // Root: .lt[data-lt-n="25"]. Rows: .lt-row[data-grp][data-f-<facet>][data-lt-text].
  // Chips: .lt-chip[data-f][data-v] — same facet = OR, across facets = AND. Group
  // header: .lt-grp[data-grp] with a .lt-n count. Cap control: .lt-more[data-grp]
  // (its label goes in .lt-more-t if present). .lt-empty hides while a filter is on,
  // .lt-none shows when nothing matches, .lt-count says how many did. Every change
  // fires 'lt:change' on the root so a page can keep its own state (ticks) honest.
  (function () {
    function init(root) {
      if (root.__ltApply) { root.__ltApply(); return; }
      var n = parseInt(root.getAttribute('data-lt-n') || '25', 10), open = {};
      var q = root.querySelector('.lt-q');
      function apply() {
        var text = (q && q.value || '').trim().toLowerCase(), facets = {};
        root.querySelectorAll('.lt-chip.on').forEach(function (c) { var f = c.getAttribute('data-f'); (facets[f] = facets[f] || []).push(c.getAttribute('data-v')); });
        var active = !!text || Object.keys(facets).length > 0, seen = {}, shown = {}, total = 0;
        root.querySelectorAll('.lt-row').forEach(function (r) {
          var g = r.getAttribute('data-grp') || '';
          var ok = !text || (r.getAttribute('data-lt-text') || r.textContent).toLowerCase().indexOf(text) >= 0;
          if (ok) for (var f in facets) { if (facets[f].indexOf(r.getAttribute('data-f-' + f) || '') < 0) { ok = false; break; } }
          r.classList.toggle('lt-out', !ok);
          if (!ok) { r.classList.remove('lt-hid'); return; }
          seen[g] = (seen[g] || 0) + 1; total++;
          var hid = !open[g] && seen[g] > n;
          r.classList.toggle('lt-hid', hid); if (!hid) shown[g] = (shown[g] || 0) + 1;
        });
        root.querySelectorAll('.lt-grp').forEach(function (h) {
          var g = h.getAttribute('data-grp') || '', c = seen[g] || 0, ns = h.querySelector('.lt-n');
          if (ns) ns.textContent = c; h.classList.toggle('lt-out', active && c === 0);
        });
        root.querySelectorAll('.lt-more').forEach(function (b) {
          var g = b.getAttribute('data-grp') || '', c = seen[g] || 0, rest = c - (shown[g] || 0);
          b.classList.toggle('lt-out', rest <= 0);
          var what = b.getAttribute('data-what'); (b.querySelector('.lt-more-t') || b).textContent = 'Show all ' + c + (what ? ' ' + what : '');
        });
        root.querySelectorAll('.lt-empty').forEach(function (e) { e.classList.toggle('lt-out', active); });
        var none = root.querySelector('.lt-none'); if (none) none.classList.toggle('lt-out', !(active && total === 0));
        var cnt = root.querySelector('.lt-count'); if (cnt) cnt.textContent = active ? total + (total === 1 ? ' match' : ' matches') : '';
        root.dispatchEvent(new CustomEvent('lt:change', { detail: { active: active, total: total } }));
      }
      root.__ltApply = apply;
      if (q) q.addEventListener('input', apply);
      root.querySelectorAll('.lt-chip').forEach(function (c) { c.addEventListener('click', function () { c.classList.toggle('on'); apply(); }); });
      root.addEventListener('click', function (e) { var b = e.target.closest('.lt-more'); if (b && root.contains(b)) { open[b.getAttribute('data-grp') || ''] = true; apply(); } });
      apply();
    }
    window.ltInit = function (scope) { (scope || document).querySelectorAll('.lt').forEach(init); };
    document.addEventListener('DOMContentLoaded', function () { ltInit(); });
    document.addEventListener('htmx:afterSwap', function () { ltInit(); });
    if (document.readyState !== 'loading') ltInit();
  })();
  // ── Live-region helpers: no-op swap guard + client-side relative time ─────────
  // Live pages refetch a region on every SSE tick. If the fragment carries a
  // server-rendered "3m ago", it is never byte-identical between ticks, so the
  // region repaints (and any afterSwap relayout re-runs) even when nothing
  // actually changed — the flicker the operator sees. Two shared primitives fix
  // this everywhere:
  //   1. Any refetch container marked  data-diff-guard  swaps ONLY when the server
  //      HTML actually differs from what's on screen (fail-open on empty/error).
  //   2. Relative times render client-side from a stable epoch: the server emits
  //      a reltime span carrying a stable data-ts unix-seconds value (no churning
  //      text), and we tick it here every second. Combined, an idle update is a true
  //      no-op: no DOM mutation, no lost scroll/hover/focus, no restarted anim.
  (function () {
    var lastHtml = {};
    document.body.addEventListener('htmx:beforeSwap', function (e) {
      var t = e.target;
      if (!t || !t.id || !t.hasAttribute || !t.hasAttribute('data-diff-guard')) return;
      var html = e.detail && e.detail.serverResponse;
      if (typeof html !== 'string' || html === '') return; // fail open — never suppress on empty/error
      if (lastHtml[t.id] === html) { e.detail.shouldSwap = false; }
      else { lastHtml[t.id] = html; }
    });
    function ago(sec) {
      var d = Math.max(0, Math.floor(Date.now() / 1000) - sec);
      if (d < 45) return 'just now';
      if (d < 90) return '1m ago';
      if (d < 3600) return Math.floor(d / 60) + 'm ago';
      if (d < 5400) return '1h ago';
      if (d < 86400) return Math.floor(d / 3600) + 'h ago';
      return Math.floor(d / 86400) + 'd ago';
    }
    window.mdmTickRel = function (root) {
      (root || document).querySelectorAll('.reltime[data-ts]').forEach(function (el) {
        var ts = parseInt(el.dataset.ts, 10);
        // A never-set timestamp (Go zero time) serializes to a large negative Unix
        // value; show a dash instead of an absurd "78652d ago".
        var txt = (!ts || ts < 0) ? '—' : ago(ts);
        if (el.textContent !== txt) el.textContent = txt;
      });
    };
    // Re-tick after any swap brings fresh .reltime spans in, and once a second.
    document.body.addEventListener('htmx:afterSwap', function () { window.mdmTickRel(); });
    setInterval(function () { window.mdmTickRel(); }, 1000);
    window.mdmTickRel();
  })();

  // ── In-place morph ────────────────────────────────────────────────────────────
  // For any [data-morph] region htmx would refresh, update ONLY the nodes that changed
  // instead of replacing the whole region. Unchanged children keep their identity, so no
  // flash, and scroll / hover / focus / caret survive. Falls back to htmx's normal swap
  // on any error, so it can never regress. Pairs with data-diff-guard: if the identical
  // guard already suppressed the swap (byte-identical), we skip the morph too.
  (function () {
    function keyOf(n) {
      if (n.nodeType !== 1) return null;
      return n.id || n.getAttribute('data-pkg') || n.getAttribute('data-cmd-id') ||
             n.getAttribute('data-serial') || n.getAttribute('data-alert-id') || null;
    }
    function morphEl(from, to) {
      // Fully preserve nodes the app pins (htmx hx-preserve, or opt-in data-morph-keep).
      if (from.getAttribute('hx-preserve') === 'true' || from.hasAttribute('data-morph-keep')) return;
      // Never disturb the value/caret of a field the user is editing.
      var tag = from.tagName;
      if ((tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') && document.activeElement === from) return;
      var fa = from.attributes, i;
      for (i = fa.length - 1; i >= 0; i--) {
        if (to.hasAttribute(fa[i].name)) continue;
        // Losing its request attribute does not unbind an element: htmx keeps the old
        // triggers, and the next one fires with no hx-target left, so it inherits the
        // body's "main" and the fragment replaces the whole page (the Action history card
        // did this once its last pending command finished). Swap such a node for a clone
        // afterwards; a clone carries no htmx bindings.
        if (/^(data-)?hx-(get|post|put|patch|delete)$/.test(fa[i].name) && staleNodes.indexOf(from) < 0) staleNodes.push(from);
        from.removeAttribute(fa[i].name);
      }
      var ta = to.attributes, hxGained = false;
      for (i = 0; i < ta.length; i++) {
        if (from.getAttribute(ta[i].name) !== ta[i].value) {
          // A newly gained/changed hx-* attribute means htmx must (re)scan this element to
          // wire its behaviour — e.g. a reused <div> that becomes the "every 2s" install
          // poller. Morph copies the attribute but htmx never sees it, so the trigger
          // (poll, load, etc.) never activates and the region silently stops refreshing.
          if (ta[i].name.indexOf('hx-') === 0) hxGained = true;
          from.setAttribute(ta[i].name, ta[i].value);
        }
      }
      // Re-process only when hx-* actually changed (not every morph), so we never duplicate
      // the static hx-trigger bindings on a region root (that caused a refetch storm).
      if (hxGained) freshNodes.push(from);
      morphChildren(from, to);
    }
    var freshNodes = []; // element nodes newly inserted this morph (for htmx to activate)
    var staleNodes = []; // elements that lost hx-get/post/…: re-created so htmx forgets them
    function morphChildren(from, to) {
      var keyed = {}, c;
      for (c = from.firstChild; c; c = c.nextSibling) { var k = keyOf(c); if (k) keyed[k] = c; }
      var toKids = Array.prototype.slice.call(to.childNodes);
      // Keys present in the NEW children — so we can tell which OLD nodes are going away.
      var newKeys = {};
      for (var j = 0; j < toKids.length; j++) { var nk = keyOf(toKids[j]); if (nk) newKeys[nk] = true; }
      var reused = [];
      var cursor = from.firstChild;
      for (var i = 0; i < toKids.length; i++) {
        var tn = toKids[i], tk = keyOf(tn), matched = null;
        if (tk && keyed[tk]) matched = keyed[tk];
        else if (cursor && !keyOf(cursor) && cursor.nodeType === tn.nodeType &&
                 (tn.nodeType !== 1 || cursor.nodeName === tn.nodeName) && reused.indexOf(cursor) < 0) matched = cursor;
        if (matched) {
          if (matched.nodeType === 1 && tn.nodeType === 1) morphEl(matched, tn);
          else if (matched.nodeValue !== tn.nodeValue) matched.nodeValue = tn.nodeValue;
        } else {
          matched = tn.cloneNode(true);
          if (matched.nodeType === 1) freshNodes.push(matched);
        }
        reused.push(matched);
        // Advance the cursor to where `matched` belongs, skipping OLD nodes that won't be
        // reused: keyed nodes absent from the new set (they get removed at the end) and the
        // whitespace text between elements. Without this, a to-be-removed node — or just the
        // newline between the install poller and the app grid — stuck at the cursor forces
        // every following node to be insertBefore'd ahead of it, moving EVERY tile (and the
        // whole apps-table), which reads as the drawer vanishing and snapping back when an
        // install tile at the front becomes its installed tile elsewhere. Stop at a keyed
        // node the new set still needs, or a meaningful (non-whitespace) unkeyed node.
        while (cursor && cursor !== matched) {
          var ck = keyOf(cursor);
          if (ck) { if (newKeys[ck]) break; }
          else if (!(cursor.nodeType === 3 && (cursor.nodeValue == null || !/\S/.test(cursor.nodeValue)))) break;
          cursor = cursor.nextSibling;
        }
        if (matched !== cursor) from.insertBefore(matched, cursor);
        else cursor = cursor.nextSibling;
      }
      var n = from.firstChild;
      while (n) { var nx = n.nextSibling; if (reused.indexOf(n) < 0) from.removeChild(n); n = nx; }
    }
    // Returns the element nodes freshly inserted (so the caller can htmx.process ONLY those —
    // never the preserved region root, whose hx-trigger bindings must not be duplicated).
    window.mdmMorph = function (from, html) {
      freshNodes = []; staleNodes = [];
      var tpl = document.createElement('template');
      tpl.innerHTML = html;
      // Elements marked hx-swap-oob (e.g. roster-meta-oob's heading/toolbar/pills) are meant
      // for a DIFFERENT target elsewhere on the page, not `from`'s own children. We can't rely
      // on htmx's own out-of-band handling here: this whole response is already being swapped
      // via our own morph (shouldSwap is about to be set false below), and in practice htmx's
      // internal OOB pass does not reliably run for every marked element once that happens.
      // So do it ourselves, with the same morphEl() this function already uses — find each
      // oob element's real target by id and morph it in place — then drop it from the
      // fragment so it isn't ALSO inserted as one of `from`'s own children.
      Array.prototype.slice.call(tpl.content.querySelectorAll('[hx-swap-oob]')).forEach(function (n) {
        if (n.id) {
          var real = document.getElementById(n.id);
          if (real) morphEl(real, n);
        }
        n.remove();
      });
      var root = tpl.content.firstElementChild;
      // An outerHTML partial re-renders the element itself (same id) → morph it whole;
      // an innerHTML partial is a child list → morph the children.
      if (root && !root.nextElementSibling && root.id && root.id === from.id) morphEl(from, root);
      else morphChildren(from, tpl.content);
      staleNodes.forEach(function (n) {
        if (!n.parentNode) return;
        var c = n.cloneNode(true);
        n.parentNode.replaceChild(c, n);
        freshNodes = freshNodes.filter(function (f) { return !n.contains(f); });
        freshNodes.push(c);
      });
      return freshNodes;
    };
    document.body.addEventListener('htmx:beforeSwap', function (e) {
      // Use the swap target htmx resolved (detail.target), not the DOM node the
      // event fired on: a boosted link inside a data-morph pane is retargeted to
      // <main> above, and morphing into the pane here would nest the whole page.
      var t = (e.detail && e.detail.target) || e.target;
      if (!t || !t.hasAttribute || !t.hasAttribute('data-morph')) return;
      if (e.detail && e.detail.shouldSwap === false) return; // diff-guard already suppressed
      var html = e.detail && e.detail.serverResponse;
      if (typeof html !== 'string' || html === '') return;    // fail open
      try {
        var fresh = window.mdmMorph(t, html);
        e.detail.shouldSwap = false;
        // Activate hx-* ONLY on freshly inserted nodes — never re-process the region root,
        // which would duplicate its hx-trigger bindings and cause a refetch storm (flash).
        if (window.htmx) fresh.forEach(function (n) { try { window.htmx.process(n); } catch (e2) {} });
        // Fire a synthetic afterSwap so each region's existing post-swap listener still runs
        // (apps recolor + kiosk picker, fleet column/density/selection, overview layout,
        // relative-time tick) — htmx skips its own afterSwap when we suppress the swap.
        if (!t.isConnected && t.id) t = document.getElementById(t.id) || t; // re-created above
        t.dispatchEvent(new CustomEvent('htmx:afterSwap', { bubbles: true, detail: { target: t, elt: t } }));
      } catch (err) { try { console.warn('[mdmMorph] fell back to innerHTML:', (t && t.id) || t, err && (err.stack || err.message || err)); } catch (e3) {} /* leave shouldSwap true → htmx does its normal swap (no regression) */ }
    });
  })();

  (function () {
    var progress = document.getElementById('htmx-progress');
    var progressActive = false;

    function beginProgress() {
      if (!progress) return;
      progressActive = true;
      document.body.classList.add('htmx-requesting-progress');
      progress.style.width = '70%';
    }

    function endProgress() {
      // Only wind down a bar we actually started. Background live-refresh partials
      // never call beginProgress (they don't target <main>), so endProgress must not
      // flash the bar to 100% on their afterRequest — that was the orange flicker on
      // every telemetry tick.
      if (!progress || !progressActive) return;
      progressActive = false;
      progress.style.width = '100%';
      setTimeout(function () {
        document.body.classList.remove('htmx-requesting-progress');
        progress.style.width = '0';
      }, 180);
    }

    // Catch-all stuck-state watchdog. Independent of request type or htmx events:
    // if the in-progress indicator stays on for more than a few seconds, a request
    // hung or was aborted without firing a terminal event (seen in some Firefox/Zen
    // configs), leaving the page frozen on a skeleton/blank <main>. Force-clear it
    // and, if the content area never got real content, reload to recover — so the
    // app can never sit blank. A full reload is a plain server render (no boosted
    // nav), so it always comes up and can't loop.
    (function () {
      var stuckSince = 0;
      setInterval(function () {
        if (document.body.classList.contains('htmx-requesting-progress')) {
          if (!stuckSince) { stuckSince = Date.now(); return; }
          if (Date.now() - stuckSince < 12000) return;
          stuckSince = 0;
          document.body.classList.remove('htmx-requesting-progress');
          if (progress) progress.style.width = '0';
          var main = document.querySelector('main');
          if (main && (!main.textContent.trim() || main.querySelector('.skel'))) {
            window.location.reload();
          }
        } else {
          stuckSince = 0;
        }
      }, 2000);
    })();

    // Page-wide busy overlay removed (spinner + dim were more distracting than
    // useful). Kept as no-ops so existing callers (device.html, markSubmitting)
    // don't need to change.
    window.mdmBeginBusy = function () {};
    window.mdmEndBusy = function () {};

    function markSubmitting(form, submitter) {
      if (!form || form.dataset.submitting === 'true') return;
      form.dataset.submitting = 'true';
      form.classList.add('form-submitting');
      window.mdmBeginBusy();
      // A disabled submit button is omitted from the form payload — disabling the
      // clicked button below would otherwise drop its name/value (e.g. a problem card's
      // status=fixed), so forms with multiple submit buttons would post no value and
      // the server would reject it. Preserve the submitter as a hidden field first.
      if (submitter && submitter.name) {
        var keep = document.createElement('input');
        keep.type = 'hidden';
        keep.name = submitter.name;
        keep.value = submitter.value;
        form.appendChild(keep);
      }
      form.querySelectorAll('button[type="submit"], input[type="submit"]').forEach(function (btn) {
        btn.disabled = true;
        // Show the loading state only on the button that was actually clicked, so a
        // form with several submit buttons (e.g. a problem's Fixed / Verified / Won't fix)
        // doesn't light every button up at once. The rest are just disabled to
        // prevent a double-submit.
        if (!submitter || btn === submitter) {
          btn.classList.add('btn-submitting');
          if (btn.tagName === 'INPUT') {
            btn.dataset.originalValue = btn.value;
            btn.value = 'Working...';
          }
        }
      });
    }

    window.mdmResetSubmitting = function (root) { resetSubmitting(root); };
    function resetSubmitting(root) {
      var scope = root || document;
      if (scope.matches && scope.matches('form[data-submitting="true"]')) {
        resetSubmittingForm(scope);
      }
      if (scope.querySelectorAll) {
        scope.querySelectorAll('form[data-submitting="true"]').forEach(resetSubmittingForm);
      }
    }

    function resetSubmittingForm(form) {
      form.dataset.submitting = 'false';
      form.classList.remove('form-submitting');
      window.mdmEndBusy();
      form.querySelectorAll('button[type="submit"], input[type="submit"]').forEach(function (btn) {
        btn.disabled = false;
        btn.classList.remove('btn-submitting');
        if (btn.tagName === 'INPUT' && btn.dataset.originalValue) {
          btn.value = btn.dataset.originalValue;
          delete btn.dataset.originalValue;
        }
      });
    }

    function ensurePollStamp(card) {
      if (!card || !card.matches('.card[hx-trigger*="every"]')) return;
      var header = card.querySelector('.card-header');
      if (!header) return;
      var stamp = header.querySelector('.poll-stamp');
      if (!stamp) {
        stamp = document.createElement('span');
        stamp.className = 'poll-stamp';
        header.appendChild(stamp);
      }
      stamp.textContent = 'Synced ' + new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    }

    var use24Hour = document.body.getAttribute('data-time24') === '1';
    function formatLocalTime(el) {
      var raw = el.dataset.utc;
      if (!raw) return;
      var date = new Date(raw);
      if (isNaN(date.getTime())) return;
      var format = el.dataset.format || 'full';
      if (format === 'short') {
        el.textContent = date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: !use24Hour });
      } else {
        el.textContent = date.toLocaleString([], {
          year: 'numeric',
          month: '2-digit',
          day: '2-digit',
          hour: '2-digit',
          minute: '2-digit',
          second: '2-digit',
          hour12: !use24Hour
        });
      }
      el.title = date.toLocaleString();
    }

    function localizeTimes(root) {
      if (!root) return;
      if (root.matches && root.matches('.js-local-time')) {
        formatLocalTime(root);
      }
      if (root.querySelectorAll) {
        root.querySelectorAll('.js-local-time').forEach(formatLocalTime);
      }
    }

    function syncPollStamps(root) {
      if (!root) return;
      if (root.matches && root.matches('.card[hx-trigger*="every"]')) {
        ensurePollStamp(root);
      }
      if (root.querySelectorAll) {
        root.querySelectorAll('.card[hx-trigger*="every"]').forEach(ensurePollStamp);
      }
    }

    // Capture phase, not bubble: htmx's own submit handler (for every hx-boosted
    // form — i.e. nearly all of them, see <body hx-boost="true">) runs on the
    // bubble phase and calls preventDefault() before it issues the AJAX request.
    // A bubble-phase listener here would see evt.defaultPrevented already true and
    // bail out, so markSubmitting (and the busy overlay it drives) would silently
    // never fire for any boosted form — only for the handful with hx-boost="false".
    // Capture always runs before bubble, so this sees the event first.
    document.addEventListener('submit', function (evt) {
      var form = evt.target;
      if (!form || form.tagName !== 'FORM' || evt.defaultPrevented) return;
      if ((form.method || 'GET').toUpperCase() !== 'POST') return;
      if (form.dataset.noSpinner === 'true') return;
      markSubmitting(form, evt.submitter);
    }, true);

    // Poll gating: drop polling requests when tab hidden or when the card's
    // collapsible body is collapsed. Manual / form requests are unaffected.
    function isPollRequest(evt) {
      var cfg = evt.detail && evt.detail.requestConfig;
      if (!cfg) return false;
      var tev = cfg.triggeringEvent;
      // htmx fires an interval-based request without a triggering DOM event.
      if (tev) return false;
      var elt = cfg.elt;
      if (!elt || !elt.getAttribute) return false;
      var trig = elt.getAttribute('hx-trigger') || '';
      return trig.indexOf('every') !== -1;
    }

    function shouldSkipPoll(elt) {
      if (document.hidden) return true;
      var body = elt.querySelector && elt.querySelector(':scope > .collapsible-body');
      if (body && body.style.display === 'none') return true;
      return false;
    }

    document.body.addEventListener('htmx:beforeRequest', function (evt) {
      if (!isPollRequest(evt)) return;
      if (shouldSkipPoll(evt.detail.requestConfig.elt)) {
        evt.preventDefault();
      }
    });

    // Phase jitter: add a 0–4s random offset before each polling element
    // starts its interval, so multiple tabs/clients don't fire in lockstep.
    function jitterPollingElements(root) {
      if (!window.htmx) return;
      var scope = root && root.querySelectorAll ? root : document;
      scope.querySelectorAll('[hx-trigger*="every"]').forEach(function (el) {
        if (el.dataset.pollJittered === 'true') return;
        el.dataset.pollJittered = 'true';
        var orig = el.getAttribute('hx-trigger');
        el.setAttribute('hx-trigger', 'manual');
        window.htmx.process(el);
        setTimeout(function () {
          el.setAttribute('hx-trigger', orig);
          window.htmx.process(el);
        }, Math.floor(Math.random() * 4000));
      });
    }

    // Fire an immediate refresh for every polling element whose card is visible.
    function refreshVisiblePollers() {
      document.querySelectorAll('[hx-trigger*="every"]').forEach(function (el) {
        if (shouldSkipPoll(el)) return;
        if (window.htmx) window.htmx.trigger(el, 'load');
      });
    }

    document.addEventListener('visibilitychange', function () {
      if (!document.hidden) refreshVisiblePollers();
    });

    // Show the top progress bar ONLY for real page navigations (a boosted GET that
    // swaps <main>). Background live-refresh partials (apps-live, vitals, commands,
    // queue, alerts, any polling/morph region) target their own element, not <main> —
    // flashing the orange bar on every telemetry tick read as a page-wide flash even
    // though the content itself morphs in place. Gate on verb+target so those stay silent.
    function isMainNav(e) {
      var d = e.detail || {};
      var verb = (d.requestConfig && d.requestConfig.verb || '').toLowerCase();
      return verb === 'get' && d.target === document.querySelector('main');
    }
    document.body.addEventListener('htmx:beforeRequest', function (e) {
      if (isMainNav(e)) beginProgress();
    });
    // End on any terminal outcome. htmx:abort/timeout are included so an aborted
    // request (Firefox fires these instead of afterRequest) never leaves the body
    // stuck in htmx-requesting-progress.
    ['htmx:afterRequest', 'htmx:responseError', 'htmx:sendError', 'htmx:abort', 'htmx:timeout'].forEach(function (ev) {
      document.body.addEventListener(ev, endProgress);
    });
    // A request that doesn't swap anything (hx-swap="none", or a 204 like the
    // dedup/toast responses) never fires htmx:afterSwap, so a form left marked
    // "submitting" by that request would otherwise sit there — busy overlay
    // included — until the 15s watchdog cleared it. afterRequest fires for every
    // request regardless of whether it swapped, so reset there too.
    ['htmx:afterRequest', 'htmx:responseError', 'htmx:sendError', 'htmx:abort', 'htmx:timeout'].forEach(function (ev) {
      document.body.addEventListener(ev, function (evt) {
        var elt = evt.detail && evt.detail.elt;
        if (elt && elt.tagName === 'FORM' && elt.dataset.submitting === 'true') resetSubmittingForm(elt);
      });
    });
    document.body.addEventListener('htmx:afterSwap', function (evt) {
      var t = evt.detail && evt.detail.target;
      if (!t) return;
      resetSubmitting(t);
      syncPollStamps(t);
      localizeTimes(t);
      // A boosted nav swaps <main> via outerHTML, so evt.detail.target is the
      // DETACHED old <main> — localize the live one too, or its timestamps
      // stay as raw ISO until reload.
      if (t.tagName === 'MAIN') {
        var m = document.querySelector('main');
        if (m && m !== t) { syncPollStamps(m); localizeTimes(m); }
      }
    });

    window.addEventListener('pageshow', function () {
      resetSubmitting(document);
    });

    window.localizeTimes = localizeTimes;
    resetSubmitting(document);
    syncPollStamps(document);
    localizeTimes(document);
    if (window.htmx) {
      jitterPollingElements(document);
    } else {
      window.addEventListener('load', function () { jitterPollingElements(document); });
    }
    document.body.addEventListener('htmx:afterSettle', function (evt) {
      if (evt.detail && evt.detail.target) jitterPollingElements(evt.detail.target);
    });

    // Toast notifications — stacked, dismissible, one call per toast (a second call
    // no longer cuts off whatever was already showing).
    var TOAST_ICONS = {
      success: '<path d="M20 6 9 17l-5-5"/>',
      error: '<path d="M18 6 6 18M6 6l12 12"/>',
      warn: '<path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"/><path d="M12 9v4"/><path d="M12 17h.01"/>',
      info: '<circle cx="12" cy="12" r="9"/><path d="M12 8h.01"/><path d="M11 12h1v4h1"/>'
    };
    function showToast(msg, type) {
      var stack = document.getElementById('toast-stack');
      if (!stack) return;
      type = (type && TOAST_ICONS[type]) ? type : 'info';
      var el = document.createElement('div');
      el.className = 'toast ' + type;
      el.setAttribute('role', type === 'error' ? 'alert' : 'status');
      el.innerHTML = '<span class="toast-ic"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' + TOAST_ICONS[type] + '</svg></span>' +
        '<span class="toast-msg"></span>' +
        '<button type="button" class="toast-x" aria-label="Dismiss">&times;</button>';
      el.querySelector('.toast-msg').textContent = msg;
      stack.appendChild(el);
      requestAnimationFrame(function () { el.classList.add('show'); });
      var timer = setTimeout(dismiss, 4000);
      function dismiss() {
        clearTimeout(timer);
        el.classList.remove('show');
        setTimeout(function () { el.remove(); }, 220);
      }
      el.querySelector('.toast-x').onclick = dismiss;
    }
    window.showToast = showToast;

    // Copy text to the clipboard with a toast + brief button feedback. Falls back to a
    // hidden textarea + execCommand where the async Clipboard API is unavailable
    // (older/insecure-origin browsers). Usage: copyText(theText, theButton).
    function copyText(text, btn) {
      function done(ok) {
        showToast(ok ? 'Copied to clipboard' : 'Copy failed', ok ? 'success' : 'error');
        if (ok && btn) {
          var prev = btn.textContent;
          btn.textContent = 'Copied';
          setTimeout(function () { btn.textContent = prev; }, 1500);
        }
      }
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function () { done(true); }, function () { done(false); });
        return;
      }
      try {
        var ta = document.createElement('textarea');
        ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
        document.body.appendChild(ta); ta.select();
        var ok = document.execCommand('copy');
        document.body.removeChild(ta);
        done(ok);
      } catch (e) { done(false); }
    }
    window.copyText = copyText;

    // Copy every device serial in a region, for any list that shows more than one
    // device. Mark the button with data-copy-serials="<css selector for the region>"
    // (empty = the button's nearest card/section). Serials are read from [data-serial]
    // when a list carries them, otherwise from the /devices/<serial> links the row
    // already has, deduped in document order — so a list needs no extra markup.
    function collectSerials(scope) {
      var out = [], seen = {};
      function push(s) {
        s = (s || '').trim();
        if (!s || seen[s]) return;
        seen[s] = true; out.push(s);
      }
      scope.querySelectorAll('[data-serial]').forEach(function (el) { push(el.getAttribute('data-serial')); });
      if (out.length === 0) {
        scope.querySelectorAll('a[href*="/devices/"]').forEach(function (a) {
          var m = (a.getAttribute('href') || '').match(/\/devices\/([^\/?#]+)/);
          if (m) push(decodeURIComponent(m[1]));
        });
      }
      return out;
    }
    function copySerials(scope, btn) {
      var list = collectSerials(scope);
      if (list.length === 0) { showToast('No device serials in this list', 'info'); return; }
      var label = btn && (btn.querySelector('.cs-lbl') || btn);
      function done(ok) {
        showToast(ok ? 'Copied ' + list.length + ' serial' + (list.length === 1 ? '' : 's') : 'Copy failed', ok ? 'success' : 'error');
        if (ok && label) {
          var prev = label.textContent;
          label.textContent = 'Copied';
          setTimeout(function () { label.textContent = prev; }, 1500);
        }
      }
      var text = list.join('\n');
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function () { done(true); }, function () { done(false); });
        return;
      }
      try {
        var ta = document.createElement('textarea');
        ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
        document.body.appendChild(ta); ta.select();
        var ok = document.execCommand('copy');
        document.body.removeChild(ta);
        done(ok);
      } catch (e) { done(false); }
    }
    window.copySerials = copySerials;
    // Delegated, so buttons inside live-refreshed regions keep working after a swap.
    document.body.addEventListener('click', function (e) {
      var btn = e.target.closest && e.target.closest('[data-copy-serials]');
      if (!btn) return;
      e.preventDefault();
      var sel = btn.getAttribute('data-copy-serials');
      var scope = (sel && document.querySelector(sel)) || btn.closest('.cc-card, section, main') || document.body;
      copySerials(scope, btn);
    });

    // Flash-via-URL: a redirect (which swallows HX-Trigger toasts) can carry
    // ?flash=<msg>&flash_type=<t>; show it once, then strip it from the address bar.
    // Runs on load and after each boosted navigation.
    function mdmFlash() {
      try {
        var u = new URL(location.href), m = u.searchParams.get('flash');
        if (!m) return;
        showToast(m, u.searchParams.get('flash_type') || 'info');
        u.searchParams.delete('flash'); u.searchParams.delete('flash_type');
        history.replaceState(null, '', u.pathname + (u.searchParams.toString() ? '?' + u.searchParams.toString() : '') + u.hash);
      } catch (e) {}
    }
    document.addEventListener('DOMContentLoaded', mdmFlash);
    document.body.addEventListener('htmx:pushedIntoHistory', mdmFlash);

    // Server-driven toasts: handlers set HX-Trigger {"toast":{"msg","type"}} and
    // htmx dispatches a `toast` event on the body. Used for hx-request feedback.
    document.body.addEventListener('toast', function (e) {
      var d = (e && e.detail) || {};
      showToast(d.msg || 'Done', d.type || 'info');
    });

    document.body.addEventListener('htmx:afterRequest', function (evt) {
      var tgt = evt.detail.target;
      if (!tgt || !tgt.closest) return;
      // Only confirm actual mutations. GET fetches (e.g. hx-trigger="load" partials
      // that populate a form) must not pop a "Done" toast just for opening the page.
      var verb = ((evt.detail.requestConfig && evt.detail.requestConfig.verb) || '').toLowerCase();
      if (verb === '' || verb === 'get') return;
      // Skip polling / SSE triggers
      var form = tgt.closest('form');
      if (!form) return;
      if (form.dataset.noToast === 'true') return;
      var xhr = evt.detail.xhr;
      if (!xhr) return;
      if (xhr.status >= 200 && xhr.status < 300) {
        showToast('Done', 'success');
      } else if (xhr.status >= 400) {
        showToast('Something went wrong', 'error');
      }
    });

    // Modal a11y: dialog semantics, focus trap, ESC to close, focus return
    var FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
    var modalLastFocus = new WeakMap();

    function initModal(m) {
      if (m.dataset.a11yReady === 'true') return;
      m.dataset.a11yReady = 'true';
      m.setAttribute('role', 'dialog');
      m.setAttribute('aria-modal', 'true');
      var title = m.querySelector('.modal-title');
      if (title) {
        if (!title.id) title.id = 'modal-title-' + Math.random().toString(36).slice(2, 8);
        m.setAttribute('aria-labelledby', title.id);
      }
    }

    function openModal(m) {
      if (!m) return;
      modalLastFocus.set(m, document.activeElement);
      // Focus first focusable inside modal
      requestAnimationFrame(function () {
        var first = m.querySelector(FOCUSABLE);
        if (first) first.focus();
      });
    }

    function closeModal(m) {
      var last = modalLastFocus.get(m);
      if (last && typeof last.focus === 'function') last.focus();
    }

    document.querySelectorAll('.modal-backdrop').forEach(initModal);

    // Watch for modal open/close transitions
    new MutationObserver(function (records) {
      records.forEach(function (r) {
        if (r.attributeName !== 'class') return;
        var el = r.target;
        if (!el.classList.contains('modal-backdrop')) return;
        var wasOpen = (r.oldValue || '').indexOf('modal-open') !== -1;
        var isOpen = el.classList.contains('modal-open');
        if (!wasOpen && isOpen) openModal(el);
        if (wasOpen && !isOpen) closeModal(el);
      });
    }).observe(document.body, {
      subtree: true,
      attributes: true,
      attributeOldValue: true,
      attributeFilter: ['class'],
    });

    // ESC closes any open modal; Tab is trapped inside it
    document.addEventListener('keydown', function (e) {
      var open = document.querySelector('.modal-backdrop.modal-open');
      if (!open) return;
      if (e.key === 'Escape') {
        e.preventDefault();
        open.classList.remove('modal-open');
        return;
      }
      if (e.key !== 'Tab') return;
      var focusables = open.querySelectorAll(FOCUSABLE);
      if (!focusables.length) return;
      var first = focusables[0], last = focusables[focusables.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    });

    // Initialize any modals injected later (HTMX swap)
    document.body.addEventListener('htmx:afterSettle', function (evt) {
      var root = (evt.detail && evt.detail.target) || document;
      if (root.querySelectorAll) root.querySelectorAll('.modal-backdrop').forEach(initModal);
    });
  })();
  

  // ── Command palette (⌘K / Ctrl+K) ───────────────────────────────────────────
  // A keyboard-first jump bar. Destinations are read from the rendered sidebar,
  // so whatever the current user is allowed to see is exactly what the palette
  // offers — no separate, drift-prone route list, and role gating is automatic.
  // Groups, venues, people and builds come from /cmdk-index; devices are searched
  // on the server as you type. Everything is ranked together, so a device whose
  // name you typed is not stuck below a page that merely contains the word.
  (function () {
    var backdrop = document.getElementById('cmdk');
    var input = document.getElementById('cmdk-input');
    var list = document.getElementById('cmdk-list');
    var empty = document.getElementById('cmdk-empty');
    var panel = backdrop && backdrop.querySelector('.cmdk-panel');
    if (!backdrop || !input || !list) return;

    var dests = [];   // {label, sub, url, type}
    var items = [];   // current filtered results, in render order
    var active = 0;   // index into items
    var lastFocus = null;

    function collectDests() {
      dests = [];
      // Pages are marked with .js-dest: the dock, the account/alerts menus, and
      // #page-index — the hidden directory that carries every route with its
      // server-side role guard already applied.
      var byUrl = {};
      document.querySelectorAll('.js-dest').forEach(function (a) {
        var label = a.getAttribute('data-cmdk-label') || (a.textContent || '').replace(/\s+/g, ' ').trim();
        var url = a.getAttribute('href') || a.getAttribute('data-cmdk-href');
        if (!url) return;
        var sec = a.getAttribute('data-cmdk-sec') || '';
        // The dock and the index both point at /devices. Keep one row per URL,
        // preferring the one that knows its section.
        var seen = byUrl[url];
        if (seen) { if (sec && !seen.sub) { seen.sub = sec; seen.label = label; } return; }
        var d = { label: label, sub: sec, url: url, type: 'page' };
        byUrl[url] = d; dests.push(d);
      });
    }

    // Row icons, by kind (outline, the dock's stroke style).
    var ICONS = {
      device: '<rect x="5" y="2.5" width="14" height="19" rx="2"/><path d="M11 18h2"/>',
      screen: '<rect x="2.5" y="4" width="19" height="13" rx="1.5"/><path d="M8 20h8"/>',
      Restaurant: '<path d="M3 21h18"/><path d="M5 21V8l7-5 7 5v13"/><path d="M10 21v-6h4v6"/>',
      Group: '<path d="m12 3 9 5-9 5-9-5z"/><path d="m3 13 9 5 9-5"/>',
      page: '<path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><path d="M14 3v6h6"/>',
      User: '<circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/>',
      Build: '<path d="M20.6 13.4 13.4 20.6a2 2 0 0 1-2.8 0L3 13V3h10l7.6 7.6a2 2 0 0 1 0 2.8z"/><circle cx="7.5" cy="7.5" r="1.5"/>',
      recent: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
      browse: '<path d="M3 7h18M3 12h18M3 17h18"/>'
    };
    var KIND = { page: 'Page', Restaurant: 'Venue', Group: 'Group', User: 'Person', Build: 'Build' };
    // Classes whose hardware is a screen on a wall rather than a handheld.
    var SCREEN_CLASSES = { dongle: 1, kds: 1, kiosk: 1 };

    function iconFor(it) {
      var k = it.type === 'device'
        ? (it.dev && SCREEN_CLASSES[it.dev.class_key] ? 'screen' : 'device')
        : it.browse ? 'browse' : (it.recent && it.type === 'page') ? 'recent' : it.type;
      var span = document.createElement('span');
      span.className = 'cmdk-ic';
      span.innerHTML = '<svg viewBox="0 0 24 24" aria-hidden="true">' + (ICONS[k] || ICONS.page) + '</svg>';
      return span;
    }

    // text with q marked: a contiguous hit, else (fuzzy) each character of an
    // in-order subsequence. Built from text nodes — labels are user data.
    function marked(text, q, fuzzy) {
      var frag = document.createDocumentFragment();
      text = text || '';
      var t = text.toLowerCase(), ql = (q || '').toLowerCase();
      var at = ql ? t.indexOf(ql) : -1;
      function add(str, hit) {
        if (!str) return;
        var n = hit ? document.createElement('mark') : document.createTextNode(str);
        if (hit) n.textContent = str;
        frag.appendChild(n);
      }
      if (at >= 0) {
        add(text.slice(0, at)); add(text.slice(at, at + ql.length), true); add(text.slice(at + ql.length));
      } else if (ql && fuzzy && subseq(ql, t)) {
        var i = 0, run = '', hit = false;
        for (var j = 0; j < text.length; j++) {
          var h = i < ql.length && t[j] === ql[i];
          if (h) i++;
          if (h !== hit) { add(run, hit); run = ''; hit = h; }
          run += text[j];
        }
        add(run, hit);
      } else {
        add(text);
      }
      return frag;
    }

    function ago(iso) {
      var t = Date.parse(iso || '');
      if (!t || t < 86400000) return '';
      var m = Math.max(1, Math.round((Date.now() - t) / 60000));
      return m < 60 ? m + ' m' : m < 2880 ? Math.round(m / 60) + ' h' : Math.round(m / 1440) + ' d';
    }

    function right(it) {
      var rt = document.createElement('span');
      rt.className = 'cmdk-rt';
      if (it.type === 'device' && it.dev && it.dev.serial) {
        var on = !!it.dev.online, since = on ? '' : ago(it.dev.last_seen);
        rt.appendChild(document.createTextNode(on ? 'online' : since ? 'offline ' + since : 'offline'));
        var dot = document.createElement('span');
        dot.className = 'cmdk-dot' + (on ? ' on' : '');
        rt.appendChild(dot);
      } else {
        var k = document.createElement('span');
        k.className = 'cmdk-kind';
        // Recents sit under their own heading, so the label stays the kind.
        k.textContent = it.browse ? it.sub + ' ›' : (KIND[it.type] || '');
        rt.appendChild(k);
      }
      return rt;
    }

    var rows = []; // option <li> per item, by index (headers are not options)
    function render() {
      list.innerHTML = '';
      rows = [];
      var q = current();
      items.forEach(function (it, i) {
        if (it.head) {
          var h = document.createElement('li');
          h.className = 'cmdk-head';
          h.setAttribute('role', 'presentation');
          h.textContent = it.head;
          list.appendChild(h);
        }
        var li = document.createElement('li');
        li.id = 'cmdk-opt-' + i;
        li.setAttribute('role', 'option');
        li.setAttribute('aria-selected', i === active ? 'true' : 'false');
        if (it.type === 'pending') {
          li.className = 'cmdk-item cmdk-rich cmdk-pending' + (i === active ? ' active' : '');
          var sp = document.createElement('span'); sp.className = 'cmdk-spin'; li.appendChild(sp);
          var pl = document.createElement('span'); pl.className = 'cmdk-l1';
          pl.textContent = pendingEnter ? 'Searching devices… Enter opens the top match' : 'Searching devices…';
          li.appendChild(pl);
        } else {
          li.className = 'cmdk-item cmdk-rich' + (i === active ? ' active' : '');
          li.appendChild(iconFor(it));
          var tx = document.createElement('span'); tx.className = 'cmdk-tx';
          var l1 = document.createElement('span'); l1.className = 'cmdk-l1';
          l1.appendChild(marked(it.label, it.browse || it.recent ? '' : q, true));
          tx.appendChild(l1);
          var sub = it.type === 'device' ? it.l2 : it.browse ? '' : it.sub;
          if (sub) {
            var l2 = document.createElement('span'); l2.className = 'cmdk-l2';
            l2.appendChild(marked(sub, it.recent ? '' : q, false));
            tx.appendChild(l2);
          }
          li.appendChild(tx);
          li.appendChild(right(it));
          var ent = document.createElement('span'); ent.className = 'cmdk-ent'; ent.setAttribute('aria-hidden', 'true'); ent.textContent = '↵';
          li.appendChild(ent);
        }
        li.addEventListener('mousemove', function () { setActive(i); });
        li.addEventListener('click', function (e) { choose(i, true, e.shiftKey || e.metaKey || e.ctrlKey); });
        list.appendChild(li);
        rows.push(li);
      });
      empty.hidden = items.length > 0;
      input.setAttribute('aria-activedescendant', items.length ? 'cmdk-opt-' + active : '');
    }

    // The entity index (groups, venues, people, builds). It is kept between opens
    // and refreshed in the background on each one, so only the very first open of a
    // page load waits for it — and that one is usually warmed by the idle prefetch.
    var entities = [], entitiesAt = 0, entitiesLoading = false;
    function loadEntities() {
      if (entitiesLoading || Date.now() - entitiesAt < 15000 || location.pathname.indexOf('/sneak-peek') === 0) return;
      entitiesLoading = true;
      fetch('/cmdk-index', { headers: { 'Accept': 'application/json' } })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (e) {
          entitiesLoading = false;
          if (!e) return;
          entities = e; entitiesAt = Date.now();
          if (!backdrop.hidden && !pagesOnly) filter(input.value, true);
        })
        .catch(function () { entitiesLoading = false; });
    }

    // Fuzzy subsequence: do q's characters appear in order within text?
    function subseq(q, text) {
      var i = 0;
      for (var j = 0; j < text.length && i < q.length; j++) { if (text[j] === q[i]) i++; }
      return i === q.length;
    }
    // How well ql (lower-case) matches text: 0 exact, 1 prefix, 2 a word starts
    // with it, 3 substring, 5 fuzzy (only when allowed), -1 no match. Devices,
    // pages and entities are all scored with this, so the list reads as one ranking.
    function score(ql, text, fuzzy) {
      var t = (text || '').toLowerCase();
      if (!t) return -1;
      if (t === ql) return 0;
      var at = t.indexOf(ql);
      if (at === 0) return 1;
      if (at > 0) return /[\s\-_.·\/(]/.test(t[at - 1]) ? 2 : 3;
      return fuzzy && subseq(ql, t) ? 5 : -1;
    }
    // A build is offered only to a query that looks like a version ("v2.1", "1.025"),
    // so digits typed for a serial are never answered with a column of builds.
    function looksLikeVersion(q) { return /^v\d|\d\.\d/i.test(q); }

    function scoreEntry(ql, d) {
      if (d.type === 'Build') {
        var b = score(ql, d.label, false);
        return looksLikeVersion(ql) && b >= 0 ? b : -1;
      }
      var s = score(ql, d.label, true);
      // The second line counts as a match for pages (their section) and people
      // (role, username); for venues and groups it is a device count.
      if (s < 0 && (d.type === 'page' || d.type === 'User') && score(ql, d.sub, false) >= 0) s = 4;
      return s;
    }

    function scoreDevice(ql, d) {
      var best = -1;
      [score(ql, d.nickname, false), score(ql, d.serial, true)].forEach(function (s) {
        if (s >= 0 && (best < 0 || s < best)) best = s;
      });
      if (best >= 0) return best;
      // Matched on something not shown in the label: its IP (rank 1/4), or its venue.
      return d.rank === 1 ? 1 : d.rank === 4 ? 3 : 6;
    }

    // Pages + indexed entities, ranked by score, then (on a tie) devices first for a
    // query with a digit in it — that is a serial or an IP — and pages first otherwise.
    function rank(q, devs) {
      var ql = q.toLowerCase();
      var seenUrl = {}; entities.forEach(function (e) { seenUrl[e.url] = true; }); // indexed entries carry a subtitle; drop the bare page link for the same URL
      var digit = /\d/.test(q);
      var out = [];
      dests.filter(function (d) { return !seenUrl[d.url]; }).forEach(function (d, i) {
        var s = scoreEntry(ql, d); if (s >= 0) out.push({ it: d, s: s, k: digit ? 2 : 0, i: i });
      });
      entities.forEach(function (d, i) {
        var s = scoreEntry(ql, d); if (s >= 0) out.push({ it: d, s: s, k: 1, i: i });
      });
      (devs || []).forEach(function (d, i) {
        out.push({ it: deviceItem(d), s: scoreDevice(ql, d), k: digit ? 0 : 2, i: i });
      });
      out.sort(function (a, b) { return a.s - b.s || a.k - b.k || a.i - b.i; });
      // Twelve rows of pages and entities at most, and every device the server sent.
      var n = 0;
      return out.filter(function (o) { return o.it.type === 'device' || ++n <= 12; })
        .map(function (o) { return o.it; });
    }

    function deviceItem(d, recent) {
      var l2 = [];
      if (d.nickname) l2.push(d.serial);
      if (d.ip) l2.push('IP ' + d.ip);
      if (d.restaurant) l2.push(d.restaurant);
      if (d.class) l2.push(d.class);
      if (d.build) l2.push(d.build);
      return {
        label: d.nickname || d.serial, l2: l2.join(' · '),
        url: '/devices/' + encodeURIComponent(d.serial), type: 'device', dev: d, recent: recent
      };
    }

    // pagesOnly is the mode "More" opens in: the same palette, scoped to
    // navigation. No device lookup, no entity index — just the ~28 pages, which
    // is a surface whose size does not change when the route table does.
    var pagesOnly = false;

    // Recently visited pages and devices, newest first. A device page is kept as
    // its serial; its state is filled in by recentDevices() once it is known.
    var recentDevs = {}; // serial -> search hit, for the open palette
    function recentItems(withDevices) {
      var byUrl = {}; dests.forEach(function (d) { byUrl[d.url] = d; });
      var out = [];
      (window.mdmRecentPages ? window.mdmRecentPages() : []).forEach(function (r) {
        if (out.length >= 5 || r.url === location.pathname) return;
        var m = /^\/devices\/([^\/?#]+)$/.exec(r.url);
        if (m && withDevices) {
          var serial = decodeURIComponent(m[1]), hit = recentDevs[serial];
          out.push(hit ? deviceItem(hit, true) : { label: serial, l2: '', url: r.url, type: 'device', recent: true, dev: { serial: '' }, serial: serial });
        } else if (byUrl[r.url]) {
          var d = byUrl[r.url];
          out.push({ label: d.label, sub: d.sub, url: d.url, type: 'page', recent: true });
        }
      });
      return out;
    }
    // Look the recent devices up once per open, for their state and venue. A device
    // the viewer can no longer see does not come back, and drops out of the list.
    function recentDevices() {
      var serials = recentItems(true).filter(function (it) { return it.type === 'device'; })
        .map(function (it) { return it.serial || it.dev.serial; });
      if (!serials.length) return;
      fetch('/devices/search?serials=' + encodeURIComponent(serials.join(',')), { headers: { 'Accept': 'application/json' } })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (devs) {
          if (!devs) return;
          recentDevs = {};
          devs.forEach(function (d) { recentDevs[d.serial] = d; });
          // Not visible (or gone): forget it rather than offer a dead row.
          serials.forEach(function (s) { if (!recentDevs[s]) recentDevs[s] = null; });
          if (!backdrop.hidden && !current()) filter('', true);
        })
        .catch(function () {});
    }

    // The empty state. Recents first (where you have been is the best guess at
    // where you are going), then — in full mode — every page under one heading;
    // in pages mode the sections as one row each, so someone who does not yet know
    // what anything is called can still browse.
    function emptyState() {
      var out = recentItems(!pagesOnly).filter(function (it) {
        return !(it.type === 'device' && recentDevs[it.serial] === null);
      });
      if (!pagesOnly) {
        var rec = {}; out.forEach(function (it) { rec[it.url] = true; });
        dests.forEach(function (d, i) {
          if (rec[d.url]) return;
          var it = { label: d.label, sub: d.sub, url: d.url, type: 'page' };
          out.push(it);
        });
        var first = out.filter(function (it) { return !it.recent; })[0];
        if (first && out[0] !== first) first.head = 'All pages';
        if (out[0] && out[0].recent) out[0].head = 'Recent';
        return out;
      }
      var secs = [], counts = {};
      dests.forEach(function (d) {
        if (!d.sub) return;
        if (!counts[d.sub]) { counts[d.sub] = 0; secs.push(d.sub); }
        counts[d.sub]++;
      });
      secs.forEach(function (name, i) {
        out.push({ label: name, sub: counts[name] + (counts[name] === 1 ? ' page' : ' pages'), browse: name, type: 'page', head: i === 0 && out.length ? 'Sections' : '' });
      });
      return out;
    }

    // The device search for the current query. Enter waits on it (see onEnter):
    // answering before it is back is how "231" + a quick Enter used to land on a
    // dead /devices/231 while the device it was about to find was a few ms away.
    var dev = { q: '', state: 'idle', hits: [] }; // state: idle | wait | done
    var devTimer = null, devSeq = 0;
    var pendingEnter = false; // Enter pressed while dev.state === 'wait'
    var picked = false;       // the user arrowed to a row: Enter means that row

    function current() { return input.value.trim(); }

    // keep: re-rank in place (entity index or device results arrived) without
    // moving the user's selection or abandoning a pending Enter.
    function filter(raw, keep) {
      var q = (raw || '').trim();
      if (!keep) { pendingEnter = false; picked = false; }
      if (pagesOnly) {
        if (!q) { items = emptyState(); active = 0; render(); return; }
        var ql = q.toLowerCase();
        items = dests.map(function (d, i) { return { it: d, s: scoreEntry(ql, d), i: i }; })
          .filter(function (o) { return o.s >= 0; })
          .sort(function (a, b) { return a.s - b.s || a.i - b.i; })
          .slice(0, 12).map(function (o) { return o.it; });
        active = 0; render();
        return;
      }
      if (!q) {
        dev = { q: '', state: 'idle', hits: [] };
        if (devTimer) { clearTimeout(devTimer); devTimer = null; }
        items = emptyState(); active = 0; render();
        return;
      }
      if (dev.q !== q) {
        // A new query: the old results describe something else, so drop them.
        dev = { q: q, state: q.length >= 2 ? 'wait' : 'idle', hits: [] };
        if (devTimer) clearTimeout(devTimer);
        devTimer = dev.state === 'wait' ? setTimeout(fetchDevices, 120) : null;
      }
      var prev = keep && items[active] && items[active].url;
      items = rank(q, dev.hits);
      if (dev.state === 'wait') items.push({ label: '', type: 'pending' });
      active = 0;
      if (keep && picked && prev) {
        for (var i = 0; i < items.length; i++) if (items[i].url === prev) { active = i; break; }
      }
      render();
    }

    // Fuzzy device search: the server subsequence-matches the serial (so "866" or
    // "at866" both find AT070AABU00866), substring-matches nickname, IP and venue,
    // and returns the ranked top matches the viewer is allowed to see.
    function fetchDevices() {
      devTimer = null;
      var q = dev.q, tok = ++devSeq;
      fetch('/devices/search?q=' + encodeURIComponent(q), { headers: { 'Accept': 'application/json' } })
        .then(function (r) { return r.ok ? r.json() : []; })
        .catch(function () { return []; })
        .then(function (devs) {
          // Stale (a newer query is out) or closed: a late answer must never repaint
          // the list for a query the input no longer holds.
          if (tok !== devSeq || backdrop.hidden || dev.q !== q || current() !== q) return;
          dev.state = 'done'; dev.hits = devs || [];
          filter(input.value, true);
          if (pendingEnter) { pendingEnter = false; if (items.length) choose(active); }
        });
    }

    function setActive(i) {
      if (i === active) return;
      active = i;
      rows.forEach(function (li, idx) {
        var on = idx === active;
        li.classList.toggle('active', on);
        li.setAttribute('aria-selected', on ? 'true' : 'false');
      });
      input.setAttribute('aria-activedescendant', items.length ? 'cmdk-opt-' + active : '');
    }

    function move(delta) {
      if (!items.length) return;
      var n = items.length;
      picked = true;
      setActive((active + delta + n) % n);
      var el = rows[active];
      if (el && el.scrollIntoView) el.scrollIntoView({ block: 'nearest' });
    }

    // Enter takes the top row — but while the device search for this exact query is
    // still out, the top row is not known yet. Wait for it (firing it now if it was
    // still in its debounce) and take the top row of the answer. A row the user
    // arrowed to is a choice already made and goes straight through.
    function onEnter(newTab) {
      var it = items[active];
      if (dev.state === 'wait' && dev.q === current() && !(picked && it && it.type !== 'pending')) {
        pendingEnter = true;
        render();
        if (devTimer) { clearTimeout(devTimer); fetchDevices(); }
        return;
      }
      choose(active, false, newTab);
    }

    function choose(i, clicked, newTab) {
      var it = items[i];
      if (!it) return;
      if (it.type === 'pending') { if (clicked) onEnter(); return; }
      // A section row is a filter, not a destination: it types its own name.
      if (it.browse) {
        input.value = it.browse;
        filter(input.value);
        input.focus();
        return;
      }
      close();
      // Opened on the next tick, outside the key event: Shift is the browser's own "new
      // window" modifier, and a window.open() inside a Shift+Enter keydown inherits it.
      if (newTab) { var u = it.url; setTimeout(function () { window.open(u, '_blank', 'noopener'); }, 0); return; }
      window.location.href = it.url;
    }

    var PLACEHOLDER_ALL = input.getAttribute('placeholder');
    function open(mode) {
      if (!backdrop.hidden) return;
      pagesOnly = (mode === 'pages');
      lastFocus = document.activeElement;
      collectDests();
      // The entity index (groups, venues, people) is only useful in full mode.
      if (!pagesOnly) { loadEntities(); recentDevs = {}; recentDevices(); }
      input.value = '';
      dev = { q: '', state: 'idle', hits: [] };
      input.setAttribute('placeholder', pagesOnly ? 'Go to a page…' : PLACEHOLDER_ALL);
      panel.classList.toggle('cmdk-pages', pagesOnly);
      filter('');
      backdrop.hidden = false;
      requestAnimationFrame(function () {
        backdrop.classList.add('open');
        input.focus();
      });
    }

    function close() {
      if (backdrop.hidden) return;
      pendingEnter = false;
      backdrop.classList.remove('open');
      backdrop.hidden = true;
      if (lastFocus && typeof lastFocus.focus === 'function') lastFocus.focus();
    }

    window.mdmOpenPalette = open;

    input.addEventListener('input', function () { filter(input.value); });

    input.addEventListener('keydown', function (e) {
      if (e.isComposing) return;
      if (e.key === 'ArrowDown') { e.preventDefault(); move(1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); move(-1); }
      // Shift+Enter (or ⌘/Ctrl+Enter) opens the destination in a new tab, like a link.
      else if (e.key === 'Enter') { e.preventDefault(); onEnter(e.shiftKey || e.metaKey || e.ctrlKey); }
      else if (e.key === 'Escape') { e.preventDefault(); close(); }
      // Trap Tab within the open palette: the input is its only focusable control
      // (results are arrow-navigated), so keep focus here instead of escaping to the
      // page behind the overlay (WCAG 2.4.3).
      else if (e.key === 'Tab') { e.preventDefault(); }
    });

    // Click the dimmed backdrop (but not the panel) to dismiss.
    backdrop.addEventListener('mousedown', function (e) {
      if (e.target === backdrop) close();
    });

    // Warm the entity index once the page has settled, so the first ⌘K of a page
    // load already has its groups and venues.
    if (document.body.classList.contains('has-dock')) {
      (window.requestIdleCallback || function (f) { setTimeout(f, 1500); })(loadEntities);
    }

    // Global shortcut: ⌘K (mac) / Ctrl+K (win/linux), and "/" when not typing.
    document.addEventListener('keydown', function (e) {
      var k = e.key.toLowerCase();
      // Esc closes the palette wherever focus is (a clicked result, the backdrop), not
      // only while the caret is in its input.
      if (e.key === 'Escape' && !backdrop.hidden) { e.preventDefault(); close(); return; }
      if ((e.metaKey || e.ctrlKey) && k === 'k') {
        e.preventDefault();
        // ⌘K on an already-open pages-mode palette widens it to everything,
        // rather than closing the thing the user just asked for more of.
        if (!backdrop.hidden && pagesOnly) { close(); open(); return; }
        backdrop.hidden ? open() : close();
      }
    });
  })();
  

  // ── Live alerts counter ──────────────────────────────────────────────────
  // One EventSource per page owns the alerts feed: it keeps the sidebar's open
  // alert badge current and re-dispatches a DOM event so the alerts list (when
  // open) refreshes from the same signal — one connection, and never a toast.
  // The badge pulses only when the count rises (a new actionable event), staying
  // quiet when alerts are resolved.
  (function () {
    if (!window.EventSource) return;
    var navLink = document.getElementById('alerts-indicator');
    var known = null; // last displayed count; null until the first sync

    function setBadge(n) {
      if (!navLink) return;
      var badge = navLink.querySelector('.nav-badge');
      if (n > 0) {
        if (!badge) {
          badge = document.createElement('span');
          badge.className = 'nav-badge';
          navLink.appendChild(badge);
        }
        badge.textContent = n;
      } else if (badge) {
        badge.remove();
      }
    }

    function pulse() {
      if (!navLink) return;
      var badge = navLink.querySelector('.nav-badge');
      if (!badge) return;
      badge.classList.remove('nav-badge-pulse');
      navLink.classList.remove('m-bob');
      void badge.offsetWidth; // restart the animation
      badge.classList.add('nav-badge-pulse');
      navLink.classList.add('m-bob'); // motion.css: the bell itself bobs twice
    }

    if (!document.getElementById('alerts-wrap')) return; // no bell (restaurant owner) → no stream
    if (location.pathname.indexOf('/sneak-peek') === 0) return; // preview: no session, no stream
    var es = new EventSource('/alerts/events');
    es.addEventListener('alert-update', function (e) {
      var n = parseInt(e.data, 10);
      if (isNaN(n)) return;
      var rose = known !== null && n > known;
      setBadge(n);
      if (rose) pulse();
      known = n;
      document.dispatchEvent(new CustomEvent('mdm:alerts-update', { detail: { count: n } }));
      // Keep the dropdown's content current in the background — refreshAlertsList
      // itself refuses to touch it while open, so this never causes the
      // mutate-while-visible flicker (see navAlerts).
      if (window.refreshAlertsList) window.refreshAlertsList();
    });
  })();

  // Friendly toast when a NEW critical alert arrives. Listens to the same signal;
  // on a count rise it fetches the most urgent alert and pops it once (deduped).
  (function () {
    var seen = {}, last = null, box = null;
    function ensure() {
      if (!box) { box = document.createElement('div'); box.id = 'mdm-toasts'; document.body.appendChild(box); }
      return box;
    }
    function show(a) {
      // With a dock, the dock itself turns into the alert (the "island"); the toast
      // is the fallback for pages without one.
      if (window.mdmIsland && window.mdmIsland(a)) return;
      var el = document.createElement('div');
      el.className = 'mtoast';
      el.innerHTML = '<span class="mt-i"></span><div class="mt-m"><div class="mt-t"></div><div class="mt-s"></div><div class="mt-a"></div></div><button class="mt-x" aria-label="Dismiss">×</button>';
      el.querySelector('.mt-t').textContent = a.headline;
      el.querySelector('.mt-s').textContent = a.sentence;
      var av = el.querySelector('.mt-a');
      if (a.href) { var l = document.createElement('a'); l.className = 'mt-b p'; l.href = a.href; l.textContent = 'View'; av.appendChild(l); }
      var all = document.createElement('a'); all.className = 'mt-b'; all.href = '/alerts'; all.textContent = 'All alerts'; av.appendChild(all);
      el.querySelector('.mt-x').onclick = function () { el.remove(); };
      ensure().appendChild(el);
      setTimeout(function () { el.classList.add('leaving'); setTimeout(function () { el.remove(); }, 400); }, 9000);
    }
    // Exposed so Settings → Debug tools can preview this without a real alert firing.
    window.mdmShowAlertToast = show;
    document.addEventListener('mdm:alerts-update', function (e) {
      var n = e.detail && e.detail.count;
      var rose = last !== null && n > last; last = n;
      if (!rose) return;
      fetch('/alerts/newest', { credentials: 'same-origin' })
        .then(function (r) { return r.json(); })
        .then(function (a) { if (a && a.severity === 'critical' && !seen[a.id]) { seen[a.id] = 1; show(a); } })
        .catch(function () {});
    });
  })();
  

  // ── Live dock ────────────────────────────────────────────────────────────
  // The dock reports on the fleet: an online ring on Fleet and the open-alert count
  // on Alerts (pulsing only while something is critical). Figures come from
  // /dock/status (server-cached 15 s), fetched on load, every 30 s while the tab is
  // visible, and whenever the alerts stream ticks.
  (function () {
    var dock = document.querySelector('.dock');
    if (!dock || !window.fetch) return;
    var busy = false;
    function el(label, cls) {
      var a = dock.querySelector('a.nav-link[data-cmdk-label="' + label + '"]');
      return a && a.querySelector('.' + cls);
    }
    // A ring around the Fleet icon, filled to the online share.
    function ring(on, total) {
      var r = 19, c = 2 * Math.PI * r, f = total ? on / total : 0;
      return '<svg viewBox="0 0 42 42" aria-hidden="true"><circle cx="21" cy="21" r="' + r + '" class="trk"/>'
        + '<circle cx="21" cy="21" r="' + r + '" class="val" stroke-dasharray="' + (c * f).toFixed(2) + ' ' + c.toFixed(2) + '" transform="rotate(-90 21 21)"/></svg>';
    }
    // The hover label carries the figure ("Fleet · 54 of 151 online"); the name
    // itself is already under the icon.
    function tip(label, text) {
      var a = dock.querySelector('a.nav-link[data-cmdk-label="' + label + '"]');
      if (!a) return;
      if (text) a.setAttribute('data-tip', label + ' · ' + text); else a.removeAttribute('data-tip');
    }
    function paint(s) {
      var f = el('Fleet', 'dock-ring'), a = el('Alerts', 'dock-count');
      if (f) {
        f.hidden = !s.fleet || !s.total;
        if (!f.hidden) f.innerHTML = ring(s.online, s.total);
        tip('Fleet', f.hidden ? '' : s.online + ' of ' + s.total + ' online');
      }
      if (a) {
        a.hidden = !s.alerts;
        a.textContent = s.alerts;
        a.classList.toggle('crit', s.critical > 0);
        tip('Alerts', s.alerts ? s.alerts + ' open' + (s.critical ? ', ' + s.critical + ' critical' : '') : '');
      }
    }
    function load() {
      if (busy || document.hidden || location.pathname.indexOf('/sneak-peek') === 0) return;
      busy = true;
      fetch('/dock/status', { credentials: 'same-origin', headers: { Accept: 'application/json' } })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (s) { if (s) paint(s); })
        .catch(function () {})
        .then(function () { busy = false; });
    }
    load();
    setInterval(load, 30000);
    document.addEventListener('visibilitychange', load);
    document.addEventListener('mdm:alerts-update', load);
  })();

  // ── Island ───────────────────────────────────────────────────────────────
  // When a new critical alert arrives the dock morphs into it for a few seconds —
  // headline, one line, View — then shrinks back into the navigation. Hovering
  // holds it open. Returns false when there is no dock, so the caller can toast.
  (function () {
    var dock = document.querySelector('.dock');
    if (!dock) return;
    var box = document.createElement('div'), timer = null;
    box.className = 'dock-island';
    box.setAttribute('role', 'status');
    box.innerHTML = '<span class="di-i" aria-hidden="true"><svg viewBox="0 0 24 24"><path d="M18 8a6 6 0 1 0-12 0c0 7-3 9-3 9h18s-3-2-3-9"/><path d="M13.73 21a2 2 0 0 1-3.46 0"/></svg></span>'
      + '<span class="di-m"><b class="di-t"></b><span class="di-s"></span></span><a class="di-go">View</a>'
      + '<button type="button" class="di-x" aria-label="Dismiss">×</button>';
    dock.appendChild(box);
    function close() { clearTimeout(timer); dock.classList.remove('island-on'); }
    function arm() { clearTimeout(timer); timer = setTimeout(close, 3000); }
    box.querySelector('.di-x').addEventListener('click', close);
    box.querySelector('.di-go').addEventListener('click', close);
    box.addEventListener('mouseenter', function () { clearTimeout(timer); });
    box.addEventListener('mouseleave', arm);
    window.mdmIsland = function (a) {
      box.querySelector('.di-t').textContent = a.headline || 'New alert';
      box.querySelector('.di-s').textContent = a.sentence || '';
      box.querySelector('.di-go').href = a.href || '/alerts';
      dock.classList.add('island-on');
      arm();
      return true;
    };
  })();

  // ?test-alert on any page plays a sample critical alert in this browser only: the
  // dock's alert count turns critical and the island opens. Nothing is written or
  // sent, and the next /dock/status poll (≤30 s) puts the real count back.
  (function () {
    if (!/[?&]test-alert(=|&|$)/.test(location.search) || !window.mdmIsland) return;
    setTimeout(function () {
      var c = document.querySelector('.dock a.nav-link[data-cmdk-label="Alerts"] .dock-count');
      if (c) { c.hidden = false; c.textContent = (parseInt(c.textContent, 10) || 0) + 1; c.classList.add('crit'); }
      window.mdmIsland({ headline: 'Test alert · Taco Loco is offline', sentence: 'A sample critical alert, shown only in this browser. Nothing was sent.', href: '/alerts' });
    }, 900);
  })();
  

  // ── Tiered confirmation ──────────────────────────────────────────────────
  // One dialog for every guarded action, so confirmations look and behave the
  // same everywhere. Tiers: 'normal' (low stakes), 'danger' (red confirm),
  // 'type' (must type a token to enable confirm — for destructive, irreversible
  // actions). Returns a Promise<boolean>. Reuses the global modal a11y (Tab trap
  // + dialog semantics); focus lands on Cancel (never the confirm) by default.
  (function () {
    var modal = document.getElementById('mdm-confirm');
    if (!modal) { window.mdmConfirm = function () { return Promise.resolve(window.confirm('Are you sure?')); }; }
    if (!modal) return;
    var titleEl = document.getElementById('mdm-confirm-title');
    var bodyEl = document.getElementById('mdm-confirm-body');
    var scopeEl = document.getElementById('mdm-confirm-scope');
    var typeWrap = document.getElementById('mdm-confirm-type');
    var typeLabel = document.getElementById('mdm-confirm-type-label');
    var typeInput = document.getElementById('mdm-confirm-type-input');
    var okBtn = document.getElementById('mdm-confirm-ok');
    var cancels = modal.querySelectorAll('[data-confirm-cancel]');
    var active = null;

    function close(result) {
      if (!active) return;
      var resolve = active; active = null;
      modal.classList.remove('modal-open');
      okBtn.onclick = null;
      typeInput.oninput = null;
      document.removeEventListener('keydown', onKey, true);
      modal.removeEventListener('mousedown', onBackdrop);
      resolve(result);
    }
    function onKey(e) { if (e.key === 'Escape') { e.preventDefault(); close(false); } }
    function onBackdrop(e) { if (e.target === modal) close(false); }

    window.mdmConfirm = function (opts) {
      opts = opts || {};
      if (active) close(false);
      return new Promise(function (resolve) {
        active = resolve;
        titleEl.textContent = opts.title || 'Confirm';
        bodyEl.textContent = opts.body || '';
        bodyEl.hidden = !opts.body;
        if (opts.scope) { scopeEl.textContent = opts.scope; scopeEl.hidden = false; } else { scopeEl.hidden = true; }
        var danger = opts.tier === 'danger' || opts.tier === 'type';
        okBtn.className = danger ? 'btn-danger' : 'btn-primary';
        okBtn.textContent = opts.confirmLabel || 'Confirm';
        var token = opts.tier === 'type' ? String(opts.typeToken == null ? '' : opts.typeToken) : '';
        if (token) {
          typeWrap.hidden = false;
          typeLabel.textContent = opts.typeLabel || ('Type ' + token + ' to confirm');
          typeInput.value = '';
          okBtn.disabled = true;
          typeInput.oninput = function () { okBtn.disabled = typeInput.value.trim() !== token; };
        } else {
          typeWrap.hidden = true;
          okBtn.disabled = false;
        }
        okBtn.onclick = function () { if (!okBtn.disabled) close(true); };
        cancels.forEach(function (c) { c.onclick = function () { close(false); }; });
        document.addEventListener('keydown', onKey, true);
        modal.addEventListener('mousedown', onBackdrop);
        modal.classList.add('modal-open');
        // Nested rAF so this runs *after* the global modal helper's own
        // focus-first-focusable pass; land focus on the field (type tier) or
        // Cancel — never the confirm button.
        requestAnimationFrame(function () {
          requestAnimationFrame(function () {
            (token ? typeInput : modal.querySelector('.mdm-confirm-actions [data-confirm-cancel]')).focus();
          });
        });
      });
    };

    // Drop-in for `onsubmit="return confirm(...)"`:
    // `onsubmit="return mdmConfirmSubmit(event, {title, body, tier, ...})"`.
    window.mdmConfirmSubmit = function (event, opts) {
      event.preventDefault();
      var form = event.currentTarget || event.target;
      window.mdmConfirm(opts).then(function (ok) { if (ok) form.submit(); });
      return false;
    };

    // Build + submit a POST form for a JS-triggered guarded action.
    window.mdmPostForm = function (action, fields) {
      var f = document.createElement('form');
      f.method = 'POST'; f.action = action;
      Object.keys(fields || {}).forEach(function (k) {
        var i = document.createElement('input');
        i.type = 'hidden'; i.name = k; i.value = fields[k];
        f.appendChild(i);
      });
      document.body.appendChild(f); f.submit();
    };
  })();
  
