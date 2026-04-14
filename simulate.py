#!/usr/bin/env python3
"""
Dummy device simulator for the local MDM server.

This script sends check-in requests to ``http://localhost:8080/api/v1/checkin``
by default and simulates a fleet of Android-like devices with changing battery,
network, and app state.

Examples:
    python3 simulate.py
    python3 simulate.py --count 10 --interval 15
    python3 simulate.py --once --count 5
    python3 simulate.py --api-key your-secret-key
"""

import argparse
import json
import os
import random
import signal
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime


BUILD_IDS = [
    "aosp-eng 13 TP1A.220624.014",
    "aosp-eng 13 TP1A.221005.002",
    "aosp-eng 14 UP1A.231005.007",
    "aosp-userdebug 14 UP1A.231105.003",
    "aosp-user 13 TP1A.220624.014",
]

WIFI_SSIDS = [
    "Corp-WiFi",
    "Warehouse-AP",
    "Office-5G",
    "MDM-Lab",
    "Staging-Network",
]

IP_PREFIXES = ["10.0.1", "10.0.2", "192.168.10", "192.168.11"]

APP_CATALOG = [
    ("com.android.settings", "Settings", "14"),
    ("com.android.chrome", "Chrome", "124.0.6367.82"),
    ("com.google.android.youtube", "YouTube", "19.12.36"),
    ("com.spotify.music", "Spotify", "8.9.30"),
    ("com.whatsapp", "WhatsApp", "2.24.8.76"),
    ("com.example.kiosk", "AIO Kiosk", "1.7.3"),
    ("com.example.agent", "AIO Device Agent", "2.3.1"),
    ("com.microsoft.teams", "Teams", "1416/1.0.0.2024093903"),
]


RESET = "\033[0m"
GREY = "\033[90m"
RED = "\033[91m"
GREEN = "\033[92m"
YELLOW = "\033[93m"
BLUE = "\033[94m"
CYAN = "\033[96m"
BOLD = "\033[1m"


print_lock = threading.Lock()
stats_lock = threading.Lock()
stats = {"ok": 0, "err": 0}
stop_event = threading.Event()


def load_env_file():
    env_path = os.path.join(os.path.dirname(__file__), ".env")
    values = {}
    try:
        with open(env_path, "r", encoding="utf-8") as handle:
            for raw_line in handle:
                line = raw_line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                key, _, value = line.partition("=")
                values[key.strip()] = value.strip().strip('"').strip("'")
    except FileNotFoundError:
        pass
    return values


def parse_args():
    env = load_env_file()
    default_url = os.environ.get("MDM_URL", env.get("MDM_URL", "http://localhost:8080"))
    default_key = os.environ.get("MDM_API_KEY", env.get("DEVICE_API_KEY", "changeme"))

    parser = argparse.ArgumentParser(description="Simulate dummy devices against the MDM server")
    parser.add_argument("--url", default=default_url, help="Base server URL")
    parser.add_argument("--api-key", default=default_key, help="Value for X-API-Key")
    parser.add_argument("--count", type=int, default=int(os.environ.get("DEVICE_COUNT", "20")), help="Number of devices")
    parser.add_argument("--interval", type=float, default=float(os.environ.get("POLL_INTERVAL", "30")), help="Seconds between polls")
    parser.add_argument("--timeout", type=float, default=10.0, help="HTTP timeout in seconds")
    parser.add_argument("--prefix", default="SIM", help="Serial prefix for generated devices")
    parser.add_argument("--start-index", type=int, default=1, help="Starting device index")
    parser.add_argument("--once", action="store_true", help="Send a single check-in per device and exit")
    parser.add_argument("--no-color", action="store_true", help="Disable ANSI colors")
    return parser.parse_args()


