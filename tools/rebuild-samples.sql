-- Rebuild device_samples online and give its free space back to the disk.
--
-- Why: downsampling (older than 60 days → one sample per 5 min) and deleting the corrupted
-- "androidboot.baseband=msm" device left the table about half empty — on 24 Sep 2026
-- pgstattuple_approx put the heap at 1374 MB with 665 MB live, and the primary key at
-- 1098 MB. Postgres reuses that space but never returns it. Same method as
-- rebuild-checkins.sql: copy the live rows into a fresh table day by day, build its
-- indexes, swap it in under a lock that lasts seconds, check the recent days, drop the
-- old one. No VACUUM FULL: that locks the table — and every check-in's sample insert —
-- for the whole rewrite.
--
-- How to run (on the host; devices keep reporting throughout):
--   docker exec -i mdm-postgres-1 psql -v ON_ERROR_STOP=1 -X -U mdm -d mdm \
--     < tools/rebuild-samples.sql 2>&1 | tee /home/ubuntu/backups/rebuild-samples.log
-- Resumable before the swap (re-run continues from the last copied day). After the swap,
-- do not re-run: check the counts and DROP TABLE device_samples_old.

\timing on
SET max_parallel_workers_per_gather = 0;
\echo === 0. sizes before
SELECT pg_size_pretty(pg_total_relation_size('device_samples')) AS samples_total,
       pg_size_pretty(pg_database_size(current_database())) AS db_total;

\echo === 1. target table
CREATE TABLE IF NOT EXISTS device_samples_new (LIKE device_samples INCLUDING DEFAULTS);

\echo === 2. copy history day by day (each day its own transaction)
CREATE OR REPLACE PROCEDURE _rebuild_samples_copy()
LANGUAGE plpgsql AS $$
DECLARE
  d      date;
  d_end  date := (now() AT TIME ZONE 'UTC')::date;   -- today's rows are copied in the swap
  n      bigint;
BEGIN
  SELECT (MAX(at) AT TIME ZONE 'UTC')::date INTO d FROM device_samples_new;
  IF d IS NOT NULL THEN
    DELETE FROM device_samples_new WHERE at >= d::timestamptz;   -- redo a partial day
    RAISE NOTICE 'resuming from %', d;
  ELSE
    SELECT (MIN(at) AT TIME ZONE 'UTC')::date INTO d FROM device_samples;
  END IF;
  IF d IS NULL THEN RETURN; END IF;
  COMMIT;
  WHILE d < d_end LOOP
    INSERT INTO device_samples_new SELECT * FROM device_samples
    WHERE at >= d::timestamptz AND at < (d + 1)::timestamptz;
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE '% copied % rows', d, n;
    COMMIT;
    d := d + 1;
  END LOOP;
END $$;
CALL _rebuild_samples_copy();
DROP PROCEDURE _rebuild_samples_copy();

\echo === 3. indexes and foreign key on the new table (built while the old one serves traffic)
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'device_samples_new_pkey') THEN
    ALTER TABLE device_samples_new ADD CONSTRAINT device_samples_new_pkey PRIMARY KEY (device_id, at);
  END IF;
END $$;
CREATE INDEX IF NOT EXISTS device_samples_new_at ON device_samples_new USING BRIN (at);
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'device_samples_new_device_id_fkey') THEN
    ALTER TABLE device_samples_new ADD CONSTRAINT device_samples_new_device_id_fkey
      FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE;
  END IF;
END $$;
ANALYZE device_samples_new;

\echo === 4. swap: copy what arrived after the copied days, rename (lock held for seconds)
-- The cut-off is fixed here as a literal so the INSERT below is an index range scan; as a
-- subquery it planned a sequential scan of the whole old table under the lock (the
-- checkins rebuild, 24 Sep).
SELECT COALESCE(max(at), '-infinity') AS cutoff FROM device_samples_new \gset
\echo copying rows newer than :cutoff
BEGIN;
LOCK TABLE device_samples IN ACCESS EXCLUSIVE MODE;
INSERT INTO device_samples_new SELECT * FROM device_samples WHERE at > :'cutoff';
ALTER TABLE device_samples RENAME CONSTRAINT device_samples_pkey TO device_samples_old_pkey;
ALTER INDEX idx_device_samples_at RENAME TO device_samples_old_at;
ALTER TABLE device_samples RENAME CONSTRAINT device_samples_device_id_fkey TO device_samples_old_device_id_fkey;
ALTER TABLE device_samples RENAME TO device_samples_old;
ALTER TABLE device_samples_new RENAME TO device_samples;
ALTER TABLE device_samples RENAME CONSTRAINT device_samples_new_pkey TO device_samples_pkey;
ALTER INDEX device_samples_new_at RENAME TO idx_device_samples_at;
ALTER TABLE device_samples RENAME CONSTRAINT device_samples_new_device_id_fkey TO device_samples_device_id_fkey;
COMMIT;

\echo === 5. check the recent days, then drop the old table
DO $$
DECLARE d date; o bigint; n bigint;
BEGIN
  FOR d IN SELECT g::date FROM generate_series((now() AT TIME ZONE 'UTC')::date - 3, (now() AT TIME ZONE 'UTC')::date, interval '1 day') g LOOP
    SELECT count(*) INTO o FROM device_samples_old WHERE at >= d::timestamptz AND at < (d + 1)::timestamptz;
    SELECT count(*) INTO n FROM device_samples     WHERE at >= d::timestamptz AND at < (d + 1)::timestamptz;
    RAISE NOTICE '% old_rows=% new_rows=%', d, o, n;
    IF n < o THEN
      RAISE EXCEPTION 'day % has fewer rows in the new table — device_samples_old kept, investigate', d;
    END IF;
  END LOOP;
  IF (SELECT min(at) FROM device_samples) IS DISTINCT FROM (SELECT min(at) FROM device_samples_old) THEN
    RAISE EXCEPTION 'oldest sample differs between the tables — device_samples_old kept';
  END IF;
END $$;
DROP TABLE device_samples_old;

\echo === 6. sizes after
SELECT pg_size_pretty(pg_total_relation_size('device_samples')) AS samples_total,
       pg_size_pretty(pg_database_size(current_database())) AS db_total;
\echo Done.
