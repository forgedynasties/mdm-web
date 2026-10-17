package db

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
)

// PruneResolvedAlerts deletes resolved alerts older than `days` days so the table
// doesn't grow without bound (open/acknowledged alerts are always kept).
func (d *DB) PruneResolvedAlerts(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	tag, err := d.pool.Exec(ctx, fmt.Sprintf(
		`DELETE FROM alerts WHERE status = 'resolved' AND resolved_at < NOW() - INTERVAL '%d days'`, days))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PruneShaped applies the check-in retention setting to the history that replaced the
// checkins table: samples older than `days` go, and so do state events — except each
// device's last event per key before the cut-off. That one is the state the device was
// in when the kept window opens; without it every reader would see "unknown" until the
// key next changed, which for a timezone can be never.
func (d *DB) PruneShaped(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	s, err := d.pool.Exec(ctx, `DELETE FROM device_samples WHERE at < NOW() - make_interval(days => $1)`, days)
	if err != nil {
		return 0, err
	}
	e, err := d.pool.Exec(ctx, `
		DELETE FROM device_state_events e
		WHERE e.at < NOW() - make_interval(days => $1)
		  AND EXISTS (SELECT 1 FROM device_state_events n
		              WHERE n.device_id = e.device_id AND n.key = e.key
		                AND n.at > e.at AND n.at < NOW() - make_interval(days => $1))`, days)
	if err != nil {
		return s.RowsAffected(), err
	}
	return s.RowsAffected() + e.RowsAffected(), nil
}

// DownsampleSamples is DownsampleCheckins for device_samples: days older than
// olderThanDays keep the first sample per device per bucketSec, up to maxDays productive
// days a run. State events are never thinned — they are one row per real change already.
func (d *DB) DownsampleSamples(ctx context.Context, olderThanDays, bucketSec, maxDays int) (rows int64, days int, err error) {
	if olderThanDays <= 0 || bucketSec <= 0 || maxDays <= 0 {
		return 0, 0, nil
	}
	var day, cutoff time.Time
	err = d.pool.QueryRow(ctx, `
		SELECT COALESCE(MIN(at)::date, CURRENT_DATE), (CURRENT_DATE - $1::int)
		FROM device_samples`, olderThanDays).Scan(&day, &cutoff)
	if err != nil {
		return 0, 0, err
	}
	const maxExamined = 400
	for examined := 0; days < maxDays && examined < maxExamined && day.Before(cutoff); examined++ {
		var n int64
		err = d.pool.QueryRow(ctx, `
			WITH doomed AS (
				SELECT device_id, at FROM (
					SELECT device_id, at, ROW_NUMBER() OVER (
						PARTITION BY device_id,
						             to_timestamp(floor(EXTRACT(EPOCH FROM at) / $2::int) * $2::int)
						ORDER BY at
					) AS rn
					FROM device_samples
					WHERE at >= $1::date AND at < $1::date + 1
				) t WHERE t.rn > 1
			), del AS (
				DELETE FROM device_samples s USING doomed dd
				WHERE s.device_id = dd.device_id AND s.at = dd.at RETURNING 1
			)
			SELECT COUNT(*) FROM del`, day, bucketSec).Scan(&n)
		if err != nil {
			return rows, days, err
		}
		rows += n
		if n > 0 {
			days++
		}
		day = day.AddDate(0, 0, 1)
	}
	return rows, days, nil
}


// RollupDailyStatsFor computes a day from the shaped tables for every device that can
// be answered, and leaves the rest alone. Callers should use this rather than
// RollupDailyStatsShaped directly.
//
// It used to be all or nothing: one device anywhere in the fleet without a state
// baseline failed the whole day. On live that was always some unassigned tablet, so
// every venue's day stopped updating at whatever hour that tablet first reported —
// 21 Sep was frozen at 06:46 and read as 6.9 powered hours for tablets that were on
// all day. Now the unanswerable devices are skipped, keep their last row, and get
// their online minutes (which need no state) corrected on their own.
func (d *DB) RollupDailyStatsFor(ctx context.Context, day time.Time) (int64, error) {
	total, unready, err := d.shapedRollupUnready(ctx, day)
	if err != nil {
		return 0, err
	}
	if total == 0 || len(unready) == total {
		// With the checkins table retired there is no second source. A device the shaped
		// tables cannot answer would otherwise roll up with every state-derived figure
		// zero, written as fact; better not rolled up at all, and said so.
		return 0, fmt.Errorf("rollup %s: shaped tables cannot answer the day (%d of %d device(s) without a state baseline)",
			day.Format("2006-01-02"), len(unready), total)
	}
	n, err := d.rollupDailyStatsShaped(ctx, day, unready)
	if err != nil {
		return n, err
	}
	if len(unready) > 0 {
		if _, err := d.recomputeOnlineMinutes(ctx, day, unready); err != nil {
			return n, err
		}
	}
	return n, nil
}

// shapedRollupUnready returns how many devices reported during the day, and which of
// them did not yet have a recorded baseline for each state the aggregates read, at or
// before their first sample of that day.
//
// It asks that question of the data rather than comparing against the date the fix
// shipped. A date would be a claim about deployment; this is a check of the thing that
// actually matters, and it stays true if history is ever backfilled or a device is
// re-enrolled. It is also naturally satisfied for a device that was offline: such a
// device contributes no samples at all, and when it returns its baseline is written in
// the same transaction as its first sample — and events sort before samples at equal
// timestamps, so it is covered from that very first row.
//
// A device short of it is left out of the rollup, because the failure it guards
// against is silent: an unknown state reads as a device that was never charging, not
// as an error.
func (d *DB) shapedRollupUnready(ctx context.Context, day time.Time) (int, []uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `
		WITH first_sample AS (
			SELECT device_id, MIN(at) AS t
			FROM device_samples
			WHERE at >= $1::date AND at < ($1::date + INTERVAL '1 day')
			GROUP BY device_id
		)
		SELECT f.device_id, EXISTS (
			SELECT 1
			FROM devices dv
			CROSS JOIN unnest($2::text[]) AS k(key)
			WHERE dv.id = f.device_id
			  AND dv.latest_extra ? k.key
			  AND NOT EXISTS (
				SELECT 1 FROM device_state_events e
				WHERE e.device_id = f.device_id AND e.key = k.key AND e.at <= f.t))
		FROM first_sample f`,
		day.Format("2006-01-02"), rollupStateKeys)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	total := 0
	var unready []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var bad bool
		if err := rows.Scan(&id, &bad); err != nil {
			return 0, nil, err
		}
		total++
		if bad {
			unready = append(unready, id)
		}
	}
	return total, unready, rows.Err()
}

