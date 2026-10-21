package db

import (
	"context"
	"fmt"
)

// BuildCrashCounts is crash/ANR events reported on one build: the last 24 h, the 24 h
// before, and the last 7 days, with the devices behind each. Same event kinds as the
// release's crash page (CrashGroupsOnBuild), scoped by the build the event was reported on.
type BuildCrashCounts struct {
	Day, PrevDay, Week      int
	DayDevices, WeekDevices int
}

func (d *DB) BuildCrashCounts(ctx context.Context, buildID string) (BuildCrashCounts, error) {
	var c BuildCrashCounts
	if buildID == "" {
		return c, nil
	}
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE e.occurred_at > NOW() - INTERVAL '24 hours'),
		       COUNT(*) FILTER (WHERE e.occurred_at <= NOW() - INTERVAL '24 hours' AND e.occurred_at > NOW() - INTERVAL '48 hours'),
		       COUNT(*) FILTER (WHERE e.occurred_at > NOW() - INTERVAL '7 days'),
		       COUNT(DISTINCT e.device_id) FILTER (WHERE e.occurred_at > NOW() - INTERVAL '24 hours'),
		       COUNT(DISTINCT e.device_id) FILTER (WHERE e.occurred_at > NOW() - INTERVAL '7 days')
		FROM device_events e
		WHERE e.build_id = $1 AND e.kind NOT IN `+crashKindsSQL+`
		  AND e.occurred_at > NOW() - INTERVAL '7 days'`, buildID).
		Scan(&c.Day, &c.PrevDay, &c.Week, &c.DayDevices, &c.WeekDevices)
	if err != nil {
		return c, fmt.Errorf("build crash counts: %w", err)
	}
	return c, nil
}

// ReleaseInstallsSince counts devices that finished installing one of a release's updates
// in the last `days` days (update_devices reaching "installed").
func (d *DB) ReleaseInstallsSince(ctx context.Context, releaseID, days int) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id
		WHERE u.release_id = $1 AND ud.status = 'installed'
		  AND ud.completed_at > NOW() - make_interval(days => $2::int)`, releaseID, days).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("release installs: %w", err)
	}
	return n, nil
}
