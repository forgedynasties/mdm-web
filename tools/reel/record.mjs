#!/usr/bin/env node
// Records the showcase reel scenes with Playwright against the test instance.
// One video per scene lands in out/scenes/NN-name.webm; assemble.sh stitches them.
//
//   node record.mjs              # all scenes
//   node record.mjs map device   # only these scenes
//
// Prereqs: node seed.mjs (demo fleet) and ./sim.sh start (devices online).
// Env: MDM_URL (default http://127.0.0.1:8082), MDM_USER/MDM_PASS (admin/admin),
//      REEL_THEME (dark|light, default dark), REEL_ZOOM (default 1.45),
//      REEL_SCENES_DIR (default out/scenes; e.g. out/scenes_light for a light take).
//
// Motion layer (all rendered in-page so the screencast captures it):
//   s.zoomIn(target, k)  camera push toward the element (body transform, eased)
//   s.zoomOut()          back to the wide shot
//   s.spot(target)       glowing outline pulse around the element
//   s.callout(text)      caption pill that floats in next to the cursor
//   s.confetti()         burst for the "it landed" beats
//   clicks draw a ripple ring at the cursor

import { chromium } from "playwright";
import { execFileSync } from "node:child_process";
import { mkdirSync, renameSync, readdirSync, rmSync, writeFileSync, readFileSync, existsSync } from "node:fs";

const BASE = process.env.MDM_URL || "http://127.0.0.1:8082";
const USER = process.env.MDM_USER || "admin";
const PASS = process.env.MDM_PASS || "admin";
const THEME = process.env.REEL_THEME || "dark";
const ZOOM = parseFloat(process.env.REEL_ZOOM || "1.45"); // the app is a ~1230px column; zoom fills a 1080p frame
const W = 1920, H = 1080;
const OUT = new URL("./" + (process.env.REEL_SCENES_DIR || "out/scenes") + "/", import.meta.url).pathname;
const COMPOSE_DIR = new URL("../../", import.meta.url).pathname;
mkdirSync(OUT, { recursive: true });

// ---------------------------------------------------------------- fleet facts
const sql = (q) => execFileSync("docker",
  ["compose", "exec", "-T", "postgres", "sh", "-c", 'psql -At -U "$POSTGRES_USER" -d "$POSTGRES_DB"'],
  { cwd: COMPOSE_DIR, input: q }).toString().trim();
const REEL = `restaurant_id IN (SELECT id FROM restaurants WHERE notes = 'reel-seed')`;
// a healthy, online tablet with a nice battery curve for the device scene
const HERO = sql(`SELECT d.serial_number FROM devices d WHERE ${REEL} AND product = 't7'
  AND last_seen_at > now() - interval '10 minutes' AND latest_battery_pct BETWEEN 30 AND 95
  AND NOT EXISTS (SELECT 1 FROM alerts a WHERE a.device_id = d.id AND a.status <> 'resolved')
  ORDER BY serial_number LIMIT 1`);
const RELEASE_ID = sql(`SELECT id FROM releases WHERE product = 't7' AND status = 'published'
  AND EXISTS (SELECT 1 FROM ota_packages o WHERE o.release_id = releases.id) ORDER BY created_at DESC LIMIT 1`);
const RESTAURANT = sql(`SELECT name FROM restaurants WHERE notes = 'reel-seed' ORDER BY name LIMIT 1`);
const ONLINE = sql(`SELECT count(*) FROM devices WHERE ${REEL} AND last_seen_at > now() - interval '10 minutes'`);
const TOTAL = sql(`SELECT count(*) FROM devices WHERE ${REEL}`);

// ---------------------------------------------------------------- helpers
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const ease = (t) => (t < 0.5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2);

