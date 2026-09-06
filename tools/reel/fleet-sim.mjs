#!/usr/bin/env node
// Fake-device simulator for the showcase reel. Opens one WebSocket per seeded
// device (the dashboard's "online" = live WS), answers telemetry requests with
// the device's stored snapshot, and plays along with commands so an install /
// reboot / shell action looks real on camera.
//
//   node fleet-sim.mjs          # run until Ctrl-C
//
// Devices whose seeded last_seen_at is > 30 min old stay offline on purpose.

import WebSocket from "ws";
import { execFileSync } from "node:child_process";
import { readFileSync, readdirSync, existsSync } from "node:fs";
import { randomUUID } from "node:crypto";

const COMPOSE_DIR = new URL("../../", import.meta.url).pathname;
const BASE = process.env.MDM_URL || "http://127.0.0.1:8082";
const API_KEY = process.env.DEVICE_API_KEY ||
  readFileSync(COMPOSE_DIR + ".env", "utf8").match(/^DEVICE_API_KEY=(.*)$/m)[1].trim();

const rows = JSON.parse(execFileSync("docker",
  ["compose", "exec", "-T", "postgres", "sh", "-c", 'psql -At -U "$POSTGRES_USER" -d "$POSTGRES_DB"'],
  { cwd: COMPOSE_DIR, input: `SELECT COALESCE(json_agg(json_build_object(
      'serial', serial_number, 'build', build_id, 'product', product,
      'batt', latest_battery_pct, 'extra', latest_extra,
      'stale', last_seen_at < now() - interval '30 minutes'))::text, '[]')
    FROM devices WHERE restaurant_id IN (SELECT id FROM restaurants WHERE notes = 'reel-seed');` }).toString());

const wsUrl = BASE.replace(/^http/, "ws") + "/api/v1/ws?serial=";
// Fake screen for Remote Control sessions: JPEG stills from kiosk-screens.mjs,
// streamed as raw binary frames (first byte 0xFF → the dashboard's still path).
const KIOSK_DIR = new URL("./out/kiosk/", import.meta.url).pathname;
const SCREENS = existsSync(KIOSK_DIR) ? readdirSync(KIOSK_DIR).filter((f) => f.endsWith(".jpg")).sort().map((f) => readFileSync(KIOSK_DIR + f)) : [];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let online = 0;

function connect(d) {
  const ws = new WebSocket(wsUrl + encodeURIComponent(d.serial), { headers: { "X-API-Key": API_KEY } });
  const send = (o) => ws.readyState === 1 && ws.send(JSON.stringify(o));
  const telemetry = () => {
    const e = { ...d.extra, uptime_seconds: (d.extra.uptime_seconds += 30) };
    e.wifi_rssi = Math.max(-90, Math.min(-40, e.wifi_rssi + Math.round((Math.random() - 0.5) * 4)));
    e.battery_temp_c = +(e.battery_temp_c + (Math.random() - 0.5) * 0.4).toFixed(1);
    const p = { type: "telemetry", serial_number: d.serial, build_id: d.build, product: d.product, extra: e };
    if (d.product === "t7") p.battery_pct = d.batt;
    send(p);
  };
  ws.on("open", () => { online++; });
  ws.on("message", async (buf) => {
    let m; try { m = JSON.parse(buf); } catch { return; }
    switch (m.type) {
      case "telemetry_request": telemetry(); break;
      case "ping_request": send({ type: "pong_response", nonce: m.nonce || "" }); break;
      case "command": await handleCommand(m); break;
      case "start_capture": {
        if (!SCREENS.length) break;
        d.screen = 0;
        clearInterval(d.capture);
        const fps = Math.min(m.max_fps || 8, 10);
        d.capture = setInterval(() => { if (ws.readyState === 1) ws.send(SCREENS[d.screen % SCREENS.length]); else clearInterval(d.capture); }, 1000 / fps);
        break;
      }
      case "stop_capture": clearInterval(d.capture); break;
      case "input_event":
        // a tap in the dashboard "advances" the kiosk UI
        if (m.action === "touch" && (m.event === "up" || m.event === "tap")) d.screen = (d.screen || 0) + 1;
        break;
      case "logcat_request":
        send({ type: "logcat_result", request_id: m.id, serial_number: d.serial,
          content: fakeLogcat(d) });
        break;
      default: break; // config, input_event, ...
    }
  });
  ws.on("close", async () => { online--; clearInterval(d.capture); await sleep(3000 + Math.random() * 3000); if (!d.stale) connect(d); });
  ws.on("error", () => {});

  async function handleCommand(m) {
    const id = m.id;
    const payload = m.payload || {};
    const ack = (status, extra = {}) => send({ type: "command_ack", command_id: id, serial_number: d.serial, status, ...extra });
    switch (m.command_type) {
      case "install_apk": {
        await sleep(300 + Math.random() * 2500);
        for (let pct = 0; pct <= 100; pct += 20) { ack("downloading", { progress: pct }); await sleep(600 + Math.random() * 400); }
        ack("installing"); await sleep(2500 + Math.random() * 1500);
        ack("installed", { package: payload.package || "com.aio.pos" });
        break;
      }
      case "reboot": {
        await sleep(500 + Math.random() * 4000); // stagger so a fleet-wide reboot visibly lands one by one
        ack("completed"); await sleep(800);
        d.extra.boot_id = randomUUID(); d.extra.uptime_seconds = 5;
        ws.close(); // reconnect after a "boot" — the close handler does that
        break;
      }
      case "shell": {
        await sleep(400 + Math.random() * 600);
        ack("completed", { output: shellOutput(d, payload.cmd || "") });
        break;
      }
      case "screenshot": { await sleep(1200); ack("completed"); break; }
      case "ota": {
        await sleep(300 + Math.random() * 3000);
        // download → verify → install with progress frames, then the terminal
        // ota_status; after "installed" the device reports the new build.
        const prog = (phase, percent) => send({ type: "ota_progress", command_id: id, phase, percent });
        for (let pct = 0; pct <= 100; pct += 10) { prog("downloading", pct); await sleep(500 + Math.random() * 500); }
        send({ type: "ota_status", command_id: id, status: "downloaded" });
        prog("verifying", 100); await sleep(1200);
        for (let pct = 0; pct <= 100; pct += 25) { prog("installing", pct); await sleep(700 + Math.random() * 500); }
        prog("finalizing", 100); await sleep(800);
        send({ type: "ota_status", command_id: id, status: "installed" });
        if (payload.build_id) d.build = payload.build_id;
        if (payload.reboot_behavior === "immediate") { await sleep(1500); d.extra.boot_id = randomUUID(); d.extra.uptime_seconds = 5; ws.close(); }
        else telemetry();
        break;
      }
      default: { await sleep(500 + Math.random() * 800); ack("completed"); }
    }
  }
}

