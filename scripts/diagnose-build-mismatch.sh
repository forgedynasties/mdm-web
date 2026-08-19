#!/usr/bin/env bash
#
# diagnose-build-mismatch.sh — why release 4428 / deployment 17 shows devices at
# "v2.0.6.tmp" yet the release counts 0 devices and every device is stuck at
# "reboot_sent" (never "installed").
#
# The OTA flow confirms an install by EXACT string equality between the device's
# reported ro.build.id and the package's target_build_id (api/handlers.go:686,1152
# and db CompleteUpdatesAtTargetBuild: pk.target_build_id = $buildid AND
# pk.status='active'). The releases page counts devices with dev.build_id = r.version.
# A single-character difference (a trailing space/newline, a ".tmp" suffix on one
# side only, wrong package status) makes all of those miss. This prints the exact
# stored strings so we can see which side differs.
#
# Read-only. Usage: bash scripts/diagnose-build-mismatch.sh [RELEASE_ID] [DEPLOYMENT_ID]
set -euo pipefail

REL="${1:-4428}"
DID="${2:-17}"

# Find the mdm postgres container and auto-detect its superuser/db (the role/db
# name vary by how the stack was initialised).
CID=$(docker ps --format '{{.Names}}' | grep -iE 'mdm.*postgres|postgres.*mdm' | head -1)
[ -z "$CID" ] && CID=$(docker ps --format '{{.Names}}' | grep -i postgres | head -1)
if [ -z "$CID" ]; then echo "No postgres container found"; exit 1; fi
PGUSER=$(docker exec "$CID" printenv POSTGRES_USER 2>/dev/null || echo postgres)
PGDB=$(docker exec "$CID" printenv POSTGRES_DB 2>/dev/null || echo "$PGUSER")
echo "Container: $CID  (user=$PGUSER db=$PGDB)  release=$REL deployment=$DID"
psql() { docker exec -i "$CID" psql -U "$PGUSER" -d "$PGDB" -v ON_ERROR_STOP=1 "$@"; }

echo
echo "== 1) Release row (note version, exact length) =="
psql -c "SELECT id, version, char_length(version) AS ver_len, product, status
         FROM releases WHERE id = $REL;"

echo
echo "== 2) Packages under this release (target_build_id EXACTLY, + status) =="
psql -c "SELECT id, type, status,
                target_build_id, char_length(target_build_id) AS tgt_len,
                source_build_id
         FROM ota_packages WHERE release_id = $REL ORDER BY id;"

echo
echo "== 3) What the deployment's devices actually report as build_id =="
psql -c "SELECT d.build_id, char_length(d.build_id) AS len, COUNT(*) AS devices,
                COUNT(*) FILTER (WHERE ud.status='installed')   AS installed,
                COUNT(*) FILTER (WHERE ud.status='reboot_sent') AS reboot_sent
         FROM update_devices ud
         JOIN devices d ON d.id = ud.device_id
         WHERE ud.update_id = $DID
         GROUP BY d.build_id ORDER BY devices DESC;"

echo
echo "== 4) Does the package target EXACTLY equal what devices report? =="
psql -c "SELECT p.target_build_id AS package_target,
                d.build_id        AS device_build,
                (p.target_build_id = d.build_id) AS exact_match,
                p.status          AS package_status
         FROM ota_packages p
         CROSS JOIN LATERAL (
           SELECT DISTINCT dv.build_id
           FROM update_devices ud JOIN devices dv ON dv.id = ud.device_id
           WHERE ud.update_id = $DID) d
         WHERE p.release_id = $REL;"

echo
echo "Read this:"
echo "  * exact_match=false  -> that's the bug. Compare the two strings + lengths above."
echo "      - lengths differ  -> trailing whitespace/newline on one side."
echo "      - one has '.tmp'   -> ROM shipped an unfinalised build id; target doesn't match."
echo "  * package_status not 'active' -> CompleteUpdatesAtTargetBuild's EXISTS also fails."
echo "  Fix options are in diagnose-build-mismatch.sh comments / ask before mutating."
