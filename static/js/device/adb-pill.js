// Loaded by templates/device.html (admins). Wireless adb as a live pill in the page header
// (D2, picked 17 Oct): "adb: off", the step it is on with a spinner, or green
// "adb ready · copy". It opens #adbp-pop — the same stepper as /adb, the adb connect lines,
// and Turn on / Turn off. Polls /adb/progress only while something is in flight or the
// popover is open. The pill lives in the header island (a copy of the page's template),
// so it is found by class each time, never held.
(function () {
  var pop = document.getElementById('adbp-pop');
  if (!pop || !window.AdbSteps) return;
  var S = window.AdbSteps, serial = pop.dataset.serial, cur = null, timer = null, hours = '24', busy = false;

  function pill() { return document.querySelector('#appbar-island .adbp') || document.querySelector('.adbp'); }

  function paintPill() {
    var p = pill();
    if (!p || !cur) return;
    var t = p.querySelector('.adbp-t');
    p.classList.remove('on', 'busy', 'ready', 'bad');
    switch (cur.state) {
      case 'ready': p.classList.add('ready'); t.textContent = 'adb ready · copy'; break;
      case 'working': p.classList.add('busy'); t.textContent = 'adb: ' + cur.word + '…'; break;
      case 'failed': p.classList.add('bad'); t.textContent = 'adb: failed'; break;
      default: t.textContent = 'adb: off';
    }
  }

  function paintPop() {
    if (!cur) { pop.innerHTML = '<div class="cc-mh">Wireless adb</div><div class="adbp-b muted small">Checking…</div>'; return; }
    var h = '<div class="cc-mh">Wireless adb ' + S.tag(cur) + '<span class="sub">' + S.esc(cur.serial) + '</span></div><div class="adbp-b">';
    if (cur.state !== 'off' && cur.state !== 'cant') h += S.steps(cur);
    // Where the device is on its own network, whether or not adb is open.
    if (cur.ip) h += '<p class="small muted adbp-ip">On its network at <span class="cc-mono">' + S.esc(cur.ip) + '</span></p>';
    if (cur.anywhere || cur.lan) {
      h += '<div class="adbp-cmds">';
      if (cur.anywhere) h += S.cmd(cur.anywhere, 'from anywhere');
      if (cur.lan) h += S.cmd(cur.lan, 'same network');
      h += '</div>';
    }
    if (cur.state === 'cant') {
      h += '<p class="small muted">' + S.esc(cur.why || 'This device can\'t take wireless adb.') + '</p>';
    } else if (cur.state === 'off' || cur.state === 'failed') {
      h += '<p class="small muted adbp-p">Opens adb so a machine whose key this device trusts can connect — through a tunnel from anywhere, or on the same network.</p>' +
        '<div class="adbp-ctl"><span class="adbp-seg" role="radiogroup" aria-label="Stays open for">' +
        [['2', '2 h'], ['24', '24 h'], ['168', '7 d'], ['0', 'Until turned off']].map(function (o) {
          return '<label><input type="radio" name="adbp-h" value="' + o[0] + '"' + (o[0] === hours ? ' checked' : '') + '><span>' + o[1] + '</span></label>';
        }).join('') + '</span><button type="button" class="cc-btn sm primary" data-adbp="on"' + (busy ? ' disabled' : '') + '>' + (cur.state === 'failed' ? 'Try again' : 'Turn on adb') + '</button></div>';
    } else {
      h += '<div class="adbp-ctl"><span class="small muted">' + (cur.state === 'ready' ? 'Closes on its own at the time shown.' : 'Keeps going by itself — you can close this.') + '</span>' +
        (cur.tunnel_id ? '<button type="button" class="cc-btn sm" data-adbp="end">End tunnel</button>' : '') +
        '<button type="button" class="cc-btn sm" data-adbp="off"' + (busy ? ' disabled' : '') + '>Turn off</button></div>';
    }
    pop.innerHTML = h + '</div>';
  }

  function schedule() {
    clearTimeout(timer);
    if (!document.body.contains(pop)) return; // the page was swapped away
    var live = cur && cur.state === 'working';
    if (live || !pop.hidden) timer = setTimeout(poll, live ? 2000 : 5000);
  }
  function poll() {
    S.fetch([serial]).then(function (r) {
      cur = (r.devices || [])[0] || null;
      paintPill(); if (!pop.hidden) paintPop();
      schedule();
    }).catch(function () { timer = setTimeout(poll, 5000); });
  }

  function place() {
    var p = pill();
    if (!p) return;
    var r = p.getBoundingClientRect();
    pop.style.top = (r.bottom + 8) + 'px';
    pop.style.left = Math.max(12, Math.min(r.right - pop.offsetWidth, window.innerWidth - pop.offsetWidth - 12)) + 'px';
  }
  window.adbPillOpen = function () {
    if (!pop.hidden) { close(); return; }
    pop.hidden = false; paintPop(); place();
    var p = pill(); if (p) p.setAttribute('aria-expanded', 'true');
    poll();
  };
  function close() {
    pop.hidden = true;
    var p = pill(); if (p) p.setAttribute('aria-expanded', 'false');
    schedule();
  }
  document.addEventListener('click', function (e) {
    if (pop.hidden) return;
    if (e.target.closest('#adbp-pop') || e.target.closest('.adbp')) return;
    if (e.target.closest('.cc-mi') && e.target.closest('.isl-menu')) return; // the ⋯ entry that opens it
    close();
  });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && !pop.hidden) close(); });
  window.addEventListener('resize', function () { if (!pop.hidden) place(); });

  function post(url, body) {
    return fetch(url, { method: 'POST', credentials: 'same-origin', headers: { Accept: 'application/json', 'Content-Type': 'application/x-www-form-urlencoded' }, body: body });
  }
  pop.addEventListener('change', function (e) { if (e.target.name === 'adbp-h') hours = e.target.value; });
  pop.addEventListener('click', function (e) {
    var b = e.target.closest('[data-adbp]');
    if (!b || busy) return;
    var what = b.getAttribute('data-adbp'), req;
    if (what === 'on') req = post('/adb/connect', 'serial=' + encodeURIComponent(serial) + '&hours=' + encodeURIComponent(hours));
    else if (what === 'off') req = post('/adb/commands', 'action=off&serial=' + encodeURIComponent(serial));
    else if (what === 'end' && cur && cur.tunnel_id) req = post('/adb/tunnel/' + encodeURIComponent(cur.tunnel_id) + '/end', '');
    if (!req) return;
    busy = true; b.disabled = true; b.textContent = 'Sending…';
    req.finally(function () { busy = false; poll(); });
  });

  // One look on load, so the pill tells the truth; after that only while in flight.
  poll();
})();
