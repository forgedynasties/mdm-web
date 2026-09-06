#!/usr/bin/env node
// Renders the title / section / outro cards as short animated clips
// (out/cards/*.webm, 1920x1080) plus a still PNG of the settled frame.
// Pure HTML/CSS animation recorded with Playwright, so restyling is a CSS edit.
//
//   node cards.mjs            # all cards
//   CARD_SEC=3.2 node cards.mjs
//
// Card kinds: title (kicker/title/sub), bullets (title + list), compare (two columns).

import { chromium } from "playwright";
import { mkdirSync, readdirSync, renameSync, rmSync } from "node:fs";

const OUT = new URL("./out/cards/", import.meta.url).pathname;
const SEC = parseFloat(process.env.CARD_SEC || "3.2");
mkdirSync(OUT, { recursive: true });

// The script. Card file names match the scene they precede (see record.mjs ORDER).
const CARDS = [
  { file: "00-intro", kicker: "AIO MDM · built in-house", title: "We manage our own fleet.<br>With our own software.", sub: "Kiosks and tablets in every venue, run from one dashboard our team designed, wrote and operates.", big: true },
  { file: "01-overview", kicker: "The stack", title: "Small, fast, ours",
    bullets: ["Go server · PostgreSQL · htmx dashboard · WebSocket hub", "Android agent baked into our own ROM as a privileged system app", "A/B seamless OTA, lock-task kiosk, H.264 screen capture", "Docker Compose · S3 for packages · runs on a single box"] },
  { file: "02-health", kicker: "Why not buy one?", title: "Off-the-shelf MDMs vs ours",
    compare: {
      left: { head: "Off-the-shelf", rows: ["Per-device, per-month fees", "Generic Android, no idea what a charging pad is", "Manages apps, not firmware", "Alerts you have to decode", "Your fleet data on their cloud"] },
      right: { head: "AIO MDM", rows: ["No seat fees, ever", "Knows our hardware: pads, kiosks, venues", "Ships whole builds over the air", "Alerts in plain language, with the fix", "Our servers, our data"] },
    } },
  { file: "03-map", kicker: "Geolocation", title: "Every device on the map", sub: "Wi-Fi scans resolved to a position. No GPS needed, works indoors, clustered by venue." },
  { file: "04-remote", kicker: "Anywhere", title: "Reach any device, wherever it is", sub: "Live screen, taps and keys from the browser. Shell, reboot, screenshot, logs. Nothing to install on your laptop." },
  { file: "05-actions", kicker: "Bulk actions", title: "One click, a whole venue", sub: "Target by restaurant, group or serial list. Every device acknowledges live." },
  { file: "06-rollout", kicker: "OTA updates", title: "Ship firmware like an app", sub: "A/B seamless updates with per-device progress, reboot policy and a rollback slot." },
  { file: "07-alerts", kicker: "Convenience", title: "It tells you what to do", sub: "Plain-language alerts, a daily briefing, filters that answer the question you actually have." },
  { file: "99-outro", kicker: "AIO MDM", title: "Our hardware.<br>Our software.<br>Our rules.", sub: "aioapp.com", big: true, credit: "Music: Kevin MacLeod (incompetech.com), CC BY 4.0" },
];

