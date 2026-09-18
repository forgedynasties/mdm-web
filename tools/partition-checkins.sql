-- Convert checkins to a monthly range-partitioned table.
--
-- WHY
--   Retention and space reclaim on a 13 GB heap are both expensive today: a DELETE
--   rewrites and then has to be vacuumed, and returning space to the volume needs a
--   full rewrite under an exclusive lock. With monthly partitions both become
--   DROP TABLE, which is instant and returns the space immediately.
--
--   It is also the moment to drop checkins_pkey. It is 756 MB, it has served zero
--   scans, nothing references checkins.id, and a random uuid index on an append-only
--   table is the worst case for write locality: every insert dirties a random page.
--   The id column stays (the public API echoes it); only the index goes.
--
-- BEFORE RUNNING
--   1. Take a backup and verify it by restoring, not by reading the TOC.
--   2. Let the dup-key strip finish (config dup_strip_cursor = "done"). It halves the
--      bytes this script has to copy — the copy writes live tuples, so stripped rows
--      are copied in their smaller form even though the old heap has not shrunk.
--   3. Check free space: this needs room for a second copy of the live data.
--
-- HOW IT RUNS
--   Phase 1 and 2 are online — the app keeps writing to the old table throughout.
--   Phase 3 is the only part that needs a quiet moment, and it is short because it
--   only moves rows written since phase 2 finished.
--
-- Run each phase separately. Do not run this file top to bottom unattended.

\set ON_ERROR_STOP on

-- ───────────────────────────────────────────────────────────────────────────
-- PHASE 1 — build the empty partitioned table (online, instant)
-- ───────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS checkins_p (
    id          UUID        NOT NULL DEFAULT gen_random_uuid(),
    device_id   UUID        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    battery_pct SMALLINT    NOT NULL,
    build_id    TEXT        NOT NULL DEFAULT '',
    extra       JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    state_hash  TEXT
) PARTITION BY RANGE (created_at);

-- No primary key. This is an append-only log that nothing looks up by id, and a
-- unique index on a partitioned table would have to include created_at to be legal,
-- producing an index bigger than the one being removed and just as unused.

-- Monthly partitions from the first check-in through a year ahead, so the table can
-- never reject a write because a partition is missing. Empty ones cost nothing.
DO $$
DECLARE
    m DATE := date_trunc('month', (SELECT COALESCE(MIN(created_at), NOW()) FROM checkins))::date;
    last_m DATE := (date_trunc('month', NOW()) + INTERVAL '12 months')::date;
BEGIN
    WHILE m <= last_m LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF checkins_p FOR VALUES FROM (%L) TO (%L)',
            'checkins_' || to_char(m, 'YYYY_MM'), m, (m + INTERVAL '1 month')::date);
        m := (m + INTERVAL '1 month')::date;
    END LOOP;
END $$;

-- The two indexes that earn their keep, by measured scan count:
--   idx_checkins_device_created_at  31,552 scans
--   idx_checkins_created_at          1,508 scans
-- Declared on the parent so every partition, including future ones, inherits them.
CREATE INDEX IF NOT EXISTS idx_checkins_p_device_created_at ON checkins_p (device_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_checkins_p_created_at        ON checkins_p (created_at DESC);


-- ───────────────────────────────────────────────────────────────────────────
-- PHASE 2 — copy history, one month at a time (online, slow, resumable)
-- ───────────────────────────────────────────────────────────────────────────
--
-- Run this block once per month of history, oldest first, checking free space as you
-- go. One month at a time keeps each statement's WAL and temp usage bounded, which
-- matters on a 2 GB Postgres — a single 13 GB INSERT..SELECT would not survive it.
--
-- Resumable: the NOT EXISTS makes a re-run of the same month copy only what is
-- missing, so an interrupted month can simply be repeated.
--
--   \set mon '2026-03-01'
--   INSERT INTO checkins_p (id, device_id, battery_pct, build_id, extra, created_at, state_hash)
--   SELECT c.id, c.device_id, c.battery_pct, c.build_id, c.extra, c.created_at, c.state_hash
--   FROM checkins c
--   WHERE c.created_at >= :'mon'::date
--     AND c.created_at <  (:'mon'::date + INTERVAL '1 month')
--     AND NOT EXISTS (SELECT 1 FROM checkins_p p WHERE p.id = c.id AND p.created_at = c.created_at);
--
-- After each month, compare the counts before moving on:
--
--   SELECT (SELECT count(*) FROM checkins   WHERE created_at >= :'mon'::date AND created_at < (:'mon'::date + INTERVAL '1 month')) AS old,
--          (SELECT count(*) FROM checkins_p WHERE created_at >= :'mon'::date AND created_at < (:'mon'::date + INTERVAL '1 month')) AS new;


-- ───────────────────────────────────────────────────────────────────────────
-- PHASE 3 — cut over (needs a quiet moment; seconds, not minutes)
-- ───────────────────────────────────────────────────────────────────────────
--
-- Everything above is repeatable and reversible: the old table is untouched and the
-- new one is invisible to the app. This is the step that is neither.
--
-- Stop the app first. The lock below would otherwise queue behind — and block — every
-- in-flight check-in, and the tail copy would race writes that arrive during it.
--
-- BEGIN;
--   LOCK TABLE checkins IN ACCESS EXCLUSIVE MODE;
--
--   -- Anything written since the last month was copied.
--   INSERT INTO checkins_p (id, device_id, battery_pct, build_id, extra, created_at, state_hash)
--   SELECT c.id, c.device_id, c.battery_pct, c.build_id, c.extra, c.created_at, c.state_hash
--   FROM checkins c
--   WHERE NOT EXISTS (SELECT 1 FROM checkins_p p WHERE p.id = c.id AND p.created_at = c.created_at);
--
--   -- Must match before going any further. If it does not, ROLLBACK and investigate.
--   SELECT (SELECT count(*) FROM checkins) AS old, (SELECT count(*) FROM checkins_p) AS new;
--
--   ALTER TABLE checkins   RENAME TO checkins_old;
--   ALTER TABLE checkins_p RENAME TO checkins;
--   ALTER INDEX idx_checkins_p_device_created_at RENAME TO idx_checkins_device_created_at_p;
--   ALTER INDEX idx_checkins_p_created_at        RENAME TO idx_checkins_created_at_p;
-- COMMIT;
--
-- Start the app. Watch that rows are landing:
--   SELECT count(*) FROM checkins WHERE created_at > NOW() - INTERVAL '5 minutes';
--
-- Keep checkins_old until you are satisfied — it is the rollback. Reverting is the
-- same two renames in reverse. Only then:
--   DROP TABLE checkins_old;    -- this is what returns the 13 GB to the volume


-- ───────────────────────────────────────────────────────────────────────────
-- AFTER
-- ───────────────────────────────────────────────────────────────────────────
-- Retention becomes a drop rather than a delete:
--   DROP TABLE checkins_2026_03;
--
-- New partitions are created ahead by the app's housekeeping (ensureCheckinPartitions),
-- so nothing has to be remembered. This script's own loop reaches a year out, which is
-- the safety net if that ever stops running.
