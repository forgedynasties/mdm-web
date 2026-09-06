#!/usr/bin/env node
// Training recorder: one continuous take per chapter with step markers.
// Each step runs its actions, then holds until its narration has finished.
//   node narrate.mjs ch01 && node training.mjs ch01
// → out/training/ch01/take.webm + take.json (step in/out times, ms since context creation)

import { chromium } from "playwright";
import { execFileSync } from "node:child_process";
import { mkdirSync, renameSync, readdirSync, rmSync, writeFileSync, readFileSync, appendFileSync } from "node:fs";

const id = process.argv[2] || "ch01";
const ch = (await import(`./chapters/${id}.mjs`)).default;
const BASE = process.env.MDM_URL || "http://127.0.0.1:8082";
const ZOOM = parseFloat(process.env.REEL_ZOOM || "1.45");
// Recorded at the exact size of the video slot in the Remotion layout, so nothing
// (top bar, bottom dock) is cropped away.
const W = 1792, H = 800;
const DIR = new URL(`./out/training/${id}/`, import.meta.url).pathname;
mkdirSync(DIR, { recursive: true });
const narration = JSON.parse(readFileSync(`${DIR}narration.json`, "utf8"));
const COMPOSE_DIR = new URL("../../", import.meta.url).pathname;
const sql = (q) => execFileSync("docker", ["compose", "exec", "-T", "postgres", "sh", "-c", 'psql -At -U "$POSTGRES_USER" -d "$POSTGRES_DB"'], { cwd: COMPOSE_DIR, input: q }).toString().trim();
const HERO = sql(`SELECT d.serial_number FROM devices d WHERE restaurant_id IN (SELECT id FROM restaurants WHERE notes = 'reel-seed') AND product = 't7' AND last_seen_at > now() - interval '10 minutes' ORDER BY serial_number LIMIT 1`);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const ease = (t) => (t < 0.5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2);

// Cursor overlay only (no ripples/callouts: captions do the talking in training)
const OVERLAY_JS = `(() => {
  const c = document.createElement('div'); c.id = '__reel_cursor';
  c.style.cssText = 'position:fixed;z-index:2147483647;left:-100px;top:-100px;width:22px;height:30px;margin:-2px 0 0 -3px;pointer-events:none;transition:transform .08s;zoom:1';
  c.innerHTML = '<svg viewBox="0 0 24 32" width="22" height="30"><path d="M3 2l17 13-7.5 1.2L17 27l-4 1.8-4.6-10.6L3 24z" fill="#111" stroke="#fff" stroke-width="1.6" stroke-linejoin="round"/></svg>';
  const st = document.createElement('style'); st.textContent = '.__reel_ripple{position:fixed;z-index:2147483646;width:14px;height:14px;margin:-7px 0 0 -7px;border-radius:50%;border:3px solid #2563eb;pointer-events:none;animation:__rip .5s ease-out forwards;zoom:1}@keyframes __rip{from{transform:scale(1);opacity:.9}to{transform:scale(4);opacity:0}}';
  const add = () => { document.documentElement.appendChild(st); document.documentElement.appendChild(c); };
  document.readyState === 'loading' ? document.addEventListener('DOMContentLoaded', add) : add();
  let mx = 0, my = 0;
  window.addEventListener('mousemove', e => { mx = e.clientX; my = e.clientY; c.style.left = mx + 'px'; c.style.top = my + 'px'; }, true);
  window.addEventListener('mousedown', () => { c.style.transform = 'scale(.85)'; const r = document.createElement('div'); r.className = '__reel_ripple'; r.style.left = mx + 'px'; r.style.top = my + 'px'; document.documentElement.appendChild(r); setTimeout(() => r.remove(), 550); }, true);
  window.addEventListener('mouseup', () => { c.style.transform = ''; }, true);
})();`;