// In-page overlay: cursor, click ripple, spotlight, callout, confetti, camera.
// Injected before any script on every navigation; lives on <html> (outside the
// zoomed/transformed <body>) so it stays 1:1 with the real mouse.
const OVERLAY_JS = `
(() => {
  const ACCENT = '#ff5a3c';
  const style = document.createElement('style');
  style.textContent = \`
    #__reel_cursor{position:fixed;z-index:2147483647;left:-100px;top:-100px;width:22px;height:30px;margin:-2px 0 0 -3px;pointer-events:none;transition:transform .08s;zoom:1}
    .__reel_ripple{position:fixed;z-index:2147483646;width:14px;height:14px;margin:-7px 0 0 -7px;border-radius:50%;border:3px solid \${ACCENT};pointer-events:none;animation:__reel_rip .55s ease-out forwards;zoom:1}
    @keyframes __reel_rip{from{transform:scale(1);opacity:.9}to{transform:scale(4.2);opacity:0}}
    .__reel_spot{position:fixed;z-index:2147483645;border-radius:14px;pointer-events:none;box-shadow:0 0 0 3px \${ACCENT},0 0 32px 6px rgba(255,90,60,.55);animation:__reel_spot 1.4s ease-in-out forwards;zoom:1}
    @keyframes __reel_spot{0%{opacity:0;transform:scale(1.08)}18%{opacity:1;transform:scale(1)}80%{opacity:1}100%{opacity:0}}
    .__reel_callout{position:fixed;z-index:2147483646;pointer-events:none;font:600 26px/1 Poppins,Inter,system-ui,sans-serif;color:#fff;background:linear-gradient(135deg,#ff5a3c,#ff8a3c);padding:16px 24px;border-radius:999px;box-shadow:0 12px 40px rgba(0,0,0,.45);white-space:nowrap;letter-spacing:.01em;animation:__reel_co .3s cubic-bezier(.2,1.4,.4,1) forwards;zoom:1}
    .__reel_callout.out{animation:__reel_co_out .3s ease-in forwards}
    @keyframes __reel_co{from{opacity:0;transform:translateY(14px) scale(.85)}to{opacity:1;transform:none}}
    @keyframes __reel_co_out{to{opacity:0;transform:translateY(-10px) scale(.95)}}
    .__reel_conf{position:fixed;z-index:2147483646;width:12px;height:18px;pointer-events:none;border-radius:3px;animation:__reel_fall var(--d) cubic-bezier(.2,.7,.5,1) forwards;zoom:1}
    @keyframes __reel_fall{0%{transform:translate(0,0) rotate(0);opacity:1}100%{transform:translate(var(--x),var(--y)) rotate(var(--r));opacity:0}}
  \`;
  const c = document.createElement('div');
  c.id = '__reel_cursor';
  c.innerHTML = '<svg viewBox="0 0 24 32" width="22" height="30"><path d="M3 2l17 13-7.5 1.2L17 27l-4 1.8-4.6-10.6L3 24z" fill="#111" stroke="#fff" stroke-width="1.6" stroke-linejoin="round"/></svg>';
  const root = () => document.documentElement;
  const add = () => { root().appendChild(style); root().appendChild(c); };
  document.readyState === 'loading' ? document.addEventListener('DOMContentLoaded', add) : add();
  let mx = 0, my = 0;
  window.addEventListener('mousemove', e => { mx = e.clientX; my = e.clientY; c.style.left = mx + 'px'; c.style.top = my + 'px'; }, true);
  window.addEventListener('mousedown', () => {
    c.style.transform = 'scale(.85)';
    const r = document.createElement('div'); r.className = '__reel_ripple'; r.style.left = mx + 'px'; r.style.top = my + 'px';
    root().appendChild(r); setTimeout(() => r.remove(), 600);
  }, true);
  window.addEventListener('mouseup', () => { c.style.transform = ''; }, true);

  window.__reel = {
    mouse: () => ({ x: mx, y: my }),
    spot(rect, ms) {
      const s = document.createElement('div'); s.className = '__reel_spot';
      s.style.left = (rect.x - 8) + 'px'; s.style.top = (rect.y - 8) + 'px'; s.style.width = (rect.width + 16) + 'px'; s.style.height = (rect.height + 16) + 'px';
      s.style.animationDuration = (ms || 1400) + 'ms';
      root().appendChild(s); setTimeout(() => s.remove(), (ms || 1400) + 50);
    },
    callout(text, ms) {
      const el = document.createElement('div'); el.className = '__reel_callout'; el.textContent = text;
      root().appendChild(el);
      const w = el.offsetWidth, h = el.offsetHeight;
      let x = mx + 34, y = my - h - 18;
      if (x + w > innerWidth - 24) x = mx - w - 24;
      if (y < 24) y = my + 34;
      el.style.left = x + 'px'; el.style.top = y + 'px';
      setTimeout(() => { el.classList.add('out'); setTimeout(() => el.remove(), 320); }, ms || 1600);
    },
    confetti(n) {
      const colors = ['#ff5a3c', '#ffb03c', '#3cc8ff', '#7cf29a', '#ffffff', '#c58cff'];
      for (let i = 0; i < (n || 90); i++) {
        const p = document.createElement('div'); p.className = '__reel_conf';
        const ang = (Math.random() * Math.PI) + Math.PI, sp = 380 + Math.random() * 620;
        p.style.left = (innerWidth / 2) + 'px'; p.style.top = (innerHeight * .78) + 'px';
        p.style.background = colors[i % colors.length];
        p.style.setProperty('--x', (Math.cos(ang) * sp * 1.4) + 'px');
        p.style.setProperty('--y', (Math.sin(ang) * sp + 700) + 'px');
        p.style.setProperty('--r', (Math.random() * 900 - 450) + 'deg');
        p.style.setProperty('--d', (1.4 + Math.random() * .9) + 's');
        root().appendChild(p); setTimeout(() => p.remove(), 2500);
      }
    },
  };
})();`;

