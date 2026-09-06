// Chapter 4 — Overheating. Scenario: one tablet climbs to 48°C.
export default {
  id: "ch04",
  number: 4,
  title: "A tablet runs hot",
  intro: "Chapter four. A tablet runs hot. Heat is the number one killer of batteries, so this one is worth ten minutes.",
  theme: "light",
  login: true,
  setup: async (s) => { s.target = s.pick("product = 't7' AND last_seen_at > now() - interval '10 minutes'", 5); s.sim(`temp ${s.target} 48.5`); await s.hold(3000); s.alert(s.target, "overheating", "critical", "Battery 48.5°C (≥ 45°C)", { temp_c: 48.5 }); },
  teardown: async (s) => { s.sim(`reset ${s.target}`); s.resolveAlerts(s.target, "overheating"); },
  steps: [
    { id: "alert", text: "Overheating alerts are critical. The threshold is forty-five degrees on the battery sensor, which is well past comfortable.",
      run: async (s) => { await s.page.goto(s.BASE + "/alerts", { waitUntil: "load" }); await s.zoom(); await s.hold(800); await s.glide("text=Battery 48 >> nth=0", { dur: 1000 }).catch(() => s.glide("text=Overheating >> nth=0", { dur: 1000 }).catch(() => {})); } },
    { id: "temp", text: "Open the device and switch vitals to Temp. A slow climb during service means placement: next to a grill, a fryer, or in the sun.",
      run: async (s) => { await s.page.goto(s.BASE + "/devices/" + s.target, { waitUntil: "load" }); await s.zoom(); await s.hold(600); await s.click("button:has-text('Temp')").catch(() => {}); await s.hold(1200); await s.click("button:has-text('24h')").catch(() => {}); await s.hold(1500); } },
    { id: "spike", text: "A sudden spike while charging points at the pad or the battery itself. Look at the charging colour on the battery chart for the same window.",
      run: async (s) => { await s.click("button:has-text('Battery')").catch(() => {}); await s.hold(2000); } },
    { id: "act", text: "Ask the venue to move it off the heat source and off the pad for a while. Acknowledge the alert so the team knows it is being handled.",
      run: async (s) => { await s.page.goto(s.BASE + "/alerts", { waitUntil: "load" }); await s.zoom(); await s.hold(600); await s.click("form[action$='/ack'] button >> nth=0").catch(() => {}); await s.reloaded(); await s.hold(1200); } },
    { id: "clear", text: "The alert resolves on its own once the sensor is back under the line. If it keeps coming back on the same unit, it is a hardware ticket.",
      run: async (s) => { s.sim(`reset ${s.target}`); await s.hold(2500); await s.glide("text=Acknowledged >> nth=0", { dur: 900 }).catch(() => {}); } },
  ],
};
