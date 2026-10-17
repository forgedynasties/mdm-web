// Loaded by templates/clients.html.
// One release menu open at a time, closed by Escape or a click anywhere else — the rows
// sit inside an htmx-swapped fragment, so this is delegated rather than bound per row.
function clMenu(e, btn) {
  e.stopPropagation();
  var pop = btn.nextElementSibling, open = !pop.hidden;
  document.querySelectorAll('.cl-pop').forEach(function (p) {
    p.hidden = true;
    var b = p.previousElementSibling;
    if (b) b.setAttribute('aria-expanded', 'false');
  });
  pop.hidden = open;
  btn.setAttribute('aria-expanded', open ? 'false' : 'true');
}
function clCopy(btn, sha) {
  var done = function () { btn.textContent = 'Copied'; setTimeout(function () { btn.textContent = 'Copy SHA-256'; }, 1200); };
  if (navigator.clipboard) navigator.clipboard.writeText(sha).then(done, function () {});
  else done();
}
if (!window.__clHooks) {
window.__clHooks = true;
document.addEventListener('click', function () {
  document.querySelectorAll('.cl-pop').forEach(function (p) { p.hidden = true; });
});
document.addEventListener('keydown', function (e) {
  if (e.key === 'Escape') document.querySelectorAll('.cl-pop').forEach(function (p) { p.hidden = true; });
});
// Dormant rows start folded; the header button reveals them — or a search that hits one.
document.addEventListener('lt:change', function (e) {
  if (!e.detail || !e.detail.active || e.target.id !== 'cl-devs') return;
  e.target.querySelectorAll('[data-fold]').forEach(function (h) {
    var key = h.getAttribute('data-fold'), btn = h.querySelector('.cl-fold');
    if (btn && btn.getAttribute('aria-expanded') !== 'true' && e.target.querySelector('.cl-tr[data-dormant="' + key + '"]:not(.lt-out)')) clFold(btn, key);
  });
});
}
function clFold(btn, key) {
  var open = btn.getAttribute('aria-expanded') === 'true';
  document.querySelectorAll('[data-dormant="' + key + '"]').forEach(function (row) {
    row.classList.toggle('hidden', open);
  });
  btn.setAttribute('aria-expanded', open ? 'false' : 'true');
  btn.textContent = open ? 'Show' : 'Hide';
}
// The rail lives inside the swapped fragment, so without this nothing moves until the
// response lands and a click reads as a hang. Move the selection and put a skeleton in
// the detail column straight away; the swap replaces both with the real thing.
// The command is queued in milliseconds but the install takes a minute or two, and
// the device says nothing until it starts. Say "Sent" on the button straight away and
// pull the page again shortly: by then the row carries the real state from the server.
// Pressing Update on a device that reports to another MDM. Nothing is queued: the
// device collects its client from that server, so a command from here would sit
// unclaimed until it expired. Say so, and offer the way there.
function clElsewhere(serial, peer, url) {
  var msg = serial + ' reports to ' + peer + '. Its client comes from that server — an update sent from here would never be collected.';
  if (window.showToast) showToast(msg, 'info');
  else alert(msg);
  if (url) {
    var go = document.getElementById('cl-go-peer');
    if (!go) {
      go = document.createElement('a');
      go.id = 'cl-go-peer';
      go.target = '_blank';
      go.rel = 'noopener';
      go.className = 'cc-btn sm';
      go.style.cssText = 'position:fixed;right:18px;bottom:76px;z-index:60';
      document.body.appendChild(go);
    }
    go.href = url + '/devices/' + encodeURIComponent(serial);
    go.textContent = 'Open ' + serial + ' on ' + peer + ' \u2197';
    clearTimeout(window.__clGoTimer);
    window.__clGoTimer = setTimeout(function () { go.remove(); }, 8000);
  }
}

function clSent(form) {
  var b = form.querySelector('button[type=submit]');
  // .cl-sent kills the pseudo-element spinner the global submit feedback adds
  // (.btn-submitting::after in style.css): this button draws its own, and the two
  // together put a spinner on each side of the word.
  if (b) { b.disabled = true; b.classList.add('cl-sent'); b.innerHTML = '<span class="cl-spin"></span>Sent'; }
  var d = document.getElementById('cl-detail');
  var slot = (new URLSearchParams(location.search)).get('client') || '';
  setTimeout(function () {
    if (window.htmx && document.getElementById('cl-detail')) htmx.ajax('GET', '/lineup?client=' + encodeURIComponent(slot) + '&partial=1',
      { target: '#cl-detail', swap: 'outerHTML' });
  }, 1500);
  void d;
}

// A variant switch keeps the heading and the switcher — only the build below them
// changes — so it marks itself active and skeletons the body, not the whole panel.
function clPick(a) {
  var rail = a && a.closest('.cl-side');
  if (rail) rail.querySelectorAll('.cl-si').forEach(function (el) { el.classList.toggle('on', el === a); });
  var d = document.getElementById('cl-detail');
  if (!d) return;
  d.innerHTML = '<div class="cl-hd"><div class="skel skel-line skel-w25" style="height:17px"></div>' +
    '<div class="skel skel-line skel-w60" style="height:11px"></div></div><div id="cl-body"></div>';
  clSkeleton(document.getElementById('cl-body'));
}

function clVar(a) {
  var seg = a.closest('.vseg');
  if (seg) seg.querySelectorAll('a').forEach(function (el) {
    var on = el === a;
    el.classList.toggle('on', on);
    el.setAttribute('aria-selected', on ? 'true' : 'false');
  });
  clSkeleton(document.getElementById('cl-body'));
}

function clSkeleton(d) {
  if (!d) return;
  var cards = '';
  for (var i = 0; i < 4; i++) cards += '<div class="cl-card"><div class="skel skel-line skel-w60" style="height:9px"></div><div class="skel skel-line skel-w40" style="height:20px"></div></div>';
  var rows = '';
  for (var j = 0; j < 4; j++) rows += '<div class="skel-row" style="padding:12px 15px"><div class="g"><div class="skel skel-line skel-w40"></div></div><div class="skel skel-pill"></div></div>';
  d.innerHTML = '<div class="cl-cards">' + cards + '</div>' +
    '<div class="cl-sec"><span>Devices</span></div><div class="cl-tbl">' + rows + '</div>';
}
