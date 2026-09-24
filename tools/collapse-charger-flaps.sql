-- Run on live 24 Sep 2026: 2,540 flaps on 65 devices, 2,077,188 events replaced by 7,467.
-- Collapse past charger flaps in device_state_events into one "flapping" state (charging = 2),
-- the rule the server applies live since 24 Sep 2026 (internal/db/charger_flap.go): 6+ toggles
-- inside 60 s start a flap; it runs while toggles keep coming within 2 min. Inside a flap the
-- per-toggle charging and charger_type events are replaced by: old → 2 at the start, 2 → the
-- settled value just after the end, and one charger_type event if the type ended up different.
-- Flaps the live server already collapsed (a 2 nearby) are left alone. One transaction.
\timing on
\set ON_ERROR_STOP on
SET statement_timeout = 0;
BEGIN;

\echo === 0. before
SELECT key, count(*) FROM device_state_events WHERE key IN ('charging','charger_type') GROUP BY key ORDER BY key;

\echo === 1. find the flaps
CREATE TEMP TABLE ev ON COMMIT DROP AS
SELECT device_id, at, from_val, to_val,
       count(*) OVER (PARTITION BY device_id ORDER BY at
                      RANGE BETWEEN interval '60 seconds' PRECEDING AND CURRENT ROW) AS n60
FROM device_state_events
WHERE key = 'charging' AND from_val IN ('true','false') AND to_val IN ('true','false') AND from_val <> to_val;

CREATE TEMP TABLE bursts ON COMMIT DROP AS
WITH f AS (SELECT * FROM ev WHERE n60 >= 6),
g AS (
  SELECT f.*, sum(brk) OVER (PARTITION BY device_id ORDER BY at) AS b
  FROM (SELECT f.*, CASE WHEN lag(at) OVER w IS NULL OR at - lag(at) OVER w > interval '120 seconds'
                         THEN 1 ELSE 0 END AS brk
        FROM f WINDOW w AS (PARTITION BY device_id ORDER BY at)) f
)
SELECT device_id, b,
       min(at) AS s, max(at) AS e, count(*) AS toggles,
       (array_agg(from_val ORDER BY at))[1]    AS held_charging,
       (array_agg(to_val ORDER BY at DESC))[1] AS final_charging
FROM g GROUP BY device_id, b;

-- Leave anything the live server already collapsed.
DELETE FROM bursts b WHERE EXISTS (
  SELECT 1 FROM device_state_events x
  WHERE x.device_id = b.device_id AND x.key = 'charging' AND (x.to_val = '2' OR x.from_val = '2')
    AND x.at BETWEEN b.s - interval '5 minutes' AND b.e + interval '5 minutes');

ALTER TABLE bursts ADD COLUMN held_type text, ADD COLUMN final_type text;
UPDATE bursts b SET
  held_type  = (SELECT x.to_val FROM device_state_events x WHERE x.device_id = b.device_id
                AND x.key = 'charger_type' AND x.at < b.s ORDER BY x.at DESC LIMIT 1),
  final_type = (SELECT x.to_val FROM device_state_events x WHERE x.device_id = b.device_id
                AND x.key = 'charger_type' AND x.at <= b.e ORDER BY x.at DESC LIMIT 1);

SELECT count(*) AS flaps, count(DISTINCT device_id) AS devices, sum(toggles) AS toggles_in_flaps,
       min(s)::date AS first, max(e)::date AS last FROM bursts;

\echo === 2. replace the toggles inside each flap
DELETE FROM device_state_events d USING bursts b
WHERE d.device_id = b.device_id AND d.key IN ('charging','charger_type') AND d.at >= b.s AND d.at <= b.e;

INSERT INTO device_state_events (device_id, at, key, from_val, to_val)
SELECT device_id, s, 'charging', held_charging, '2' FROM bursts
UNION ALL
SELECT device_id, e + interval '1 millisecond', 'charging', '2', final_charging FROM bursts
UNION ALL
SELECT device_id, e + interval '1 millisecond', 'charger_type', COALESCE(held_type, ''), final_type
FROM bursts WHERE final_type IS NOT NULL AND final_type IS DISTINCT FROM held_type
ON CONFLICT DO NOTHING;

\echo === 3. after
SELECT key, count(*) FROM device_state_events WHERE key IN ('charging','charger_type') GROUP BY key ORDER BY key;
SELECT count(*) AS flap_periods FROM device_state_events WHERE key = 'charging' AND to_val = '2';
COMMIT;
\echo Done.
