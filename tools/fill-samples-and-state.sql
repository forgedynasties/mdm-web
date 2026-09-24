-- Run on live 24 Sep 2026: filled 1,493,160 samples and wrote 2,339,846 state events.
-- Needs samples_fill loaded first — see the header of this file.
-- One sitting, 24 Sep 2026: put back what the dup-strip took, and the pre-17-Sep state history.
-- Expects samples_fill already loaded (step 0 in the run command). Every step only fills
-- what is missing and never overwrites, so re-running is safe.
\timing on
\set ON_ERROR_STOP on
SET statement_timeout = 0;
SET max_parallel_workers_per_gather = 0;

\echo === 0. before
SELECT count(*) AS samples_before_cutoff,
       count(*) FILTER (WHERE temp_c IS NULL)      AS missing_temp,
       count(*) FILTER (WHERE ram_used_mb IS NULL) AS missing_ram
FROM device_samples WHERE at < '2026-09-17T14:08:16.26779Z';

\echo === 1. fill samples from the local copy, one day per transaction
CREATE INDEX IF NOT EXISTS samples_fill_key ON samples_fill (device_id, at);
ANALYZE samples_fill;
CREATE OR REPLACE PROCEDURE _fill() LANGUAGE plpgsql AS $$
DECLARE d date; d_end date; n bigint; tot bigint := 0;
BEGIN
  SELECT min(at)::date, max(at)::date INTO d, d_end FROM samples_fill;
  WHILE d <= d_end LOOP
    UPDATE device_samples s SET
      temp_c          = COALESCE(s.temp_c,          f.temp_c),
      ram_used_mb     = COALESCE(s.ram_used_mb,     f.ram_used_mb),
      ram_total_mb    = COALESCE(s.ram_total_mb,    f.ram_total_mb),
      wifi_rssi       = COALESCE(s.wifi_rssi,       f.wifi_rssi),
      storage_free_gb = COALESCE(s.storage_free_gb, f.storage_free_gb),
      cpu_temp_c      = COALESCE(s.cpu_temp_c,      f.cpu_temp_c)
    FROM samples_fill f
    WHERE f.device_id = s.device_id AND f.at = s.at
      AND f.at >= d AND f.at < d + 1 AND s.at >= d AND s.at < d + 1
      AND (   (s.temp_c          IS NULL AND f.temp_c          IS NOT NULL)
           OR (s.ram_used_mb     IS NULL AND f.ram_used_mb     IS NOT NULL)
           OR (s.ram_total_mb    IS NULL AND f.ram_total_mb    IS NOT NULL)
           OR (s.wifi_rssi       IS NULL AND f.wifi_rssi       IS NOT NULL)
           OR (s.storage_free_gb IS NULL AND f.storage_free_gb IS NOT NULL)
           OR (s.cpu_temp_c      IS NULL AND f.cpu_temp_c      IS NOT NULL));
    GET DIAGNOSTICS n = ROW_COUNT;
    tot := tot + n;
    IF n > 0 THEN RAISE NOTICE '% filled %', d, n; END IF;
    COMMIT;
    d := d + 1;
  END LOOP;
  RAISE NOTICE 'filled % sample(s) in total', tot;
END $$;
CALL _fill();
DROP PROCEDURE _fill();

\echo === 2. state history from the old check-ins, oldest day first, up to the live start
CREATE OR REPLACE PROCEDURE _state(until timestamptz, keys text[]) LANGUAGE plpgsql AS $$
DECLARE d date; n bigint; tot bigint := 0;
BEGIN
  SELECT min(created_at)::date INTO d FROM checkins;
  WHILE d IS NOT NULL AND d::timestamptz < until LOOP
    WITH k(key) AS (SELECT unnest(keys)),
    r AS (
      SELECT c.device_id, c.created_at AS at, k.key,
             CASE jsonb_typeof(c.extra -> k.key)
                  WHEN 'string' THEN c.extra ->> k.key
                  WHEN 'null'   THEN 'null'
                  ELSE (c.extra -> k.key)::text END AS v
      FROM checkins c CROSS JOIN k
      WHERE c.created_at >= d::timestamptz AND c.created_at < LEAST((d + 1)::timestamptz, until)
        AND c.extra ? k.key
    ),
    w AS (SELECT r.*, LAG(v) OVER (PARTITION BY device_id, key ORDER BY at) AS pv FROM r),
    p AS (
      SELECT w.device_id, w.at, w.key, w.v,
             COALESCE(w.pv, (SELECT e.to_val FROM device_state_events e
                             WHERE e.device_id = w.device_id AND e.key = w.key AND e.at < w.at
                             ORDER BY e.at DESC LIMIT 1)) AS prev
      FROM w
    )
    INSERT INTO device_state_events (device_id, at, key, from_val, to_val)
    SELECT device_id, at, key, COALESCE(prev, ''), v FROM p
    WHERE prev IS DISTINCT FROM v
    ON CONFLICT DO NOTHING;
    GET DIAGNOSTICS n = ROW_COUNT;
    tot := tot + n;
    COMMIT;
    d := d + 1;
  END LOOP;
  RAISE NOTICE 'wrote % state event(s) in total', tot;
END $$;
CALL _state('2026-09-17T14:08:16.26779Z',
            ARRAY['charging','charger_type','wlc_status','wlc_charging','screen_on','kiosk_suspended',
                  'boot_id','boot_reason','ip_address','wifi','timezone','foreground_pkg',
                  'adb_enabled','screen_lock_set','storage_encrypted']);
DROP PROCEDURE _state(timestamptz, text[]);

\echo === 3. after
SELECT count(*) AS samples_before_cutoff,
       count(*) FILTER (WHERE temp_c IS NULL)      AS missing_temp,
       count(*) FILTER (WHERE ram_used_mb IS NULL) AS missing_ram
FROM device_samples WHERE at < '2026-09-17T14:08:16.26779Z';
SELECT count(*) AS state_events_before_cutoff FROM device_state_events WHERE at < '2026-09-17T14:08:16.26779Z';
DROP TABLE samples_fill;
\echo Done.
