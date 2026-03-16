#!/usr/bin/env bash
# Backfills 24 hours of battery history directly into the DB.
# Inserts one checkin per device every 10 minutes for the past 24 hours.
# Usage:
#   ./backfill.sh
#   INTERVAL=5 ./backfill.sh   # checkin every 5 minutes instead of 10

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
DB_HOST="${DB_HOST:-localhost}"
DB_PORT="${DB_PORT:-5432}"
DB_USER="${DB_USER:-mdm}"
DB_PASSWORD="${DB_PASSWORD:-mdm}"
DB_NAME="${DB_NAME:-mdm}"
INTERVAL="${INTERVAL:-10}"  # minutes between checkins

PSQL="docker compose exec -e PGPASSWORD=$DB_PASSWORD postgres psql -U $DB_USER -d $DB_NAME -q"

# ── Device definitions ────────────────────────────────────────────────────────
# SERIAL | BUILD_ID | BATTERY_START (at 24h ago) | DRIFT_PER_HOUR (negative=drain)
DEVICES=(
  "SIM-DEVICE-001|QP1A.190711.020|95|-3"
  "SIM-DEVICE-002|TP1A.220624.014|80|-2"
  "SIM-DEVICE-003|SP2A.220505.002|60|-4"
  "SIM-DEVICE-004|RQ3A.210805.001|100|-2"
  "SIM-DEVICE-005|QQ3A.200805.001|45|-1"
)

STEPS=$(( 24 * 60 / INTERVAL ))
echo "Inserting $STEPS checkins per device ($INTERVAL-min interval) over the past 24h..."
echo ""

for entry in "${DEVICES[@]}"; do
  IFS='|' read -r serial build battery_start drift_per_hour <<< "$entry"

  echo "→ $serial  (start=${battery_start}%, drift=${drift_per_hour}%/h)"

  # Build one big SQL transaction per device
  $PSQL <<SQL
BEGIN;

-- Upsert device
INSERT INTO devices (serial_number, build_id, last_seen_at)
VALUES ('$serial', '$build', NOW())
ON CONFLICT (serial_number) DO UPDATE
  SET build_id     = EXCLUDED.build_id,
      last_seen_at = NOW();

-- Insert backdated checkins
DO \$\$
DECLARE
  v_device_id   UUID;
  v_battery     INT  := $battery_start;
  v_drift_step  NUMERIC := $drift_per_hour::NUMERIC * $INTERVAL / 60.0;
  v_steps       INT  := $STEPS;
  v_step        INT;
  v_ts          TIMESTAMPTZ;
  v_signal      INT;
  v_temp        INT;
BEGIN
  SELECT id INTO v_device_id FROM devices WHERE serial_number = '$serial';

  FOR v_step IN 0 .. (v_steps - 1) LOOP
    -- timestamp: 24h ago + step*interval minutes
    v_ts := NOW() - INTERVAL '$INTERVAL minutes' * (v_steps - v_step);

    -- clamp battery 5-100; reset to high when it drains too low
    IF v_battery <= 5 THEN
      v_battery := 70 + floor(random() * 30)::INT;
    END IF;

    v_signal := -(40 + floor(random() * 40)::INT);
    v_temp   := 20 + floor(random() * 15)::INT;

    INSERT INTO checkins (device_id, battery_pct, build_id, extra, created_at)
    VALUES (
      v_device_id,
      v_battery,
      '$build',
      json_build_object(
        'wifi_ssid',       'Office-WiFi',
        'signal_strength', v_signal,
        'temperature_c',   v_temp
      ),
      v_ts
    );

    -- apply drift and add small random jitter (+/-1)
    v_battery := v_battery + v_drift_step::INT + (floor(random() * 3) - 1)::INT;
  END LOOP;
END;
\$\$;

COMMIT;
SQL

  echo "   done"
done

echo ""
echo "Backfill complete."