function shellOutput(d, cmd) {
  if (/getprop/.test(cmd)) return `[ro.build.id]: [${d.build}]\n[ro.product.model]: [${d.extra.model}]\n[ro.serialno]: [${d.serial}]`;
  if (/uptime/.test(cmd)) return ` 11:42:07 up 3 days,  4:12,  load average: 0.31, 0.28, 0.25`;
  if (/df/.test(cmd)) return `Filesystem      1K-blocks     Used Available Use% Mounted on\n/data            61079552 12874332  48205220  22% /data`;
  return `$ ${cmd}\nok`;
}

function fakeLogcat(d) {
  const t = () => new Date().toISOString().slice(5, 23).replace("T", " ");
  return [
    `${t()}  1421  1421 I MdmService: telemetry keyframe sent (build ${d.build})`,
    `${t()}  1421  1498 D MdmWebSocket: ping_request → pong_response`,
    `${t()}  2210  2210 I ${d.extra.kiosk_package}: OrderScreen resumed`,
    `${t()}  1421  1421 I MdmService: kiosk lock verified (${d.extra.kiosk_package})`,
    `${t()}   612   612 W WifiService: rssi ${d.extra.wifi_rssi} dBm on ${d.extra.wifi}`,
  ].join("\n");
}

// SIGUSR1: re-read build_id from the DB (record.mjs resets builds before the
// rollout scene so there is something to update).
process.on("SIGUSR1", () => {
  try {
    const fresh = JSON.parse(execFileSync("docker",
      ["compose", "exec", "-T", "postgres", "sh", "-c", 'psql -At -U "$POSTGRES_USER" -d "$POSTGRES_DB"'],
      { cwd: COMPOSE_DIR, input: `SELECT COALESCE(json_object_agg(serial_number, build_id)::text, '{}') FROM devices WHERE restaurant_id IN (SELECT id FROM restaurants WHERE notes = 'reel-seed');` }).toString());
    for (const d of rows) if (fresh[d.serial]) d.build = fresh[d.serial];
    console.log("\nbuilds reloaded");
  } catch (e) { console.log("reload failed", e.message); }
});

for (const d of rows) { if (!d.stale) { connect(d); await sleep(40); } }
console.log(`simulating ${rows.filter((d) => !d.stale).length} live devices (${rows.filter((d) => d.stale).length} kept offline). Ctrl-C to stop.`);
setInterval(() => process.stdout.write(`\r  online: ${online}   `), 2000);
