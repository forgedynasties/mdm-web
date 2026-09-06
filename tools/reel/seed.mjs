#!/usr/bin/env node
// Seed the TEST MDM instance (:8082) with a realistic-looking demo fleet for the
// showcase reel. Everything it creates is tagged (restaurant notes = 'reel-seed',
// group names prefixed "[reel]"; devices belong to those restaurants) so
// `--clean` removes only what it added.
//
//   node seed.mjs            # add demo fleet
//   node seed.mjs --clean    # remove demo fleet
//
// Writes SQL straight into the compose postgres container (no host psql needed),
// then drops the daily-stats backfill flag and restarts the server so 7 days of
// history roll up into the fleet-health / overview charts.

import { execFileSync } from "node:child_process";

const TAG = "reel-seed";
const DAYS = 7;
const STEP_MIN = 8; // checkin cadence in history (must stay under the chart's 15-min gap threshold)
const COMPOSE_DIR = new URL("../../", import.meta.url).pathname;

const psql = (sql) =>
  execFileSync(
    "docker",
    ["compose", "exec", "-T", "postgres", "sh", "-c",
      'psql -v ON_ERROR_STOP=1 -q -U "$POSTGRES_USER" -d "$POSTGRES_DB"'],
    { cwd: COMPOSE_DIR, input: sql, stdio: ["pipe", "inherit", "inherit"], maxBuffer: 1 << 28 },
  );