class Scene {
  constructor(page, t0) { this.page = page; this.t0 = t0; this.pos = { x: W / 2, y: H / 2 }; this.cam = 1; this.camEvents = []; }
  now() { return Date.now() - this.t0; }
  // start the clip here (e.g. after a map has finished loading tiles)
  startHere(lead = 0) { this.trimMs = this.now() + lead; }
  locator(target) { return typeof target === "string" ? this.page.locator(target).first() : target; }
  async box(target) {
    const el = this.locator(target);
    await el.waitFor({ state: "visible", timeout: 15000 });
    await el.scrollIntoViewIfNeeded();
    let b = await el.boundingBox();
    if (!b) return null;
    // keep the target clear of the fixed top bar / bottom dock (they'd eat the click)
    // (at 1.45x the top bar is ~130px and the bottom dock ~150px)
    if (b.y < 160 || b.y + b.height > H - 170) {
      await el.evaluate((e) => e.scrollIntoView({ block: "center", behavior: "instant" }));
      await sleep(350);
      b = await el.boundingBox();
    }
    return b;
  }
  async glide(target, { dur = 700, offset = { x: 0, y: 0 } } = {}) {
    const b = await this.box(target);
    if (!b) return;
    await this.moveTo(b.x + b.width / 2 + offset.x, b.y + b.height / 2 + offset.y, dur);
  }
  async moveTo(x, y, dur = 700) {
    const steps = Math.max(12, Math.round(dur / 16));
    const from = { ...this.pos };
    for (let i = 1; i <= steps; i++) {
      const t = ease(i / steps);
      await this.page.mouse.move(from.x + (x - from.x) * t, from.y + (y - from.y) * t);
      await sleep(dur / steps);
    }
    this.pos = { x, y };
  }
  async click(target, opts = {}) {
    await this.glide(target, opts);
    await sleep(180);
    await this.page.mouse.down(); await sleep(90); await this.page.mouse.up();
  }
  async scroll(px, dur = 900) {
    const steps = Math.max(10, Math.round(dur / 40));
    for (let i = 0; i < steps; i++) { await this.page.mouse.wheel(0, px / steps); await sleep(dur / steps); }
  }
  // zoom the body, not <html>: the overlay hangs off <html> so it stays 1:1 with the mouse
  async zoom() { await this.page.evaluate((z) => { document.body.style.zoom = z; }, ZOOM).catch(() => {}); }
  async goto(path, settle = 2200) {
    await this.page.goto(BASE + path, { waitUntil: "load" });
    await this.zoom();
    if (this.onFirstPaint) { this.onFirstPaint(); this.onFirstPaint = null; }
    await sleep(settle);
    await this.page.keyboard.press("Escape"); // dismiss a first-run tour if it popped
  }
  async reloaded() { await this.page.waitForLoadState("load").catch(() => {}); await this.zoom(); }
  async hold(ms) { await sleep(ms); }