class Scene {
  constructor(page, t0) { this.page = page; this.t0 = t0; this.pos = { x: W / 2, y: H / 2 }; this.BASE = BASE; this.HERO = HERO; this.OPS_USER = process.env.OPS_USER || "ops@aioapp.com"; this.OPS_PASS = process.env.OPS_PASS || "OpsDemo2026!"; }
  now() { return Date.now() - this.t0; }
  locator(t) { return typeof t === "string" ? this.page.locator(t).first() : t; }
  async box(target) {
    const el = this.locator(target);
    await el.waitFor({ state: "visible", timeout: 10000 });
    await el.scrollIntoViewIfNeeded();
    let b = await el.boundingBox(); if (!b) return null;
    if (b.y < 150 || b.y + b.height > H - 150) { await el.evaluate((e) => e.scrollIntoView({ block: "center", behavior: "instant" })); await sleep(350); b = await el.boundingBox(); }
    return b;
  }
  async glide(target, { dur = 900, offset = { x: 0, y: 0 } } = {}) { const b = await this.box(target); if (!b) return; await this.moveTo(b.x + b.width / 2 + offset.x, b.y + b.height / 2 + offset.y, dur); }
  async moveTo(x, y, dur = 900) {
    const steps = Math.max(12, Math.round(dur / 16)); const from = { ...this.pos };
    for (let i = 1; i <= steps; i++) { const t = ease(i / steps); await this.page.mouse.move(from.x + (x - from.x) * t, from.y + (y - from.y) * t); await sleep(dur / steps); }
    this.pos = { x, y };
  }
  async click(target, opts = {}) { await this.glide(target, opts); await sleep(220); await this.page.mouse.down(); await sleep(90); await this.page.mouse.up(); }
  async type(text, { secret = false } = {}) { for (const ch of text) { await this.page.keyboard.type(ch); await sleep(secret ? 70 : 90 + Math.random() * 60); } }
  async scroll(px, dur = 900) { const n = Math.max(10, Math.round(dur / 40)); for (let i = 0; i < n; i++) { await this.page.mouse.wheel(0, px / n); await sleep(dur / n); } }
  async zoom() { await this.page.evaluate((z) => { document.body.style.zoom = z; }, ZOOM).catch(() => {}); }
  async reloaded() { await this.page.waitForLoadState("load").catch(() => {}); await this.zoom(); await this.noTour(); }
  // a fresh account gets the guided tour on first load; skip it so it never covers a step
  async noTour() {
    const skip = this.page.locator(".tour-skip");
    if (await skip.isVisible().catch(() => false)) { await skip.click().catch(() => {}); await sleep(400); }
  }
  async hold(ms) { await sleep(ms); }
  // ---- scenario helpers
  sql(q) { return sql(q); }
  sim(cmd) { appendFileSync(new URL("./out/sim.cmd", import.meta.url).pathname, cmd + "\n"); }
  // a device of the demo fleet: healthy online tablet by default
  pick(where = "product = 't7' AND last_seen_at > now() - interval '10 minutes'", offset = 0) {
    return sql(`SELECT serial_number FROM devices WHERE restaurant_id IN (SELECT id FROM restaurants WHERE notes = 'reel-seed') AND ${where} ORDER BY serial_number OFFSET ${offset} LIMIT 1`);
  }
  // fire an alert now (the rule engine runs once a minute; on camera we don't wait)
  alert(serial, type, severity, summary, detail = {}) {
    sql(`INSERT INTO alerts (rule_id, type, device_id, severity, status, summary, detail)
         SELECT r.id, '${type}', d.id, '${severity}', 'open', '${summary.replace(/'/g, "''")}', '${JSON.stringify(detail)}'
         FROM devices d LEFT JOIN alert_rules r ON r.type = '${type}' WHERE d.serial_number = '${serial}' LIMIT 1
         ON CONFLICT DO NOTHING`);
  }
  resolveAlerts(serial, type) { sql(`UPDATE alerts SET status='resolved', resolved_at=now() WHERE device_id=(SELECT id FROM devices WHERE serial_number='${serial}') AND type='${type}' AND status<>'resolved'`); }
}

const browser = await chromium.launch();
const tmp = DIR + "_tmp/"; rmSync(tmp, { recursive: true, force: true });
const ctx = await browser.newContext({ viewport: { width: W, height: H }, recordVideo: { dir: tmp, size: { width: W, height: H } } });
const t0 = Date.now();
await ctx.addInitScript((theme) => { try { localStorage.setItem("mdm-theme", theme); } catch {} }, ch.theme || "light");
await ctx.addInitScript(OVERLAY_JS);
const page = await ctx.newPage();
const s = new Scene(page, t0);
const marks = [];
for (const st of ch.steps) {
  const start = s.now();
  process.stdout.write(`▶ ${st.id} … `);
  await s.noTour();
  try { await st.run(s); } catch (e) { console.log(`\n   ! ${st.id}: ${e.message.split("\n")[0]}`); }
  await s.noTour();
  // hold until the narration (which starts with the step) has finished, plus a beat
  const need = narration.steps[st.id].dur * 1000 + 700;
  const elapsed = s.now() - start;
  if (elapsed < need) await sleep(need - elapsed);
  marks.push({ id: st.id, text: st.text, in: start, out: s.now() });
  console.log(`${((s.now() - start) / 1000).toFixed(1)}s`);
}
await ctx.close(); await browser.close();
const f = readdirSync(tmp).find((x) => x.endsWith(".webm"));
renameSync(tmp + f, `${DIR}take.webm`); rmSync(tmp, { recursive: true, force: true });
writeFileSync(`${DIR}take.json`, JSON.stringify({ id, number: ch.number, title: ch.title, intro: ch.intro, steps: marks }, null, 2));
console.log(`→ ${DIR}take.webm (${(s.now() / 1000).toFixed(1)}s)`);
