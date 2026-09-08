// Actions-page reel: scenes for record.mjs (REEL_SCRIPT=reel-actions.mjs).
// Record as the operator account so the Recent / Most frequent panes show real
// operator sends (admin sends are excluded from presets by design):
//   MDM_USER=ops@aioapp.com MDM_PASS=OpsDemo2026! REEL_SCRIPT=reel-actions.mjs \
//   REEL_SCENES_DIR=out/scenes_actions node record.mjs
import { execFileSync } from "node:child_process";

const COMPOSE_DIR = new URL("../../", import.meta.url).pathname;
const sql = (q) => execFileSync("docker",
  ["compose", "exec", "-T", "postgres", "sh", "-c", 'psql -At -U "$POSTGRES_USER" -d "$POSTGRES_DB"'],
  { cwd: COMPOSE_DIR, input: q }).toString().trim();
const REEL = `restaurant_id IN (SELECT id FROM restaurants WHERE notes = 'reel-seed')`;
const ONLINE_T7 = sql(`SELECT serial_number FROM devices WHERE ${REEL} AND product = 't7'
  AND last_seen_at > now() - interval '10 minutes' ORDER BY serial_number LIMIT 6`).split("\n").filter(Boolean);
const [D1, D2, D3, D4, D5] = ONLINE_T7;
const BOGUS = "AT070AA2699999";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Send a few things as the operator so the presets panes have real history:
// the same reboot to the same pair three times (→ Most frequent) and a couple
// of one-offs (→ Recent). Driven through the real console, headless.
export async function prepare({ browser, storageState, BASE }) {
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 }, storageState });
  const page = await ctx.newPage();
  const sendTo = async (serials, action) => {
    await page.goto(BASE + "/commands", { waitUntil: "load" });
    await page.keyboard.press("Escape");
    await page.fill("#co-q", serials.join(" "));
    await page.keyboard.press("Enter");
    await page.waitForFunction((n) => document.querySelectorAll("#co-pills [data-rm]").length >= n, serials.length, { timeout: 10000 }).catch(() => {});
    await page.click(`.co-act[data-t='${action}']`);
    await sleep(600);
    await page.click("#co-send");
    await sleep(500);
    if (await page.locator("#co-send:has-text('Confirm')").count()) await page.click("#co-send");
    await page.waitForURL(/\/commands\//, { timeout: 15000 }).catch(() => {});
    await sleep(800);
  };
  for (let i = 0; i < 3; i++) await sendTo([D1, D2], "reboot");
  await sendTo([D3], "screenshot");
  await sendTo([D4, D5], "screenshot");
  await sendTo([D3, D4], "reboot");
  // One pinned setup so the Pinned pane isn't empty (only if none exists yet).
  await page.goto(BASE + "/commands", { waitUntil: "load" });
  await page.keyboard.press("Escape");
  if (!(await page.locator("#p-pinned .p-pin-ic").count())) {
    await page.fill("#co-q", [D1, D2].join(" "));
    await page.keyboard.press("Enter");
    await page.waitForFunction(() => document.querySelectorAll("#co-pills [data-rm]").length >= 2, null, { timeout: 10000 }).catch(() => {});
    await page.click(".co-act[data-t='reboot']");
    await sleep(600);
    await page.click(".p-newpin");
    await page.fill("#recipe-name", "Nightly reboot, front counter");
    await page.click("#recipe-modal .btn-primary");
    await sleep(1200);
  }
  await ctx.close();
  await sleep(1500);
}

export const ORDER = ["who", "paste", "what", "send", "presets"];