// RollupDailyStatsShaped computes the same day from device_samples and
// device_state_events instead of the check-in snapshots. Every statistic is produced by
// the identical SQL — only the samples CTE differs — so the two can be run against each
// other and compared column by column, which is how this was checked.
//
// Not yet wired into the housekeeping. It cannot be until every device has a recorded
// baseline for the states it reports: the aggregates that read a state (charging_frac,
// wlc_guest_frac, pad_readable, the charge sessions, and online_minutes_open through
// the device's timezone) return a device that was simply never charging, rather than
// failing, when the state is unknown. The seeding fix makes those baselines appear on
// each device's next check-in; this becomes correct a day after that ships.
//
// Verified on live over one identical five-hour window across 80 devices: for the 39
// with a charging baseline already recorded, checkin_count, battery min/max/avg,
// temp_max, ram_pct_peak, charging_frac, online_minutes, build_id, wlc_guest_frac,
// pad_readable, discharge_pct, charge_connect_n and screen_on_minutes all matched
// exactly. Every remaining difference traced to a missing baseline and to nothing else
// — the 31 devices whose online_minutes_open differed are precisely the 31 with no
// venue timezone configured, which fall through to the device's own timezone, of which
// there were no events at all.
func (d *DB) RollupDailyStatsShaped(ctx context.Context, day time.Time) (int64, error) {
	return d.rollupDailyStatsShaped(ctx, day, nil)
}

