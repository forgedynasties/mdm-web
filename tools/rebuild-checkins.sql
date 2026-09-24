-- Rebuild the checkins table in one pass, online, and give its free space back to the disk.
--
-- Why: downsampling (25.2M rows → 9.9M) and dropping the stored snapshot left the table
-- two-thirds empty — on 24 Sep 2026 pgstattuple_approx put it at 13 GB with 4.7 GB of
-- live rows and 8.3 GB free, plus ~1 GB of slack in its indexes. Postgres reuses that
-- space but never returns it. This copies the live rows into a fresh table, swaps it in
-- under a lock that lasts seconds, and drops the old one, so the space goes back to the
-- disk at once. No VACUUM FULL: that would lock checkins — and so every device check-in —
-- for the whole rewrite.
--
-- How to run (on the host, ~30–60 min; devices keep reporting throughout):
--   cd /home/ubuntu/mdm
--   docker compose exec -T postgres sh -c 'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
--     < tools/rebuild-checkins.sql 2>&1 | tee /home/ubuntu/backups/rebuild-checkins.log
--
-- Before: a verified backup. Maintenance mode only hides the dashboard from non-admins;
-- it does not stop check-ins, which is fine — the swap step picks up whatever arrived.
--
-- Resumable: if it fails before the swap (e.g. Postgres restarted), run it again — it
-- keeps checkins_new and continues from the last fully copied day. If it failed AFTER
-- the swap, do not re-run: the table is rebuilt; check counts and DROP TABLE checkins_old.
--
-- Run on live 24 Sep 2026: 14 GB → 5.4 GB (database 17 → 8.2 GB). Two lessons from that
-- run are built in below: the swap's cut-off is fixed before the lock is taken (as a
-- subquery the planner could not use the index, read all 13 GB, and held the lock ~2 min
-- while every check-in queued behind it), and the final check compares recent days
-- instead of counting both tables (19 GB of reads, just as the queued check-ins were
-- released — a Postgres backend died and the server restarted).
--
-- Schema as of 24 Sep 2026: no primary key (the uuid key was dropped as dead weight —
-- 756 MB, never scanned), two indexes, one foreign key, state_hash for coalescing.
-- Memory: needs the Postgres container at ≥1.5 GB (it has 2 GB).

\timing on
\echo === 0. sizes before
SELECT pg_size_pretty(pg_total_relation_size('checkins')) AS checkins_total,
       (SELECT reltuples::bigint FROM pg_class WHERE relname = 'checkins') AS est_rows;

\echo === 1. target table (kept across runs so a failed copy resumes; indexes added after the copy)
CREATE TABLE IF NOT EXISTS checkins_new (LIKE checkins INCLUDING DEFAULTS);

\echo === 2. copy history day by day (each day its own transaction)
CREATE OR REPLACE PROCEDURE _rebuild_checkins_copy()
LANGUAGE plpgsql AS $$
DECLARE
  d      date;
  d_end  date := (now() AT TIME ZONE 'UTC')::date;   -- today (UTC); today's rows are copied in the swap
  n      bigint;
BEGIN
  -- Resume: the newest day already copied may be partial — redo it from scratch.
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
    INSERT INTO checkins_new (id, device_id, battery_pct, build_id, extra, created_at, state_hash)
    SELECT id, device_id, battery_pct, build_id, extra, created_at, state_hash
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

\echo === 3. indexes and foreign key on the new table (built while the old one still serves traffic)
CREATE INDEX IF NOT EXISTS checkins_new_device_created_at ON checkins_new (device_id, created_at DESC);
CREATE INDEX IF NOT EXISTS checkins_new_created_at        ON checkins_new (created_at DESC);
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'checkins_new_device_id_fkey') THEN
    ALTER TABLE checkins_new
      ADD CONSTRAINT checkins_new_device_id_fkey FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE;
  END IF;
END $$;
ANALYZE checkins_new;

\echo === 4. swap: copy what arrived after the copied days, rename (lock held for seconds)
-- Everything newer than the last copied row, not "today": a run that crosses UTC midnight
-- would otherwise lose the whole day it started on. Past days are complete once copied —
-- check-ins are stamped NOW(), so nothing lands in them later. The cut-off is taken here,
-- as a literal, so the INSERT below is an index range scan over today's rows; written as a
-- subquery it planned a sequential scan of the whole old table, under the lock.
SELECT COALESCE(max(created_at), '-infinity') AS cutoff FROM checkins_new \gset
\echo copying rows newer than :cutoff
BEGIN;
LOCK TABLE checkins IN ACCESS EXCLUSIVE MODE;
INSERT INTO checkins_new (id, device_id, battery_pct, build_id, extra, created_at, state_hash)
SELECT id, device_id, battery_pct, build_id, extra, created_at, state_hash
FROM checkins
WHERE created_at > :'cutoff';
-- Free the names first: renaming a table does not rename its indexes or constraints.
ALTER INDEX idx_checkins_device_created_at RENAME TO checkins_old_device_created_at;
ALTER INDEX idx_checkins_created_at        RENAME TO checkins_old_created_at;
ALTER TABLE checkins RENAME CONSTRAINT checkins_device_id_fkey TO checkins_old_device_id_fkey;
ALTER TABLE checkins RENAME TO checkins_old;
ALTER TABLE checkins_new RENAME TO checkins;
ALTER INDEX checkins_new_device_created_at RENAME TO idx_checkins_device_created_at;
ALTER INDEX checkins_new_created_at        RENAME TO idx_checkins_created_at;
ALTER TABLE checkins RENAME CONSTRAINT checkins_new_device_id_fkey TO checkins_device_id_fkey;
COMMIT;

\echo === 5. check, then drop the old table — only if the rows made it across
-- Not a full count: counting both tables reads the whole old one again, and on 24 Sep that
-- load, landing as the queued check-ins were released, is when Postgres lost a backend.
-- Days before the copy's last day were each copied in one committed transaction (their
-- counts are in the log above) and cannot change afterwards, so what needs checking is
-- where the two copies meet: the last three days, each index range scans. checkins_old is
-- frozen since the swap and checkins keeps taking rows, so each day must have at least as
-- many new rows as old. Anything short keeps the old table.
SET max_parallel_workers_per_gather = 0;
DO $$
DECLARE d date; o bigint; n bigint;
BEGIN
  FOR d IN SELECT g::date FROM generate_series((now() AT TIME ZONE 'UTC')::date - 3, (now() AT TIME ZONE 'UTC')::date, interval '1 day') g LOOP
    SELECT count(*) INTO o FROM checkins_old WHERE created_at >= d::timestamptz AND created_at < (d + 1)::timestamptz;
    SELECT count(*) INTO n FROM checkins     WHERE created_at >= d::timestamptz AND created_at < (d + 1)::timestamptz;
    RAISE NOTICE '% old_rows=% new_rows=%', d, o, n;
    IF n < o THEN
      RAISE EXCEPTION 'day % has fewer rows in the new table — checkins_old kept, investigate before dropping', d;
    END IF;
  END LOOP;
  IF (SELECT min(created_at) FROM checkins) IS DISTINCT FROM (SELECT min(created_at) FROM checkins_old) THEN
    RAISE EXCEPTION 'oldest row differs between the tables — checkins_old kept';
  END IF;
END $$;
DROP TABLE checkins_old;

\echo === 6. sizes after
SELECT pg_size_pretty(pg_total_relation_size('checkins')) AS checkins_total,
       pg_size_pretty(pg_database_size(current_database())) AS db_total;
\echo Done.
