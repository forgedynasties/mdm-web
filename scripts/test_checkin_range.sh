#!/usr/bin/env bash
# Simulate checkins for serial range AT070AABU00001 → AT070AABU00900
# Usage: ./test_checkin_range.sh [BASE_URL] [API_KEY]

BASE_URL="${1:-http://localhost:8080}"
API_KEY="${2:-${DEVICE_API_KEY:-changeme}}"

PREFIX="AT070AABU"
START=1
END=900
BUILD_ID="AT07-BUILD-001"

ok=0
fail=0

for i in $(seq "$START" "$END"); do
  serial=$(printf "%s%05d" "$PREFIX" "$i")
  battery=$(( (RANDOM % 81) + 10 ))  # 10–90%

  http_code=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST "$BASE_URL/api/v1/checkin" \
    -H "Content-Type: application/json" \
    -H "X-API-Key: $API_KEY" \
    -d "{\"serial_number\":\"$serial\",\"build_id\":\"$BUILD_ID\",\"battery_pct\":$battery}")

  if [ "$http_code" = "200" ]; then
    ok=$((ok + 1))
    echo "OK  $serial  battery=${battery}%"
  else
    fail=$((fail + 1))
    echo "ERR $serial  http=$http_code"
  fi
done

echo ""
echo "Done: $ok ok, $fail failed (total $((ok + fail)))"