class Device:
    def __init__(self, serial):
        self.serial = serial
        self.build_id = random.choice(BUILD_IDS)
        self.battery = random.randint(35, 100)
        self.charging = random.choice([True, False])
        self.ip_address = self._make_ip()
        self.wifi_ssid = random.choice(WIFI_SSIDS)
        self.signal_strength = -1 * random.randint(42, 78)
        self.temperature_c = random.randint(24, 37)
        self.ram_total_mb = random.choice([2048, 3072, 4096, 6144, 8192])
        self.ram_used_mb = random.randint(int(self.ram_total_mb * 0.25), int(self.ram_total_mb * 0.78))
        self.storage_free_gb = round(random.uniform(8.0, 64.0), 1)
        self.installed_apps = self._make_apps()

    def _make_ip(self):
        return f"{random.choice(IP_PREFIXES)}.{random.randint(2, 254)}"

    def _make_apps(self):
        picked = random.sample(APP_CATALOG, k=random.randint(3, 6))
        return [
            {
                "package": package,
                "name": name,
                "version_name": version,
            }
            for package, name, version in picked
        ]

    def maybe_drift(self):
        if self.charging:
            self.battery = min(100, self.battery + random.randint(1, 4))
            if self.battery == 100 or random.random() < 0.10:
                self.charging = False
        else:
            self.battery = max(1, self.battery - random.randint(0, 3))
            if self.battery < 15 or random.random() < 0.05:
                self.charging = True

        if random.random() < 0.03:
            next_build = random.choice(BUILD_IDS)
            if next_build != self.build_id:
                self.build_id = next_build

        if random.random() < 0.06:
            self.ip_address = self._make_ip()
            self.wifi_ssid = random.choice(WIFI_SSIDS)
            self.signal_strength = -1 * random.randint(42, 78)

        if random.random() < 0.04:
            self.temperature_c = max(20, min(45, self.temperature_c + random.randint(-2, 3)))

        if random.random() < 0.10:
            drift = random.randint(-180, 220)
            floor = int(self.ram_total_mb * 0.18)
            ceiling = int(self.ram_total_mb * 0.92)
            self.ram_used_mb = max(floor, min(ceiling, self.ram_used_mb + drift))

        if random.random() < 0.02:
            self.storage_free_gb = round(max(1.0, self.storage_free_gb - random.uniform(0.2, 1.0)), 1)

    def payload(self):
        ram_free_mb = max(0, self.ram_total_mb - self.ram_used_mb)
        return {
            "serial_number": self.serial,
            "build_id": self.build_id,
            "battery_pct": self.battery,
            "extra": {
                "ip_address": self.ip_address,
                "wifi_ssid": self.wifi_ssid,
                "wifi": self.wifi_ssid,
                "signal_strength": self.signal_strength,
                "battery_temp_c": self.temperature_c,
                "temperature_c": self.temperature_c,
                "ram_usage_mb": {
                    "total": self.ram_total_mb,
                    "used": self.ram_used_mb,
                    "free": ram_free_mb,
                },
                "ram_total_mb": self.ram_total_mb,
                "ram_used_mb": self.ram_used_mb,
                "ram_free_mb": ram_free_mb,
                "storage_free_gb": self.storage_free_gb,
                "charging": self.charging,
                "simulated": True,
            },
            "installed_apps": self.installed_apps,
        }


def now():
    return datetime.now().strftime("%H:%M:%S")


def color(enabled, value):
    return value if enabled else ""


def battery_color(enabled, pct):
    if not enabled:
        return ""
    if pct < 20:
        return RED
    if pct < 50:
        return YELLOW
    return GREEN


def log(args, serial, message, tone=""):
    with print_lock:
        print(
            f"{color(not args.no_color, GREY)}{now()}{color(not args.no_color, RESET)}  "
            f"{color(not args.no_color, tone)}{serial:<14}{color(not args.no_color, RESET)}  "
            f"{message}"
        )


