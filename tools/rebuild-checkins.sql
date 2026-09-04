-- Rebuild the checkins table in one pass, online.
--
-- Why: historical rows carry two jsonb keys (crash_events, wifi_scan) that were
-- ~80% of the table's bytes. UpsertCheckin no longer writes them; this copies the
-- history into a fresh table without them, swaps it in under a lock that lasts
-- seconds, and drops the old table so the disk space is returned immediately.
-- No VACUUM FULL, no long lock on device check-ins.
--
-- How to run (on the host, ~30–60 min on a small box; devices keep reporting):
--   cd /home/ubuntu/mdm
--   docker compose exec -T postgres sh -c 'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
--     < tools/rebuild-checkins.sql 2>&1 | tee /home/ubuntu/backups/rebuild-checkins.log
--
-- Before: a verified backup, and Settings → Data → Maintenance mode ON.
-- After:  Settings → Data → "Mark complete" on the legacy cleanup line, then
--         Maintenance mode OFF. Watch `df -h /` drop.
--
-- Resumable: if it fails before the final swap (e.g. Postgres restarted), just run
-- it again — it keeps checkins_new and continues from the last fully copied day.
-- If it failed AFTER the swap, do not re-run — the table is already rebuilt; just
-- DROP TABLE IF EXISTS checkins_old.
--
-- Memory: run with the Postgres container allowed at least 1.5 GB. At 1 GB the OOM
-- killer took a backend mid-copy on a 60 GB table (device traffic + copy + cache).
-- Also stop the hourly legacy-strip job first (Settings → Data → Mark complete);
-- it competes for the same memory and is redundant once this has run.

\timing on
\echo === 0. sizes before
SELECT pg_size_pretty(pg_total_relation_size('checkins')) AS checkins_total,
       (SELECT reltuples::bigint FROM pg_class WHERE relname = 'checkins') AS est_rows;

\echo === 1. target table (kept across runs so a failed copy resumes; indexes added after the copy)
CREATE TABLE IF NOT EXISTS checkins_new (LIKE checkins INCLUDING DEFAULTS);

\echo === 2. copy history day by day, stripping the two keys (each day its own transaction)
CREATE OR REPLACE PROCEDURE _rebuild_checkins_copy()
LANGUAGE plpgsql AS $$
DECLARE
  d      date;
  d_end  date := (now() AT TIME ZONE 'UTC')::date;   -- today (UTC); rows from today+ are copied in the swap step
  n      bigint;
BEGIN
  -- Resume: the newest day already in checkins_new may be partial (a crash mid-day
  -- rolled back only that day's transaction, but be safe) — redo it from scratch.
  SELECT (MAX(created_at) AT TIME ZONE 'UTC')::date INTO d FROM checkins_new;
  IF d IS NOT NULL THEN
    DELETE FROM checkins_new WHERE created_at >= d::timestamptz;
    RAISE NOTICE 'resuming from %', d;
  ELSE
    SELECT (MIN(created_at) AT TIME ZONE 'UTC')::date INTO d FROM checkins;
  END IF;
  IF d IS NULL THEN RETURN; END IF;
  COMMIT;
  WHILE d < d_end LOOP
    INSERT INTO checkins_new (id, device_id, battery_pct, build_id, extra, created_at)
    SELECT id, device_id, battery_pct, build_id,
           extra - 'crash_events' - 'wifi_scan', created_at
    FROM checkins
    WHERE created_at >= d::timestamptz AND created_at < (d + 1)::timestamptz;
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE '% copied % rows', d, n;
    COMMIT;
    d := d + 1;
  END LOOP;
END $$;
CALL _rebuild_checkins_copy();
DROP PROCEDURE _rebuild_checkins_copy();

\echo === 3. indexes on the new table (built while the old one still serves traffic)
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'checkins_new_pkey') THEN
    ALTER TABLE checkins_new ADD PRIMARY KEY (id);
  END IF;
END $$;
CREATE INDEX IF NOT EXISTS checkins_new_device_id          ON checkins_new (device_id);
CREATE INDEX IF NOT EXISTS checkins_new_device_created_at  ON checkins_new (device_id, created_at DESC);
CREATE INDEX IF NOT EXISTS checkins_new_created_at         ON checkins_new (created_at DESC);
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'checkins_new_device_id_fkey') THEN
    ALTER TABLE checkins_new
      ADD CONSTRAINT checkins_new_device_id_fkey FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE;
  END IF;
END $$;
ANALYZE checkins_new;

\echo === 4. swap: copy rows that arrived today meanwhile, rename (lock held for seconds)
BEGIN;
LOCK TABLE checkins IN ACCESS EXCLUSIVE MODE;
INSERT INTO checkins_new (id, device_id, battery_pct, build_id, extra, created_at)
SELECT c.id, c.device_id, c.battery_pct, c.build_id,
       c.extra - 'crash_events' - 'wifi_scan', c.created_at
FROM checkins c
WHERE c.created_at >= (now() AT TIME ZONE 'UTC')::date::timestamptz
  AND NOT EXISTS (SELECT 1 FROM checkins_new n WHERE n.id = c.id);
-- Free the names first: renaming a table does not rename its indexes/constraints.
ALTER INDEX checkins_pkey                     RENAME TO checkins_old_pkey;
ALTER INDEX idx_checkins_device_id            RENAME TO checkins_old_device_id;
ALTER INDEX idx_checkins_device_created_at    RENAME TO checkins_old_device_created_at;
ALTER INDEX idx_checkins_created_at           RENAME TO checkins_old_created_at;
ALTER TABLE checkins RENAME CONSTRAINT checkins_device_id_fkey TO checkins_old_device_id_fkey;
ALTER TABLE checkins RENAME TO checkins_old;
ALTER TABLE checkins_new RENAME TO checkins;
ALTER INDEX checkins_new_pkey                 RENAME TO checkins_pkey;
ALTER INDEX checkins_new_device_id            RENAME TO idx_checkins_device_id;
ALTER INDEX checkins_new_device_created_at    RENAME TO idx_checkins_device_created_at;
ALTER INDEX checkins_new_created_at           RENAME TO idx_checkins_created_at;
ALTER TABLE checkins RENAME CONSTRAINT checkins_new_device_id_fkey TO checkins_device_id_fkey;
COMMIT;

\echo === 5. sanity: row counts old vs new, then drop the old table
SELECT (SELECT count(*) FROM checkins_old) AS old_rows, (SELECT count(*) FROM checkins) AS new_rows;
DROP TABLE checkins_old;

\echo === 6. sizes after
SELECT pg_size_pretty(pg_total_relation_size('checkins')) AS checkins_total;
\echo Done. Now: Settings → Data → "Mark complete", then Maintenance mode OFF.
