#!/usr/bin/env bash
# Simulates a fleet of Android devices checking in to the local MDM server.
# Usage:
#   ./simulate.sh              # run once (one checkin per device)
#   ./simulate.sh --loop       # keep looping every 10s until Ctrl+C
#   ./simulate.sh --loop 30    # loop every 30s

set -euo pipefail

HOST="${MDM_HOST:-http://localhost:8080}"
ENDPOINT="$HOST/api/v1/checkin"
API_KEY="${DEVICE_API_KEY:-your-secret-key-here}"

LOOP=false
INTERVAL=10
if [[ "${1:-}" == "--loop" ]]; then
  LOOP=true
  INTERVAL="${2:-10}"
fi

# ── Device definitions ────────────────────────────────────────────────────────
# Each entry: "SERIAL|BUILD_ID|BATTERY_START|BATTERY_DRIFT"
# BATTERY_DRIFT: how many % points battery changes per cycle (negative = draining)
DEVICES=(
  "SIM-DEVICE-001|QP1A.190711.020|85|-2"
  "SIM-DEVICE-002|TP1A.220624.014|42|-1"
  "SIM-DEVICE-003|SP2A.220505.002|15|-1"
  "SIM-DEVICE-004|RQ3A.210805.001|97|-3"
  "SIM-DEVICE-005|QQ3A.200805.001|63|-2"
)

# State file to persist battery levels across loop iterations
STATE_FILE="/tmp/mdm_simulate_state"

# ── Init state ────────────────────────────────────────────────────────────────
init_state() {
  rm -f "$STATE_FILE"
  for entry in "${DEVICES[@]}"; do
    IFS='|' read -r serial build start drift <<< "$entry"
    echo "$serial=$start" >> "$STATE_FILE"
  done
}

get_battery() {
  local serial="$1"
  grep "^$serial=" "$STATE_FILE" 2>/dev/null | cut -d= -f2
}

set_battery() {
  local serial="$1"
  local val="$2"
  # clamp 5-100
  (( val < 5  )) && val=5
  (( val > 100 )) && val=100
  sed -i "s/^$serial=.*/$serial=$val/" "$STATE_FILE"
}

# ── Checkin ───────────────────────────────────────────────────────────────────
checkin() {
  local serial="$1"
  local build="$2"
  local battery="$3"

  # Build extra JSON with simulated sensor data
  local extra
  extra=$(printf '{"wifi_ssid":"Office-WiFi","signal_strength":-%d,"temperature_c":%d}' \
    $(( RANDOM % 40 + 40 )) \
    $(( RANDOM % 15 + 20 )))

  local payload
  payload=$(printf '{"serial_number":"%s","build_id":"%s","battery_pct":%d,"extra":%s}' \
    "$serial" "$build" "$battery" "$extra")

  local response
  response=$(curl -sf -X POST "$ENDPOINT" \
    -H "Content-Type: application/json" \
    -H "X-API-Key: $API_KEY" \
    -d "$payload" 2>&1) || { echo "  [!] curl failed for $serial"; return; }

  local status
  status=$(echo "$response" | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4)
  printf "  %-20s  battery=%3d%%  %s\n" "$serial" "$battery" "${status:-$response}"
}

# ── Main loop ─────────────────────────────────────────────────────────────────
[[ ! -f "$STATE_FILE" ]] && init_state

cycle=0
while true; do
  (( cycle++ )) || true
  echo "── Cycle $cycle  $(date '+%H:%M:%S') ────────────────────────────"

  for entry in "${DEVICES[@]}"; do
    IFS='|' read -r serial build start drift <<< "$entry"

    battery=$(get_battery "$serial")
    if [[ -z "$battery" ]]; then
      battery="$start"
      echo "$serial=$battery" >> "$STATE_FILE"
    fi

    checkin "$serial" "$build" "$battery"

    # Apply drift; if battery hits floor, reset to simulate a charge
    new_bat=$(( battery + drift ))
    if (( new_bat <= 5 )); then
      new_bat=$(( RANDOM % 30 + 70 ))  # charged back to 70-100%
    fi
    set_battery "$serial" "$new_bat"
  done

  echo ""
  $LOOP || break
  sleep "$INTERVAL"
done