def send_checkin(args, device):
    payload = json.dumps(device.payload()).encode("utf-8")
    request = urllib.request.Request(
        f"{args.url.rstrip('/')}/api/v1/checkin",
        data=payload,
        headers={
            "Content-Type": "application/json",
            "X-API-Key": args.api_key,
        },
        method="POST",
    )

    with urllib.request.urlopen(request, timeout=args.timeout) as response:
        body = response.read().decode("utf-8")
        data = json.loads(body) if body else {}
        return response.status, data


def worker(args, device):
    if not args.once:
        time.sleep(random.uniform(0, max(args.interval, 1.0)))

    while not stop_event.is_set():
        if random.random() < 0.04 and not args.once:
            log(args, device.serial, "skipped poll (simulated offline)", GREY)
            if stop_event.wait(args.interval + random.uniform(0, 5)):
                return
            continue

        device.maybe_drift()

        try:
            status, response = send_checkin(args, device)
            commands = response.get("commands") or []
            config = response.get("config") or {}
            bc = battery_color(not args.no_color, device.battery)
            charging_flag = "charging" if device.charging else "battery"
            summary = (
                f"HTTP {status}  "
                f"bat={bc}{device.battery:3d}%{color(not args.no_color, RESET)} {charging_flag}  "
                f"temp={device.temperature_c}C  "
                f"ram={device.ram_used_mb}/{device.ram_total_mb}MB  "
                f"wifi={device.wifi_ssid}  apps={len(device.installed_apps)}"
            )
            if commands:
                summary += f"  commands={len(commands)}"
            if config:
                summary += f"  kiosk={config.get('kiosk_enabled', False)}"
            log(args, device.serial, summary, CYAN)
            with stats_lock:
                stats["ok"] += 1
        except urllib.error.HTTPError as exc:
            body = exc.read().decode("utf-8", errors="replace")
            log(args, device.serial, f"HTTP {exc.code} {exc.reason}  {body}", RED)
            with stats_lock:
                stats["err"] += 1
        except Exception as exc:
            log(args, device.serial, f"error: {exc}", RED)
            with stats_lock:
                stats["err"] += 1

        if args.once:
            return

        sleep_for = max(1.0, args.interval + random.uniform(-5, 5))
        if stop_event.wait(sleep_for):
            return


def stats_printer(args):
    while not stop_event.wait(30):
        with stats_lock:
            ok = stats["ok"]
            err = stats["err"]
        total = ok + err
        rate = 0.0 if total == 0 else (ok / total) * 100.0
        with print_lock:
            print(
                f"{color(not args.no_color, BOLD)}"
                f"\n{'-' * 64}\n"
                f"requests={total}  ok={ok}  err={err}  success={rate:.1f}%\n"
                f"{'-' * 64}"
                f"{color(not args.no_color, RESET)}\n"
            )


def main():
    args = parse_args()

    if args.count < 1:
        raise SystemExit("--count must be at least 1")

    devices = [
        Device(f"{args.prefix}-{index:04d}")
        for index in range(args.start_index, args.start_index + args.count)
    ]

    def handle_signal(_signum, _frame):
        stop_event.set()

    signal.signal(signal.SIGINT, handle_signal)
    signal.signal(signal.SIGTERM, handle_signal)

    print(f"{color(not args.no_color, BOLD)}Dummy Device Simulator{color(not args.no_color, RESET)}")
    print(f"URL       : {args.url.rstrip('/')}")
    print(f"Devices   : {args.count}")
    print(f"Interval  : {args.interval}s")
    print(f"Once      : {args.once}")
    print(f"API key   : {args.api_key[:8]}{'*' * max(0, len(args.api_key) - 8)}")
    print("")

    if not args.once:
        threading.Thread(target=stats_printer, args=(args,), daemon=True).start()

    threads = []
    for device in devices:
        thread = threading.Thread(target=worker, args=(args, device), daemon=True)
        thread.start()
        threads.append(thread)

    for thread in threads:
        thread.join()

    with stats_lock:
        ok = stats["ok"]
        err = stats["err"]
    print(f"\nFinished. ok={ok} err={err}")


if __name__ == "__main__":
    main()
