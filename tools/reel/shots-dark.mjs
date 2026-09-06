import { chromium } from "playwright";
const BASE = "http://127.0.0.1:8082";
const b = await chromium.launch();
const ctx = await b.newContext({ viewport: { width: 1920, height: 1080 } });
await ctx.addInitScript(() => { try { localStorage.setItem("mdm-theme", "dark"); } catch {} });
const p = await ctx.newPage();
await p.goto(BASE + "/login"); await p.fill("#username", "admin"); await p.fill("#password", "admin"); await p.click("button[type=submit]"); await p.waitForLoadState("load");
for (const path of process.argv.slice(2)) {
  await p.goto(BASE + path, { waitUntil: "load" }); await p.waitForTimeout(2500);
  await p.screenshot({ path: "out/shots/dark" + (path.replace(/[^a-z0-9]+/gi, "_") || "root") + ".png" });
}
await b.close();
