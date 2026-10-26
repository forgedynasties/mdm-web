// Loaded by templates/layout.html.
  (function () {
    if ('serviceWorker' in navigator) {
      window.addEventListener('load', function () { navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch(function () {}); });
    }
    var standalone = window.matchMedia('(display-mode: standalone)').matches || window.navigator.standalone === true;
    if (standalone) document.documentElement.classList.add('is-pwa');
    // Install banner (Chrome/Edge/Android): show once, dismissible for 14 days.
    var deferred = null;
    function dismissed() { try { return Date.now() - (+localStorage.getItem('mdm-install-dismissed') || 0) < 14 * 864e5; } catch (e) { return false; } }
    window.addEventListener('beforeinstallprompt', function (e) {
      if (standalone || dismissed() || window.innerWidth > 900) return;
      e.preventDefault(); deferred = e;
      var b = document.createElement('div'); b.className = 'pwa-nudge'; b.setAttribute('role', 'dialog');
      b.innerHTML = '<img src="/static/icons/mdm-192.png" alt=""><div class="t"><b>Install AIO MDM</b><span>Add it to your home screen for a full-screen app.</span></div><button type="button" class="ok">Install</button><button type="button" class="x" aria-label="Not now">×</button>';
      document.body.appendChild(b);
      b.querySelector('.ok').addEventListener('click', function () { if (deferred) { deferred.prompt(); deferred = null; } b.remove(); });
      b.querySelector('.x').addEventListener('click', function () { try { localStorage.setItem('mdm-install-dismissed', String(Date.now())); } catch (e) {} b.remove(); });
    });
  })();
  

  (function () {
    var STEPS = [
      { sel: '.appbar-brand', title: 'Welcome to AIO MDM', body: 'This is your fleet command center. Take a 60-second tour of where things live — or skip and explore on your own.', pos: 'bottom' },
      { sel: '.dock .nav-link[href="/"]', title: 'Overview', body: 'Your fleet at a glance: live counts, what needs attention right now, rollouts in flight, and every restaurant worst first. Each number opens its list.', pos: 'top' },
      { sel: '.dock .nav-link[href="/devices"]', title: 'Fleet', body: 'Every device, grouped by restaurant, group, product or release. Filter, select many, and open a device for its vitals, apps and remote tools.', pos: 'top' },
      { sel: '.fleet-tab[href="/enrollment"]', title: 'Enroll', body: 'New devices land in the onboarding inbox until they have a site. Enrollment profiles are scannable QR codes that say what a batch of outsourced devices is and where it goes.', pos: 'bottom', optional: true },
      { sel: '.dock .nav-link[href="/restaurants"]', title: 'Venues', body: 'Every restaurant and the devices in it. A device at a venue runs its role\'s app and stays on its prod build by itself.', pos: 'top', optional: true },
      { sel: '.dock .nav-link[href="/lineup"]', title: 'Lineup', body: 'Each kind of device: the models that are it, its firmware, and the MDM clients with their devices and releases.', pos: 'top', optional: true },
      { sel: '.dock .nav-link[href="/updates"]', title: 'Updates', body: 'Releases and the OTA rollouts carrying them, with per-device progress.', pos: 'top', optional: true },
      { sel: '.dock-search', title: 'Search anything', body: 'Jump to a device by serial, a restaurant, or any page. Press ⌘K / Ctrl+K from anywhere.', pos: 'top', optional: true },
      { sel: '#alerts-indicator', title: 'Live alerts', body: 'Open alerts update live. Click for the newest ones without leaving the page.', pos: 'bottom' },
      { sel: '#acct-wrap', title: 'You are set', body: 'Theme, settings, users and this tour live under your account. Install MDM on your phone from the browser menu for a full-screen app.', pos: 'bottom' }
    ];
    var i = 0, els = {}, active = false, mobile = function () { return window.innerWidth < 720; };
    function post(url, body) { return fetch(url, { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) }).catch(function () {}); }
    function steps() { return STEPS.filter(function (s) { return !s.optional || document.querySelector(s.sel); }); }
    function build() {
      var ov = document.createElement('div'); ov.className = 'tour-ov'; ov.innerHTML = '<div class="tour-spot"></div><div class="tour-card" role="dialog" aria-live="polite"><div class="tour-k"></div><div class="tour-t"></div><div class="tour-b"></div><div class="tour-f"><button type="button" class="tour-skip">Skip tour</button><span class="tour-dots"></span><span class="tour-nav"><button type="button" class="tour-back">Back</button><button type="button" class="tour-next">Next</button></span></div></div>';
      document.body.appendChild(ov);
      els = { ov: ov, spot: ov.querySelector('.tour-spot'), card: ov.querySelector('.tour-card'), k: ov.querySelector('.tour-k'), t: ov.querySelector('.tour-t'), b: ov.querySelector('.tour-b'), dots: ov.querySelector('.tour-dots'), back: ov.querySelector('.tour-back'), next: ov.querySelector('.tour-next'), skip: ov.querySelector('.tour-skip') };
      els.next.addEventListener('click', function () { go(i + 1); });
      els.back.addEventListener('click', function () { go(i - 1); });
      els.skip.addEventListener('click', function () { end('skipped'); });
      ov.addEventListener('click', function (e) { if (e.target === ov) {} });
      document.addEventListener('keydown', onKey);
      window.addEventListener('resize', place);
      window.addEventListener('scroll', place, true);
    }
    function onKey(e) { if (!active) return; if (e.key === 'Escape') end('skipped'); else if (e.key === 'ArrowRight' || e.key === 'Enter') go(i + 1); else if (e.key === 'ArrowLeft') go(i - 1); }
    function place() {
      if (!active) return;
      var s = steps()[i], t = document.querySelector(s.sel);
      if (!t) { go(i + 1); return; }
      var r = t.getBoundingClientRect(), pad = 8;
      els.spot.style.cssText = 'top:' + (r.top - pad) + 'px;left:' + (r.left - pad) + 'px;width:' + (r.width + pad * 2) + 'px;height:' + (r.height + pad * 2) + 'px';
      var c = els.card, cw = Math.min(360, window.innerWidth - 24), ch = c.offsetHeight;
      c.style.width = cw + 'px';
      if (mobile()) { c.style.cssText += ';left:12px;right:12px;width:auto;bottom:calc(12px + env(safe-area-inset-bottom));top:auto'; c.classList.add('sheet'); return; }
      c.classList.remove('sheet');
      var top = s.pos === 'top' ? r.top - ch - 14 : r.bottom + 14;
      if (top < 12) top = r.bottom + 14;
      if (top + ch > window.innerHeight - 12) top = Math.max(12, r.top - ch - 14);
      var left = Math.min(Math.max(12, r.left + r.width / 2 - cw / 2), window.innerWidth - cw - 12);
      c.style.top = top + 'px'; c.style.left = left + 'px'; c.style.bottom = 'auto';
      c.classList.toggle('above', s.pos === 'top');
      var ax = r.left + r.width / 2 - left; c.style.setProperty('--ax', Math.max(18, Math.min(cw - 18, ax)) + 'px');
    }
    function go(n) {
      var list = steps();
      if (n >= list.length) { end('completed'); return; }
      if (n < 0) n = 0;
      i = n;
      var s = list[i], t = document.querySelector(s.sel);
      if (t && t.scrollIntoView) t.scrollIntoView({ block: 'nearest', inline: 'nearest' });
      els.k.textContent = 'Step ' + (i + 1) + ' of ' + list.length;
      els.t.textContent = s.title; els.b.textContent = s.body;
      els.back.disabled = i === 0;
      els.next.textContent = i === list.length - 1 ? 'Done' : 'Next';
      els.dots.innerHTML = list.map(function (_, k) { return '<i class="' + (k === i ? 'on' : k < i ? 'done' : '') + '"></i>'; }).join('');
      requestAnimationFrame(place);
    }
    function end(status) {
      if (!active) return;
      active = false;
      els.ov.classList.add('out');
      setTimeout(function () { els.ov.remove(); }, 200);
      document.removeEventListener('keydown', onKey);
      window.removeEventListener('resize', place); window.removeEventListener('scroll', place, true);
      post('/tour/done', { status: status, step: i });
    }
    window.mdmStartTour = function (replay) {
      if (active) return;
      var pop = document.getElementById('acct-pop'); if (pop) pop.hidden = true;
      if (replay) post('/tour/reset');
      active = true; build(); go(0);
      requestAnimationFrame(function () { els.ov.classList.add('in'); });
    };
    // First run: start after the page settles so targets are laid out.
    if (window.MDM_SHOW_TOUR) window.addEventListener('load', function () { setTimeout(function () { if (!active && !document.hidden) window.mdmStartTour(false); }, 700); });
  })();
  
