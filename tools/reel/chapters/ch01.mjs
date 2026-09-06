// Chapter 1 — Orientation. Each step: narration (spoken + captioned) and what the
// cursor does meanwhile. Holds are sized to the narration automatically.
export default {
  id: "ch01",
  number: 1,
  title: "Orientation",
  intro: "Chapter one. Orientation. Where things live in the dashboard.",
  theme: "light",
  steps: [
    {
      id: "login",
      text: "This is AIO MDM, our in-house dashboard for the kiosk and tablet fleet. From the landing page, Take me to the dashboard leads to sign-in. Use your AIO ops account.",
      run: async (s) => {
        await s.page.goto(s.BASE + "/", { waitUntil: "load" }); await s.hold(1200);
        await s.click("a.ld-btn.primary >> nth=0"); await s.page.waitForURL(/\/login/, { timeout: 15000 }).catch(() => {}); await s.reloaded(); await s.hold(600);
        await s.click("#username"); await s.type(s.OPS_USER); await s.hold(300);
        await s.click("#password"); await s.type(s.OPS_PASS, { secret: true }); await s.hold(400);
        await s.click("button[type=submit]");
        await s.page.waitForURL((u) => !/\/login/.test(u.href)); await s.reloaded();
      },
    },
    {
      id: "overview",
      text: "You land on the Overview. The headline says how many sites need attention today, and how many devices are online right now.",
      run: async (s) => { await s.glide(".ov3-fact:has-text('online')", { dur: 1200 }); },
    },
    {
      id: "score",
      text: "The fleet health score is computed from our own telemetry over the last seven days. Watch or At Risk means open the fleet health page and look at the worst venue first.",
      run: async (s) => { await s.glide(".ov3-st:has-text('Fleet health')", { dur: 1000 }); await s.hold(600); await s.glide("a.ov3-btn.primary", { dur: 900 }); },
    },
    {
      id: "tiles",
      text: "These tiles are live: online, offline, low battery, overheating, crashes and open alerts. Click any of them to jump straight to the filtered device list.",
      run: async (s) => {
        for (const t of ["Online", "Offline", "Low battery", "Overheating", "Crashes", "Open alerts"]) { await s.glide(`text=${t} >> nth=0`, { dur: 550 }).catch(() => {}); await s.hold(250); }
      },
    },
    {
      id: "search",
      text: "The search box at the top finds any device by serial, nickname or venue. Command K opens it from anywhere.",
      run: async (s) => { await s.click("button.topbar-search"); await s.hold(500); await s.type("Harbor"); await s.hold(1800); await s.page.keyboard.press("Escape"); await s.hold(300); },
    },
    {
      id: "dock",
      text: "The dock at the bottom is the main navigation. Overview, Fleet, Actions, Policies, Alerts and Releases.",
      run: async (s) => {
        for (const t of ["Overview", "Fleet", "Actions", "Policies", "Alerts", "Releases"]) { await s.glide(`.dock >> text=${t}`, { dur: 450 }).catch(() => {}); await s.hold(200); }
      },
    },
    {
      id: "bell",
      text: "The bell shows open alerts. A red badge means something needs a decision now, not later.",
      run: async (s) => { await s.glide("a[href='/alerts'] >> nth=0", { dur: 900 }).catch(() => s.glide("text=Alerts >> nth=0", { dur: 900 })); },
    },
    {
      id: "theme",
      text: "Dark or light is your choice, and it is remembered per account.",
      run: async (s) => { const b = "button[aria-label*='theme' i], button[title*='theme' i], .theme-toggle"; await s.click(b).catch(() => {}); await s.hold(1200); await s.click(b).catch(() => {}); await s.hold(600); },
    },
    {
      id: "device",
      text: "Open any device to see its vitals, apps, history and placement. Chapter two covers the device page in detail.",
      run: async (s) => { await s.click("a.ov3-btn:has-text('Fleet health')").catch(() => {}); await s.page.goto(s.BASE + "/devices/" + s.HERO, { waitUntil: "load" }); await s.zoom(); await s.hold(1500); await s.glide("#dd-tab-apps", { dur: 900 }); },
    },
  ],
};
