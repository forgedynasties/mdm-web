const { invoke } = window.__TAURI__.core;
const { listen } = window.__TAURI__.event;
const $ = (id) => document.getElementById(id);

const CLASSES = ['dongle', 'pos', 'kds', 'kiosk'];
const STEPS = ['Checking the device', 'Getting a token', 'Installing the agent', 'Setting Device Owner',
  'Granting permissions', 'Starting the agent', 'Waiting for the server'];
const svg = (inner) => `<svg class="ic" viewBox="0 0 24 24">${inner}</svg>`;
const ICON = {
  phone: svg('<rect x="7" y="2.5" width="10" height="19" rx="2.2"/><path d="M11 18.5h2"/>'),
  dongle: svg('<rect x="3" y="5" width="18" height="12" rx="2"/><path d="M8 21h8M12 17v4"/>'),
  pos: svg('<path d="M6 3h12v18l-2-1.5L14 21l-2-1.5L10 21l-2-1.5L6 21z"/><path d="M9 8h6M9 12h6"/>'),
  kds: svg('<rect x="3" y="4" width="18" height="13" rx="2"/><path d="M7 9h5M7 12.5h8M9 21h6"/>'),
  kiosk: svg('<rect x="6" y="2.5" width="12" height="16" rx="2"/><path d="M9 21.5h6M12 18.5v3"/>'),
  help: svg('<circle cx="12" cy="12" r="9"/><path d="M9.5 9.5a2.6 2.6 0 1 1 3.6 2.4c-.7.4-1.1.9-1.1 1.8M12 17h.01"/>'),
};
const PHONE = ICON.phone;
const CHECK = '<svg class="check" viewBox="0 0 24 24"><path d="M5 12.5l4.5 4.5L19 7.5"/></svg>';

