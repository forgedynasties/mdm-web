// Chapter 7 — Following a firmware rollout as an operator: progress, a failed
// device, retry, reboot. The rollout itself is started behind the scenes in setup
// (deploying needs a role the operator does not have; the chapter does not dwell on it).
import { readFileSync } from "node:fs";

const BASE = process.env.MDM_URL || "http://127.0.0.1:8082";
const REEL = "restaurant_id IN (SELECT id FROM restaurants WHERE notes = 'reel-seed')";

// Start a deployment through the same form the UI posts, using the release account.
async function startRollout(release, serials) {
  const jar = [];
  const login = await fetch(`${BASE}/login`, { method: "POST", redirect: "manual", headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ username: "release@aioapp.com", password: "ReleaseDemo2026!" }) });
  for (const c of login.headers.getSetCookie?.() || []) jar.push(c.split(";")[0]);
  const form = new URLSearchParams({ release_id: release, reboot_behavior: "immediate", delivery: "smart" });
  for (const s of serials) form.append("serials", s);
  const res = await fetch(`${BASE}/updates`, { method: "POST", redirect: "manual", headers: { cookie: jar.join("; "), "content-type": "application/x-www-form-urlencoded" }, body: form });
  return res.headers.get("location") || "";
}

export default {
  id: "ch07",
  number: 7,
  title: "Follow a firmware rollout",
  intro: "Chapter seven. Following a firmware rollout, and handling the one device that fails.",
  theme: "light",
  login: true,
  setup: async (s) => {
    s.sql(`DELETE FROM updates WHERE id IN (SELECT ud.update_id FROM update_devices ud JOIN devices d ON d.id = ud.device_id WHERE d.${REEL});
           DELETE FROM commands WHERE type = 'ota' AND id IN (SELECT ct.command_id FROM command_targets ct JOIN devices d ON d.id = ct.target_id WHERE d.${REEL});
           UPDATE devices SET build_id = 'v2.0.8i4' WHERE id IN (SELECT id FROM devices WHERE ${REEL} AND product = 't7' AND last_seen_at > now() - interval '10 minutes' ORDER BY serial_number LIMIT 8)`);
    s.release = s.sql(`SELECT id FROM releases WHERE product = 't7' AND status = 'published' AND EXISTS (SELECT 1 FROM ota_packages o WHERE o.release_id = releases.id) ORDER BY created_at DESC LIMIT 1`);
    const serials = s.sql(`SELECT string_agg(serial_number, ' ') FROM devices WHERE ${REEL} AND product = 't7' AND build_id = 'v2.0.8i4'`).split(" ").filter(Boolean);
    s.failing = serials[2];
    s.sim(`otafail ${s.failing}`);
    try { process.kill(Number(readFileSync(new URL("../out/sim.pid", import.meta.url).pathname, "utf8").trim()), "SIGUSR1"); } catch (e) { console.log("sim reload:", e.message); }
    await s.hold(4000);
    s.deployUrl = await startRollout(s.release, serials);
    if (!/deployments/.test(s.deployUrl)) {
      const row = s.sql(`SELECT u.release_id || '/deployments/' || u.id FROM updates u ORDER BY u.created_at DESC LIMIT 1`);
      s.deployUrl = `/releases/${row}`;
    }
    console.log("rollout at", s.deployUrl);
    await s.hold(1500);
  },
  teardown: async (s) => { s.sim(`reset ${s.failing}`); },
  steps: [
    { id: "releases", text: "Releases lists the builds we ship. A rollout in progress shows up on its release, and under Updates.",
      run: async (s) => { await s.page.goto(s.BASE + "/releases", { waitUntil: "load" }); await s.zoom(); await s.hold(1500); await s.glide("a[href^='/updates'], text=Updates >> nth=0", { dur: 900 }).catch(() => {}); } },
    { id: "deployment", text: "Open the deployment. Every device has a row: downloading, verifying, installing, rebooted. It is an A B update, so the old build stays on the other slot.",
      run: async (s) => { await s.page.goto(s.BASE + s.deployUrl, { waitUntil: "load" }); await s.zoom(); await s.hold(6000); } },
    { id: "progress", text: "You do not have to stay on the page. Progress keeps coming in over the live connection, and the counters at the top update as devices finish.",
      run: async (s) => { await s.glide("text=Device status >> nth=0", { dur: 900 }).catch(() => {}); await s.hold(1200); await s.scroll(250, 800); await s.hold(2500); } },
    { id: "failed", text: "One device failed with a hash mismatch: a bad download. That is the common failure. Retry re-sends the same package to that unit only.",
      run: async (s) => { await s.page.reload({ waitUntil: "load" }); await s.zoom(); await s.hold(800); await s.glide("text=Failed >> nth=0", { dur: 900 }).catch(() => s.glide("text=error >> nth=0", { dur: 900 }).catch(() => {})); await s.hold(800); s.sim(`reset ${s.failing}`); await s.hold(1500); await s.click("form[action$='/retry'] button >> nth=0").catch(() => {}); await s.reloaded(); await s.hold(4000); } },
    { id: "reboot", text: "With a manual reboot policy the install waits for a reboot command. Reboot all, or one device, from the same page, for example after closing time.",
      run: async (s) => { await s.glide("form[action$='/reboot-all'] button, text=Reboot all >> nth=0", { dur: 900 }).catch(() => {}); await s.hold(1500); } },
    { id: "done", text: "When every row reads installed, the fleet is on the new build. Devices report the new version on their next check-in.",
      run: async (s) => { await s.page.reload({ waitUntil: "load" }); await s.zoom(); await s.hold(1500); await s.scroll(300, 800); await s.hold(800); } },
  ],
};
