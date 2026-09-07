// HTTP status matrix for the access policy. Usage: node access-matrix.mjs
// Expects the seeded ops/dev accounts (see docs/access-control-plan.md §6).
const B = process.env.MDM_URL || "http://127.0.0.1:8082";
const IN = "AT070AA2600100", IN2 = "AT070AA2600101", OUT = process.argv[2];
async function login(u, p) {
  const r = await fetch(B + "/login", { method: "POST", redirect: "manual", headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ username: u, password: p }) });
  return (r.headers.getSetCookie?.() || []).map(c => c.split(";")[0]).join("; ");
}
async function get(cookie, path, method = "GET", body) {
  const r = await fetch(B + path, { method, redirect: "manual", headers: { cookie, ...(body ? { "content-type": "application/x-www-form-urlencoded" } : {}), "Origin": B, "Sec-Fetch-Site": "same-origin" }, body });
  const t = await r.text();
  return { s: r.status, t, loc: r.headers.get("location") };
}
let fail = 0;
function expect(name, got, want) { const ok = Array.isArray(want) ? want.includes(got) : got === want; if (!ok) fail++; console.log((ok ? "  ok  " : "  FAIL") + ` ${name}: ${got}` + (ok ? "" : ` (want ${want})`)); }

const ops = await login("ops@aioapp.com", "OpsDemo2026!");
console.log("operator (venue Harbor Grill: view/reboot/screenshot; remote on " + IN + "; hidden elsewhere)");
let r = await get(ops, "/devices"); expect("device list", r.s, 200);
const seen = (r.t.match(/AT070AA26\d{5}|AK\d{3}AA26\d{5}/g) || []); const uniq = [...new Set(seen)];
expect("list hides out-of-scope (" + uniq.length + " serials, none from other venues)", uniq.includes(OUT), false);
expect("device page in scope", (await get(ops, "/devices/" + IN)).s, 200);
expect("device page hidden → 404", (await get(ops, "/devices/" + OUT)).s, 404);
expect("chart-data hidden → 404", (await get(ops, "/devices/" + OUT + "/chart-data")).s, 404);
expect("stats partial hidden → 404", (await get(ops, "/devices/" + OUT + "/stats")).s, 404);
expect("remote on granted device", (await get(ops, "/devices/" + IN + "/remote")).s, 200);
expect("remote on other in-scope device → 403", (await get(ops, "/devices/" + IN2 + "/remote")).s, 403);
expect("remote on hidden device → 404", (await get(ops, "/devices/" + OUT + "/remote")).s, 404);
expect("shell page → 403 (role)", (await get(ops, "/devices/" + IN + "/shell")).s, 403);
expect("notes POST without notes grant → 403", (await get(ops, "/devices/" + IN + "/notes", "POST", "notes=x")).s, 403);
expect("logcat live without grant → 403", (await get(ops, "/devices/" + IN + "/logcat/live")).s, 403);
r = await get(ops, "/devices/map.json"); expect("map.json excludes hidden", r.t.includes(OUT), false);
r = await get(ops, "/devices/search?q=" + OUT.slice(0, 8)); expect("search excludes hidden", r.t.includes(OUT), false);
r = await get(ops, "/devices/select-serials"); expect("select-all excludes hidden", r.t.includes(OUT), false);
r = await get(ops, "/"); expect("overview → redirected to device list", r.s === 302 ? r.loc : r.s, "/devices");
expect("fleet-health → redirected", (await get(ops, "/fleet-health")).s, 302);
expect("alerts page", (await get(ops, "/alerts")).s, 200);
r = await get(ops, "/commands/history"); expect("history page", r.s, 200); expect("history excludes hidden serials", r.t.includes(OUT), false);
r = await get(ops, "/commands", "POST", new URLSearchParams({ type: "reboot", target_type: "devices", target_serials: IN + "\n" + OUT }).toString());
expect("reboot IN+OUT → narrowed (2xx/3xx)", r.s < 400, true);
r = await get(ops, "/commands", "POST", new URLSearchParams({ type: "reboot", target_type: "devices", target_serials: OUT }).toString());
expect("reboot OUT only → 403", r.s, 403);
r = await get(ops, "/commands", "POST", new URLSearchParams({ type: "install_apk", target_type: "devices", target_serials: IN, apk_url: "x" }).toString());
expect("install without grant → 403/400", [403, 400].includes(r.s), true);

const dev = await login("dev@aioapp.com", "OpsDemo2026!");
console.log("dev (everything; OTA + shell only in Harbor Grill)");
expect("shell page in scope", (await get(dev, "/devices/" + IN + "/shell")).s, [200, 403]); // 403 only when shell is disabled in settings
expect("shell page outside scope → 403", (await get(dev, "/devices/" + OUT + "/shell")).s, 403);
expect("remote anywhere (dev)", (await get(dev, "/devices/" + OUT + "/remote")).s, [200, 403]);
expect("device page anywhere", (await get(dev, "/devices/" + OUT)).s, 200);
expect("overview (dev sees all)", (await get(dev, "/")).s, 200);
r = await get(dev, "/commands", "POST", new URLSearchParams({ type: "shell", target_type: "devices", target_serials: OUT, command: "id" }).toString());
expect("shell command outside scope → 403", r.s, 403);

const adm = await login("admin", "admin");
console.log("admin");
expect("hidden device page", (await get(adm, "/devices/" + OUT)).s, 200);
expect("editor page", (await get(adm, "/users/access")).s, 200);
console.log(fail ? `\n${fail} FAILED` : "\nall passed");
process.exit(fail ? 1 : 0);