const store = {
  get: (k, d) => { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
  set: (k, v) => { try { localStorage.setItem(k, v); } catch {} },
};
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// Follow the OS light/dark setting (style.css themes via data-theme).
const mq = window.matchMedia('(prefers-color-scheme: dark)');
const applyTheme = () => document.documentElement.setAttribute('data-theme', mq.matches ? 'dark' : 'light');
applyTheme(); mq.addEventListener('change', applyTheme);

// Scale the whole UI with the window (the dashboard CSS is in fixed px), and let people
// nudge it with Ctrl/Cmd + / - / 0. The nudge is remembered.
let userZoom = parseFloat(store.get('zoom', '1')) || 1;
function applyZoom() {
  const auto = Math.min(1.6, Math.max(1, Math.min(window.innerWidth / 820, window.innerHeight / 620)));
  document.documentElement.style.zoom = String(+(auto * userZoom).toFixed(3));
}
window.addEventListener('resize', applyZoom);
document.addEventListener('keydown', (e) => {
  if (!(e.ctrlKey || e.metaKey)) return;
  const k = e.key;
  if (k === '+' || k === '=') userZoom = Math.min(1.6, userZoom + 0.1);
  else if (k === '-') userZoom = Math.max(0.7, userZoom - 0.1);
  else if (k === '0') userZoom = 1;
  else return;
  e.preventDefault();
  store.set('zoom', String(+userZoom.toFixed(2)));
  applyZoom();
});
applyZoom();

let devices = [];
let selected = null;           // handle
let timer = null, polling = false;
const picked = {};             // handle -> class chosen
const run = {};                // handle -> { step, error, done }  (this session's enroll attempts)
let todayCount = 0;
let heroKey = '';

const classOf = (h) => picked[h] || store.get('class', 'dongle');
const iconOf = (d) => (d.status === 'unauthorized' ? ICON.help : ICON[d.class] || ICON.phone);
const dotOf = (d) => ({ ready: '', enrolled: '', enrolling: 'wait', blocked: 'bad', unauthorized: 'wait', offline: 'none' }[d.status] ?? 'none');

function renderRail() {
  const rail = $('rail');
  const have = new Map([...rail.children].map((el) => [el.dataset.h, el]));
  devices.forEach((d, i) => {
    let el = have.get(d.handle);
    if (!el) {
      el = document.createElement('button');
      el.className = 'ri in';
      el.dataset.h = d.handle;
      el.addEventListener('click', () => { selected = d.handle; refresh(); });
    }
    have.delete(d.handle);
    el.classList.toggle('on', d.handle === selected);
    el.innerHTML = `<div class="ph">${iconOf(d)}</div><span class="nm">${esc(d.name || 'Unknown device')}</span><i class="${dotOf(d)}"></i>`;
    if (rail.children[i] !== el) rail.insertBefore(el, rail.children[i] || null);
  });
  have.forEach((el) => el.remove());
}

function timeline(step) {
  return `<ul class="tl">${STEPS.map((s, i) => `<li class="${i < step ? 'ok' : i === step ? 'on' : ''}">${s}</li>`).join('')}</ul>`;
}

function setStep(handle, step) {
  const ul = document.querySelector(`#hero [data-tl="${CSS.escape(handle)}"] .tl`) || document.querySelector('#hero .tl');
  if (!ul) return;
  [...ul.children].forEach((li, i) => { li.className = i < step ? 'ok' : i === step ? 'on' : ''; });
}

function renderHero() {
  const hero = $('hero');
  const d = devices.find((x) => x.handle === selected);
  const r = d && run[d.handle];
  const key = d ? [d.handle, d.status, classOf(d.handle), r?.error || '', r?.done ? 'd' : ''].join('|') : 'none';
  if (key === heroKey) return;          // nothing visible changed: don't restart animations
  heroKey = key;

  if (!d) {
    hero.innerHTML = `<div class="radar"><b></b><b></b><b></b>${PHONE}</div><h2>Plug in a device</h2><p class="msg">Connect an Android device with a USB cable and tap “Allow USB debugging”. It appears here by itself.</p>`;
    return;
  }
  const head = `<div class="bigph">${iconOf(d)}</div><h2>${esc(d.name || 'Unknown device')}</h2>
    <div class="mono" style="color:var(--muted)">${esc(d.serial || d.handle)}${d.android ? ' · Android ' + esc(d.android) : ''}</div>`;

  if (d.status === 'enrolling') {
    hero.innerHTML = `<div class="bigph">${iconOf(d)}</div><h2>Enrolling ${esc(d.name)}…</h2><div data-tl="${esc(d.handle)}">${timeline(r?.step ?? 0)}</div>`;
  } else if (d.status === 'enrolled') {
    const burst = r?.done ? '<div class="ring"></div>' : '';
    hero.innerHTML = `<div class="okbig">${CHECK}</div><h2 style="margin-top:8px">Enrolled</h2>
      <div class="mono" style="color:var(--muted)">${esc(d.name)} · ${esc(d.class || 'device')}${d.agent_version ? ' · agent ' + esc(d.agent_version) : ''}</div>
      <button class="cc-btn primary" id="next" style="margin-top:12px">Next device</button>${burst}`;
    if (r) r.done = false;
    heroKey = [d.handle, d.status, classOf(d.handle), '', ''].join('|');
  } else if (d.status === 'blocked') {
    hero.innerHTML = `${head}<div class="fix">${esc(d.note)}<br>Then plug it in again.</div>`;
  } else if (d.status === 'unauthorized') {
    hero.innerHTML = `${head}<p class="msg" style="margin-top:10px">Look at the device screen and tap <b>Allow</b> on the USB debugging prompt.</p>`;
  } else if (d.status === 'ready') {
    const cls = classOf(d.handle);
    const err = r?.error ? `<div class="fix shake">${esc(r.error)}</div>` : '';
    hero.innerHTML = `${head}<div class="q">What is this device used for?</div>
      <div class="chips">${CLASSES.map((c) => `<button data-c="${c}" class="${c === cls ? 'on' : ''}">${c}</button>`).join('')}</div>
      <button class="cc-btn primary bigbtn" id="go">${r?.error ? 'Try again' : 'Enroll this device'}</button>${err}`;
  } else {
    hero.innerHTML = `${head}<p class="msg" style="margin-top:10px">${esc(d.note || d.status)}</p>`;
  }
}

function refresh() { renderRail(); renderHero(); }

$('hero').addEventListener('click', async (e) => {
  const d = devices.find((x) => x.handle === selected);
  if (!d) return;
  const chip = e.target.closest('[data-c]');
  if (chip) {
    picked[d.handle] = chip.dataset.c;
    store.set('class', chip.dataset.c);
    return refresh();
  }
  if (e.target.closest('#next')) {
    const nxt = devices.find((x) => x.status === 'ready' && x.handle !== d.handle);
    if (nxt) selected = nxt.handle;
    return refresh();
  }
  const go = e.target.closest('#go');
  if (!go) return;
  go.classList.add('press');
  run[d.handle] = { step: 0 };
  d.status = 'enrolling';
  refresh();
  try {
    await invoke('enroll', { handle: d.handle, class: classOf(d.handle) });
    run[d.handle] = { step: 7, done: true };
    todayCount++; store.set('today', JSON.stringify({ day: new Date().toDateString(), n: todayCount }));
    showToday();
  } catch (err) {
    run[d.handle] = { error: String(err) };
  }
  heroKey = '';
  tick();
});

listen('enroll-step', (e) => {
  const { handle, step } = e.payload;
  run[handle] = { ...(run[handle] || {}), step };
  setStep(handle, step);
});

function showToday() { $('today').textContent = todayCount ? `${todayCount} enrolled today` : ''; }
try {
  const t = JSON.parse(store.get('today', 'null'));
  if (t && t.day === new Date().toDateString()) todayCount = t.n;
} catch {}

async function tick() {
  if (polling) return;
  polling = true;
  try {
    const next = await invoke('list_devices');
    // A device the UI is mid-enroll on stays "enrolling" even if adb blips.
    next.forEach((d) => { if (run[d.handle] && run[d.handle].step !== undefined && !run[d.handle].done && !run[d.handle].error && d.status === 'ready') d.status = 'enrolling'; });
    devices = next;
    if (!devices.some((d) => d.handle === selected)) {
      selected = (devices.find((d) => d.status === 'ready') || devices[0])?.handle ?? null;
    }
    $('foot').textContent = 'Watching for devices';
    refresh();
  } catch (err) {
    if (String(err) === 'signed-out') return goSignIn('Session expired. Sign in again.');
    $('foot').textContent = String(err);
  } finally { polling = false; }
}

function showWho(name, avatar) {
  $('whoName').textContent = name;
  const el = $('pfp');
  if (avatar) { el.innerHTML = `<img alt="" src="${avatar}">`; return; }
  el.textContent = (name || '?').trim().split(/\s+/).map((w) => w[0]).slice(0, 2).join('').toUpperCase();
}

function goApp(me) {
  showWho(me.username, null);
  invoke('profile').then((p) => showWho(p.name || me.username, p.avatar)).catch(() => {});
  $('signin').hidden = true; $('app').hidden = false;
  showToday(); heroKey = ''; clearInterval(timer);
  tick(); timer = setInterval(tick, 2500);
}
async function goSignIn(msg = '') {
  clearInterval(timer);
  try {
    const l = await invoke('last_login');
    if (l) { if (!$('u').value) $('u').value = l.username || ''; if (l.server && l.server !== 'https://mdm.dev.aioapp.com' && !$('srv').value) $('srv').value = l.server; }
  } catch {}
  $('signinErr').textContent = msg; $('p').value = '';
  $('app').hidden = true; $('signin').hidden = false;
  ($('u').value ? $('p') : $('u')).focus();
}

$('signinForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  $('signinBtn').disabled = true; $('signinErr').textContent = '';
  try { goApp(await invoke('sign_in', { server: $('srv').value, username: $('u').value, password: $('p').value })); }
  catch (err) { $('signinErr').textContent = String(err); }
  $('signinBtn').disabled = false;
});
$('signOut').addEventListener('click', async () => { await invoke('sign_out'); goSignIn(); });
document.addEventListener('keydown', (e) => { if (e.key === 'Enter' && document.activeElement === document.body) $('go')?.click(); });

(async () => { const me = await invoke('me'); me ? goApp(me) : goSignIn(); })();
