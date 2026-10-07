#!/usr/bin/env bash
# Enroll Android devices over adb — for kiosks without a camera and for volume.
# Every device in `adb devices` gets: agent installed → set as Device Owner →
# handed the server URL + enrollment token (no typing on the device). The agent
# exchanges the token for its own key and the device shows up on the dashboard.
#
#   tools/enroll-adb.sh -s https://mdm.dev.aioapp.com -t enr_XXXX -k admin_key           # every connected device
#   tools/enroll-adb.sh -s https://mdm.dev.aioapp.com -t enr_XXXX -k admin_key -d SERIAL # one device
#   tools/enroll-adb.sh ... --apk path/to/aio-mdm-dpc.apk                                # else downloaded from the server
#
# Devices must be factory-fresh with NO account added (Device Owner can't be set
# otherwise) and have USB debugging on. Use a profile with a max-devices limit
# for a batch; revoke it afterwards.
#
# -k/--key (or $MDM_ADMIN_KEY) is optional but strongly recommended: without it, the
# script can only watch on-device logcat to tell whether enrollment landed, and that
# check is unreliable on several boards (confirmed on RockChip/Sunmi hardware: their
# `date` binary doesn't support the format this script needs, so `logcat -T` silently
# matches nothing and a successful enrollment is reported as failed). With the key, the
# script instead asks the server directly — the only confirmation that can't lie.
set -euo pipefail
SERVER=""; TOKEN=""; APK=""; ONLY=""; ADMIN_KEY="${MDM_ADMIN_KEY:-}"
while [ $# -gt 0 ]; do
  case "$1" in
    -s|--server) SERVER="${2%/}"; shift 2 ;;
    -t|--token) TOKEN="$2"; shift 2 ;;
    -d|--device) ONLY="$2"; shift 2 ;;
    -k|--key) ADMIN_KEY="$2"; shift 2 ;;
    --apk) APK="$2"; shift 2 ;;
    -h|--help) sed -n 2,22p "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
[ -n "$SERVER" ] && [ -n "$TOKEN" ] || { echo "need -s SERVER and -t TOKEN (see -h)" >&2; exit 2; }
command -v adb >/dev/null || { echo "adb not found in PATH" >&2; exit 2; }
if [ -z "$ADMIN_KEY" ]; then
  echo "⚠ no admin key (-k or \$MDM_ADMIN_KEY) — falling back to the unreliable on-device" >&2
  echo "  logcat check. Pass one so this script can verify with the server instead." >&2
fi
OUR_PKG="aio.app.mdmclient.dpc"
OUR_ADMIN="aio.app.mdmclient.dpc.MdmDeviceAdminReceiver"
COMPONENT="$OUR_PKG/$OUR_ADMIN"

if [ -z "$APK" ]; then
  APK="$(mktemp -t aio-mdm-dpc.XXXXXX.apk)"
  echo "→ downloading agent from $SERVER/agent/aio-mdm-dpc.apk"
  curl -fsSL -o "$APK" "$SERVER/agent/aio-mdm-dpc.apk" || { echo "download failed — is an agent APK hosted in Settings › App library?" >&2; exit 1; }
fi

if [ -n "$ONLY" ]; then DEVICES="$ONLY"; else DEVICES="$(adb devices | awk 'NR>1 && $2=="device"{print $1}')"; fi
[ -n "$DEVICES" ] || { echo "no devices in 'adb devices' (USB debugging on? authorized?)" >&2; exit 1; }