const html = (c) => `<!doctype html><html><head><meta charset="utf-8">
<style>
  @import url('https://fonts.googleapis.com/css2?family=Poppins:wght@500;700&family=Inter:wght@400;500;600&display=swap');
  html,body{margin:0;width:1920px;height:1080px;overflow:hidden;background:#0f1115;color:#f3f4f6;font-family:Inter,system-ui,sans-serif}
  .bg{position:absolute;inset:-10%;background:
      radial-gradient(1200px 700px at 15% 85%, rgba(231,76,60,.30), transparent 60%),
      radial-gradient(900px 600px at 85% 15%, rgba(99,102,241,.24), transparent 60%),#0f1115;
      animation:drift 9s ease-in-out infinite alternate}
  @keyframes drift{from{transform:translate(-2%,1%) scale(1.02)}to{transform:translate(2%,-1%) scale(1.08)}}
  .grid{position:absolute;inset:0;background-image:linear-gradient(rgba(255,255,255,.045) 1px,transparent 1px),linear-gradient(90deg,rgba(255,255,255,.045) 1px,transparent 1px);background-size:80px 80px;mask-image:radial-gradient(ellipse at center,#000 30%,transparent 75%);animation:gridin 1.4s ease-out both}
  @keyframes gridin{from{opacity:0;transform:scale(1.1)}to{opacity:1;transform:none}}
  .bar{position:absolute;left:160px;top:0;width:6px;height:0;background:linear-gradient(#ff5a3c,#ff8a3c);border-radius:3px;animation:bar .7s cubic-bezier(.2,.8,.2,1) .15s both}
  @keyframes bar{to{height:100%}}
  .wrap{position:absolute;left:210px;top:0;bottom:0;right:160px;display:flex;flex-direction:column;justify-content:center}
  .kicker{font:500 26px/1 Inter;letter-spacing:.32em;text-transform:uppercase;color:#ff5a3c;margin-bottom:30px;animation:up .6s cubic-bezier(.2,.8,.2,1) .25s both}
  h1{font:700 ${(c.big ? 104 : c.bullets || c.compare ? 76 : 96)}px/1.06 Poppins;margin:0 0 ${(c.bullets || c.compare) ? 44 : 30}px;letter-spacing:-.02em;overflow:hidden;max-width:1500px}
  h1 span{display:block;animation:up .75s cubic-bezier(.2,.8,.2,1) both}
  h1 span:nth-child(2){animation-delay:.14s}
  h1 span:nth-child(3){animation-delay:.28s}
  p{font:400 38px/1.35 Inter;color:#b3b8c4;margin:0;max-width:1400px;animation:up .7s cubic-bezier(.2,.8,.2,1) .55s both}
  ul{list-style:none;margin:0;padding:0}
  li{font:500 36px/1.3 Inter;color:#e6e8ee;padding:14px 0 14px 46px;position:relative;animation:up .6s cubic-bezier(.2,.8,.2,1) both}
  li::before{content:"";position:absolute;left:0;top:30px;width:16px;height:16px;border-radius:50%;background:#ff5a3c;box-shadow:0 0 18px rgba(255,90,60,.7)}
  li:nth-child(1){animation-delay:.45s}li:nth-child(2){animation-delay:.6s}li:nth-child(3){animation-delay:.75s}li:nth-child(4){animation-delay:.9s}li:nth-child(5){animation-delay:1.05s}
  .cmp{display:grid;grid-template-columns:1fr 1fr;gap:36px}
  .col{background:rgba(255,255,255,.04);border:1px solid rgba(255,255,255,.08);border-radius:22px;padding:30px 36px;animation:up .7s cubic-bezier(.2,.8,.2,1) both}
  .col.r{border-color:rgba(255,90,60,.55);box-shadow:0 0 60px rgba(255,90,60,.18);animation-delay:.2s}
  .col h2{font:700 34px Poppins;margin:0 0 14px;color:#9aa0ad}.col.r h2{color:#ff7a5c}
  .col div{font:500 30px/1.25 Inter;color:#d6d9e0;padding:11px 0;border-top:1px solid rgba(255,255,255,.07)}
  .col.l div{color:#8f95a3}
  @keyframes up{from{opacity:0;transform:translateY(46px)}to{opacity:1;transform:none}}
  .brand{position:absolute;right:120px;bottom:80px;font:700 34px Poppins;color:#fff;opacity:0;animation:fade .6s ease .8s forwards}
  .brand span{color:#ff5a3c}
  .credit{position:absolute;left:210px;bottom:84px;font:400 22px Inter;color:#7b8190;opacity:0;animation:fade .6s ease 1.1s forwards}
  @keyframes fade{to{opacity:.9}}
  .dots{position:absolute;inset:0;pointer-events:none}
  .dots i{position:absolute;width:8px;height:8px;border-radius:50%;background:#ff5a3c;opacity:0;animation:pop 2.4s ease-out infinite}
  @keyframes pop{0%{opacity:0;transform:scale(.4)}20%{opacity:.9}100%{opacity:0;transform:scale(2.6)}}
</style></head><body>
<div class="bg"></div><div class="grid"></div><div class="bar"></div>
<div class="dots">${Array.from({ length: 14 }, (_, i) => `<i style="left:${55 + ((i * 37) % 40)}%;top:${15 + ((i * 53) % 70)}%;animation-delay:${(i * 0.37) % 2.4}s"></i>`).join("")}</div>
<div class="wrap"><div class="kicker">${c.kicker}</div><h1>${c.title.split("<br>").map((l) => `<span>${l}</span>`).join("")}</h1>
${c.sub ? `<p>${c.sub}</p>` : ""}
${c.bullets ? `<ul>${c.bullets.map((b) => `<li>${b}</li>`).join("")}</ul>` : ""}
${c.compare ? `<div class="cmp"><div class="col l"><h2>${c.compare.left.head}</h2>${c.compare.left.rows.map((r) => `<div>${r}</div>`).join("")}</div><div class="col r"><h2>${c.compare.right.head}</h2>${c.compare.right.rows.map((r) => `<div>${r}</div>`).join("")}</div></div>` : ""}
</div>
${c.credit ? `<div class="credit">${c.credit}</div>` : ""}
<div class="brand"><span>AIO</span> MDM</div>
</body></html>`;

const b = await chromium.launch();
for (const c of CARDS) {
  const sec = c.bullets || c.compare ? SEC + 2.4 : SEC; // dense cards get more read time
  const tmp = OUT + "_tmp/"; rmSync(tmp, { recursive: true, force: true });
  const ctx = await b.newContext({ viewport: { width: 1920, height: 1080 }, recordVideo: { dir: tmp, size: { width: 1920, height: 1080 } } });
  const p = await ctx.newPage();
  await p.setContent(html(c).replace("<body>", "<body style='visibility:hidden'>"), { waitUntil: "load" });
  await p.waitForTimeout(600); // web fonts, then start the show
  await p.evaluate(() => { document.body.style.visibility = ""; });
  await p.waitForTimeout(sec * 1000 + 300);
  await p.screenshot({ path: `${OUT}${c.file}.png` });
  await ctx.close();
  const f = readdirSync(tmp).find((x) => x.endsWith(".webm"));
  renameSync(tmp + f, `${OUT}${c.file}.webm`);
  rmSync(tmp, { recursive: true, force: true });
  console.log("card", c.file, `${sec}s`);
}
await b.close();
