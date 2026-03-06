#!/usr/bin/env python3
"""
MDM Device Simulator
Simulates N devices continuously polling the MDM server with random behaviour.

Usage:
    python3 simulate.py

Environment variables:
    MDM_URL        Server base URL  (default: http://localhost:8080)
    MDM_API_KEY    Shared API key   (default: reads from .env in same dir)
    DEVICE_COUNT   Number of devices to simulate (default: 50)
    POLL_INTERVAL  Seconds between polls (default: 60)
"""

import threading
import time
import random
import json
import urllib.request
import urllib.error
import os
import sys
from datetime import datetime

# ── Config ────────────────────────────────────────────────────────────────────

def load_env_file():
    """Read key=value pairs from .env in the same directory as this script."""
    env_path = os.path.join(os.path.dirname(__file__), ".env")
    values = {}
    try:
        with open(env_path) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                key, _, val = line.partition("=")
                values[key.strip()] = val.strip().strip('"').strip("'")
    except FileNotFoundError:
        pass
    return values

_env = load_env_file()

API_URL       = os.environ.get("MDM_URL",       _env.get("MDM_URL", "http://localhost:8080")).rstrip("/")
API_KEY       = os.environ.get("MDM_API_KEY",   _env.get("DEVICE_API_KEY", "changeme"))
DEVICE_COUNT  = int(os.environ.get("DEVICE_COUNT",  "50"))
POLL_INTERVAL = int(os.environ.get("POLL_INTERVAL", "60"))

# ── Simulated data pools ──────────────────────────────────────────────────────

BUILD_IDS = [
    "aosp-eng 13 TP1A.220624.014",
    "aosp-eng 13 TP1A.221005.002",
    "aosp-eng 14 UP1A.231005.007",
    "aosp-userdebug 14 UP1A.231105.003",
    "aosp-user 13 TP1A.220624.014",
]

WIFI_SSIDS    = ["Corp-WiFi", "MDM-Network", "Office-5G", "Warehouse-AP", "Floor3-WiFi"]
IP_PREFIXES   = ["10.0.1", "10.0.2", "192.168.10", "192.168.11"]

# ── Terminal colours ──────────────────────────────────────────────────────────

BOLD  = "\033[1m"
DIM   = "\033[2m"
RED   = "\033[91m"
YELL  = "\033[93m"
GREEN = "\033[92m"
BLUE  = "\033[94m"
CYAN  = "\033[96m"
GREY  = "\033[90m"
RESET = "\033[0m"

# ── Shared state ──────────────────────────────────────────────────────────────

print_lock = threading.Lock()
stats      = {"ok": 0, "err": 0}
stats_lock = threading.Lock()

def ts():
    return datetime.now().strftime("%H:%M:%S")

def log(serial, msg, colour=RESET):
    with print_lock:
        print(f"{GREY}{ts()}{RESET}  {colour}{serial:<12}{RESET}  {msg}")

def battery_colour(pct):
    if pct < 20:   return RED
    if pct < 50:   return YELL
    return GREEN

# ── API call ──────────────────────────────────────────────────────────────────

def checkin(serial, build_id, battery_pct, extra):
    payload = json.dumps({
        "serial_number": serial,
        "build_id":      build_id,
        "battery_pct":   battery_pct,
        "extra":         extra,
    }).encode()

    req = urllib.request.Request(
        f"{API_URL}/api/v1/checkin",
        data=payload,
        headers={"Content-Type": "application/json", "X-API-Key": API_KEY},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=10) as r:
        return r.status

# ── Device simulation ─────────────────────────────────────────────────────────

