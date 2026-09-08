#!/usr/bin/env bash
# Enroll Android devices over adb — for kiosks without a camera and for volume.
# Every device in `adb devices` gets: agent installed → set as Device Owner →
# handed the server URL + enrollment token (no typing on the device). The agent
# exchanges the token for its own key and the device shows up on the dashboard.
#
#   tools/enroll-adb.sh -s https://mdm.dev.aioapp.com -t enr_XXXX            # every connected device
#   tools/enroll-adb.sh -s https://mdm.dev.aioapp.com -t enr_XXXX -d SERIAL  # one device
#   tools/enroll-adb.sh ... --apk path/to/skorra-agent.apk                    # else downloaded from the server
#
# Devices must be factory-fresh with NO account added (Device Owner can't be set
# otherwise) and have USB debugging on. Use a profile with a max-devices limit
# for a batch; revoke it afterwards.
set -euo pipefail
SERVER=""; TOKEN=""; APK=""; ONLY=""
while [ $# -gt 0 ]; do
  case "$1" in
    -s|--server) SERVER="${2%/}"; shift 2 ;;
    -t|--token) TOKEN="$2"; shift 2 ;;
    -d|--device) ONLY="$2"; shift 2 ;;
    --apk) APK="$2"; shift 2 ;;
    -h|--help) sed -n 2,14p "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
[ -n "$SERVER" ] && [ -n "$TOKEN" ] || { echo "need -s SERVER and -t TOKEN (see -h)" >&2; exit 2; }
command -v adb >/dev/null || { echo "adb not found in PATH" >&2; exit 2; }
COMPONENT="com.skorra.agent/com.skorra.agent.MdmDeviceAdminReceiver"

if [ -z "$APK" ]; then
  APK="$(mktemp -t skorra-agent.XXXXXX.apk)"
  echo "→ downloading agent from $SERVER/agent/skorra-agent.apk"
  curl -fsSL -o "$APK" "$SERVER/agent/skorra-agent.apk" || { echo "download failed — is an agent APK hosted in Settings › App library?" >&2; exit 1; }
fi

if [ -n "$ONLY" ]; then DEVICES="$ONLY"; else DEVICES="$(adb devices | awk 'NR>1 && $2=="device"{print $1}')"; fi
[ -n "$DEVICES" ] || { echo "no devices in 'adb devices' (USB debugging on? authorized?)" >&2; exit 1; }

ok=0; fail=0
for D in $DEVICES; do
  echo "== $D"
  A="adb -s $D"
  accounts=$($A shell dumpsys account 2>/dev/null | grep -c 'Account {' || true)
  if [ "${accounts:-0}" -gt 0 ]; then echo "   ✗ $accounts account(s) on the device — factory reset and do not add an account"; fail=$((fail+1)); continue; fi
  if $A shell dumpsys device_policy 2>/dev/null | grep -q 'Device Owner:'; then
    echo "   · already has a Device Owner, installing/updating agent only"
  fi
  if ! $A install -r "$APK" >/dev/null 2>&1; then echo "   ✗ install failed"; fail=$((fail+1)); continue; fi
  if ! $A shell dumpsys device_policy 2>/dev/null | grep -q 'Device Owner:'; then
    out=$($A shell dpm set-device-owner "$COMPONENT" 2>&1 || true)
    case "$out" in *Success*) echo "   ✓ device owner set" ;; *) echo "   ✗ set-device-owner: $out"; fail=$((fail+1)); continue ;; esac
  fi
  $A shell am start -W -n com.skorra.agent/.ui.MainActivity --es server_url "$SERVER" --es enroll_token "$TOKEN" >/dev/null 2>&1 || true
  # the agent enrolls in the background; give it a moment and read back its serial
  serial=$($A shell getprop ro.serialno 2>/dev/null | tr -d '\r')
  echo "   ✓ handed server + token to the agent (serial $serial) — watch Fleet › Enroll"
  ok=$((ok+1))
done
echo "done: $ok enrolled, $fail failed"
