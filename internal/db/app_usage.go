package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Per-app usage (A1, picked 22 Oct 2026; user: "stats of individual apps and their time
// consumption of usage and contribution to usage of memory cpu etc. so that we know what
// apps or services are consuming most"). Clients post a summary of each app about every
// 15 minutes (POST /api/v1/app-usage); it is added into the hour it covers.
//
// What each client can measure: the firmware client (a system app) all of it; the
// Standard client screen time, launches and network only (other apps' memory, CPU and
// battery are closed to a normal app), so those columns stay zero for it.

// AppUsageReport is one app's share of one reporting window.
type AppUsageReport struct {
	Package  string
	System   bool
	FgMs     int64
	Launches int
	RxBytes  int64
	TxBytes  int64
	PssMB    *float64 // average over the window, nil = not measured
	PssMaxMB *int
	CPUPct   *float64
	PowerMAh float64
}

// AddAppUsage adds a window's reports into the hour bucket at `at`.
func (d *DB) AddAppUsage(ctx context.Context, deviceID uuid.UUID, at time.Time, apps []AppUsageReport) error {
	if len(apps) == 0 {
		return nil
	}
	hour := at.UTC().Truncate(time.Hour)
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, a := range apps {
		var pss, cpu float64
		var pssN, cpuN, pssMax int
		if a.PssMB != nil {
			pss, pssN = *a.PssMB, 1
		}
		if a.PssMaxMB != nil {
			pssMax = *a.PssMaxMB
		}
		if a.CPUPct != nil {
			cpu, cpuN = *a.CPUPct, 1
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO app_usage (device_id, bucket, daily, package, system, fg_ms, launches, rx_bytes, tx_bytes,
			                       pss_sum_mb, pss_max_mb, pss_n, cpu_sum_pct, cpu_n, power_mah)
			VALUES ($1, $2, FALSE, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT (device_id, daily, bucket, package) DO UPDATE SET
			    system = app_usage.system OR EXCLUDED.system,
			    fg_ms = app_usage.fg_ms + EXCLUDED.fg_ms,
			    launches = app_usage.launches + EXCLUDED.launches,
			    rx_bytes = app_usage.rx_bytes + EXCLUDED.rx_bytes,
			    tx_bytes = app_usage.tx_bytes + EXCLUDED.tx_bytes,
			    pss_sum_mb = app_usage.pss_sum_mb + EXCLUDED.pss_sum_mb,
			    pss_max_mb = GREATEST(app_usage.pss_max_mb, EXCLUDED.pss_max_mb),
			    pss_n = app_usage.pss_n + EXCLUDED.pss_n,
			    cpu_sum_pct = app_usage.cpu_sum_pct + EXCLUDED.cpu_sum_pct,
			    cpu_n = app_usage.cpu_n + EXCLUDED.cpu_n,
			    power_mah = app_usage.power_mah + EXCLUDED.power_mah`,
			deviceID, hour, a.Package, a.System, a.FgMs, a.Launches, a.RxBytes, a.TxBytes,
			pss, pssMax, pssN, cpu, cpuN, a.PowerMAh); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// AppUsageRow is one app's totals over a window, for the device page.
type AppUsageRow struct {
	Package  string
	Label    string // app name from the installed-app list, "" when unknown
	System   bool
	FgMs     int64
	Launches int
	RxBytes  int64
	TxBytes  int64
	PssAvgMB *float64
	PssMaxMB int
	CPUAvg   *float64
	PowerMAh float64
}

// AppUsage sums a device's usage since `since`. Hourly and daily rows never overlap
// (the rollup deletes the hours it folds), so both are simply added.
func (d *DB) AppUsage(ctx context.Context, deviceID uuid.UUID, since time.Time) ([]AppUsageRow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT u.package, COALESCE(MAX(p.app_name), ''), BOOL_OR(u.system),
		       SUM(u.fg_ms)::bigint, SUM(u.launches)::int, SUM(u.rx_bytes)::bigint, SUM(u.tx_bytes)::bigint,
		       CASE WHEN SUM(u.pss_n) > 0 THEN SUM(u.pss_sum_mb) / SUM(u.pss_n) END,
		       MAX(u.pss_max_mb),
		       CASE WHEN SUM(u.cpu_n) > 0 THEN SUM(u.cpu_sum_pct) / SUM(u.cpu_n) END,
		       SUM(u.power_mah)
		FROM app_usage u
		LEFT JOIN device_packages p ON p.device_id = u.device_id AND p.package_name = u.package
		WHERE u.device_id = $1
		  AND ((NOT u.daily AND u.bucket >= date_trunc('hour', $2::timestamptz))
		    OR (u.daily AND u.bucket >= date_trunc('day', $2::timestamptz)))
		GROUP BY u.package`, deviceID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppUsageRow
	for rows.Next() {
		var r AppUsageRow
		if err := rows.Scan(&r.Package, &r.Label, &r.System, &r.FgMs, &r.Launches, &r.RxBytes, &r.TxBytes,
			&r.PssAvgMB, &r.PssMaxMB, &r.CPUAvg, &r.PowerMAh); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RollupAppUsage folds hourly rows older than `keepHourly` into daily rows and deletes
// daily rows older than `keepDaily`. Run from housekeeping.
func (d *DB) RollupAppUsage(ctx context.Context, keepHourly, keepDaily time.Duration) (int64, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	cut := time.Now().UTC().Add(-keepHourly).Truncate(24 * time.Hour)
	if _, err := tx.Exec(ctx, `
		INSERT INTO app_usage (device_id, bucket, daily, package, system, fg_ms, launches, rx_bytes, tx_bytes,
		                       pss_sum_mb, pss_max_mb, pss_n, cpu_sum_pct, cpu_n, power_mah)
		SELECT device_id, date_trunc('day', bucket), TRUE, package, BOOL_OR(system), SUM(fg_ms), SUM(launches),
		       SUM(rx_bytes), SUM(tx_bytes), SUM(pss_sum_mb), MAX(pss_max_mb), SUM(pss_n), SUM(cpu_sum_pct),
		       SUM(cpu_n), SUM(power_mah)
		FROM app_usage WHERE NOT daily AND bucket < $1
		GROUP BY device_id, date_trunc('day', bucket), package
		ON CONFLICT (device_id, daily, bucket, package) DO UPDATE SET
		    fg_ms = app_usage.fg_ms + EXCLUDED.fg_ms, launches = app_usage.launches + EXCLUDED.launches,
		    rx_bytes = app_usage.rx_bytes + EXCLUDED.rx_bytes, tx_bytes = app_usage.tx_bytes + EXCLUDED.tx_bytes,
		    pss_sum_mb = app_usage.pss_sum_mb + EXCLUDED.pss_sum_mb,
		    pss_max_mb = GREATEST(app_usage.pss_max_mb, EXCLUDED.pss_max_mb), pss_n = app_usage.pss_n + EXCLUDED.pss_n,
		    cpu_sum_pct = app_usage.cpu_sum_pct + EXCLUDED.cpu_sum_pct, cpu_n = app_usage.cpu_n + EXCLUDED.cpu_n,
		    power_mah = app_usage.power_mah + EXCLUDED.power_mah`, cut); err != nil {
		return 0, err
	}
	a, err := tx.Exec(ctx, `DELETE FROM app_usage WHERE NOT daily AND bucket < $1`, cut)
	if err != nil {
		return 0, err
	}
	b, err := tx.Exec(ctx, `DELETE FROM app_usage WHERE daily AND bucket < $1`, time.Now().Add(-keepDaily))
	if err != nil {
		return 0, err
	}
	return a.RowsAffected() + b.RowsAffected(), tx.Commit(ctx)
}