def run_device(device_id):
    serial    = f"SIM-{device_id:04d}"
    build_id  = random.choice(BUILD_IDS)
    battery   = random.randint(25, 100)
    charging  = random.choice([True, False])
    ip        = f"{random.choice(IP_PREFIXES)}.{random.randint(2, 254)}"
    ssid      = random.choice(WIFI_SSIDS)

    # Stagger startup over one full interval so requests don't all fire at once
    time.sleep(random.uniform(0, POLL_INTERVAL))
    log(serial, f"online  build={build_id[:28]}  battery={battery}%", CYAN)

    while True:
        # ── 5 % chance of a missed poll (device offline / busy) ──
        if random.random() < 0.05:
            log(serial, "skipped poll  (simulated offline)", GREY)
            time.sleep(POLL_INTERVAL + random.uniform(0, 30))
            continue

        # ── Battery behaviour ──
        if charging:
            battery = min(100, battery + random.randint(1, 5))
            if battery == 100 or random.random() < 0.08:
                charging = False
        else:
            battery = max(0, battery - random.randint(0, 3))
            if battery <= 12 or random.random() < 0.04:
                charging = True

        # ── 1 % chance of OTA build update ──
        if random.random() < 0.01:
            candidate = random.choice(BUILD_IDS)
            if candidate != build_id:
                build_id = candidate
                log(serial, f"{BLUE}OTA update → {build_id}{RESET}", BLUE)

        # ── Occasional IP roam ──
        if random.random() < 0.03:
            ip   = f"{random.choice(IP_PREFIXES)}.{random.randint(2, 254)}"
            ssid = random.choice(WIFI_SSIDS)

        extra = {"ip_address": ip, "wifi_ssid": ssid}

        try:
            status   = checkin(serial, build_id, battery, extra)
            bc       = battery_colour(battery)
            icon     = "↑chrg" if charging else "↓drn "
            log(serial,
                f"battery={bc}{battery:3d}%{RESET} {icon}  "
                f"build={build_id[:22]}  "
                f"{GREEN}HTTP {status}{RESET}",
                bc)
            with stats_lock:
                stats["ok"] += 1

        except urllib.error.HTTPError as e:
            log(serial, f"{RED}HTTP {e.code} {e.reason}{RESET}", RED)
            with stats_lock:
                stats["err"] += 1

        except Exception as e:
            log(serial, f"{RED}error: {e}{RESET}", RED)
            with stats_lock:
                stats["err"] += 1

        # Jitter ±10 s around the poll interval
        time.sleep(max(10, POLL_INTERVAL + random.uniform(-10, 10)))

# ── Stats printer ─────────────────────────────────────────────────────────────

def print_stats():
    while True:
        time.sleep(60)
        with stats_lock:
            ok  = stats["ok"]
            err = stats["err"]
        total = ok + err
        rate  = (ok / total * 100) if total else 0
        with print_lock:
            print(
                f"\n{BOLD}{'─'*60}\n"
                f"  Stats  total={total}  ok={ok}  err={err}  "
                f"success={rate:.1f}%\n"
                f"{'─'*60}{RESET}\n"
            )

# ── Entry point ───────────────────────────────────────────────────────────────

def main():
    print(f"\n{BOLD}MDM Device Simulator{RESET}")
    print(f"  URL      : {API_URL}")
    print(f"  API key  : {API_KEY[:8]}{'*' * (len(API_KEY) - 8)}")
    print(f"  Devices  : {DEVICE_COUNT}")
    print(f"  Interval : {POLL_INTERVAL}s  (±10s jitter)")
    print(f"  Stagger  : devices spread over first {POLL_INTERVAL}s\n")

    threading.Thread(target=print_stats, daemon=True).start()

    for i in range(1, DEVICE_COUNT + 1):
        threading.Thread(target=run_device, args=(i,), daemon=True).start()

    try:
        while True:
            time.sleep(1)
    except KeyboardInterrupt:
        with stats_lock:
            ok, err = stats["ok"], stats["err"]
        print(f"\n{YELL}Stopped.{RESET}  ok={ok}  err={err}")
        sys.exit(0)

if __name__ == "__main__":
    main()
