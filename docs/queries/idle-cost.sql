-- What idle devices cost the database per hour — the senior dev's fourth question.
-- Read-only, and every scan is bounded to one hour on an indexed time column: this box
-- has had Postgres OOM-killed by a single heavy read.
--
--   docker exec -i mdm-postgres-1 psql -U mdm -d mdm -f - < docs/queries/idle-cost.sql
--
-- "Idle" = reported at least once in the last finished clock hour, with no row in
-- device_state_events in it (nothing changed state). Bytes per row are given two ways:
--   marginal  — the rows written in that hour: pg_column_size(row) + 24 B tuple header
--               + 4 B line pointer (heap), and the table's index bytes per live row;
--   amortized — whole relation ÷ live rows, which also carries free space and bloat.

BEGIN READ ONLY;
SET LOCAL statement_timeout = '60s';

-- The hour, fixed once so every query below measures the same one.
SELECT date_trunc('hour', now()) - interval '1 hour' AS t0,
       date_trunc('hour', now())                     AS t1 \gset
\echo '== hour measured:' :t0 '→' :t1

\echo '== devices in the hour: idle vs changed, by agent'
WITH rep AS (
    SELECT c.device_id, count(*) AS checkins
    FROM checkins c
    WHERE c.created_at >= :'t0' AND c.created_at < :'t1'
    GROUP BY 1
), smp AS (
    SELECT s.device_id, count(*) AS samples
    FROM device_samples s
    WHERE s.at >= :'t0' AND s.at < :'t1'
    GROUP BY 1
), ev AS (
    SELECT e.device_id, count(*) AS events
    FROM device_state_events e
    WHERE e.at >= :'t0' AND e.at < :'t1'
    GROUP BY 1
), dev AS (
    SELECT d.id,
           CASE WHEN d.agent_kind = 'firmware' THEN 'firmware'
                ELSE COALESCE(NULLIF(d.latest_extra->>'agent_type', ''), d.agent_kind) END AS agent,
           COALESCE(rep.checkins, 0) AS checkins,
           COALESCE(smp.samples, 0)  AS samples,
           COALESCE(ev.events, 0)    AS events
    FROM devices d
    LEFT JOIN rep ON rep.device_id = d.id
    LEFT JOIN smp ON smp.device_id = d.id
    LEFT JOIN ev  ON ev.device_id  = d.id
    WHERE rep.device_id IS NOT NULL OR smp.device_id IS NOT NULL
)
SELECT state,
       agent,
       count(*)                          AS devices,
       round(avg(checkins), 1)           AS checkins_per_dev_h,
       round(avg(samples), 1)            AS samples_per_dev_h,
       round(avg(events), 1)             AS events_per_dev_h,
       max(checkins)                     AS max_checkins
FROM (SELECT *, CASE WHEN events = 0 THEN 'idle' ELSE 'changed' END AS state FROM dev) x
GROUP BY ROLLUP (state, agent)
ORDER BY state NULLS LAST, agent NULLS LAST;

\echo '== bytes per row: marginal (rows written in the hour) and amortized (whole table)'
WITH w AS (
    SELECT 'checkins' AS tbl, count(*) AS rows_h, avg(pg_column_size(c.*)) + 28 AS heap_b
    FROM checkins c
    WHERE c.created_at >= :'t0' AND c.created_at < :'t1'
    UNION ALL
    SELECT 'device_samples', count(*), avg(pg_column_size(s.*)) + 28
    FROM device_samples s
    WHERE s.at >= :'t0' AND s.at < :'t1'
    UNION ALL
    SELECT 'device_state_events', count(*), avg(pg_column_size(e.*)) + 28
    FROM device_state_events e
    WHERE e.at >= :'t0' AND e.at < :'t1'
)
SELECT w.tbl,
       w.rows_h,
       round(w.heap_b)                                                            AS heap_b_marginal,
       round(pg_indexes_size(w.tbl::regclass) / NULLIF(GREATEST(cl.reltuples, 0), 0)::numeric) AS index_b_per_row,
       round(pg_relation_size(w.tbl::regclass) / NULLIF(GREATEST(cl.reltuples, 0), 0)::numeric) AS heap_b_amortized,
       cl.reltuples::bigint                                                       AS est_rows,
       pg_size_pretty(pg_total_relation_size(w.tbl::regclass))                    AS total_size
FROM w JOIN pg_class cl ON cl.oid = w.tbl::regclass
ORDER BY 1;

\echo '== extra jsonb still written to checkins (bytes, rows in the hour)'
SELECT round(avg(pg_column_size(c.extra))) AS extra_b, max(pg_column_size(c.extra)) AS extra_b_max
FROM checkins c
    WHERE c.created_at >= :'t0' AND c.created_at < :'t1';

ROLLBACK;
