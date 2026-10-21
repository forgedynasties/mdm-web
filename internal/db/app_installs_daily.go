package db

import (
	"context"
	"fmt"
	"time"
)

// InstallDay is one day of app installs sent from the library (install_apk commands): how
// many devices they were sent to and how many of those reported a failure. For the Apps
// page's Installs card and its rail bars (A1, 21 Oct 2026).
type InstallDay struct {
	Day    time.Time
	Sent   int
	Failed int
}

// AppInstallsDaily returns the last `days` days, oldest first, every day present (UTC). A
// command's devices are its delivery rows, or its device targets when it has not been
// delivered yet (a group send only has rows once delivered).
func (d *DB) AppInstallsDaily(ctx context.Context, days int) ([]InstallDay, error) {
	if days < 1 {
		days = 7
	}
	rows, err := d.pool.Query(ctx, `
		WITH c AS (
			SELECT (c.created_at AT TIME ZONE 'UTC')::date AS day,
			       GREATEST(
			         (SELECT count(*) FROM command_status cs WHERE cs.command_id = c.id),
			         CASE WHEN c.target_type = 'devices'
			              THEN (SELECT count(*) FROM command_targets ct WHERE ct.command_id = c.id) ELSE 0 END) AS sent,
			       (SELECT count(*) FROM command_status cs WHERE cs.command_id = c.id AND cs.status = 'failed') AS failed
			FROM commands c
			WHERE c.type = 'install_apk'
			  AND c.created_at >= (CURRENT_DATE - ($1::int - 1))::timestamp AT TIME ZONE 'UTC'
		)
		SELECT g.day::date, COALESCE(SUM(c.sent), 0)::int, COALESCE(SUM(c.failed), 0)::int
		FROM generate_series(CURRENT_DATE - ($1::int - 1), CURRENT_DATE, interval '1 day') AS g(day)
		LEFT JOIN c ON c.day = g.day::date
		GROUP BY 1 ORDER BY 1`, days)
	if err != nil {
		return nil, fmt.Errorf("app installs daily: %w", err)
	}
	defer rows.Close()
	var out []InstallDay
	for rows.Next() {
		var x InstallDay
		if err := rows.Scan(&x.Day, &x.Sent, &x.Failed); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// AppsAddedSince counts library uploads (APK rows) created since t.
func (d *DB) AppsAddedSince(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `SELECT count(*) FROM apps WHERE created_at >= $1`, t).Scan(&n)
	return n, err
}