export const SCENES = {
  // scope tabs → a venue in one click. Camera: push in on the rail, pan down the
  // scope list, pull back to see the pills fill.
  async who(s) {
    s.recenter = false; // the camera frames things; the page itself never jumps
    await s.goto("/commands", 2400);
    await s.glide("#co-stabs", { dur: 800 });
    await s.zoomIn("#co-stabs", 1.6, 700, 650);
    await s.callout("Scope tabs", 1300);
    await s.click(".co-stab[data-stab='restaurant']");
    await s.hold(900);
    await s.frame("#co-scopes", 1.6, 700, 650); // pan down: the whole scope list in frame
    await s.glide(".co-scope[data-kind='restaurant'] >> nth=0", { dur: 700 });
    await s.hold(400);
    await s.click(".co-scope[data-kind='restaurant'] >> nth=0");
    await s.callout("One click, a whole venue", 1700);
    await s.hold(1600);
    await s.zoomOut(700, 650);
    await s.hold(600);
    await s.click(".co-stab[data-stab='group']");
    await s.hold(1300);
    await s.click("#co-clear");
    await s.hold(700);
  },
  // paste serials → validated, unknown ones called out
  async paste(s) {
    s.recenter = false;
    await s.goto("/commands", 2400);
    await s.click("#co-q");
    await s.zoomIn("#co-q", 1.5, 700, 650);
    await s.callout("Paste serials from a sheet", 1500);
    await s.page.keyboard.type(`${D1} ${D2} ${BOGUS} ${D3}`, { delay: 22 });
    await s.hold(700);
    await s.page.keyboard.press("Enter");
    await s.page.locator("#co-missing-modal.modal-open").waitFor({ timeout: 8000 }).catch(() => {});
    await s.hold(400);
    await s.frame("#co-missing-modal .modal-box", 1.6, 700, 650); // pan to the popup
    await s.callout("Typos don't ride along", 1700);
    await s.hold(2000);
    await s.click("#co-missing-modal .btn-primary");
    await s.hold(500);
    await s.frame("#co-devs", 1.6, 700, 650).catch(() => {}); // pan to the list
    await s.glide("#co-devs .co-dev.on >> nth=0", { dur: 700 }).catch(() => {});
    await s.callout("Real ones added, picked rows on top", 1700);
    await s.hold(1800);
    await s.zoomOut(700, 650);
    await s.hold(400);
  },
  // action grid adapts; app picker closes after each pick
  async what(s) {
    s.recenter = false;
    await s.goto("/commands", 2400);
    await s.click(".co-scope[data-kind='restaurant'] >> nth=0");
    await s.hold(800);
    await s.frame("#co-grid", 1.5, 700, 650);
    await s.callout("Only what every selected device can run", 1800);
    await s.hold(1400);
    await s.click(".co-act[data-t='install_apk']");
    await s.hold(700);
    await s.scroll(260, 1100); // bring the payload strip up smoothly
    await s.hold(300);
    await s.frame("#co-strip", 1.5, 700, 650); // pan down to the strip
    await s.click(".co-addbtn[data-menu='apps']");
    await s.hold(800);
    await s.click(".co-opt >> nth=0");
    await s.hold(700);
    await s.callout("Picker closes after each pick", 1500);
    await s.hold(900);
    await s.click(".co-addbtn[data-menu='apps']");
    await s.hold(800);
    await s.click(".co-opt >> nth=2");
    await s.hold(1400);
    await s.zoomOut(700, 650);
    await s.hold(500);
  },
  // reboot three devices → land on the command page, acks live
  async send(s) {
    s.recenter = false;
    await s.goto("/commands", 2400);
    await s.click("#co-q");
    await s.page.keyboard.type(`${D1} ${D2} ${D3}`, { delay: 16 });
    await s.page.keyboard.press("Enter");
    await s.hold(900);
    await s.click(".co-act[data-t='reboot']");
    await s.hold(700);
    await s.scroll(240, 1000);
    await s.zoomIn("#co-send", 1.5, 700, 650);
    await s.click("#co-send");
    await s.callout("Confirm", 900);
    await s.hold(800);
    await s.zoomOut(700, 650); // back to wide before the page changes
    await s.click("#co-send");
    await s.page.waitForURL(/\/commands\//, { timeout: 15000 }).catch(() => {});
    await s.zoom();
    await s.hold(900);
    await s.callout("Straight to the result", 1600);
    await s.frame(".ad-head", 1.5, 700, 650);
    await s.hold(1200);
    await s.zoomIn("text=Rebooted >> nth=0", 1.5, 700, 650).catch(() => {}); // pan to the column
    await s.hold(3200);
    await s.zoomOut(700, 650);
    await s.confetti();
    await s.hold(1600);
  },
  // presets: pinned / recent / most frequent, one-click resend
  async presets(s) {
    s.recenter = false;
    await s.goto("/commands", 2400);
    await s.scroll(900, 1800); // glide down to the presets
    await s.hold(600);
    await s.glide(".p-cap:has-text('Most frequent')", { dur: 800 });
    await s.callout("What operators actually send", 1700);
    await s.hold(1000);
    await s.frame(".p-stack > div:nth-child(1)", 1.5, 700, 650);
    await s.hold(1200);
    await s.frame(".p-stack > div:nth-child(2)", 1.5, 700, 650); // pan across
    await s.hold(1000);
    await s.click(".p-stack > div:nth-child(2) .p-resend >> nth=0");
    await s.hold(800);
    await s.zoomOut(700, 650);
    await s.callout("Loaded into the console, ready to send", 1800);
    await s.glide("#co-send", { dur: 900 });
    await s.hold(1800);
  },
};
