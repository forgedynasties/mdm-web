// Chapter 7 — OTA rollout, including one failed device and a retry.
export default {
  id: "ch07",
  number: 7,
  title: "Roll out a firmware update",
  intro: "Chapter seven. Rolling out a firmware update, and handling the one device that fails.",
  theme: "light",
  login: true,
  setup: async (s) => {
    const REEL = "restaurant_id IN (SELECT id FROM restaurants WHERE notes = 'reel-seed')";
    s.sql(`DELETE FROM updates WHERE id IN (SELECT ud.update_id FROM update_devices ud JOIN devices d ON d.id = ud.device_id WHERE d.${REEL});
           DELETE FROM commands WHERE type = 'ota' AND id IN (SELECT ct.command_id FROM command_targets ct JOIN devices d ON d.id = ct.target_id WHERE d.${REEL});
           UPDATE devices SET build_id = 'v2.0.8i4' WHERE id IN (SELECT id FROM devices WHERE ${REEL} AND product = 't7' AND last_seen_at > now() - interval '10 minutes' ORDER BY serial_number LIMIT 10)`);
    s.release = s.sql(`SELECT id FROM releases WHERE product = 't7' AND status = 'published' AND EXISTS (SELECT 1 FROM ota_packages o WHERE o.release_id = releases.id) ORDER BY created_at DESC LIMIT 1`);
    s.failing = s.pick("product = 't7' AND build_id = 'v2.0.8i4'", 2);
    s.sim(`otafail ${s.failing}`);
    try { process.kill(Number(require("node:fs").readFileSync(new URL("../out/sim.pid", import.meta.url).pathname, "utf8").trim()), "SIGUSR1"); } catch {}
    await s.hold(2500);
  },
  teardown: async (s) => { s.sim(`reset ${s.failing}`); },
  steps: [
    { id: "releases", text: "Releases is the list of builds we have signed off. A release with an active package can be deployed.",
      run: async (s) => { await s.page.goto(s.BASE + "/releases", { waitUntil: "load" }); await s.zoom(); await s.hold(1500); } },
    { id: "new", text: "Deploy update opens the picker. Choose the release, then the devices. Only units on an older build are eligible; the rest show as up to date.",
      run: async (s) => { await s.page.goto(s.BASE + "/updates/new?release=" + s.release, { waitUntil: "load" }); await s.zoom(); await s.hold(1200); await s.scroll(300, 800); } },
    { id: "select", text: "Select all eligible, or tick a venue. Start with one venue on a new build; go fleet-wide once it has run a day.",
      run: async (s) => { await s.click("#push-all"); await s.hold(1500); } },
    { id: "reboot", text: "Reboot policy: immediate reboots right after install; manual waits for a reboot command, for example after closing time.",
      run: async (s) => { await s.glide("text=Manual >> nth=0", { dur: 900 }).catch(() => {}); await s.hold(600); await s.glide("text=Immediate >> nth=0", { dur: 700 }).catch(() => {}); } },
    { id: "push", text: "Push. The deployment page tracks every device: downloading, verifying, installing, rebooted. It is an A/B update, so the old build stays on the other slot.",
      run: async (s) => { await s.click("#push-btn"); await s.click("#mdm-confirm-ok").catch(() => {}); await s.page.waitForURL((u) => !/updates\/new/.test(u.href), { timeout: 15000 }).catch(() => {}); await s.reloaded(); await s.hold(9000); } },
    { id: "failed", text: "One device failed with a hash mismatch: a bad download. That is the common failure. Retry re-sends the same package to that unit only.",
      run: async (s) => { await s.page.reload({ waitUntil: "load" }); await s.zoom(); await s.hold(800); await s.glide("text=Failed >> nth=0", { dur: 900 }).catch(() => s.glide("text=error >> nth=0", { dur: 900 }).catch(() => {})); await s.hold(800); s.sim(`reset ${s.failing}`); await s.hold(1500); await s.click("form[action$='/retry'] button >> nth=0").catch(() => {}); await s.reloaded(); await s.hold(4000); } },
    { id: "done", text: "When every row reads installed, the fleet is on the new build. Devices report the new version on their next check-in.",
      run: async (s) => { await s.page.reload({ waitUntil: "load" }); await s.zoom(); await s.hold(1500); await s.scroll(300, 800); } },
  ],
};
