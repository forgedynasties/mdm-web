// Chapter 5 — The kiosk app crashed. Scenario: crash events + alert on one unit.
export default {
  id: "ch05",
  number: 5,
  title: "The app crashed",
  intro: "Chapter five. The kiosk app crashed. Finding the crash, reading the log, and pushing a fix.",
  theme: "light",
  login: true,
  setup: async (s) => {
    s.target = s.pick("product = 't7' AND last_seen_at > now() - interval '10 minutes'", 7);
    for (let i = 0; i < 3; i++) s.sql(`INSERT INTO device_events (device_id, kind, summary, detail, occurred_at, build_id) SELECT id, 'data_app_crash', 'com.aio.pos: java.lang.NullPointerException at OrderCartAdapter.bind', 'FATAL EXCEPTION: main\nProcess: com.aio.pos\njava.lang.NullPointerException: Attempt to invoke virtual method on a null object reference\n\tat com.aio.pos.ui.OrderCartAdapter.bind(OrderCartAdapter.java:142)', now() - interval '${3 + i * 4} minutes', build_id FROM devices WHERE serial_number='${s.target}' ON CONFLICT DO NOTHING`);
    s.alert(s.target, "device_crash", "warning", "3 crashes in 15 min (com.aio.pos)", { crashes: 3 });
  },
  teardown: async (s) => { s.resolveAlerts(s.target, "device_crash"); },
  steps: [
    { id: "alert", text: "A crash alert fires when the same app dies repeatedly in a short window. One crash is noise; three in fifteen minutes is a pattern.",
      run: async (s) => { await s.page.goto(s.BASE + "/alerts", { waitUntil: "load" }); await s.zoom(); await s.hold(800); await s.glide("text=crashes >> nth=0", { dur: 1000 }).catch(() => {}); } },
    { id: "events", text: "On the device, the Alerts tab lists each crash with the exception. The first line usually names the screen that broke.",
      run: async (s) => { await s.page.goto(s.BASE + "/devices/" + s.target, { waitUntil: "load" }); await s.zoom(); await s.hold(600); await s.click("#dd-tab-alerts"); await s.hold(1800); await s.scroll(200, 700); } },
    { id: "trace", text: "Expand one to see the stack trace. Copy it into the ticket for the app team; they do not need device access to read it.",
      run: async (s) => { await s.click("details summary >> nth=0").catch(() => {}); await s.hold(2200); } },
    { id: "logcat", text: "Need more context? Request a logcat from the History tab. The device uploads the last few thousand lines within a minute.",
      run: async (s) => { await s.click("#dd-tab-history"); await s.hold(1500); await s.glide("button:has-text('Logcat'), a:has-text('Logcat'), text=logcat >> nth=0", { dur: 900 }).catch(() => {}); await s.hold(800); } },
    { id: "fix", text: "When the app team ships a fix, push it from Actions, Install app, targeted at that venue. The crash alert resolves once the count drops.",
      run: async (s) => { await s.page.goto(s.BASE + "/commands", { waitUntil: "load" }); await s.zoom(); await s.hold(800); await s.click("#apk-picker").catch(() => {}); await s.hold(1800); await s.page.keyboard.press("Escape"); await s.hold(400); } },
  ],
};