  // ---- motion layer
  async spot(target, ms = 1400) {
    const b = await this.box(target).catch(() => null); if (!b) return;
    await this.page.evaluate(([r, ms]) => window.__reel && window.__reel.spot(r, ms), [b, ms]).catch(() => {});
  }
  async callout(text, ms = 1700) {
    await this.page.evaluate(([t, ms]) => window.__reel && window.__reel.callout(t, ms), [text, ms]).catch(() => {});
  }
  async confetti() { await this.page.evaluate(() => window.__reel && window.__reel.confetti()).catch(() => {}); }
  // Camera moves are NOT applied in-page (a body transform drags the fixed header
  // and dock along). They are logged as keyframes and rendered by assemble.mjs as a
  // real crop/scale push-in, so the whole frame zooms like a camera.
  async zoomIn(target, k = 1.6, settle = 800, dur = 750) {
    let x = this.pos.x, y = this.pos.y;
    if (target) { const b = await this.box(target).catch(() => null); if (b) { x = b.x + b.width / 2; y = b.y + b.height / 2; } }
    this.cam = k;
    this.camEvents.push({ t: this.now(), k, x, y, dur });
    await sleep(settle);
  }
  async zoomOut(settle = 800, dur = 750) {
    if (this.cam === 1) return;
    this.cam = 1;
    this.camEvents.push({ t: this.now(), k: 1, dur });
    await sleep(settle);
  }
}