// rollupDailyStatsShaped is the rollup with some devices left out; their rows are not
// touched.
func (d *DB) rollupDailyStatsShaped(ctx context.Context, day time.Time, skip []uuid.UUID) (int64, error) {
	if skip == nil {
		skip = []uuid.UUID{}
	}
	tag, err := d.pool.Exec(ctx, rollupSQL(rollupSamplesFromShaped), day.Format("2006-01-02"), rollupStateKeys, skip)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// The daily rollup is split so that the arithmetic and the source of the readings are
// separate things. Everything that computes a statistic lives in rollupAggregateSQL and
// is shared verbatim; only the samples CTE differs between reading a check-in snapshot
// and reading the shaped tables. Two copies of a hundred lines of time-weighted
// aggregation would drift, and a rollup that drifts does not fail — it reports a
// slightly different number and nobody can tell which one is right.
func rollupSQL(samplesCTE string) string {
	return "\t\tWITH samples AS (\n" + samplesCTE + "\n\t\t),\n" + rollupAggregateSQL
}


const rollupAggregateSQL = `		-- Charge SESSIONS, not flag flips. The charging flag chatters: a full battery
		-- alternates CHARGING/FULL, and a failing charger can toggle ten times a minute
		-- (which is what the charger_flapping rule exists to catch). Counting every
		-- false->true edge therefore reported thousands of "charges" a week, all at 98%.
		-- Consecutive samples in the same state are folded into one run, and only runs
		-- that lasted a few minutes count as somebody actually plugging the device in.
		runs AS (
			SELECT device_id, battery_pct, created_at,
				COALESCE((extra->>'charging')::boolean, false) AS charging,
				SUM(CASE WHEN COALESCE((extra->>'charging')::boolean, false)
				         IS DISTINCT FROM prev_charging THEN 1 ELSE 0 END)
					OVER (PARTITION BY device_id ORDER BY created_at) AS run_id
			FROM samples
		),
		sessions AS (
			SELECT device_id, run_id, bool_or(charging) AS charging,
				EXTRACT(EPOCH FROM (MAX(created_at) - MIN(created_at))) AS secs,
				(ARRAY_AGG(battery_pct ORDER BY created_at))[1] AS first_batt,
				(ARRAY_AGG(battery_pct ORDER BY created_at DESC))[1] AS last_batt
			FROM runs
			GROUP BY device_id, run_id
		),
		-- Only a session somebody both plugged IN and unplugged counts. A charging run
		-- with no not-charging run before it in the day began before midnight, and one
		-- with none after it was still on charge at midnight — neither edge was seen
		-- here. Counting those meant a tablet left plugged at 100% over a long weekend
		-- was reported as a fresh "charge at 100%" every single day, and the same for a
		-- unit sitting flat and unplugged: today's rollup re-counted yesterday's state.
		-- An open run is not lost, only unreported until the day its other edge lands.
		bounded AS (
			SELECT *,
				LAG(run_id)  OVER (PARTITION BY device_id ORDER BY run_id) AS prev_run,
				LEAD(run_id) OVER (PARTITION BY device_id ORDER BY run_id) AS next_run
			FROM sessions
		),
		charge_events AS (
			SELECT device_id,
				COUNT(*)::int AS n,
				COALESCE(SUM(first_batt), 0)::real AS connect_sum,
				COALESCE(SUM(last_batt), 0)::real AS disconnect_sum
			FROM bounded
			WHERE charging AND secs >= 300   -- five minutes on charge = a real plug-in
			  AND prev_run IS NOT NULL       -- plugged in during this day
			  AND next_run IS NOT NULL       -- and unplugged again during it
			GROUP BY device_id
		)
		INSERT INTO device_daily_stats AS s (
			device_id, day, checkin_count, battery_min, battery_max, battery_avg,
			temp_max, ram_pct_peak, charging_frac, online_minutes, build_id,
			first_seen_at, last_seen_at,
			wlc_guest_frac, pad_readable, storage_free_last_gb, discharge_pct,
			wlc_minutes, wlc_drain_pct, wlc_drain_minutes,
			charge_connect_sum, charge_connect_n, charge_disconnect_sum, charge_disconnect_n,
			online_minutes_open, screen_on_minutes, computed_at)
		SELECT
			s.device_id,
			$1::date,
			COUNT(*),
			MIN(battery_pct),
			MAX(battery_pct),
			COALESCE((SUM(battery_pct * w) / NULLIF(SUM(w), 0))::real, AVG(battery_pct)::real),
			MAX((extra->>'battery_temp_c')::numeric)::real,
			MAX(COALESCE(
				((extra->'ram_usage_mb'->>'used')::numeric * 100)
					/ NULLIF((extra->'ram_usage_mb'->>'total')::numeric, 0),
				0))::smallint,
			COALESCE(
				(SUM(CASE WHEN (extra->>'charging')::boolean THEN w ELSE 0 END) / NULLIF(SUM(w), 0))::real,
				AVG(CASE WHEN (extra->>'charging')::boolean THEN 1 ELSE 0 END)::real),
			(SUM(w) / 60.0)::int,
			(ARRAY_AGG(build_id ORDER BY created_at DESC))[1],
			MIN(created_at),
			MAX(created_at),
			-- T7 pad utilisation: time-weighted fraction with a guest device on the pad,
			-- whether the pad was readable at all that day, and the day's last storage reading.
			COALESCE(
				(SUM(CASE WHEN extra->>'wlc_status' = '1' THEN w ELSE 0 END) / NULLIF(SUM(w), 0))::real,
				AVG(CASE WHEN extra->>'wlc_status' = '1' THEN 1 ELSE 0 END)::real),
			-- Readable means the pad answered: 0 or 1. Excludes -1 (sysfs read failed) and
			-- 2 (no pad attached) — 2 is a successful read of a floating pin, so treating
			-- it as readable would report a healthy pad on a unit that has none.
			bool_or(extra->>'wlc_status' IN ('0', '1')),
			(ARRAY_AGG((extra->>'storage_free_gb')::numeric ORDER BY created_at DESC))[1]::real,
			-- True discharge: sum of per-step battery DROPS (charge climbs contribute 0,
			-- so a device that only charges accrues 0). NULL prev_batt (day's first
			-- sample) contributes nothing, so a discharge spanning midnight isn't split.
			COALESCE(SUM(GREATEST(0, prev_batt - battery_pct)), 0)::real,
			-- Guest pad: how long a customer's phone sat on this tablet.
			(SUM(CASE WHEN extra->>'wlc_status' = '1' THEN w ELSE 0 END) / 60.0)::real,
			-- What that cost us: battery drops while the pad was busy AND we were not on
			-- mains. On mains the charger hides the cost, so those samples are excluded
			-- from both the drop and the minutes it is divided by.
			COALESCE(SUM(CASE WHEN extra->>'wlc_status' = '1' AND COALESCE((extra->>'charging')::boolean, false) = false
				THEN GREATEST(0, prev_batt - battery_pct) ELSE 0 END), 0)::real,
			(SUM(CASE WHEN extra->>'wlc_status' = '1' AND COALESCE((extra->>'charging')::boolean, false) = false
				THEN w ELSE 0 END) / 60.0)::real,
			-- Battery at the start and end of each charge session (see charge_events).
			-- Sums and counts rather than averages, so a week aggregates as SUM/SUM.
			COALESCE(MAX(ce.connect_sum), 0)::real,
			COALESCE(MAX(ce.n), 0)::int,
			COALESCE(MAX(ce.disconnect_sum), 0)::real,
			COALESCE(MAX(ce.n), 0)::int,
			-- Powered minutes inside the site's opening hours. NULL when the site has no
			-- window configured, so the UI can say so instead of showing a misleading 0.
			CASE WHEN bool_or(in_open_hours IS NOT NULL)
				THEN (SUM(CASE WHEN in_open_hours THEN w ELSE 0 END) / 60.0)::real END,
			-- Minutes with the screen on. NULL unless at least one sample carried the
			-- field, so a device that has not taken the firmware yet is excluded from the
			-- standby figure instead of counting as 100% idle.
			CASE WHEN bool_or(extra ? 'screen_on')
				THEN (SUM(CASE WHEN (extra->>'screen_on')::boolean THEN w ELSE 0 END) / 60.0)::real END,
			NOW()
		FROM samples s
		LEFT JOIN charge_events ce ON ce.device_id = s.device_id
		GROUP BY s.device_id
		ON CONFLICT (device_id, day) DO UPDATE SET
			checkin_count  = EXCLUDED.checkin_count,
			battery_min    = EXCLUDED.battery_min,
			battery_max    = EXCLUDED.battery_max,
			battery_avg    = EXCLUDED.battery_avg,
			temp_max       = EXCLUDED.temp_max,
			ram_pct_peak   = EXCLUDED.ram_pct_peak,
			charging_frac  = EXCLUDED.charging_frac,
			online_minutes = EXCLUDED.online_minutes,
			build_id       = EXCLUDED.build_id,
			first_seen_at  = EXCLUDED.first_seen_at,
			last_seen_at   = EXCLUDED.last_seen_at,
			wlc_guest_frac       = EXCLUDED.wlc_guest_frac,
			pad_readable         = EXCLUDED.pad_readable,
			storage_free_last_gb = EXCLUDED.storage_free_last_gb,
			discharge_pct  = EXCLUDED.discharge_pct,
			wlc_minutes           = EXCLUDED.wlc_minutes,
			wlc_drain_pct         = EXCLUDED.wlc_drain_pct,
			wlc_drain_minutes     = EXCLUDED.wlc_drain_minutes,
			charge_connect_sum    = EXCLUDED.charge_connect_sum,
			charge_connect_n      = EXCLUDED.charge_connect_n,
			charge_disconnect_sum = EXCLUDED.charge_disconnect_sum,
			charge_disconnect_n   = EXCLUDED.charge_disconnect_n,
			online_minutes_open   = EXCLUDED.online_minutes_open,
			screen_on_minutes     = EXCLUDED.screen_on_minutes,
			computed_at    = EXCLUDED.computed_at`

// rollupStateKeys are the states the aggregates read. Kept beside the CTE that pivots
// them so adding an aggregate that reads a new state cannot forget to carry it.
var rollupStateKeys = []string{"charging", "wlc_status", "screen_on", "timezone"}

// rollupSamplesFromShaped reads the same day's readings out of device_samples and
// device_state_events, and hands the aggregates the identical columns — including a
// rebuilt `extra` object.
//
// Rebuilding extra rather than rewriting the aggregates against typed columns is the
// same call made for the CSV export, for the same reason and with more at stake: the
// hundred lines downstream stay character-for-character identical between the two
// sources, so the only thing this change can alter is where the numbers came from. A
// rollup that computes a slightly different number does not fail; it publishes, and
// then nobody can say which figure was right.
//
// Every aggregate reads its state through ->>, which yields text whether the value was
// stored as a JSON string or a number, so storing the event values as strings changes
// no comparison downstream.
//
// The states are carried forward by gaps-and-islands rather than by a lookup per
// sample: samples and events are merged into one ordered stream, each state column is
// non-null only on its own event rows, a running count of non-nulls labels the stretch
// between two events, and first_value fills the stretch. Events sort before samples at
// the same instant, which matters because the writer records a sample and the events
// around it in one transaction at one timestamp — the flip belongs to the sample it
// arrived with.
const rollupSamplesFromShaped = `
			WITH bounds AS (SELECT $1::date AS d0, ($1::date + INTERVAL '1 day') AS d1),
			-- The state of play when the day opened, pinned to its first instant, plus
			-- every change during it. Without the leading row a device that has been on
			-- charge since yesterday reads as unknown until it next unplugs.
			ev AS (
				-- One primary-key probe per (device, state) rather than DISTINCT ON over
				-- every event before the day: that sorted all of history (415k rows, on
				-- disk) to keep a few hundred, and was where the temp files came from.
				-- Checked row-for-row against the DISTINCT ON form on live data.
				(
					SELECT d.id AS device_id, b.d0::timestamptz AS at, k.key, l.to_val
					FROM devices d CROSS JOIN unnest($2::text[]) AS k(key) CROSS JOIN bounds b
					CROSS JOIN LATERAL (
						SELECT e.to_val FROM device_state_events e
						WHERE e.device_id = d.id AND e.key = k.key AND e.at < b.d0
						ORDER BY e.at DESC LIMIT 1
					) l
				)
				UNION ALL
				(
					SELECT e.device_id, e.at, e.key, e.to_val
					FROM device_state_events e, bounds b
					WHERE e.key = ANY($2) AND e.at >= b.d0 AND e.at < b.d1
				)
			),
			-- The build each device was running when the window opened. Three sources in
			-- order of authority: the last recorded change before it; failing that the
			-- from_build of its first change after it, which states what it was running
			-- beforehand; and failing both, the build on the device, which is correct
			-- precisely because a device with no history has never changed build.
			bh AS (
				SELECT d.id AS device_id, b.d0::timestamptz AS at,
					COALESCE(
						(SELECT h.to_build FROM device_build_history h
						  WHERE h.device_id = d.id AND h.at < b.d0 ORDER BY h.at DESC LIMIT 1),
						(SELECT h.from_build FROM device_build_history h
						  WHERE h.device_id = d.id AND h.at >= b.d0 ORDER BY h.at LIMIT 1),
						d.build_id
					) AS to_build
				FROM devices d, bounds b
				UNION ALL
				SELECT h.device_id, h.at, h.to_build
				FROM device_build_history h, bounds b
				WHERE h.at >= b.d0 AND h.at < b.d1
			),
			merged AS (
				SELECT s.device_id, s.at, TRUE AS is_sample,
				       s.battery_pct, s.temp_c, s.ram_used_mb, s.ram_total_mb, s.storage_free_gb,
				       NULL::text AS ev_charging, NULL::text AS ev_wlc,
				       NULL::text AS ev_screen, NULL::text AS ev_tz, NULL::text AS ev_build
				FROM device_samples s, bounds b
				WHERE s.at >= b.d0 AND s.at < b.d1
				  AND NOT (s.device_id = ANY($3::uuid[]))
				UNION ALL
				SELECT ev.device_id, ev.at, FALSE,
				       NULL::smallint, NULL::float8, NULL::int, NULL::int, NULL::float8,
				       -- "2" is a flapping charger (charger_flap.go). It is carried as not
				       -- charging: the ::boolean casts below would fail on it, and a charger
				       -- toggling every second is not charging the device in any useful sense.
				       CASE WHEN ev.key = 'charging'   THEN CASE WHEN ev.to_val = '2' THEN 'false' ELSE ev.to_val END END,
				       CASE WHEN ev.key = 'wlc_status' THEN ev.to_val END,
				       CASE WHEN ev.key = 'screen_on'  THEN ev.to_val END,
				       CASE WHEN ev.key = 'timezone'   THEN ev.to_val END,
				       NULL::text
				FROM ev
				UNION ALL
				SELECT bh.device_id, bh.at, FALSE,
				       NULL::smallint, NULL::float8, NULL::int, NULL::int, NULL::float8,
				       NULL::text, NULL::text, NULL::text, NULL::text, bh.to_build
				FROM bh
			),
			grouped AS (
				SELECT m.*,
					count(ev_charging) OVER w AS g_charging,
					count(ev_wlc)      OVER w AS g_wlc,
					count(ev_screen)   OVER w AS g_screen,
					count(ev_tz)       OVER w AS g_tz,
					count(ev_build)    OVER w AS g_build
				FROM merged m
				WINDOW w AS (PARTITION BY m.device_id ORDER BY m.at, m.is_sample
				             ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)
			),
			carried AS (
				SELECT device_id, at, is_sample, battery_pct, temp_c, ram_used_mb, ram_total_mb, storage_free_gb,
					first_value(ev_charging) OVER (PARTITION BY device_id, g_charging ORDER BY at, is_sample) AS charging,
					first_value(ev_wlc)      OVER (PARTITION BY device_id, g_wlc      ORDER BY at, is_sample) AS wlc_status,
					first_value(ev_screen)   OVER (PARTITION BY device_id, g_screen   ORDER BY at, is_sample) AS screen_on,
					first_value(ev_tz)       OVER (PARTITION BY device_id, g_tz       ORDER BY at, is_sample) AS timezone,
					first_value(ev_build)    OVER (PARTITION BY device_id, g_build    ORDER BY at, is_sample) AS build_id
				FROM grouped
			),
			shaped AS (
				SELECT c.device_id, c.at AS created_at, c.battery_pct, COALESCE(c.build_id, '') AS build_id,
				       -- Absent keys are stripped rather than left null, so a key-presence
				       -- test still answers "did this device ever report it", as it does
				       -- against a real snapshot.
				       jsonb_strip_nulls(jsonb_build_object(
				           'charging',        c.charging,
				           'wlc_status',      c.wlc_status,
				           'screen_on',       c.screen_on,
				           'timezone',        c.timezone,
				           'battery_temp_c',  c.temp_c,
				           'storage_free_gb', c.storage_free_gb,
				           'ram_usage_mb',    CASE WHEN c.ram_used_mb IS NOT NULL AND c.ram_total_mb IS NOT NULL
				                                   THEN jsonb_build_object('used', c.ram_used_mb, 'total', c.ram_total_mb) END
				       )) AS extra
				FROM carried c
				WHERE c.is_sample
			)
			SELECT s.device_id, s.battery_pct, s.build_id, s.created_at, s.extra,
				-- COALESCE inside LEAST, not around it: LEAST ignores NULLs, so the day's
				-- last sample (no LEAD) was credited the full 600s cap. That put every
				-- always-on device at 1445-1450 minutes, a day of 24.1 hours.
				LEAST(COALESCE(EXTRACT(EPOCH FROM (
					LEAD(s.created_at) OVER (PARTITION BY s.device_id ORDER BY s.created_at)
					- s.created_at)), 0), 600) AS w,
				LAG(s.battery_pct) OVER (PARTITION BY s.device_id ORDER BY s.created_at) AS prev_batt,
				LAG((s.extra->>'charging')::boolean) OVER (PARTITION BY s.device_id ORDER BY s.created_at) AS prev_charging,
				CASE
					WHEN COALESCE(sw.open_min, fsw.open_min) IS NULL THEN NULL
					ELSE (
						WITH lt AS (
							SELECT (EXTRACT(HOUR FROM s.created_at AT TIME ZONE COALESCE(NULLIF(sw.timezone, ''), NULLIF(fsw.timezone, ''), NULLIF(s.extra->>'timezone', ''), 'UTC')) * 60
							      + EXTRACT(MINUTE FROM s.created_at AT TIME ZONE COALESCE(NULLIF(sw.timezone, ''), NULLIF(s.extra->>'timezone', ''), 'UTC')))::int AS m
						)
						SELECT CASE WHEN COALESCE(sw.open_min, fsw.open_min) <= COALESCE(sw.close_min, fsw.close_min)
							THEN lt.m >= COALESCE(sw.open_min, fsw.open_min) AND lt.m < COALESCE(sw.close_min, fsw.close_min)
							ELSE lt.m >= COALESCE(sw.open_min, fsw.open_min) OR lt.m < COALESCE(sw.close_min, fsw.close_min) END
						FROM lt
					)
				END AS in_open_hours
			FROM shaped s
			JOIN devices d ON d.id = s.device_id
			LEFT JOIN service_windows sw ON sw.restaurant_id = d.restaurant_id
			LEFT JOIN service_windows fsw ON fsw.restaurant_id IS NULL AND fsw.group_id IS NULL`

// backfillFlag marks the one-time historical daily-stats backfill as done, so it never
// re-runs on subsequent deploys (the hourly housekeeping keeps recent days current).
const backfillFlag = "daily_stats_backfilled_v1"

// BackfillDailyStats rolls up recent calendar days that have checkins but no
// device_daily_stats row yet, so historical telemetry is captured the first time the
// feature is deployed. Bounded to the last maxDays days (checkins beyond retention are
// pruned, so older days have no data to roll up) and probed with cheap indexed EXISTS
// queries — no full-table scan. Runs at most once ever (guarded by app_flags), so every
// later deploy returns immediately and startup stays fast. Returns days processed.
// BackfillCommandAuthors attributes authorless commands (rows from before
// created_by was recorded) from the audit log's command.send entries: by the
// "cmd=<id>" detail when present, otherwise by type and a ±3 s timestamp match.
// Idempotent — only touches created_by = '' — so it can run at every startup.
// The audit actor is already the post-merge username, so merged people land on
// their current account. OTA-sent reboots have no audit entry and stay authorless.
func (d *DB) BackfillCommandAuthors(ctx context.Context) (int64, error) {
	var total int64
	byID, err := d.pool.Exec(ctx, `
		UPDATE commands c SET created_by = a.actor
		FROM audit_log a
		WHERE c.created_by = '' AND a.action = 'command.send' AND a.actor <> ''
		  AND a.detail LIKE '%cmd=' || c.id::text || '%'`)
	if err != nil {
		return 0, err
	}
	total += byID.RowsAffected()
	byTime, err := d.pool.Exec(ctx, `
		UPDATE commands c SET created_by = s.actor
		FROM (
			SELECT c2.id,
			       (SELECT a.actor FROM audit_log a
			         WHERE a.action = 'command.send' AND a.actor <> '' AND a.target = c2.type
			           AND a.created_at BETWEEN c2.created_at - interval '3 seconds' AND c2.created_at + interval '3 seconds'
			         ORDER BY abs(extract(epoch from (a.created_at - c2.created_at))) LIMIT 1) AS actor
			FROM commands c2
			WHERE c2.created_by = '' AND c2.type <> 'ota'
		) s
		WHERE c.id = s.id AND s.actor IS NOT NULL`)
	if err != nil {
		return total, err
	}
	total += byTime.RowsAffected()
	return total, nil
}

// siteMetricsBackfillFlag marks the one-time recompute that fills the power/usage columns
// on days that were rolled up before those columns existed.
// v3: a versioned recompute has to run even when the columns are already populated.
// v2 only looked for days where they were NULL, so after v1 had filled them with the bad
// charge counts every day was skipped and the flag was written as though the work was
// done — leaving 32,000 "charges" on days 13 and 14 while fresh days read correctly.
// v4: the last sample of each day was weighted 600s, so online time ran 10 minutes over.
// v5: v4 skipped every day with one device the shaped tables cannot answer; the rollup
// now leaves out only that device, so those days recompute in full.
const siteMetricsBackfillFlag = "site_metrics_backfilled_v5"

// recomputeOnlineMinutes rewrites one day's online_minutes from device_samples, with
// the same weighting as the rollup (each sample credited the gap to the next, capped
// at ten minutes; the day's last sample credited nothing). For the devices, or days, the
// full rollup cannot answer; nil means every device. computed_at moves so cached report
// PDFs of that week go stale.
func (d *DB) recomputeOnlineMinutes(ctx context.Context, day time.Time, only []uuid.UUID) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE device_daily_stats s
		SET online_minutes = x.m, computed_at = NOW()
		FROM (
			SELECT device_id, (SUM(w) / 60.0)::int AS m
			FROM (
				SELECT device_id,
					LEAST(COALESCE(EXTRACT(EPOCH FROM (
						LEAD(at) OVER (PARTITION BY device_id ORDER BY at) - at)), 0), 600) AS w
				FROM device_samples
				WHERE at >= $1::date AND at < ($1::date + INTERVAL '1 day')
				  AND ($2::uuid[] IS NULL OR device_id = ANY($2::uuid[]))
			) g
			GROUP BY device_id
		) x
		WHERE s.device_id = x.device_id AND s.day = $1::date AND s.online_minutes <> x.m`,
		day.Format("2006-01-02"), only)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// BackfillSiteMetrics recomputes recent days so the power/usage figures cover a whole
// week immediately rather than filling in a day at a time, and so a corrected formula
// reaches the days already stored under the old one.
//
// It recomputes unconditionally within the window: whether a day looks populated says
// nothing about WHICH version of the maths populated it. Bounded and flagged, because
// checkins are kept forever and this runs on a small box.
func (d *DB) BackfillSiteMetrics(ctx context.Context, maxDays int) (int, error) {
	var done bool
	if err := d.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM app_flags WHERE flag = $1)`, siteMetricsBackfillFlag).Scan(&done); err != nil {
		return 0, err
	}
	if done {
		return 0, nil
	}
	if maxDays <= 0 || maxDays > 30 {
		maxDays = 14
	}
	today := time.Now().UTC()
	n := 0
	for i := 1; i <= maxDays; i++ { // today and yesterday are owned by housekeeping
		day := today.AddDate(0, 0, -i)
		// Only days that actually have rows: rolling up a day with no checkins is a
		// wasted scan on a box that cannot spare one.
		var hasRows bool
		if err := d.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM device_daily_stats WHERE day = $1::date)`,
			day.Format("2006-01-02")).Scan(&hasRows); err != nil {
			return n, err
		}
		if !hasRows {
			continue
		}
		// A day the shaped tables cannot answer keeps its stored row, but its online
		// minutes are still corrected: they come from sample timestamps alone, not from
		// the state baselines the full rollup is waiting for. It must not stop the days
		// after it, or the flag is never written and this re-runs every boot.
		if _, err := d.RollupDailyStatsFor(ctx, day); err != nil {
			n2, err2 := d.recomputeOnlineMinutes(ctx, day, nil)
			log.Printf("[backfill] site metrics %s: %v; online minutes recomputed for %d device(s) (err %v)",
				day.Format("2006-01-02"), err, n2, err2)
			continue
		}
		n++
	}
	if _, err := d.pool.Exec(ctx,
		`INSERT INTO app_flags (flag) VALUES ($1) ON CONFLICT DO NOTHING`, siteMetricsBackfillFlag); err != nil {
		return n, err
	}
	return n, nil
}
