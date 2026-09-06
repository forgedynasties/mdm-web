#!/usr/bin/env node
// Renders a few fake self-order screens (JPEG, 1280x800 landscape tablet) that the
// simulator streams to the Remote Control page. Tapping in the dashboard advances
// to the next screen, so the session looks interactive on camera.
//   node kiosk-screens.mjs   → out/kiosk/0N.jpg

import { chromium } from "playwright";
import { mkdirSync } from "node:fs";

const OUT = new URL("./out/kiosk/", import.meta.url).pathname;
mkdirSync(OUT, { recursive: true });

const base = (body, extra = "") => `<!doctype html><html><head><meta charset="utf-8"><style>
  @import url('https://fonts.googleapis.com/css2?family=Poppins:wght@500;600;700&family=Inter:wght@400;500;600&display=swap');
  html,body{margin:0;width:1280px;height:800px;overflow:hidden;background:#f6f1ea;font-family:Inter,system-ui,sans-serif;color:#1c1a17}
  .top{height:84px;display:flex;align-items:center;justify-content:space-between;padding:0 40px;background:#fff;border-bottom:1px solid #e9e2d8}
  .brand{font:700 30px Poppins;color:#c0392b}.brand span{color:#1c1a17}
  .clock{font:500 22px Inter;color:#6b645b}
  .wrap{display:flex;height:716px}
  .cats{width:250px;background:#fff;border-right:1px solid #e9e2d8;padding:24px 0}
  .cat{padding:18px 32px;font:600 20px Inter;color:#6b645b}.cat.on{color:#c0392b;background:#fbeae7;border-right:4px solid #c0392b}
  .grid{flex:1;padding:32px;display:grid;grid-template-columns:repeat(3,1fr);gap:24px;align-content:start}
  .item{background:#fff;border-radius:18px;overflow:hidden;box-shadow:0 6px 24px rgba(0,0,0,.06)}
  .item .ph{height:130px;background:linear-gradient(135deg,var(--a),var(--b))}
  .item .t{padding:16px 18px 6px;font:600 20px Inter}.item .p{padding:0 18px 18px;font:500 18px Inter;color:#c0392b}
  .cart{width:330px;background:#fff;border-left:1px solid #e9e2d8;display:flex;flex-direction:column}
  .cart h3{margin:0;padding:26px 28px 12px;font:700 22px Poppins}
  .line{display:flex;justify-content:space-between;padding:12px 28px;font:500 18px Inter;color:#3d3833}
  .tot{margin-top:auto;padding:22px 28px;border-top:1px solid #e9e2d8;display:flex;justify-content:space-between;font:700 22px Poppins}
  .btn{margin:0 28px 28px;padding:20px;border-radius:14px;background:#c0392b;color:#fff;text-align:center;font:700 22px Poppins}
  .modal{position:absolute;inset:0;background:rgba(28,26,23,.55);display:flex;align-items:center;justify-content:center}
  .card{width:620px;background:#fff;border-radius:24px;padding:40px;text-align:center}
  .card h1{font:700 40px Poppins;margin:0 0 12px}.card p{font:400 22px Inter;color:#6b645b;margin:0 0 28px}
  .big{font:700 96px Poppins;color:#c0392b;margin:10px 0 24px}
  ${extra}
</style></head><body>${body}</body></html>`;

const items = [["Smash Burger", "$12.50", "#f7b267,#f4845f"], ["Fish Tacos", "$11.00", "#a8dadc,#457b9d"], ["Caesar Salad", "$9.50", "#b7e4c7,#52b788"], ["Garlic Fries", "$5.00", "#ffe066,#f4a261"], ["Clam Chowder", "$8.00", "#e9c46a,#e76f51"], ["Lemonade", "$4.00", "#fff3b0,#ffd166"]];
const grid = (hl = -1) => items.map(([t, p, c], i) => `<div class="item" style="--a:${c.split(",")[0]};--b:${c.split(",")[1]};${i === hl ? "outline:4px solid #c0392b;" : ""}"><div class="ph"></div><div class="t">${t}</div><div class="p">${p}</div></div>`).join("");
const shell = (inner, cart, modal = "") => `
<div class="top"><div class="brand">Harbor <span>Grill</span></div><div class="clock">Order at the counter · 11:42</div></div>
<div class="wrap"><div class="cats"><div class="cat on">Mains</div><div class="cat">Sides</div><div class="cat">Drinks</div><div class="cat">Desserts</div></div>
<div class="grid">${inner}</div><div class="cart"><h3>Your order</h3>${cart}</div></div>${modal}`;

const SCREENS = [
  shell(grid(), `<div class="line" style="color:#a39c92">Tap an item to start</div><div class="tot"><span>Total</span><span>$0.00</span></div><div class="btn" style="opacity:.4">Checkout</div>`),
  shell(grid(0), `<div class="line"><span>1 × Smash Burger</span><span>$12.50</span></div><div class="tot"><span>Total</span><span>$12.50</span></div><div class="btn">Checkout</div>`),
  shell(grid(0), `<div class="line"><span>1 × Smash Burger</span><span>$12.50</span></div><div class="line"><span>1 × Garlic Fries</span><span>$5.00</span></div><div class="tot"><span>Total</span><span>$17.50</span></div><div class="btn">Checkout</div>`),
  shell(grid(), `<div class="line"><span>1 × Smash Burger</span><span>$12.50</span></div><div class="line"><span>1 × Garlic Fries</span><span>$5.00</span></div><div class="tot"><span>Total</span><span>$17.50</span></div><div class="btn">Checkout</div>`,
    `<div class="modal"><div class="card"><h1>Thanks!</h1><p>Your order number is</p><div class="big">42</div><p>We'll call you when it's ready.</p></div></div>`),
];

const b = await chromium.launch();
const p = await b.newPage({ viewport: { width: 1280, height: 800 } });
for (let i = 0; i < SCREENS.length; i++) {
  await p.setContent(base(SCREENS[i]), { waitUntil: "load" });
  await p.waitForTimeout(500);
  await p.screenshot({ path: `${OUT}0${i}.jpg`, type: "jpeg", quality: 80 });
  console.log("screen", i);
}
await b.close();