// ---------------------------------------------------------------- scenes
// Script: in-house → stack → why not buy → geolocation → reach anywhere (remote
// control) → bulk actions → OTA → convenience. Cards carry the narration
// (cards.mjs); scenes show the product. No highlight boxes, callouts only.
const SCENES = {
  // the command center (after the "stack" card)
  async overview(s) {
    await s.goto("/", 2600);
    await s.glide(".ov3-fact:has-text('online')", { dur: 900 });
    await s.callout(`${ONLINE} of ${TOTAL} devices online, live over WebSocket`, 2000);
    await s.hold(1800);
    await s.zoomIn(".ov3-st:has-text('Fleet health')", 1.6);
    await s.glide(".ov3-st:has-text('Fleet health')", { dur: 600 });
    await s.callout("One health score, computed from our own telemetry", 2000);
    await s.hold(2000);
    await s.zoomOut();
    await s.scroll(420, 1200);
    await s.callout("Daily briefing, written for the ops team", 1800);
    await s.hold(1800);
  },
  // venue-aware health (after the comparison card)
  async health(s) {
    await s.goto("/fleet-health", 2800);
    await s.glide("text=Needs action >> nth=0", { dur: 800 }).catch(() => {});
    await s.callout("It knows what a restaurant is", 1800);
    await s.hold(800);
    await s.zoomIn("text=Needs action >> nth=0", 1.45, 800);
    await s.hold(1800);
    await s.zoomOut(700);
    await s.scroll(500, 1200);
    await s.callout("…and what a charging pad is", 1600);
    await s.hold(1800);
  },
  // geolocation
  async map(s) {
    await s.goto("/map", 5000); // let tiles + dark style settle (not recorded)
    await s.click("#fm-fit");
    await s.hold(3000);          // fit animation + tile fetch, also not recorded
    s.startHere();
    await s.hold(500);
    await s.callout("Position from Wi-Fi scans, no GPS", 2000);
    await s.hold(2200);
    await s.click(".fm-list button, .fm-dev, [class*=fm-] a[href^='/devices/'] >> nth=0").catch(() => {});
    await s.callout("Click a device to fly to it", 1600);
    await s.hold(3200);
  },
  // reach any device: device page → live remote control, taps from the browser
  async remote(s) {
    await s.goto(`/devices/${HERO}`, 2600);
    await s.glide("a:has-text('Remote Control')", { dur: 900 });
    await s.callout("From the browser, wherever the device is", 1800);
    await s.hold(1200);
    await s.page.mouse.down(); await sleep(90); await s.page.mouse.up();
    // one navigation only: a second connect evicts the first session and races its
    // teardown. The page paints JPEG frames from the simulator whatever codec it asked for.
    await s.page.waitForURL(/\/remote/, { timeout: 15000 }).catch(() => {});
    await s.reloaded();
    await s.page.locator("#rc-canvas").waitFor({ state: "visible", timeout: 20000 }).catch(() => {});
    await s.hold(1500);
    await s.callout("Live screen", 1400);
    await s.zoomIn("#rc-canvas", 1.3, 900);
    // taps: each one advances the kiosk UI in the simulator
    await s.click("#rc-canvas", { offset: { x: -160, y: -40 } });
    await s.hold(1400);
    await s.click("#rc-canvas", { offset: { x: 120, y: 60 } });
    await s.callout("Taps and keys go straight to the device", 1800);
    await s.hold(1500);
    await s.click("#rc-canvas", { offset: { x: 300, y: 200 } });
    await s.hold(1800);
    await s.zoomOut(700);
    await s.hold(600);
  },
  // bulk actions with live acks
  async actions(s) {
    await s.goto("/commands", 2500);
    await s.click(".pill[data-type='reboot']");
    await s.hold(700);
    await s.click(`#scope-rail .sri[data-mode='restaurant'] >> nth=0`);
    await s.callout("Target a whole venue", 1500);
    await s.hold(1500);
    await s.click("#send-btn");
    await s.page.waitForURL(/\/commands\//, { timeout: 15000 }).catch(() => {});
    await s.zoom();
    await s.hold(900);
    await s.callout("Every device acknowledges live", 1800);
    await s.zoomIn("text=Rebooted >> nth=0", 1.3, 900).catch(() => {});
    await s.hold(4200);
    await s.zoomOut(600);
    await s.confetti();
    await s.hold(1600);
  },
  // OTA rollout landing in real time
  async rollout(s) {
    sql(`DELETE FROM updates WHERE id IN (SELECT ud.update_id FROM update_devices ud JOIN devices d ON d.id = ud.device_id WHERE d.${REEL});
         DELETE FROM commands WHERE type = 'ota' AND id IN (SELECT ct.command_id FROM command_targets ct JOIN devices d ON d.id = ct.target_id WHERE d.${REEL});`);
    sql(`UPDATE devices SET build_id = 'v2.0.8i4' WHERE id IN (SELECT id FROM devices WHERE ${REEL} AND product = 't7' AND last_seen_at > now() - interval '10 minutes' ORDER BY serial_number LIMIT 14)`);
    const pidFile = new URL("./out/sim.pid", import.meta.url).pathname;
    if (existsSync(pidFile)) { try { process.kill(Number(readFileSync(pidFile, "utf8").trim()), "SIGUSR1"); } catch {} }
    await sleep(2500);
    await s.goto(`/updates/new?release=${RELEASE_ID}`, 2500);
    await s.click("#push-all");
    await s.callout("Whole build, every eligible tablet", 1600);
    await s.hold(900);
    await s.click("#push-btn");
    await s.click("#mdm-confirm-ok").catch(() => {});
    await s.page.waitForURL((u) => !/updates\/new/.test(u.href), { timeout: 15000 }).catch(() => {});
    await s.zoom();
    await s.hold(1200);
    await s.callout("Download → verify → install, per device", 2000);
    await s.hold(3200);
    await s.zoomIn("h3:has-text('Device status')", 1.4, 800);
    await s.hold(3000);
    await s.zoomOut(700);
    await s.scroll(300, 800);
    await s.hold(1200);
    await s.confetti();
    await s.callout("Fleet updated, A/B slot kept for rollback", 1800);
    await s.hold(1800);
  },
  // convenience: alerts that say what to do
  async alerts(s) {
    await s.goto("/alerts", 2500);
    await s.glide("a:has-text('Open device') >> nth=0", { dur: 800 }).catch(() => {});
    await s.callout("Plain language, and what to do about it", 1800);
    await s.hold(1500);
    await s.zoomIn("form[action$='/ack'] button >> nth=0", 1.4, 900);
    await s.click("form[action$='/ack'] button >> nth=0").catch(() => {});
    await s.reloaded();
    await s.zoomOut(600);
    await s.callout("Acknowledged", 1100);
    await s.hold(1200);
  },
  // convenience: filters that answer the question
  async devices(s) {
    await s.goto("/devices", 2500);
    await s.click("button.fleet-view[data-view='online']");
    await s.hold(900); await s.zoom();
    await s.callout("Who's online?", 1300);
    await s.hold(1000);
    await s.click(`a.ri-main:has-text("${RESTAURANT.split(" — ")[0]}")`).catch(() => {});
    await s.reloaded();
    await s.callout("What's at this venue?", 1400);
    await s.hold(1200);
    await s.zoomIn("a.link-detail >> nth=1", 1.45);
    await s.glide("a.link-detail >> nth=3", { dur: 900 });
    await s.hold(1200);
    await s.zoomOut();
    await s.hold(500);
  },
};

let ORDER = ["overview", "health", "map", "remote", "actions", "rollout", "alerts", "devices"];
// Another reel can bring its own scenes: REEL_SCRIPT=reel-actions.mjs exports
// { SCENES, ORDER, prepare? }. prepare(browser, ctx) runs once before recording.
let SCRIPT = null;
if (process.env.REEL_SCRIPT) {
  SCRIPT = await import(new URL("./" + process.env.REEL_SCRIPT, import.meta.url).href);
  Object.assign(SCENES, SCRIPT.SCENES);
  ORDER = SCRIPT.ORDER;
}
const wanted = process.argv.slice(2).length ? process.argv.slice(2) : ORDER;

// ---------------------------------------------------------------- run
const browser = await chromium.launch();
console.log(`hero device ${HERO}, release ${RELEASE_ID}, venue ${RESTAURANT}`);
// Log in once in a throwaway context; scenes reuse the session cookie so the
// recording never shows the login page or the post-login redirect.
const auth = await browser.newContext();
{
  const p = await auth.newPage();
  await p.goto(BASE + "/login", { waitUntil: "load" });
  await p.fill("#username", USER); await p.fill("#password", PASS);
  await Promise.all([p.waitForURL((u) => !/\/login/.test(u.href)), p.click("button[type=submit]")]);
}
const storageState = await auth.storageState();
await auth.close();
if (SCRIPT && SCRIPT.prepare) await SCRIPT.prepare({ browser, storageState, BASE, sql, sleep });
for (const name of wanted) {
  const idx = String(ORDER.indexOf(name) + 1).padStart(2, "0");
  const tmpDir = OUT + "_tmp_" + name + "/";
  rmSync(tmpDir, { recursive: true, force: true });
  const ctx = await browser.newContext({
    viewport: { width: W, height: H }, deviceScaleFactor: 1,
    recordVideo: { dir: tmpDir, size: { width: W, height: H } },
    reducedMotion: "no-preference",
    storageState,
  });
  const ctxStart = Date.now();
  await ctx.addInitScript((theme) => { try { localStorage.setItem("mdm-theme", theme); } catch {} }, THEME);
  await ctx.addInitScript(OVERLAY_JS);
  const page = await ctx.newPage();
  const s = new Scene(page, ctxStart);
  s.onFirstPaint = () => { s.firstPaintMs = s.now(); s.trimMs = s.firstPaintMs + 400; }; // skip the blank pre-navigation frames
  const t0 = Date.now();
  process.stdout.write(`▶ ${name} … `);
  try { await SCENES[name](s); } catch (e) { console.log(`\n   ! ${name}: ${e.message.split("\n")[0]}`); }
  await ctx.close(); // flushes the video
  const file = readdirSync(tmpDir).find((f) => f.endsWith(".webm"));
  renameSync(tmpDir + file, `${OUT}${idx}-${name}.webm`);
  // All times are ms since context creation. The video's own clock starts later
  // (when capture begins); assemble.mjs finds the first paint in the video and
  // aligns trimMs/cam to it using firstPaintMs.
  writeFileSync(`${OUT}${idx}-${name}.json`, JSON.stringify({ trimMs: s.trimMs || 0, firstPaintMs: s.firstPaintMs || 0, cam: s.camEvents }));
  rmSync(tmpDir, { recursive: true, force: true });
  console.log(`${((Date.now() - t0) / 1000).toFixed(1)}s → ${OUT.replace(process.cwd() + "/", "")}${idx}-${name}.webm`);
}
await browser.close();