ok=0; fail=0
for D in $DEVICES; do
  echo "== $D"
  A="adb -s $D"
  accounts=$($A shell dumpsys account 2>/dev/null | grep -c 'Account {' || true)
  if [ "${accounts:-0}" -gt 0 ]; then echo "   ✗ $accounts account(s) on the device — factory reset and do not add an account"; fail=$((fail+1)); continue; fi
  # A Device Owner that isn't already ours blocks dpm set-device-owner outright — Android
  # only allows one, and the only way off is a factory reset (there is no adb-level
  # override). Checking this up front, and checking it's OUR component specifically and
  # not just "some Device Owner exists", avoids silently skipping set-device-owner and
  # leaving the freshly-installed APK unprivileged with the enroll extras doing nothing.
  owner_line=$($A shell dumpsys device_policy 2>/dev/null | grep "Device Owner:" -A2 || true)
  if echo "$owner_line" | grep -q "admin="; then
    if echo "$owner_line" | grep -q "$COMPONENT"; then
      echo "   · already Device Owner (ours) — installing/updating agent only"
      is_owner=1
    else
      owner_pkg=$(echo "$owner_line" | grep -o "package=[^ ]*" | head -1)
      echo "   ✗ a DIFFERENT Device Owner is set ($owner_pkg) — factory reset this device first, adb cannot replace it"
      fail=$((fail+1)); continue
    fi
  else
    is_owner=0
  fi
  if ! $A install -r "$APK" >/dev/null 2>&1; then echo "   ✗ install failed"; fail=$((fail+1)); continue; fi
  if [ "$is_owner" = "0" ]; then
    out=$($A shell dpm set-device-owner "$COMPONENT" 2>&1 || true)
    case "$out" in *Success*) echo "   ✓ device owner set" ;; *) echo "   ✗ set-device-owner: $out"; fail=$((fail+1)); continue ;; esac
  fi
  # Grants a Device Owner cannot give itself but adb can (no root): crash/ANR reports and
  # whole-device logcat (READ_LOGS + usage-stats appop), silent screen capture (PROJECT_MEDIA),
  # self-enabled remote input (WRITE_SECURE_SETTINGS). Best-effort: an older agent that does
  # not declare them just stays degraded.
  for p in android.permission.READ_LOGS android.permission.WRITE_SECURE_SETTINGS; do
    $A shell pm grant aio.app.mdmclient.dpc "$p" >/dev/null 2>&1 || echo "   · $p not granted (agent too old?)"
  done
  $A shell appops set aio.app.mdmclient.dpc GET_USAGE_STATS allow >/dev/null 2>&1 || true
  $A shell appops set aio.app.mdmclient.dpc PROJECT_MEDIA allow >/dev/null 2>&1 || true
  serial=$($A shell getprop ro.serialno 2>/dev/null | tr -d '\r')
  since=$($A shell date +'%m-%d %H:%M:%S.000' | tr -d '\r')
  $A shell am start -W -n aio.app.mdmclient.dpc/.ui.MainActivity --es server_url "$SERVER" --es enroll_token "$TOKEN" >/dev/null 2>&1 || true
  enrolled=""
  if [ -n "$ADMIN_KEY" ]; then
    # Ask the server directly — the agent's own serial (DeviceIdentity.serial(), almost
    # always ro.serialno but not guaranteed) is what it actually enrolled under, so a
    # mismatch here would be a false negative too; this is still the one check that can't
    # be fooled by an on-device quirk, which is the whole reason it beats the logcat path.
    for _ in $(seq 1 20); do
      status=$(curl -sS "$SERVER/api/v1/devices/$serial" -H "X-API-Key: $ADMIN_KEY" 2>/dev/null \
        | grep -o '"enrollment_status":"[^"]*"' | cut -d'"' -f4 || true)
      if [ "$status" = "enrolled" ]; then enrolled=1; break; fi
      sleep 1
    done
  else
    # Fallback only: unreliable on boards whose `date` binary doesn't support this format
    # (confirmed on RockChip/Sunmi hardware during the 2026-10-05 lab pass — logcat -T then
    # silently matches nothing and a successful enrollment reports as failed here). Pass
    # -k/--key to use the real check above instead.
    for _ in $(seq 1 20); do
      if $A logcat -d -T "$since" -s MdmService:I 2>/dev/null | grep "Enrolled" >/dev/null; then enrolled=1; break; fi
      sleep 1
    done
  fi
  if [ -n "$enrolled" ]; then
    echo "   ✓ enrolled (serial $serial)"
    ok=$((ok+1))
  elif [ -z "$ADMIN_KEY" ]; then
    echo "   ? agent did not confirm via logcat within 20s (serial $serial) — this check is known-unreliable without -k;"
    echo "     verify by hand: curl $SERVER/api/v1/devices/$serial -H \"X-API-Key: \$KEY\""
    fail=$((fail+1))
  else
    echo "   ✗ server has no record of $serial as enrolled after 20s — check the server URL is reachable from the device"
    fail=$((fail+1))
  fi
done
echo "done: $ok enrolled, $fail failed"