// Deterministic PRNG so re-seeding gives the same fleet.
let s = 20260906;
const rnd = () => ((s = (s * 1664525 + 1013904223) >>> 0) / 2 ** 32);
const ri = (a, b) => a + Math.floor(rnd() * (b - a + 1));
const pick = (arr) => arr[Math.floor(rnd() * arr.length)];
const q = (v) => "'" + String(v).replace(/'/g, "''") + "'";
const j = (o) => q(JSON.stringify(o));
const uuid = () => {
  const h = () => Math.floor(rnd() * 0xffff).toString(16).padStart(4, "0");
  return `${h()}${h()}-${h()}-4${h().slice(1)}-a${h().slice(1)}-${h()}${h()}${h()}`;
};

const RESTAURANTS = [
  { name: "Harbor Grill — Embarcadero", address: "1 Ferry Building, San Francisco, CA", lat: 37.7955, lon: -122.3937, n: 9 },
  { name: "Mission Taqueria", address: "2889 Mission St, San Francisco, CA", lat: 37.7521, lon: -122.4184, n: 7 },
  { name: "Sunset Noodle Bar", address: "1901 Irving St, San Francisco, CA", lat: 37.7636, lon: -122.4788, n: 6 },
  { name: "Uptown Burgers — Oakland", address: "2000 Broadway, Oakland, CA", lat: 37.8107, lon: -122.2683, n: 8 },
  { name: "Campus Café — Berkeley", address: "2495 Bancroft Way, Berkeley, CA", lat: 37.8686, lon: -122.2566, n: 6 },
  { name: "University Ave Bistro — Palo Alto", address: "342 University Ave, Palo Alto, CA", lat: 37.4467, lon: -122.1608, n: 8 },
  { name: "Downtown Pho — San José", address: "150 S 1st St, San Jose, CA", lat: 37.3352, lon: -121.8891, n: 9 },
  { name: "Bayside Pizza — Sausalito", address: "660 Bridgeway, Sausalito, CA", lat: 37.8591, lon: -122.4784, n: 5 },
];

const PRODUCTS = [
  { code: "t7", prefix: "AT070AA26", model: "AIO T7", kiosk: "com.aio.pos", w: 6 },
  { code: "kiosk22", prefix: "AK220AA26", model: "AIO Kiosk 22", kiosk: "com.aio.kiosk", w: 3 },
  { code: "kiosk27", prefix: "AK270AA26", model: "AIO Kiosk 27", kiosk: "com.aio.kiosk", w: 2 },
];
const BUILDS = [["v2.0.8i5", 70], ["v2.0.8i4", 22], ["v2.0.8i2", 8]];
const weighted = (rows) => {
  let r = rnd() * rows.reduce((a, [, w]) => a + w, 0);
  for (const [v, w] of rows) if ((r -= w) < 0) return v;
  return rows[0][0];
};
const NICK = ["Front counter", "Drive-thru", "Bar", "Patio", "Register 1", "Register 2", "Host stand", "Kitchen pass", "Pickup", "Self-order A", "Self-order B", "Manager"];

// ---------------------------------------------------------------- clean
if (process.argv.includes("--clean")) {
  psql(`
    DELETE FROM devices WHERE restaurant_id IN (SELECT id FROM restaurants WHERE notes = ${q(TAG)});
    DELETE FROM restaurants WHERE notes = ${q(TAG)};
    DELETE FROM groups WHERE name LIKE '[reel] %';
    DELETE FROM device_daily_stats WHERE device_id NOT IN (SELECT id FROM devices);
    UPDATE devices SET hidden = false WHERE last_seen_at > now() - interval '10 days';
  `);
  console.log("demo fleet removed");
  process.exit(0);
}

// ---------------------------------------------------------------- build fleet
const now = Date.now();
let sql = [`BEGIN;`];
let seq = 100;
const devices = [];
const groupIds = { t7: uuid(), kiosk: uuid() };
sql.push(`INSERT INTO groups (id, name) VALUES (${q(groupIds.t7)}, '[reel] Counter tablets'), (${q(groupIds.kiosk)}, '[reel] Self-order kiosks');`);

for (const r of RESTAURANTS) {
  r.id = uuid();
  sql.push(`INSERT INTO restaurants (id, name, address, latitude, longitude, timezone, notes)
    VALUES (${q(r.id)}, ${q(r.name)}, ${q(r.address)}, ${r.lat}, ${r.lon}, 'America/Los_Angeles', ${q(TAG)});`);
  const nicks = [...NICK];
  for (let i = 0; i < r.n; i++) {
    const p = weighted(PRODUCTS.map((p) => [p, p.w]));
    const serial = `${p.prefix}${String(seq++).padStart(5, "0")}`;
    const d = {
      id: uuid(), serial, product: p, build: weighted(BUILDS), rest: r,
      nick: nicks.splice(Math.floor(rnd() * nicks.length), 1)[0],
      lat: r.lat + (rnd() - 0.5) * 0.0006, lon: r.lon + (rnd() - 0.5) * 0.0006,
      wifi: pick(["AIO-Store", "Guest-5G", "Restaurant-POS", "Store-IoT"]),
      ip: `10.${ri(10, 40)}.${ri(1, 8)}.${ri(20, 240)}`,
      ram_base: ri(40, 62), rssi_base: ri(-66, -46), temp_base: 29 + rnd() * 5,
      storage_base: 36 + rnd() * 22,
      scenario: "healthy",
      minutes_ago: ri(0, 2),
    };
    devices.push(d);
  }
}

// Sprinkle problems so alerts / fleet health have something to show.
const scenario = (n, name, t7Only = false) => { for (let i = 0; i < n; i++) { const d = pick(devices.filter((d) => d.scenario === "healthy" && (!t7Only || d.product.code === "t7"))); if (d) d.scenario = name; } };
scenario(3, "offline");        // silent for hours
scenario(2, "battery_low", true);
scenario(1, "overheating");
scenario(2, "wifi_weak");
scenario(1, "storage_low");
scenario(1, "memory_pressure");
scenario(2, "crashy");
scenario(1, "wlc_dead", true);

const localHour = (t) => (new Date(t).getUTCHours() + 24 - 7) % 24; // PDT
const extraFor = (d, t /* ms */, batt) => {
  const h = localHour(t);
  const isT7 = d.product.code === "t7";
  // Tablets sit on the pad outside service hours; on the counter 8am-9pm.
  // On the pad 9pm-11pm (charging to the overnight cap), held off-pad-equivalent
  // (pad idle) until the 6-8am top-up, then on the counter through service.
  const charging = isT7 ? ((h >= 21 || (h >= 6 && h < 8)) ? rnd() < 0.92 : rnd() < 0.08) : undefined;
  const total = isT7 ? 3630 : 7852;
  const ramPct = d.scenario === "memory_pressure" ? ri(84, 93) : d.ram_base + ri(-4, 4);
  const used = Math.round((total * ramPct) / 100);
  const rssi = d.scenario === "wifi_weak" ? ri(-88, -76) : d.rssi_base + ri(-3, 3);
  // slow diurnal swing (+ small noise) instead of per-sample jitter so the chart reads as a curve
  const swing = 1.8 * Math.sin((t / 3600_000) * (Math.PI / 6)) + (h >= 11 && h <= 14 ? 1.5 : 0);
  const temp = d.scenario === "overheating" ? 46 + swing + rnd() : d.temp_base + swing + (rnd() - 0.5) * 0.4;
  const e = {
    model: d.product.model,
    wifi: `"${d.wifi}"`, wifi_rssi: rssi, ip_address: d.ip,
    timezone: "America/Los_Angeles",
    latitude: +d.lat.toFixed(6), longitude: +d.lon.toFixed(6),
    ram_usage_mb: { used, total, available: total - used },
    battery_temp_c: +temp.toFixed(1),
    uptime_seconds: ri(3600 * 20, 3600 * 24 * 12),
    wifi_disconnects_1h: d.scenario === "wifi_weak" ? ri(2, 6) : 0,
    // steady, drifting down ~50 MB/day; the storage_low unit sits just under 1 GB
    storage_free_gb: d.scenario === "storage_low" ? +(0.6 + rnd() * 0.1).toFixed(1) : +(d.storage_base + ((now - t) / 86400_000) * 0.05).toFixed(1),
    kiosk_enabled: true, kiosk_package: d.product.kiosk,
    boot_id: d.boot_id,
  };
  if (isT7) {
    e.charging = charging;
    e.wlc_status = d.scenario === "wlc_dead" ? 0 : charging ? 1 : 0;
  }
  return e;
};

for (const d of devices) {
  d.boot_id = uuid();
  const lastSeenMinAgo = d.scenario === "offline" ? ri(90, 600) : d.minutes_ago;
  const lastSeen = now - lastSeenMinAgo * 60_000;
  // battery curve (tablets only): 6-8am top-up to ~95, discharge through service
  // 8am-9pm down to ~35, overnight charge held at a battery-friendly ~55.
  const battAt = (t) => {
    if (d.product.code !== "t7") return 50; // no battery on kiosks: never rendered; 50 keeps it out of the <20 and >=60 counters
    const h = localHour(t) + new Date(t).getUTCMinutes() / 60;
    let b;
    if (h >= 6 && h < 8) b = 55 + (h - 6) * 20;
    else if (h >= 8 && h < 21) b = 95 - (h - 8) * (60 / 13);
    else if (h >= 21) b = 35 + (h - 21) * 7;
    else b = Math.min(55, 56 + h * 0); // 0-6am: held at cap
    b -= d.serial.charCodeAt(12) % 7;
    if (d.scenario === "battery_low") b = Math.max(6, b - 40);
    return Math.max(5, Math.min(100, Math.round(b + (rnd() - 0.5) * 3)));
  };
  const latestBatt = battAt(lastSeen);
  const latestExtra = extraFor(d, lastSeen, latestBatt);
  sql.push(`INSERT INTO devices (id, serial_number, build_id, latest_battery_pct, latest_extra, last_seen_at, created_at, restaurant_id, last_boot_id, notes, product)
    VALUES (${q(d.id)}, ${q(d.serial)}, ${q(d.build)}, ${latestBatt}, ${j(latestExtra)}, to_timestamp(${lastSeen / 1000}), to_timestamp(${(now - DAYS * 86400_000 - ri(0, 40) * 86400_000) / 1000}), ${q(d.rest.id)}, ${q(d.boot_id)}, '', ${q(d.product.code)});`);
  sql.push(`INSERT INTO device_nicknames (device_id, name) VALUES (${q(d.id)}, ${q(d.nick)});`);
  sql.push(`INSERT INTO device_config (device_id, kiosk_enabled, kiosk_package) VALUES (${q(d.id)}, true, ${q(d.product.kiosk)});`);
  sql.push(`INSERT INTO device_groups (device_id, group_id) VALUES (${q(d.id)}, ${q(d.product.code === "t7" ? groupIds.t7 : groupIds.kiosk)});`);

  // history
  const rows = [];
  for (let t = now - DAYS * 86400_000; t < lastSeen; t += STEP_MIN * 60_000 + ri(-60, 60) * 1000) {
    const b = battAt(t);
    rows.push(`(${q(d.id)}, ${b}, ${q(d.build)}, ${j(extraFor(d, t, b))}, to_timestamp(${t / 1000}))`);
  }
  rows.push(`(${q(d.id)}, ${latestBatt}, ${q(d.build)}, ${j(latestExtra)}, to_timestamp(${lastSeen / 1000}))`);
  for (let i = 0; i < rows.length; i += 400)
    sql.push(`INSERT INTO checkins (device_id, battery_pct, build_id, extra, created_at) VALUES ${rows.slice(i, i + 400).join(",")};`);

  // installed apps (Applications tab)
  const APPS = d.product.code === "t7"
    ? [["com.aio.pos", "AIO POS", "4.12.0"], ["com.aio.mdm", "AIO MDM Agent", d.build], ["com.aio.printer", "Receipt Printer", "2.3.1"], ["com.aio.kds", "Kitchen Display", "1.8.4"], ["com.squareup.reader", "Card Reader", "6.2.0"]]
    : [["com.aio.kiosk", "AIO Self-Order", "3.5.2"], ["com.aio.mdm", "AIO MDM Agent", d.build], ["com.aio.payments", "Payments", "2.0.9"], ["com.aio.signage", "Digital Signage", "1.2.0"]];
  for (const [pkg, name, ver] of APPS)
    sql.push(`INSERT INTO device_packages (device_id, package_name, app_name, version_name, is_system) VALUES (${q(d.id)}, ${q(pkg)}, ${q(name)}, ${q(ver)}, false);`);
  for (const [pkg, name, ver] of [["com.android.settings", "Settings", "12"], ["com.android.systemui", "System UI", "12"], ["com.google.android.gms", "Google Play services", "24.31.33"]])
    sql.push(`INSERT INTO device_packages (device_id, package_name, app_name, version_name, is_system) VALUES (${q(d.id)}, ${q(pkg)}, ${q(name)}, ${q(ver)}, true);`);

  // events: a reboot or two, crashes for the crashy ones
  for (let k = 0; k < ri(1, 2); k++)
    sql.push(`INSERT INTO device_events (device_id, kind, summary, occurred_at, build_id) VALUES (${q(d.id)}, 'reboot', ${q(pick(["", "ota", "watchdog", "user"]))}, to_timestamp(${(now - ri(1, DAYS * 24) * 3600_000) / 1000}), ${q(d.build)}) ON CONFLICT DO NOTHING;`);
  if (d.scenario === "crashy")
    for (let k = 0; k < ri(3, 6); k++)
      sql.push(`INSERT INTO device_events (device_id, kind, summary, detail, occurred_at, build_id) VALUES (${q(d.id)}, 'data_app_crash', ${q(`${d.product.kiosk}: java.lang.NullPointerException at OrderCartAdapter.bind`)}, 'FATAL EXCEPTION: main\nProcess: ${d.product.kiosk}\njava.lang.NullPointerException: Attempt to invoke virtual method on a null object reference\n\tat com.aio.pos.ui.OrderCartAdapter.bind(OrderCartAdapter.java:142)', to_timestamp(${(now - ri(1, DAYS * 24) * 3600_000) / 1000}), ${q(d.build)}) ON CONFLICT DO NOTHING;`);

  // alerts
  const alert = (type, sev, summary, detail = {}, minAgo = ri(5, 300)) =>
    sql.push(`INSERT INTO alerts (rule_id, type, device_id, severity, status, summary, detail, fired_at, last_seen_at, occurrences)
      SELECT id, ${q(type)}, ${q(d.id)}, ${q(sev)}, 'open', ${q(summary)}, ${j(detail)}, now() - interval '${minAgo} minutes', now() - interval '${ri(0, Math.min(minAgo, 4))} minutes', ${ri(1, 4)}
      FROM alert_rules WHERE type = ${q(type)} LIMIT 1;`);
  switch (d.scenario) {
    case "offline": alert("offline", "warning", `Offline — last check-in ${Math.round(lastSeenMinAgo / 60)}h ago`, { offline_minutes: lastSeenMinAgo }, lastSeenMinAgo - 5); break;
    case "battery_low": alert("battery_low", "warning", `Battery ${latestBatt}% during peak (< 20%)`, { soc_pct: latestBatt }); break;
    case "overheating": alert("overheating", "critical", `Battery ${latestExtra.battery_temp_c}°C (≥ 45°C)`, { temp_c: latestExtra.battery_temp_c }); break;
    case "wifi_weak": alert("wifi_weak", "warning", `Wi-Fi ${latestExtra.wifi_rssi} dBm for 10+ min`, { rssi_dbm: latestExtra.wifi_rssi }); break;
    case "storage_low": alert("storage_low", "critical", `Storage ${latestExtra.storage_free_gb} GB free (< 1 GB)`, { free_gb: latestExtra.storage_free_gb }); break;
    case "memory_pressure": alert("memory_pressure", "warning", `RAM ${Math.round(latestExtra.ram_usage_mb.used * 100 / latestExtra.ram_usage_mb.total)}% used (≥ 85%)`, {}); break;
    case "crashy": alert("device_crash", "warning", `3 crashes in 15 min (${d.product.kiosk})`, { crashes: 3 }); break;
    case "wlc_dead": alert("wlc_dead", "critical", "Wireless charger not functional all day", {}); break;
  }
}

// A resolved-history sprinkle so the alerts page isn't all "open"
for (let i = 0; i < 12; i++) {
  const d = pick(devices);
  sql.push(`INSERT INTO alerts (rule_id, type, device_id, severity, status, summary, detail, fired_at, resolved_at, last_seen_at)
    SELECT id, 'offline', ${q(d.id)}, 'warning', 'resolved', 'Offline — last check-in ${ri(6, 40)}m ago', '{}', now() - interval '${ri(6, 120)} hours', now() - interval '${ri(1, 5)} hours', now() - interval '${ri(1, 5)} hours'
    FROM alert_rules WHERE type = 'offline' LIMIT 1;`);
}

// Park the instance's real bench devices so the reel shows only the demo fleet
// (hidden devices drop out of every list/count; `--clean` restores them).
sql.push(`UPDATE devices SET hidden = true WHERE restaurant_id IS NULL OR restaurant_id NOT IN (SELECT id FROM restaurants WHERE notes = ${q(TAG)});`);
sql.push(`UPDATE alerts SET status = 'resolved', resolved_at = now() WHERE status <> 'resolved' AND device_id IN (SELECT id FROM devices WHERE hidden);`);

// Force daily-stats backfill on next server boot (history → charts)
sql.push(`DELETE FROM app_flags WHERE flag = 'daily_stats_backfilled_v1';`);
sql.push(`COMMIT;`);

console.log(`seeding ${devices.length} devices across ${RESTAURANTS.length} restaurants, ${DAYS}d history...`);
psql(sql.join("\n"));
console.log("restarting server so daily stats backfill runs...");
execFileSync("docker", ["compose", "restart", "server"], { cwd: COMPOSE_DIR, stdio: "inherit" });
console.log("done. scenarios:", Object.entries(devices.reduce((a, d) => ((a[d.scenario] = (a[d.scenario] || 0) + 1), a), {})).map(([k, v]) => `${k}=${v}`).join(" "));
