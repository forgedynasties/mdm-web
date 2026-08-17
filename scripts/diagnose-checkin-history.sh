#!/usr/bin/env bash
#
# diagnose-checkin-history.sh — figure out why device graphs are empty for older
# builds. Run it ON the server host (it talks to the postgres container). It answers:
# do pre-<build> check-in rows exist at all, and do they actually carry the telemetry
# the graphs plot (battery_pct + extra.battery_temp_c)?
#
# The graph reads the `checkins` table filtered by device_id + time range only (never
# by build_id), and the only server-side deletion of checkins is the time-based
# retention prune (CheckinRetentionDays > 0) — there is NO delete on build change.
# So a build upgrade cannot erase history in code; this script shows whether the data
# was pruned by time or simply never sent by the older client.
#
# Usage: bash scripts/diagnose-checkin-history.sh
set -euo pipefail

# Find the mdm postgres container (fall back to any postgres container).
CID=$(docker ps --format '{{.Names}}' | grep -iE 'mdm.*postgres|postgres.*mdm' | head -1)
[ -z "$CID" ] && CID=$(docker ps --format '{{.Names}}' | grep -i postgres | head -1)
if [ -z "$CID" ]; then echo "No postgres container found (docker ps | grep postgres)"; exit 1; fi
echo "Using container: $CID"

psql() { docker exec -i "$CID" psql -U mdm -d mdm -v ON_ERROR_STOP=1 "$@"; }

echo
echo "== 1) Checkin rows + date range per build =="
psql -c "SELECT build_id, COUNT(*) AS rows, MIN(created_at) AS oldest, MAX(created_at) AS newest
         FROM checkins GROUP BY build_id ORDER BY oldest;"

echo
echo "== 2) Do old-build checkins carry battery % and temperature? =="
psql -c "SELECT build_id,
                COUNT(*) AS rows,
                COUNT(*) FILTER (WHERE battery_pct > 0)          AS with_battery,
                COUNT(*) FILTER (WHERE extra ? 'battery_temp_c') AS with_temp
         FROM checkins GROUP BY build_id ORDER BY build_id;"

echo
echo "== 3) A device that actually upgraded (>=2 builds), history split by build =="
psql -c "WITH multi AS (
           SELECT device_id FROM checkins
           GROUP BY device_id HAVING COUNT(DISTINCT build_id) > 1
           LIMIT 1)
         SELECT d.serial_number, c.build_id, COUNT(*) AS rows,
                MIN(c.created_at) AS oldest, MAX(c.created_at) AS newest
         FROM checkins c
         JOIN devices d ON d.id = c.device_id
         WHERE c.device_id IN (SELECT device_id FROM multi)
         GROUP BY d.serial_number, c.build_id
         ORDER BY oldest;"

echo
echo "== 4) Overall span + row count =="
psql -c "SELECT COUNT(*) AS total_checkins, MIN(created_at) AS oldest, MAX(created_at) AS newest FROM checkins;"

echo
echo "Interpreting the result:"
echo "  * Section 2 shows old builds with_battery/with_temp ~ 0  -> the older client never"
echo "    sent that telemetry; nothing to recover (2.0.6l+ reports it going forward)."
echo "  * Old rows simply absent while retention is enabled       -> time-based prune;"
echo "    raise/disable it in Settings -> Data lifecycle (checkin retention)."
echo "  * Old rows present WITH battery/temp but graph still empty -> display/query bug."
