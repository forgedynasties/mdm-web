#!/usr/bin/env bash
# Battle-tests install_apk de-duplication: the same APK cannot pile up on the same
# device. Drives the admin command API + the device ack endpoint through the full
# lifecycle (pending → installed/failed), which is deterministic and independent of
# the delivery channel (WebSocket vs legacy checkin).
#
# The server ALSO collapses duplicates at delivery time (GetPendingCommandsForDevice
# hands a device at most one install per APK); with legacy-checkin on you can watch a
# checkin return the install exactly once. This script asserts the creation guard,
# which is what stops the pile-up in the first place.
#
# Usage: ./scripts/test_install_dedup.sh [BASE_URL] [ADMIN_API_KEY] [DEVICE_API_KEY]
set -euo pipefail

BASE="${1:-http://localhost:8099}"
ADMIN_KEY="${2:-adminkey}"
DEVICE_KEY="${3:-devkey}"
SERIAL="DEDUP-TEST-$$"
APK="https://apps.example.com/pos-5.2.apk"
APK2="https://apps.example.com/other-1.0.apk"

pass() { echo "  ✓ $1"; }
fail() { echo "  ✗ $1"; echo "FAILED"; exit 1; }

# Create an install for $SERIAL; echoes the JSON response.
create() { # $1 = apk url
  curl -s -X POST "$BASE/api/v1/commands" -H "X-API-Key: $ADMIN_KEY" -H 'Content-Type: application/json' \
    --data "{\"type\":\"install_apk\",\"apk_url\":\"$1\",\"target_type\":\"devices\",\"targets\":[\"$SERIAL\"]}"
}
# Ack a command as the device (status: installed|failed|completed).
ack() { # $1 = command id, $2 = status
  curl -s -X POST "$BASE/api/v1/commands/$1/ack" -H "X-API-Key: $DEVICE_KEY" -H 'Content-Type: application/json' \
    --data "{\"serial_number\":\"$SERIAL\",\"status\":\"$2\"}" >/dev/null
}
cmd_id() { echo "$1" | grep -o '"id":"[0-9a-f-]\{36\}"' | head -1 | cut -d'"' -f4; }
is_skipped() { echo "$1" | grep -q '"created":false'; }

echo "==> Register device ($SERIAL)"
curl -s -o /dev/null -X POST "$BASE/api/v1/checkin" -H "X-API-Key: $DEVICE_KEY" -H 'Content-Type: application/json' \
  --data "{\"serial_number\":\"$SERIAL\",\"build_id\":\"v1\",\"battery_pct\":80}"

echo "==> 1) First install is created"
r1=$(create "$APK"); id1=$(cmd_id "$r1")
[ -n "$id1" ] && pass "created ($id1)" || fail "no command id in: $r1"

echo "==> 2) Re-sending the same APK while it's in flight is skipped"
r2=$(create "$APK")
is_skipped "$r2" && pass "second send skipped (created:false)" || fail "second send NOT skipped: $r2"

echo "==> 3) A different APK is unaffected"
r3=$(create "$APK2"); id3=$(cmd_id "$r3")
[ -n "$id3" ] && pass "different APK created ($id3)" || fail "different APK was skipped: $r3"
ack "$id3" installed

echo "==> 4) After it's installed, the same APK can be sent again (reinstall)"
ack "$id1" installed
r4=$(create "$APK"); id4=$(cmd_id "$r4")
[ -n "$id4" ] && ! is_skipped "$r4" && pass "reinstall allowed after 'installed' ($id4)" || fail "reinstall wrongly blocked: $r4"

echo "==> 5) That reinstall is now in flight — another send is skipped again"
r5=$(create "$APK")
is_skipped "$r5" && pass "in-flight reinstall blocks a further send" || fail "pile-up not prevented: $r5"

echo "==> 6) A failed install does NOT block a retry"
ack "$id4" failed
r6=$(create "$APK"); id6=$(cmd_id "$r6")
[ -n "$id6" ] && ! is_skipped "$r6" && pass "retry allowed after 'failed' ($id6)" || fail "retry wrongly blocked after failure: $r6"

echo
echo "ALL PASSED — install commands cannot pile up on the same device for the same app."
