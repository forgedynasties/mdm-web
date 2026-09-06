// Chapter 9 — Onboarding a new device: it appears unassigned, we name and place it.
export default {
  id: "ch09",
  number: 9,
  title: "Onboard a new device",
  intro: "Chapter nine. A new device out of the box. Naming it, placing it, and checking its policy.",
  theme: "light",
  login: true,
  setup: async (s) => {
    // a fresh unit: no restaurant, no nickname, checked in a minute ago
    s.target = "AT070AA2600199";
    s.sql(`INSERT INTO devices (serial_number, build_id, latest_battery_pct, latest_extra, last_seen_at, product, notes)
           VALUES ('${s.target}', 'v2.0.8i5', 87, '{"model":"AIO T7","wifi":"\\"AIO-Office\\"","wifi_rssi":-52,"charging":true,"kiosk_enabled":false}', now(), 't7', '') ON CONFLICT (serial_number) DO UPDATE SET restaurant_id = NULL, hidden = false, last_seen_at = now();
           DELETE FROM device_nicknames WHERE device_id = (SELECT id FROM devices WHERE serial_number='${s.target}')`);
    s.alert(s.target, "new_device", "info", `New device onboarded: ${s.target}`);
  },
  teardown: async (s) => { s.sql(`DELETE FROM devices WHERE serial_number='${s.target}'`); },
  steps: [
    { id: "appears", text: "A new unit checks in the first time it gets Wi-Fi. It shows up in Fleet as unassigned, and a new-device alert tells you it arrived.",
      run: async (s) => { await s.page.goto(s.BASE + "/devices?q=" + s.target, { waitUntil: "load" }); await s.zoom(); await s.hold(1500); await s.glide("a.link-detail >> nth=0", { dur: 900 }).catch(() => {}); } },
    { id: "open", text: "Open it. The serial is printed on the back of the device; match it before you assign anything.",
      run: async (s) => { await s.page.goto(s.BASE + "/devices/" + s.target, { waitUntil: "load" }); await s.zoom(); await s.hold(1500); } },
    { id: "name", text: "Name it the way the venue talks about it: Front counter, Bar, Patio. Save.",
      run: async (s) => { await s.click(".dd-nick summary").catch(() => {}); await s.hold(500); await s.click(".dd-nick input[name=nickname]").catch(() => {}); await s.type("Front counter"); await s.hold(400); await s.click(".dd-nick button[type=submit] >> nth=0").catch(() => {}); await s.reloaded(); await s.hold(1000); } },
    { id: "place", text: "Placement: pick the restaurant. Groups are optional; they are handy for staged rollouts.",
      run: async (s) => { await s.glide("text=Placement >> nth=0", { dur: 900 }).catch(() => {}); await s.hold(500); await s.click("text=Lab (unassigned) >> nth=0").catch(() => s.glide("form[action$='/restaurant']", { dur: 800 }).catch(() => {})); await s.hold(800); await s.click("form[action$='/restaurant'] label:has-text('Harbor')").catch(() => {}); await s.hold(600); await s.click("form[action$='/restaurant'] button[type=submit]").catch(() => {}); await s.reloaded(); await s.hold(1200); } },
    { id: "policy", text: "Check the kiosk policy under Policies: which app it locks to and whether the offline exit is allowed. New units inherit the venue default.",
      run: async (s) => { await s.page.goto(s.BASE + "/manage", { waitUntil: "load" }); await s.zoom(); await s.hold(2000); await s.scroll(250, 800); } },
    { id: "verify", text: "Back on the device page it now has a name, a venue and a lock. Ready for the counter.",
      run: async (s) => { await s.page.goto(s.BASE + "/devices/" + s.target, { waitUntil: "load" }); await s.zoom(); await s.hold(2000); } },
  ],
};
